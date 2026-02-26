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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/kubernetes-sigs/kro/pkg/controller/instance/applyset"
	"github.com/kubernetes-sigs/kro/pkg/graph"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
	"github.com/kubernetes-sigs/kro/pkg/runtime"
)

// reconcileNodes orchestrates node processing, apply, prune, and state updates.
func (c *Controller) reconcileNodes(rcx *ReconcileContext) error {
	rcx.Log.V(2).Info("Reconciling resources")

	applier := c.createApplySet(rcx)

	// ---------------------------------------------------------
	// 1. Process nodes (build applyset inputs)
	// ---------------------------------------------------------
	resources, lastUnresolved, err := c.processNodes(rcx)
	if err != nil {
		return err
	}
	prune := lastUnresolved == ""

	// ---------------------------------------------------------
	// 2. Project applyset metadata and patch parent
	// ---------------------------------------------------------
	supersetPatch, err := applier.Project(resources)
	if err != nil {
		return rcx.delayedRequeue(fmt.Errorf("project failed: %w", err))
	}

	if err := c.patchInstanceWithApplySetMetadata(rcx, supersetPatch); err != nil {
		return rcx.delayedRequeue(fmt.Errorf("failed to patch instance with applyset labels: %w", err))
	}

	// ---------------------------------------------------------
	// 3. Apply desired resources
	// ---------------------------------------------------------
	result, batchMeta, err := applier.Apply(rcx.Ctx, resources, applyset.ApplyMode{})
	if err != nil {
		return rcx.delayedRequeue(fmt.Errorf("apply failed: %w", err))
	}

	// clusterMutated tracks any cluster-side change from apply and/or prune.
	// NOTE: it must start from apply results and only ever be OR-ed with
	// prune outcomes. Be careful overwrriting this later, as we may drop the
	// "apply changed the cluster" signal and skip the requeue needed for CEL
	// refresh.
	clusterMutated := result.HasClusterMutation()

	// ---------------------------------------------------------
	// 4. Prune orphans (when desired is fully resolved)
	// ---------------------------------------------------------
	//
	// Prune is intentionally gated by two independent conditions:
	//   1) prune == true  -> all desired objects were resolvable (no ErrDataPending)
	//   2) result.Errors() == nil -> apply had no per resource errors
	//
	// The split is deliberate: "unresolved desired" is not an apply error, but
	// pruning in that case would delete still-managed objects because they were
	// omitted from the apply set. Keeping both checks visible prevents  regressions
	// where one gate gets removed and prune becomes unsafe.
	if prune && result.Errors() == nil {
		pruned, err := c.pruneOrphans(rcx, applier, result, supersetPatch, batchMeta)
		if err != nil {
			return err
		}
		clusterMutated = clusterMutated || pruned
	}

	// ---------------------------------------------------------
	// 5. Process results and update node state
	// ---------------------------------------------------------
	if err := c.processApplyResults(rcx, result); err != nil {
		return rcx.delayedRequeue(err)
	}

	// Update state manager after processing apply results.
	// This ensures StateManager.State reflects current node states
	// (including WaitingForReadiness) before the controller checks it.
	rcx.StateManager.Update()

	if lastUnresolved != "" {
		return rcx.delayedRequeue(fmt.Errorf("waiting for unresolved resource %q", lastUnresolved))
	}
	if clusterMutated {
		return rcx.delayedRequeue(fmt.Errorf("cluster mutated"))
	}

	return nil
}

// processNodes walks every runtime node, resolves desired objects, observes
// current objects from the cluster where needed, and updates runtime observations
// so subsequent nodes can become resolvable/ready/includable. It returns the
// applyset.Resource list to be applied and reports the last unresolved node ID
// (when desired data is pending) so callers can gate pruning.
func (c *Controller) processNodes(
	rcx *ReconcileContext,
) ([]applyset.Resource, string, error) {
	nodes := rcx.Runtime.Nodes()

	var resources []applyset.Resource
	var lastUnresolved string

	for _, node := range nodes {
		resourcesToAdd, unresolvedID, err := c.processNode(rcx, node)
		if err != nil {
			return nil, "", err
		}
		resources = append(resources, resourcesToAdd...)
		if unresolvedID != "" {
			lastUnresolved = unresolvedID
		}
	}

	return resources, lastUnresolved, nil
}

