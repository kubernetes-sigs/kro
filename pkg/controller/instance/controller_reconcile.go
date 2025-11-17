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
	stdruntime "runtime"
	"sync"

	"github.com/go-logr/logr"
	"golang.org/x/sync/errgroup"
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

var KROTooling = applyset.ToolingID{
	Name:    "kro",
	Version: version.GetVersionInfo().GitVersion,
}

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
	resourceState, _ := igr.state.GetResourceState(resourceID)
	if ready, reason, err := igr.runtime.IsResourceReady(resourceID); err != nil || !ready {
		log.V(1).Info("Resource not ready", "reason", reason, "error", err)
		resourceState.State = ResourceStateWaitingForReadiness
		resourceState.Err = fmt.Errorf("resource not ready: %s: %w", reason, err)
	} else {
		resourceState.State = ResourceStateSynced
	}
}

// areDependenciesReady checks if all dependencies of a resource are ready
func (igr *instanceGraphReconciler) areDependenciesReady(resourceID string) bool {
	dependencies := igr.runtime.ResourceDescriptor(resourceID).GetDependencies()

	for _, depID := range dependencies {
		// Check if dependency is resolved
		if _, state := igr.runtime.GetResource(depID); state != runtime.ResourceStateResolved {
			return false
		}

		// Check if dependency satisfies its readyWhen conditions
		if ready, _, err := igr.runtime.IsResourceReady(depID); err != nil || !ready {
			return false
		}
	}

	return true
}

