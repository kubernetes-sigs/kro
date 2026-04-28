// walk.go implements the DAG walk algorithm for a single reconcile cycle.
//
// The coordinator dispatches nodes as soon as their dependencies are satisfied.
// Workers are pure functions — they receive a read-only scope snapshot and
// return results via nodeResult. The coordinator is the single writer to
// shared state (scope, plan, applied keys).
//
// Per 005-reconciliation.md § Reconcile: "The coordinator walks the DAG in
// topological order, dispatching each node when its dependencies are resolved."
package graphcontroller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/kubernetes-sigs/kro/experimental/controller/compiler"
	dagpkg "github.com/kubernetes-sigs/kro/experimental/controller/dag"
	graphpkg "github.com/kubernetes-sigs/kro/experimental/controller/graph"
	"github.com/kubernetes-sigs/kro/experimental/controller/watches"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// nodeResult carries a worker's output back to the coordinator.
type nodeResult struct {
	idx          int
	keys         []string
	state        dagpkg.NodeState
	err          error
	scopeKey     string        // node ID to set in scope
	scopeValue   any           // value to set (the full K8s object or collection)
	evalDuration time.Duration // wall-clock time of reconcileNode (measured inside the worker)
	// forEach state updates — returned by forEach workers for the
	// coordinator to merge back into the instance state.
	forEachUpdates map[string]map[string][]string // nodeID → itemID → keys
	forEachScopes  map[string]map[string]any      // nodeID → itemID → scope
	forEachHashes  map[string]map[string]string   // nodeID → itemID → content hash
	forEachItems   map[string][]any               // cache key → collection items
	// Watch cache update — returned by Watch workers for the
	// coordinator to merge back into the instance state.
	collectionCacheUpdate map[string][]any // node ID → updated cached list
	// collectionDidFullList is true when the Watch worker took the
	// full-List path (full re-List from API server), as opposed to
	// incremental merge. Coordinator uses this to clear the dirty flag
	// only after an authoritative refresh.
	collectionDidFullList bool
	// nodeReadyUpdate carries the node's readyWhen verdict from worker
	// back to the coordinator. Used by the AST-rewritten `.ready()`
	// lookup for Watch nodes (see readyrewrite.go). nil if the
	// worker did not evaluate readiness (e.g., non-Watch or
	// early-exit path).
	nodeReadyUpdate *bool
	// resolvedGVK carries the GVK resolved by a dynamic-GVK node's template
	// evaluation. The coordinator uses it to detect staleness per
	// 004-compilation.md § Deferred Types.
	resolvedGVK *schema.GroupVersionKind
}

// walkState holds coordinator-local state for a single DAG walk.
// Extracted from the Reconcile closure for readability — the coordinator
// loop in Reconcile reads/writes these fields directly.
type walkState struct {
	r       *GraphReconciler
	ctx     context.Context
	graph   *unstructured.Unstructured
	dag     *dagpkg.DAG
	eval    *evaluator
	state   *instanceState
	plan    *dagpkg.PlanState
	watcher *watches.GraphWatcher

	// Trigger maps
	triggered                map[string]bool
	resyncTriggered          map[string]bool
	propagationTriggered     map[string]bool
	lazyPropagationTriggered map[string]bool

	// Watch incremental cache — drained once at walk start.
	// Per 005-reconciliation.md § Propagation.
	collectionChanges map[string][]watches.CollectionChange // nodeID → changes

	// Walk-local tracking
	dispatched        map[int]bool
	outputsReady      map[string]bool
	nodeKeys          map[string][]string // per-node applied keys; last writer wins
	nodeErrors        []string            // "nodeID: reason" for status reporting
	results           chan nodeResult
	inflight          int
	dynamicGVKChanged bool // set when a dynamic GVK resolves for the first time or changes

	// staleLazyDeps tracks nodes dispatched to workers while some of
	// their lazy dependencies were still inflight. The worker snapshot
	// captured stale .ready() values. After the walk, explicit triggers
	// are deposited so the next reconcile re-evaluates them with fresh
	// scope from completed lazy deps.
	staleLazyDeps map[string]bool

	// --- Inputs set by Reconcile before run() ---

	// preWalkSchemaGen is the schema generation counter before the walk.
	// After the walk, if the counter advanced (a CRD was created by a
	// template node), run() re-validates compilation to catch child graph
	// type errors within the same cycle.
	preWalkSchemaGen int64

	// compilationErr is the compilation error from Phase 1 (if any).
	// run() may update it if post-walk recompilation detects new errors.
	compilationErr error

	// activeRevision is the revision being reconciled from. Used by
	// the post-walk recompilation check.
	activeRevision *unstructured.Unstructured

	// --- Outputs set by run() for Reconcile to read ---

	// walkAttempted is true after run() executes. Gates the watcher
	// commit: if no walk happens, previous watch registrations are preserved.
	walkAttempted bool

	// requeueFloor is an explicit requeue interval set during the walk.
	// Zero means no walk-initiated floor.
	// The prune phase in Reconcile may further update the floor.
	requeueFloor time.Duration

	// appliedKeys is the flattened set of all per-node applied keys
	// from the walk. Consumed by the prune phase.
	appliedKeys []string

	// summary is the aggregate plan state after the walk completes.
	summary dagpkg.PlanSummary
}

