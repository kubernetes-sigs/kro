// Package graphcontroller implements a proof-of-concept Graph controller.
//
// The controller watches Graph custom resources and reconciles them in two phases:
//
// Phase 1 — Revision management:
//  1. Ensure a GraphRevision exists for the current Graph spec generation
//  2. If the spec changed, materialize + compile + create a new revision
//  3. Manage revision activation (old stays Active until new revision converges)
//
// Phase 2 — Node reconciliation (from the active revision):
//  1. Parse the active revision's spec into a DAG
//  2. Walk the DAG in topological order, evaluating pre-compiled CEL programs
//  3. Apply evaluated templates via server-side apply
//  4. Prune resources removed between revisions
//  5. Update revision and Graph status
package graphcontroller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"strings"
	"sync"
	"time"

	"github.com/gobuffalo/flect"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

var GraphGVK = schema.GroupVersionKind{
	Group:   "experimental.kro.run",
	Version: "v1alpha1",
	Kind:    "Graph",
}

const (
	finalizer = "experimental.kro.run/graph-controller"

	// defaultDriftInterval is the per-node consistency floor interval.
	// Per 004-graph-execution.md § The Walk: "Each node has an in-memory
	// drift timer with a jittered interval (default 30 minutes)."
	// On expiry, the node is triggered unconditionally.
	defaultDriftInterval = 30 * time.Minute
	maxDriftJitter       = 5 * time.Minute

	// systemErrorRequeueInterval is the retry interval for Graphs with
	// nodes in SystemError state. Per design: "backoff retry with a low
	// cap, then wait for drift timer."
	systemErrorRequeueInterval = 5 * time.Second

	// DefaultMaxConcurrentReconciles is the number of reconcile workers.
	// Multiple workers keep the API server busy — each reconcile does
	// SSA applies, GETs, and informer syncs that can block. Watch index
	// updates are batched (one Lock per reconcile), so worker count does
	// not amplify coordinator lock contention. 16 is a heuristic — high
	// enough to keep a typical API server busy under normal graph
	// workloads, tune if needed.
	DefaultMaxConcurrentReconciles = 16
)

// gvkToGVR converts a GVK to a GVR using English pluralization rules.
// Uses flect.Pluralize for correct handling of irregular plurals
// (e.g., NetworkPolicy → networkpolicies, Ingress → ingresses).
func gvkToGVR(gvk schema.GroupVersionKind) schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    gvk.Group,
		Version:  gvk.Version,
		Resource: flect.Pluralize(strings.ToLower(gvk.Kind)),
	}
}

// GraphReconciler reconciles Graph objects.
type GraphReconciler struct {
	Client    client.Client
	Watcher   *WatchCoordinator // nil = no dynamic watches (backward compat with existing tests)
	Caches    *graphCaches      // per-revision compiled expression caches
	Resources *resourceCache    // per-resource full object cache
}

