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
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gobuffalo/flect"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

var GraphGVK = schema.GroupVersionKind{
	Group:   "kro.run",
	Version: "v1alpha1",
	Kind:    "Graph",
}

const (
	finalizer = "kro.run/graph-controller"

	// defaultRequeueAfter is used when a resource is not yet ready
	// and we need to wait for the dynamic watch to trigger. This is
	// a fallback — the watch should fire first in most cases.
	defaultRequeueAfter = 1 * time.Second
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
	var watcher *graphWatcher
	if r.Watcher != nil {
		watcher = r.Watcher.forGraph(req.NamespacedName)
		defer func() {
			watcher.done(reconcileErr == nil)
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

	// Scoped walk: determine which nodes to visit this reconcile.
	// nil walkScope = full walk (resync, error retry, first reconcile, revision transition).
	// non-nil walkScope = only visit nodes in the scope.
	var walkScope map[string]bool
	isRevisionTransition := len(supersededRevisions) > 0
	if watcher != nil && !isRevisionTransition {
		triggers := watcher.drainTriggers()
		if len(triggers) > 0 {
			walkScope = ScopeFromTriggers(dag, triggers)
			// If no triggers mapped to DAG nodes, the scope is empty but
			// there were real events (e.g., owned resource status updates).
			// Fall back to full walk — empty scope would skip all nodes.
			if len(walkScope) == 0 {
				walkScope = nil
			} else {
				logger.V(1).Info("scoped walk", "triggers", len(triggers), "scope", len(walkScope), "dagSize", len(dag.Nodes))
			}
		}
	}
	// Revision transitions force full walks — the DAG structure changed
	// and stale entries would never be cleaned up by a scoped walk.
	// See 004-graph-execution.md § Revision Transitions.

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

		// Scoped walk: if the node is outside the walk scope, retain
		// previous state. This is equivalent to a change check match —
		// the node is untouched because the triggering event doesn't
		// affect it. See 004-graph-execution.md § Scoped Walks.
		if walkScope != nil && !walkScope[node.ID] {
			if prev, ok := state.previousScope[node.ID]; ok {
				eval.scope[node.ID] = prev
			}
			if prevKeys, ok := state.previousKeys[node.ID]; ok {
				appliedKeys = append(appliedKeys, prevKeys...)
			}
			if prevState, ok := state.previousPlanStates[node.ID]; ok {
				plan.States[node.ID] = prevState
				// Restore propagateWhen from previous cycle.
				if (prevState == NodeReady || prevState == NodeNotReady) &&
					len(node.PropagateWhen) > 0 && state.previousScope[node.ID] != nil {
					plan.PropagateReady[node.ID] = eval.checkPropagateWhen(
						node.PropagateWhen, state.previousScope[node.ID], node.ID)
				}
			}
			// Dispatch dependents — this node is "done" (retained previous state)
			// and its dependents may be in scope and waiting.
			for _, depIdx := range dag.Dependents[node.ID] {
				tryDispatch(depIdx)
			}
			return
		}

		// Check dependencies. Distinguish "still running" from "permanently blocked."
		for depID := range node.Dependencies {
			state, exists := plan.States[depID]
			if !exists {
				continue // not a DAG node
			}
			switch state {
			case NodeReady, NodeNotReady:
				continue // resolved, good
			case NodePending:
				return // dependency still inflight — don't dispatch yet, don't exclude
			default:
				// Excluded, DataPending, Error, SystemError, Conflict — blocked
				plan.SetState(dag, node.ID, NodeExcluded)
				return
			}
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
		// 1. Dependency hash match + self-state unchanged → skip everything
		// 2. Dependency hash match + self-state changed   → skip template/apply,
		//    re-evaluate readyWhen/propagateWhen only
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
					// Dependency inputs unchanged. Check self-state.
					prevScope := state.previousScope[node.ID]
					selfHash, selfErr := hashSelfSections(node, prevScope)
					selfChanged := false
					if selfErr == nil && selfHash != "" {
						prevSelfHash := state.previousSelfHashes[node.ID]
						selfChanged = selfHash != prevSelfHash
					}

					if !selfChanged {
						// Path 1: dependency hash match + self unchanged → skip everything.
						logger.V(1).Info("input hash match — skipping evaluation",
							"node", node.ID)
						if prev, ok := state.previousScope[node.ID]; ok {
							eval.scope[node.ID] = prev
						}
						if prevKeys, ok := state.previousKeys[node.ID]; ok {
							appliedKeys = append(appliedKeys, prevKeys...)
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

					// Path 2: dependency hash match + self-state changed → re-evaluate
					// gates only (readyWhen/propagateWhen). Template hasn't changed so
					// skip template evaluation and apply.
					logger.V(1).Info("self-state changed — re-evaluating gates only",
						"node", node.ID)
					if prev, ok := state.previousScope[node.ID]; ok {
						eval.scope[node.ID] = prev
					}
					if prevKeys, ok := state.previousKeys[node.ID]; ok {
						appliedKeys = append(appliedKeys, prevKeys...)
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
					// Update self hash for next reconcile.
					if newSelfHash, err := hashSelfSections(node, observed); err == nil {
						state.previousSelfHashes[node.ID] = newSelfHash
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
			// Dispatch dependents — tryDispatch will see NodeError
			// and exclude them via the dependency check.
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
		}

		// Evaluate propagateWhen (coordinator reads from now-merged scope).
		if (res.state == NodeReady || res.state == NodeNotReady) &&
			len(node.PropagateWhen) > 0 && eval.scope[node.ID] != nil {
			plan.PropagateReady[node.ID] = eval.checkPropagateWhen(
				node.PropagateWhen, eval.scope[node.ID], node.ID)
		}

		// Save per-node state for next reconcile.
		state.previousScope[node.ID] = eval.scope[node.ID]
		state.previousKeys[node.ID] = res.keys
		state.previousPlanStates[node.ID] = res.state

		// Store input hash for next reconcile's change check.
		if inputHash, err := hashNodeInputs(node, eval.scope); err == nil && inputHash != "" {
			state.previousInputHashes[node.ID] = inputHash
		}
		// Store self hash for gate re-evaluation detection.
		if observed := eval.scope[node.ID]; observed != nil {
			if selfHash, err := hashSelfSections(node, observed); err == nil {
				state.previousSelfHashes[node.ID] = selfHash
			}
		}

		// Check dependents: dispatch any whose dependencies are now satisfied.
		for _, depIdx := range dag.Dependents[node.ID] {
			tryDispatch(depIdx)
		}
	}

	// Derive aggregate state from the DAG plan
	summary := plan.Summary()

	// Persist the applied set on the revision for prune diffing and teardown.
	//
	// ORDERING INVARIANT: read the previous applied set BEFORE writing the
	// new one. Intra-revision prune (forEach scale-down, includeWhen toggle)
	// depends on comparing the previous cycle's keys to the current cycle's
	// keys. If setAppliedSet runs first, the previous state is lost and
	// intra-revision prune silently stops working.
	// Protected by: TestForEachCollectionScaleUpDown, TestIncludeWhenToggle.
	previousAppliedSet := getAppliedSet(activeRevision)
	if len(appliedKeys) > 0 {
		if err := setAppliedSet(ctx, r.Client, activeRevision, appliedKeys); err != nil {
			logger.V(1).Info("failed to persist applied set", "error", err)
		}
	}

	// -----------------------------------------------------------------------
	// Prune resources no longer in the applied set
	// -----------------------------------------------------------------------
	//
	// Two prune sources, unioned into a single previous key set:
	// 1. Superseded revisions' applied sets — handles cross-revision prune
	//    (spec changes that remove/rename resources).
	// 2. Active revision's PREVIOUS applied set — handles intra-revision
	//    prune (forEach scale-down, includeWhen toggle, data-driven changes
	//    within the same generation).
	//
	// The prune candidate set is: union(all previous keys) - current keys.
	pruneOK := true
	if !summary.HasDataPending {
		allPreviousKeys := map[string]bool{}
		for _, rev := range supersededRevisions {
			revAppliedSet := getAppliedSet(rev)
			if len(revAppliedSet) > 0 {
				for _, k := range revAppliedSet {
					allPreviousKeys[k] = true
				}
			} else {
				// Fall back to spec-based extraction for revisions without annotations.
				if revSpec, err := extractRevisionSpec(rev); err == nil {
					for _, node := range revSpec.Nodes {
						if key := resourceKeyFromTemplate(node.Template, graph.GetNamespace()); key != "" {
							allPreviousKeys[key] = true
						}
					}
				}
			}
		}
		for _, k := range previousAppliedSet {
			allPreviousKeys[k] = true
		}
		if len(allPreviousKeys) > 0 {
			if err := r.pruneRemovedResources(ctx, graph, allPreviousKeys, appliedKeys); err != nil {
				logger.Error(err, "pruning removed resources")
				pruneOK = false
				// Classify prune error through the same taxonomy as node errors.
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
	// — retries. The environment can change independently of the controller's
	// inputs, so we always retry.
	allReady := rstate.accepted && !summary.HasDataPending && !summary.HasNotReady &&
		!summary.HasConflict && !summary.HasError && !summary.HasSystemError
	r.updateRevisionStatus(ctx, activeRevision, supersededRevisions, allReady, pruneOK)

	if !allReady {
		return ctrl.Result{RequeueAfter: defaultRequeueAfter}, nil
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

	// Collect all managed resource keys from all revisions for this Graph.
	// Keys come from two sources:
	// 1. Applied set annotations (accurate for dynamic names, forEach, contribute)
	// 2. Static spec extraction (fallback for revisions without annotations)
	ownsKeys := map[string]bool{}
	contributeKeys := map[string]bool{} // key → hasStatus encoded in the key
	for _, rev := range revisions {
		// Try applied set annotation first — includes both Owns and Contribute keys.
		if keys := getAppliedSet(rev); len(keys) > 0 {
			for _, k := range keys {
				if strings.HasPrefix(k, contributeKeyPrefix) {
					contributeKeys[k] = true
				} else {
					ownsKeys[k] = true
				}
			}
			continue
		}

		// Fall back: extract static Owns keys from the revision's spec.
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
		return ctrl.Result{RequeueAfter: defaultRequeueAfter}, nil
	}
	for _, key := range deleteOrder {
		if key == "" {
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

		deletedKeys[key] = true
		if err := r.Client.Delete(ctx, obj); err != nil {
			if client.IgnoreNotFound(err) != nil {
				return ctrl.Result{}, fmt.Errorf("deleting managed resource %s: %w", key, err)
			}
		} else {
			logger.V(1).Info("deleted managed resource", "key", key)
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

// SetupWithManager registers the Graph controller with a controller-runtime
// manager. This is the single setup path for both production and tests.
// It creates the watch infrastructure internally — callers provide the
// manager and a rest.Config (needed for the metadata client).
//
// Returns a shutdown function that stops the watch manager. The caller
// must invoke this on teardown.
func SetupWithManager(mgr ctrl.Manager, restConfig *rest.Config) (shutdown func(), err error) {
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

	graphObj := &unstructured.Unstructured{}
	graphObj.SetGroupVersionKind(GraphGVK)

	if err := ctrl.NewControllerManagedBy(mgr).
		For(graphObj).
		Named("graph").
		WatchesRawSource(source.Channel(watchChan, &handler.EnqueueRequestForObject{})).
		Complete(reconciler); err != nil {
		return nil, fmt.Errorf("building controller: %w", err)
	}

	return watchMgr.shutdown, nil
}
