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
	"strings"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	"github.com/aws-controllers-k8s/symphony/internal/metadata"
	"github.com/aws-controllers-k8s/symphony/internal/requeue"
	"github.com/aws-controllers-k8s/symphony/internal/runtime"
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
}

// reconcile performs the reconciliation of the instance and its sub-resources.
func (igr *instanceGraphReconciler) reconcile(ctx context.Context) error {
	instance := igr.runtime.GetInstance()
	var reconcileErr error
	isDeleteEvent := !instance.GetDeletionTimestamp().IsZero()
	instanceState := "IN_PROGRESS"
	if isDeleteEvent {
		instanceState = "DELETING"
	}
	resourceStates := make(map[string]*ResourceState)

	defer func() {
		// if a requeue error is returned, we should leave the instance in IN_PROGRESS state
		switch reconcileErr.(type) {
		case *requeue.NoRequeue, *requeue.RequeueNeeded, *requeue.RequeueNeededAfter:
			// do nothing
		default:
			if reconcileErr != nil {
				instanceState = "ERROR"
			} else {
				instanceState = "ACTIVE"
			}
		}

		status := igr.prepareStatus(instanceState, reconcileErr, resourceStates)
		if err := igr.patchInstanceStatus(ctx, status); err != nil &&
			// Ignore the error if the has been deleted. This is possible because the instance
			// may have been deleted before the status is patched.
			!(isDeleteEvent && reconcileErr == nil) {
			igr.log.Error(err, "Failed to patch instance status")
		}
	}()

	// handle deletion case
	if isDeleteEvent {
		igr.log.V(1).Info("Handling instance deletion", "deletionTimestamp", instance.GetDeletionTimestamp())
		reconcileErr = igr.handleInstanceDeletion(ctx, resourceStates)
		return reconcileErr
	}

	igr.log.V(1).Info("Reconciling instance", "instance", instance)
	reconcileErr = igr.reconcileInstance(ctx, resourceStates)
	if reconcileErr == nil {
		instanceState = "ACTIVE"
	}
	return reconcileErr
}

func (igr *instanceGraphReconciler) reconcileInstance(ctx context.Context, resourceStates map[string]*ResourceState) error {
	instance := igr.runtime.GetInstance()

	patched, err := igr.setManaged(ctx, instance, instance.GetUID())
	if err != nil {
		return fmt.Errorf("failed to set managed: %w", err)
	}

	if patched != nil {
		instance.Object = patched.Object
	}

	igr.log.V(1).Info("Reconciling individual resources [following topological order]")

	// Set all resources to PENDING state
	for _, resourceID := range igr.runtime.TopologicalOrder() {
		resourceStates[resourceID] = &ResourceState{State: "PENDING"}
	}

	for _, resourceID := range igr.runtime.TopologicalOrder() {
		if err := igr.reconcileResource(ctx, resourceID, resourceStates); err != nil {
			return err
		}
		// If the resource reconciled successfully, we can now resolve dynamic variables
		// for the next resources.
		_, err := igr.runtime.Synchronize()
		if err != nil {
			return fmt.Errorf("failed to synchronize resources: %w", err)
		}
	}
	return nil
}

func (igr *instanceGraphReconciler) getResourceNamespace(resourceID string) string {
	instance := igr.runtime.GetInstance()
	resource, _ := igr.runtime.GetResource(resourceID)

	// Use the resource namespace if it is set
	namespace := resource.GetNamespace()
	if namespace == "" {
		// Use the instance namespace if the resource namespace is not set
		namespace = instance.GetNamespace()
	}
	if namespace == "" {
		// Use the default namespace if the instance namespace is not set
		namespace = metav1.NamespaceDefault
	}
	return namespace
}

