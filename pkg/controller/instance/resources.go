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
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	apimachineryruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/kubernetes-sigs/kro/pkg/controller/instance/applyset"
	"github.com/kubernetes-sigs/kro/pkg/dynamiccontroller"
	"github.com/kubernetes-sigs/kro/pkg/graph"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
	"github.com/kubernetes-sigs/kro/pkg/runtime"
)

type resourceDeletingError struct {
	NodeID      string
	ResourceRef string
}

func (e *resourceDeletingError) Error() string {
	return fmt.Sprintf(
		"resource %q for node %q is currently being deleted; waiting for deletion to complete before continuing reconciliation",
		e.ResourceRef,
		e.NodeID,
	)
}

func newResourceDeletingError(nodeID string, obj *unstructured.Unstructured) *resourceDeletingError {
	return &resourceDeletingError{
		NodeID:      nodeID,
		ResourceRef: resourceRef(obj),
	}
}

func resourceRef(obj *unstructured.Unstructured) string {
	if obj.GetNamespace() == "" {
		return obj.GetName()
	}
	return obj.GetNamespace() + "/" + obj.GetName()
}

// reconcileNodes orchestrates node processing, apply, prune, and state updates.
func (c *Controller) reconcileNodes(rcx *ReconcileContext) error {
	rcx.Log.V(2).Info("Reconciling resources")

	applier := c.createApplySet(rcx)

	// ---------------------------------------------------------
	// 1. Process nodes (build applyset inputs)
	// ---------------------------------------------------------
	var lastUnresolvedErr error
	resources, err := c.processNodes(rcx)
	if err != nil {
		if !runtime.IsDataPending(err) {
			return err
		}
		lastUnresolvedErr = err
	}
	prune := lastUnresolvedErr == nil

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
	pruneNeedsRetry := false
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
		pruned, needsRetry, err := c.pruneOrphans(rcx, applier, result, supersetPatch, batchMeta)
		if err != nil {
			return err
		}
		clusterMutated = clusterMutated || pruned
		pruneNeedsRetry = pruneNeedsRetry || needsRetry
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

	if lastUnresolvedErr != nil {
		return rcx.delayedRequeue(fmt.Errorf("waiting for unresolved resource: %w", lastUnresolvedErr))
	}
	if pruneNeedsRetry {
		return rcx.delayedRequeue(fmt.Errorf("prune encountered UID conflicts; retrying"))
	}
	if clusterMutated {
		return rcx.delayedRequeue(fmt.Errorf("cluster mutated"))
	}

	return nil
}

// processNodes walks every runtime node, resolves desired objects, observes
// current objects from the cluster where needed, and updates runtime observations
// so subsequent nodes can become resolvable/ready/includable. It returns the
// applyset.Resource list to be applied and an aggregated error if any nodes are
// pending resolution.
func (c *Controller) processNodes(
	rcx *ReconcileContext,
) ([]applyset.Resource, error) {
	nodes := rcx.Runtime.Nodes()

	var resources []applyset.Resource

	var lastUnresolvedErr error
	for _, node := range nodes {
		resourcesToAdd, err := c.processNode(rcx, node)
		if err != nil {
			if !runtime.IsDataPending(err) {
				return nil, err
			}
			lastUnresolvedErr = err
		}
		resources = append(resources, resourcesToAdd...)
	}

	return resources, lastUnresolvedErr
}

