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
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	v1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	kroclient "github.com/kubernetes-sigs/kro/pkg/client"
	instancectrl "github.com/kubernetes-sigs/kro/pkg/controller/instance"
	"github.com/kubernetes-sigs/kro/pkg/graph"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
)

// reconcileResourceGraphDefinition orchestrates the reconciliation of a ResourceGraphDefinition by:
// 1. Processing the resource graph
// 2. Ensuring CRDs are present
// 3. Setting up and starting the microcontroller
func (r *ResourceGraphDefinitionReconciler) reconcileResourceGraphDefinition(
	ctx context.Context,
	rgd *v1alpha1.ResourceGraphDefinition,
) ([]string, []v1alpha1.ResourceInformation, error) {
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

	// Ensure CRD exists and is up to date (non-blocking)
	log.V(1).Info("reconciling resource graph definition CRD")
	if err := r.ensureCRD(ctx, crd); err != nil {
		mark.KindUnready(err)
		// CRD watch will trigger re-reconcile when CRD becomes established
		return processedRGD.TopologicalOrder, resourcesInfo, nil
	}

	// Get fresh CRD to check establishment status
	crd, err = r.crdClient.Get(ctx, crd.Name, metav1.GetOptions{})
	if err != nil {
		mark.KindUnready(err)
		// CRD watch will trigger re-reconcile when CRD becomes established
		return processedRGD.TopologicalOrder, resourcesInfo, nil
	}

	// Check if CRD is established - if not, return early
	// CRD watch will trigger re-reconcile when CRD becomes established
	if !kroclient.IsEstablished(crd) {
		mark.KindUnready(errors.New("CRD not yet established"))
		return processedRGD.TopologicalOrder, resourcesInfo, nil
	}
	mark.KindReady(crd.Status.AcceptedNames.Kind)

	// TODO: the context that is passed here is tied to the reconciliation of the rgd, we might need to make
	// a new context with our own cancel function here to allow us to cleanly term the dynamic controller
	// rather than have it ignore this context and use the background context.
	gvr := processedRGD.Instance.GetGroupVersionResource()
	if err := r.reconcileResourceGraphDefinitionMicroController(ctx, gvr, processedRGD, graphExecLabeler); err != nil {
		mark.ControllerFailedToStart(err.Error())
		return processedRGD.TopologicalOrder, resourcesInfo, err
	}

	// Set condition based on sync state
	if r.dynamicController.HasSynced(gvr) {
		mark.ControllerRunning()
	} else {
		mark.ControllerSyncing()
	}

	// Check watch health for instance GVR and all child resource GVRs
	resourceGVRs := r.getResourceGVRsToWatchForRGD(processedRGD)
	allGVRs := append([]schema.GroupVersionResource{gvr}, resourceGVRs...)
	r.checkWatchHealth(mark, allGVRs)

	return processedRGD.TopologicalOrder, resourcesInfo, nil
}

func (r *ResourceGraphDefinitionReconciler) getResourceGVRsToWatchForRGD(processedRGD *graph.Graph) []schema.GroupVersionResource {
	resourceHandlers := make(map[schema.GroupVersionResource]struct{}, len(processedRGD.Resources))
	for _, resource := range processedRGD.Resources {
		resourceHandlers[resource.GetGroupVersionResource()] = struct{}{}
	}
	return slices.Collect(maps.Keys(resourceHandlers))
}

// setupLabeler creates and merges the required labelers for the resource graph definition
func (r *ResourceGraphDefinitionReconciler) setupLabeler(rgd *v1alpha1.ResourceGraphDefinition) (metadata.Labeler, error) {
	rgLabeler := metadata.NewResourceGraphDefinitionLabeler(rgd)
	return r.metadataLabeler.Merge(rgLabeler)
}

