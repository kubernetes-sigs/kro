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
	"fmt"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/kubernetes-sigs/kro/pkg/controller/instance/applyset"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
	"github.com/kubernetes-sigs/kro/pkg/runtime"
)

func (c *Controller) reconcileResources(rcx *ReconcileContext) error {
	rcx.Log.V(2).Info("Reconciling resources")

	order := rcx.Runtime.TopologicalOrder()

	// The controller drives SSA + prune in a single pass:
	//   1) build the desired applyset resources in topo order (respecting includeWhen,
	//      readyWhen, dependencies, external refs)
	//   2) compute the superset parent patch so the instance is labeled/annotated before SSA
	//   3) apply all desired objects (SSA) and optionally prune orphans with the same applyset ID
	//   4) update runtime state/readiness from apply results and surface a "cluster mutated"
	//      requeue when SSA or prune changed the cluster
	// ---------------------------------------------------------
	// 1. Create ApplySet
	// ---------------------------------------------------------
	applier := c.createApplySet(rcx)

	// ---------------------------------------------------------
	// 2. Build resources list and track state
	// ---------------------------------------------------------
	var resources []applyset.Resource
	var unresolved string
	prune := true

	for _, id := range order {
		resourcesToAdd, shouldPrune, unresolvedID, err := c.prepareResource(rcx, id)
		if err != nil {
			return err
		}
		resources = append(resources, resourcesToAdd...)
		if !shouldPrune {
			prune = false
		}
		if unresolvedID != "" {
			unresolved = unresolvedID
		}
	}

	// ---------------------------------------------------------
	// 3. Project (get superset parent patch)
	// ---------------------------------------------------------
	supersetPatch, err := applier.Project(resources)
	if err != nil {
		return rcx.delayedRequeue(fmt.Errorf("project failed: %w", err))
	}

	// ---------------------------------------------------------
	// 4. Patch parent with superset (before apply/prune)
	// ---------------------------------------------------------
	if err := c.patchInstanceWithApplySetMetadata(rcx, supersetPatch); err != nil {
		return rcx.delayedRequeue(fmt.Errorf("failed to patch instance with applyset labels: %w", err))
	}

	// ---------------------------------------------------------
	// 5. Apply
	// ---------------------------------------------------------
	result, batchMeta, err := applier.Apply(rcx.Ctx, resources, applyset.ApplyMode{})
	if err != nil {
		return rcx.delayedRequeue(fmt.Errorf("apply failed: %w", err))
	}

	// ---------------------------------------------------------
	// 6. Prune orphans (if enabled and no apply errors)
	// ---------------------------------------------------------
	clusterMutated := result.HasClusterMutation()

	if prune && result.Errors() == nil {
		pruneResult, err := applier.Prune(rcx.Ctx, applyset.PruneOptions{
			KeepUIDs: result.ObservedUIDs(),
			Scope:    supersetPatch.PruneScope(),
		})
		if err != nil {
			return rcx.delayedRequeue(fmt.Errorf("prune failed: %w", err))
		}
		if pruneResult.HasPruned() {
			clusterMutated = true
		}
		// Prune succeeded (errors return directly), safe to shrink metadata
		if err := c.patchInstanceWithApplySetMetadata(rcx, batchMeta); err != nil {
			rcx.Log.V(1).Info("failed to shrink instance annotations", "error", err)
		}
	}

	// ---------------------------------------------------------
	// 7. Process results and update runtime state
	// ---------------------------------------------------------
	if err := c.processApplyResults(rcx, result); err != nil {
		return rcx.delayedRequeue(err)
	}

	// ---------------------------------------------------------
	// 8. Final resolution checks
	// ---------------------------------------------------------
	if unresolved != "" {
		return rcx.delayedRequeue(fmt.Errorf("waiting for unresolved resource %q", unresolved))
	}
	if clusterMutated {
		return rcx.delayedRequeue(fmt.Errorf("cluster mutated"))
	}

	return nil
}