// pruneOrphans deletes previously managed resources that are not in the current
// apply set. It shrinks parent applyset metadata only when prune completes
// without UID conflicts.
func (c *Controller) pruneOrphans(
	rcx *ReconcileContext,
	applier *applyset.ApplySet,
	result *applyset.ApplyResult,
	supersetPatch applyset.Metadata,
	batchMeta applyset.Metadata,
) (bool, bool, error) {
	pruneScope := supersetPatch.PruneScope()
	pruneResult, err := applier.Prune(rcx.Ctx, applyset.PruneOptions{
		KeepUIDs: result.ObservedUIDs(),
		Scope:    pruneScope,
	})
	if err != nil {
		return false, false, rcx.delayedRequeue(fmt.Errorf("prune failed: %w", err))
	}

	// Keep superset metadata and retry prune on UID conflicts.
	if pruneResult.HasConflicts() {
		rcx.Log.V(1).Info("prune skipped resources due to UID conflicts; keeping superset applyset metadata for retry",
			"conflicts", pruneResult.Conflicts,
		)
		return pruneResult.HasPruned(), true, nil
	}

	// Prune succeeded (errors return directly), safe to shrink metadata
	if err := c.patchInstanceWithApplySetMetadata(rcx, batchMeta); err != nil {
		rcx.Log.V(1).Info("failed to shrink instance annotations", "error", err)
	}
	return pruneResult.HasPruned(), false, nil
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
) ([]applyset.Resource, error) {
	id := node.Spec.Meta.ID
	rcx.Log.V(3).Info("Preparing resource", "id", id)

	state := rcx.StateManager.NewNodeState(id)

	ignored, err := node.IsIgnored()
	if err != nil {
		state.SetError(err)
		return nil, err
	}
	if ignored {
		state.SetSkipped()
		rcx.Log.V(2).Info("Skipping resource", "id", id, "reason", "ignored")
		return []applyset.Resource{{
			ID:        id,
			SkipApply: true,
		}}, nil
	}

	desired, err := node.GetDesired()
	if err != nil {
		if runtime.IsDataPending(err) {
			// Skip prune when any resource is unresolved to avoid deleting
			// previously managed resources that are still pending resolution.
			// Returning the unresolved ID signals the caller to disable prune.
			return nil, fmt.Errorf("gvr %q: %w", node.Spec.Meta.GVR.String(), err)
		}
		state.SetError(err)
		return nil, err
	}

	switch node.Spec.Meta.Type {
	case graph.NodeTypeExternal:
		if err := c.processExternalRefNode(rcx, node, state, desired); err != nil {
			return nil, err
		}
		return nil, nil
	case graph.NodeTypeExternalCollection:
		if err := c.processExternalCollectionNode(rcx, node, state, desired); err != nil {
			return nil, err
		}
		return nil, nil
	case graph.NodeTypeCollection:
		resources, err := c.processCollectionNode(rcx, node, state, desired)
		if err != nil {
			return nil, err
		}
		return resources, nil
	case graph.NodeTypeResource:
		resources, err := c.processRegularNode(rcx, node, state, desired)
		if err != nil {
			return nil, err
		}
		return resources, nil
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

	// Register watch BEFORE operating on the resource to avoid event gaps.
	requestWatch(rcx, id, nodeMeta.GVR, desired.GetName(), desired.GetNamespace())

	ri := resourceClientFor(rcx, nodeMeta, desired.GetNamespace())
	current, err := ri.Get(rcx.Ctx, desired.GetName(), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		current = nil
		err = nil
	}
	if err != nil {
		state.SetError(fmt.Errorf("failed to get current state for %s/%s: %w", desired.GetNamespace(), desired.GetName(), err))
		return nil, state.Err
	}

	if current != nil && current.GetDeletionTimestamp() != nil {
		state.SetDeleting()
		rcx.Log.V(1).Info("Resource is terminating; waiting for deletion to complete",
			"id", id,
			"namespace", current.GetNamespace(),
			"name", current.GetName(),
		)
		return nil, newResourceDeletingError(id, current)
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

	// Empty collection: observed is set (possibly with orphans to prune), mark ready.
	if collectionSize == 0 {
		node.SetObserved(existingItems)
		state.SetReady()
		return nil, nil
	}

	// Build lookup map for current items keyed by namespace/name.
	existingByKey := make(map[string]*unstructured.Unstructured, len(existingItems))
	for _, current := range existingItems {
		key := current.GetNamespace() + "/" + current.GetName()
		existingByKey[key] = current
	}

	for _, expandedResource := range expandedResources {
		requestWatch(rcx, id, gvr, expandedResource.GetName(), expandedResource.GetNamespace())
	}

	for _, expandedResource := range expandedResources {
		key := expandedResource.GetNamespace() + "/" + expandedResource.GetName()
		current := existingByKey[key]
		if current != nil && current.GetDeletionTimestamp() != nil {
			state.SetDeleting()
			rcx.Log.V(1).Info("Collection resource is terminating; waiting for deletion to complete",
				"id", id,
				"namespace", current.GetNamespace(),
				"name", current.GetName(),
			)
			return nil, newResourceDeletingError(id, current)
		}
	}

	// Pass unordered observed items to runtime; it will align them to desired
	// order by identity.
	node.SetObserved(existingItems)

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

	// Register watch BEFORE reading the external resource.
	requestWatch(rcx, id, node.Spec.Meta.GVR, desired.GetName(), desired.GetNamespace())

	// External refs are read-only: fetch and push into runtime for dependency/readiness.
	ri := resourceClientFor(rcx, node.Spec.Meta, desired.GetNamespace())
	actual, err := ri.Get(rcx.Ctx, desired.GetName(), metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			state.SetWaitingForReadiness(fmt.Errorf("waiting for external reference %q: %w", id, err))
			return nil
		}
		state.SetError(fmt.Errorf("external ref get %s %s/%s: %w",
			desired.GroupVersionKind().String(), desired.GetNamespace(), desired.GetName(), err))
		return state.Err
	}

	rcx.Log.V(2).Info("External reference resolved",
		"id", id,
		"gvk", desired.GroupVersionKind().String(),
		"namespace", actual.GetNamespace(),
		"name", actual.GetName(),
	)

	node.SetObserved([]*unstructured.Unstructured{actual})

	if err := node.CheckReadiness(); err != nil {
		if errors.Is(err, runtime.ErrWaitingForReadiness) {
			state.SetWaitingForReadiness(fmt.Errorf("waiting for external reference %q: %w", id, err))
			return nil
		}
		state.SetError(err)
		return err
	}
	state.SetReady()

	return nil
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
		case graph.NodeTypeResource:
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
		case graph.NodeTypeExternal, graph.NodeTypeExternalCollection:
			// External refs/collections handled before applyset.
			continue
		case graph.NodeTypeInstance:
			panic("instance node should not be in apply results")
		default:
			panic(fmt.Sprintf("unknown node type: %v", node.Spec.Meta.Type))
		}
	}

	var errs []error
	for _, state := range rcx.StateManager.NodeStates {
		if state.Err != nil && !errors.Is(state.Err, runtime.ErrWaitingForReadiness) {
			errs = append(errs, state.Err)
		}
	}
	if err := errors.Join(errs...); err != nil {
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
	if err := node.CheckReadiness(); err != nil {
		if errors.Is(err, runtime.ErrWaitingForReadiness) {
			state.SetWaitingForReadiness(fmt.Errorf("waiting for node %q: %w", node.Spec.Meta.ID, err))
			return
		}
		state.SetError(err)
		return
	}
	state.SetReady()
}