// notifyDependents dispatches all dependents of a node after its state is
// committed without execution (Excluded, Blocked, Pending, includeWhen
// failure). Each dependent is propagation-triggered so that tryDispatch
// bypasses the skip check and evaluates dependencies with full precedence.
//
// Bounded by DAG depth (acyclic, verified at compile time). Each dependent
// is dispatched at most once — the early return in tryDispatch guards
// against re-evaluation of already-committed nodes.
func (w *walkState) notifyDependents(nodeID string) {
	for _, depIdx := range w.dag.Dependents[nodeID] {
		depNode := &w.dag.Nodes[depIdx]
		w.propagationTriggered[depNode.ID] = true
		if depNode.Dependencies[nodeID] == graphpkg.DepLazy {
			w.lazyPropagationTriggered[depNode.ID] = true
		}
		w.tryDispatch(depIdx)
	}
}

// carryForwardKeys retains a node's previous applied keys in the walk's
// per-node key map. Used when a node is skipped, blocked, or in error — the
// resource may still exist in the cluster, so its keys must remain in the
// applied set to prevent spurious pruning. Last-writer-wins: a subsequent
// dispatch for the same node overwrites these keys.
func (w *walkState) carryForwardKeys(nodeID string) {
	if prevKeys, ok := w.state.previousKeys[nodeID]; ok {
		if len(prevKeys) > 0 {
			log.FromContext(w.ctx).V(1).Info("carryForwardKeys", "node", nodeID, "keys", prevKeys)
		}
		w.nodeKeys[nodeID] = prevKeys
	}
}

// resolveForEachChangedItems determines whether a forEach dispatch can use
// the incremental O(K) path. Returns the set of changed item identities
// when conditions are met, nil otherwise (full O(N) rehash).
//
// Conditions for incremental diff:
//  1. CollectionSource is set (compile-time: single scope var, not in template)
//  2. Not resync-triggered or directly triggered (watch event on forEach itself)
//  3. CollectionSource has CollectionChanges (incremental Watch path)
//  4. Every non-CollectionSource dependency was skipped (output unchanged)
func (w *walkState) resolveForEachChangedItems(node *graphpkg.Node) map[string]bool {
	src := node.ForEach.CollectionSource

	// Direct trigger (watch event) or resync → full rehash.
	if w.resyncTriggered[node.ID] || w.triggered[node.ID] {
		return nil
	}

	// CollectionChanges are only available for the incremental Watch path.
	changes, hasChanges := w.collectionChanges[src]
	if !hasChanges || len(changes) == 0 {
		return nil
	}

	// Every non-src dependency must have been skipped (outputsReady).
	// If any was dispatched, its output might have changed and every
	// item needs re-evaluation.
	for depID := range node.Dependencies {
		if depID == src {
			continue
		}
		if !w.outputsReady[depID] {
			return nil
		}
	}

	// Translate CollectionChanges to forEach item identities.
	changedIDs := make(map[string]bool, len(changes))
	for _, change := range changes {
		if change.Namespace != "" {
			changedIDs[change.Namespace+"/"+change.Name] = true
		} else {
			changedIDs[change.Name] = true
		}
	}
	return changedIDs
}

func (w *walkState) populateDepsMap(node *graphpkg.Node) {
	depsMap, _ := w.eval.scope[compiler.ReservedDepsMapVar].(map[string]any)
	if depsMap == nil {
		depsMap = make(map[string]any, len(w.dag.Nodes))
		w.eval.scope[compiler.ReservedDepsMapVar] = depsMap
	}
	depValues := make([]any, 0, len(node.Dependencies))
	for depID := range node.Dependencies {
		if v, ok := w.eval.scope[depID]; ok {
			depValues = append(depValues, v)
		}
	}
	depsMap[node.ID] = depValues
}

// skipNode retains a node's previous state without re-evaluation. Used when
// no external trigger fired and propagation didn't reach the node, or when
// evaluation hashing proves inputs are unchanged.
func (w *walkState) skipNode(node *graphpkg.Node) {
	if prev, ok := w.state.previousScope[node.ID]; ok {
		w.eval.scope[node.ID] = prev
	}
	w.carryForwardKeys(node.ID)
	if w.watcher != nil {
		w.watcher.RetainWatches(node.ID)
	}
	w.outputsReady[node.ID] = true
	for _, depIdx := range w.dag.Dependents[node.ID] {
		w.tryDispatch(depIdx)
	}
}