func (r *GraphReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, reconcileErr error) {
	logger := log.FromContext(ctx)

	// 1. Get the Graph
	graph := &unstructured.Unstructured{}
	graph.SetGroupVersionKind(GraphGVK)
	if err := r.Client.Get(ctx, req.NamespacedName, graph); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Handle deletion
	if !graph.GetDeletionTimestamp().IsZero() {
		return r.reconcileDelete(ctx, graph)
	}

	// Add finalizer
	if !controllerutil.ContainsFinalizer(graph, finalizer) {
		controllerutil.AddFinalizer(graph, finalizer)
		if err := r.Client.Update(ctx, graph); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Set up watch tracking for this reconcile cycle.
	// walkAttempted gates the commit: if no walk happens (empty trigger
	// set), the previous cycle's watch registrations are preserved.
	// Without this, a no-op reconcile (e.g., from status update enqueue)
	// would commit an empty watch set, removing all scalar/collection
	// index entries and releasing informers. Any walk attempt counts —
	// including partial walks that error — because visited nodes consume
	// watch events and register new watches.
	var watcher *graphWatcher
	var walkAttempted bool
	if r.Watcher != nil {
		watcher = r.Watcher.forGraph(req.NamespacedName)
		defer func() {
			watcher.done(walkAttempted && reconcileErr == nil)
		}()
	}

	// -----------------------------------------------------------------------
	// Phase 1: Revision management
	// -----------------------------------------------------------------------
	//
	// Ensure a GraphRevision exists for the current generation. If the spec
	// changed (generation bumped), materialize a new revision. A revision can
	// only be created if compilation succeeds — its existence proves validity.

	activeRevision, supersededRevisions, err := r.ensureRevision(ctx, graph)
	if err != nil {
		// Compilation or materialization failure — no revision created.
		// Report the error on the Graph and return. The returned error is
		// logged once by controller-runtime; no logger.Error here.
		if statusErr := r.updateStatus(ctx, graph, &reconcileState{accepted: false, acceptedErr: err}); statusErr != nil {
			logger.Error(statusErr, "updating status after revision error")
		}
		return ctrl.Result{}, fmt.Errorf("ensuring revision: %w", err)
	}

	// -----------------------------------------------------------------------
	// Phase 2: Node reconciliation from the active revision
	// -----------------------------------------------------------------------

	// Parse and compile the active revision's spec (cached by revision name).
	revisionSpec, state, err := r.compileRevision(activeRevision)
	if err != nil {
		if statusErr := r.updateStatus(ctx, graph, &reconcileState{accepted: false, acceptedErr: err}); statusErr != nil {
			logger.Error(statusErr, "updating status after compilation error")
		}
		return ctrl.Result{}, fmt.Errorf("compiling revision: %w", err)
	}

	eval := newEvaluator(state)
	dag := state.compiled.dag
	state.initResolvedShapes()
	plan := NewPlanState(dag)
	var appliedKeys []string

	// Determine which nodes are triggered this reconcile.
	// Per 004-graph-execution.md § The Walk: nodes evaluate on external
	// triggers or propagation triggers. Otherwise O(1) skip.
	triggered := make(map[string]bool, len(dag.Nodes))
	isRevisionTransition := len(supersededRevisions) > 0
	isFirstReconcile := len(state.previousPlanStates) == 0
	if isFirstReconcile || isRevisionTransition {
		// All nodes triggered on first reconcile or revision transition.
		for _, node := range dag.Nodes {
			triggered[node.ID] = true
		}
		// Transfer previousAppliedKeys from superseded revisions so the
		// prune phase knows what the old revision applied. Without this,
		// a new instanceState (empty previousAppliedKeys) combined with
		// informer lag leaves the prune with no candidates — and if the
		// superseded revision is GC'd, the keys are lost permanently.
		if isRevisionTransition && state.previousAppliedKeys == nil {
			for _, rev := range supersededRevisions {
				oldKey := rev.GetNamespace() + "/" + rev.GetName()
				if oldState := r.Caches.get(oldKey); oldState != nil {
					for k := range oldState.previousAppliedKeys {
						if state.previousAppliedKeys == nil {
							state.previousAppliedKeys = make(map[string]bool)
						}
						state.previousAppliedKeys[k] = true
					}
				}
			}
		}
		// Clean up metric series for nodes removed between revisions.
		// Active revision nodes define the live set; any node in a
		// superseded revision not present in the active set is stale.
		if isRevisionTransition {
			activeNodeIDs := make(map[string]bool, len(dag.Nodes))
			for _, node := range dag.Nodes {
				activeNodeIDs[node.ID] = true
			}
			removedIDs := make(map[string]bool)
			for _, rev := range supersededRevisions {
				if spec, err := extractRevisionSpec(rev); err == nil {
					for _, node := range spec.Nodes {
						if !activeNodeIDs[node.ID] {
							removedIDs[node.ID] = true
						}
					}
				}
			}
			if len(removedIDs) > 0 {
				deleteNodeMetrics(graph.GetName(), graph.GetNamespace(), removedIDs)
			}
		}
	} else if watcher != nil {
		// Watch triggers: specific nodes that received events.
		watchTriggers := watcher.drainTriggers()
		for nodeID := range watchTriggers {
			triggered[nodeID] = true
		}
		// Drift timer triggers: nodes whose consistency timer expired.
		// Per 004-graph-execution.md § The Walk: "Each node has an
		// in-memory drift timer with a jittered interval."
		for _, node := range dag.Nodes {
			if state.isDriftExpired(node.ID) {
				triggered[node.ID] = true
				DriftTimerFiresTotal.With(graphMetricLabels(
					graph.GetName(), graph.GetNamespace(), node.ID,
				)).Inc()
			}
		}
		// SystemError nodes retry (transient error backoff).
		for nodeID, prevState := range state.previousPlanStates {
			if prevState == NodeSystemError {
				triggered[nodeID] = true
				SystemErrorRetriesTotal.With(graphMetricLabels(
					graph.GetName(), graph.GetNamespace(), nodeID,
				)).Inc()
			}
		}
	} else {
		// No watcher — trigger all nodes (backward compat, tests without watches).
		for _, node := range dag.Nodes {
			triggered[node.ID] = true
		}
	}
	// Propagation triggers are set during the walk (step 7) when a node's
	// propagation hash changes. Tracked in propagationTriggered below.
	propagationTriggered := make(map[string]bool)

	// Early exit: no nodes triggered → no walk needed. Preserves previous
	// No triggered nodes → no walk needed. Preserve existing watch state
	// (walkCompleted stays false → watcher.done(false)). Schedule next
	// reconcile at the earliest drift timer expiry.
	//
	// Exception: revision transitions where the superseded revision has
	// nodes not in the active set MUST reach the prune phase even with
	// 0 triggered nodes. Without this, a spec change that removes nodes
	// (or empties the spec) would leave orphaned resources.
	needsPruneSweep := false
	if isRevisionTransition && len(triggered) == 0 {
		activeNodeIDs := make(map[string]bool, len(dag.Nodes))
		for _, node := range dag.Nodes {
			activeNodeIDs[node.ID] = true
		}
		for _, rev := range supersededRevisions {
			if spec, err := extractRevisionSpec(rev); err == nil {
				for _, node := range spec.Nodes {
					if !activeNodeIDs[node.ID] && node.Finalizes == "" {
						needsPruneSweep = true
						break
					}
				}
			}
			if needsPruneSweep {
				break
			}
		}
	}
	if len(triggered) == 0 && !needsPruneSweep {
		if next := state.nextDriftExpiry(); !next.IsZero() {
			if remaining := time.Until(next); remaining > 0 {
				return ctrl.Result{RequeueAfter: remaining}, nil
			}
		}
		return ctrl.Result{}, nil
	}

	// Walk DAG with eager scheduling: nodes are dispatched as soon as their
	// dependencies are satisfied. Workers are pure functions — they receive
	// a read-only scope snapshot and return results. The coordinator is the
	// single writer to shared state (scope, plan, applied keys).
	type nodeResult struct {
		idx        int
		keys       []string
		state      NodeState
		err        error
		scopeKey   string // node ID to set in scope
		scopeValue any    // value to set (the full K8s object or collection)
		// forEach state updates — returned by forEach workers for the
		// coordinator to merge back into the instance state.
		forEachUpdates map[string]map[string][]string // nodeID → itemID → keys
		forEachScopes  map[string]map[string]any      // nodeID → itemID → scope
		forEachItems   map[string][]any               // cache key → collection items
	}

	results := make(chan nodeResult, len(dag.Nodes))
	inflight := 0
	// dispatched guards against double-dispatch: when a node has multiple
	// parents that complete, each parent's completion calls tryDispatch on
	// shared dependents. Without this guard the second call sees NodePending
	// (state hasn't changed yet) and spawns a duplicate goroutine — two
	// goroutines writing to the same maps causes concurrent-map-writes panic.
	// Coordinator-local (single-threaded), no synchronization needed.
	dispatched := make(map[int]bool, len(dag.Nodes))
	// outputsReady tracks nodes whose outputs are available from a previous
	// reconcile (via the skip path) but whose dispatch eligibility remains
	// open. This separates "outputs available" from "evaluation complete."
	// A skipped node makes its outputs available so dependents can check
	// dependencies, but stays NodePending so a propagation trigger can
	// reclaim it for re-evaluation later in the same walk.
	outputsReady := make(map[string]bool, len(dag.Nodes))
	var nodeErrors []string // "nodeID: reason" for status reporting

	// tryDispatch checks if a node can be dispatched. Three outcomes:
	// 1. All dependencies resolved → dispatch to worker
	// 2. Some dependency still inflight (Pending) → skip, it'll be retried
	//    when that dependency completes
	// 3. Some dependency permanently blocked → mark Excluded
	var tryDispatch func(idx int)
	tryDispatch = func(idx int) {
		node := &dag.Nodes[idx]

		if plan.States[node.ID] != NodePending {
			return // already processed or excluded
		}
		if dispatched[idx] {
			return // goroutine already running for this node
		}

		// Finalizer nodes are dormant during normal operation — they only
		// materialize during prune/teardown. Skip them in the walk.
		if node.Finalizes != "" {
			plan.SetState(dag, node.ID, NodeReady) // mark as "done" so dependents can proceed
			return
		}

		// Step 1: Skip check — no external trigger and no propagation trigger.
		// Per 004-graph-execution.md § Wind step 1: retain previous evaluation.
		//
		// Carry forward previous outputs (scope, keys, propagateWhen) so
		// dependents can read them, but leave plan.States as NodePending.
		// This keeps the node dispatch-eligible: if a propagation trigger
		// arrives later in this walk (upstream output changed), tryDispatch
		// can reclaim the node for re-evaluation. Setting NodeReady here
		// would block re-dispatch — tryDispatch returns early for non-Pending.
		if !triggered[node.ID] && !propagationTriggered[node.ID] {
			if prev, ok := state.previousScope[node.ID]; ok {
				eval.scope[node.ID] = prev
			}
			if prevKeys, ok := state.previousKeys[node.ID]; ok {
				appliedKeys = append(appliedKeys, prevKeys...)
			}
			// Carry forward watch registrations so doneGraph doesn't
			// treat this node's watches as stale. Without this, a
			// trigger-scoped walk would remove watches for skipped nodes.
			if watcher != nil {
				watcher.retainWatches(node.ID)
			}
			// Restore propagateWhen from previous cycle.
			if prevState, ok := state.previousPlanStates[node.ID]; ok {
				if (prevState == NodeReady || prevState == NodeNotReady) &&
					len(node.PropagateWhen) > 0 && state.previousScope[node.ID] != nil {
					plan.PropagateReady[node.ID] = eval.checkPropagateWhen(
						node.PropagateWhen, state.previousScope[node.ID], node.ID)
				}
			}
			// Mark outputs available without setting plan.States — node
			// remains dispatch-eligible for propagation triggers.
			outputsReady[node.ID] = true
			// Dispatch dependents — they check outputsReady for dependency
			// satisfaction alongside plan.States.
			for _, depIdx := range dag.Dependents[node.ID] {
				tryDispatch(depIdx)
			}
			return
		}

		// Check dependencies. Two-pass scan to enforce Excluded > Blocked precedence.
		// Per 004-graph-execution.md § Wind step 2: "Excluded takes precedence over
		// Blocked: if a node has both, it is Excluded — the blocked dependency's
		// resolution cannot make the node viable while the Excluded dependency is absent."
		hasExcluded := false
		hasBlocked := false
		hasInflight := false
		for depID := range node.Dependencies {
			depState, exists := plan.States[depID]
			if !exists {
				continue // not a DAG node
			}
			switch depState {
			case NodeReady, NodeNotReady:
				continue // resolved, good
			case NodePending:
				// Dependency still inflight — unless its outputs are ready
				// from the skip path (previous reconcile's state carried forward).
				if outputsReady[depID] {
					if prevState, ok := state.previousPlanStates[depID]; ok {
						switch prevState {
						case NodeReady, NodeNotReady:
							continue // outputs available, dependency satisfied
						case NodeExcluded:
							hasExcluded = true
							continue
						default:
							hasBlocked = true
							continue
						}
					}
					continue // no previous state, assume satisfied
				}
				hasInflight = true
			case NodeExcluded:
				hasExcluded = true
			default:
				// Blocked, DataPending, Error, SystemError, Conflict — uncertain absence
				hasBlocked = true
			}
		}
		// Excluded takes precedence over Blocked (definitive > uncertain).
		if hasExcluded {
			plan.SetState(dag, node.ID, NodeExcluded)
			return
		}
		if hasBlocked {
			plan.SetState(dag, node.ID, NodeBlocked)
			return
		}
		if hasInflight {
			return // dependency still inflight — don't dispatch yet
		}

		// Step 2: propagateWhen check
		if blockedBy := plan.DependencyPropagateBlocked(node); blockedBy != "" {
			logger.V(1).Info("propagateWhen gate — retaining previous state",
				"node", node.ID, "blockedBy", blockedBy)
			if prev, ok := state.previousScope[node.ID]; ok {
				eval.scope[node.ID] = prev
			}
			if prevKeys, ok := state.previousKeys[node.ID]; ok {
				appliedKeys = append(appliedKeys, prevKeys...)
			}
			if prevState, ok := state.previousPlanStates[node.ID]; ok {
				plan.States[node.ID] = prevState
			}
			// Dispatch dependents — this node retained previous state but
			// dependents still need to be evaluated.
			for _, depIdx := range dag.Dependents[node.ID] {
				tryDispatch(depIdx)
			}
			return
		}

		// Step 3: Change check — section-scoped input hashing.
		// Hash the node's dependency inputs (only referenced sections) and
		// compare against the previous reconcile's hash. Three outcomes:
		//
		// 1. Dependency hash match + self-state unchanged + no upstream
		//    readiness change → skip everything
		// 2. Dependency hash match + (self-state changed OR upstream
		//    readiness changed) → skip template/apply, re-evaluate
		//    readyWhen/propagateWhen only
		// 3. Dependency hash mismatch → full evaluation (dispatch to worker)
		//
		// Watch and CollectionWatch nodes are excluded from input hashing
		// because their output is determined by cluster state (GET/List),
		// not by scope data. A Watch node's dependency inputs can be unchanged
		// while a new resource was created in the cluster.
		nodeShape := node.Shape()
		canHashSkip := nodeShape != ShapeWatch && nodeShape != ShapeCollectionWatch
		if canHashSkip {
			if _, hasPrevHash := state.previousInputHashes[node.ID]; hasPrevHash {
				inputHash, hashErr := hashNodeInputs(node, eval.scope)
				if hashErr == nil && inputHash != "" && inputHash == state.previousInputHashes[node.ID] {
					// Dependency inputs unchanged. Check self-state and
					// gate function deps (e.g., dep.ready()).
					prevScope := state.previousScope[node.ID]
					selfChanged := false
					if len(node.SelfSections) > 0 && watcher != nil && prevScope != nil {
						// Extract identity from previousScope to query the informer.
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
								liveRV := watcher.getResourceVersion(gvr, prevNS, prevName)
								if liveRV != "" && liveRV != prevRV {
									selfChanged = true
								}
							}
						}
					}

					// Check if any upstream node referenced via .ready()
					// changed plan state. Readiness is a runtime property
					// outside section scope — compare plan states directly.
					readinessDepChanged := false
					for depID := range node.ReadinessDeps {
						prevState, hasPrev := state.previousPlanStates[depID]
						currState, hasCurr := plan.States[depID]
						if !hasPrev || !hasCurr || prevState != currState {
							readinessDepChanged = true
							break
						}
					}

					if !selfChanged && !readinessDepChanged {
						// Path 1: dependency hash match + self unchanged → skip everything.
						logger.V(1).Info("input hash match — skipping evaluation",
							"node", node.ID)
						if prev, ok := state.previousScope[node.ID]; ok {
							eval.scope[node.ID] = prev
						}
						if prevKeys, ok := state.previousKeys[node.ID]; ok {
							appliedKeys = append(appliedKeys, prevKeys...)
						}
						if watcher != nil {
							watcher.retainWatches(node.ID)
						}
						if prevState, ok := state.previousPlanStates[node.ID]; ok {
							plan.States[node.ID] = prevState
							// Propagate readyWhen result from previous cycle.
							if prevState == NodeReady || prevState == NodeNotReady {
								if len(node.PropagateWhen) > 0 && prevScope != nil {
									plan.PropagateReady[node.ID] = eval.checkPropagateWhen(
										node.PropagateWhen, prevScope, node.ID)
								}
							}
						}
						// Dispatch dependents — this node is done with retained state.
						for _, depIdx := range dag.Dependents[node.ID] {
							tryDispatch(depIdx)
						}
						return
					}

					// Path 2: dependency hash match + self-state changed → skip
					// template evaluation and apply (template inputs unchanged),
					// but GET the live object to refresh scope for downstream
					// consumers and re-evaluate readyWhen/propagateWhen gates.
					logger.V(1).Info("self-state changed — refreshing scope",
						"node", node.ID)
					if prevMap, ok := prevScope.(map[string]any); ok {
						prevMD, _ := prevMap["metadata"].(map[string]any)
						prevAPIVersion, _ := prevMap["apiVersion"].(string)
						prevKind, _ := prevMap["kind"].(string)
						prevNS, _ := prevMD["namespace"].(string)
						prevName, _ := prevMD["name"].(string)
						gv, _ := schema.ParseGroupVersion(prevAPIVersion)
						gvk := gv.WithKind(prevKind)
						readBack := &unstructured.Unstructured{}
						readBack.SetGroupVersionKind(gvk)
						if err := r.Client.Get(ctx, types.NamespacedName{Namespace: prevNS, Name: prevName}, readBack); err == nil {
							eval.scope[node.ID] = readBack.Object
						} else {
							// GET failed — retain previous scope.
							eval.scope[node.ID] = prevScope
						}
					} else if prevScope != nil {
						eval.scope[node.ID] = prevScope
					}
					if prevKeys, ok := state.previousKeys[node.ID]; ok {
						appliedKeys = append(appliedKeys, prevKeys...)
					}
					if watcher != nil {
						watcher.retainWatches(node.ID)
					}
					nodeState := NodeReady
					observed := eval.scope[node.ID]
					if len(node.ReadyWhen) > 0 && observed != nil {
						if err := eval.checkReadiness(node.ReadyWhen, observed, node.ID); err != nil {
							nodeState = NodeNotReady
						}
					}
					plan.States[node.ID] = nodeState
					if (nodeState == NodeReady || nodeState == NodeNotReady) &&
						len(node.PropagateWhen) > 0 && observed != nil {
						plan.PropagateReady[node.ID] = eval.checkPropagateWhen(
							node.PropagateWhen, observed, node.ID)
					}
					state.previousPlanStates[node.ID] = nodeState
					// Dispatch dependents — this node is done with gate re-evaluation.
					for _, depIdx := range dag.Dependents[node.ID] {
						tryDispatch(depIdx)
					}
					return
				}
				// Path 3: dependency hash mismatch or hash error → full evaluation.
				// Fall through to dispatch.
			}
		} // canHashSkip

		// Step 3: includeWhen (evaluated in coordinator — reads shared scope)
		if len(node.IncludeWhen) > 0 {
			included, err := eval.includeWhen(node.IncludeWhen)
			if err != nil {
				if errors.Is(err, ErrDataPending) {
					plan.SetState(dag, node.ID, NodeDataPending)
				} else {
					plan.SetState(dag, node.ID, NodeError)
				}
				return
			}
			if !included {
				plan.SetState(dag, node.ID, NodeExcluded)
				return
			}
		}

		// Build a snapshot evaluator for the worker. Contains the node's
		// dependency data and forEach previous state — the worker can't
		// see or mutate any shared state.
		workerEval := eval.snapshotFor(node, state)

		// Resolve Deferred shapes in the coordinator (single-threaded)
		// before dispatching to workers. This ensures the resolvedShapes
		// map is only written from the coordinator goroutine.
		// ForEach nodes handle their own per-item shape detection — skip.
		nodeShape = state.resolvedShapes[node.ID]
		if nodeShape == ShapeDeferred && node.ForEach == nil {
			resolved, err := r.resolveShape(ctx, graph, *node, workerEval)
			if err != nil {
				// Shape resolution failed — treat like a node error.
				nodeState := NodeDataPending
				if !errors.Is(err, ErrDataPending) {
					info := classifyAPIError(err)
					nodeState = info.state
				}
				plan.SetState(dag, node.ID, nodeState)
				return
			}
			nodeShape = resolved
			state.resolvedShapes[node.ID] = resolved
		}

		// Dispatch to worker goroutine.
		dispatched[idx] = true
		go func(n Node, we *evaluator, shape TemplateShape) {
			keys, err := r.reconcileNode(ctx, graph, n, shape, we, watcher)
			state := NodeReady
			if err != nil {
				switch {
				case errors.Is(err, ErrDataPending):
					state = NodeDataPending
				case errors.Is(err, ErrWaitingForReadiness):
					state = NodeNotReady
				case errors.Is(err, ErrFieldConflict):
					state = NodeConflict
				default:
					state = NodeError
				}
			}
			// The worker's scope now contains the node's output under node.ID.
			results <- nodeResult{
				idx:            idx,
				keys:           keys,
				state:          state,
				err:            err,
				scopeKey:       n.ID,
				scopeValue:     we.scope[n.ID],
				forEachUpdates: we.forEachNewKeys,
				forEachScopes:  we.forEachNewScope,
				forEachItems:   we.forEachNewItems,
			}
		}(*node, workerEval, nodeShape)
		inflight++
	}

	// Seed: dispatch all nodes with no in-graph dependencies.
	for _, idx := range dag.Levels[0] {
		tryDispatch(idx)
	}

	// Coordinator loop: receive completions, merge into scope, dispatch dependents.
	for inflight > 0 {
		res := <-results
		inflight--

		node := &dag.Nodes[res.idx]

		// Error handling: block dependents, continue independent branches.
		// Classify the API error to determine the plan state — NodeError
		// for client errors (4xx), NodeSystemError for server/infra
		// failures (5xx/timeout/network). Both retry; the distinction
		// flows into the status condition for operator triage.
		if res.state == NodeError {
			info := classifyAPIError(res.err)
			plan.SetState(dag, node.ID, info.state)
			state.previousPlanStates[node.ID] = info.state
			nodeErrors = append(nodeErrors, fmt.Sprintf("%s: %s", node.ID, info.reason))
			logger.V(0).Info("error on node", "node", node.ID,
				"state", info.state, "reason", info.reason, "error", res.err)
			// Retain previous keys — the resource may still exist in the cluster.
			if prevKeys, ok := state.previousKeys[node.ID]; ok {
				appliedKeys = append(appliedKeys, prevKeys...)
			}
			// Dispatch dependents — tryDispatch will see the error state
			// and mark them as Blocked via the dependency check.
			for _, depIdx := range dag.Dependents[node.ID] {
				tryDispatch(depIdx)
			}
			continue
		}
		if res.state == NodeConflict {
			plan.SetState(dag, node.ID, NodeConflict)
			state.previousPlanStates[node.ID] = NodeConflict
			state.previousScope[node.ID] = res.scopeValue
			state.previousKeys[node.ID] = res.keys
			nodeErrors = append(nodeErrors, fmt.Sprintf("%s: field conflict", node.ID))
			logger.V(0).Info("conflict on node", "node", node.ID, "error", res.err)
			if prevKeys, ok := state.previousKeys[node.ID]; ok {
				appliedKeys = append(appliedKeys, prevKeys...)
			}
			for _, depIdx := range dag.Dependents[node.ID] {
				tryDispatch(depIdx)
			}
			continue
		}
		if res.state == NodeDataPending {
			plan.SetState(dag, node.ID, NodeDataPending)
			// Reset Contribute shape when a conflicted target disappears.
			if state.resolvedShapes[node.ID] == ShapeContribute &&
				state.previousPlanStates[node.ID] == NodeConflict {
				state.resolvedShapes[node.ID] = ShapeDeferred
				delete(state.previousInputHashes, node.ID)
			}
			state.previousPlanStates[node.ID] = NodeDataPending
			state.previousScope[node.ID] = res.scopeValue
			state.previousKeys[node.ID] = res.keys
			logger.V(1).Info("data pending for node", "node", node.ID, "error", res.err)
			if prevKeys, ok := state.previousKeys[node.ID]; ok {
				appliedKeys = append(appliedKeys, prevKeys...)
			}
			for _, depIdx := range dag.Dependents[node.ID] {
				tryDispatch(depIdx)
			}
			continue
		}

		// Merge worker output into shared scope.
		if res.scopeValue != nil {
			eval.scope[res.scopeKey] = res.scopeValue
		}

		// Merge forEach state updates into the shared instance state.
		for k, v := range res.forEachItems {
			state.forEachItems[k] = v
		}
		for nodeID, itemScopes := range res.forEachScopes {
			state.forEachItemScope[nodeID] = itemScopes
		}
		for nodeID, itemKeys := range res.forEachUpdates {
			state.forEachItemKeys[nodeID] = itemKeys
		}

		// Update plan state.
		plan.SetState(dag, node.ID, res.state)
		if res.state == NodeReady || res.state == NodeNotReady {
			appliedKeys = append(appliedKeys, res.keys...)
		} else {
			// Non-success states that reach here (e.g., NodeNotReady with keys) —
			// retain previous keys since the resource may still exist.
			if prevKeys, ok := state.previousKeys[node.ID]; ok {
				appliedKeys = append(appliedKeys, prevKeys...)
			}
		}

		// Evaluate propagateWhen (coordinator reads from now-merged scope).
		if (res.state == NodeReady || res.state == NodeNotReady) &&
			len(node.PropagateWhen) > 0 && eval.scope[node.ID] != nil {
			plan.PropagateReady[node.ID] = eval.checkPropagateWhen(
				node.PropagateWhen, eval.scope[node.ID], node.ID)
		}

		// Step 7: Propagation check — hash the specific field paths
		// dependents reference from this node's output, plus propagateWhen
		// state. If the hash differs from the previous reconcile, mark
		// dependents as having a propagation trigger.
		// Per 004-graph-execution.md § Wind step 7.
		if res.state == NodeReady || res.state == NodeNotReady {
			if observed := eval.scope[node.ID]; observed != nil {
				propagateHash, err := hashSelfSections(node, observed)
				if err == nil && propagateHash == "" {
					// No SelfSections (collection watch, bare reference) —
					// fall back to hashing the full output. Without this,
					// collection changes would never propagate to forEach.
					if m, ok := observed.(map[string]any); ok {
						propagateHash, err = hashDesiredState(m)
					} else {
						// Array output (collection watch, forEach) — use JSON hash.
						data, jsonErr := json.Marshal(observed)
						if jsonErr == nil {
							h := fnv.New64a()
							h.Write(data)
							propagateHash = fmt.Sprintf("%016x", h.Sum64())
						}
					}
				}
				if err == nil && propagateHash != "" {
					prevHash := state.previousSelfHashes[node.ID]
					if prevHash == "" || propagateHash != prevHash {
						// Propagation hash changed (or first time node
						// produced output) — trigger dependents.
						// Per 004-graph-execution.md § Wind step 7: "If
						// the hash changed — or no previous hash exists —
						// dependents evaluate."
						for _, depIdx := range dag.Dependents[node.ID] {
							propagationTriggered[dag.Nodes[depIdx].ID] = true
						}
					}
					state.previousSelfHashes[node.ID] = propagateHash
				}
			}
		}

		// Save per-node state for next reconcile.
		state.previousScope[node.ID] = eval.scope[node.ID]
		state.previousKeys[node.ID] = res.keys
		state.previousPlanStates[node.ID] = res.state

		// Store input hash for next reconcile's change check (step 3).
		// This enables the content-addressed skip: if dependency inputs
		// haven't changed, template evaluation is deterministic and the
		// write can be skipped. Without this, every triggered node does
		// a full apply cycle including re-creating externally deleted resources.
		if inputHash, err := hashNodeInputs(node, eval.scope); err == nil && inputHash != "" {
			state.previousInputHashes[node.ID] = inputHash
		}

		// Check dependents: dispatch any whose dependencies are now satisfied.
		for _, depIdx := range dag.Dependents[node.ID] {
			tryDispatch(depIdx)
		}
	}

	// Finalize skipped nodes: nodes that were skipped (outputsReady) and never
	// re-dispatched via propagation trigger still have plan.States = Pending.
	// Restore their previous state for the plan summary (status reporting).
	walkAttempted = true
	for nodeID := range outputsReady {
		if plan.States[nodeID] == NodePending {
			if prevState, ok := state.previousPlanStates[nodeID]; ok {
				plan.States[nodeID] = prevState
			}
		}
	}

	// Retain previous keys for blocked nodes. Blocked nodes were never
	// dispatched to workers, so their keys aren't in appliedKeys yet.
	// Without this, blocked resources would appear as prune candidates.
	for _, node := range dag.Nodes {
		if plan.States[node.ID] == NodeBlocked {
			if prevKeys, ok := state.previousKeys[node.ID]; ok {
				appliedKeys = append(appliedKeys, prevKeys...)
			}
		}
	}

	// Derive aggregate state from the DAG plan
	summary := plan.Summary()

	// -----------------------------------------------------------------------
	// Prune resources no longer in the applied set
	// -----------------------------------------------------------------------
	//
	// The applied set is derived from the watch cache — all resources where
	// the Graph's identity label exists in the controller's informer stores.
	// Per 004-graph-execution.md § Applied Set.
	//
	// Prune candidates = appliedSet - currentKeySet.
	// forEach scale-down, includeWhen toggles, and revision transitions all
	// produce the same diff — one mechanism.
	pruneOK := true
	pruneSafe := !summary.HasDataPending && !summary.HasError && !summary.HasSystemError
	if pruneSafe {
		allPreviousKeys := map[string]bool{}
		logger.V(1).Info("prune gate open", "previousAppliedKeys", len(state.previousAppliedKeys), "superseded", len(supersededRevisions))

		// Derive the applied set from the watch cache.
		if r.Watcher != nil {
			appliedSet := r.Watcher.watches.deriveAppliedSet(graph.GetName(), graph.GetNamespace())
			for key := range appliedSet {
				allPreviousKeys[key] = true
			}
		}

		// Include the previous reconcile's applied keys to cover the
		// informer lag window: resources written in the last reconcile
		// might not yet appear in the informer cache. Without this,
		// forEach scale-down and includeWhen toggle produce prune
		// candidates that are missing from the watch cache, preventing
		// cleanup. This is a consistency bridge, not an architectural
		// feature — removable once informer cache consistency is
		// guaranteed within the reconcile loop.
		for k := range state.previousAppliedKeys {
			allPreviousKeys[k] = true
		}
		// Update the previous key set for the next reconcile.
		state.updateAppliedKeys(appliedKeys)

		// Also extract static keys from superseded revisions for resources
		// that may not yet be in the informer cache. Skip finalizer nodes —
		// they're dormant during normal operation and only appear in the
		// applied set when finalization actually creates them.
		supersededDAGs := map[string]*DAG{}
		for _, rev := range supersededRevisions {
			if revSpec, err := extractRevisionSpec(rev); err == nil {
				for _, node := range revSpec.Nodes {
					if node.Finalizes != "" {
						continue // finalizer node — dormant, never in applied set
					}
					if key := resourceKeyFromTemplate(node.Template, graph.GetNamespace()); key != "" {
						allPreviousKeys[key] = true
					}
				}
			}
			// Compile superseded revisions to access their finalizer relationships.
			if _, revState, compileErr := r.compileRevision(rev); compileErr == nil {
				supersededDAGs[rev.GetName()] = revState.compiled.dag
			}
		}

		if len(allPreviousKeys) > 0 {
			_, err := r.pruneRemovedResources(ctx, graph, allPreviousKeys, appliedKeys, dag, supersededDAGs, eval, watcher)
			if err != nil {
				logger.Error(err, "pruning removed resources")
				pruneOK = false
				info := classifyAPIError(err)
				switch info.state {
				case NodeSystemError:
					summary.HasSystemError = true
				case NodeConflict:
					summary.HasConflict = true
				default:
					summary.HasError = true
				}
				nodeErrors = append(nodeErrors, fmt.Sprintf("prune: %s", info.reason))
			}
		}
	}

	// -----------------------------------------------------------------------
	// Update status on Graph and revision
	// -----------------------------------------------------------------------
	rstate := &reconcileState{
		accepted:       true,
		nodeCount:      len(revisionSpec.Nodes),
		appliedCount:   summary.ReadyCount,
		hasDataPending: summary.HasDataPending,
		hasNotReady:    summary.HasNotReady,
		hasBlocked:     summary.HasBlocked,
		hasConflict:    summary.HasConflict,
		hasError:       summary.HasError,
		hasSystemError: summary.HasSystemError,
		nodeErrors:     nodeErrors,
	}
	if err := r.updateStatus(ctx, graph, rstate); err != nil {
		logger.Error(err, "status update")
	}

	// The graph is fully converged when every node is Ready and the spec is
	// accepted. Everything else — errors, conflicts, pending data, not-ready
	// — retries via watch events, not periodic requeue.
	allReady := rstate.accepted && !summary.HasDataPending && !summary.HasNotReady &&
		!summary.HasBlocked && !summary.HasConflict && !summary.HasError && !summary.HasSystemError
	r.updateRevisionStatus(ctx, activeRevision, supersededRevisions, allReady, pruneOK)

	// Reset drift timers for nodes that were successfully evaluated.
	// Per 004-graph-execution.md § The Walk: "An SSA apply resets the
	// drift timer." We reset on evaluation (Ready or NotReady) since
	// the node was fully processed.
	//
	// For nodes in transient non-converged states (DataPending,
	// SystemError), set a short drift timer so they retry quickly.
	// DataPending: the common case (dependency resolves → propagation
	// trigger) is watch-driven. The 1s timer is a fallback for edge
	// cases where no watch event arrives — e.g., externally deleted
	// owned resource where the delete event was consumed but the
	// re-creation needs a second reconcile to clear the cache.
	// SystemError: transient server failure needs backoff retry.
	// NotReady nodes don't need short timers — watch events fire when
	// the resource's status changes. Error/Conflict/Blocked resolve via
	// propagation, revision transition, or the standard drift timer.
	for _, node := range dag.Nodes {
		nodeState := plan.States[node.ID]
		switch nodeState {
		case NodeReady, NodeNotReady:
			state.resetDriftTimer(node.ID, defaultDriftInterval, maxDriftJitter)
		case NodeDataPending:
			state.resetDriftTimer(node.ID, 1*time.Second, 0)
		case NodeSystemError:
			state.resetDriftTimer(node.ID, systemErrorRequeueInterval, 0)
		}
	}

	// Schedule next reconcile. Watch events handle convergence — no
	// periodic polling. The drift timer is the consistency floor.
	// Per 004-graph-execution.md § Why Not: "Periodic full-graph resync
	// ... Informer resyncs trigger all nodes simultaneously — correlated,
	// expensive. Per-node drift timers with jitter amortize resync."
	//
	// Non-converged nodes (DataPending, SystemError) have short drift
	// timers set above, so the earliest expiry reflects urgency.
	if next := state.nextDriftExpiry(); !next.IsZero() {
		if remaining := time.Until(next); remaining > 0 {
			return ctrl.Result{RequeueAfter: remaining}, nil
		}
	}
	return ctrl.Result{}, nil
}