func (igr *instanceGraphReconciler) reconcileResource(ctx context.Context, resourceID string, resourceStates map[string]*ResourceState) error {
	log := igr.log.WithValues("resourceID", resourceID)
	log.V(1).Info("Reconciling resource")

	resourceState := &ResourceState{State: "IN_PROGRESS"}
	resourceStates[resourceID] = resourceState

	if want, err := igr.runtime.WantToCreateResource(resourceID); err != nil || !want {
		// TODO(michaelhtm) parse error to decide whether to set terminal error
		// or move forward
		log.V(1).Info("Skipping resource creation", "reason", err)
		resourceState.State = "SKIPPED"
		igr.runtime.IgnoreResource(resourceID)
		return nil
	}

	rUnstructured, state := igr.runtime.GetResource(resourceID)
	if state != runtime.ResourceStateResolved {
		return igr.delayedRequeue(fmt.Errorf("resource %s is not resolved: %v", resourceID, state))
	}

	descriptor := igr.runtime.ResourceDescriptor(resourceID)
	gvr := igr.runtime.ResourceDescriptor(resourceID).GetGroupVersionResource()

	var rc dynamic.ResourceInterface
	var namespace string
	if descriptor.IsNamespaced() {
		namespace = igr.getResourceNamespace(resourceID)
		rc = igr.client.Resource(gvr).Namespace(namespace)
	} else {
		rc = igr.client.Resource(gvr)
	}

	log.V(1).Info("Checking resource existence", "namespace", namespace, "name", rUnstructured.GetName())
	observed, err := rc.Get(ctx, rUnstructured.GetName(), metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) || strings.Contains(err.Error(), "the server could not find the requested resource") {
			log.V(1).Info("Resource not found", "resource", resourceID, "err", err, "namespace", namespace, "name", rUnstructured.GetName())

			err := igr.createResource(ctx, rc, rUnstructured, resourceID, resourceState)
			if err != nil {
				return err
			}
			// Requeue for the next reconciliation loop
			return igr.delayedRequeue(fmt.Errorf("resource created"))
		}
		resourceState.State = "ERROR"
		resourceState.Err = fmt.Errorf("failed to get resource: %w", err)
		return resourceState.Err
	}

	igr.runtime.SetResource(resourceID, observed)

	if ready, reason, err := igr.runtime.IsResourceReady(resourceID); err != nil || !ready {
		log.V(1).Info("Resource not ready", "reason", reason, "error", err)
		resourceState.State = "WaitingForReadiness"
		// TODO(michaelhtm) parse error to ensure we have terminal/nonTerminal errors
		resourceState.Err = fmt.Errorf("resource not ready, reason: %s, error: %w", reason, err)
		return igr.delayedRequeue(resourceState.Err)
	}

	resourceState.State = "SYNCED"
	return igr.updateResource(ctx, rc, rUnstructured, observed, resourceID, resourceState)
}

func (igr *instanceGraphReconciler) createResource(ctx context.Context, rc dynamic.ResourceInterface, resource *unstructured.Unstructured, resourceID string, resourceState *ResourceState) error {
	igr.instanceSubResourcesLabeler.ApplyLabels(resource)
	/* 	resource.SetOwnerReferences([]metav1.OwnerReference{
		k8smetadata.NewInstanceOwnerReference(
			igr.runtime.Instance.GroupVersionKind(),
			igr.runtime.Instance.GetName(),
			igr.runtime.Instance.GetUID(),
		),
	}) */

	igr.log.V(1).Info("Creating resource", "resource", resourceID)

	if _, err := rc.Create(ctx, resource, metav1.CreateOptions{}); err != nil {
		resourceState.State = "ERROR"
		resourceState.Err = fmt.Errorf("failed to create resource: %w", err)
		return resourceState.Err
	}

	igr.log.V(1).Info("Resource created", "resource", resourceID)
	resourceState.State = "CREATED"
	resourceState.Err = nil
	return nil
}

func (igr *instanceGraphReconciler) updateResource(
	_ context.Context,
	_ dynamic.ResourceInterface,
	_, _ *unstructured.Unstructured,
	resourceID string,
	resourceState *ResourceState,
) error {
	igr.log.V(1).Info("Updating resource", "resource", resourceID)

	// TODO: Implement some kind of diffing mechanism to determine if the resource needs to be updated
	// There are two ways to do this:
	// 1. DFS traversal of the resource data structure and compare each field
	// 2. Use some kind of hash function to hash the resource and compare the hash

	// resourceState.State = "UPDATED"
	resourceState.Err = nil
	return nil
}