// reconcileInstance handles the reconciliation of an active instance.
// Resources are processed level-by-level using a single ApplySet for the entire instance.
// This follows the ApplySet spec which uses GKNN-based inventory tracking in parent annotations.
// See https://github.com/kubernetes/enhancements/blob/master/keps/sig-cli/3659-kubectl-apply-prune/README.md#design-details-applyset-specification
func (igr *instanceGraphReconciler) reconcileInstance(ctx context.Context) error {
	instance := igr.runtime.GetInstance()
	mark := NewConditionsMarkerFor(instance)

	// Set managed state and handle instance labels
	if err := igr.setupInstance(ctx, instance); err != nil {
		return fmt.Errorf("failed to setup instance: %w", err)
	}

	mark.GraphResolved()

	// Get topological levels from the DAG
	dag := igr.runtime.DAG()
	levels, err := dag.TopologicalSortLevels()
	if err != nil {
		mark.ResourcesNotReady("failed to compute topological levels: %v", err)
		return fmt.Errorf("failed to compute topological levels: %w", err)
	}

	igr.log.V(1).Info("Processing levels", "levels", len(levels))

	// Create a single ApplySet for the entire instance
	config := applyset.Config{
		ToolLabels:   igr.instanceSubResourcesLabeler.Labels(),
		FieldManager: FieldManagerForApplyset,
		ToolingID:    KROTooling,
		Log:          igr.log,
	}

	aset, err := applyset.New(instance, igr.restMapper, igr.client, config)
	if err != nil {
		mark.ResourcesNotReady("failed to create applyset: %v", err)
		return fmt.Errorf("failed to create applyset: %w", err)
	}

	// Process each level sequentially, adding resources to the same ApplySet
	for levelNum, levelResources := range levels {
		igr.log.V(1).Info("Processing level", "level", levelNum, "resources", len(levelResources))

		if err := igr.processLevel(ctx, aset, levelNum, levelResources); err != nil {
			mark.ResourcesNotReady("failed to process level %d: %v", levelNum, err)
			return err
		}

		// We apply after each level (without pruning) to:
		// 1. Respect topological ordering - resources exist before dependents need them
		// 2. Update the ApplySet parent incrementally for failure recovery
		// 3. Synchronize runtime so later levels can reference earlier outputs
		// Final pruning happens only after all levels are successfully applied
		result, err := aset.Apply(ctx, false)
		if err != nil {
			mark.ResourcesNotReady("failed to apply level %d: %v", levelNum, err)
			return igr.delayedRequeue(fmt.Errorf("failed to apply level %d: %w", levelNum, err))
		}

		if err := result.Errors(); err != nil {
			mark.ResourcesNotReady("errors applying level %d: %v", levelNum, err)
			return igr.delayedRequeue(fmt.Errorf("failed to apply level %d: %w", levelNum, err))
		}

		// Update tracking state and runtime with the results from this apply operation.
		// We sync both the reconciliation state (for status reporting) and the runtime
		// (for CEL evaluation) with the latest cluster state of applied resources.
		for _, applied := range result.AppliedObjects {
			resourceState, _ := igr.state.GetResourceState(applied.ID)
			if applied.Error != nil {
				resourceState.State = ResourceStateError
				resourceState.Err = applied.Error
			} else {
				if applied.LastApplied != nil {
					igr.runtime.SetResource(applied.ID, applied.LastApplied)
				}
				igr.updateResourceReadiness(applied.ID)
			}
		}

		// Verify all resources in this level are ready before proceeding to the next level.
		// This enforces the dependency contract: resources must be fully ready before
		// their dependents (in later levels) are created.
		for _, resourceID := range levelResources {
			resourceState, ok := igr.state.GetResourceState(resourceID)
			if !ok || resourceState.State == ResourceStateSkipped {
				continue
			}

			if ready, reason, err := igr.runtime.IsResourceReady(resourceID); err != nil || !ready {
				mark.ResourcesInProgress("level %d resource %s not ready: %s", levelNum, resourceID, reason)
				return igr.delayedRequeue(fmt.Errorf("level %d resource %s not ready: %s", levelNum, resourceID, reason))
			}
		}

		// If the cluster modified any applied resources (via webhooks, admission controllers,
		// or defaulting), requeue to fetch the updated state. The next reconciliation will
		// have the post-mutation values available for CEL evaluation in later levels.
		if result.HasClusterMutation() {
			mark.ResourcesInProgress("level %d had cluster mutations", levelNum)
			return igr.delayedRequeue(fmt.Errorf("level %d had cluster mutations", levelNum))
		}

		// Synchronize runtime after applying this level to ensure the CEL context is up-to-date.
		// Resources in later levels may have CEL expressions that reference fields from resources
		// in this level, so we must synchronize before proceeding to the next level.
		if _, err := igr.runtime.Synchronize(); err != nil {
			mark.ResourcesNotReady("failed to synchronize after level %d: %v", levelNum, err)
			return fmt.Errorf("failed to synchronize after level %d: %w", levelNum, err)
		}

		igr.log.V(1).Info("Level processed successfully", "level", levelNum)
	}

	// Prune resources that are no longer part of the desired state
	// The ApplySet tracks inventory via GKNN in parent annotations
	// We do a final apply with prune=true to clean up orphaned resources
	pruneResult, err := aset.Apply(ctx, true)
	if err != nil {
		mark.ResourcesNotReady("failed to prune orphaned resources: %v", err)
		return fmt.Errorf("failed to prune orphaned resources: %w", err)
	}

	// Log pruned resources
	for _, pruned := range pruneResult.PrunedObjects {
		if pruned.Error != nil {
			igr.log.Error(pruned.Error, "Failed to prune resource", "resource", pruned.String())
		} else {
			igr.log.V(2).Info("Pruned resource", "resource", pruned.String())
		}
	}

	// All resources have been successfully reconciled
	mark.ResourcesReady()
	return nil
}