// propagateIfChanged computes the propagation hash for a node's output and
// triggers dependents if the hash differs from the previous reconcile. Returns
// true if a hash was successfully computed, false if hashing failed (caller
// should fall back to unconditional propagation if needed).
//
// Per 005-reconciliation.md § Propagation: "Hash the specific field paths
// dependents reference from this node's output. If the hash differs from the
// previous reconcile, mark dependents as having a propagation trigger."
func (w *walkState) propagateIfChanged(node *graphpkg.Node, observed any, nodeState dagpkg.NodeState) bool {
	propagateHash, err := hashSelfPaths(node, observed)
	if err == nil && propagateHash == "" {
		// No SelfPaths (Watch, bare reference) — fall back to hashing
		// the full output. Without this, collection changes would never
		// propagate to forEach.
		if m, ok := observed.(map[string]any); ok {
			propagateHash, err = hashDesiredState(m)
		} else {
			// Array output (Watch, forEach) — use JSON hash.
			data, jsonErr := json.Marshal(observed)
			if jsonErr == nil {
				h := fnv.New64a()
				h.Write(data)
				propagateHash = fmt.Sprintf("%016x", h.Sum64())
			}
		}
	}
	if err != nil || propagateHash == "" {
		return false
	}
	prevHash := w.state.previousSelfHashes[node.ID]
	propagated := prevHash == "" || propagateHash != prevHash
	if propagated {
		for _, depIdx := range w.dag.Dependents[node.ID] {
			depNode := &w.dag.Nodes[depIdx]
			w.propagationTriggered[depNode.ID] = true
			if depNode.Dependencies[node.ID] == graphpkg.DepLazy {
				w.lazyPropagationTriggered[depNode.ID] = true
			}
		}
	}
	log.FromContext(w.ctx).V(1).Info("propagation hash check",
		"node", node.ID, "propagated", propagated,
		"prevHash", prevHash, "currHash", propagateHash)
	w.state.previousSelfHashes[node.ID] = propagateHash
	return true
}

// unconditionalPropagate marks all dependents as propagation-triggered.
// Used when the propagation hash cannot be computed — the safe fallback
// is to assume output changed.
func (w *walkState) unconditionalPropagate(node *graphpkg.Node) {
	for _, depIdx := range w.dag.Dependents[node.ID] {
		depNode := &w.dag.Nodes[depIdx]
		w.propagationTriggered[depNode.ID] = true
		if depNode.Dependencies[node.ID] == graphpkg.DepLazy {
			w.lazyPropagationTriggered[depNode.ID] = true
		}
	}
}