// ---------------------------------------------------------------------------
// Deletion
// ---------------------------------------------------------------------------

func (r *GraphReconciler) reconcileDelete(ctx context.Context, graph *unstructured.Unstructured) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if r.Watcher != nil {
		r.Watcher.removeGraph(types.NamespacedName{Name: graph.GetName(), Namespace: graph.GetNamespace()})
	}

	// Clean up all expression caches for this Graph's revisions.
	revisions, _ := listRevisions(ctx, r.Client, graph.GetName(), graph.GetNamespace())
	for _, rev := range revisions {
		r.Caches.remove(rev.GetNamespace() + "/" + rev.GetName())
	}

	// Clean up the resource cache for this Graph only.
	r.Resources.removeForGraph(graph.GetName(), graph.GetNamespace())

	// Clean up all metric time series for this Graph via partial match.
	// Covers every node that ever emitted a metric, even if revision specs
	// are no longer parseable.
	deleteGraphMetricsForGraph(graph.GetName(), graph.GetNamespace())

	// Collect all managed resource keys from all revisions for this Graph.
	// Keys come from two sources:
	// 1. Watch cache — informer stores scanned for identity labels
	// 2. Static spec extraction (fallback for resources not in cache)
	ownsKeys := map[string]bool{}
	contributeKeys := map[string]bool{} // key → hasStatus encoded in the key

	// Derive applied set from watch cache if available.
	if r.Watcher != nil {
		appliedSet := r.Watcher.watches.deriveAppliedSet(graph.GetName(), graph.GetNamespace())
		for key, entry := range appliedSet {
			if entry.Role == RoleContributes {
				// For contribute keys, we need the contribute prefix format.
				contributeKeys[contributeKeyPrefix+key] = true
			} else {
				ownsKeys[key] = true
			}
		}
	}

	// Also extract static keys from revision specs for coverage.
	for _, rev := range revisions {
		spec, err := extractRevisionSpec(rev)
		if err != nil {
			continue
		}
		for _, node := range spec.Nodes {
			if node.Template == nil {
				continue
			}
			// Skip Watch and CollectionWatch (read-only).
			shape := DetectShape(node.Template)
			if shape == ShapeWatch || shape == ShapeCollectionWatch {
				continue
			}
			// Skip finalizer nodes — dormant during normal operation.
			if node.Finalizes != "" {
				continue
			}
			if key := resourceKeyFromTemplate(node.Template, graph.GetNamespace()); key != "" {
				ownsKeys[key] = true
			}
		}
	}

	// Release Contribute fields first via skeleton apply.
	// Per the design (003-ownership): Contribute never deletes — it releases
	// field ownership so the actual owner retains the resource.
	fieldOwner := graphFieldOwner(graph)
	for key := range contributeKeys {
		resKey, hasStatus := parseContributeKey(key)
		if resKey == "" {
			continue
		}
		gvk, nn := parseResourceKey(resKey)
		if gvk.Kind == "" {
			continue
		}
		if err := skeletonApply(ctx, r.Client, gvk, nn.Namespace, nn.Name, fieldOwner, hasStatus); err != nil {
			logger.Error(err, "releasing contribution fields during teardown", "key", resKey)
		} else {
			logger.V(1).Info("released contribution fields during teardown", "key", resKey)
		}
	}

	// Convert Owns keys to slice for ordered deletion.
	var keys []string
	for k := range ownsKeys {
		keys = append(keys, k)
	}

	// Also include dynamically-named resources found by label selector.
	// This catches forEach-stamped resources that aren't in the static spec.
	dynamicKeys, _ := r.findManagedResourceKeys(ctx, graph)
	for _, k := range dynamicKeys {
		if !ownsKeys[k] {
			keys = append(keys, k)
		}
	}

	// Pass 1: Issue deletes in reverse topological order.
	// Track which keys we actually attempted to delete (had our hash).
	deletedKeys := map[string]bool{}
	deleteOrder, err := r.deletionOrder(graph, keys)
	if err != nil {
		// Per the design (004-graph-execution): teardown is blocked until
		// ordering is available — never degrade to unordered deletion.
		logger.Error(err, "cannot determine deletion order, requeueing")
		return ctrl.Result{RequeueAfter: systemErrorRequeueInterval}, nil
	}

	// Compile the active revision (if available) to get the DAG for
	// finalizer relationships and an evaluator for template rendering.
	var teardownDAG *DAG
	var teardownEval *evaluator
	if len(revisions) > 0 {
		if _, state, compileErr := r.compileRevision(revisions[0]); compileErr == nil {
			teardownDAG = state.compiled.dag
			teardownEval = newEvaluator(state)
		}
	}

	// Build resource-key-to-node-ID map for finalizer lookup during teardown.
	keyToNodeID := map[string]string{}
	finalizerNodeKeys := map[string]bool{} // keys of finalizer nodes — skip from regular deletion
	if teardownDAG != nil {
		for _, node := range teardownDAG.Nodes {
			if node.Template != nil {
				if rk := resourceKeyFromTemplate(node.Template, graph.GetNamespace()); rk != "" {
					keyToNodeID[rk] = node.ID
					if node.Finalizes != "" {
						finalizerNodeKeys[rk] = true
					}
				}
			}
		}
	}

	teardownBlocked := false
	for _, key := range deleteOrder {
		if key == "" {
			continue
		}
		// Skip finalizer node keys — they're created and cleaned up as
		// part of the finalization sequence, not as regular resources.
		if finalizerNodeKeys[key] {
			continue
		}
		gvk, nn := parseResourceKey(key)
		if gvk.Kind == "" {
			continue
		}
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(gvk)
		obj.SetName(nn.Name)
		obj.SetNamespace(nn.Namespace)

		// Check if we successfully owned this resource (has our hash annotation)
		if err := r.Client.Get(ctx, nn, obj); err != nil {
			continue // already gone
		}
		objAnnotations := obj.GetAnnotations()
		if objAnnotations == nil || objAnnotations[templateHashAnnotation] == "" {
			logger.V(1).Info("skipping delete for resource without template hash (never successfully applied)", "key", key)
			continue
		}

		// Contributor-aware deletion: check managedFields for other field
		// managers before deleting. If present, deletion is blocked — the
		// finalizer holds until the other manager releases.
		ownManager := string(graphFieldOwner(graph))
		if blockers := thirdPartyFieldManagers(obj, ownManager); len(blockers) > 0 {
			logger.Info("teardown blocked: resource has other field managers",
				"key", key, "blockers", blockers)
			teardownBlocked = true
			continue // skip delete — finalizer holds
		}

		// Finalization: if this target has finalizer nodes, run the
		// finalization sequence before deleting.
		if teardownDAG != nil && teardownEval != nil {
			nodeID := keyToNodeID[key]
			if finalizerNodeIDs, ok := teardownDAG.Finalizers[nodeID]; ok && len(finalizerNodeIDs) > 0 {
				ready, _, finErr := r.runFinalization(ctx, graph, obj, finalizerNodeIDs, teardownDAG, teardownEval, nil)
				if finErr != nil {
					logger.Error(finErr, "teardown finalization failed", "key", key)
					teardownBlocked = true
					continue // TeardownBlocked — can't create/check finalizer
				}
				if !ready {
					logger.Info("teardown finalization in progress — deletion deferred",
						"key", key, "finalizers", finalizerNodeIDs)
					teardownBlocked = true
					continue // block deletion until all finalizers ready
				}
				logger.Info("teardown finalization complete", "key", key)
			}
		}

		deletedKeys[key] = true
		if err := r.Client.Delete(ctx, obj); err != nil {
			if client.IgnoreNotFound(err) != nil {
				return ctrl.Result{}, fmt.Errorf("deleting managed resource %s: %w", key, err)
			}
		} else {
			logger.V(1).Info("deleted managed resource", "key", key)

			// Clean up finalizer resources after the target is deleted.
			if teardownDAG != nil {
				nodeID := keyToNodeID[key]
				if finalizerNodeIDs, ok := teardownDAG.Finalizers[nodeID]; ok {
					for _, finNodeID := range finalizerNodeIDs {
						if finIdx, ok2 := teardownDAG.Index[finNodeID]; ok2 {
							finNode := &teardownDAG.Nodes[finIdx]
							if finNode.Template != nil {
								if fk := resourceKeyFromTemplate(finNode.Template, graph.GetNamespace()); fk != "" {
									fGVK, fNN := parseResourceKey(fk)
									finDel := &unstructured.Unstructured{}
									finDel.SetGroupVersionKind(fGVK)
									finDel.SetName(fNN.Name)
									finDel.SetNamespace(fNN.Namespace)
									if delErr := r.Client.Delete(ctx, finDel); delErr != nil {
										logger.V(1).Info("finalizer resource cleanup", "key", fk, "error", delErr)
									} else {
										logger.V(1).Info("cleaned up finalizer resource", "key", fk)
									}
								}
							}
						}
					}
				}
			}
		}
	}

	// Pass 2: Verify managed resources that we actually deleted are gone.
	// Only check resources that had our template hash — others (e.g., conflicted
	// resources that were never successfully applied) are not our responsibility.
	for key := range deletedKeys {
		gvk, nn := parseResourceKey(key)
		if gvk.Kind == "" {
			continue
		}
		check := &unstructured.Unstructured{}
		check.SetGroupVersionKind(gvk)
		if err := r.Client.Get(ctx, nn, check); err == nil {
			logger.V(1).Info("waiting for managed resource to be deleted", "key", key)
			return ctrl.Result{RequeueAfter: 500 * time.Millisecond}, nil
		}
	}

	// If any resource deletion was blocked by third-party field managers,
	// requeue — the finalizer holds until the other managers release.
	if teardownBlocked {
		logger.Info("teardown blocked: waiting for third-party field managers to release")
		return ctrl.Result{RequeueAfter: systemErrorRequeueInterval}, nil
	}

	// Pass 3: Delete all GraphRevisions.
	for _, rev := range revisions {
		if err := deleteRevision(ctx, r.Client, rev); err != nil {
			if client.IgnoreNotFound(err) != nil {
				logger.Error(err, "deleting revision", "revision", rev.GetName())
			}
		} else {
			logger.V(1).Info("deleted revision", "revision", rev.GetName())
		}
	}

	controllerutil.RemoveFinalizer(graph, finalizer)
	if err := r.Client.Update(ctx, graph); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// hydrateWatchCachesFromRevisions lists all existing GraphRevisions and starts