func (c *Controller) createApplySet(rcx *ReconcileContext) *applyset.ApplySet {
	cfg := applyset.Config{
		Client:          rcx.Client,
		RESTMapper:      rcx.RestMapper,
		Log:             rcx.Log,
		ParentNamespace: rcx.Runtime.GetInstance().GetNamespace(),
	}
	return applyset.New(cfg, rcx.Runtime.GetInstance())
}

func (c *Controller) prepareResource(
	rcx *ReconcileContext,
	id string,
) ([]applyset.Resource, bool, string, error) {
	rcx.Log.V(3).Info("Preparing resource", "id", id)

	st := &ResourceState{State: ResourceStateInProgress}
	rcx.StateManager.ResourceStates[id] = st

	// 1. Should we process?
	want, err := rcx.Runtime.ReadyToProcessResource(id)
	if err != nil || !want {
		st.State = ResourceStateSkipped
		rcx.Log.V(2).Info("Skipping resource", "id", id, "reason", err)
		rcx.Runtime.IgnoreResource(id)

		// SkipApply entry: prune relies on parent annotation "memory" from previous
		// reconciles to find and delete this resource if it was previously applied.
		desired, _ := rcx.Runtime.GetResource(id)

		return []applyset.Resource{{
			ID:        id,
			Object:    desired,
			SkipApply: true,
		}}, true, "", nil
	}

	// 2. Must be resolved
	_, rstate := rcx.Runtime.GetResource(id)
	if rstate != runtime.ResourceStateResolved {
		return nil, false, id, nil
	}

	// 3. Dependencies must be ready
	if !rcx.Runtime.AreDependenciesReady(id) {
		return nil, false, id, nil
	}

	// 4. External reference - don't add to applyset
	if rcx.Runtime.ResourceDescriptor(id).IsExternalRef() {
		if err := c.handleExternalRef(rcx, id, st); err != nil {
			return nil, true, "", err
		}
		return nil, true, "", nil
	}

	// 5. Collection resource
	if rcx.Runtime.ResourceDescriptor(id).IsCollection() {
		return c.prepareCollectionResource(rcx, id, st)
	}

	// 6. Regular resource
	return c.prepareRegularResource(rcx, id, st)
}

func (c *Controller) prepareRegularResource(
	rcx *ReconcileContext,
	id string,
	st *ResourceState,
) ([]applyset.Resource, bool, string, error) {
	desired, _ := rcx.Runtime.GetResource(id)
	desc := rcx.Runtime.ResourceDescriptor(id)
	gvr := desc.GetGroupVersionResource()

	// Determine namespace: use resource's namespace, or fall back to instance namespace for namespaced resources
	namespace := desired.GetNamespace()
	if namespace == "" && desc.IsNamespaced() {
		namespace = rcx.Runtime.GetInstance().GetNamespace()
		desired.SetNamespace(namespace)
	}

	// GET current cluster state - needed for CEL expressions that reference this resource's status
	current, err := c.getCurrentClusterState(rcx, gvr, namespace, desired.GetName())
	if err != nil {
		st.State = ResourceStateError
		st.Err = err
		return nil, true, "", err
	}

	// Set current state in runtime so CEL and readiness have real status to read
	var currentRevision string
	if current != nil {
		rcx.Runtime.SetResource(id, current)
		updateReadiness(rcx, id)
		if _, err := rcx.Runtime.Synchronize(); err != nil && !runtime.IsDataPending(err) {
			return nil, true, "", err
		}
		currentRevision = current.GetResourceVersion()
	}

	// Apply decorator labels to desired object
	c.applyDecoratorLabels(rcx, desired, id, nil)

	resource := applyset.Resource{
		ID:              id,
		Object:          desired,
		CurrentRevision: currentRevision,
	}

	return []applyset.Resource{resource}, true, "", nil
}

