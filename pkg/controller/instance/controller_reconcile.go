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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	"sigs.k8s.io/release-utils/version"

	"github.com/kubernetes-sigs/kro/pkg/applyset"
	"github.com/kubernetes-sigs/kro/pkg/graph"
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

	FieldManagerForApplyset = "kro.run/applyset"
	FieldManagerForLabeler  = "kro.run/labeller"
)

var (
	KROTooling = applyset.ToolingID{
		Name:    "kro",
		Version: version.GetVersionInfo().GitVersion,
	}
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
	// rgd is a read-only reference to the Graph that the controller is
	// managing instances for.
	rgd *graph.Graph
	// instance is the instance being reconciled
	instance *unstructured.Unstructured
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
	igr.log.V(2).Info("reconciling instance")

	igr.state = newInstanceState()

	// Create runtime - if this fails, the defer in Controller.Reconcile handles status
	rgRuntime, err := igr.rgd.NewGraphRuntime(igr.instance)
	if err != nil {
		mark := NewConditionsMarkerFor(igr.instance)
		mark.GraphNotResolved("failed to create runtime resource graph definition: %v", err)
		return fmt.Errorf("failed to create runtime resource graph definition: %w", err)
	}
	igr.runtime = rgRuntime

	instance := igr.runtime.GetInstance()

	// Handle instance deletion if marked for deletion
	if !instance.GetDeletionTimestamp().IsZero() {
		igr.state.State = ResourceStateDeleting
		return igr.handleReconciliation(ctx, igr.handleInstanceDeletion)
	}

	return igr.handleReconciliation(ctx, igr.reconcileInstance)
}