// a watch informer for every GVR referenced in their node templates.
//
// This is the replacement for the per-revision finalizer. After a controller
// restart, the three sources for allPreviousKeys in the prune phase are:
//
//	(1) deriveAppliedSet — watch cache, requires an active informer for the GVR
//	(2) state.previousAppliedKeys — in-memory, lost on restart
//	(3) superseded revision static keys — extracted from revision objects
//
// Without hydration, source (1) is empty on startup for cross-GVR transitions:
// if the current revision manages a ConfigMap but a superseded revision managed
// a Deployment, no ConfigMap informer is running for the Deployment GVR.
// Hydration fixes this by starting informers eagerly from all existing
// revisions before the first reconcile fires — using the same graphOwnerID as
// the normal reconcile path so ref-counting works naturally.
//
// Called synchronously in SetupWithManager before the controller is registered,
// so there is no window where a reconcile fires before hydration completes.
func hydrateWatchCachesFromRevisions(restConfig *rest.Config, watchMgr *WatchManager) {
	logger := log.Log.WithName("startup-hydration")

	dynClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		logger.Error(err, "creating dynamic client; skipping startup watch hydration")
		return
	}

	graphRevisionGVR := schema.GroupVersionResource{
		Group:    "experimental.kro.run",
		Version:  "v1alpha1",
		Resource: "graphrevisions",
	}

	list, err := dynClient.Resource(graphRevisionGVR).Namespace("").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		// CRD may not exist on first installation. Log and skip — no existing
		// revisions to hydrate from.
		logger.Info("could not list GraphRevisions; skipping startup watch hydration", "err", err)
		return
	}

	// Collect unique (graph, gvr) pairs across all revisions.
	type hydrateKey struct {
		graph graphKey
		gvr   schema.GroupVersionResource
	}
	toHydrate := make(map[hydrateKey]struct{})

	for i := range list.Items {
		rev := &list.Items[i]
		graphName := rev.GetLabels()[LabelRevisionGraphName]
		if graphName == "" {
			continue
		}
		graph := graphKey{Name: graphName, Namespace: rev.GetNamespace()}

		spec, err := extractRevisionSpec(rev)
		if err != nil {
			logger.V(1).Info("skipping revision during hydration", "revision", rev.GetName(), "err", err)
			continue
		}
		for _, node := range spec.Nodes {
			if node.Finalizes != "" {
				continue // finalizer node — dormant during normal operation
			}
			tmpl := node.Template
			if tmpl == nil {
				continue
			}
			apiVersion, _ := tmpl["apiVersion"].(string)
			kind, _ := tmpl["kind"].(string)
			if apiVersion == "" || kind == "" {
				continue
			}
			gv, err := schema.ParseGroupVersion(apiVersion)
			if err != nil {
				continue
			}
			gvr := gvkToGVR(gv.WithKind(kind))
			toHydrate[hydrateKey{graph: graph, gvr: gvr}] = struct{}{}
		}
	}

	if len(toHydrate) == 0 {
		return
	}

	// Start informers in parallel — each ensureWatch blocks until cache sync,
	// so parallel execution reduces startup time from O(n·30s) to O(30s).
	var wg sync.WaitGroup
	for k := range toHydrate {
		wg.Add(1)
		go func(graph graphKey, gvr schema.GroupVersionResource) {
			defer wg.Done()
			ownerID := graphOwnerID(graph)
			if err := watchMgr.ensureWatch(gvr, ownerID); err != nil {
				logger.Error(err, "failed to hydrate watch", "gvr", gvr, "graph", graph.Name)
			} else {
				logger.V(1).Info("hydrated watch from revision", "gvr", gvr, "graph", graph.Name)
			}
		}(k.graph, k.gvr)
	}
	wg.Wait()
	logger.Info("startup watch hydration complete", "watchCount", len(toHydrate))
}

