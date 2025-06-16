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

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	"github.com/kro-run/kro/pkg/controller/instance/delta"
	"github.com/kro-run/kro/pkg/metadata"
	"github.com/kro-run/kro/pkg/requeue"
	"github.com/kro-run/kro/pkg/runtime"
)

const (
	ResourceStatePending             = "PENDING"
	ResourceStateInProgress          = "IN_PROGRESS"
	ResourceStateDeleting            = "DELETING"
	ResourceStateSkipped             = "SKIPPED"
	ResourceStateError               = "ERROR"
	ResourceStateSynced              = "SYNCED"
	ResourceStateCreated             = "CREATED"
	ResourceStateDeleted             = "DELETED"
	ResourceStatePendingDeletion     = "PENDING_DELETION"
	ResourceStateWaitingForReadiness = "WAITING_FOR_READINESS"
	ResourceStateUpdating            = "UPDATING"
)

// instanceGraphReconciler is responsible for reconciling a single instance and
// and its associated sub-resources. It executes the reconciliation logic based
// on the graph inferred from the ResourceGraphDefinition analysis.
type instanceGraphReconciler struct {
	log logr.Logger
	// gvr represents the Group, Version, and Resource of the custom resource
	// this controller is responsible for.
	gvr schema.GroupVersionResource
	// client is a dynamic client for interacting with the Kubernetes API server
	client dynamic.Interface
	// runtime is the runtime representation of the ResourceGraphDefinition. It holds the
	// information about the instance and its sub-resources, the CEL expressions
	// their dependencies, and the resolved values... etc
	runtime runtime.Interface
	// instanceLabeler is responsible for applying labels to the instance object
	instanceLabeler metadata.Labeler
	// instanceSubResourcesLabeler is responsible for applying labels to the
	// sub resources.
	instanceSubResourcesLabeler metadata.Labeler
	// reconcileConfig holds the configuration parameters for the reconciliation
	// process.
	reconcileConfig ReconcileConfig
	// state holds the current state of the instance and its sub-resources.
	state *InstanceState
}

// reconcile performs the reconciliation of the instance and its sub-resources.
// It manages the full lifecycle of the instance including creation, updates,
// and deletion.
func (igr *instanceGraphReconciler) reconcile(ctx context.Context) error {
	instance := igr.runtime.GetInstance()
	igr.state = newInstanceState()

	// Handle instance deletion if marked for deletion
	if !instance.GetDeletionTimestamp().IsZero() {
		igr.state.State = ResourceStateDeleting
		return igr.handleReconciliation(ctx, igr.handleInstanceDeletion)
	}

	return igr.handleReconciliation(ctx, igr.reconcileInstance)
}

// handleReconciliation provides a common wrapper for reconciliation operations,
// handling status updates and error management.
func (igr *instanceGraphReconciler) handleReconciliation(ctx context.Context, reconcileFunc func(context.Context) error) error {
	defer func() {
		// Update instance state based on reconciliation result
		igr.updateInstanceState()

		// Prepare and patch status
		status := igr.prepareStatus()
		if err := igr.patchInstanceStatus(ctx, status); err != nil {
			// Only log error if instance still exists
			if !apierrors.IsNotFound(err) {
				igr.log.Error(err, "Failed to patch instance status")
			}
		}
	}()

	igr.state.ReconcileErr = reconcileFunc(ctx)
	return igr.state.ReconcileErr
}

// reconcileInstance handles the reconciliation of an active instance
func (igr *instanceGraphReconciler) reconcileInstance(ctx context.Context) error {
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
		if err := igr.reconcileResource(ctx, resourceID); err != nil {
			return err
		}

		// Synchronize runtime state after each resource
		if _, err := igr.runtime.Synchronize(); err != nil {
			return fmt.Errorf("failed to synchronize reconciling resource %s: %w", resourceID, err)
		}
	}

	return nil
}

// setupInstance prepares an instance for reconciliation by setting up necessary
// labels and managed state.
func (igr *instanceGraphReconciler) setupInstance(ctx context.Context, instance *unstructured.Unstructured) error {
	patched, err := igr.setManaged(ctx, instance, instance.GetUID())
	if err != nil {
		return err
	}
	if patched != nil {
		instance.Object = patched.Object
	}
	return nil
}

