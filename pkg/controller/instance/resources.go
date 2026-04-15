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
	"maps"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
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

// reconcileResult tracks state flowing through the reconcileNodes pipeline.
type reconcileResult struct {
	resources      []applyset.Resource
	applier        *applyset.ApplySet
	applyResult    *applyset.ApplyResult
	supersetPatch  applyset.Metadata
	batchMeta      applyset.Metadata
	unresolvedErr  error
	clusterMutated bool
	pruneRetry     bool
}

// reconcileNodes orchestrates node processing, apply, prune, and state updates.
func (c *Controller) reconcileNodes(rcx *ReconcileContext) error {
	rcx.Log.V(2).Info("Reconciling resources")

	r, err := c.buildApplyInputs(rcx)
	if err != nil {
		return err
	}
	if err := c.applyResources(rcx, r); err != nil {
		return err
	}
	if err := c.pruneIfSafe(rcx, r); err != nil {
		return err
	}
	return c.finalizeState(rcx, r)
}

// buildApplyInputs processes all nodes and projects applyset metadata.
func (c *Controller) buildApplyInputs(rcx *ReconcileContext) (*reconcileResult, error) {
	r := &reconcileResult{
		applier: c.createApplySet(rcx),
	}

	resources, err := c.processNodes(rcx)
	if err != nil {
		if !runtime.IsDataPending(err) {
			return nil, err
		}
		r.unresolvedErr = err
	}
	r.resources = resources

	r.supersetPatch, err = r.applier.Project(resources)
	if err != nil {
		return nil, rcx.delayedRequeue(fmt.Errorf("project failed: %w", err))
	}

	if err := c.patchInstanceWithApplySetMetadata(rcx, r.supersetPatch); err != nil {
		return nil, rcx.delayedRequeue(fmt.Errorf("failed to patch instance with applyset labels: %w", err))
	}

	return r, nil
}

// applyResources submits the desired resources to the cluster.
func (c *Controller) applyResources(rcx *ReconcileContext, r *reconcileResult) error {
	result, batchMeta, err := r.applier.Apply(rcx.Ctx, r.resources, applyset.ApplyMode{})
	if err != nil {
		return rcx.delayedRequeue(fmt.Errorf("apply failed: %w", err))
	}
	r.applyResult = result
	r.batchMeta = batchMeta
	r.clusterMutated = result.HasClusterMutation()
	return nil
}

// pruneIfSafe deletes orphaned resources when the desired set is fully resolved
// and apply had no per-resource errors.
//
// Prune is intentionally gated by two independent conditions:
//  1. unresolvedErr == nil -> all desired objects were resolvable (no ErrDataPending)
//  2. applyResult.Errors() == nil -> apply had no per resource errors
//
// The split is deliberate: "unresolved desired" is not an apply error, but
// pruning in that case would delete still-managed objects because they were
// omitted from the apply set.
func (c *Controller) pruneIfSafe(rcx *ReconcileContext, r *reconcileResult) error {
	if r.unresolvedErr != nil || r.applyResult.Errors() != nil {
		return nil
	}
	pruned, needsRetry, err := c.pruneOrphans(rcx, r.applier, r.applyResult, r.supersetPatch, r.batchMeta)
	if err != nil {
		return err
	}
	r.clusterMutated = r.clusterMutated || pruned
	r.pruneRetry = needsRetry
	return nil
}