func (c *Controller) prepareCollectionResource(
	rcx *ReconcileContext,
	id string,
	st *ResourceState,
) ([]applyset.Resource, bool, string, error) {
	expandedResources, err := rcx.Runtime.ExpandCollection(id)
	if err != nil {
		st.State = ResourceStateError
		st.Err = fmt.Errorf("failed to expand collection: %w", err)
		return nil, false, id, nil
	}

	desc := rcx.Runtime.ResourceDescriptor(id)
	gvr := desc.GetGroupVersionResource()
	collectionSize := len(expandedResources)

	// LIST all existing collection items with single call (more efficient than N GETs)
	existingByKey, err := c.listCollectionItems(rcx, gvr, id)
	if err != nil {
		st.State = ResourceStateError
		st.Err = fmt.Errorf("failed to list collection items: %w", err)
		return nil, false, id, nil
	}

	// Set current items in runtime so CEL expressions in dependent resources can resolve
	currentItems := make([]*unstructured.Unstructured, collectionSize)
	for i, expandedResource := range expandedResources {
		if expandedResource.GetNamespace() == "" && desc.IsNamespaced() {
			expandedResource.SetNamespace(rcx.Runtime.GetInstance().GetNamespace())
		}
		key := expandedResource.GetNamespace() + "/" + expandedResource.GetName()
		if current, ok := existingByKey[key]; ok {
			currentItems[i] = current
		}
	}
	rcx.Runtime.SetCollectionResources(id, currentItems)

	// Synchronize CEL expressions that may reference collection items
	if _, err := rcx.Runtime.Synchronize(); err != nil && !runtime.IsDataPending(err) {
		return nil, true, "", err
	}

	// Build resources list for apply
	var resources []applyset.Resource
	for i, expandedResource := range expandedResources {
		// Apply decorator labels with collection info
		collectionInfo := &CollectionInfo{Index: i, Size: collectionSize}
		c.applyDecoratorLabels(rcx, expandedResource, id, collectionInfo)

		// Look up current revision from LIST results
		var currentRevision string
		key := expandedResource.GetNamespace() + "/" + expandedResource.GetName()
		if current, ok := existingByKey[key]; ok {
			currentRevision = current.GetResourceVersion()
		}

		expandedID := fmt.Sprintf("%s-%d", id, i)
		resources = append(resources, applyset.Resource{
			ID:              expandedID,
			Object:          expandedResource,
			CurrentRevision: currentRevision,
		})
	}

	return resources, true, "", nil
}

// listCollectionItems returns existing collection items indexed by namespace/name.
// Uses a single LIST with label selector instead of N individual GETs.
func (c *Controller) listCollectionItems(
	rcx *ReconcileContext,
	gvr schema.GroupVersionResource,
	nodeID string,
) (map[string]*unstructured.Unstructured, error) {
	// Filter by both instance UID and node ID for precise matching
	instanceUID := string(rcx.Runtime.GetInstance().GetUID())
	selector := fmt.Sprintf("%s=%s,%s=%s",
		metadata.InstanceIDLabel, instanceUID,
		metadata.NodeIDLabel, nodeID,
	)

	// List across all namespaces - collection items may span namespaces
	list, err := rcx.Client.Resource(gvr).List(rcx.Ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		return nil, err
	}

	result := make(map[string]*unstructured.Unstructured, len(list.Items))
	for i := range list.Items {
		item := &list.Items[i]
		key := item.GetNamespace() + "/" + item.GetName()
		result[key] = item
	}
	return result, nil
}

// CollectionInfo holds collection item metadata for decorator.
type CollectionInfo struct {
	Index int
	Size  int
}

func (c *Controller) applyDecoratorLabels(
	rcx *ReconcileContext,
	obj *unstructured.Unstructured,
	nodeID string,
	collectionInfo *CollectionInfo,
) {
	labels := obj.GetLabels()
	if labels == nil {
		labels = make(map[string]string)
	}

	// Merge tool labels from labeler. On conflict (duplicate keys), log and use
	// instance labels only - this avoids panic from nil dereference.
	instanceLabeler := metadata.NewInstanceLabeler(rcx.Runtime.GetInstance())
	toolLabels, err := instanceLabeler.Merge(rcx.Labeler)
	if err != nil {
		rcx.Log.V(1).Info("label merge conflict, using instance labels only", "error", err)
		toolLabels = instanceLabeler
	}
	for k, v := range toolLabels.Labels() {
		labels[k] = v
	}

	// Add node ID label
	labels[metadata.NodeIDLabel] = nodeID

	// Add collection labels if applicable
	if collectionInfo != nil {
		labels[metadata.CollectionIndexLabel] = fmt.Sprintf("%d", collectionInfo.Index)
		labels[metadata.CollectionSizeLabel] = fmt.Sprintf("%d", collectionInfo.Size)
	}

	obj.SetLabels(labels)
}