// handleReconciliation provides a common wrapper for reconciliation operations.
// Status updates are handled by the defer in Controller.Reconcile.
func (igr *instanceGraphReconciler) handleReconciliation(
	ctx context.Context,
	reconcileFunc func(context.Context) error,
) error {
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

// areDependenciesReady checks if all dependencies of a resource are ready.
// It checks both resolution state (applied to K8s) and readiness conditions.
func (igr *instanceGraphReconciler) areDependenciesReady(resourceID string) bool {
	dependencies := igr.runtime.ResourceDescriptor(resourceID).GetDependencies()

	for _, depID := range dependencies {
		descriptor := igr.runtime.ResourceDescriptor(depID)

		if descriptor.IsCollection() {
			// Collections: check if expanded and applied, then check readiness
			items, state := igr.runtime.GetCollectionResources(depID)
			if state != runtime.ResourceStateResolved || items == nil {
				return false
			}
			if ready, _, err := igr.runtime.IsCollectionReady(depID); err != nil || !ready {
				return false
			}
		} else {
			// Single resources: check if resolved, then check readiness
			if _, state := igr.runtime.GetResource(depID); state != runtime.ResourceStateResolved {
				return false
			}
			if ready, _, err := igr.runtime.IsResourceReady(depID); err != nil || !ready {
				return false
			}
		}
	}

	return true
}

// reconcileInstance handles the reconciliation of an active instance
func (igr *instanceGraphReconciler) reconcileInstance(ctx context.Context) error {
	instance := igr.runtime.GetInstance()
	mark := NewConditionsMarkerFor(instance)

	// Set managed state and handle instance labels
	if err := igr.setupInstance(ctx, instance); err != nil {
		return fmt.Errorf("failed to setup instance: %w", err)
	}

	mark.GraphResolved()

	// Initialize resource states
	for _, resourceID := range igr.runtime.TopologicalOrder() {
		igr.state.ResourceStates[resourceID] = &ResourceState{State: ResourceStatePending}
	}

	config := applyset.Config{
		ToolLabels:   igr.instanceSubResourcesLabeler.Labels(),
		FieldManager: FieldManagerForApplyset,
		ToolingID:    KROTooling,
		Log:          igr.log,
	}

	aset, err := applyset.New(instance, igr.restMapper, igr.client, config)
	if err != nil {
		return igr.delayedRequeue(fmt.Errorf("failed creating an applyset: %w", err))
	}

	unresolvedResourceID := ""
	prune := true
	// Reconcile resources in topological order
	for _, resourceID := range igr.runtime.TopologicalOrder() {
		log := igr.log.WithValues("resourceID", resourceID)

		// Initialize resource state in instance state
		resourceState := &ResourceState{State: ResourceStateInProgress}
		igr.state.ResourceStates[resourceID] = resourceState

		// Check if resource should be processed (create or get)
		// TODO(barney-s): skipping on error seems un-intuitive, should we skip on CEL evaluation error?
		if want, err := igr.runtime.ReadyToProcessResource(resourceID); err != nil || !want {
			log.V(1).Info("Skipping resource processing", "reason", err)
			resourceState.State = ResourceStateSkipped
			igr.runtime.IgnoreResource(resourceID)
			continue
		}

		// Check if the resource dependencies are resolved and can be reconciled
		resource, state := igr.runtime.GetResource(resourceID)

		if state != runtime.ResourceStateResolved {
			unresolvedResourceID = resourceID
			prune = false
			break
		}

		// Check if all dependencies are ready
		if !igr.areDependenciesReady(resourceID) {
			unresolvedResourceID = resourceID
			prune = false
			break
		}

		// ExternalRefs are read-only - fetch them directly instead of adding to applyset
		if igr.runtime.ResourceDescriptor(resourceID).IsExternalRef() {
			shouldBreak, err := igr.processExternalRef(ctx, resourceID, resource, resourceState)
			if err != nil {
				return err
			}
			if shouldBreak {
				break
			}
			resourceState.State = ResourceStateSynced
			continue
		}

		// Collection resources expand into multiple resources
		if igr.runtime.ResourceDescriptor(resourceID).IsCollection() {
			shouldBreak, err := igr.processCollectionResource(ctx, resourceID, resourceState, aset, log)
			if err != nil {
				return err
			}
			if shouldBreak {
				prune = false
				break
			}
			continue
		}

		// Regular resources go through the applyset
		if err := igr.processRegularResource(ctx, resourceID, resource, aset); err != nil {
			return err
		}
	}

	result, err := aset.Apply(ctx, prune)

	// Build maps for efficient lookup
	appliedByID := make(map[string]*unstructured.Unstructured)
	errorsByID := make(map[string]error)
	for _, applied := range result.AppliedObjects {
		if applied.Error != nil {
			errorsByID[applied.ID] = applied.Error
		} else if applied.LastApplied != nil {
			appliedByID[applied.ID] = applied.LastApplied
		}
	}

	// Update all resources from apply results
	for resourceID, resourceState := range igr.state.ResourceStates {
		if igr.runtime.ResourceDescriptor(resourceID).IsCollection() {
			if err := igr.updateCollectionFromApplyResults(resourceID, resourceState, appliedByID, errorsByID); err != nil {
				return err
			}
		} else {
			// Regular resources
			if err, hasErr := errorsByID[resourceID]; hasErr {
				resourceState.State = ResourceStateError
				resourceState.Err = err
			} else if applied, ok := appliedByID[resourceID]; ok {
				igr.runtime.SetResource(resourceID, applied)
				igr.updateResourceReadiness(resourceID)
			}
		}
	}

	if err != nil {
		mark.ResourcesNotReady("failed to reconcile the apply set: %v", err)
		return igr.delayedRequeue(fmt.Errorf("failed to apply/prune resources: %w", err))
	}

	// Inspect resource states and return error if any resource is in error state
	if err := igr.state.ResourceErrors(); err != nil {
		mark.ResourcesNotReady("at least one resource reports an error: %v", err)
		return igr.delayedRequeue(err)
	}

	if err := result.Errors(); err != nil {
		mark.ResourcesNotReady("there was an error while reconciling resources in the apply set: %v", err)
		return igr.delayedRequeue(fmt.Errorf("failed to apply/prune resources: %w", err))
	}

	if unresolvedResourceID != "" {
		mark.ResourcesInProgress("waiting for resource resolution: %s", unresolvedResourceID)
		return igr.delayedRequeue(fmt.Errorf("unresolved resource: %s", unresolvedResourceID))
	}

	// If there are any cluster mutations, we need to requeue.
	if result.HasClusterMutation() {
		mark.ResourcesInProgress("reconciling cluster mutation after apply")
		return igr.delayedRequeue(fmt.Errorf("changes applied to cluster"))
	}

	// All resources have been successfully reconciled
	mark.ResourcesReady()
	return nil
}

// setupInstance prepares an instance for reconciliation by setting up necessary
// labels and managed state.
func (igr *instanceGraphReconciler) setupInstance(ctx context.Context, instance *unstructured.Unstructured) error {
	mark := NewConditionsMarkerFor(instance)

	patched, err := igr.setManaged(ctx, instance, instance.GetUID())
	if err != nil {
		mark.InstanceNotManaged("failed to setup instance: %v", err)
		return err
	}
	if patched != nil {
		instance.Object = patched.Object
		// Update runtime with the patched instance for condition management
		igr.runtime.SetInstance(patched)
		mark = NewConditionsMarkerFor(patched)
	}

	mark.InstanceManaged()
	return nil
}

// processExternalRef handles the fetching and processing of an external ref resource.
// Returns (shouldBreak, error) where shouldBreak indicates the main loop should break.
func (igr *instanceGraphReconciler) processExternalRef(
	ctx context.Context,
	resourceID string,
	resource *unstructured.Unstructured,
	resourceState *ResourceState,
) (bool, error) {
	clusterObj, err := igr.readExternalRef(ctx, resourceID, resource)
	if err != nil {
		resourceState.State = ResourceStateError
		resourceState.Err = fmt.Errorf("failed to read external ref: %w", err)
		return true, nil
	}
	igr.runtime.SetResource(resourceID, clusterObj)
	igr.updateResourceReadiness(resourceID)
	// Synchronize runtime state after each resource to re-evaluate CEL expressions
	if _, err := igr.runtime.Synchronize(); err != nil {
		return false, fmt.Errorf("failed to synchronize after reading external ref: %w", err)
	}
	resourceState.State = ResourceStateSynced
	return false, nil
}

// processRegularResource handles adding a regular resource to the applyset.
func (igr *instanceGraphReconciler) processRegularResource(
	ctx context.Context,
	resourceID string,
	resource *unstructured.Unstructured,
	aset applyset.Set,
) error {
	applyable := applyset.ApplyableObject{
		Unstructured: resource,
		ID:           resourceID,
	}
	clusterObj, err := aset.Add(ctx, applyable)
	if err != nil {
		return fmt.Errorf("failed to add resource to applyset: %w", err)
	}

	if clusterObj != nil {
		igr.runtime.SetResource(resourceID, clusterObj)
		igr.updateResourceReadiness(resourceID)
		// Synchronize runtime state after each resource to re-evaluate CEL expressions
		if _, err := igr.runtime.Synchronize(); err != nil {
			return fmt.Errorf("failed to synchronize after apply/prune: %w", err)
		}
	}
	return nil
}

// processCollectionResource handles the expansion and initial processing of a collection resource.
// Returns (shouldBreak, error) where shouldBreak indicates the main loop should break.
func (igr *instanceGraphReconciler) processCollectionResource(
	ctx context.Context,
	resourceID string,
	resourceState *ResourceState,
	aset applyset.Set,
	log logr.Logger,
) (bool, error) {
	expandedResources, err := igr.runtime.ExpandCollection(resourceID)
	if err != nil {
		resourceState.State = ResourceStateError
		resourceState.Err = fmt.Errorf("failed to expand collection: %w", err)
		return true, nil
	}

	collectionSize := len(expandedResources)
	collectionResults := make([]*unstructured.Unstructured, collectionSize)
	for i, expandedResource := range expandedResources {
		collectionLabeler := metadata.NewCollectionItemLabeler(resourceID, i, collectionSize)
		collectionLabeler.ApplyLabels(expandedResource)
		expandedID := fmt.Sprintf("%s-%d", resourceID, i)
		clusterObj, err := aset.Add(ctx, applyset.ApplyableObject{Unstructured: expandedResource, ID: expandedID})
		if err != nil {
			return false, fmt.Errorf("failed to add expanded resource %s to applyset: %w", expandedID, err)
		}
		collectionResults[i] = clusterObj
	}

	// Set collection resources and check readiness (consistent with regular resources)
	igr.runtime.SetCollectionResources(resourceID, collectionResults)
	if _, err := igr.runtime.Synchronize(); err != nil {
		return false, fmt.Errorf("failed to synchronize after expanding collection %s: %w", resourceID, err)
	}

	// Check readiness - if not ready, we'll check again after apply with observed objects
	if ready, reason, err := igr.runtime.IsCollectionReady(resourceID); err != nil || !ready {
		log.V(1).Info("Collection not ready", "reason", reason, "error", err)
		resourceState.State = ResourceStateWaitingForReadiness
		if err != nil {
			resourceState.Err = err
		}
	}
	return false, nil
}

// updateCollectionFromApplyResults updates a collection resource from apply results.
func (igr *instanceGraphReconciler) updateCollectionFromApplyResults(
	resourceID string,
	resourceState *ResourceState,
	appliedByID map[string]*unstructured.Unstructured,
	errorsByID map[string]error,
) error {
	currentResources, state := igr.runtime.GetCollectionResources(resourceID)
	if state != runtime.ResourceStateResolved || currentResources == nil {
		return nil
	}

	for i := range currentResources {
		expandedID := fmt.Sprintf("%s-%d", resourceID, i)
		if err, hasErr := errorsByID[expandedID]; hasErr {
			resourceState.State = ResourceStateError
			resourceState.Err = fmt.Errorf("collection item %d: %w", i, err)
			return nil
		}
		if applied, ok := appliedByID[expandedID]; ok {
			currentResources[i] = applied
		}
	}

	igr.runtime.SetCollectionResources(resourceID, currentResources)
	if _, err := igr.runtime.Synchronize(); err != nil {
		return fmt.Errorf("failed to synchronize after applying collection %s: %w", resourceID, err)
	}

	// Check collection readiness (consistent with regular resources via updateResourceReadiness)
	if ready, reason, err := igr.runtime.IsCollectionReady(resourceID); err != nil || !ready {
		igr.log.V(1).Info("Collection not ready", "resourceID", resourceID, "reason", reason, "error", err)
		resourceState.State = ResourceStateWaitingForReadiness
		// Always set Err (like updateResourceReadiness) - triggers requeue via ResourceErrors()
		resourceState.Err = fmt.Errorf("collection not ready: %s: %w", reason, err)
	} else {
		resourceState.State = ResourceStateSynced
	}
	return nil
}

// handleInstanceDeletion manages the deletion of an instance and its resources
// following the reverse topological order to respect dependencies.
func (igr *instanceGraphReconciler) handleInstanceDeletion(ctx context.Context) error {
	igr.log.V(1).Info("Beginning instance deletion process")
	instance := igr.runtime.GetInstance()
	mark := NewConditionsMarkerFor(instance)

	// Mark resources as being deleted
	mark.ResourcesInProgress("deleting resources in reverse topological order")

	// Initialize deletion state for all resources
	if err := igr.initializeDeletionState(ctx); err != nil {
		mark.ResourcesNotReady("failed to initialize deletion state: %v", err)
		return fmt.Errorf("failed to initialize deletion state: %w", err)
	}

	// Delete resources in reverse order
	if err := igr.deleteResourcesInOrder(ctx); err != nil {
		mark.ResourcesNotReady("failed to delete resources: %v", err)
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

		descriptor := igr.runtime.ResourceDescriptor(resourceID)

		// Handle collection resources differently - they expand to multiple K8s resources
		if descriptor.IsCollection() {
			gvr := descriptor.GetGroupVersionResource()
			instance := igr.runtime.GetInstance()
			selector := fmt.Sprintf("%s=%s,%s=%s",
				metadata.NodeIDLabel, resourceID,
				metadata.InstanceIDLabel, string(instance.GetUID()),
			)

			// List across all namespaces since collection items may be created in
			// different namespaces via template expressions. The label selector
			// ensures we only find resources belonging to this instance.
			var list *unstructured.UnstructuredList
			var err error
			list, err = igr.client.Resource(gvr).List(ctx, metav1.ListOptions{
				LabelSelector: selector,
			})
			if err != nil {
				return fmt.Errorf("failed to get collection items for %s: %w", resourceID, err)
			}

			if len(list.Items) == 0 {
				igr.state.ResourceStates[resourceID] = &ResourceState{
					State: ResourceStateDeleted,
				}
				continue
			}

			items := make([]*unstructured.Unstructured, len(list.Items))
			for i := range list.Items {
				items[i] = &list.Items[i]
			}
			igr.runtime.SetCollectionResources(resourceID, items)

			igr.state.ResourceStates[resourceID] = &ResourceState{
				State: ResourceStatePendingDeletion,
			}
			continue
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

	descriptor := igr.runtime.ResourceDescriptor(resourceID)

	// Handle collection resources - delete each item
	if descriptor.IsCollection() {
		items, _ := igr.runtime.GetCollectionResources(resourceID)
		if len(items) == 0 {
			igr.state.ResourceStates[resourceID].State = ResourceStateDeleted
			return nil
		}

		gvr := descriptor.GetGroupVersionResource()
		allDeleted := true
		for _, item := range items {
			// Use the item's namespace if set; otherwise fall back to instance namespace for namespaced resources
			ns := item.GetNamespace()
			if ns == "" && descriptor.IsNamespaced() {
				ns = igr.runtime.GetInstance().GetNamespace()
			}
			var rc dynamic.ResourceInterface
			base := igr.client.Resource(gvr)
			if descriptor.IsNamespaced() {
				rc = base.Namespace(ns)
			} else {
				rc = base
			}
			err := rc.Delete(ctx, item.GetName(), metav1.DeleteOptions{})
			if err != nil {
				if apierrors.IsNotFound(err) {
					continue
				}
				igr.state.ResourceStates[resourceID].State = InstanceStateError
				igr.state.ResourceStates[resourceID].Err = fmt.Errorf("failed to delete collection item %s: %w", item.GetName(), err)
				return igr.state.ResourceStates[resourceID].Err
			}
			allDeleted = false
		}

		if allDeleted {
			igr.state.ResourceStates[resourceID].State = ResourceStateDeleted
			return nil
		}

		igr.state.ResourceStates[resourceID].State = InstanceStateDeleting
		return igr.delayedRequeue(fmt.Errorf("collection deletion in progress"))
	}

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

	// All resources are deleted, mark as ready for finalization
	instance := igr.runtime.GetInstance()
	mark := NewConditionsMarkerFor(instance)

	// Remove finalizer from instance
	patched, err := igr.setUnmanaged(ctx, instance)
	if err != nil {
		mark.InstanceNotManaged("failed to remove instance finalizer: %v", err)
		return fmt.Errorf("failed to remove instance finalizer: %w", err)
	}

	igr.runtime.SetInstance(patched)
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

// readExternalRef fetches an external reference from the cluster.
// External references are resources that exist outside of this instance's control.
func (igr *instanceGraphReconciler) readExternalRef(ctx context.Context, resourceID string, resource *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	gvk := resource.GroupVersionKind()
	restMapping, err := igr.restMapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return nil, fmt.Errorf("failed to get REST mapping for %v: %w", gvk, err)
	}

	var dynResource dynamic.ResourceInterface
	if restMapping.Scope.Name() == meta.RESTScopeNameNamespace {
		namespace := igr.getResourceNamespace(resourceID)
		dynResource = igr.client.Resource(restMapping.Resource).Namespace(namespace)
	} else {
		dynResource = igr.client.Resource(restMapping.Resource)
	}

	clusterObj, err := dynResource.Get(ctx, resource.GetName(), metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get external ref %s/%s: %w", resource.GetNamespace(), resource.GetName(), err)
	}

	igr.log.V(2).Info("read external ref", "gvk", gvk, "namespace", resource.GetNamespace(), "name", resource.GetName())
	return clusterObj, nil
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