// pruneOrphans deletes previously managed resources that are not in the current
// apply set and then shrinks parent applyset metadata.
func (c *Controller) pruneOrphans(
	rcx *ReconcileContext,
	applier *applyset.ApplySet,
	result *applyset.ApplyResult,
	supersetPatch applyset.Metadata,
	batchMeta applyset.Metadata,
) (bool, error) {
	pruneScope := supersetPatch.PruneScope()
	pruneResult, err := applier.Prune(rcx.Ctx, applyset.PruneOptions{
		KeepUIDs: result.ObservedUIDs(),
		Scope:    pruneScope,
	})
	if err != nil {
		return false, rcx.delayedRequeue(fmt.Errorf("prune failed: %w", err))
	}

	// Prune succeeded (errors return directly), safe to shrink metadata
	if err := c.patchInstanceWithApplySetMetadata(rcx, batchMeta); err != nil {
		rcx.Log.V(1).Info("failed to shrink instance annotations", "error", err)
	}
	return pruneResult.HasPruned(), nil
}

// createApplySet constructs an applyset configured for the current instance.
func (c *Controller) createApplySet(rcx *ReconcileContext) *applyset.ApplySet {
	cfg := applyset.Config{
		Client:          rcx.Client,
		RESTMapper:      rcx.RestMapper,
		Log:             rcx.Log,
		ParentNamespace: rcx.Instance.GetNamespace(),
	}
	return applyset.New(cfg, rcx.Instance)
}

// processNode resolves a single node into applyset inputs.
// It evaluates includeWhen, resolves desired objects (or returns an unresolved
// marker when data is pending), reads existing cluster state where required,
// and updates runtime observations so other nodes can become resolvable/ready/
// includable. It produces the applyset.Resource entries for that node.
func (c *Controller) processNode(
	rcx *ReconcileContext,
	node *runtime.Node,
) ([]applyset.Resource, string, error) {
	id := node.Spec.Meta.ID
	rcx.Log.V(3).Info("Preparing resource", "id", id)

	state := rcx.StateManager.NewNodeState(id)

	ignored, err := node.IsIgnored()
	if err != nil {
		state.SetError(err)
		return nil, "", err
	}
	if ignored {
		state.SetSkipped()
		rcx.Log.V(2).Info("Skipping resource", "id", id, "reason", "ignored")
		return []applyset.Resource{{
			ID:        id,
			SkipApply: true,
		}}, "", nil
	}

	desired, err := node.GetDesired()
	if err != nil {
		if runtime.IsDataPending(err) {
			// Skip prune when any resource is unresolved to avoid deleting
			// previously managed resources that are still pending resolution.
			// Returning the unresolved ID signals the caller to disable prune.
			return nil, id, nil
		}
		state.SetError(err)
		return nil, "", err
	}

	switch node.Spec.Meta.Type {
	case graph.NodeTypeExternal:
		if err := c.processExternalRefNode(rcx, node, state, desired); err != nil {
			return nil, "", err
		}
		return nil, "", nil
	case graph.NodeTypeCollection:
		resources, err := c.processCollectionNode(rcx, node, state, desired)
		if err != nil {
			return nil, "", err
		}
		return resources, "", nil
	case graph.NodeTypeScalar:
		resources, err := c.processRegularNode(rcx, node, state, desired)
		if err != nil {
			return nil, "", err
		}
		return resources, "", nil
	case graph.NodeTypeInstance:
		panic("instance node should not be processed for apply")
	default:
		panic(fmt.Sprintf("unknown node type: %v", node.Spec.Meta.Type))
	}
}