// setupMicroController creates a new controller instance with the required configuration
func (r *ResourceGraphDefinitionReconciler) setupMicroController(
	processedRGD *graph.Graph,
	labeler metadata.Labeler,
) *instancectrl.Controller {
	gvr := processedRGD.Instance.GetGroupVersionResource()
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
		r.clientSet.RESTMapper(),
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

// ensureCRD ensures the CRD exists and is up to date. This is non-blocking -
// it creates or updates the CRD but doesn't wait for establishment.
func (r *ResourceGraphDefinitionReconciler) ensureCRD(ctx context.Context, crd *v1.CustomResourceDefinition) error {
	log := ctrl.LoggerFrom(ctx)

	existing, err := r.crdClient.Get(ctx, crd.Name, metav1.GetOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return newCRDError(fmt.Errorf("failed to check for existing CRD: %w", err))
		}

		// CRD doesn't exist - create it
		log.Info("Creating CRD", "name", crd.Name)
		if _, err := r.crdClient.Create(ctx, crd, metav1.CreateOptions{}); err != nil {
			return newCRDError(fmt.Errorf("failed to create CRD: %w", err))
		}
		return nil
	}

	// CRD exists - check ownership before updating
	kroOwned, nameMatch, idMatch := metadata.CompareRGDOwnership(existing.ObjectMeta, crd.ObjectMeta)
	if !kroOwned {
		return NewTerminalError(newCRDError(fmt.Errorf(
			"CRD %s already exists and is not owned by KRO", crd.Name,
		)))
	}

	if !nameMatch {
		existingRGDName := existing.Labels[metadata.ResourceGraphDefinitionNameLabel]
		return NewTerminalError(newCRDError(fmt.Errorf(
			"CRD %s is owned by another ResourceGraphDefinition %s", crd.Name, existingRGDName,
		)))
	}

	if nameMatch && !idMatch {
		log.Info(
			"Adopting CRD with different RGD ID - RGD may have been deleted and recreated",
			"crd", crd.Name,
			"existingRGDID", existing.Labels[metadata.ResourceGraphDefinitionIDLabel],
			"newRGDID", crd.Labels[metadata.ResourceGraphDefinitionIDLabel],
		)
	}

	// Update existing CRD
	log.V(1).Info("Updating existing CRD", "name", crd.Name)
	patchBytes, err := json.Marshal(crd)
	if err != nil {
		return newCRDError(fmt.Errorf("failed to marshal CRD for patch: %w", err))
	}

	if _, err := r.crdClient.Patch(ctx, crd.Name, types.MergePatchType, patchBytes, metav1.PatchOptions{}); err != nil {
		return newCRDError(fmt.Errorf("failed to patch CRD: %w", err))
	}
	return nil
}

// reconcileResourceGraphDefinitionMicroController starts the microcontroller for handling the resources
func (r *ResourceGraphDefinitionReconciler) reconcileResourceGraphDefinitionMicroController(
	ctx context.Context,
	gvr schema.GroupVersionResource,
	processedRGD *graph.Graph,
	graphExecLabeler metadata.Labeler,
) error {
	// If we want to react to changes to resources, we need to watch for them
	// and trigger reconciliations of the instances whenever these resources change.
	resourceGVRsToWatch := r.getResourceGVRsToWatchForRGD(processedRGD)

	// Setup and start microcontroller
	controller := r.setupMicroController(processedRGD, graphExecLabeler)

	ctrl.LoggerFrom(ctx).V(1).Info("reconciling resource graph definition micro controller")

	err := r.dynamicController.Register(ctx, gvr, controller.Reconcile, resourceGVRsToWatch...)
	if err != nil {
		return newMicroControllerError(err)
	}
	return nil
}

// checkWatchHealth checks the health of all watches and sets the WatchResourcesHealthy condition.
func (r *ResourceGraphDefinitionReconciler) checkWatchHealth(mark *ConditionsMarker, gvrs []schema.GroupVersionResource) {
	var degradedGVRs []string
	for _, gvr := range gvrs {
		state, found := r.dynamicController.GetWatchState(gvr)
		if found && state.HasError {
			degradedGVRs = append(degradedGVRs, gvr.String())
		}
	}

	if len(degradedGVRs) > 0 {
		mark.WatchResourcesDegraded(fmt.Sprintf("watch errors on: %v", degradedGVRs))
	} else {
		mark.WatchResourcesHealthy()
	}
}

// Error types for the resourcegraphdefinition controller
type (
	graphError           struct{ err error }
	crdError             struct{ err error }
	microControllerError struct{ err error }
	// terminalError wraps errors that are unrecoverable and won't self-heal.
	// Examples: CRD ownership conflicts, invalid schema definitions.
	terminalError struct{ err error }
)

// Error interface implementation
func (e *graphError) Error() string           { return e.err.Error() }
func (e *crdError) Error() string             { return e.err.Error() }
func (e *microControllerError) Error() string { return e.err.Error() }
func (e *terminalError) Error() string        { return e.err.Error() }

// Unwrap interface implementation
func (e *graphError) Unwrap() error           { return e.err }
func (e *crdError) Unwrap() error             { return e.err }
func (e *microControllerError) Unwrap() error { return e.err }
func (e *terminalError) Unwrap() error        { return e.err }

// Error constructors
func newGraphError(err error) error           { return &graphError{err} }
func newCRDError(err error) error             { return &crdError{err} }
func newMicroControllerError(err error) error { return &microControllerError{err} }

// NewTerminalError wraps an error to indicate it's unrecoverable.
func NewTerminalError(err error) error {
	if err == nil {
		return nil
	}
	return &terminalError{err}
}

// IsTerminalError returns true if the error (or any wrapped error) is terminal.
func IsTerminalError(err error) bool {
	var t *terminalError
	return errors.As(err, &t)
}
