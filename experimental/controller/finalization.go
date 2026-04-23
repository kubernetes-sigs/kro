// finalization.go implements the finalization state machine described in
// 005-reconciliation.md § Finalization.
//
// Finalization gates target deletion on successful creation and readiness of
// "finalizer" resources. It runs as a separate phase BEFORE the prune walk's
// deletion decisions. The prune walk receives completedTargets (safe to delete)
// and protectedKeys (must not prune) from finalization.
//
// State machine phases:
//
//	Creating     — some finalizer children don't exist yet
//	WaitingReady — all children exist, waiting for readyWhen
//	Complete     — all children satisfy readyWhen; target may be deleted
//
// State persists on instanceState.activeFinalization across reconcile cycles.
package graphcontroller

import (
	"context"
	"fmt"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// FinalizationPhase represents the current state of a finalization sequence.
type FinalizationPhase string

const (
	FinalizationCreating     FinalizationPhase = "Creating"
	FinalizationWaitingReady FinalizationPhase = "WaitingReady"
	FinalizationComplete     FinalizationPhase = "Complete"
)

// finalizationEntry tracks a single in-flight finalization sequence.
// Persisted on instanceState.activeFinalization across reconciles.
type finalizationEntry struct {
	Phase     FinalizationPhase
	ChildKeys []string // resource keys of created finalization children
}

// finalizationResult carries the output of advanceFinalization back to the
// coordinator. The prune walk uses these to make deletion decisions without
// any finalization logic of its own.
type finalizationResult struct {
	// CompletedTargets: keys whose finalization is complete — safe to delete.
	CompletedTargets map[string]bool
	// ProtectedKeys: keys that must not be pruned (active children + static deps).
	ProtectedKeys map[string]bool
	// ChildKeysToCleanup: maps target key → child keys to delete AFTER
	// the target is successfully deleted by the prune walk.
	ChildKeysToCleanup map[string][]string
	// BlockedReasons: TeardownBlocked messages for in-progress sequences.
	BlockedReasons []string
	// Notes: informational messages (FinalizerSkipped — target absent).
	Notes []string
	// DeferredTargets: target keys that can't be deleted yet.
	DeferredTargets []string
}

// advanceFinalization processes all prune candidates that have finalizer nodes.
// It advances each target's state machine and produces a result the prune walk
// consumes. Called BEFORE the prune walk's deletion loop.
//
// The prune walk only needs to:
//   - Skip protectedKeys
//   - Delete targets in completedTargets (finalization done)
//   - Skip targets with finalizers that aren't in completedTargets
//   - Clean up children after successful target deletion
func (r *GraphReconciler) advanceFinalization(
	ctx context.Context,
	graph *unstructured.Unstructured,
	pruneCandidates []string,
	keyToNodeID map[string]string,
	nodeIDToKey map[string]string,
	allDAGs []*DAG,
	eval *evaluator,
	watcher *graphWatcher,
	state *instanceState,
) (*finalizationResult, error) {
	logger := log.FromContext(ctx)
	result := &finalizationResult{
		CompletedTargets:   map[string]bool{},
		ProtectedKeys:      map[string]bool{},
		ChildKeysToCleanup: map[string][]string{},
	}

	if state.activeFinalization == nil {
		state.activeFinalization = map[string]*finalizationEntry{}
	}

	// Pre-seed protected keys from persisted active finalization state.
	for _, entry := range state.activeFinalization {
		for _, k := range entry.ChildKeys {
			result.ProtectedKeys[k] = true
		}
	}

	// findFinalizers looks up finalizer node IDs for a target across all DAGs.
	findFinalizers := func(nodeID string) (*DAG, []string) {
		for _, d := range allDAGs {
			if fins, ok := d.Finalizers[nodeID]; ok && len(fins) > 0 {
				return d, fins
			}
		}
		return nil, nil
	}

	// Protect static dependencies of finalizer nodes.
	for _, key := range pruneCandidates {
		nodeID := keyToNodeID[key]
		finDAG, finalizerNodeIDs := findFinalizers(nodeID)
		if finDAG == nil {
			continue
		}
		for _, finNodeID := range finalizerNodeIDs {
			finIdx, exists := finDAG.Index[finNodeID]
			if !exists {
				continue
			}
			for depID := range finDAG.Nodes[finIdx].Dependencies {
				if depID == nodeID {
					continue
				}
				if dk, ok := nodeIDToKey[depID]; ok {
					result.ProtectedKeys[dk] = true
				}
			}
			if dk, ok := nodeIDToKey[finNodeID]; ok {
				result.ProtectedKeys[dk] = true
			}
		}
	}

	// Process each prune candidate that has finalizers.
	for _, key := range pruneCandidates {
		nodeID := keyToNodeID[key]
		if nodeID == "" {
			continue
		}
		finDAG, finalizerNodeIDs := findFinalizers(nodeID)
		if finDAG == nil {
			continue // no finalizers — prune walk handles directly
		}

		// GET the target to verify it exists.
		obj, nn, ok := unstructuredFromKey(key)
		if !ok {
			continue
		}
		if getErr := r.apiReader().Get(ctx, nn, obj); getErr != nil {
			if apierrors.IsNotFound(getErr) {
				// Target already gone — skip finalization.
				logger.Info("finalization skipped: target resource does not exist",
					"key", key, "finalizers", finalizerNodeIDs)
				result.Notes = append(result.Notes, fmt.Sprintf("FinalizerSkipped: %s (target absent)", key))
				delete(state.activeFinalization, key)
				result.CompletedTargets[key] = true
				continue
			}
			return nil, fmt.Errorf("checking finalization target %s: %w", key, getErr)
		}

		// Run the finalization sequence.
		ready, childKeys, finErr := r.runFinalization(ctx, graph, obj, nodeID, finalizerNodeIDs, finDAG, eval, watcher)

		// Protect all child keys from pruning.
		for _, ck := range childKeys {
			result.ProtectedKeys[ck] = true
		}

		if finErr != nil {
			logger.Error(finErr, "finalization failed", "key", key)
			result.BlockedReasons = append(result.BlockedReasons, fmt.Sprintf(
				"TeardownBlocked: %s (finalizer creation failed: %s)", key, finErr))
			result.DeferredTargets = append(result.DeferredTargets, key)
			state.activeFinalization[key] = &finalizationEntry{
				Phase:     FinalizationCreating,
				ChildKeys: childKeys,
			}
			continue
		}

		if !ready {
			logger.Info("finalization in progress — deletion deferred",
				"key", key, "finalizers", finalizerNodeIDs)
			result.BlockedReasons = append(result.BlockedReasons, fmt.Sprintf(
				"TeardownBlocked: %s (finalizer not ready: %s)",
				key, strings.Join(finalizerNodeIDs, ", ")))
			result.DeferredTargets = append(result.DeferredTargets, key)
			state.activeFinalization[key] = &finalizationEntry{
				Phase:     FinalizationWaitingReady,
				ChildKeys: childKeys,
			}
			continue
		}

		// Finalization complete — target can be deleted.
		logger.Info("finalization complete", "key", key)
		result.CompletedTargets[key] = true
		result.ChildKeysToCleanup[key] = childKeys
		delete(state.activeFinalization, key)
	}

	return result, nil
}

// ---------------------------------------------------------------------------
// Finalization execution (stateless — re-derives position from cluster state)
// ---------------------------------------------------------------------------

// runFinalization executes the finalization sequence for a single target.
// Returns (true, keys, nil) when all finalizer resources are ready.
// Returns (false, keys, nil) when finalizers are in progress.
// Returns (false, keys, err) when a finalizer can't be created.
func (r *GraphReconciler) runFinalization(
	ctx context.Context,
	graph *unstructured.Unstructured,
	target *unstructured.Unstructured,
	targetNodeID string,
	finalizerNodeIDs []string,
	dag *DAG,
	eval *evaluator,
	watcher *graphWatcher,
) (bool, []string, error) {
	logger := log.FromContext(ctx)
	var keys []string

	// Put the target in scope so finalizer templates can reference it.
	if targetNodeID != "" {
		eval.scope[targetNodeID] = normalizeTypes(target.Object)
	}

	// Sort finalizer nodes by topological position.
	ordered := sortFinalizerNodes(finalizerNodeIDs, dag)

	allReady := true
	for _, finNodeID := range ordered {
		idx, ok := dag.Index[finNodeID]
		if !ok {
			return false, keys, fmt.Errorf("finalizer node %q not found in DAG", finNodeID)
		}
		finNode := &dag.Nodes[idx]

		// forEach + finalizes: expand collection.
		if finNode.ForEach != nil {
			ready, fKeys, err := r.runForEachFinalization(ctx, graph, finNode, dag, eval, watcher)
			keys = append(keys, fKeys...)
			if err != nil {
				return false, keys, err
			}
			if !ready {
				allReady = false
			}
			continue
		}

		// Single-resource finalizer.
		evalMap, err := eval.toMapNode(*finNode)
		if err != nil {
			return false, keys, fmt.Errorf("evaluating finalizer template %s: %w", finNodeID, err)
		}

		finObj := &unstructured.Unstructured{Object: evalMap}
		if finObj.GetNamespace() == "" {
			finObj.SetNamespace(graph.GetNamespace())
		}
		existing := &unstructured.Unstructured{}
		existing.SetGroupVersionKind(finObj.GroupVersionKind())
		err = r.apiReader().Get(ctx, client.ObjectKey{
			Namespace: finObj.GetNamespace(),
			Name:      finObj.GetName(),
		}, existing)

		if err != nil {
			if !apierrors.IsNotFound(err) {
				return false, keys, fmt.Errorf("checking finalizer resource %s: %w", finNodeID, err)
			}
			// Create finalizer resource.
			logger.Info("creating finalizer resource", "finalizer", finNodeID,
				"target", target.GetName())
			applied, applyErr := r.applySSA(ctx, graph, evalMap, watcher, finNodeID, NodeTypeTemplate, eval.effectiveGeneration, false, false)
			if applyErr != nil {
				return false, keys, fmt.Errorf("creating finalizer resource %s: %w", finNodeID, applyErr)
			}
			keys = append(keys, resourceKey(applied))
			eval.scope[finNodeID] = applied.Object
			allReady = false
			continue
		}

		// Exists — check readyWhen.
		eval.scope[finNodeID] = normalizeTypes(existing.Object)
		keys = append(keys, resourceKey(existing))
		if len(finNode.ReadyWhen) > 0 {
			if err := eval.evalReadinessConditions(finNode.ReadyWhen, finNodeID); err != nil {
				logger.V(1).Info("finalizer not ready", "finalizer", finNodeID)
				allReady = false
				continue
			}
		}
		logger.V(1).Info("finalizer ready", "finalizer", finNodeID)
	}

	return allReady, keys, nil
}

// runForEachFinalization handles the forEach + finalizes case.
func (r *GraphReconciler) runForEachFinalization(
	ctx context.Context,
	graph *unstructured.Unstructured,
	finNode *Node,
	dag *DAG,
	eval *evaluator,
	watcher *graphWatcher,
) (bool, []string, error) {
	logger := log.FromContext(ctx)
	var createdKeys []string
	allReady := true

	varName := finNode.ForEach.VarName
	collectionExpr := finNode.ForEach.Expr

	collection, err := eval.evalString(collectionExpr)
	if err != nil {
		return false, createdKeys, fmt.Errorf("forEach finalizer %s: evaluating collection %q: %w", finNode.ID, collectionExpr, err)
	}

	items, ok := collection.([]any)
	if !ok {
		items = []any{collection}
	}
	logger.Info("forEach finalization expanding", "finalizer", finNode.ID, "var", varName, "count", len(items))

	for _, item := range items {
		innerScope := copyScope(eval.scope)
		innerScope[varName] = item
		innerEval := eval.withScope(innerScope)

		evalMap, err := innerEval.toMapNode(*finNode)
		if err != nil {
			return false, createdKeys, fmt.Errorf("forEach finalizer %s item: %w", finNode.ID, err)
		}

		childObj := &unstructured.Unstructured{Object: evalMap}
		if childObj.GetNamespace() == "" {
			childObj.SetNamespace(graph.GetNamespace())
		}
		stampForEachChildLabels(childObj, finNode.ID, graph.GetName(), graph.GetNamespace(), eval.effectiveGeneration, NodeTypeTemplate)
		evalMap = childObj.Object

		existing := &unstructured.Unstructured{}
		existing.SetGroupVersionKind(childObj.GroupVersionKind())
		getErr := r.apiReader().Get(ctx, client.ObjectKey{
			Namespace: childObj.GetNamespace(),
			Name:      childObj.GetName(),
		}, existing)

		if getErr != nil {
			if !apierrors.IsNotFound(getErr) {
				return false, createdKeys, fmt.Errorf("checking forEach finalizer child %s/%s: %w", finNode.ID, childObj.GetName(), getErr)
			}
			logger.Info("creating forEach finalizer child",
				"finalizer", finNode.ID, "name", childObj.GetName())
			applied, applyErr := r.applySSA(ctx, graph, evalMap, watcher, finNode.ID, NodeTypeTemplate, eval.effectiveGeneration, false, false)
			if applyErr != nil {
				return false, createdKeys, fmt.Errorf("creating forEach finalizer child %s/%s: %w", finNode.ID, childObj.GetName(), applyErr)
			}
			createdKeys = append(createdKeys, resourceKey(applied))
			allReady = false
			continue
		}

		createdKeys = append(createdKeys, resourceKey(existing))
		if len(finNode.ReadyWhen) > 0 {
			innerEval.scope[finNode.ID] = normalizeTypes(existing.Object)
			if err := innerEval.evalReadinessConditions(finNode.ReadyWhen, finNode.ID); err != nil {
				logger.V(1).Info("forEach finalizer child not ready",
					"finalizer", finNode.ID, "name", existing.GetName())
				allReady = false
				continue
			}
		}
		logger.V(1).Info("forEach finalizer child ready",
			"finalizer", finNode.ID, "name", existing.GetName())
	}

	return allReady, createdKeys, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// cleanupFinalizationChildren deletes finalizer resources after their target
// has been successfully deleted. Failures are non-fatal — children become
// normal prune candidates on the next cycle.
func (r *GraphReconciler) cleanupFinalizationChildren(ctx context.Context, keys []string) {
	logger := log.FromContext(ctx)
	for _, fk := range keys {
		finDel, _, ok := unstructuredFromKey(fk)
		if !ok {
			continue
		}
		if delErr := r.Client.Delete(ctx, finDel); delErr != nil {
			if client.IgnoreNotFound(delErr) != nil {
				logger.V(1).Info("finalizer resource cleanup failed (will retry as prune candidate)", "key", fk, "error", delErr)
			}
		} else {
			logger.V(1).Info("cleaned up finalizer resource", "key", fk)
			r.Resources.remove(fk)
		}
	}
}

// sortFinalizerNodes returns finalizer node IDs sorted by topological position.
func sortFinalizerNodes(finalizerNodeIDs []string, dag *DAG) []string {
	ordered := make([]string, len(finalizerNodeIDs))
	copy(ordered, finalizerNodeIDs)
	topoPos := make(map[string]int, len(dag.TopologicalOrder))
	for pos, nodeIdx := range dag.TopologicalOrder {
		topoPos[dag.Nodes[nodeIdx].ID] = pos
	}
	sort.Slice(ordered, func(i, j int) bool {
		return topoPos[ordered[i]] < topoPos[ordered[j]]
	})
	return ordered
}
