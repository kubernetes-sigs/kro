// Copyright 2025 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package resourcegraphdefinition

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/kro-run/kro/pkg/client"
	authv1 "k8s.io/api/authorization/v1"
	v1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	authclient "k8s.io/client-go/kubernetes/typed/authorization/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/kro-run/kro/api/v1alpha1"
	instancectrl "github.com/kro-run/kro/pkg/controller/instance"
	"github.com/kro-run/kro/pkg/dynamiccontroller"
	"github.com/kro-run/kro/pkg/graph"
	"github.com/kro-run/kro/pkg/metadata"
)

// reconcileResourceGraphDefinition orchestrates the reconciliation of a ResourceGraphDefinition by:
// 1. Processing the resource graph
// 2. Ensuring CRDs are present
// 3. Setting up and starting the microcontroller
func (r *ResourceGraphDefinitionReconciler) reconcileResourceGraphDefinition(ctx context.Context, rgd *v1alpha1.ResourceGraphDefinition) ([]string, []v1alpha1.ResourceInformation, error) {
	log := ctrl.LoggerFrom(ctx)
	mark := NewConditionsMarkerFor(rgd)

	// Process resource graph definition graph first to validate structure
	log.V(1).Info("reconciling resource graph definition graph")
	processedRGD, resourcesInfo, err := r.reconcileResourceGraphDefinitionGraph(ctx, rgd)
	if err != nil {
		mark.ResourceGraphInvalid(err.Error())
		return nil, nil, err
	}
	mark.ResourceGraphValid()

	// Setup metadata labeling
	graphExecLabeler, err := r.setupLabeler(rgd)
	if err != nil {
		mark.FailedLabelerSetup(err.Error())
		return nil, nil, fmt.Errorf("failed to setup labeler: %w", err)
	}

	crd := processedRGD.Instance.GetCRD()
	graphExecLabeler.ApplyLabels(&crd.ObjectMeta)

	// Ensure CRD exists and is up to date
	log.V(1).Info("reconciling resource graph definition CRD")
	if err := r.reconcileResourceGraphDefinitionCRD(ctx, crd); err != nil {
		mark.KindUnready(err.Error())
		return processedRGD.TopologicalOrder, resourcesInfo, err
	}
	if crd, err = r.crdManager.Get(ctx, crd.Name); err != nil {
		mark.KindUnready(err.Error())
	} else {
		mark.KindReady(crd.Status.AcceptedNames.Kind)
	}

	// Setup and start microcontroller
	gvr := processedRGD.Instance.GetGroupVersionResource()
	controller := r.setupMicroController(gvr, processedRGD, rgd.Spec.DefaultServiceAccounts, graphExecLabeler)

	log.V(1).Info("reconciling resource graph definition micro controller")
	// TODO: the context that is passed here is tied to the reconciliation of the rgd, we might need to make
	// a new context with our own cancel function here to allow us to cleanly term the dynamic controller
	// rather than have it ignore this context and use the background context.
	if err := r.reconcileResourceGraphDefinitionMicroController(ctx, &gvr, controller.Reconcile); err != nil {
		mark.ControllerFailedToStart(err.Error())
		return processedRGD.TopologicalOrder, resourcesInfo, err
	}
	mark.ControllerRunning()

	return processedRGD.TopologicalOrder, resourcesInfo, nil
}

// setupLabeler creates and merges the required labelers for the resource graph definition
func (r *ResourceGraphDefinitionReconciler) setupLabeler(rgd *v1alpha1.ResourceGraphDefinition) (metadata.Labeler, error) {
	rgLabeler := metadata.NewResourceGraphDefinitionLabeler(rgd)
	return r.metadataLabeler.Merge(rgLabeler)
}

