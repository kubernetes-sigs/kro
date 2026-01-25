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
	"github.com/kubernetes-sigs/kro/pkg/graph"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
	"github.com/kubernetes-sigs/kro/pkg/runtime"
)

func (c *Controller) reconcileResources(rcx *ReconcileContext) error {
	rcx.Log.V(2).Info("Reconciling resources")

	nodes := rcx.Runtime.Nodes()
	applier := c.createApplySet(rcx)

	var resources []applyset.Resource
	var unresolved string
	prune := true

	for _, node := range nodes {
		resourcesToAdd, shouldPrune, unresolvedID, err := c.prepareResource(rcx, node)
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

	supersetPatch, err := applier.Project(resources)
	if err != nil {
		return rcx.delayedRequeue(fmt.Errorf("project failed: %w", err))
	}

	if err := c.patchInstanceWithApplySetMetadata(rcx, supersetPatch); err != nil {
		return rcx.delayedRequeue(fmt.Errorf("failed to patch instance with applyset labels: %w", err))
	}

	result, batchMeta, err := applier.Apply(rcx.Ctx, resources, applyset.ApplyMode{})
	if err != nil {
		return rcx.delayedRequeue(fmt.Errorf("apply failed: %w", err))
	}

	clusterMutated := result.HasClusterMutation()

	if prune && result.Errors() == nil {
		pruneScope := supersetPatch.PruneScope()
		pruneResult, err := applier.Prune(rcx.Ctx, applyset.PruneOptions{
			KeepUIDs: result.ObservedUIDs(),
			Scope:    pruneScope,
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

	if err := c.processApplyResults(rcx, result); err != nil {
		return rcx.delayedRequeue(err)
	}

	// Update state manager after processing apply results.
	// This ensures StateManager.State reflects current resource states
	// (including WaitingForReadiness) before the controller checks it.
	rcx.StateManager.Update()

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
		ParentNamespace: rcx.Instance.GetNamespace(),
	}
	return applyset.New(cfg, rcx.Instance)
}

func (c *Controller) prepareResource(
	rcx *ReconcileContext,
	node *runtime.Node,
) ([]applyset.Resource, bool, string, error) {
	id := node.Spec.Meta.ID
	rcx.Log.V(3).Info("Preparing resource", "id", id)

	st := &ResourceState{State: ResourceStateInProgress}
	rcx.StateManager.ResourceStates[id] = st

	ignored, err := node.IsIgnored()
	if err != nil {
		st.State = ResourceStateError
		st.Err = err
		return nil, true, "", err
	}
	if ignored {
		st.State = ResourceStateSkipped
		rcx.Log.V(2).Info("Skipping resource", "id", id, "reason", "ignored")
		return []applyset.Resource{{
			ID:        id,
			SkipApply: true,
		}}, true, "", nil
	}

	desired, err := node.GetDesired()
	if err != nil {
		if runtime.IsDataPending(err) {
			st.State = ResourceStateWaitingForReadiness
			return nil, true, id, nil
		}
		st.State = ResourceStateError
		st.Err = err
		return nil, true, "", err
	}

	// Nothing to apply (empty desired)
	if len(desired) == 0 {
		st.State = ResourceStateSkipped
		return nil, true, "", nil
	}

	switch node.Spec.Meta.Type {
	case graph.NodeTypeExternal:
		if err := c.handleExternalRef(rcx, node, st, desired); err != nil {
			return nil, true, "", err
		}
		return nil, true, "", nil
	case graph.NodeTypeCollection:
		return c.prepareCollectionResource(rcx, node, st, desired)
	default:
		return c.prepareRegularResource(rcx, node, st, desired)
	}
}

func (c *Controller) prepareRegularResource(
	rcx *ReconcileContext,
	node *runtime.Node,
	st *ResourceState,
	desiredList []*unstructured.Unstructured,
) ([]applyset.Resource, bool, string, error) {
	id := node.Spec.Meta.ID
	desc := node.Spec.Meta
	gvr := desc.GVR

	if len(desiredList) == 0 {
		st.State = ResourceStateSynced
		return nil, true, "", nil
	}
	desired := desiredList[0]

	current, err := c.getCurrentClusterState(
		rcx,
		gvr,
		desired.GetNamespace(),
		desired.GetName(),
	)
	if err != nil {
		st.State = ResourceStateError
		st.Err = err
		return nil, true, "", err
	}

	if current != nil {
		node.SetObserved([]*unstructured.Unstructured{current})
	}

	// Apply decorator labels to desired object
	c.applyDecoratorLabels(rcx, desired, id, nil)

	resource := applyset.Resource{
		ID:      id,
		Object:  desired,
		Current: current,
	}

	return []applyset.Resource{resource}, true, "", nil
}

func (c *Controller) prepareCollectionResource(
	rcx *ReconcileContext,
	node *runtime.Node,
	st *ResourceState,
	expandedResources []*unstructured.Unstructured,
) ([]applyset.Resource, bool, string, error) {
	id := node.Spec.Meta.ID
	desc := node.Spec.Meta
	gvr := desc.GVR

	collectionSize := len(expandedResources)

	// Empty collection is already handled - state would be Ready
	if collectionSize == 0 {
		st.State = ResourceStateSynced
		return nil, true, "", nil
	}

	// LIST all existing collection items with single call (more efficient than N GETs)
	existingItems, err := c.listCollectionItems(rcx, gvr, id)
	if err != nil {
		st.State = ResourceStateError
		st.Err = fmt.Errorf("failed to list collection items: %w", err)
		return nil, false, id, nil
	}

	// Pass unordered observed items to runtime; it will align them to desired
	// order by identity.
	node.SetObserved(existingItems)

	// Build lookup map for current items keyed by namespace/name.
	existingByKey := make(map[string]*unstructured.Unstructured, len(existingItems))
	for _, current := range existingItems {
		key := current.GetNamespace() + "/" + current.GetName()
		existingByKey[key] = current
	}

	// Build resources list for apply
	resources := make([]applyset.Resource, 0, collectionSize)
	for i, expandedResource := range expandedResources {
		// Apply decorator labels with collection info
		collectionInfo := &CollectionInfo{Index: i, Size: collectionSize}
		c.applyDecoratorLabels(rcx, expandedResource, id, collectionInfo)

		// Look up current revision from LIST results
		key := expandedResource.GetNamespace() + "/" + expandedResource.GetName()
		current := existingByKey[key]

		expandedID := fmt.Sprintf("%s-%d", id, i)
		resources = append(resources, applyset.Resource{
			ID:      expandedID,
			Object:  expandedResource,
			Current: current,
		})
	}

	return resources, true, "", nil
}

// listCollectionItems returns existing collection items.
// Uses a single LIST with label selector instead of N individual GETs.
func (c *Controller) listCollectionItems(
	rcx *ReconcileContext,
	gvr schema.GroupVersionResource,
	nodeID string,
) ([]*unstructured.Unstructured, error) {
	// Filter by both instance UID and node ID for precise matching
	instanceUID := string(rcx.Instance.GetUID())
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

	items := make([]*unstructured.Unstructured, len(list.Items))
	for i := range list.Items {
		items[i] = &list.Items[i]
	}
	return items, nil
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
	instanceLabeler := metadata.NewInstanceLabeler(rcx.Instance)
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
	inst := rcx.Instance

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
	node *runtime.Node,
	st *ResourceState,
	desiredList []*unstructured.Unstructured,
) error {
	id := node.Spec.Meta.ID
	if len(desiredList) == 0 {
		st.State = ResourceStateSkipped
		return nil
	}
	desired := desiredList[0]

	// External refs are read-only here: fetch and push into runtime for dependency/readiness.
	actual, err := c.readExternalRef(rcx, id, desired)
	if err != nil {
		st.State = ResourceStateWaitingForReadiness
		st.Err = fmt.Errorf("waiting for external reference %q: %w", id, err)
		return nil
	}

	node.SetObserved([]*unstructured.Unstructured{actual})

	ready, err := node.IsReady()
	if err != nil {
		st.State = ResourceStateError
		st.Err = err
		return err
	}
	if ready {
		st.State = ResourceStateSynced
		st.Err = nil
	} else {
		st.State = ResourceStateWaitingForReadiness
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
		ns := rcx.getResourceNamespace(desired)
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

func (c *Controller) processApplyResults(
	rcx *ReconcileContext,
	result *applyset.ApplyResult,
) error {
	rcx.Log.V(2).Info("Processing apply results")

	// Build nodeMap for lookups
	nodes := rcx.Runtime.Nodes()
	nodeMap := make(map[string]*runtime.Node, len(nodes))
	for _, node := range nodes {
		nodeMap[node.Spec.Meta.ID] = node
	}

	// Build map for efficient lookup
	byID := result.ByID()

	// Process all resources from apply results
	for resourceID, resourceState := range rcx.StateManager.ResourceStates {
		node, ok := nodeMap[resourceID]
		if !ok {
			continue
		}

		if resourceState.State == ResourceStateError || resourceState.State == ResourceStateSkipped {
			continue
		}

		switch node.Spec.Meta.Type {
		case graph.NodeTypeCollection:
			if err := c.updateCollectionFromApplyResults(rcx, node, resourceState, byID); err != nil {
				return err
			}
		case graph.NodeTypeExternal:
			// External refs handled before applyset.
			continue
		default:
			if item, ok := byID[resourceID]; ok {
				if item.Error != nil {
					resourceState.State = ResourceStateError
					resourceState.Err = item.Error
					rcx.Log.V(1).Info("apply error", "id", resourceID, "error", item.Error)
					continue
				}
				if item.Observed != nil {
					node.SetObserved([]*unstructured.Unstructured{item.Observed})
				}
				setStateFromReadiness(node, resourceState)
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
	_ *ReconcileContext,
	node *runtime.Node,
	resourceState *ResourceState,
	byID map[string]applyset.ApplyResultItem,
) error {
	resourceID := node.Spec.Meta.ID
	desiredItems, err := node.GetDesired()
	if err != nil || len(desiredItems) == 0 {
		resourceState.State = ResourceStateSynced
		return nil
	}

	observedItems := make([]*unstructured.Unstructured, 0, len(desiredItems))

	for i := range desiredItems {
		expandedID := fmt.Sprintf("%s-%d", resourceID, i)
		if item, ok := byID[expandedID]; ok {
			if item.Error != nil {
				resourceState.State = ResourceStateError
				resourceState.Err = fmt.Errorf("collection item %d: %w", i, item.Error)
				return nil
			}
			if item.Observed != nil {
				observedItems = append(observedItems, item.Observed)
			}
		}
	}

	node.SetObserved(observedItems)
	setStateFromReadiness(node, resourceState)
	return nil
}

func setStateFromReadiness(node *runtime.Node, st *ResourceState) {
	ready, err := node.IsReady()
	if err != nil {
		st.State = ResourceStateError
		st.Err = err
		return
	}
	if ready {
		st.State = ResourceStateSynced
		st.Err = nil
		return
	}
	st.State = ResourceStateWaitingForReadiness
	st.Err = nil
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
