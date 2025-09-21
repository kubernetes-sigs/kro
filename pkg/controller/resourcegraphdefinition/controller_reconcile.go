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
	"fmt"
	"time"

	v1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/kro-run/kro/api/v1alpha1"
	instancectrl "github.com/kro-run/kro/pkg/controller/instance"
	"github.com/kro-run/kro/pkg/dynamiccontroller"
	"github.com/kro-run/kro/pkg/graph"
	"github.com/kro-run/kro/pkg/metadata"
	"github.com/kro-run/kro/pkg/requeue"
)

const (
	crdNotEstablishedRequeueDuration = 500 * time.Millisecond
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
	established, err := r.reconcileResourceGraphDefinitionCRD(ctx, crd)
	if err != nil {
		mark.KindUnready(err.Error())
		return processedRGD.TopologicalOrder, resourcesInfo, err
	}
	if !established {
		log.V(1).Info("CRD not yet established, requeuing")

		mark.KindUnready("CRD exists but not yet established")
		// CRD establishment is async, requeue to check readiness
		return processedRGD.TopologicalOrder, resourcesInfo, requeue.NeededAfter(nil, crdNotEstablishedRequeueDuration)
	}
	mark.KindReady(crd.Status.AcceptedNames.Kind)

	if crd, err = r.clientSet.CRD().Get(ctx, crd.Name, metav1.GetOptions{}); err != nil {
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
func (r *ResourceGraphDefinitionReconciler) reconcileResourceGraphDefinitionGraph(_ context.Context, rgd *v1alpha1.ResourceGraphDefinition) (*graph.Graph, []v1alpha1.ResourceInformation, error) {
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

// reconcileResourceGraphDefinitionCRD checks if the CRD exists and is established, creating it if necessary
// returns a boolean indicating if the CRD is established or not. If error is not nil, the CRD doesn't exist at all.
func (r *ResourceGraphDefinitionReconciler) reconcileResourceGraphDefinitionCRD(ctx context.Context, crd *v1.CustomResourceDefinition) (bool, error) {
	existing, err := r.clientSet.CRD().Get(ctx, crd.Name, metav1.GetOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return false, err
		}

		// CRD doesn't exist, create it
		_, err = r.clientSet.CRD().Create(ctx, crd, metav1.CreateOptions{})
		if err != nil {
			return false, err
		}

		// CRDs will need time to be established after creation
		// We optimistically return not ready here; the next reconciliation loop
		// will check readiness
		return false, nil
	}

	// CRD exists, check if it's ready (Established condition)
	established := false
	for _, cond := range existing.Status.Conditions {
		if cond.Type == v1.Established && cond.Status == v1.ConditionTrue {
			established = true
			break
		}
	}

	return established, nil
}

// For cleanup/deletion
func (r *ResourceGraphDefinitionReconciler) deleteCRD(ctx context.Context, name string) error {
	err := r.clientSet.CRD().Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
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