// setupMicroController creates a new controller instance with the required configuration
func (r *ResourceGraphDefinitionReconciler) setupMicroController(
	gvr schema.GroupVersionResource,
	processedRGD *graph.Graph,
	defaultSVCs map[string]string,
	labeler metadata.Labeler,
) *instancectrl.Controller {
	instanceLogger := r.instanceLogger.WithName(fmt.Sprintf("%s-controller", gvr.Resource)).WithValues(
		"controller", gvr.Resource,
		"controllerGroup", processedRGD.Instance.GetCRD().Spec.Group,
		"controllerKind", processedRGD.Instance.GetCRD().Spec.Names.Kind,
	)

	return instancectrl.NewController(
		instanceLogger,
		instancectrl.ReconcileConfig{
			DefaultRequeueDuration:    3 * time.Second,
			DeletionGraceTimeDuration: 30 * time.Second,
			DeletionPolicy:            "Delete",
		},
		gvr,
		processedRGD,
		r.clientSet,
		defaultSVCs,
		labeler,
	)
}

// reconcileResourceGraphDefinitionGraph processes the resource graph definition to build a dependency graph
// and extract resource information
func (r *ResourceGraphDefinitionReconciler) reconcileResourceGraphDefinitionGraph(ctx context.Context, rgd *v1alpha1.ResourceGraphDefinition) (*graph.Graph, []v1alpha1.ResourceInformation, error) {
	processedRGD, err := r.rgBuilder.NewResourceGraphDefinition(rgd)
	if err != nil {
		return nil, nil, newGraphError(err)
	}

	resourcesInfo := make([]v1alpha1.ResourceInformation, 0, len(processedRGD.Resources))
	for name, resource := range processedRGD.Resources {
		deps := resource.GetDependencies()
		if len(deps) > 0 {
			resourcesInfo = append(resourcesInfo, buildResourceInfo(name, deps))
		}
	}

	// make sure the service accounts configured in the resource graph definition have the required permissions.
	if err := r.reconcileResourceGraphDefinitionPermissions(ctx, processedRGD.Resources, rgd.Spec.DefaultServiceAccounts); err != nil {
		return nil, nil, err
	}

	return processedRGD, resourcesInfo, nil
}

// buildResourceInfo creates a ResourceInformation struct from name and dependencies
func buildResourceInfo(name string, deps []string) v1alpha1.ResourceInformation {
	dependencies := make([]v1alpha1.Dependency, 0, len(deps))
	for _, dep := range deps {
		dependencies = append(dependencies, v1alpha1.Dependency{ID: dep})
	}
	return v1alpha1.ResourceInformation{
		ID:           name,
		Dependencies: dependencies,
	}
}

// reconcileResourceGraphDefinitionCRD ensures the CRD is present and up to date in the cluster
func (r *ResourceGraphDefinitionReconciler) reconcileResourceGraphDefinitionCRD(ctx context.Context, crd *v1.CustomResourceDefinition) error {
	if err := r.crdManager.Ensure(ctx, *crd); err != nil {
		return newCRDError(err)
	}
	return nil
}

// reconcileResourceGraphDefinitionMicroController starts the microcontroller for handling the resources
func (r *ResourceGraphDefinitionReconciler) reconcileResourceGraphDefinitionMicroController(ctx context.Context, gvr *schema.GroupVersionResource, handler dynamiccontroller.Handler) error {
	err := r.dynamicController.StartServingGVK(ctx, *gvr, handler)
	if err != nil {
		return newMicroControllerError(err)
	}
	return nil
}

// Error types for the resourcegraphdefinition controller
type (
	graphError           struct{ err error }
	crdError             struct{ err error }
	microControllerError struct{ err error }
)

// Error interface implementation
func (e *graphError) Error() string           { return e.err.Error() }
func (e *crdError) Error() string             { return e.err.Error() }
func (e *microControllerError) Error() string { return e.err.Error() }

// Unwrap interface implementation
func (e *graphError) Unwrap() error           { return e.err }
func (e *crdError) Unwrap() error             { return e.err }
func (e *microControllerError) Unwrap() error { return e.err }

// Error constructors
func newGraphError(err error) error           { return &graphError{err} }
func newCRDError(err error) error             { return &crdError{err} }
func newMicroControllerError(err error) error { return &microControllerError{err} }