// run executes the complete DAG walk: seeds level-0 nodes, runs the
// coordinator loop, and performs post-walk cleanup. After run() returns,
// Reconcile reads output fields: walkAttempted, requeueFloor, appliedKeys,
// summary, nodeErrors, dynamicGVKChanged, compilationErr.
//
// The coordinator dispatches nodes via tryDispatch and receives results
// via the results channel. It is the single writer to shared state (scope,
// plan, applied keys). Workers are pure functions — they send nodeResult
// and touch nothing shared.
func (w *walkState) run() {
	logger := log.FromContext(w.ctx)

	// Seed: dispatch all nodes with no in-graph dependencies.
	for _, idx := range w.dag.Levels[0] {
		w.tryDispatch(idx)
	}

	// Coordinator loop: receive completions, merge into scope, dispatch dependents.
	for w.inflight > 0 {
		res := <-w.results
		w.inflight--

		node := &w.dag.Nodes[res.idx]

		// Observe per-node evaluation duration (measured inside the worker).
		if res.evalDuration > 0 {
			NodeEvalDurationSeconds.With(graphMetricLabels(
				w.graph.GetName(), w.graph.GetNamespace(), node.ID,
			)).Observe(res.evalDuration.Seconds())
		}

		// Watch incremental-cache integrity: if the worker errored
		// AND did not persist a cache update, the drained
		// CollectionChanges are lost. Mark the node dirty so the next
		// reconcile takes the full-list path to recover authoritative
		// state from the API server.
		if res.err != nil && node.Type() == graphpkg.NodeTypeWatch &&
			len(res.collectionCacheUpdate) == 0 {
			w.state.collectionDirty[node.ID] = true
		}

		// Error handling: block dependents, continue independent branches.
		if res.state == dagpkg.NodeError {
			info := classifyAPIError(res.err)
			prevState := w.state.previousPlanStates[node.ID]
			w.plan.SetState(w.dag, node.ID, info.state)
			w.state.previousPlanStates[node.ID] = info.state
			w.nodeErrors = append(w.nodeErrors, fmt.Sprintf("%s: %s", node.ID, info.reason))
			logger.V(0).Info("error on node", "node", node.ID,
				"previousState", prevState, "state", info.state, "reason", info.reason, "error", res.err)
			w.carryForwardKeys(node.ID)
			for _, depIdx := range w.dag.Dependents[node.ID] {
				w.tryDispatch(depIdx)
			}
			continue
		}
		if res.state == dagpkg.NodeConflict {
			prevState := w.state.previousPlanStates[node.ID]
			w.plan.SetState(w.dag, node.ID, dagpkg.NodeConflict)
			w.state.previousPlanStates[node.ID] = dagpkg.NodeConflict
			w.state.previousScope[node.ID] = res.scopeValue
			w.state.previousKeys[node.ID] = res.keys
			w.nodeErrors = append(w.nodeErrors, fmt.Sprintf("%s: field conflict", node.ID))
			logger.V(0).Info("conflict on node", "node", node.ID, "previousState", prevState, "error", res.err)
			w.carryForwardKeys(node.ID)
			for _, depIdx := range w.dag.Dependents[node.ID] {
				w.tryDispatch(depIdx)
			}
			continue
		}
		if res.state == dagpkg.NodePending {
			prevState := w.state.previousPlanStates[node.ID]
			w.plan.SetState(w.dag, node.ID, dagpkg.NodePending)
			w.state.previousPlanStates[node.ID] = dagpkg.NodePending
			w.state.previousScope[node.ID] = res.scopeValue
			w.state.previousKeys[node.ID] = res.keys
			logger.V(1).Info("data pending for node", "node", node.ID, "previousState", prevState, "error", res.err)
			w.carryForwardKeys(node.ID)
			for _, depIdx := range w.dag.Dependents[node.ID] {
				w.tryDispatch(depIdx)
			}
			continue
		}

		// Merge worker output into shared scope.
		if res.scopeValue != nil {
			w.eval.scope[res.scopeKey] = res.scopeValue
		}

		if w.r.SchemaGen != nil && node.Type() == graphpkg.NodeTypeTemplate {
			if scopeMap, ok := res.scopeValue.(map[string]any); ok {
				if scopeMap["apiVersion"] == "apiextensions.k8s.io/v1" && scopeMap["kind"] == "CustomResourceDefinition" {
					w.r.SchemaGen.AdvanceGeneration()
				}
			}
		}

		// Merge node-readiness verdict.
		if w.eval.nodeReady != nil {
			if res.nodeReadyUpdate != nil {
				w.eval.nodeReady[res.scopeKey] = *res.nodeReadyUpdate
			} else {
				w.eval.nodeReady[res.scopeKey] = (res.state == dagpkg.NodeReady)
			}
		}

		// Merge forEach state updates into the shared instance state.
		for k, v := range res.forEachItems {
			w.state.forEachItems[k] = v
		}
		for nodeID, itemScopes := range res.forEachScopes {
			w.state.forEachItemScope[nodeID] = itemScopes
		}
		for nodeID, itemKeys := range res.forEachUpdates {
			w.state.forEachItemKeys[nodeID] = itemKeys
		}
		for nodeID, itemHashes := range res.forEachHashes {
			w.state.forEachItemHashes[nodeID] = itemHashes
		}

		// Merge Watch cache updates into the shared instance state.
		for nodeID, cached := range res.collectionCacheUpdate {
			w.state.collectionCache[nodeID] = cached
			if res.collectionDidFullList {
				delete(w.state.collectionDirty, nodeID)
			}
		}

		// Update plan state.
		prevState := w.state.previousPlanStates[node.ID]
		w.plan.SetState(w.dag, node.ID, res.state)
		if prevState != res.state {
			logger.V(1).Info("node state transition", "node", node.ID,
				"previousState", prevState, "newState", res.state, "duration", res.evalDuration)
		}

		// Surface readyWhen expression errors.
		if res.state == dagpkg.NodeNotReady && res.err != nil && errors.Is(res.err, compiler.ErrReadyWhenFailed) {
			w.nodeErrors = append(w.nodeErrors, fmt.Sprintf("%s: %s", node.ID, res.err.Error()))
			logger.V(0).Info("readyWhen expression error (not gating dependents)",
				"node", node.ID, "error", res.err)
		}

		if res.state == dagpkg.NodeReady || res.state == dagpkg.NodeNotReady {
			w.nodeKeys[node.ID] = res.keys
		} else {
			w.carryForwardKeys(node.ID)
		}

		// Step 8: Propagation check.
		if res.state == dagpkg.NodeReady || res.state == dagpkg.NodeNotReady {
			if observed := w.eval.scope[node.ID]; observed != nil {
				w.propagateIfChanged(node, observed, res.state)
			}
		}

		// Save per-node state for next reconcile.
		w.state.previousScope[node.ID] = w.eval.scope[node.ID]
		w.state.previousKeys[node.ID] = res.keys
		w.state.previousPlanStates[node.ID] = res.state

		// Store evaluation hash for next reconcile's change check (step 3).
		if evalHash, err := hashNodeInputs(node, w.eval.scope); err == nil && evalHash != "" {
			w.state.previousEvalHashes[node.ID] = evalHash
		}

		// Record dynamic GVK resolutions.
		if res.resolvedGVK != nil {
			if w.state.mergeDynamicGVK(node.ID, *res.resolvedGVK) {
				w.dynamicGVKChanged = true
				logger.Info("dynamic GVK resolved; will recompile with schema on next reconcile",
					"node", node.ID, "gvk", *res.resolvedGVK)
			}
		}

		// Dispatch dependents whose dependencies are now satisfied.
		for _, depIdx := range w.dag.Dependents[node.ID] {
			w.tryDispatch(depIdx)
		}
	}

	// --- Post-walk cleanup ---

	w.walkAttempted = true

	// Stale lazy dep dispatch: when a node dispatches before its lazy
	// deps complete within the same walk, the worker sees stale .ready()
	// values. Deposit explicit triggers so the next reconcile re-evaluates
	// promptly (1s) rather than waiting for the default resync interval.
	if len(w.staleLazyDeps) > 0 && w.watcher != nil {
		for nodeID := range w.staleLazyDeps {
			w.watcher.DepositTrigger(nodeID)
		}
		if w.requeueFloor == 0 || w.requeueFloor > time.Second {
			w.requeueFloor = time.Second
		}
	}

	// Late propagation: when a lazy dependency's output changes after
	// the consumer was already processed in this walk, the propagation
	// trigger fires but the consumer can't re-dispatch. Deposit
	// explicit triggers and set a 1-second requeue floor so the next
	// reconcile re-evaluates promptly. Only lazy propagation triggers
	// are deposited — hard dep propagation doesn't need this because
	// hard deps are guaranteed to complete before their consumers
	// dispatch. Triggers that arrived AFTER the node was processed
	// remain (they were cleared at the start of processing).
	if w.watcher != nil {
		for nodeID := range w.lazyPropagationTriggered {
			w.watcher.DepositTrigger(nodeID)
			if w.requeueFloor == 0 || w.requeueFloor > time.Second {
				w.requeueFloor = time.Second
			}
		}
	}

	dagpkg.FinalizeSkippedStates(w.plan, w.outputsReady, w.state.previousPlanStates, func(nodeID string) {
		logger.V(1).Info("skipped node with no prior state — marking Pending", "node", nodeID)
	})

	// Retain previous keys for uncertain-absence nodes.
	for _, node := range w.dag.Nodes {
		if w.plan.States[node.ID] == dagpkg.NodeBlocked || w.plan.States[node.ID] == dagpkg.NodePending {
			w.carryForwardKeys(node.ID)
		}
	}

	// Retain watches for all DAG nodes not already retained via skipNode
	// or worker dispatch.
	if w.watcher != nil {
		for i, node := range w.dag.Nodes {
			if !w.dispatched[i] && !w.outputsReady[node.ID] {
				w.watcher.RetainWatches(node.ID)
			}
		}
	}

	// Flatten per-node keys into the applied key set.
	for _, keys := range w.nodeKeys {
		w.appliedKeys = append(w.appliedKeys, keys...)
	}

	// Post-walk recompile check: if schema generation advanced during
	// the walk (a CRD was created), re-validate compilation to catch
	// child graph type errors within the same cycle.
	if w.r.SchemaGen != nil && w.r.SchemaGen.Generation() > w.preWalkSchemaGen && w.compilationErr == nil {
		if _, _, err := w.r.compileRevision(w.ctx, w.graph.GetNamespace(), w.activeRevision); err != nil {
			w.compilationErr = err
			logger.Error(err, "post-walk recompilation detected error")
		}
	}

	// Derive aggregate state from the DAG plan.
	w.summary = w.plan.Summary()

	// Update node state gauge metrics.
	updateNodeStateMetrics(w.graph.GetName(), w.graph.GetNamespace(), w.plan, w.dag)
}