// reconcileResource handles the reconciliation of a single resource within the instance
func (igr *instanceGraphReconciler) reconcileResource(ctx context.Context, resourceID string) error {
	log := igr.log.WithValues("resourceID", resourceID)
	resourceState := &ResourceState{State: ResourceStateInProgress}
	igr.state.ResourceStates[resourceID] = resourceState

	// Check if resource should be processed (create or get)
	if want, err := igr.runtime.ReadyToProcessResource(resourceID); err != nil || !want {
		log.V(1).Info("Skipping resource processing", "reason", err)
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
	return igr.handleResourceReconciliation(ctx, resourceID, resource, resourceState)
}

// handleResourceReconciliation manages the reconciliation of a specific resource,
// including creation, updates, and readiness checks.
func (igr *instanceGraphReconciler) handleResourceReconciliation(
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
			return igr.handleResourceCreation(ctx, rc, resource, resourceID, resourceState)
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

	return igr.updateResource(ctx, rc, resource, observed, resourceID, resourceState)
}

// getResourceClient returns the appropriate dynamic client and namespace for a resource
func (igr *instanceGraphReconciler) getResourceClient(resourceID string) dynamic.ResourceInterface {
	descriptor := igr.runtime.ResourceDescriptor(resourceID)
	gvr := descriptor.GetGroupVersionResource()
	namespace := igr.getResourceNamespace(resourceID)

	if descriptor.IsNamespaced() {
		return igr.client.Resource(gvr).Namespace(namespace)
	}
	return igr.client.Resource(gvr)
}

// handleResourceCreation manages the creation of a new resource
func (igr *instanceGraphReconciler) handleResourceCreation(
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
func (igr *instanceGraphReconciler) updateResource(
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

// handleInstanceDeletion manages the deletion of an instance and its resources
// following the reverse topological order to respect dependencies.
func (igr *instanceGraphReconciler) handleInstanceDeletion(ctx context.Context) error {
	igr.log.V(1).Info("Beginning instance deletion process")

	// Initialize deletion state for all resources
	if err := igr.initializeDeletionState(); err != nil {
		return fmt.Errorf("failed to initialize deletion state: %w", err)
	}

	// Delete resources in reverse order
	if err := igr.deleteResourcesInOrder(ctx); err != nil {
		return err
	}

	// Check if all resources are deleted and cleanup instance
	return igr.finalizeDeletion(ctx)
}

// initializeDeletionState prepares resources for deletion by checking their
// current state and marking them appropriately.
func (igr *instanceGraphReconciler) initializeDeletionState() error {
	for _, resourceID := range igr.runtime.TopologicalOrder() {
		if _, err := igr.runtime.Synchronize(); err != nil {
			return fmt.Errorf("failed to synchronize during deletion state initialization: %w", err)
		}

		resource, state := igr.runtime.GetResource(resourceID)
		if state != runtime.ResourceStateResolved {
			igr.state.ResourceStates[resourceID] = &ResourceState{
				State: ResourceStateSkipped,
			}
			continue
		}

		// Check if resource exists
		rc := igr.getResourceClient(resourceID)
		observed, err := rc.Get(context.TODO(), resource.GetName(), metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				igr.state.ResourceStates[resourceID] = &ResourceState{
					State: ResourceStateDeleted,
				}
				continue
			}
			return fmt.Errorf("failed to check resource %s existence: %w", resourceID, err)
		}

		igr.runtime.SetResource(resourceID, observed)
		igr.state.ResourceStates[resourceID] = &ResourceState{
			State: ResourceStatePendingDeletion,
		}
	}
	return nil
}

// deleteResourcesInOrder processes resource deletion in reverse topological order
// to respect dependencies between resources.
func (igr *instanceGraphReconciler) deleteResourcesInOrder(ctx context.Context) error {
	// Process resources in reverse order
	resources := igr.runtime.TopologicalOrder()
	for i := len(resources) - 1; i >= 0; i-- {
		resourceID := resources[i]
		resourceState := igr.state.ResourceStates[resourceID]

		if resourceState == nil || resourceState.State != ResourceStatePendingDeletion {
			continue
		}

		// Skip deletion for read-only resources
		if igr.runtime.ResourceDescriptor(resourceID).IsExternalRef() {
			igr.state.ResourceStates[resourceID].State = ResourceStateSkipped
			continue
		}

		if err := igr.deleteResource(ctx, resourceID); err != nil {
			return err
		}
	}
	return nil
}

// deleteResource handles the deletion of a single resource and updates its state.
func (igr *instanceGraphReconciler) deleteResource(ctx context.Context, resourceID string) error {
	igr.log.V(1).Info("Deleting resource", "resourceID", resourceID)

	resource, _ := igr.runtime.GetResource(resourceID)
	rc := igr.getResourceClient(resourceID)

	// Attempt to delete the resource
	err := rc.Delete(ctx, resource.GetName(), metav1.DeleteOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			igr.state.ResourceStates[resourceID].State = ResourceStateDeleted
			return nil
		}
		igr.state.ResourceStates[resourceID].State = InstanceStateError
		igr.state.ResourceStates[resourceID].Err = fmt.Errorf("failed to delete resource: %w", err)
		return igr.state.ResourceStates[resourceID].Err
	}

	igr.state.ResourceStates[resourceID].State = InstanceStateDeleting
	return igr.delayedRequeue(fmt.Errorf("resource deletion in progress"))
}

// finalizeDeletion checks if all resources are deleted and removes the instance finalizer
// if appropriate.
func (igr *instanceGraphReconciler) finalizeDeletion(ctx context.Context) error {
	// Check if all resources are deleted
	for _, resourceState := range igr.state.ResourceStates {
		if resourceState.State != ResourceStateDeleted && resourceState.State != ResourceStateSkipped {
			return igr.delayedRequeue(fmt.Errorf("waiting for resource deletion completion"))
		}
	}

	// Remove finalizer from instance
	instance := igr.runtime.GetInstance()
	patched, err := igr.setUnmanaged(ctx, instance)
	if err != nil {
		return fmt.Errorf("failed to remove instance finalizer: %w", err)
	}

	igr.runtime.SetInstance(patched)
	return nil
}

// setManaged ensures the instance has the necessary finalizer and labels.
func (igr *instanceGraphReconciler) setManaged(ctx context.Context, obj *unstructured.Unstructured, _ types.UID) (*unstructured.Unstructured, error) {
	if exist, _ := metadata.HasInstanceFinalizerUnstructured(obj); exist {
		return obj, nil
	}

	igr.log.V(1).Info("Setting managed state", "name", obj.GetName(), "namespace", obj.GetNamespace())

	copy := obj.DeepCopy()
	if err := metadata.SetInstanceFinalizerUnstructured(copy); err != nil {
		return nil, fmt.Errorf("failed to set finalizer: %w", err)
	}

	igr.instanceLabeler.ApplyLabels(copy)

	updated, err := igr.client.Resource(igr.gvr).
		Namespace(obj.GetNamespace()).
		Update(ctx, copy, metav1.UpdateOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to update managed state: %w", err)
	}

	return updated, nil
}

// setUnmanaged removes the finalizer from the instance.
func (igr *instanceGraphReconciler) setUnmanaged(ctx context.Context, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	if exist, _ := metadata.HasInstanceFinalizerUnstructured(obj); !exist {
		return obj, nil
	}

	igr.log.V(1).Info("Removing managed state", "name", obj.GetName(), "namespace", obj.GetNamespace())

	copy := obj.DeepCopy()
	if err := metadata.RemoveInstanceFinalizerUnstructured(copy); err != nil {
		return nil, fmt.Errorf("failed to remove finalizer: %w", err)
	}

	updated, err := igr.client.Resource(igr.gvr).
		Namespace(obj.GetNamespace()).
		Update(ctx, copy, metav1.UpdateOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to update unmanaged state: %w", err)
	}

	return updated, nil
}

// delayedRequeue wraps an error with requeue information for the controller runtime.
func (igr *instanceGraphReconciler) delayedRequeue(err error) error {
	return requeue.NeededAfter(err, igr.reconcileConfig.DefaultRequeueDuration)
}

// getResourceNamespace determines the appropriate namespace for a resource.
// It follows this precedence order:
// 1. Resource's explicitly specified namespace
// 2. Instance's namespace
// 3. Default namespace
func (igr *instanceGraphReconciler) getResourceNamespace(resourceID string) string {
	instance := igr.runtime.GetInstance()
	resource, _ := igr.runtime.GetResource(resourceID)

	// First check if resource has an explicitly specified namespace
	if ns := resource.GetNamespace(); ns != "" {
		igr.log.V(2).Info("Using resource-specified namespace",
			"resourceID", resourceID,
			"namespace", ns)
		return ns
	}

	// Then use instance namespace
	if ns := instance.GetNamespace(); ns != "" {
		igr.log.V(2).Info("Using instance namespace",
			"resourceID", resourceID,
			"namespace", ns)
		return ns
	}

	// Finally fall back to default namespace
	igr.log.V(2).Info("Using default namespace",
		"resourceID", resourceID,
		"namespace", metav1.NamespaceDefault)
	return metav1.NamespaceDefault
}