// processExternalCollectionNode reads external resources matching a label selector
// and updates node state. The selector is extracted from the resolved template
// (desired), which was resolved by the standard template pipeline.
func (c *Controller) processExternalCollectionNode(
	rcx *ReconcileContext,
	node *runtime.Node,
	state *NodeState,
	desired []*unstructured.Unstructured,
) error {
	id := node.Spec.Meta.ID
	nodeMeta := node.Spec.Meta

	if len(desired) == 0 {
		state.SetSkipped()
		return nil
	}

	// Extract the resolved selector from the template and convert to labels.Selector.
	// A missing selector means "select everything" (unfiltered list).
	var selector labels.Selector
	selectorRaw, found, err := unstructured.NestedMap(desired[0].Object, "metadata", "selector")
	if err != nil || !found {
		selector = labels.Everything()
	} else {
		ls := &metav1.LabelSelector{}
		if err := apimachineryruntime.DefaultUnstructuredConverter.FromUnstructured(selectorRaw, ls); err != nil {
			state.SetError(fmt.Errorf("failed to convert selector for %s: %w", id, err))
			return state.Err
		}
		selector, err = metav1.LabelSelectorAsSelector(ls)
		if err != nil {
			state.SetError(fmt.Errorf("invalid label selector for %s: %w", id, err))
			return state.Err
		}
	}

	// Get namespace from the resolved template. For cluster-scoped resources,
	// use empty namespace so the LIST is not scoped to a single namespace.
	ns := desired[0].GetNamespace()
	if !nodeMeta.Namespaced {
		ns = ""
	} else if ns == "" {
		// if no namespace is specified, use the namespace of the instance.
		ns = rcx.Instance.GetNamespace()
	}

	// Register collection watch with the coordinator.
	requestCollectionWatch(rcx, id, nodeMeta.GVR, ns, selector)

	// LIST external resources matching the selector.
	var list *unstructured.UnstructuredList
	if ns != "" {
		list, err = rcx.Client.Resource(nodeMeta.GVR).Namespace(ns).List(rcx.Ctx, metav1.ListOptions{
			LabelSelector: selector.String(),
		})
	} else {
		list, err = rcx.Client.Resource(nodeMeta.GVR).List(rcx.Ctx, metav1.ListOptions{
			LabelSelector: selector.String(),
		})
	}
	if err != nil {
		state.SetError(fmt.Errorf("failed to list external collection %s: %w", id, err))
		return state.Err
	}

	items := make([]*unstructured.Unstructured, len(list.Items))
	for i := range list.Items {
		items[i] = &list.Items[i]
	}

	node.SetObserved(items)

	if err := node.CheckReadiness(); err != nil {
		if errors.Is(err, runtime.ErrWaitingForReadiness) {
			state.SetWaitingForReadiness(fmt.Errorf("waiting for external collection %q: %w", id, err))
			return nil
		}
		state.SetError(err)
		return err
	}
	state.SetReady()

	rcx.Log.V(2).Info("External collection resolved",
		"id", id,
		"gvr", nodeMeta.GVR.String(),
		"count", len(items),
	)
	return nil
}

// requestWatch registers a scalar watch request with the coordinator.
func requestWatch(rcx *ReconcileContext, nodeID string, gvr schema.GroupVersionResource, name, namespace string) {
	if err := rcx.Watcher.Watch(dynamiccontroller.WatchRequest{
		NodeID:    nodeID,
		GVR:       gvr,
		Name:      name,
		Namespace: namespace,
	}); err != nil {
		rcx.Log.Error(err, "failed to register watch", "nodeID", nodeID, "gvr", gvr)
	}
}

// requestCollectionWatch registers a collection (selector-based) watch request.
func requestCollectionWatch(rcx *ReconcileContext, nodeID string, gvr schema.GroupVersionResource, namespace string, selector labels.Selector) {
	if err := rcx.Watcher.Watch(dynamiccontroller.WatchRequest{
		NodeID:    nodeID,
		GVR:       gvr,
		Namespace: namespace,
		Selector:  selector,
	}); err != nil {
		rcx.Log.Error(err, "failed to register collection watch", "nodeID", nodeID, "gvr", gvr)
	}
}