// tryDispatch checks if a node can be dispatched. Three outcomes:
// 1. All dependencies resolved → dispatch to worker
// 2. Some dependency still inflight → skip, retried when dependency completes
// 3. Some dependency permanently blocked → mark Excluded
func (w *walkState) tryDispatch(idx int) {
	node := &w.dag.Nodes[idx]
	logger := log.FromContext(w.ctx)

	if w.plan.States[node.ID] != dagpkg.NodeUnvisited {
		// A gate-blocked node (propagateWhen set the state but the node
		// was never dispatched to a worker) can be re-evaluated when a
		// new propagation trigger arrives. This handles lazy deps: the
		// gate may reference .ready() on a lazy dependency that wasn't
		// complete when the gate was first evaluated. When the lazy dep
		// completes, its propagation trigger gives the gate another
		// chance.
		if w.propagationTriggered[node.ID] && !w.dispatched[idx] {
			delete(w.plan.States, node.ID)
		} else {
			return // already processed or excluded
		}
	}
	if w.dispatched[idx] {
		return // goroutine already running for this node
	}

	// Finalizer nodes are dormant during normal operation — they only
	// materialize during prune/teardown. Skip them in the walk.
	if node.Finalizes != "" {
		w.plan.SetState(w.dag, node.ID, dagpkg.NodeReady)
		return
	}

	// Step 1: Skip check — no external trigger and no propagation trigger.
	if !w.triggered[node.ID] && !w.propagationTriggered[node.ID] {
		if prevKeys, ok := w.state.previousKeys[node.ID]; ok && len(prevKeys) > 0 {
			logger.V(1).Info("skip check — skipping node with non-empty previousKeys",
				"node", node.ID, "triggered", w.triggered[node.ID],
				"propagationTriggered", w.propagationTriggered[node.ID],
				"previousKeys", prevKeys)
		}
		w.skipNode(node)
		return
	}

	// Consume propagation triggers — they're handled by this evaluation.
	// Any trigger arriving AFTER this point (from a dep completing later
	// in the walk) will remain set for post-walk deposit.
	hadLazyPropagation := w.lazyPropagationTriggered[node.ID]
	delete(w.propagationTriggered, node.ID)
	delete(w.lazyPropagationTriggered, node.ID)

	// Check hard dependencies. Lazy deps don't gate dispatch or cause
	// exclusion — the expression has a branch that handles absent data.
	hasExcluded := false
	hasBlocked := false
	hasPending := false
	hasInflight := false
	for depID, kind := range node.Dependencies {
		if kind != graphpkg.DepHard {
			continue
		}
		depState, exists := w.plan.States[depID]
		if !exists {
			continue
		}
		switch depState {
		case dagpkg.NodeReady, dagpkg.NodeNotReady:
			continue
		case dagpkg.NodeUnvisited:
			if w.outputsReady[depID] {
				if prevState, ok := w.state.previousPlanStates[depID]; ok {
					switch prevState {
					case dagpkg.NodeReady, dagpkg.NodeNotReady:
						continue
					case dagpkg.NodeExcluded:
						hasExcluded = true
						continue
					case dagpkg.NodePending:
						hasPending = true
						continue
					default:
						hasBlocked = true
						continue
					}
				}
				continue
			}
			hasInflight = true
		case dagpkg.NodeExcluded:
			hasExcluded = true
		case dagpkg.NodePending:
			hasPending = true
		default:
			hasBlocked = true
		}
	}
	if hasExcluded {
		// Contagious exclusion: any hard dependency Excluded → Excluded.
		// Per 005-reconciliation.md § Propagation step 1. Unconditional —
		// no exception for propagateWhen. Lazy dependencies (.ready()
		// targets) are not in node.Dependencies, so they don't trigger
		// exclusion. The expression has a branch that handles absent data.
		logger.V(1).Info("node excluded — dependency excluded", "node", node.ID)
		w.plan.SetState(w.dag, node.ID, dagpkg.NodeExcluded)
		w.state.previousPlanStates[node.ID] = dagpkg.NodeExcluded
		delete(w.state.previousEvalHashes, node.ID)
		// Excluded nodes advertise ready=true so that non-excluded status
		// rollup expressions (e.g., ACTIVE/IN_PROGRESS) don't block on
		// absent nodes. Both scope and nodeReady are set: scope covers
		// the .ready() CEL function path (reads __ready from map),
		// nodeReady covers the AST-rewritten __kroNodeReady lookup path.
		// Persist to previousScope so the value survives across reconciles
		// (skipNode restores from previousScope on subsequent cycles).
		w.eval.scope[node.ID] = map[string]any{"__ready": true}
		w.eval.nodeReady[node.ID] = true
		w.state.previousScope[node.ID] = w.eval.scope[node.ID]
		w.notifyDependents(node.ID)
		return
	}
	if hasBlocked {
		logger.V(1).Info("node blocked — dependency in error state", "node", node.ID)
		w.plan.SetState(w.dag, node.ID, dagpkg.NodeBlocked)
		w.state.previousPlanStates[node.ID] = dagpkg.NodeBlocked
		delete(w.state.previousEvalHashes, node.ID)
		w.notifyDependents(node.ID)
		return
	}
	if hasPending {
		logger.V(1).Info("node pending — dependency pending", "node", node.ID)
		w.plan.SetState(w.dag, node.ID, dagpkg.NodePending)
		w.state.previousPlanStates[node.ID] = dagpkg.NodePending
		delete(w.state.previousEvalHashes, node.ID)
		w.notifyDependents(node.ID)
		return
	}
	if hasInflight {
		return
	}

	// Step 3: propagateWhen — input gate on this node.
	// For forEach nodes, propagateWhen is evaluated per-item inside the
	// expansion loop (reconcileForEach), not here. Per 001-graph.md §
	// propagateWhen: "With forEach, [...] the controller evaluates
	// propagateWhen per-item and halts when the condition is first false."
	if len(node.PropagateWhen) > 0 && node.ForEach == nil {
		// Populate __kroDeps for this node so that <id>.dependencies()
		// expressions resolve correctly.
		w.populateDepsMap(node)

		gate := w.eval.checkPropagateWhen(node.PropagateWhen, node.ID)
		if gate != gatePass {
			unsatisfied := w.eval.firstUnsatisfiedCondition(node.PropagateWhen)
			logger.V(1).Info("propagateWhen input gate — retaining previous state",
				"node", node.ID, "unsatisfied", unsatisfied)
			if prev, ok := w.state.previousScope[node.ID]; ok {
				w.eval.scope[node.ID] = prev
			}
			w.carryForwardKeys(node.ID)
			if prevState, ok := w.state.previousPlanStates[node.ID]; ok {
				w.plan.States[node.ID] = prevState
			} else {
				w.plan.States[node.ID] = dagpkg.NodePending
			}
			// Record previousPlanStates so subsequent reconciles that
			// skip this node (outputsReady) can restore the correct
			// state via FinalizeSkippedStates. Without this, a gate-
			// blocked node's state is lost between reconciles, causing
			// "skipped node with no prior state" fallthrough to Pending.
			w.state.previousPlanStates[node.ID] = w.plan.States[node.ID]
			for _, depIdx := range w.dag.Dependents[node.ID] {
				w.tryDispatch(depIdx)
			}
			return
		}
	}

	// Step 4: Evaluation check — section-scoped evaluation hashing.
	// Propagation-triggered nodes bypass the hash check: a dependency's
	// output changed, so re-evaluate. The hash optimization applies only
	// to self-triggered nodes (watch events, resync) where the question
	// is "did my inputs actually change?"
	declaredNodeType := node.Type()
	canHashSkip := declaredNodeType != graphpkg.NodeTypeRef && declaredNodeType != graphpkg.NodeTypeWatch && !w.resyncTriggered[node.ID] && !hadLazyPropagation
	if canHashSkip {
		if _, hasPrevHash := w.state.previousEvalHashes[node.ID]; hasPrevHash {
			evalHash, hashErr := hashNodeInputs(node, w.eval.scope)
			if hashErr == nil && evalHash != "" && evalHash == w.state.previousEvalHashes[node.ID] {
				prevScope := w.state.previousScope[node.ID]
				selfChanged := false
				// Check whether the node's own resource changed since
				// the last reconcile by comparing informer-cache RV
				// against the RV in previousScope. This detects status
				// updates by external controllers, drift corrections,
				// and any mutation not reflected in eval-hash inputs.
				//
				// The check is unconditional — it runs for ALL nodes
				// with a Kubernetes resource in scope, not just those
				// with non-empty SelfPaths. SelfPaths tracks which
				// fields *downstream* nodes reference; readyWhen and
				// propagation hash use the full resource and must
				// re-evaluate on any RV change.
				if w.watcher != nil && prevScope != nil {
					if prevMap, ok := prevScope.(map[string]any); ok {
						prevMD, _ := prevMap["metadata"].(map[string]any)
						prevRV, _ := prevMD["resourceVersion"].(string)
						prevAPIVersion, _ := prevMap["apiVersion"].(string)
						prevKind, _ := prevMap["kind"].(string)
						prevNS, _ := prevMD["namespace"].(string)
						prevName, _ := prevMD["name"].(string)
						if prevRV != "" && prevAPIVersion != "" && prevKind != "" {
							gv, _ := schema.ParseGroupVersion(prevAPIVersion)
							gvr := gvkToGVR(gv.WithKind(prevKind))
						liveRV := w.watcher.GetResourceVersion(gvr, prevNS, prevName)
						// Covers both mutations (RV changed) and deletions (RV went from non-empty to "").
						if liveRV != prevRV {
							selfChanged = true
						}
						}
					}
				}

			if !selfChanged {
				// Path 1: skip everything — inputs unchanged, live state
				// unchanged (informer RV matches).
				logger.V(1).Info("evaluation hash match — skipping evaluation",
					"node", node.ID)
				if prevState, ok := w.state.previousPlanStates[node.ID]; ok {
					w.plan.States[node.ID] = prevState
				}
				w.skipNode(node)
				return
			}

			// Path 2: self-state changed (liveRV differs or resource
			// deleted) — fall through to full evaluation. SSA apply is
			// idempotent and corrects drift from desired state (external
			// modification, deletion, status updates).
			delete(w.state.previousEvalHashes, node.ID)
			goto fullEval
			}
		fullEval:
			// Path 3: hash mismatch → full evaluation.
		}
	} // canHashSkip

	// includeWhen
	if len(node.IncludeWhen) > 0 {
		included, err := w.eval.includeWhen(node.IncludeWhen)
		if err != nil {
			// Retain previous applied keys — the resource may still exist
			// from a prior successful apply, and the gate expression errored
			// before we could definitively include or exclude. Without this,
			// includeWhen errors would produce a phantom prune candidate the
			// prune gate already blocks on Pending/Error states, but key
			// retention makes correctness structural rather than relying on
			// a distant safety net.
			w.carryForwardKeys(node.ID)
			if errors.Is(err, compiler.ErrPending) {
				w.plan.SetState(w.dag, node.ID, dagpkg.NodePending)
				w.state.previousPlanStates[node.ID] = dagpkg.NodePending
			} else {
				w.plan.SetState(w.dag, node.ID, dagpkg.NodeError)
				w.state.previousPlanStates[node.ID] = dagpkg.NodeError
			}
			// Clear stale eval hash — the node was not evaluated this cycle,
			// so the hash from a prior successful evaluation is invalid. Without
			// this, a future reconcile where inputs cycle back to their original
			// values would match the stale hash and skip re-evaluation, leaving
			// the node permanently stuck in Pending/Error.
			delete(w.state.previousEvalHashes, node.ID)
			w.notifyDependents(node.ID)
			return
		}
		if !included {
			logger.V(1).Info("node excluded by includeWhen", "node", node.ID)
			w.plan.SetState(w.dag, node.ID, dagpkg.NodeExcluded)
			w.state.previousPlanStates[node.ID] = dagpkg.NodeExcluded
			delete(w.state.previousEvalHashes, node.ID)
			// Excluded nodes advertise ready=true — same rationale as
			// contagious exclusion above.
			w.eval.scope[node.ID] = map[string]any{"__ready": true}
			w.eval.nodeReady[node.ID] = true
			w.state.previousScope[node.ID] = w.eval.scope[node.ID]
			w.notifyDependents(node.ID)
			return
		}
	}

	// Build snapshot evaluator for the worker.
	workerEval := w.eval.snapshotFor(node, w.state)

	// Watch incremental cache: pass cached list and collection changes
	// to the worker so reconcileWatch can GET only changed items.
	if node.Type() == graphpkg.NodeTypeWatch {
		workerEval.dispatch.collectionNodeID = node.ID
		cached, hasCached := w.state.collectionCache[node.ID]
		// dirty: a previous incremental reconcile errored mid-merge,
		// so the drained CollectionChanges were lost. Force a full
		// re-List from the API server to recover authoritative state.
		// Per 005-reconciliation.md § Propagation: incremental
		// updates must not allow the cache to serve stale data beyond
		// the resync interval when a recoverable error interrupted a
		// merge.
		dirty := w.state.collectionDirty[node.ID]
		if hasCached && !w.resyncTriggered[node.ID] && !dirty {
			workerEval.dispatch.collectionCachedList = cached
			workerEval.dispatch.collectionChanges = w.collectionChanges[node.ID]
		} else {
			// First reconcile, resync, or previous incremental error:
			// force full list.
			workerEval.dispatch.collectionResyncOrFull = true
		}
	}

	// Classification is a parse-time property of the declared keyword —
	// no runtime resolution. node.Type() is authoritative and
	// invariant across reconciles within a revision.
	resolvedNodeType := node.Type()

	// forEach incremental diff: when a forEach node is triggered
	// exclusively by collection-item changes from its collection source,
	// annotate the dispatch with the changed item identities. The worker
	// skips hash computation for items not in this set — O(K) instead of
	// O(N). Falls back to full rehash (nil annotation) when any other
	// dependency changed or conditions aren't met.
	if node.ForEach != nil && node.ForEach.CollectionSource != "" {
		workerEval.dispatch.forEachChangedItems = w.resolveForEachChangedItems(node)
	}

	// Dispatch to worker goroutine.
	w.dispatched[idx] = true

	// Track stale lazy deps: if any lazy dependency is still inflight
	// (not yet completed in this walk), the worker snapshot has stale
	// .ready() values. After the walk, deposit explicit triggers so the
	// next reconcile re-evaluates these nodes with fresh scope.
	for depID, kind := range node.Dependencies {
		if kind != graphpkg.DepLazy {
			continue
		}
		if w.plan.States[depID] == dagpkg.NodeUnvisited && !w.outputsReady[depID] {
			if w.staleLazyDeps == nil {
				w.staleLazyDeps = map[string]bool{}
			}
			w.staleLazyDeps[node.ID] = true
			break
		}
	}

	isResync := w.resyncTriggered[node.ID]
	logger.V(1).Info("dispatching node", "node", node.ID, "nodeType", resolvedNodeType, "resyncCorrection", isResync)
	go func(n graphpkg.Node, we *evaluator, nodeType graphpkg.NodeType, resyncCorrection bool) {
		evalStart := time.Now()
		keys, err := w.r.reconcileNode(w.ctx, w.graph, n, nodeType, we, w.watcher, resyncCorrection)
		evalDuration := time.Since(evalStart)
		state := dagpkg.NodeReady
		if err != nil {
			switch {
			case errors.Is(err, compiler.ErrPending):
				state = dagpkg.NodePending
			case errors.Is(err, compiler.ErrWaitingForReadiness):
				state = dagpkg.NodeNotReady
			case errors.Is(err, compiler.ErrReadyWhenFailed):
				// Per 001-graph.md: "readyWhen is a health signal — it does
				// not gate downstream execution." A broken readyWhen expression
				// (wrong return type, CEL error) must not produce NodeError
				// (which blocks dependents). NodeNotReady preserves the invariant.
				state = dagpkg.NodeNotReady
			case errors.Is(err, compiler.ErrFieldConflict):
				state = dagpkg.NodeConflict
			default:
				state = dagpkg.NodeError
			}
		}
		// Extract the worker's node-readiness verdict (Watch-only).
		// The worker wrote its verdict into its local nodeReady copy;
		// the coordinator merges the value back into the shared map
		// once the result is received. Non-Watch workers do not
		// write to nodeReady, so the lookup returns (false, false).
		var nodeReadyUpdate *bool
		if we.nodeReady != nil {
			if v, ok := we.nodeReady[n.ID]; ok {
				nodeReadyUpdate = &v
			}
		}
		// Extract dynamic GVK resolution from worker.
		var resolvedGVK *schema.GroupVersionKind
		if we.dispatch.dynamicGVKResolved != nil {
			if gvk, ok := we.dispatch.dynamicGVKResolved[n.ID]; ok {
				resolvedGVK = &gvk
			}
		}
		w.results <- nodeResult{
			idx:                   idx,
			keys:                  keys,
			state:                 state,
			err:                   err,
			scopeKey:              n.ID,
			scopeValue:            we.scope[n.ID],
			evalDuration:          evalDuration,
			forEachUpdates:        we.dispatch.forEachNewKeys,
			forEachScopes:         we.dispatch.forEachNewScope,
			forEachHashes:         we.dispatch.forEachNewHashes,
			forEachItems:          we.dispatch.forEachNewItems,
			collectionCacheUpdate: we.collectionCacheUpdate(),
			collectionDidFullList: we.dispatch.collectionDidFullList,
			nodeReadyUpdate:       nodeReadyUpdate,
			resolvedGVK:           resolvedGVK,
		}
	}(*node, workerEval, resolvedNodeType, isResync)
	w.inflight++
}
