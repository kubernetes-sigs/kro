// Copyright 2025 The Kube Resource Orchestrator Authors
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

package instance

import (
	"context"
	"fmt"

	"github.com/kubernetes-sigs/kro/pkg/controller/instance/delta"
	"github.com/kubernetes-sigs/kro/pkg/runtime"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
)

func (igr *instanceGraphReconciler) reconcileInstanceClientSideDelta(ctx context.Context) error {
	instance := igr.runtime.GetInstance()

	// Set managed state and handle instance labels
	if err := igr.setupInstance(ctx, instance); err != nil {
		return fmt.Errorf("failed to setup instance: %w", err)
	}

	// Initialize resource states
	for _, resourceID := range igr.runtime.TopologicalOrder() {
		igr.state.ResourceStates[resourceID] = &ResourceState{State: ResourceStatePending}
	}

	// Reconcile resources in topological order
	for _, resourceID := range igr.runtime.TopologicalOrder() {
		if err := igr.reconcileResourceClientSideDelta(ctx, resourceID); err != nil {
			return err
		}

		// Synchronize runtime state after each resource
		if _, err := igr.runtime.Synchronize(); err != nil {
			return fmt.Errorf("failed to synchronize reconciling resource %s: %w", resourceID, err)
		}
	}

	return nil
}

// reconcileResource handles the reconciliation of a single resource within the instance
func (igr *instanceGraphReconciler) reconcileResourceClientSideDelta(ctx context.Context, resourceID string) error {
	log := igr.log.WithValues("resourceID", resourceID)
	resourceState := &ResourceState{State: ResourceStateInProgress}
	igr.state.ResourceStates[resourceID] = resourceState

	// Check if resource should be processed (create or get)
	if want, err := igr.runtime.ReadyToProcessResource(resourceID); err != nil || !want {
		var reason any
		if err != nil {
			reason = err
		} else {
			reason = "resource not yet ready to process"
		}
		log.V(1).Info("Skipping resource processing", "reason", reason)
		resourceState.State = ResourceStateSkipped
		igr.runtime.IgnoreResource(resourceID)
		return nil
	}

	// Get and validate resource state
	resource, state := igr.runtime.GetResource(resourceID)
	if state != runtime.ResourceStateResolved {
		return igr.delayedRequeue(fmt.Errorf("resource %s not resolved: state=%v", resourceID, state))
	}

	// Handle resource reconciliation
	return igr.handleResourceReconciliationClientSideDelta(ctx, resourceID, resource, resourceState)
}

// handleResourceReconciliation manages the reconciliation of a specific resource,
// including creation, updates, and readiness checks.
func (igr *instanceGraphReconciler) handleResourceReconciliationClientSideDelta(
	ctx context.Context,
	resourceID string,
	resource *unstructured.Unstructured,
	resourceState *ResourceState,
) error {
	log := igr.log.WithValues("resourceID", resourceID)

	// Get resource client and namespace
	rc := igr.getResourceClient(resourceID)

	// Check if resource exists
	observed, err := rc.Get(ctx, resource.GetName(), metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			// For read-only resources, we don't create
			if igr.runtime.ResourceDescriptor(resourceID).IsExternalRef() {
				resourceState.State = "WAITING_FOR_EXTERNAL_RESOURCE"
				resourceState.Err = fmt.Errorf("external resource not found: %w", err)
				return igr.delayedRequeue(resourceState.Err)
			}
			return igr.handleResourceCreationClientSideDelta(ctx, rc, resource, resourceID, resourceState)
		}
		resourceState.State = ResourceStateError
		resourceState.Err = fmt.Errorf("failed to get resource: %w", err)
		return resourceState.Err
	}

	// Update runtime with observed state
	igr.runtime.SetResource(resourceID, observed)

	// Check resource readiness
	if ready, reason, err := igr.runtime.IsResourceReady(resourceID); err != nil || !ready {
		log.V(1).Info("Resource not ready", "reason", reason, "error", err)
		resourceState.State = ResourceStateWaitingForReadiness
		resourceState.Err = fmt.Errorf("resource not ready: %s: %w", reason, err)
		return igr.delayedRequeue(resourceState.Err)
	}

	resourceState.State = ResourceStateSynced

	// For read-only resources, don't perform updates
	if igr.runtime.ResourceDescriptor(resourceID).IsExternalRef() {
		return nil
	}

	return igr.updateResourceClientSideDelta(ctx, rc, resource, observed, resourceID, resourceState)
}

// handleResourceCreation manages the creation of a new resource
func (igr *instanceGraphReconciler) handleResourceCreationClientSideDelta(
	ctx context.Context,
	rc dynamic.ResourceInterface,
	resource *unstructured.Unstructured,
	resourceID string,
	resourceState *ResourceState,
) error {
	igr.log.V(1).Info("Creating new resource", "resourceID", resourceID)

	// Apply labels and create resource
	igr.instanceSubResourcesLabeler.ApplyLabels(resource)
	if _, err := rc.Create(ctx, resource, metav1.CreateOptions{}); err != nil {
		resourceState.State = ResourceStateError
		resourceState.Err = fmt.Errorf("failed to create resource: %w", err)
		return resourceState.Err
	}

	resourceState.State = ResourceStateCreated
	return igr.delayedRequeue(fmt.Errorf("awaiting resource creation completion"))
}

// updateResource handles updates to an existing resource, comparing the desired
// and observed states and applying the necessary changes.
func (igr *instanceGraphReconciler) updateResourceClientSideDelta(
	ctx context.Context,
	rc dynamic.ResourceInterface,
	desired, observed *unstructured.Unstructured,
	resourceID string,
	resourceState *ResourceState,
) error {
	igr.log.V(1).Info("Processing resource update", "resourceID", resourceID)

	// Compare desired and observed states
	differences, err := delta.Compare(desired, observed)
	if err != nil {
		resourceState.State = ResourceStateError
		resourceState.Err = fmt.Errorf("failed to compare desired and observed states: %w", err)
		return resourceState.Err
	}

	// If no differences are found, the resource is in sync.
	if len(differences) == 0 {
		resourceState.State = ResourceStateSynced
		igr.log.V(1).Info("No deltas found for resource", "resourceID", resourceID)
		return nil
	}

	// Proceed with the update, note that we don't need to handle each difference
	// individually. We can apply all changes at once.
	//
	// NOTE(a-hilaly): are there any cases where we need to handle each difference individually?
	igr.log.V(1).Info("Found deltas for resource",
		"resourceID", resourceID,
		"delta", differences,
	)
	igr.instanceSubResourcesLabeler.ApplyLabels(desired)

	// Apply changes to the resource
	// TODO: Handle annotations
	desired.SetResourceVersion(observed.GetResourceVersion())
	desired.SetFinalizers(observed.GetFinalizers())
	_, err = rc.Update(ctx, desired, metav1.UpdateOptions{})
	if err != nil {
		resourceState.State = ResourceStateError
		resourceState.Err = fmt.Errorf("failed to update resource: %w", err)
		return resourceState.Err
	}

	// Set state to UPDATING and requeue to check the update
	resourceState.State = ResourceStateUpdating
	return igr.delayedRequeue(fmt.Errorf("resource update in progress"))
}