// finalizeState processes apply results, updates node states, and determines
// whether the controller needs to requeue.
func (c *Controller) finalizeState(rcx *ReconcileContext, r *reconcileResult) error {
	if err := c.processApplyResults(rcx, r.applyResult); err != nil {
		return rcx.delayedRequeue(err)
	}

	// Update state manager after processing apply results.
	// This ensures StateManager.State reflects current node states
	// before the controller checks it.
	rcx.StateManager.Update()

	if r.unresolvedErr != nil {
		return rcx.delayedRequeue(fmt.Errorf("waiting for unresolved resource: %w", r.unresolvedErr))
	}
	if r.pruneRetry {
		return rcx.delayedRequeue(fmt.Errorf("prune encountered UID conflicts; retrying"))
	}
	if r.clusterMutated {
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

	var firstUnresolvedErr error
	for _, node := range nodes {
		resourcesToAdd, err := c.processNode(rcx, node)
		if err != nil {
			if !runtime.IsDataPending(err) {
				return nil, err
			}
			// surface the root error
			if firstUnresolvedErr == nil {
				firstUnresolvedErr = err
			}
		}
		resources = append(resources, resourcesToAdd...)
	}

	return resources, firstUnresolvedErr
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
//
// processNode is the single registration point for node state — all process*Node
// methods return NodeState values, and this method writes them to the StateManager.
func (c *Controller) processNode(
	rcx *ReconcileContext,
	node *runtime.Node,
) ([]applyset.Resource, error) {
	id := node.Spec.Meta.ID
	rcx.Log.V(3).Info("Preparing resource", "id", id)

	ignored, err := node.IsIgnored()
	if err != nil {
		if runtime.IsDataPending(err) {
			rcx.StateManager.SetNodeState(id, inProgressState())
			return nil, fmt.Errorf("gvr %q: %w", node.Spec.Meta.GVR.String(), err)
		}
		rcx.StateManager.SetNodeState(id, errorState(err))
		return nil, err
	}
	if ignored {
		rcx.StateManager.SetNodeState(id, skippedState())
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
			rcx.StateManager.SetNodeState(id, inProgressState())
			return nil, fmt.Errorf("gvr %q: %w", node.Spec.Meta.GVR.String(), err)
		}
		rcx.StateManager.SetNodeState(id, errorState(err))
		return nil, err
	}

	var resources []applyset.Resource
	var nodeState NodeState

	switch node.Spec.Meta.Type {
	case graph.NodeTypeExternal:
		nodeState, err = c.processExternalRefNode(rcx, node, desired)
	case graph.NodeTypeExternalCollection:
		nodeState, err = c.processExternalCollectionNode(rcx, node, desired)
	case graph.NodeTypeCollection:
		resources, nodeState, err = c.processCollectionNode(rcx, node, desired)
	case graph.NodeTypeResource:
		resources, nodeState, err = c.processRegularNode(rcx, node, desired)
	case graph.NodeTypeInstance:
		panic("instance node should not be processed for apply")
	default:
		panic(fmt.Sprintf("unknown node type: %v", node.Spec.Meta.Type))
	}

	rcx.StateManager.SetNodeState(id, nodeState)
	return resources, err
}

// processRegularNode builds applyset inputs for a single-resource node.
// Returns the resources to apply and the resulting node state.
func (c *Controller) processRegularNode(
	rcx *ReconcileContext,
	node *runtime.Node,
	desiredList []*unstructured.Unstructured,
) ([]applyset.Resource, NodeState, error) {
	id := node.Spec.Meta.ID
	nodeMeta := node.Spec.Meta

	if len(desiredList) == 0 {
		return nil, readyState(), nil
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
		return nil, errorState(fmt.Errorf("failed to get current state for %s/%s: %w", desired.GetNamespace(), desired.GetName(), err)), err
	}

	if current != nil && current.GetDeletionTimestamp() != nil {
		rcx.Log.V(1).Info("Resource is terminating; waiting for deletion to complete",
			"id", id,
			"namespace", current.GetNamespace(),
			"name", current.GetName(),
		)
		return nil, deletingState(), newResourceDeletingError(id, current)
	}

	if current != nil {
		node.SetObserved([]*unstructured.Unstructured{current})
	}

	// Add allowlisted labels & annotations from the instance first, so that
	// kro's own decorator labels always take precedence on key conflicts.
	c.applyMetadataPropagation(rcx, desired)

	// Apply decorator labels to desired object
	c.applyDecoratorLabels(rcx, desired, id, nil)

	resource := applyset.Resource{
		ID:      id,
		Object:  desired,
		Current: current,
	}

	return []applyset.Resource{resource}, inProgressState(), nil
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
	instanceLabeler := metadata.NewInstanceLabeler(rcx.Instance, c.namespaced)
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

// applyMetadataPropagation propagates labels and annotations from the RGI to a
// child resource, filtered by the patterns defined in spec.metadataPropagation.
func (c *Controller) applyMetadataPropagation(
	rcx *ReconcileContext,
	obj *unstructured.Unstructured,
) {
	if rcx.Config.MetadataPropagation == nil {
		return
	}

	// Instance labels/annotations take precedence over existing resource values
	propagateMatching(rcx.Instance.GetLabels(), rcx.Config.MetadataPropagation.Labels, obj.GetLabels, obj.SetLabels)
	propagateMatching(rcx.Instance.GetAnnotations(), rcx.Config.MetadataPropagation.Annotations, obj.GetAnnotations, obj.SetAnnotations)
}

// propagateMatching copies entries from instanceMetadata into the object metadata map
// (via getObjMetadata/setObjMetadata) for keys that match at least one pattern in allowlist.
func propagateMatching(instanceMetadata map[string]string, allowlist []string, getObjMetadata func() map[string]string, setObjMetadata func(map[string]string)) {
	if len(allowlist) == 0 {
		return
	}
	matched := make(map[string]string)
	for k, v := range instanceMetadata {
		if matchesPattern(k, allowlist) {
			matched[k] = v
		}
	}
	if len(matched) == 0 {
		return
	}
	existing := getObjMetadata()
	if existing == nil {
		existing = make(map[string]string)
	}
	maps.Copy(existing, matched)
	setObjMetadata(existing)
}

// matchesPattern returns true if key matches at least one pattern in the list.
// Patterns ending in * are treated as prefix matches (e.g. "myorg.com/*"
// matches any key starting with "myorg.com/"). All other patterns are exact matches.
func matchesPattern(key string, patterns []string) bool {
	for _, p := range patterns {
		if strings.HasSuffix(p, "*") {
			if strings.HasPrefix(key, p[:len(p)-1]) {
				return true
			}
		} else if key == p {
			return true
		}
	}
	return false
}

// patchInstanceWithApplySetMetadata applies applyset metadata to the parent instance.
func (c *Controller) patchInstanceWithApplySetMetadata(rcx *ReconcileContext, meta applyset.Metadata) error {
	inst := rcx.Instance

	// SSA is idempotent - just apply, server handles no-op if unchanged
	patchObj := instanceSSAPatch(inst)
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

		if state.State == v1alpha1.NodeStateError ||
			state.State == v1alpha1.NodeStateSkipped ||
			state.State == v1alpha1.NodeStateWaitingForReadiness {
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

	// Aggregate all node errors
	if err := rcx.StateManager.NodeErrors(); err != nil {
		return fmt.Errorf("apply results contain errors: %w", err)
	}

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
