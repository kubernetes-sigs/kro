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

	dagpkg "github.com/ellistarn/kro/experimental/controller/dag"
	graphpkg "github.com/ellistarn/kro/experimental/controller/graph"
)

// FinalizationPhase represents the current state of a finalization sequence.
type FinalizationPhase string

const (
	FinalizationCreating     FinalizationPhase = "Creating"
	FinalizationWaitingReady FinalizationPhase = "WaitingReady"
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
func (c *clusterAccess) advanceFinalization(
	ctx context.Context,
	rs *reconcileScope,
	pruneCandidates []string,
	keyToNodeID map[string]string,
	nodeIDToKey map[string]string,
	allDAGs []*dagpkg.DAG,
	eval *evaluator,
	state *instanceState,
) (*finalizationResult, error) {
	logger := log.FromContext(ctx)
	result := &finalizationResult{
		CompletedTargets:   map[string]bool{},
		ProtectedKeys:      map[string]bool{},
		ChildKeysToCleanup: map[string][]string{},
	}

	if state.prune.activeFinalization == nil {
		state.prune.activeFinalization = map[string]*finalizationEntry{}
	}

	// Pre-seed protected keys from persisted active finalization state.
	for _, entry := range state.prune.activeFinalization {
		for _, k := range entry.ChildKeys {
			result.ProtectedKeys[k] = true
		}
	}

	// findFinalizers looks up finalizer node IDs for a target across all DAGs.
	findFinalizers := func(nodeID string) (*dagpkg.DAG, []string) {
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
		if getErr := c.reader.Get(ctx, nn, obj); getErr != nil {
			if apierrors.IsNotFound(getErr) {
				// Target already gone — skip finalization.
				// If a previous cycle created finalizer children, schedule
				// them for cleanup and unprotect their keys so the prune
				// walk can reach them.
				logger.Info("finalization skipped: target resource does not exist",
					"key", key, "finalizers", finalizerNodeIDs)
				result.Notes = append(result.Notes, fmt.Sprintf("FinalizerSkipped: %s (target absent)", key))
				if entry, ok := state.prune.activeFinalization[key]; ok {
					result.ChildKeysToCleanup[key] = entry.ChildKeys
					for _, ck := range entry.ChildKeys {
						delete(result.ProtectedKeys, ck)
					}
				}
				delete(state.prune.activeFinalization, key)
				result.CompletedTargets[key] = true
				continue
			}
			return nil, fmt.Errorf("checking finalization target %s: %w", key, getErr)
		}

		// Run the finalization sequence.
		ready, childKeys, finErr := c.runFinalization(ctx, rs, obj, nodeID, finalizerNodeIDs, finDAG, eval)

		// Protect all child keys from pruning.
		for _, ck := range childKeys {
			result.ProtectedKeys[ck] = true
		}

		prevPhase := FinalizationPhase("")
		if entry, ok := state.prune.activeFinalization[key]; ok {
			prevPhase = entry.Phase
		}

		if finErr != nil {
			logger.Error(finErr, "finalization failed",
				"key", key, "previousPhase", prevPhase, "newPhase", FinalizationCreating)
			result.BlockedReasons = append(result.BlockedReasons, fmt.Sprintf(
				"TeardownBlocked: %s (finalizer creation failed: %s)", key, finErr))
			result.DeferredTargets = append(result.DeferredTargets, key)
			state.prune.activeFinalization[key] = &finalizationEntry{
				Phase:     FinalizationCreating,
				ChildKeys: childKeys,
			}
			continue
		}

		if !ready {
			logger.Info("finalization in progress — deletion deferred",
				"key", key, "finalizers", finalizerNodeIDs,
				"previousPhase", prevPhase, "newPhase", FinalizationWaitingReady)
			result.BlockedReasons = append(result.BlockedReasons, fmt.Sprintf(
				"TeardownBlocked: %s (finalizer not ready: %s)",
				key, strings.Join(finalizerNodeIDs, ", ")))
			result.DeferredTargets = append(result.DeferredTargets, key)
			state.prune.activeFinalization[key] = &finalizationEntry{
				Phase:     FinalizationWaitingReady,
				ChildKeys: childKeys,
			}
			continue
		}

		// Finalization complete — target can be deleted.
		logger.Info("finalization complete", "key", key,
			"previousPhase", prevPhase)
		result.CompletedTargets[key] = true
		result.ChildKeysToCleanup[key] = childKeys
		delete(state.prune.activeFinalization, key)
	}

	return result, nil
}

// ---------------------------------------------------------------------------
// Finalization execution (stateless — re-derives position from cluster state)
// ---------------------------------------------------------------------------

// ensureFinalizerResource implements the shared GET → create-if-absent →
// evaluate-readyWhen pattern used by both single-resource and forEach
// finalization. Returns (ready, key, error).
//
// When the resource already exists on the cluster, scope is updated to
// the actual cluster object BEFORE evaluating readyWhen. This is
// critical: readyWhen expressions (e.g., `${finalizerJob.status.succeeded
// > 0}`) must evaluate against real status fields, not the template
// output which lacks controller-set fields.
func (c *clusterAccess) ensureFinalizerResource(
	ctx context.Context,
	rs *reconcileScope,
	eval *evaluator,
	node *graphpkg.Node,
	evalMap map[string]any,
) (bool, string, error) {
	logger := log.FromContext(ctx)

	obj := &unstructured.Unstructured{Object: evalMap}
	if obj.GetNamespace() == "" {
		obj.SetNamespace(rs.namespace)
	}

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(obj.GroupVersionKind())
	err := c.reader.Get(ctx, client.ObjectKey{
		Namespace: obj.GetNamespace(),
		Name:      obj.GetName(),
	}, existing)

	if err != nil {
		if !apierrors.IsNotFound(err) {
			return false, "", fmt.Errorf("checking finalizer resource %s: %w", node.ID, err)
		}
		// Create finalizer resource.
		logger.Info("creating finalizer resource", "finalizer", node.ID,
			"name", obj.GetName())
		applied, applyErr := c.applySSA(ctx, rs, evalMap, node.ID, graphpkg.NodeTypeTemplate, eval.effectiveGeneration, false)
		if applyErr != nil {
			return false, "", fmt.Errorf("creating finalizer resource %s: %w", node.ID, applyErr)
		}
		return false, resourceKey(applied), nil
	}

	// Exists — update scope to cluster state so readyWhen evaluates against
	// real status fields, not the template output.
	eval.scope[node.ID] = graphpkg.NormalizeTypes(existing.Object)

	key := resourceKey(existing)
	if len(node.ReadyWhen) > 0 {
		if err := eval.evalReadinessConditions(node.ReadyWhen, node.ID); err != nil {
			logger.V(1).Info("finalizer not ready", "finalizer", node.ID, "name", existing.GetName())
			return false, key, nil
		}
	}
	logger.V(1).Info("finalizer ready", "finalizer", node.ID, "name", existing.GetName())
	return true, key, nil
}

// runFinalization executes the finalization sequence for a single target.
// Returns (true, keys, nil) when all finalizer resources are ready.
// Returns (false, keys, nil) when finalizers are in progress.
// Returns (false, keys, err) when a finalizer can't be created.
func (c *clusterAccess) runFinalization(
	ctx context.Context,
	rs *reconcileScope,
	target *unstructured.Unstructured,
	targetNodeID string,
	finalizerNodeIDs []string,
	dag *dagpkg.DAG,
	eval *evaluator,
) (bool, []string, error) {
	var keys []string

	// Put the target in scope so finalizer templates can reference it.
	if targetNodeID != "" {
		eval.scope[targetNodeID] = graphpkg.NormalizeTypes(target.Object)
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
			ready, fKeys, err := c.runForEachFinalization(ctx, rs, finNode, dag, eval)
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

		// Seed scope with template output so downstream finalizer templates
		// can reference this node. ensureFinalizerResource will overwrite
		// scope with the actual cluster object when the resource exists.
		eval.scope[finNodeID] = graphpkg.NormalizeTypes((&unstructured.Unstructured{Object: evalMap}).Object)
		ready, key, ensureErr := c.ensureFinalizerResource(ctx, rs, eval, finNode, evalMap)
		if ensureErr != nil {
			return false, keys, ensureErr
		}
		keys = append(keys, key)
		if !ready {
			allReady = false
		}
	}

	return allReady, keys, nil
}

// runForEachFinalization handles the forEach + finalizes case.
func (c *clusterAccess) runForEachFinalization(
	ctx context.Context,
	rs *reconcileScope,
	finNode *graphpkg.Node,
	dag *dagpkg.DAG,
	eval *evaluator,
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
		innerScope := graphpkg.CopyScope(eval.scope)
		innerScope[varName] = item
		innerEval := eval.withScope(innerScope)

		evalMap, err := innerEval.toMapNode(*finNode)
		if err != nil {
			return false, createdKeys, fmt.Errorf("forEach finalizer %s item: %w", finNode.ID, err)
		}

		childObj := &unstructured.Unstructured{Object: evalMap}
		if childObj.GetNamespace() == "" {
			childObj.SetNamespace(rs.namespace)
		}
		graphpkg.StampForEachChildLabels(childObj, finNode.ID, rs.name, rs.namespace, eval.effectiveGeneration, graphpkg.NodeTypeTemplate)
		evalMap = childObj.Object

		innerEval.scope[finNode.ID] = graphpkg.NormalizeTypes(childObj.Object)
		ready, key, ensureErr := c.ensureFinalizerResource(ctx, rs, innerEval, finNode, evalMap)
		if ensureErr != nil {
			return false, createdKeys, ensureErr
		}
		createdKeys = append(createdKeys, key)
		if !ready {
			allReady = false
		}
	}

	return allReady, createdKeys, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// sortFinalizerNodes returns finalizer node IDs sorted by topological position.
func sortFinalizerNodes(finalizerNodeIDs []string, dag *dagpkg.DAG) []string {
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
