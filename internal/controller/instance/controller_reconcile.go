// Copyright Amazon.com Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//     http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

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

	"github.com/awslabs/kro/internal/metadata"
	"github.com/awslabs/kro/internal/requeue"
	"github.com/awslabs/kro/internal/runtime"
)

// instanceGraphReconciler is responsible for reconciling a single instance and
// and its associated sub-resources. It executes the reconciliation logic based
// on the graph inferred from the ResourceGroup analysis.
type instanceGraphReconciler struct {
	log logr.Logger
	// gvr represents the Group, Version, and Resource of the custom resource
	// this controller is responsible for.
	gvr schema.GroupVersionResource
	// client is a dynamic client for interacting with the Kubernetes API server
	client dynamic.Interface
	// runtime is the runtime representation of the ResourceGroup. It holds the
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
		igr.state.State = "DELETING"
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
		igr.state.ResourceStates[resourceID] = &ResourceState{State: "PENDING"}
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
	resourceState := &ResourceState{State: "IN_PROGRESS"}
	igr.state.ResourceStates[resourceID] = resourceState

	// Check if resource should be created
	if want, err := igr.runtime.WantToCreateResource(resourceID); err != nil || !want {
		log.V(1).Info("Skipping resource creation", "reason", err)
		resourceState.State = "SKIPPED"
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
			return igr.handleResourceCreation(ctx, rc, resource, resourceID, resourceState)
		}
		resourceState.State = "ERROR"
		resourceState.Err = fmt.Errorf("failed to get resource: %w", err)
		return resourceState.Err
	}

	// Update runtime with observed state
	igr.runtime.SetResource(resourceID, observed)

	// Check resource readiness
	if ready, reason, err := igr.runtime.IsResourceReady(resourceID); err != nil || !ready {
		log.V(1).Info("Resource not ready", "reason", reason, "error", err)
		resourceState.State = "WAITING_FOR_READINESS"
		resourceState.Err = fmt.Errorf("resource not ready: %s: %w", reason, err)
		return igr.delayedRequeue(resourceState.Err)
	}

	resourceState.State = "SYNCED"
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
		resourceState.State = "ERROR"
		resourceState.Err = fmt.Errorf("failed to create resource: %w", err)
		return resourceState.Err
	}

	resourceState.State = "CREATED"
	return igr.delayedRequeue(fmt.Errorf("awaiting resource creation completion"))
}

// updateResource handles updates to an existing resource.
// Currently performs basic state management, but could be extended to include
// more sophisticated update logic and diffing.
func (igr *instanceGraphReconciler) updateResource(
	_ context.Context,
	_ dynamic.ResourceInterface,
	_, _ *unstructured.Unstructured,
	resourceID string,
	resourceState *ResourceState,
) error {
	igr.log.V(1).Info("Processing potential resource update", "resourceID", resourceID)

	// TODO: Implement resource diffing logic
	// TODO: Add update strategy options (e.g., server-side apply)

	resourceState.State = "SYNCED"
	return nil
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
				State: "SKIPPED",
			}
			continue
		}

		// Check if resource exists
		rc := igr.getResourceClient(resourceID)
		observed, err := rc.Get(context.TODO(), resource.GetName(), metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				igr.state.ResourceStates[resourceID] = &ResourceState{
					State: "DELETED",
				}
				continue
			}
			return fmt.Errorf("failed to check resource %s existence: %w", resourceID, err)
		}

		igr.runtime.SetResource(resourceID, observed)
		igr.state.ResourceStates[resourceID] = &ResourceState{
			State: "PENDING_DELETION",
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

		if resourceState == nil || resourceState.State != "PENDING_DELETION" {
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
			igr.state.ResourceStates[resourceID].State = "DELETED"
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
		if resourceState.State != "DELETED" && resourceState.State != "SKIPPED" {
			return igr.delayedRequeue(fmt.Errorf("waiting for resource deletion completion"))
		}
	}

	// Remove finalizer from instance
	instance := igr.runtime.GetInstance()
	patched, err := igr.setUnmanaged(ctx, instance, instance.GetUID())
	if err != nil {
		return fmt.Errorf("failed to remove instance finalizer: %w", err)
	}

	igr.runtime.SetInstance(patched)
	return nil
}

// setManaged ensures the instance has the necessary finalizer and labels.
func (igr *instanceGraphReconciler) setManaged(ctx context.Context, obj *unstructured.Unstructured, uid types.UID) (*unstructured.Unstructured, error) {
	if exist, _ := metadata.HasInstanceFinalizerUnstructured(obj, uid); exist {
		return obj, nil
	}

	igr.log.V(1).Info("Setting managed state", "name", obj.GetName(), "namespace", obj.GetNamespace())

	copy := obj.DeepCopy()
	if err := metadata.SetInstanceFinalizerUnstructured(copy, uid); err != nil {
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
func (igr *instanceGraphReconciler) setUnmanaged(ctx context.Context, obj *unstructured.Unstructured, uid types.UID) (*unstructured.Unstructured, error) {
	if exist, _ := metadata.HasInstanceFinalizerUnstructured(obj, uid); !exist {
		return obj, nil
	}

	igr.log.V(1).Info("Removing managed state", "name", obj.GetName(), "namespace", obj.GetNamespace())

	copy := obj.DeepCopy()
	if err := metadata.RemoveInstanceFinalizerUnstructured(copy, uid); err != nil {
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