// SetupWithManager registers the Graph controller with a controller-runtime
// manager. This is the single setup path for both production and tests.
// It creates the watch infrastructure internally — callers provide the
// manager and a rest.Config (needed for the metadata client).
//
// maxWorkers controls MaxConcurrentReconciles. Values ≤ 0 default to 4.
// Multiple workers prevent watch event starvation under load — with a
// single worker, dynamic watch events can't be delivered while it's busy
// processing another Graph's reconcile.
//
// Returns a shutdown function that stops the watch manager. The caller
// must invoke this on teardown.
func SetupWithManager(mgr ctrl.Manager, restConfig *rest.Config, maxWorkers int) (shutdown func(), err error) {
	RegisterMetrics(crmetrics.Registry)

	if maxWorkers <= 0 {
		maxWorkers = DefaultMaxConcurrentReconciles
	}

	metadataClient, err := metadata.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("creating metadata client: %w", err)
	}

	watchChan := make(chan event.GenericEvent, 256)

	watchMgr := newWatchManager(metadataClient, 0, nil, log.Log)
	coordinator := newWatchCoordinator(watchMgr, func(graph graphKey) {
		obj := &unstructured.Unstructured{}
		obj.SetName(graph.Name)
		obj.SetNamespace(graph.Namespace)
		watchChan <- event.GenericEvent{Object: obj}
	}, log.Log)
	watchMgr.onEvent = coordinator.routeEvent

	reconciler := &GraphReconciler{
		Client:    mgr.GetClient(),
		Watcher:   coordinator,
		Caches:    newGraphCaches(),
		Resources: newResourceCache(),
	}

	// Pre-populate watch informers from existing GraphRevisions before the
	// controller starts. This ensures deriveAppliedSet works for cross-GVR
	// transitions on the first reconcile after a restart — no window where
	// a reconcile fires before the cache is hydrated.
	hydrateWatchCachesFromRevisions(restConfig, watchMgr)

	graphObj := &unstructured.Unstructured{}
	graphObj.SetGroupVersionKind(GraphGVK)

	if err := ctrl.NewControllerManagedBy(mgr).
		For(graphObj).
		Named("graph").
		WithOptions(controller.Options{MaxConcurrentReconciles: maxWorkers}).
		WatchesRawSource(source.Channel(watchChan, &handler.EnqueueRequestForObject{})).
		Complete(reconciler); err != nil {
		return nil, fmt.Errorf("building controller: %w", err)
	}

	return watchMgr.shutdown, nil
}
