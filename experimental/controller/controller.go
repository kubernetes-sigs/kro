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
//  2. Walk the DAG via dependency-driven scheduling, evaluating pre-compiled CEL programs
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
	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apiserver/pkg/cel/openapi/resolver"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/rest"

	schemaresolver "github.com/kubernetes-sigs/kro/pkg/graph/schema/resolver"
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

// DefaultDriftInterval is the per-node consistency floor interval.
// Per 004-graph-reconciliation.md § Reconcile: "Each node has an in-memory
// drift timer with a jittered interval (default 30 minutes)."
// On expiry, the node bypasses the apply-hash check and applies
// unconditionally.
var DefaultDriftInterval = 30 * time.Minute

// MaxDriftJitter is the maximum random jitter added to the drift interval.
// Decorrelates timers across nodes to avoid correlated bursts.
var MaxDriftJitter = 5 * time.Minute

const (
	finalizer = "experimental.kro.run/graph-controller"

	// systemErrorRequeueInterval is the retry interval for Graphs with
	// nodes in SystemError state. Per design: "backoff retry with a low
	// cap, then wait for drift timer."
	systemErrorRequeueInterval = 5 * time.Second

	// finalizationRequeueInterval is the consistency floor for finalization
	// in progress. The primary trigger is a watch event on the gate
	// resource (when readyWhen's dependencies change). This timer ensures
	// the gate is re-checked even if that watch event is slow.
	// Per 004-graph-reconciliation.md § Finalization: "wait for readyWhen before
	// deleting the target."
	finalizationRequeueInterval = 5 * time.Second

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
	Client         client.Client
	SchemaResolver resolver.SchemaResolver // nil = all resource nodes fall back to dyn
	Watcher        *WatchCoordinator       // nil = no dynamic watches (backward compat with existing tests)
	Caches         *graphCaches            // per-revision compiled expression caches
	Resources      *resourceCache          // per-resource full object cache
	DriftInterval  time.Duration           // per-node drift timer interval; 0 = use DefaultDriftInterval
	DriftJitter    time.Duration           // max drift jitter; 0 = use MaxDriftJitter
	Scope          GVKScopeResolver        // nil = unknown scope; staticResourceKey falls back to namespace-substitution heuristic
}

// nodeResult carries a worker's output back to the coordinator.
type nodeResult struct {
	idx          int
	keys         []string
	state        NodeState
	err          error
	scopeKey     string        // node ID to set in scope
	scopeValue   any           // value to set (the full K8s object or collection)
	evalDuration time.Duration // wall-clock time of reconcileNode (measured inside the worker)
	// forEach state updates — returned by forEach workers for the
	// coordinator to merge back into the instance state.
	forEachUpdates map[string]map[string][]string // nodeID → itemID → keys
	forEachScopes  map[string]map[string]any      // nodeID → itemID → scope
	forEachItems   map[string][]any               // cache key → collection items
	// WatchKind cache update — returned by WatchKind workers for the
	// coordinator to merge back into the instance state.
	watchKindCacheUpdate map[string][]any // node ID → updated cached list
	// watchKindDidFullList is true when the WatchKind worker took the
	// full-List path (full re-List from API server), as opposed to
	// incremental merge. Coordinator uses this to clear the dirty flag
	// only after an authoritative refresh.
	watchKindDidFullList bool
	// nodeReadyUpdate carries the node's readyWhen verdict from worker
	// back to the coordinator. Used by the AST-rewritten `.ready()`
	// lookup for WatchKind nodes (see readyrewrite.go). nil if the
	// worker did not evaluate readiness (e.g., non-WatchKind or
	// early-exit path).
	nodeReadyUpdate *bool
	// forEach propagateWhen aggregate — true if all items satisfy
	// propagateWhen. nil if not applicable (non-forEach or no propagateWhen).
	forEachAllItemsPropagateReady *bool
}