func (igr *instanceGraphReconciler) handleInstanceDeletion(ctx context.Context, resourceStates map[string]*ResourceState) error {
	instanceUnstructured := igr.runtime.GetInstance()

	igr.log.V(1).Info("Getting all resources created by Symphony")
	for _, resourceID := range igr.runtime.TopologicalOrder() {
		_, err := igr.runtime.Synchronize()
		if err != nil {
			return fmt.Errorf("failed to synchronize resources: %w", err)
		}

		resource, state := igr.runtime.GetResource(resourceID)
		if state != runtime.ResourceStateResolved {
			continue
		}

		gvr := igr.runtime.ResourceDescriptor(resourceID).GetGroupVersionResource()

		rname := resource.GetName()

		rc := igr.client.Resource(gvr).Namespace(igr.getResourceNamespace(resourceID))
		igr.log.V(1).Info("Checking if resource exists", "resource", resourceID)
		observed, err := rc.Get(ctx, rname, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				igr.log.V(1).Info("Resource not found", "resource", resourceID)
				resourceStates[resourceID] = &ResourceState{
					State: "DELETED",
					Err:   nil,
				}
				continue
			}
			igr.log.Error(err, "Failed to get resource")
			resourceStates[resourceID] = &ResourceState{
				State: "ERROR",
				Err:   fmt.Errorf("failed to get resource %s: %w", resourceID, err),
			}
			return resourceStates[resourceID].Err
		}

		igr.runtime.SetResource(resourceID, observed)
		resourceStates[resourceID] = &ResourceState{
			State: "PENDING_DELETION",
			Err:   nil,
		}
	}

	// Delete resources in reverse order
	for i := len(igr.runtime.TopologicalOrder()) - 1; i >= 0; i-- {
		resourceID := igr.runtime.TopologicalOrder()[i]
		if resourceStates[resourceID] == nil {
			continue
		}
		if resourceStates[resourceID].State == "PENDING_DELETION" {
			if err := igr.deleteResource(ctx, resourceID, resourceStates); err != nil {
				return err
			}
		}
	}

	// Check if all resources are deleted
	allResourcesDeleted := true
	for _, resourceState := range resourceStates {
		if resourceState.State != "DELETED" {
			allResourcesDeleted = false
			break
		}
	}

	if allResourcesDeleted {
		// Remove finalizer
		patched, err := igr.setUnmanaged(ctx, instanceUnstructured, instanceUnstructured.GetUID())
		if err != nil {
			return err
		}
		igr.runtime.SetInstance(patched)
		return nil
	} else {
		// Requeue for continued deletion
		return igr.delayedRequeue(fmt.Errorf("deletion in progress"))
	}
}

func (igr *instanceGraphReconciler) deleteResource(ctx context.Context, resourceID string, resourceStates map[string]*ResourceState) error {
	igr.log.V(1).Info("Deleting resource", "resource", resourceID)

	gvr := igr.runtime.ResourceDescriptor(resourceID).GetGroupVersionResource()
	rUnstructured, _ := igr.runtime.GetResource(resourceID)

	err := igr.client.Resource(gvr).Namespace(igr.getResourceNamespace(resourceID)).Delete(ctx, rUnstructured.GetName(), metav1.DeleteOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			resourceStates[resourceID] = &ResourceState{
				State: "DELETED",
				Err:   nil,
			}
			return nil
		}
		resourceStates[resourceID] = &ResourceState{
			State: "ERROR",
			Err:   fmt.Errorf("failed to delete resource: %w", err),
		}
		return resourceStates[resourceID].Err
	}

	resourceStates[resourceID] = &ResourceState{
		State: "DELETING",
		Err:   nil,
	}
	return igr.delayedRequeue(fmt.Errorf("deletion in progress"))
}

func (igr *instanceGraphReconciler) setManaged(ctx context.Context, uObj *unstructured.Unstructured, uid types.UID) (*unstructured.Unstructured, error) {
	// if the instance is already managed, do nothing
	if exist, _ := metadata.HasInstanceFinalizerUnstructured(uObj, uid); exist {
		return uObj, nil
	}

	igr.log.V(1).Info("Setting managed", "resource", uObj.GetName(), "namespace", uObj.GetNamespace())

	dc := uObj.DeepCopy()
	if err := metadata.SetInstanceFinalizerUnstructured(dc, uid); err != nil {
		return nil, fmt.Errorf("failed to set instance finalizer: %w", err)
	}

	igr.instanceLabeler.ApplyLabels(dc)

	patched, err := igr.client.Resource(igr.gvr).Namespace(uObj.GetNamespace()).Update(ctx, dc, metav1.UpdateOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to update object: %w", err)
	}

	return patched, nil
}

func (igr *instanceGraphReconciler) setUnmanaged(ctx context.Context, uObj *unstructured.Unstructured, uid types.UID) (*unstructured.Unstructured, error) {
	// if the instance is already unmanaged, do nothing
	if exist, _ := metadata.HasInstanceFinalizerUnstructured(uObj, uid); !exist {
		return uObj, nil
	}

	igr.log.V(1).Info("Setting unmanaged", "resource", uObj.GetName(), "namespace", uObj.GetNamespace())

	dc := uObj.DeepCopy()
	if err := metadata.RemoveInstanceFinalizerUnstructured(dc, uid); err != nil {
		return nil, fmt.Errorf("failed to remove instance finalizer: %w", err)
	}

	patched, err := igr.client.Resource(igr.gvr).Namespace(uObj.GetNamespace()).Update(ctx, dc, metav1.UpdateOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to update object: %w", err)
	}

	return patched, nil
}

func (igr *instanceGraphReconciler) delayedRequeue(err error) error {
	return requeue.NeededAfter(err, igr.reconcileConfig.DefaultRequeueDuration)
}