func (r *ResourceGraphDefinitionReconciler) reconcileResourceGraphDefinitionPermissions(ctx context.Context, resources map[string]*graph.Resource, accounts map[string]string) error {
	var errs []error
	for _, resource := range resources {
		selectedServiceAccount := selectServiceAccountForResource(accounts, resource)
		if err := verifyResourcePermissions(ctx, r.clientSet, selectedServiceAccount, resource); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

func verifyResourcePermissions(ctx context.Context, clientSet client.SetInterface, serviceAccount string, resource *graph.Resource) error {
	var clnt authclient.SelfSubjectAccessReviewInterface
	if serviceAccount != "" {
		impersonatedClientSet, err := clientSet.WithImpersonation(serviceAccount)
		if err != nil {
			return err
		}
		clnt = impersonatedClientSet.Authorization().SelfSubjectAccessReviews()
	} else {
		clnt = clientSet.Authorization().SelfSubjectAccessReviews()
	}

	allowed, err := hasPermission(ctx, clnt, resource, "create", "update", "delete", "patch", "watch")
	if err != nil {
		return err
	}

	errs := make([]error, 0, len(allowed))
	for _, verb := range slices.Sorted(maps.Keys(allowed)) {
		if allowed[verb] {
			continue
		}
		description := fmt.Sprintf("specified service account %s does not have permission to %s %q (%s)",
			serviceAccount, verb, resource.GetID(), resource.GetGroupVersionResource().String())
		if resource.IsNamespaced() {
			namespace := resource.Unstructured().GetNamespace()
			if namespace == "" {
				namespace = metav1.NamespaceDefault
			}
			description += fmt.Sprintf(" in namespace %s", namespace)
		}
		errs = append(errs, errors.New(description))
	}
	return errors.Join(errs...)
}

func selectServiceAccountForResource(accounts map[string]string, resource *graph.Resource) string {
	var selectedServiceAccount string
	for namespace, serviceAccount := range accounts {
		if resource.IsNamespaced() && namespace == resource.Unstructured().GetNamespace() {
			selectedServiceAccount = getServiceAccountUserName(resource.Unstructured().GetNamespace(), serviceAccount)
			break
		}
	}

	if selectedServiceAccount == "" && accounts[v1alpha1.DefaultServiceAccountKey] != "" {
		selectedServiceAccount = getServiceAccountUserName(resource.Unstructured().GetNamespace(), accounts[v1alpha1.DefaultServiceAccountKey])
	}

	return selectedServiceAccount
}

func hasPermission(
	ctx context.Context,
	clnt authclient.SelfSubjectAccessReviewInterface,
	resource *graph.Resource,
	verbs ...string,
) (map[string]bool, error) {
	gvr := resource.GetGroupVersionResource()

	namespace := metav1.NamespaceDefault
	if ns := resource.Unstructured().GetNamespace(); ns != "" {
		namespace = ns
	}

	results := make(map[string]bool, len(verbs))
	var mu sync.Mutex
	errCh := make(chan error, len(verbs))

	var wg sync.WaitGroup
	wg.Add(len(verbs))

	for _, verb := range verbs {
		go func() {
			defer wg.Done()

			review := &authv1.SelfSubjectAccessReview{
				Spec: authv1.SelfSubjectAccessReviewSpec{
					ResourceAttributes: &authv1.ResourceAttributes{
						Namespace: namespace,
						Verb:      verb,
						Group:     gvr.Group,
						Version:   gvr.Version,
						Resource:  gvr.Resource,
					},
				},
			}

			resp, err := clnt.Create(ctx, review, metav1.CreateOptions{})
			if err != nil {
				errCh <- err
				return
			}

			mu.Lock()
			results[verb] = resp.Status.Allowed
			mu.Unlock()
		}()
	}

	wg.Wait()
	close(errCh)

	// return first error if any
	for err := range errCh {
		if err != nil {
			return nil, err
		}
	}

	return results, nil
}

// getServiceAccountUserName builds the impersonate service account user name.
// The format of the user name is "system:serviceaccount:<serviceaccount>"
func getServiceAccountUserName(namespace, serviceAccount string) string {
	if namespace == "" {
		namespace = metav1.NamespaceDefault
	}
	return fmt.Sprintf("system:serviceaccount:%s:%s", namespace, serviceAccount)
}
