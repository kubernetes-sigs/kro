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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

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
	triggered            map[string]bool
	resyncTriggered       map[string]bool
	propagationTriggered map[string]bool

	// Watch incremental cache — drained once at walk start.
	// Per 005-reconciliation.md § Propagation.
	collectionChanges map[string][]watches.CollectionChange // nodeID → changes

	// Walk-local tracking
	dispatched        map[int]bool
	outputsReady      map[string]bool
	appliedKeys       []string
	nodeErrors        []string // "nodeID: reason" for status reporting
	results           chan nodeResult
	inflight          int
	dynamicGVKChanged bool // set when a dynamic GVK resolves for the first time or changes

	// staleReadinessDeps tracks nodes dispatched to workers while some
	// of their ReadinessDeps were still inflight. The worker snapshot
	// captured stale .ready() values. After the walk, explicit triggers
	// are deposited for these nodes so the next reconcile re-evaluates
	// them with fresh scope from completed readiness deps.
	staleReadinessDeps map[string]bool
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
		w.propagationTriggered[w.dag.Nodes[depIdx].ID] = true
		w.tryDispatch(depIdx)
	}
}

// carryForwardKeys appends a node's previous applied keys to the walk's
// applied key set. Used when a node is skipped, blocked, or in error — the
// resource may still exist in the cluster, so its keys must remain in the
// applied set to prevent spurious pruning.
func (w *walkState) carryForwardKeys(nodeID string) {
	if prevKeys, ok := w.state.previousKeys[nodeID]; ok {
		w.appliedKeys = append(w.appliedKeys, prevKeys...)
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
	for _, depIdx := range w.dag.ReadinessDependents[node.ID] {
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
	// Include readiness state in the propagation hash so downstream nodes
	// that reference .ready() are triggered when readiness changes, even
	// if output field paths are unchanged.
	if nodeState == dagpkg.NodeReady {
		propagateHash += ":ready=true"
	} else {
		propagateHash += ":ready=false"
	}
	prevHash := w.state.previousSelfHashes[node.ID]
	if prevHash == "" || propagateHash != prevHash {
		for _, depIdx := range w.dag.Dependents[node.ID] {
			w.propagationTriggered[w.dag.Nodes[depIdx].ID] = true
		}
		// ReadinessDependents consume .ready() state without a hard DAG
		// edge. They need the same propagation trigger so tryDispatch
		// bypasses the skip check and re-evaluates them with the updated
		// readiness verdict.
		for _, depIdx := range w.dag.ReadinessDependents[node.ID] {
			w.propagationTriggered[w.dag.Nodes[depIdx].ID] = true
		}
	}
	w.state.previousSelfHashes[node.ID] = propagateHash
	return true
}

// unconditionalPropagate marks all dependents and readiness dependents as
// propagation-triggered. Used when the propagation hash cannot be computed
// — the safe fallback is to assume output changed.
func (w *walkState) unconditionalPropagate(node *graphpkg.Node) {
	for _, depIdx := range w.dag.Dependents[node.ID] {
		w.propagationTriggered[w.dag.Nodes[depIdx].ID] = true
	}
	for _, depIdx := range w.dag.ReadinessDependents[node.ID] {
		w.propagationTriggered[w.dag.Nodes[depIdx].ID] = true
	}
}

// tryDispatch checks if a node can be dispatched. Three outcomes:
// 1. All dependencies resolved → dispatch to worker
// 2. Some dependency still inflight → skip, retried when dependency completes
// 3. Some dependency permanently blocked → mark Excluded
func (w *walkState) tryDispatch(idx int) {
	node := &w.dag.Nodes[idx]
	logger := log.FromContext(w.ctx)

	if w.plan.States[node.ID] != dagpkg.NodeUnvisited {
		return // already processed or excluded
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
		w.skipNode(node)
		return
	}

	// Check dependencies.
	hasExcluded := false
	hasBlocked := false
	hasPending := false
	hasInflight := false
	for depID := range node.Dependencies {
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
		// Nodes with propagateWhen can handle excluded dependencies —
		// the gate expression decides whether to proceed despite
		// exclusion. Example: rgdStatus has propagateWhen:
		// ${!validation.result.valid || crd.ready()}. When crd is
		// excluded (invalid spec), the gate short-circuits to true
		// and rgdStatus fires to write the error status. Without
		// this check, Excluded propagates unconditionally and the
		// status patch never happens.
		if len(node.PropagateWhen) == 0 || node.ForEach != nil {
			logger.V(1).Info("node excluded — dependency excluded", "node", node.ID)
			w.plan.SetState(w.dag, node.ID, dagpkg.NodeExcluded)
			w.state.previousPlanStates[node.ID] = dagpkg.NodeExcluded
			delete(w.state.previousEvalHashes, node.ID)
			w.notifyDependents(node.ID)
			return
		}
		// Fall through to propagateWhen evaluation.
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
			// When a dependency is Excluded and the gate ERRORS (not just
			// blocks), the gate can't evaluate because its inputs are
			// missing — the excluded dependency's data is absent from scope.
			// Contagious exclusion should proceed: the gate didn't actively
			// decide "no," it failed to decide at all.
			if hasExcluded && gate == gateError {
				w.plan.SetState(w.dag, node.ID, dagpkg.NodeExcluded)
				w.state.previousPlanStates[node.ID] = dagpkg.NodeExcluded
				delete(w.state.previousEvalHashes, node.ID)
				w.notifyDependents(node.ID)
				return
			}

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
	declaredNodeType := node.Type()
	canHashSkip := declaredNodeType != graphpkg.NodeTypeRef && declaredNodeType != graphpkg.NodeTypeWatch && !w.resyncTriggered[node.ID]
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
							if liveRV != "" && liveRV != prevRV {
								selfChanged = true
							}
						}
					}
				}

				readinessDepChanged := false
				for depID := range node.ReadinessDeps {
					prevState, hasPrev := w.state.previousPlanStates[depID]
					currState, hasCurr := w.plan.States[depID]
					if !hasPrev || !hasCurr || prevState != currState {
						readinessDepChanged = true
						break
					}
				}

			if !selfChanged && !readinessDepChanged {
				// Path 1: skip everything — inputs unchanged, live state
				// unchanged (informer RV matches), no readiness dep change.
				logger.V(1).Info("evaluation hash match — skipping evaluation",
					"node", node.ID)
				if prevState, ok := w.state.previousPlanStates[node.ID]; ok {
					w.plan.States[node.ID] = prevState
				}
				w.skipNode(node)
				return
			}

				// When a readiness dep's plan state changed (e.g.,
				// deployment went from NotReady to Ready), body .ready()
				// calls will return different values. Path 2 only refreshes
				// the node's own scope — it doesn't re-evaluate the body.
				// Fall through to Path 3 so the body picks up the new
				// readiness state and the status patch transitions from
				// IN_PROGRESS to ACTIVE.
				if readinessDepChanged {
					logger.V(1).Info("readiness dep changed — falling through to full evaluation",
						"node", node.ID)
					delete(w.state.previousEvalHashes, node.ID)
					goto fullEval
				}

				// Path 2: watch-triggered or self-state changed — refresh scope.
				//
				// This is the primary fast path for watch-driven state changes.
				// A watch event signals a resource change (e.g., external
				// controller updated status) but the node's template inputs
				// haven't changed (eval hash match). Re-read live state,
				// re-evaluate readyWhen, propagate only if output changed.
				//
				// Known liveness bound: the APIReader.Get below runs in the
				// single-threaded coordinator, serializing API server round
				// trips at O(N × RTT). If profiling shows this is a
				// bottleneck, move the GET to a lightweight worker goroutine.
				logger.V(1).Info("watch refresh — re-reading live state",
					"node", node.ID)
				SelfRefreshTotal.With(graphMetricLabels(
					w.graph.GetName(), w.graph.GetNamespace(), node.ID,
				)).Inc()
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
					// Direct API server read — bypasses the controller-runtime
					// cache, which can lag behind the metadata informer and
					// serve stale data.
					if err := w.r.apiReader().Get(w.ctx, types.NamespacedName{Namespace: prevNS, Name: prevName}, readBack); err == nil {
						w.eval.scope[node.ID] = readBack.Object
					} else if apierrors.IsNotFound(err) {
						// Resource was deleted externally. Path 2 can't
						// refresh — fall through to Path 3 (full evaluation)
						// which will re-create the resource via SSA apply.
						logger.V(1).Info("resource deleted externally — falling through to full evaluation",
							"node", node.ID)
						delete(w.state.previousEvalHashes, node.ID)
						goto fullEval
					} else {
						w.eval.scope[node.ID] = prevScope
					}
				} else if prevScope != nil {
					w.eval.scope[node.ID] = prevScope
				}
				w.carryForwardKeys(node.ID)
				if w.watcher != nil {
					w.watcher.RetainWatches(node.ID)
				}
				nodeState := dagpkg.NodeReady
				if err := w.eval.evalReadiness(node.ID, node.ReadyWhen); err != nil {
					nodeState = dagpkg.NodeNotReady
				}
				w.plan.States[node.ID] = nodeState
				w.state.previousPlanStates[node.ID] = nodeState
				w.state.previousScope[node.ID] = w.eval.scope[node.ID]
				if evalHash, err := hashNodeInputs(node, w.eval.scope); err == nil && evalHash != "" {
					w.state.previousEvalHashes[node.ID] = evalHash
				}
				// Propagation check: hash the output fields dependents
				// reference and compare with the previous reconcile. Only
				// mark dependents as propagation-triggered if the hash
				// actually changed. This mirrors Step 8 in the coordinator
				// loop — without it, every watch event cascades through
				// the entire downstream subgraph.
				if observed := w.eval.scope[node.ID]; observed != nil {
					if !w.propagateIfChanged(node, observed, nodeState) {
						w.unconditionalPropagate(node)
					}
				}
			for _, depIdx := range w.dag.Dependents[node.ID] {
				w.tryDispatch(depIdx)
			}
			for _, depIdx := range w.dag.ReadinessDependents[node.ID] {
				w.tryDispatch(depIdx)
			}
			return
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

	// Track stale readiness dispatch: if any ReadinessDep (not a hard dep)
	// is still inflight, the worker snapshot has stale .ready() values.
	// After the walk, explicit triggers are deposited so the next
	// reconcile re-evaluates this node with fresh scope.
	for depID := range node.ReadinessDeps {
		if node.Dependencies[depID] {
			continue
		}
		if w.plan.States[depID] == dagpkg.NodeUnvisited && !w.outputsReady[depID] {
			if w.staleReadinessDeps == nil {
				w.staleReadinessDeps = map[string]bool{}
			}
			w.staleReadinessDeps[node.ID] = true
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