// processRegularNode builds applyset inputs for a single-resource node.
func (c *Controller) processRegularNode(
	rcx *ReconcileContext,
	node *runtime.Node,
	state *NodeState,
	desiredList []*unstructured.Unstructured,
) ([]applyset.Resource, error) {
	id := node.Spec.Meta.ID
	nodeMeta := node.Spec.Meta

	if len(desiredList) == 0 {
		state.SetReady()
		return nil, nil
	}
	desired := desiredList[0]

	ri := resourceClientFor(rcx, nodeMeta, desired.GetNamespace())
	current, err := ri.Get(rcx.Ctx, desired.GetName(), metav1.GetOptions{})
	if errors.IsNotFound(err) {
		current = nil
		err = nil
	}
	if err != nil {
		state.SetError(fmt.Errorf("failed to get current state for %s/%s: %w", desired.GetNamespace(), desired.GetName(), err))
		return nil, state.Err
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

	return []applyset.Resource{resource}, nil
}

// processCollectionNode builds applyset inputs for a collection node and
// aligns observed items to desired items.
func (c *Controller) processCollectionNode(
	rcx *ReconcileContext,
	node *runtime.Node,
	state *NodeState,
	expandedResources []*unstructured.Unstructured,
) ([]applyset.Resource, error) {
	id := node.Spec.Meta.ID
	nodeMeta := node.Spec.Meta
	gvr := nodeMeta.GVR

	collectionSize := len(expandedResources)

	// LIST all existing collection items with single call (more efficient than N GETs)
	existingItems, err := c.listCollectionItems(rcx, gvr, id)
	if err != nil {
		state.SetError(fmt.Errorf("failed to list collection items: %w", err))
		return nil, state.Err
	}

	// Pass unordered observed items to runtime; it will align them to desired
	// order by identity.
	node.SetObserved(existingItems)

	// Empty collection: observed is set (possibly with orphans to prune), mark ready.
	if collectionSize == 0 {
		state.SetReady()
		return nil, nil
	}

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

	return resources, nil
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

// applyDecoratorLabels merges tool labels and adds node/collection identifiers.
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
	nodeLabeler := metadata.NewNodeLabeler()
	merged, err := instanceLabeler.Merge(nodeLabeler)
	if err != nil {
		rcx.Log.V(1).Info("label merge conflict between instance and node labeler, using instance labels only", "error", err)
		merged = instanceLabeler
	}
	toolLabels, err := merged.Merge(rcx.Labeler)
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

// patchInstanceWithApplySetMetadata applies applyset metadata to the parent instance.
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

// processExternalRefNode reads an external ref object and updates node state.
func (c *Controller) processExternalRefNode(
	rcx *ReconcileContext,
	node *runtime.Node,
	state *NodeState,
	desiredList []*unstructured.Unstructured,
) error {
	id := node.Spec.Meta.ID
	if len(desiredList) == 0 {
		state.SetSkipped()
		return nil
	}
	desired := desiredList[0]

	// External refs are read-only here: fetch and push into runtime for dependency/readiness.
	actual, err := c.readExternalRefNode(rcx, node, desired)
	if err != nil {
		if errors.IsNotFound(err) {
			state.SetWaitingForReadiness(fmt.Errorf("waiting for external reference %q: %w", id, err))
			return nil
		}
		state.SetError(err)
		return err
	}

	node.SetObserved([]*unstructured.Unstructured{actual})

	ready, err := node.IsReady()
	if err != nil {
		state.SetError(err)
		return err
	}
	if ready {
		state.SetReady()
	} else {
		state.SetWaitingForReadiness(nil)
	}
	return nil
}

// readExternalRefNode fetches the referenced object for an external node.
func (c *Controller) readExternalRefNode(
	rcx *ReconcileContext,
	node *runtime.Node,
	desired *unstructured.Unstructured,
) (*unstructured.Unstructured, error) {
	nodeMeta := node.Spec.Meta
	ri := resourceClientFor(rcx, nodeMeta, desired.GetNamespace())

	name := desired.GetName()
	obj, err := ri.Get(rcx.Ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("external ref get %s %s/%s: %w",
			desired.GroupVersionKind().String(), desired.GetNamespace(), name, err,
		)
	}

	rcx.Log.V(2).Info("External reference resolved",
		"id", node.Spec.Meta.ID,
		"gvk", desired.GroupVersionKind().String(),
		"namespace", obj.GetNamespace(),
		"name", obj.GetName(),
	)

	return obj, nil
}

// processApplyResults updates runtime observations and node states from apply results.
// It maps per-item results back to nodes (including collections) and records
// errors surfaced by apply.
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
	for nodeID, state := range rcx.StateManager.NodeStates {
		node, ok := nodeMap[nodeID]
		if !ok {
			continue
		}

		if state.State == NodeStateError ||
			state.State == NodeStateSkipped ||
			state.State == NodeStateWaitingForReadiness {
			continue
		}

		switch node.Spec.Meta.Type {
		case graph.NodeTypeCollection:
			if err := c.updateCollectionFromApplyResults(rcx, node, state, byID); err != nil {
				return err
			}
		case graph.NodeTypeScalar:
			if item, ok := byID[nodeID]; ok {
				if item.Error != nil {
					state.SetError(item.Error)
					rcx.Log.V(1).Info("apply error", "id", nodeID, "error", item.Error)
					continue
				}
				if item.Observed != nil {
					node.SetObserved([]*unstructured.Unstructured{item.Observed})
				}
				setStateFromReadiness(node, state)
			}
		case graph.NodeTypeExternal:
			// External refs handled before applyset.
			continue
		case graph.NodeTypeInstance:
			panic("instance node should not be in apply results")
		default:
			panic(fmt.Sprintf("unknown node type: %v", node.Spec.Meta.Type))
		}
	}

	// Aggregate all node errors
	if err := rcx.StateManager.NodeErrors(); err != nil {
		return fmt.Errorf("apply results contain errors: %w", err)
	}

	return nil
}

// updateCollectionFromApplyResults maps per-item apply results back to the
// collection node and refreshes the observed list in runtime.
func (c *Controller) updateCollectionFromApplyResults(
	_ *ReconcileContext,
	node *runtime.Node,
	state *NodeState,
	byID map[string]applyset.ApplyResultItem,
) error {
	nodeID := node.Spec.Meta.ID
	// Re-evaluate desired for collections when processing apply results:
	// - Any non-pending error is a real failure (bad expression, missing field, etc.),
	//   so we mark ERROR and stop.
	// - An empty resolved collection (len==0) is correct by design and is treated
	//   as SYNCED/ready because there is nothing to apply.
	// - Otherwise we expect item-level apply results and proceed to reconcile them.
	desiredItems, err := node.GetDesired()
	if err != nil {
		if runtime.IsDataPending(err) {
			return nil
		}
		state.SetError(err)
		return err
	}
	if len(desiredItems) == 0 {
		state.SetReady()
		return nil
	}

	observedItems := make([]*unstructured.Unstructured, 0, len(desiredItems))

	for i := range desiredItems {
		expandedID := fmt.Sprintf("%s-%d", nodeID, i)
		if item, ok := byID[expandedID]; ok {
			if item.Error != nil {
				state.SetError(fmt.Errorf("collection item %d: %w", i, item.Error))
				return nil
			}
			if item.Observed != nil {
				observedItems = append(observedItems, item.Observed)
			}
		}
	}

	node.SetObserved(observedItems)
	setStateFromReadiness(node, state)
	return nil
}

// setStateFromReadiness evaluates node readiness and updates the node state
// to synced, waiting, or error.
func setStateFromReadiness(node *runtime.Node, state *NodeState) {
	ready, err := node.IsReady()
	if err != nil {
		state.SetError(err)
		return
	}
	if ready {
		state.SetReady()
		return
	}
	state.SetWaitingForReadiness(nil)
}