func (c *Controller) patchInstanceWithApplySetMetadata(rcx *ReconcileContext, meta applyset.Metadata) error {
	inst := rcx.Runtime.GetInstance()

	// SSA is idempotent - just apply, server handles no-op if unchanged
	patchObj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": inst.GetAPIVersion(),
			"kind":       inst.GetKind(),
			"metadata": map[string]interface{}{
				"name":      inst.GetName(),
				"namespace": inst.GetNamespace(),
			},
		},
	}
	patchObj.SetLabels(meta.Labels())
	patchObj.SetAnnotations(meta.Annotations())

	_, err := rcx.InstanceClient().Apply(
		rcx.Ctx,
		inst.GetName(),
		patchObj,
		metav1.ApplyOptions{
			FieldManager: applyset.FieldManager + "-parent",
			Force:        true,
		},
	)
	return err
}

func (c *Controller) handleExternalRef(
	rcx *ReconcileContext,
	id string,
	st *ResourceState,
) error {
	desired, _ := rcx.Runtime.GetResource(id)

	// External refs are read-only here: fetch and push into runtime for dependency/readiness.
	actual, err := c.readExternalRef(rcx, id, desired)
	if err != nil {
		// NotFound means external ref doesn't exist yet - waiting, not error.
		// Other errors (API issues, mapping errors) are also treated as waiting
		// since the external ref may become available.
		st.State = ResourceStateWaitingForReadiness
		st.Err = fmt.Errorf("waiting for external reference %q: %w", id, err)
		return nil
	}

	rcx.Runtime.SetResource(id, actual)
	updateReadiness(rcx, id)
	if _, err = rcx.Runtime.Synchronize(); err != nil && !runtime.IsDataPending(err) {
		return err
	}
	return nil
}

func (c *Controller) readExternalRef(
	rcx *ReconcileContext,
	resourceID string,
	desired *unstructured.Unstructured,
) (*unstructured.Unstructured, error) {

	gvk := desired.GroupVersionKind()

	// 1. Map GVK → GVR
	mapping, err := c.client.RESTMapper().RESTMapping(
		gvk.GroupKind(),
		gvk.Version,
	)
	if err != nil {
		return nil, fmt.Errorf("externalRef: RESTMapping for %s: %w", gvk, err)
	}

	// 2. Determine which client to use
	var ri dynamic.ResourceInterface

	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		ns := desired.GetNamespace()
		if ns == "" {
			ns = rcx.getResourceNamespace(resourceID)
		}
		ri = c.client.Dynamic().Resource(mapping.Resource).Namespace(ns)
	} else {
		ri = c.client.Dynamic().Resource(mapping.Resource)
	}

	// 3. Fetch existing object
	name := desired.GetName()

	obj, err := ri.Get(rcx.Ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("externalRef: GET %s %s/%s: %w",
			gvk.String(), desired.GetNamespace(), name, err,
		)
	}

	rcx.Log.Info("External reference resolved",
		"resourceID", resourceID,
		"gvk", gvk.String(),
		"namespace", obj.GetNamespace(),
		"name", obj.GetName(),
	)

	return obj, nil
}

func updateReadiness(rcx *ReconcileContext, id string) {
	st := rcx.StateManager.ResourceStates[id]

	ready, reason, err := rcx.Runtime.IsResourceReady(id)
	if err != nil {
		st.State = ResourceStateWaitingForReadiness
		st.Err = fmt.Errorf("readiness check failed: %s: %w", reason, err)
	} else if !ready {
		st.State = ResourceStateWaitingForReadiness
		st.Err = fmt.Errorf("not ready: %s", reason)
	} else {
		st.State = ResourceStateSynced
		st.Err = nil
	}
}

