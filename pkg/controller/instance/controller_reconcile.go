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

package instance

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	"github.com/kubernetes-sigs/kro/pkg/metadata"
	"github.com/kubernetes-sigs/kro/pkg/requeue"
	"github.com/kubernetes-sigs/kro/pkg/runtime"
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

	FieldManagerForLabeler = "kro.run/labeller"
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

	// restMapper is a REST mapper for the Kubernetes API server
	restMapper meta.RESTMapper

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

	switch igr.reconcileConfig.Mode {
	case v1alpha1.ResourceGraphDefinitionReconcileModeApplySet:
		return igr.handleReconciliation(ctx, igr.reconcileInstanceApplySet)
	case v1alpha1.ResourceGraphDefinitionReconcileModeClientSideDelta, "":
		return igr.handleReconciliation(ctx, igr.reconcileInstanceCSA)
	default:
		return fmt.Errorf("unsupported apply mode: %s", igr.reconcileConfig.Mode)
	}
}

// handleReconciliation provides a common wrapper for reconciliation operations,
// handling status updates and error management.
func (igr *instanceGraphReconciler) handleReconciliation(
	ctx context.Context,
	reconcileFunc func(context.Context) error,
) error {
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

func (igr *instanceGraphReconciler) updateResourceReadiness(resourceID string) {
	log := igr.log.WithValues("resourceID", resourceID)
	resourceState := igr.state.ResourceStates[resourceID]
	if ready, reason, err := igr.runtime.IsResourceReady(resourceID); err != nil || !ready {
		log.V(1).Info("Resource not ready", "reason", reason, "error", err)
		resourceState.State = ResourceStateWaitingForReadiness
		resourceState.Err = fmt.Errorf("resource not ready: %s: %w", reason, err)
	} else {
		resourceState.State = ResourceStateSynced
	}
}

// handleInstanceDeletion manages the deletion of an instance and its resources
// following the reverse topological order to respect dependencies.
func (igr *instanceGraphReconciler) handleInstanceDeletion(ctx context.Context) error {
	igr.log.V(1).Info("Beginning instance deletion process")

	// Initialize deletion state for all resources
	if err := igr.initializeDeletionState(ctx); err != nil {
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
func (igr *instanceGraphReconciler) initializeDeletionState(ctx context.Context) error {
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
		observed, err := rc.Get(ctx, resource.GetName(), metav1.GetOptions{})
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

// setUnmanaged removes the finalizer from the instance.
func (igr *instanceGraphReconciler) setUnmanaged(
	ctx context.Context,
	obj *unstructured.Unstructured,
) (*unstructured.Unstructured, error) {
	if exist, _ := metadata.HasInstanceFinalizerUnstructured(obj); !exist {
		return obj, nil
	}

	igr.log.V(1).Info("Removing managed state", "name", obj.GetName(), "namespace", obj.GetNamespace())

	instancePatch := &unstructured.Unstructured{}
	instancePatch.SetUnstructuredContent(map[string]interface{}{
		"apiVersion": obj.GetAPIVersion(),
		"kind":       obj.GetKind(),
		"metadata": map[string]interface{}{
			"name":      obj.GetName(),
			"namespace": obj.GetNamespace(),
		},
	})
	instancePatch.SetFinalizers(obj.GetFinalizers())
	if err := metadata.RemoveInstanceFinalizerUnstructured(instancePatch); err != nil {
		return nil, fmt.Errorf("failed to remove finalizer: %w", err)
	}

	updated, err := igr.client.Resource(igr.gvr).
		Namespace(obj.GetNamespace()).
		Apply(ctx, instancePatch.GetName(), instancePatch,
			metav1.ApplyOptions{FieldManager: FieldManagerForLabeler, Force: true})
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

// setManaged ensures the instance has the necessary finalizer and labels.
func (igr *instanceGraphReconciler) setManaged(
	ctx context.Context,
	obj *unstructured.Unstructured,
	_ types.UID,
) (*unstructured.Unstructured, error) {
	if exist, _ := metadata.HasInstanceFinalizerUnstructured(obj); exist {
		return obj, nil
	}

	igr.log.V(1).Info("Setting managed state", "name", obj.GetName(), "namespace", obj.GetNamespace())

	instancePatch := &unstructured.Unstructured{}
	instancePatch.SetUnstructuredContent(map[string]interface{}{
		"apiVersion": obj.GetAPIVersion(),
		"kind":       obj.GetKind(),
		"metadata": map[string]interface{}{
			"name":      obj.GetName(),
			"namespace": obj.GetNamespace(),
			"labels":    obj.GetLabels(),
		},
	})

	err := unstructured.SetNestedStringSlice(instancePatch.Object, obj.GetFinalizers(), "metadata", "finalizers")
	if err != nil {
		return nil, fmt.Errorf("failed to copy existing finalizers to patch: %w", err)
	}

	if err := metadata.SetInstanceFinalizerUnstructured(instancePatch); err != nil {
		return nil, fmt.Errorf("failed to set finalizer: %w", err)
	}

	igr.instanceLabeler.ApplyLabels(instancePatch)

	updated, err := igr.client.Resource(igr.gvr).
		Namespace(obj.GetNamespace()).
		Apply(ctx, instancePatch.GetName(), instancePatch,
			metav1.ApplyOptions{FieldManager: FieldManagerForLabeler, Force: true})
	if err != nil {
		return nil, fmt.Errorf("failed to update managed state: %w", err)
	}

	return updated, nil
}