// processLevel processes all resources in a single topological level by adding them
// to the provided ApplySet. Resources within a level are processed in parallel using errgroup.
// The ApplySet is shared across all levels to maintain proper GKNN-based inventory tracking.
func (igr *instanceGraphReconciler) processLevel(ctx context.Context, aset applyset.Set, levelNum int, resourceIDs []string) error {
	instance := igr.runtime.GetInstance()
	mark := NewConditionsMarkerFor(instance)

	igr.log.V(1).Info("Adding resources to applyset for level", "level", levelNum, "resources", len(resourceIDs))

	unresolvedResourceID := ""
	processingErrors := make(map[string]error)
	var processingErrorsMu sync.Mutex

	// Process resources in this level in parallel using errgroup
	// All resources in the same topological level are independent by definition
	processResource := func(ctx context.Context, resourceID string) error {
		log := igr.log.WithValues("resourceID", resourceID, "level", levelNum)

		igr.state.SetResourceState(resourceID, &ResourceState{State: ResourceStatePending})

		// Mark resource as in progress
		resourceState := &ResourceState{State: ResourceStateInProgress}
		igr.state.SetResourceState(resourceID, resourceState)

		// Check if resource should be processed
		if want, err := igr.runtime.ReadyToProcessResource(resourceID); err != nil || !want {
			log.V(1).Info("Skipping resource processing", "reason", err)
			resourceState.State = ResourceStateSkipped
			igr.runtime.IgnoreResource(resourceID)
			return nil // Not an error, just skipped
		}

		// Check if the resource dependencies are resolved
		resource, state := igr.runtime.GetResource(resourceID)
		if state != runtime.ResourceStateResolved {
			resourceState.State = ResourceStateError
			resourceState.Err = fmt.Errorf("resource not resolved")
			return resourceState.Err
		}

		// Check if all dependencies are ready
		if !igr.areDependenciesReady(resourceID) {
			resourceState.State = ResourceStateError
			resourceState.Err = fmt.Errorf("dependencies not ready")
			return resourceState.Err
		}

		// Handle ExternalRefs
		if igr.runtime.ResourceDescriptor(resourceID).IsExternalRef() {
			clusterObj, err := igr.readExternalRef(ctx, resourceID, resource)
			if err != nil {
				resourceState.State = ResourceStateError
				resourceState.Err = fmt.Errorf("failed to read external ref: %w", err)
				return resourceState.Err
			}
			igr.runtime.SetResource(resourceID, clusterObj)
			igr.updateResourceReadiness(resourceID)
			resourceState.State = ResourceStateSynced
			return nil
		}

		// Regular resources go through the applyset
		applyable := applyset.ApplyableObject{
			Unstructured: resource,
			ID:           resourceID,
		}
		clusterObj, err := aset.Add(ctx, applyable)
		if err != nil {
			resourceState.State = ResourceStateError
			resourceState.Err = fmt.Errorf("failed to add resource to applyset: %w", err)
			return resourceState.Err
		}

		if clusterObj != nil {
			igr.runtime.SetResource(resourceID, clusterObj)
		}

		return nil
	}

	// Use errgroup to process all resources in parallel
	// Resources in the same level have no dependencies on each other
	g, gCtx := errgroup.WithContext(ctx)
	g.SetLimit(stdruntime.NumCPU()) // Limit parallelism to number of CPUs

	for _, resourceID := range resourceIDs {
		g.Go(func() error {
			if err := processResource(gCtx, resourceID); err != nil {
				processingErrorsMu.Lock()
				processingErrors[resourceID] = err
				processingErrorsMu.Unlock()
				return err
			}
			return nil
		})
	}

	// Wait for all resources in this level to complete
	if err := g.Wait(); err != nil {
		// At least one resource failed
	}

	// Process any errors
	for resourceID, err := range processingErrors {
		igr.log.Error(err, "Error processing resource", "resourceID", resourceID, "level", levelNum)
		if resourceState, ok := igr.state.GetResourceState(resourceID); ok {
			resourceState.State = ResourceStateError
			resourceState.Err = err
		}
		if unresolvedResourceID == "" {
			unresolvedResourceID = resourceID
		}
	}

	if unresolvedResourceID != "" {
		mark.ResourcesInProgress("waiting for resource resolution in level %d: %s", levelNum, unresolvedResourceID)
		return igr.delayedRequeue(fmt.Errorf("unresolved resource in level %d: %s", levelNum, unresolvedResourceID))
	}

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

// handleInstanceDeletion manages the deletion of an instance and its resources
// following the reverse topological order to respect dependencies.
func (igr *instanceGraphReconciler) handleInstanceDeletion(ctx context.Context) error {
	igr.log.V(1).Info("Beginning instance deletion process")
	instance := igr.runtime.GetInstance()
	mark := NewConditionsMarkerFor(instance)

	// Mark resources as being deleted
	mark.ResourcesInProgress("deleting resources in reverse topological order")

	// Initialize deletion state for all resources
	if err := igr.initializeDeletionState(); err != nil {
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
func (igr *instanceGraphReconciler) initializeDeletionState() error {
	// Iterate through all resources to check resource states
	// Order doesn't matter here - we're just gathering current state
	levels, err := igr.runtime.DAG().TopologicalSortLevels()
	if err != nil {
		return fmt.Errorf("failed to get resource IDs: %w", err)
	}

	for _, level := range levels {
		for _, resourceID := range level {
			if _, err := igr.runtime.Synchronize(); err != nil {
				return fmt.Errorf("failed to synchronize during deletion state initialization: %w", err)
			}

			resource, state := igr.runtime.GetResource(resourceID)
			if state != runtime.ResourceStateResolved {
				igr.state.SetResourceState(resourceID, &ResourceState{
					State: ResourceStateSkipped,
				})
				continue
			}

			// Check if resource exists
			rc := igr.getResourceClient(resourceID)
			observed, err := rc.Get(context.TODO(), resource.GetName(), metav1.GetOptions{})
			if err != nil {
				if apierrors.IsNotFound(err) {
					igr.state.SetResourceState(resourceID, &ResourceState{
						State: ResourceStateDeleted,
					})
					continue
				}
				return fmt.Errorf("failed to check resource %s existence: %w", resourceID, err)
			}

			igr.runtime.SetResource(resourceID, observed)
			igr.state.SetResourceState(resourceID, &ResourceState{
				State: ResourceStatePendingDeletion,
			})
		}
	}
	return nil
}

// deleteResourcesInOrder processes resource deletion in reverse topological order
// to respect dependencies between resources. Processes deletions level-by-level
// in reverse order for predictable resource cleanup.
func (igr *instanceGraphReconciler) deleteResourcesInOrder(ctx context.Context) error {
	// Get topological levels from the DAG
	graphDAG := igr.runtime.DAG()
	levels, err := graphDAG.TopologicalSortLevels()
	if err != nil {
		return fmt.Errorf("failed to compute topological levels for deletion: %w", err)
	}

	igr.log.V(1).Info("Deleting resources in reverse levels", "totalLevels", len(levels))

	// Process each level in reverse order (bottom-up for deletion)
	for i := len(levels) - 1; i >= 0; i-- {
		levelResources := levels[i]
		igr.log.V(1).Info("Deleting level", "level", i, "resources", len(levelResources))

		hasDeleting := false
		deletionErrors := make(map[string]error)
		var deletionErrorsMu sync.Mutex

		deleteResource := func(ctx context.Context, resourceID string) error {
			resourceState, ok := igr.state.GetResourceState(resourceID)
			if !ok || resourceState == nil || resourceState.State != ResourceStatePendingDeletion {
				// Resource not pending deletion, skip
				return nil
			}

			// Skip deletion for read-only resources
			if igr.runtime.ResourceDescriptor(resourceID).IsExternalRef() {
				resourceState.State = ResourceStateSkipped
				return nil
			}

			igr.log.V(2).Info("Deleting resource", "resourceID", resourceID, "level", i)
			if err := igr.deleteResource(ctx, resourceID); err != nil {
				return fmt.Errorf("failed to delete resource %s: %w", resourceID, err)
			}

			return nil
		}

		// Use errgroup to delete all resources in this level in parallel
		// Resources in the same level have no dependencies on each other
		g, gCtx := errgroup.WithContext(ctx)
		g.SetLimit(stdruntime.NumCPU())

		for _, resourceID := range levelResources {
			g.Go(func() error {
				if err := deleteResource(gCtx, resourceID); err != nil {
					deletionErrorsMu.Lock()
					deletionErrors[resourceID] = err
					deletionErrorsMu.Unlock()
					return err
				}
				return nil
			})
		}

		// Wait for all deletions in this level to complete
		if err := g.Wait(); err != nil {
			// At least one deletion failed
		}

		// Process errors
		for resourceID, err := range deletionErrors {
			igr.log.Error(err, "Error deleting resource", "resourceID", resourceID, "level", i)
			return err
		}

		// Check if any resources in this level are still deleting
		for _, resourceID := range levelResources {
			if resourceState, ok := igr.state.GetResourceState(resourceID); ok {
				if resourceState.State == InstanceStateDeleting {
					hasDeleting = true
					break
				}
			}
		}

		// If any resources in this level are still deleting, wait before proceeding to next level
		if hasDeleting {
			igr.log.V(2).Info("Resource deletion in progress for level", "level", i)
			return igr.delayedRequeue(fmt.Errorf("resource deletion in progress for level %d", i))
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
			resourceState, _ := igr.state.GetResourceState(resourceID)
			resourceState.State = ResourceStateDeleted
			return nil
		}
		resourceState, _ := igr.state.GetResourceState(resourceID)
		resourceState.State = InstanceStateError
		resourceState.Err = fmt.Errorf("failed to delete resource: %w", err)
		return resourceState.Err
	}

	// Delete initiated successfully - mark as deleting and return nil
	// The controller will requeue to check deletion status later
	resourceState, _ := igr.state.GetResourceState(resourceID)
	resourceState.State = InstanceStateDeleting
	return nil
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