func (c *Controller) processApplyResults(
	rcx *ReconcileContext,
	result *applyset.ApplyResult,
) error {
	rcx.Log.V(2).Info("Processing apply results")

	// Build map for efficient lookup
	byID := result.ByID()

	// Process all resources from apply results
	for resourceID, resourceState := range rcx.StateManager.ResourceStates {
		if rcx.Runtime.ResourceDescriptor(resourceID).IsCollection() {
			if err := c.updateCollectionFromApplyResults(rcx, resourceID, resourceState, byID); err != nil {
				return err
			}
		} else {
			// Regular resources
			if item, ok := byID[resourceID]; ok {
				if item.Error != nil {
					resourceState.State = ResourceStateError
					resourceState.Err = item.Error
					rcx.Log.V(1).Info("apply error", "id", resourceID, "error", item.Error)
				} else if item.Observed != nil {
					rcx.Runtime.SetResource(resourceID, item.Observed)
					updateReadiness(rcx, resourceID)

					// Re-run synchronization so dependents can see fresh status/data.
					if _, err := rcx.Runtime.Synchronize(); err != nil && !runtime.IsDataPending(err) {
						resourceState.State = ResourceStateError
						resourceState.Err = fmt.Errorf("failed to synchronize after apply: %w", err)
						continue
					}
				}
			}
		}
	}

	// Aggregate all resource errors
	if err := rcx.StateManager.ResourceErrors(); err != nil {
		return fmt.Errorf("apply results contain errors: %w", err)
	}

	return nil
}

func (c *Controller) updateCollectionFromApplyResults(
	rcx *ReconcileContext,
	resourceID string,
	resourceState *ResourceState,
	byID map[string]applyset.ApplyResultItem,
) error {
	currentResources, state := rcx.Runtime.GetCollectionResources(resourceID)
	if state != runtime.ResourceStateResolved || currentResources == nil {
		return nil
	}

	for i := range currentResources {
		expandedID := fmt.Sprintf("%s-%d", resourceID, i)
		if item, ok := byID[expandedID]; ok {
			if item.Error != nil {
				resourceState.State = ResourceStateError
				resourceState.Err = fmt.Errorf("collection item %d: %w", i, item.Error)
				return nil
			}
			if item.Observed != nil {
				currentResources[i] = item.Observed
			}
		}
	}

	rcx.Runtime.SetCollectionResources(resourceID, currentResources)
	if _, err := rcx.Runtime.Synchronize(); err != nil && !runtime.IsDataPending(err) {
		return fmt.Errorf("failed to synchronize after applying collection %s: %w", resourceID, err)
	}

	// Check collection readiness
	if ready, reason, err := rcx.Runtime.IsCollectionReady(resourceID); err != nil || !ready {
		rcx.Log.V(1).Info("Collection not ready", "resourceID", resourceID, "reason", reason, "error", err)
		resourceState.State = ResourceStateWaitingForReadiness
		resourceState.Err = fmt.Errorf("collection not ready: %s: %w", reason, err)
	} else {
		resourceState.State = ResourceStateSynced
	}
	return nil
}

// getCurrentClusterState fetches the current state of a resource from the cluster.
// Returns nil, nil if the resource doesn't exist yet (NotFound).
func (c *Controller) getCurrentClusterState(
	rcx *ReconcileContext,
	gvr schema.GroupVersionResource,
	namespace, name string,
) (*unstructured.Unstructured, error) {
	var ri dynamic.ResourceInterface
	if namespace != "" {
		ri = rcx.Client.Resource(gvr).Namespace(namespace)
	} else {
		ri = rcx.Client.Resource(gvr)
	}

	obj, err := ri.Get(rcx.Ctx, name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get current state for %s/%s: %w", namespace, name, err)
	}
	return obj, nil
}