// walkState holds coordinator-local state for a single DAG walk.
// Extracted from the Reconcile closure for readability — the coordinator
// loop in Reconcile reads/writes these fields directly.
type walkState struct {
	r       *GraphReconciler
	ctx     context.Context
	graph   *unstructured.Unstructured
	dag     *DAG
	eval    *evaluator
	state   *instanceState
	plan    *PlanState
	watcher *graphWatcher

	// Trigger maps
	triggered            map[string]bool
	driftTriggered       map[string]bool
	propagationTriggered map[string]bool

	// WatchKind incremental cache — drained once at walk start.
	// Per 004-graph-reconciliation.md § Propagation.
	collectionChanges map[string][]CollectionChange // nodeID → changes

	// Walk-local tracking
	dispatched   map[int]bool
	outputsReady map[string]bool
	appliedKeys  []string
	nodeErrors   []string // "nodeID: reason" for status reporting
	results      chan nodeResult
	inflight     int
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

// tryDispatch checks if a node can be dispatched. Three outcomes:
// 1. All dependencies resolved → dispatch to worker
// 2. Some dependency still inflight → skip, retried when dependency completes
// 3. Some dependency permanently blocked → mark Excluded
func (w *walkState) tryDispatch(idx int) {
	node := &w.dag.Nodes[idx]
	logger := log.FromContext(w.ctx)

	if w.plan.States[node.ID] != nodeUnvisited {
		return // already processed or excluded
	}
	if w.dispatched[idx] {
		return // goroutine already running for this node
	}

	// Finalizer nodes are dormant during normal operation — they only
	// materialize during prune/teardown. Skip them in the walk.
	if node.Finalizes != "" {
		w.plan.SetState(w.dag, node.ID, NodeReady)
		return
	}

	// Step 1: Skip check — no external trigger and no propagation trigger.
	if !w.triggered[node.ID] && !w.propagationTriggered[node.ID] {
		if prev, ok := w.state.previousScope[node.ID]; ok {
			w.eval.scope[node.ID] = prev
		}
		w.carryForwardKeys(node.ID)
		if w.watcher != nil {
			w.watcher.retainWatches(node.ID)
		}
		if prevState, ok := w.state.previousPlanStates[node.ID]; ok {
			if (prevState == NodeReady || prevState == NodeNotReady) &&
				len(node.PropagateWhen) > 0 && w.state.previousScope[node.ID] != nil {
				if node.ForEach != nil {
					// forEach: carry forward the previous PropagateReady.
					// Per-item evaluation only happens in the worker;
					// the coordinator scope has the array, not items.
					if prev, exists := w.state.previousPropagateReady[node.ID]; exists {
						w.plan.PropagateReady[node.ID] = prev
					}
				} else {
					w.plan.PropagateReady[node.ID] = w.eval.checkPropagateWhen(
						node.PropagateWhen, node.ID)
				}
			}
		}
		w.outputsReady[node.ID] = true
		for _, depIdx := range w.dag.Dependents[node.ID] {
			w.tryDispatch(depIdx)
		}
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
		case NodeReady, NodeNotReady:
			continue
		case nodeUnvisited:
			if w.outputsReady[depID] {
				if prevState, ok := w.state.previousPlanStates[depID]; ok {
					switch prevState {
					case NodeReady, NodeNotReady:
						continue
					case NodeExcluded:
						hasExcluded = true
						continue
					case NodePending:
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
		case NodeExcluded:
			hasExcluded = true
		case NodePending:
			hasPending = true
		default:
			hasBlocked = true
		}
	}
	if hasExcluded {
		w.plan.SetState(w.dag, node.ID, NodeExcluded)
		w.notifyDependents(node.ID)
		return
	}
	if hasBlocked {
		w.plan.SetState(w.dag, node.ID, NodeBlocked)
		w.notifyDependents(node.ID)
		return
	}
	if hasPending {
		w.plan.SetState(w.dag, node.ID, NodePending)
		w.notifyDependents(node.ID)
		return
	}
	if hasInflight {
		return
	}

	// Step 3: propagateWhen check
	if blockedBy := w.plan.DependencyPropagateBlocked(node); blockedBy != "" {
		logger.V(1).Info("propagateWhen gate — retaining previous state",
			"node", node.ID, "blockedBy", blockedBy)
		if prev, ok := w.state.previousScope[node.ID]; ok {
			w.eval.scope[node.ID] = prev
		}
		w.carryForwardKeys(node.ID)
		if prevState, ok := w.state.previousPlanStates[node.ID]; ok {
			w.plan.States[node.ID] = prevState
		} else {
			w.plan.States[node.ID] = NodePending
		}
		for _, depIdx := range w.dag.Dependents[node.ID] {
			w.tryDispatch(depIdx)
		}
		return
	}

	// Step 4: Evaluation check — section-scoped evaluation hashing.
	nodeRef := node.Reference()
	canHashSkip := nodeRef != ReferenceWatch && nodeRef != ReferenceWatchKind && !w.driftTriggered[node.ID]
	if canHashSkip {
		if _, hasPrevHash := w.state.previousEvalHashes[node.ID]; hasPrevHash {
			evalHash, hashErr := hashNodeInputs(node, w.eval.scope)
			if hashErr == nil && evalHash != "" && evalHash == w.state.previousEvalHashes[node.ID] {
				prevScope := w.state.previousScope[node.ID]
				selfChanged := false
				if len(node.SelfPaths) > 0 && w.watcher != nil && prevScope != nil {
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
							liveRV := w.watcher.getResourceVersion(gvr, prevNS, prevName)
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
					// Path 1: skip everything.
					logger.V(1).Info("evaluation hash match — skipping evaluation",
						"node", node.ID)
					if prev, ok := w.state.previousScope[node.ID]; ok {
						w.eval.scope[node.ID] = prev
					}
					w.carryForwardKeys(node.ID)
					if w.watcher != nil {
						w.watcher.retainWatches(node.ID)
					}
					if prevState, ok := w.state.previousPlanStates[node.ID]; ok {
						w.plan.States[node.ID] = prevState
						if prevState == NodeReady || prevState == NodeNotReady {
							if len(node.PropagateWhen) > 0 && prevScope != nil {
								if node.ForEach != nil {
									if prev, exists := w.state.previousPropagateReady[node.ID]; exists {
										w.plan.PropagateReady[node.ID] = prev
									}
								} else {
									w.plan.PropagateReady[node.ID] = w.eval.checkPropagateWhen(
										node.PropagateWhen, node.ID)
								}
							}
						}
					}
					for _, depIdx := range w.dag.Dependents[node.ID] {
						w.tryDispatch(depIdx)
					}
					return
				}

				// Path 2: self-state changed — refresh scope.
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
					if err := w.r.Client.Get(w.ctx, types.NamespacedName{Namespace: prevNS, Name: prevName}, readBack); err == nil {
						w.eval.scope[node.ID] = readBack.Object
					} else {
						w.eval.scope[node.ID] = prevScope
					}
				} else if prevScope != nil {
					w.eval.scope[node.ID] = prevScope
				}
				w.carryForwardKeys(node.ID)
				if w.watcher != nil {
					w.watcher.retainWatches(node.ID)
				}
				nodeState := NodeReady
				scopeData := w.eval.scope[node.ID]
				if len(node.ReadyWhen) > 0 && scopeData != nil {
					if err := w.eval.checkReadiness(node.ReadyWhen, node.ID); err != nil {
						nodeState = NodeNotReady
					}
				}
				w.plan.States[node.ID] = nodeState
				if (nodeState == NodeReady || nodeState == NodeNotReady) &&
					len(node.PropagateWhen) > 0 && scopeData != nil {
					if node.ForEach != nil {
						// forEach: self-state changed but we're in the coordinator,
						// not the worker. Carry forward previous per-item result.
						if prev, exists := w.state.previousPropagateReady[node.ID]; exists {
							w.plan.PropagateReady[node.ID] = prev
						}
					} else {
						w.plan.PropagateReady[node.ID] = w.eval.checkPropagateWhen(
							node.PropagateWhen, node.ID)
					}
				}
				w.state.previousPlanStates[node.ID] = nodeState
				for _, depIdx := range w.dag.Dependents[node.ID] {
					w.tryDispatch(depIdx)
				}
				return
			}
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
			if errors.Is(err, ErrPending) {
				w.plan.SetState(w.dag, node.ID, NodePending)
			} else {
				w.plan.SetState(w.dag, node.ID, NodeError)
			}
			w.notifyDependents(node.ID)
			return
		}
		if !included {
			// Excluded is definitive absence — the node's previous resources
			// are a legitimate prune candidate, so keys are NOT carried
			// forward. Per 004-graph-reconciliation.md § Prune: "Excluded
			// propagates as Excluded (definitive absence — safe to prune)."
			w.plan.SetState(w.dag, node.ID, NodeExcluded)
			w.notifyDependents(node.ID)
			return
		}
		// Transitioning from Excluded to included: re-resolve the reference.
		// The previous classification may be stale (e.g., a resource that
		// existed when we first resolved may have been deleted by the
		// previous owner's teardown). Deleting the map entry signals the
		// resolution chokepoint that the node needs classification again —
		// absent = needs resolution, present = already a ResolvedReference.
		if prev, ok := w.state.previousPlanStates[node.ID]; ok && prev == NodeExcluded {
			delete(w.state.resolvedReferences, node.ID)
		}
	}

	// Build snapshot evaluator for the worker.
	workerEval := w.eval.snapshotFor(node, w.state)

	// WatchKind incremental cache: pass cached list and collection changes
	// to the worker so reconcileWatchKind can GET only changed items.
	if node.Reference() == ReferenceWatchKind {
		workerEval.watchKindNodeID = node.ID
		cached, hasCached := w.state.watchKindCache[node.ID]
		// dirty: a previous incremental reconcile errored mid-merge,
		// so the drained CollectionChanges were lost. Force a full
		// re-List from the API server to recover authoritative state.
		// Per 004-graph-reconciliation.md § Propagation: incremental
		// updates must not allow the cache to serve stale data beyond
		// the drift interval when a recoverable error interrupted a
		// merge.
		dirty := w.state.watchKindDirty[node.ID]
		if hasCached && !w.driftTriggered[node.ID] && !dirty {
			workerEval.watchKindCachedList = cached
			workerEval.watchKindChanges = w.collectionChanges[node.ID]
		} else {
			// First reconcile, drift, or previous incremental error:
			// force full list.
			workerEval.watchKindDriftOrFull = true
		}
	}

	// Resolve Unresolved references in the coordinator. The map entry's
	// presence or absence is authoritative — absent means the node has
	// not been resolved yet (either first reconcile or a post-Excluded/
	// Conflict reset), present means the ResolvedReference is ready to
	// dispatch. Compile-time-classified references (Own/Watch/WatchKind/
	// Definition) were populated by initResolvedReferences; only
	// Unresolved nodes reach this block with an absent entry.
	resolvedRef, haveResolved := w.state.resolvedReferences[node.ID]
	if !haveResolved && node.ForEach == nil {
		resolved, err := w.r.resolveReference(w.ctx, w.graph, *node, workerEval)
		if err != nil {
			nodeState := NodePending
			if !errors.Is(err, ErrPending) {
				info := classifyAPIError(err)
				nodeState = info.state
				// Store deterministic errors in previousPlanStates so the
				// state survives the skip path on subsequent reconciles.
				// Without this, the error is overwritten with Ready on the
				// next reconcile (plan.States stays nodeUnvisited → Summary
				// ignores the node). Pending errors are NOT stored — the
				// data may become available on the next reconcile.
				w.state.previousPlanStates[node.ID] = nodeState
				w.nodeErrors = append(w.nodeErrors, fmt.Sprintf("%s: %s", node.ID, err))
				logger.V(0).Info("error resolving reference", "node", node.ID, "state", nodeState, "error", err)
			}
			// Retain previous applied keys — the resource may still exist
			// from a prior successful apply. Without this, the resource
			// would be a prune candidate. The prune gate independently
			// blocks on error states, but key retention makes the error
			// path self-contained rather than relying on a distant safety
			// net.
			w.carryForwardKeys(node.ID)
			w.plan.SetState(w.dag, node.ID, nodeState)
			w.notifyDependents(node.ID)
			return
		}
		resolvedRef = resolved
		w.state.resolvedReferences[node.ID] = resolvedRef
	}

	// Dispatch to worker goroutine.
	w.dispatched[idx] = true
	isDrift := w.driftTriggered[node.ID]
	go func(n Node, we *evaluator, ref ResolvedReference, driftCorrection bool) {
		evalStart := time.Now()
		keys, err := w.r.reconcileNode(w.ctx, w.graph, n, ref, we, w.watcher, driftCorrection)
		evalDuration := time.Since(evalStart)
		state := NodeReady
		if err != nil {
			switch {
			case errors.Is(err, ErrPending):
				state = NodePending
			case errors.Is(err, ErrWaitingForReadiness):
				state = NodeNotReady
			case errors.Is(err, ErrReadyWhenFailed):
				// Per 001-graph.md: "readyWhen is a health signal — it does
				// not gate downstream execution." A broken readyWhen expression
				// (wrong return type, CEL error) must not produce NodeError
				// (which blocks dependents). NodeNotReady preserves the invariant.
				state = NodeNotReady
			case errors.Is(err, ErrFieldConflict):
				state = NodeConflict
			default:
				state = NodeError
			}
		}
		// Extract the worker's node-readiness verdict (WatchKind-only).
		// The worker wrote its verdict into its local nodeReady copy;
		// the coordinator merges the value back into the shared map
		// once the result is received. Non-WatchKind workers do not
		// write to nodeReady, so the lookup returns (false, false).
		var nodeReadyUpdate *bool
		if we.nodeReady != nil {
			if v, ok := we.nodeReady[n.ID]; ok {
				nodeReadyUpdate = &v
			}
		}
		w.results <- nodeResult{
			idx:                           idx,
			keys:                          keys,
			state:                         state,
			err:                           err,
			scopeKey:                      n.ID,
			scopeValue:                    we.scope[n.ID],
			evalDuration:                  evalDuration,
			forEachUpdates:                we.forEachNewKeys,
			forEachScopes:                 we.forEachNewScope,
			forEachItems:                  we.forEachNewItems,
			watchKindCacheUpdate:          we.watchKindCacheUpdate(),
			watchKindDidFullList:          we.watchKindDidFullList,
			forEachAllItemsPropagateReady: we.forEachAllItemsPropagateReady,
			nodeReadyUpdate:               nodeReadyUpdate,
		}
	}(*node, workerEval, resolvedRef, isDrift)
	w.inflight++
}

// driftInterval returns the effective drift interval for this reconciler.
func (r *GraphReconciler) driftInterval() time.Duration {
	if r.DriftInterval > 0 {
		return r.DriftInterval
	}
	return DefaultDriftInterval
}

// driftJitter returns the effective drift jitter for this reconciler.
func (r *GraphReconciler) driftJitter() time.Duration {
	if r.DriftInterval > 0 {
		// When interval is overridden, use overridden jitter (even if 0).
		return r.DriftJitter
	}
	return MaxDriftJitter
}

func (r *GraphReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, reconcileErr error) {
	reconcileStart := time.Now()
	logger := log.FromContext(ctx)
	// requeueFloor is an explicit requeue interval independent of drift timers.
	// Set when a transient condition needs re-checking beyond what watch events
	// guarantee — e.g., finalization in progress. Zero means no explicit floor.
	var requeueFloor time.Duration

	// 1. Get the Graph
	graph := &unstructured.Unstructured{}
	graph.SetGroupVersionKind(GraphGVK)
	if err := r.Client.Get(ctx, req.NamespacedName, graph); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Observe reconcile duration on return. Placed after the Graph fetch
	// so the label values are available; the timer started before the fetch
	// to include its latency.
	defer func() {
		ReconcileDurationSeconds.With(prometheus.Labels{
			"graph_name":      graph.GetName(),
			"graph_namespace": graph.GetNamespace(),
		}).Observe(time.Since(reconcileStart).Seconds())
	}()

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
	var compilationErr error // non-nil when current generation failed to compile
	if err != nil {
		// Compilation or materialization failure — no revision created for
		// the current generation. Per 004-graph-execution.md § Compilation:
		// "Reconciliation continues on the previous revision if one exists."
		// Fall back to the most recent existing revision so healthy resources
		// keep converging. A typo in the spec should not halt management.
		graphName := graph.GetName()
		namespace := graph.GetNamespace()
		revisions, listErr := listRevisions(ctx, r.Client, graphName, namespace)
		if listErr != nil || len(revisions) == 0 {
			// No previous revision to fall back to — truly stuck.
			if statusErr := r.updateStatus(ctx, graph, &reconcileState{compiled: false, compiledErr: err}); statusErr != nil {
				logger.Error(statusErr, "updating status after revision error")
			}
			return ctrl.Result{}, fmt.Errorf("ensuring revision: %w", err)
		}
		// Use the most recent revision (listRevisions returns sorted by
		// generation ascending, so the last element is the latest).
		activeRevision = revisions[len(revisions)-1]
		supersededRevisions = nil // No transition — same revision as before.
		compilationErr = err
		logger.Error(err, "compilation failed for current generation")
		logger.Info("falling back to previous revision",
			"revision", activeRevision.GetName(),
			"generation", revisionGeneration(activeRevision))
	}

	// effectiveGeneration is the generation to stamp on identity labels
	// during apply. Normally this is graph.GetGeneration() — the current
	// generation matches what we're converging to. On a compilation-failure
	// fallback, we're converging to a prior revision, so the labels must
	// reflect that revision's generation, not the failed one — otherwise
	// identity labels lie about which generation materialized the resource.
	// Plumbed as an explicit parameter so the choice is visible at stamp
	// sites rather than mutating the graph object as a side channel.
	effectiveGeneration := graph.GetGeneration()
	if compilationErr != nil {
		effectiveGeneration = revisionGeneration(activeRevision)
	}

	// -----------------------------------------------------------------------
	// Phase 2: Node reconciliation from the active revision
	// -----------------------------------------------------------------------

	// Parse and compile the active revision's spec (cached by revision name).
	revisionSpec, state, err := r.compileRevision(activeRevision)
	if err != nil {
		if statusErr := r.updateStatus(ctx, graph, &reconcileState{compiled: false, compiledErr: err}); statusErr != nil {
			logger.Error(statusErr, "updating status after compilation error")
		}
		return ctrl.Result{}, fmt.Errorf("compiling revision: %w", err)
	}

	eval := newEvaluator(state)
	eval.effectiveGeneration = effectiveGeneration
	dag := state.compiled.dag
	state.initResolvedReferences()
	plan := NewPlanState(dag)

	// Determine which nodes are triggered this reconcile.
	// Per 004-graph-reconciliation.md § Reconcile: nodes evaluate on external
	// triggers or propagation triggers. Otherwise O(1) skip.
	triggered := make(map[string]bool, len(dag.Nodes))
	// driftTriggered tracks nodes triggered specifically by the drift timer.
	// Per 004-graph-reconciliation.md § Reconcile: "The drift timer bypasses the
	// template-hash check — apply unconditionally." Drift-triggered nodes
	// skip the step 3 evaluation hash check AND force the SSA Patch in step 5,
	// because the question is "does live state match desired state?" not
	// "did inputs change?" — different questions with different cache semantics.
	driftTriggered := make(map[string]bool, len(dag.Nodes))
	var collectionChanges map[string][]CollectionChange // WatchKind incremental cache
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
		// Per 004-graph-execution.md § Revision transition: "Nodes that
		// differ are triggered." On revision transition, transfer previous
		// state from the superseded revision's cache for unchanged nodes.
		// The evaluation hash check at Step 4 will then skip unchanged
		// nodes — they appear to have been reconciled before with
		// identical inputs, so template evaluation and SSA apply are
		// elided. Changed and new nodes start fresh (no previous state).
		if isRevisionTransition {
			changedNodes := diffRevisionNodes(revisionSpec, supersededRevisions)
			if changedNodes != nil {
				// Transfer state from the most recent superseded revision.
				baseline := supersededRevisions[len(supersededRevisions)-1]
				oldKey := baseline.GetNamespace() + "/" + baseline.GetName()
				if oldState := r.Caches.get(oldKey); oldState != nil {
					for _, node := range dag.Nodes {
						if !changedNodes[node.ID] {
							// Node spec unchanged — inherit previous state.
							if v, ok := oldState.previousScope[node.ID]; ok {
								state.previousScope[node.ID] = v
							}
							if v, ok := oldState.previousPlanStates[node.ID]; ok {
								state.previousPlanStates[node.ID] = v
							}
							if v, ok := oldState.previousPropagateReady[node.ID]; ok {
								state.previousPropagateReady[node.ID] = v
							}
							if v, ok := oldState.previousEvalHashes[node.ID]; ok {
								state.previousEvalHashes[node.ID] = v
							}
							if v, ok := oldState.previousSelfHashes[node.ID]; ok {
								state.previousSelfHashes[node.ID] = v
							}
							if v, ok := oldState.previousKeys[node.ID]; ok {
								state.previousKeys[node.ID] = v
							}
							if v, ok := oldState.resolvedReferences[node.ID]; ok {
								state.resolvedReferences[node.ID] = v
							}
						}
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
		// WatchKind collection changes: buffered resource keys for
		// incremental cache updates. Drained alongside triggers so the
		// coordinator knows which specific items changed.
		collectionChanges = watcher.drainCollectionChanges()
		// Drift timer triggers: nodes whose consistency timer expired.
		// Per 004-graph-reconciliation.md § Reconcile: "Each node has an
		// in-memory drift timer with a jittered interval (default 30
		// minutes). On expiry, the node runs the full pipeline (steps
		// 1-7). The drift timer bypasses the template-hash check —
		// apply unconditionally."
		for _, node := range dag.Nodes {
			if state.isDriftExpired(node.ID) {
				triggered[node.ID] = true
				driftTriggered[node.ID] = true
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
	walk := &walkState{
		r:                    r,
		ctx:                  ctx,
		graph:                graph,
		dag:                  dag,
		eval:                 eval,
		state:                state,
		plan:                 plan,
		watcher:              watcher,
		triggered:            triggered,
		driftTriggered:       driftTriggered,
		propagationTriggered: propagationTriggered,
		collectionChanges:    collectionChanges,
		dispatched:           make(map[int]bool, len(dag.Nodes)),
		outputsReady:         make(map[string]bool, len(dag.Nodes)),
		results:              make(chan nodeResult, len(dag.Nodes)),
	}

	// Seed: dispatch all nodes with no in-graph dependencies.
	for _, idx := range dag.Levels[0] {
		walk.tryDispatch(idx)
	}

	// Coordinator loop: receive completions, merge into scope, dispatch dependents.
	for walk.inflight > 0 {
		res := <-walk.results
		walk.inflight--

		node := &dag.Nodes[res.idx]

		// Observe per-node evaluation duration (measured inside the worker).
		if res.evalDuration > 0 {
			NodeEvalDurationSeconds.With(graphMetricLabels(
				graph.GetName(), graph.GetNamespace(), node.ID,
			)).Observe(res.evalDuration.Seconds())
		}

		// WatchKind incremental-cache integrity: if the worker errored
		// AND did not persist a cache update, the drained
		// CollectionChanges are lost. Mark the node dirty so the next
		// reconcile takes the full-list path to recover authoritative
		// state from the API server. Without this, stale cache can
		// persist for up to the drift interval (default 30m). Per
		// 004-graph-reconciliation.md § Propagation.
		if res.err != nil && node.Reference() == ReferenceWatchKind &&
			len(res.watchKindCacheUpdate) == 0 {
			state.watchKindDirty[node.ID] = true
		}

		// Error handling: block dependents, continue independent branches.
		// Classify the API error to determine the plan state — NodeError
		// for client errors (4xx), NodeSystemError for server/infra
		// failures (5xx/timeout/network). Both retry; the distinction
		// flows into the status condition for operator triage.
		if res.state == NodeError {
			info := classifyAPIError(res.err)
			plan.SetState(dag, node.ID, info.state)
			state.previousPlanStates[node.ID] = info.state
			walk.nodeErrors = append(walk.nodeErrors, fmt.Sprintf("%s: %s", node.ID, info.reason))
			logger.V(0).Info("error on node", "node", node.ID,
				"state", info.state, "reason", info.reason, "error", res.err)
			// Retain previous keys — the resource may still exist in the cluster.
			walk.carryForwardKeys(node.ID)
			// Dispatch dependents — tryDispatch will see the error state
			// and mark them as Blocked via the dependency check.
			for _, depIdx := range dag.Dependents[node.ID] {
				walk.tryDispatch(depIdx)
			}
			continue
		}
		if res.state == NodeConflict {
			plan.SetState(dag, node.ID, NodeConflict)
			state.previousPlanStates[node.ID] = NodeConflict
			state.previousScope[node.ID] = res.scopeValue
			state.previousKeys[node.ID] = res.keys
			walk.nodeErrors = append(walk.nodeErrors, fmt.Sprintf("%s: field conflict", node.ID))
			logger.V(0).Info("conflict on node", "node", node.ID, "error", res.err)
			walk.carryForwardKeys(node.ID)
			for _, depIdx := range dag.Dependents[node.ID] {
				walk.tryDispatch(depIdx)
			}
			continue
		}
		if res.state == NodePending {
			plan.SetState(dag, node.ID, NodePending)
			// Reset Contribute reference when a conflicted target disappears.
			// Deleting the map entry signals the resolution chokepoint that
			// the node needs classification again on the next reconcile.
			if state.resolvedReferences[node.ID] == ResolvedReferenceContribute &&
				state.previousPlanStates[node.ID] == NodeConflict {
				delete(state.resolvedReferences, node.ID)
				delete(state.previousEvalHashes, node.ID)
			}
			state.previousPlanStates[node.ID] = NodePending
			state.previousScope[node.ID] = res.scopeValue
			state.previousKeys[node.ID] = res.keys
			logger.V(1).Info("data pending for node", "node", node.ID, "error", res.err)
			walk.carryForwardKeys(node.ID)
			for _, depIdx := range dag.Dependents[node.ID] {
				walk.tryDispatch(depIdx)
			}
			continue
		}

		// Merge worker output into shared scope.
		if res.scopeValue != nil {
			eval.scope[res.scopeKey] = res.scopeValue
		}

		// Merge node-readiness verdict (WatchKind workers). Writing
		// here makes the verdict visible to every subsequent node's
		// CEL evaluations via the AST-rewritten `<wk_id>.ready()`
		// lookup — including dependents that fan out after this node
		// completes.
		if res.nodeReadyUpdate != nil && eval.nodeReady != nil {
			eval.nodeReady[res.scopeKey] = *res.nodeReadyUpdate
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

		// Merge WatchKind cache updates into the shared instance state.
		// Dirty is cleared only when the worker took the full-List path
		// — that's the only thing that recovers from a lost incremental
		// merge. An incremental success against an already-stale cache
		// does not address the staleness. Per 004-graph-reconciliation.md
		// § Propagation.
		for nodeID, cached := range res.watchKindCacheUpdate {
			state.watchKindCache[nodeID] = cached
			if res.watchKindDidFullList {
				delete(state.watchKindDirty, nodeID)
			}
		}

		// Update plan state.
		plan.SetState(dag, node.ID, res.state)

		// Surface readyWhen expression errors. Per 001-graph.md: readyWhen
		// errors produce NodeNotReady (not NodeError), so they don't gate
		// dependents. But the user needs to know their expression is broken
		// and won't self-heal — log it and include in nodeErrors for status.
		if res.state == NodeNotReady && res.err != nil && errors.Is(res.err, ErrReadyWhenFailed) {
			walk.nodeErrors = append(walk.nodeErrors, fmt.Sprintf("%s: %s", node.ID, res.err.Error()))
			logger.V(0).Info("readyWhen expression error (not gating dependents)",
				"node", node.ID, "error", res.err)
		}

		if res.state == NodeReady || res.state == NodeNotReady {
			walk.appliedKeys = append(walk.appliedKeys, res.keys...)
		} else {
			// Non-success states that reach here (e.g., NodeNotReady with keys) —
			// retain previous keys since the resource may still exist.
			walk.carryForwardKeys(node.ID)
		}

		// Evaluate propagateWhen (coordinator reads from now-merged scope).
		// For forEach nodes, the worker evaluates propagateWhen per-item and
		// returns the aggregate result. The per-item evaluation is authoritative
		// because the coordinator's scope has the array (not individual items),
		// so per-field expressions wouldn't work against the aggregate.
		if (res.state == NodeReady || res.state == NodeNotReady) &&
			len(node.PropagateWhen) > 0 && eval.scope[node.ID] != nil {
			if res.forEachAllItemsPropagateReady != nil {
				// forEach node: use the worker's per-item aggregate result.
				plan.PropagateReady[node.ID] = *res.forEachAllItemsPropagateReady
			} else {
				// Regular node: evaluate propagateWhen against the coordinator scope.
				plan.PropagateReady[node.ID] = eval.checkPropagateWhen(
					node.PropagateWhen, node.ID)
			}
		}

		// Step 8: Propagation check — hash the specific field paths
		// dependents reference from this node's output, plus propagateWhen
		// state. If the hash differs from the previous reconcile, mark
		// dependents as having a propagation trigger.
		// Per 004-graph-reconciliation.md § Propagation.
		if res.state == NodeReady || res.state == NodeNotReady {
			if observed := eval.scope[node.ID]; observed != nil {
				propagateHash, err := hashSelfPaths(node, observed)
				if err == nil && propagateHash == "" {
					// No SelfPaths (WatchKind, bare reference) —
					// fall back to hashing the full output. Without this,
					// collection changes would never propagate to forEach.
					if m, ok := observed.(map[string]any); ok {
						propagateHash, err = hashDesiredState(m)
					} else {
						// Array output (WatchKind, forEach) — use JSON hash.
						data, jsonErr := json.Marshal(observed)
						if jsonErr == nil {
							h := fnv.New64a()
							h.Write(data)
							propagateHash = fmt.Sprintf("%016x", h.Sum64())
						}
					}
				}
				if err == nil && propagateHash != "" {
					// Per 004-graph-reconciliation.md § Propagation: the
					// propagation hash includes propagateWhen state.
					// When a gate transitions (false→true or true→false),
					// the hash changes and dependents are triggered —
					// even if the output field paths are unchanged.
					if len(node.PropagateWhen) > 0 {
						if plan.PropagateReady[node.ID] {
							propagateHash += ":propagate=true"
						} else {
							propagateHash += ":propagate=false"
						}
					}
					// Include readiness state in the propagation hash so
					// downstream nodes that reference .ready() are
					// triggered when readiness changes, even if output
					// field paths are unchanged. Without this, a node
					// going from NotReady → Ready (same output data)
					// wouldn't trigger downstream re-evaluation of
					// .ready()-dependent expressions until the drift
					// timer fires.
					//
					// This is unconditional — every node's readiness
					// state is included, even if no downstream references
					// .ready(). The cost is re-evaluation (CEL compute),
					// not re-application (the apply-hash gates writes).
					// Scoping to only nodes with .ready()-dependent
					// downstreams would avoid the wasted compute but
					// introduces false-negative risk if .ready()
					// detection is incomplete (e.g., comprehension
					// variables). Correct-by-construction over
					// correct-by-analysis-completeness.
					if res.state == NodeReady {
						propagateHash += ":ready=true"
					} else {
						propagateHash += ":ready=false"
					}
					prevHash := state.previousSelfHashes[node.ID]
					if prevHash == "" || propagateHash != prevHash {
						for _, depIdx := range dag.Dependents[node.ID] {
							walk.propagationTriggered[dag.Nodes[depIdx].ID] = true
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
		state.previousPropagateReady[node.ID] = plan.PropagateReady[node.ID]

		// Store evaluation hash for next reconcile's change check (step 3).
		if evalHash, err := hashNodeInputs(node, eval.scope); err == nil && evalHash != "" {
			state.previousEvalHashes[node.ID] = evalHash
		}

		// Check dependents: dispatch any whose dependencies are now satisfied.
		for _, depIdx := range dag.Dependents[node.ID] {
			walk.tryDispatch(depIdx)
		}
	}

	// Finalize skipped nodes: nodes that were skipped (outputsReady) and never
	// re-dispatched via propagation trigger still have plan.States = Pending.
	// Restore their previous state for the plan summary (status reporting).
	//
	// Fallthrough case: outputsReady without a previousPlanStates entry is
	// structurally impossible today — the skip paths that set outputsReady
	// only fire when the node has prior state — but defending explicitly
	// makes the invariant checkable. A silent Ready (zero-value nodeUnvisited
	// slipping through Summary, which ignores it) would under-report node
	// count and mask latent bugs. Treat "skipped with no prior state" as
	// Pending: we haven't confirmed anything about this node yet.
	walkAttempted = true
	for nodeID := range walk.outputsReady {
		if plan.States[nodeID] == nodeUnvisited {
			if prevState, ok := state.previousPlanStates[nodeID]; ok {
				plan.States[nodeID] = prevState
			} else {
				plan.States[nodeID] = NodePending
				logger.V(1).Info("skipped node with no prior state — marking Pending",
					"node", nodeID)
			}
		}
	}

	// Retain previous keys for uncertain-absence nodes. These nodes were never
	// dispatched to workers, so their keys aren't in appliedKeys yet.
	// Without this, their managed resources would appear as prune candidates.
	// Per 004-graph-reconciliation.md § Prune: "Pending and Blocked both represent
	// uncertain absence — previous applied keys are retained, not safe to prune."
	//
	// Belt-and-suspenders: the prune gate also blocks on these states, but key
	// retention is the surgical fallback if the gate logic ever changes.
	for _, node := range dag.Nodes {
		if plan.States[node.ID] == NodeBlocked || plan.States[node.ID] == NodePending {
			walk.carryForwardKeys(node.ID)
		}
	}

	appliedKeys := walk.appliedKeys
	nodeErrors := walk.nodeErrors
	var nodeNotes []string // informational messages (e.g., FinalizerSkipped) routed to status without gating Ready

	// Derive aggregate state from the DAG plan
	summary := plan.Summary()

	// -----------------------------------------------------------------------
	// Prune resources no longer in the applied set
	// -----------------------------------------------------------------------
	//
	// The applied set is derived from the watch cache — all resources where
	// the Graph's identity label exists in the controller's informer stores.
	// Per 004-graph-reconciliation.md § Prune.
	//
	// Prune candidates = appliedSet - currentKeySet.
	// forEach scale-down, includeWhen toggles, and revision transitions all
	// produce the same diff — one mechanism.
	pruneOK := true
	prunePending := false
	// Per 004-graph-reconciliation.md § Prune: "Uncertain absence (Pending, Blocked,
	// Error, SystemError) blocks pruning — the resource might reappear once the
	// blocker resolves."
	pruneSafe := !summary.HasPending && !summary.HasBlocked && !summary.HasError && !summary.HasSystemError
	if pruneSafe {
		allPreviousKeys := map[string]bool{}
		logger.V(1).Info("prune gate open", "previousAppliedKeys", len(state.previousAppliedKeys), "deferredPruneKeys", len(state.deferredPruneKeys), "superseded", len(supersededRevisions))

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
		// Include keys whose deletion was deferred in the previous reconcile
		// (finalization in progress, third-party field-manager block, etc.).
		// These may not appear in the watch cache or previousAppliedKeys, so
		// without this they'd silently disappear from the prune candidate set.
		for k := range state.deferredPruneKeys {
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
					if key := staticResourceKey(node.Identity(), graph.GetNamespace(), r.Scope); key != "" {
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
			var deferred []string
			var pruneBlockedReasons []string
			var pruneNotes []string
			deferred, pruneBlockedReasons, pruneNotes, err = r.pruneRemovedResources(ctx, graph, allPreviousKeys, appliedKeys, dag, supersededDAGs, eval, watcher)
			// Route structured results:
			//   - blocked reasons become error text and gate Ready (HasBlocked set below)
			//   - notes (FinalizerSkipped) become informational text, Ready stays True
			// Per 004-graph-reconciliation.md § Finalization.
			nodeErrors = append(nodeErrors, pruneBlockedReasons...)
			nodeNotes = append(nodeNotes, pruneNotes...)
			if len(pruneBlockedReasons) > 0 {
				summary.HasBlocked = true
			}
			if len(deferred) > 0 {
				prunePending = true
				// Store deferred keys for the next reconcile to retry.
				state.deferredPruneKeys = make(map[string]bool, len(deferred))
				for _, k := range deferred {
					state.deferredPruneKeys[k] = true
				}
				// Finalization is in progress (finalizer resource exists but
				// readyWhen not yet satisfied). Request a short requeue as a
				// consistency floor — the primary trigger is a watch event on
				// the gate resource, but under load that event may be slow.
				// Per 004-graph-reconciliation.md § Finalization: the controller
				// waits for readyWhen before deleting the target. This floor
				// ensures the gate is re-checked even if the watch event is
				// delayed. Same principle as the NodePending 1s timer,
				// but graph-level (not per-node) so it doesn't touch the
				// drift timer map.
				requeueFloor = finalizationRequeueInterval
			} else {
				state.deferredPruneKeys = nil
			}
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
		compiled:    compilationErr == nil,
		compiledErr: compilationErr,
		nodeCount:   len(revisionSpec.Nodes),
		PlanSummary: summary,
		nodeErrors:  nodeErrors,
		nodeNotes:   nodeNotes,
	}
	if err := r.updateStatus(ctx, graph, rstate); err != nil {
		logger.Error(err, "status update")
	}

	// The graph is fully converged when every node is Ready and the spec is
	// compiled. Everything else — errors, conflicts, pending data, not-ready
	// — retries via watch events, not periodic requeue.
	allReady := rstate.compiled && !rstate.HasPending && !rstate.HasNotReady &&
		!rstate.HasBlocked && !rstate.HasConflict && !rstate.HasError && !rstate.HasSystemError
	r.updateRevisionStatus(ctx, activeRevision, supersededRevisions, allReady, pruneOK && !prunePending)

	// Reset drift timers for nodes that were dispatched to workers.
	// Per 004-graph-reconciliation.md § Reconcile: "An SSA apply resets the
	// drift timer. A skipped write during normal evaluation (hash match
	// from a watch event or propagation trigger) does not — the timer
	// still fires to catch divergence that the hash cannot detect."
	//
	// Only dispatched nodes (which evaluated and potentially applied via
	// SSA) reset their drift timers. Nodes that were skipped — no
	// trigger, evaluation-hash match, propagateWhen gate, or
	// coordinator-resolved states (Excluded, Blocked) — retain their
	// existing timer so the consistency floor is preserved. Without
	// this guard, frequent reconciles (driven by watch events on other
	// nodes) perpetually reset timers for stable nodes, preventing the
	// drift timer from ever firing.
	//
	// Drift-triggered dispatches always write (applySSA bypasses the
	// apply-hash check when driftCorrection=true), so resetting after
	// dispatch is correct. Non-drift dispatches may skip the write if
	// the apply-hash matches — the timer reset is at most one-interval
	// imprecise, bounded by the next drift expiry.
	//
	// Pending and SystemError get short timers regardless of dispatch
	// status — these are retry mechanisms, not drift detection.
	// Pending: fallback for edge cases where no watch event arrives.
	// SystemError: transient server failure needs backoff retry.
	for i, node := range dag.Nodes {
		nodeState := plan.States[node.ID]
		switch nodeState {
		case NodeReady, NodeNotReady:
			if walk.dispatched[i] {
				state.resetDriftTimer(node.ID, r.driftInterval(), r.driftJitter())
			}
			// Reset exponential backoff on any non-SystemError state.
			// Per muse: "Reset on any non-SystemError evaluation, not just
			// success. If a node transitions from SystemError to Error,
			// the backoff should reset because the failure mode changed."
			delete(state.systemErrorBackoff, node.ID)
		case NodePending:
			state.resetDriftTimer(node.ID, 1*time.Second, 0)
			delete(state.systemErrorBackoff, node.ID)
		case NodeSystemError:
			// Per 004-graph-reconciliation.md § Trigger: "Transient errors
			// (5xx) retry with exponential backoff [1s, resyncInterval]."
			// Double the backoff duration on each consecutive SystemError,
			// capped at the drift interval. Initial backoff is 1s.
			backoff := state.systemErrorBackoff[node.ID]
			if backoff == 0 {
				backoff = 1 * time.Second
			} else {
				backoff *= 2
			}
			cap := r.driftInterval()
			if backoff > cap {
				backoff = cap
			}
			state.systemErrorBackoff[node.ID] = backoff
			state.resetDriftTimer(node.ID, backoff, 0)
		case NodeError:
			// Per 004-graph-reconciliation.md § Trigger: "Deterministic
			// errors (4xx) are not retried — same inputs produce the same
			// failure. They resolve via changes or resync." The drift timer
			// is the resync path — the designed recovery mechanism for
			// deterministic errors when no external change arrives.
			state.resetDriftTimer(node.ID, r.driftInterval(), r.driftJitter())
			delete(state.systemErrorBackoff, node.ID)
		}
	}

	// Schedule next reconcile. Watch events handle convergence — no
	// periodic polling. The drift timer is the consistency floor.
	// Per 004-graph-reconciliation.md § Why Not: "Periodic full-graph resync
	// ... Informer resyncs trigger all nodes simultaneously — correlated,
	// expensive. Per-node drift timers with jitter amortize resync."
	//
	// Non-converged nodes (Pending, SystemError) have short drift
	// timers set above, so the earliest expiry reflects urgency.
	//
	// requeueFloor provides an explicit lower bound independent of drift
	// timers — used for graph-level transient conditions (e.g., finalization
	// in progress) that are not associated with any single node.
	requeue := requeueFloor
	if next := state.nextDriftExpiry(); !next.IsZero() {
		if remaining := time.Until(next); remaining > 0 {
			if requeue == 0 || remaining < requeue {
				requeue = remaining
			}
		}
	}
	if requeue > 0 {
		return ctrl.Result{RequeueAfter: requeue}, nil
	}
	return ctrl.Result{}, nil
}

// ---------------------------------------------------------------------------
// Deletion
// ---------------------------------------------------------------------------

func (r *GraphReconciler) reconcileDelete(ctx context.Context, graph *unstructured.Unstructured) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// List revisions early — needed for key extraction and finalization.
	// Cache eviction is deferred to after finalization (which calls
	// compileRevision and would re-add entries if evicted too early).
	revisions, _ := listRevisions(ctx, r.Client, graph.GetName(), graph.GetNamespace())

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
	ownKeys := map[string]bool{}
	contributeKeys := map[string]bool{} // key → hasStatus encoded in the key

	// Build a map from resource keys to hasStatus by scanning revision specs.
	// This recovers the +status suffix that the watch cache cannot provide.
	// Per 003-ownership.md § Status Subresource: "Releases only target the
	// subresources the template actually applied to."
	contributeStatusMap := map[string]bool{} // resource key → hasStatus
	// Also collect all static keys with correct Kind casing for cross-referencing.
	staticKeys := map[string]bool{} // all static resource keys from revision specs
	for _, rev := range revisions {
		spec, err := extractRevisionSpec(rev)
		if err != nil {
			continue
		}
		for _, node := range spec.Nodes {
			if node.Identity() == nil {
				continue
			}
			if key := staticResourceKey(node.Identity(), graph.GetNamespace(), r.Scope); key != "" {
				staticKeys[key] = true
			}
			if templateHasStatus(node.Payload()) {
				if key := staticResourceKey(node.Identity(), graph.GetNamespace(), r.Scope); key != "" {
					contributeStatusMap[key] = true
				}
			}
		}
	}

	// Derive applied set from watch cache if available.
	// Must run BEFORE removeGraph — removeGraph releases watch ownership,
	// which can stop informers if this Graph is the sole watcher of a GVR.
	// deriveAppliedSet needs those informers to scan for identity labels on
	// Contribute targets.
	//
	// deriveAppliedSet keys may have incorrect Kind casing (metadata informers
	// don't always populate TypeMeta, falling back to singularize which doesn't
	// preserve CamelCase). Build a normalizedKey→staticKey map to correct this.
	normalizedToStatic := make(map[string]string, len(staticKeys))
	for sk := range staticKeys {
		normalizedToStatic[strings.ToLower(sk)] = sk
	}
	if r.Watcher != nil {
		appliedSet := r.Watcher.watches.deriveAppliedSet(graph.GetName(), graph.GetNamespace())
		for key, entry := range appliedSet {
			// Correct the key's Kind casing by matching against static keys.
			if corrected, ok := normalizedToStatic[strings.ToLower(key)]; ok {
				key = corrected
			}
			if entry.Reference == ResolvedReferenceContribute {
				// For contribute keys, encode hasStatus from revision spec scan.
				cKey := contributeKeyPrefix + key
				if contributeStatusMap[key] {
					cKey += contributeStatusSuffix
				}
				contributeKeys[cKey] = true
			} else {
				ownKeys[key] = true
			}
		}
	}

	// Release watch state now that contribute keys have been collected.
	if r.Watcher != nil {
		r.Watcher.removeGraph(types.NamespacedName{Name: graph.GetName(), Namespace: graph.GetNamespace()})
	}

	// Also extract static keys from revision specs for coverage.
	for _, rev := range revisions {
		spec, err := extractRevisionSpec(rev)
		if err != nil {
			continue
		}
		for _, node := range spec.Nodes {
			if node.Identity() == nil {
				continue
			}
			// Skip Watch, WatchKind (read-only).
			ref := node.Reference()
			if ref == ReferenceWatch || ref == ReferenceWatchKind {
				continue
			}
			// Skip finalizer nodes — dormant during normal operation.
			if node.Finalizes != "" {
				continue
			}
			if key := staticResourceKey(node.Identity(), graph.GetNamespace(), r.Scope); key != "" {
				ownKeys[key] = true
			}
		}
	}

	// Release Contribute fields first via release apply.
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
		if err := releaseApply(ctx, r.Client, gvk, nn.Namespace, nn.Name, fieldOwner, hasStatus); err != nil {
			logger.Error(err, "releasing contribution fields during teardown", "key", resKey)
		} else {
			logger.V(1).Info("released contribution fields during teardown", "key", resKey)
		}
	}

	// Convert Own keys to slice for ordered deletion.
	var keys []string
	for k := range ownKeys {
		keys = append(keys, k)
	}

	// Also include dynamically-named resources found by label selector.
	// This catches forEach-stamped resources that aren't in the static spec.
	dynamicKeys, _ := r.findManagedResourceKeys(ctx, graph)
	for _, k := range dynamicKeys {
		if !ownKeys[k] {
			keys = append(keys, k)
		}
	}

	// Compile the active revision (if available) to get the DAG for
	// finalizer relationships, deletion ordering, and an evaluator for
	// template rendering. listRevisions returns ascending by generation,
	// so the active revision is the last element. Per
	// 004-graph-reconciliation.md § Teardown: "Ordering comes from the
	// active revision's DAG." Compiled BEFORE deletionOrder so the
	// already-compiled DAG can drive ordering rather than re-parsing the
	// live Graph spec.
	var teardownDAG *DAG
	var teardownEval *evaluator
	var teardownCompileErr error
	if len(revisions) > 0 {
		active := revisions[len(revisions)-1]
		if _, state, compileErr := r.compileRevision(active); compileErr == nil {
			teardownDAG = state.compiled.dag
			teardownEval = newEvaluator(state)
			// During teardown, the effective generation is the active
			// revision's generation — the graph's live generation is
			// irrelevant because we're not applying new state, just
			// stamping any finalizer resources we need to create.
			teardownEval.effectiveGeneration = revisionGeneration(active)
		} else {
			// Per 004-graph-reconciliation.md § Teardown, ordering comes from
			// the active revision's DAG. If compile fails at teardown — a
			// CRD was uninstalled mid-life, a schema change invalidated the
			// revision — surface it so operators know why ordering fell back
			// to the live Graph spec, and why finalizer templates that
			// depend on evaluated scope may not run.
			teardownCompileErr = compileErr
			logger.Error(compileErr, "active revision failed to compile during teardown; falling back to live Graph spec",
				"revision", active.GetName())
		}
	}

	// Pass 1: Issue deletes in reverse topological order.
	// Track which keys we actually attempted to delete (had our hash).
	deletedKeys := map[string]bool{}
	deleteOrder, err := r.deletionOrder(graph, keys, teardownDAG)
	if err != nil {
		// Per the design (004-graph-reconciliation): teardown is blocked until
		// ordering is available — never degrade to unordered deletion.
		logger.Error(err, "cannot determine deletion order, requeueing")
		return ctrl.Result{RequeueAfter: systemErrorRequeueInterval}, nil
	}

	// Build resource-key-to-node-ID map for finalizer lookup during teardown.
	keyToNodeID := map[string]string{}
	finalizerNodeKeys := map[string]bool{} // keys of finalizer nodes — skip from regular deletion
	if teardownDAG != nil {
		for _, node := range teardownDAG.Nodes {
			if node.Identity() != nil {
				if rk := staticResourceKey(node.Identity(), graph.GetNamespace(), r.Scope); rk != "" {
					keyToNodeID[rk] = node.ID
					if node.Finalizes != "" {
						finalizerNodeKeys[rk] = true
					}
				}
			}
		}
	}

	// Track structured teardown-blocked reasons per-resource so the Graph
	// status can distinguish:
	//   - third-party field managers still writing the resource
	//   - finalizer creation failed (can't build or apply the finalizer resource)
	//   - finalizer created but never reaches readyWhen
	// Per 004-graph-reconciliation.md § Finalization, these three causes have
	// different remediation actions; collapsing them into one message sends
	// operators chasing the wrong problem.
	var teardownBlockedReasons []string
	// teardownNotes accumulates informational notes (e.g., FinalizerSkipped)
	// that don't block teardown but are operationally useful. Per
	// 004-graph-reconciliation.md § Finalization: "The Graph's status surfaces
	// this: FinalizerSkipped with a message naming the resource." The prune
	// path already surfaces these via pruneNotes; teardown gets the same
	// treatment so the signal is consistent across both deletion paths.
	var teardownNotes []string
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

		// Check if we successfully owned this resource (has our hash annotation).
		// Target absent is not a teardown block — the design classifies it as
		// FinalizerSkipped when a finalizer was declared, or a silent no-op
		// otherwise. Emit the note so operators can tell finalization was
		// bypassed vs never needed.
		if err := r.Client.Get(ctx, nn, obj); err != nil {
			if teardownDAG != nil {
				if nodeID := keyToNodeID[key]; nodeID != "" {
					if finalizerNodeIDs, ok := teardownDAG.Finalizers[nodeID]; ok && len(finalizerNodeIDs) > 0 {
						logger.Info("teardown finalization skipped: target resource does not exist",
							"key", key, "finalizers", finalizerNodeIDs)
						teardownNotes = append(teardownNotes,
							fmt.Sprintf("FinalizerSkipped: %s (target absent)", key))
					}
				}
			}
			continue // already gone
		}
		objAnnotations := obj.GetAnnotations()
		if objAnnotations == nil || objAnnotations[applyHashAnnotation] == "" {
			logger.V(1).Info("skipping delete for resource without apply hash (never successfully applied)", "key", key)
			continue
		}

		// Contributor-aware deletion: check managedFields for other field
		// managers before deleting. If present, deletion is blocked — the
		// finalizer holds until the other manager releases.
		ownManager := string(graphFieldOwner(graph))
		if blockers := thirdPartyFieldManagers(obj, ownManager); len(blockers) > 0 {
			logger.Info("teardown blocked: resource has other field managers",
				"key", key, "blockers", blockers)
			teardownBlockedReasons = append(teardownBlockedReasons,
				fmt.Sprintf("TeardownBlocked: %s (third-party field managers: %s)",
					key, strings.Join(blockers, ", ")))
			continue // skip delete — finalizer holds
		}

		// Finalization: if this target has finalizer nodes, run the
		// finalization sequence before deleting.
		var finKeys []string
		if teardownDAG != nil && teardownEval != nil {
			nodeID := keyToNodeID[key]
			if finalizerNodeIDs, ok := teardownDAG.Finalizers[nodeID]; ok && len(finalizerNodeIDs) > 0 {
				ready, fk, finErr := r.runFinalization(ctx, graph, obj, nodeID, finalizerNodeIDs, teardownDAG, teardownEval, nil)
				finKeys = fk
				if finErr != nil {
					logger.Error(finErr, "teardown finalization failed", "key", key)
					teardownBlockedReasons = append(teardownBlockedReasons,
						fmt.Sprintf("TeardownBlocked: %s (finalizer creation failed: %s)", key, finErr))
					continue // TeardownBlocked — can't create/check finalizer
				}
				if !ready {
					logger.Info("teardown finalization in progress — deletion deferred",
						"key", key, "finalizers", finalizerNodeIDs)
					teardownBlockedReasons = append(teardownBlockedReasons,
						fmt.Sprintf("TeardownBlocked: %s (finalizer not ready: %s)",
							key, strings.Join(finalizerNodeIDs, ", ")))
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
					// First, clean up static-name finalizer resources.
					for _, finNodeID := range finalizerNodeIDs {
						if finIdx, ok2 := teardownDAG.Index[finNodeID]; ok2 {
							finNode := &teardownDAG.Nodes[finIdx]
							if finNode.Identity() != nil && finNode.ForEach == nil {
								if fk := staticResourceKey(finNode.Identity(), graph.GetNamespace(), r.Scope); fk != "" {
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
					// Second, clean up forEach finalizer children via their tracked keys.
					for _, fk := range finKeys {
						fGVK, fNN := parseResourceKey(fk)
						if fGVK.Kind == "" {
							continue
						}
						finDel := &unstructured.Unstructured{}
						finDel.SetGroupVersionKind(fGVK)
						finDel.SetName(fNN.Name)
						finDel.SetNamespace(fNN.Namespace)
						if delErr := r.Client.Delete(ctx, finDel); delErr != nil {
							logger.V(1).Info("forEach finalizer child cleanup", "key", fk, "error", delErr)
						} else {
							logger.V(1).Info("cleaned up forEach finalizer child", "key", fk)
						}
					}
				}
			}
		}
	}

	// Pass 2: Verify managed resources that we actually deleted are gone.
	// Only check resources that had our apply hash — others (e.g., conflicted
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

	// If any resource deletion was blocked, surface each distinct reason so
	// operators can triage. Per 004-graph-reconciliation.md § Teardown:
	// TeardownBlocked is not a skip — the target has data the user intended
	// to finalize. A single "teardown blocked" message collapses three
	// distinct causes (third-party field managers, finalizer creation
	// failure, finalizer not ready); the per-reason messages make the
	// remediation path obvious from status.
	//
	// FinalizerSkipped notes are surfaced alongside blocked reasons when
	// teardown is otherwise blocked; when teardown completes cleanly,
	// skipped notes only appear if the Graph is about to be removed, so
	// we log-and-drop them (status is about to vanish).
	if len(teardownBlockedReasons) > 0 {
		logger.Info("teardown blocked", "reasons", teardownBlockedReasons)
		nodeErrors := append([]string{}, teardownBlockedReasons...)
		if teardownCompileErr != nil {
			// A compile failure during teardown degrades finalizer-aware
			// ordering and prevents finalizer expressions from evaluating.
			// Surface alongside the blocked reasons so the operator sees
			// both symptoms of the same underlying cause.
			nodeErrors = append(nodeErrors,
				fmt.Sprintf("active revision compile failed: %s", teardownCompileErr))
		}
		if statusErr := r.updateStatus(ctx, graph, &reconcileState{
			compiled:    true,
			PlanSummary: PlanSummary{HasBlocked: true},
			nodeErrors:  nodeErrors,
			nodeNotes:   teardownNotes, // FinalizerSkipped — informational
		}); statusErr != nil {
			logger.Error(statusErr, "updating status during teardown")
		}
		return ctrl.Result{RequeueAfter: systemErrorRequeueInterval}, nil
	}
	// FinalizerSkipped during teardown: log so the event is visible even if
	// status vanishes before the next reconcile picks it up.
	for _, note := range teardownNotes {
		logger.Info("teardown note", "note", note)
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

	// Clean up expression caches AFTER finalization and revision deletion.
	// This must happen after compileRevision calls in the teardown phase
	// (line ~1219), which re-populate the cache to get the DAG and evaluator
	// for finalization. Evicting earlier would be immediately undone.
	for _, rev := range revisions {
		r.Caches.remove(rev.GetNamespace() + "/" + rev.GetName())
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
			id := node.Identity()
			if id == nil {
				continue
			}
			apiVersion, _ := id["apiVersion"].(string)
			kind, _ := id["kind"].(string)
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
//
// driftInterval overrides the per-node drift timer interval. 0 uses the default (30m).
func SetupWithManager(mgr ctrl.Manager, restConfig *rest.Config, maxWorkers int, driftInterval time.Duration) (shutdown func(), caches *graphCaches, err error) {
	RegisterMetrics(crmetrics.Registry)

	if maxWorkers <= 0 {
		maxWorkers = DefaultMaxConcurrentReconciles
	}

	metadataClient, err := metadata.NewForConfig(restConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("creating metadata client: %w", err)
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

	// Create schema resolver for compile-time type checking.
	// Core types resolve from compiled-in definitions, CRDs resolve via
	// cached discovery client.
	schemaResolver, err := schemaresolver.NewCombinedResolver(restConfig, nil)
	if err != nil {
		// Schema resolution is an operational dependency — log the failure.
		// All resource nodes will fall back to dyn (no field-level type checking).
		log.Log.Error(err, "failed to create schema resolver; compile-time type checking disabled for resource nodes")
		schemaResolver = nil
	}

	reconciler := &GraphReconciler{
		Client:         mgr.GetClient(),
		SchemaResolver: schemaResolver,
		Watcher:        coordinator,
		Caches:         newGraphCaches(),
		Resources:      newResourceCache(),
		DriftInterval:  driftInterval,
		// Scope is used by staticResourceKey to avoid namespacing
		// cluster-scoped resource keys. Without this, prune/teardown
		// silently miss cluster-scoped resources because their keys
		// never match post-apply resourceKey(). Per 003-ownership.md
		// § Priority Resolution.
		Scope: newRESTMapperGVKScopeResolver(mgr.GetRESTMapper()),
	}

	// When the watch infrastructure observes a new type (first informer for a
	// GVR), check if any compiled graph had that specific GVR unresolved. If
	// so, evict and recompile — the schema may now be available. Handles
	// CRDs, aggregated APIs, and any other mechanism that makes a new type
	// watchable. Filtering by GVR prevents thundering-herd recompilation when
	// only one type becomes available.
	watchMgr.onNewType = func(gvr schema.GroupVersionResource) {
		affected := reconciler.Caches.evictUnresolved(gvr)
		if len(affected) == 0 {
			return
		}
		log.Log.Info("new type observed; recompiling affected graphs",
			"gvr", gvr, "affectedGraphs", len(affected))
		for _, gk := range affected {
			obj := &unstructured.Unstructured{}
			obj.SetName(gk.Name)
			obj.SetNamespace(gk.Namespace)
			select {
			case watchChan <- event.GenericEvent{Object: obj}:
			default:
			}
		}
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
		return nil, nil, fmt.Errorf("building controller: %w", err)
	}

	return watchMgr.shutdown, reconciler.Caches, nil
}
