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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apiserver/pkg/cel/openapi/resolver"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/metadata/metadatainformer"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"

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

// DefaultResyncInterval is the per-node consistency floor interval.
// Per 005-reconciliation.md § Reconcile: "Each node has an in-memory
// resync timer with a jittered interval (default 30 minutes)."
// On expiry, the node bypasses the apply-hash check and applies
// unconditionally.
var DefaultResyncInterval = 30 * time.Minute

// MaxResyncJitter is the maximum random jitter added to the resync interval.
// Decorrelates timers across nodes to avoid correlated bursts.
var MaxResyncJitter = 5 * time.Minute

const (
	finalizer = "experimental.kro.run/graph-controller"

	// systemErrorRequeueInterval is the retry interval for Graphs with
	// nodes in SystemError state. Per design: "backoff retry with a low
	// cap, then wait for resync timer."
	systemErrorRequeueInterval = 5 * time.Second

	// finalizationRequeueInterval is the consistency floor for finalization
	// in progress. The primary trigger is a watch event on the gate
	// resource (when readyWhen's dependencies change). This timer ensures
	// the gate is re-checked even if that watch event is slow.
	// Per 005-reconciliation.md § Finalization: "wait for readyWhen before
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
	APIReader      client.Reader           // direct API server reader — bypasses cache for managed resources
	SchemaResolver resolver.SchemaResolver // nil = all resource nodes fall back to dyn
	SchemaGen      *SchemaGeneration       // nil = no generation tracking; never triggers recompilation
	Watcher        *WatchCoordinator       // nil = no dynamic watches (backward compat with existing tests)
	Caches         *graphCaches            // per-revision compiled expression caches
	Resources      *resourceCache          // per-resource full object cache
	ResyncInterval  time.Duration           // per-node resync timer interval; 0 = use DefaultResyncInterval
	ResyncJitter    time.Duration           // max resync jitter; 0 = use MaxResyncJitter
	Scope          GVKScopeResolver        // nil = unknown scope; staticResourceKey falls back to namespace-substitution heuristic
}

// apiReader returns the direct API server reader for managed resource reads.
// Falls back to Client when APIReader is nil (unit tests that don't set up
// a full manager).
func (r *GraphReconciler) apiReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
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
	dag     *DAG
	eval    *evaluator
	state   *instanceState
	plan    *PlanState
	watcher *graphWatcher

	// Trigger maps
	triggered            map[string]bool
	resyncTriggered       map[string]bool
	propagationTriggered map[string]bool

	// Watch incremental cache — drained once at walk start.
	// Per 005-reconciliation.md § Propagation.
	collectionChanges map[string][]CollectionChange // nodeID → changes

	// Walk-local tracking
	dispatched        map[int]bool
	outputsReady      map[string]bool
	appliedKeys       []string
	nodeErrors        []string // "nodeID: reason" for status reporting
	results           chan nodeResult
	inflight          int
	dynamicGVKChanged bool // set when a dynamic GVK resolves for the first time or changes
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
func (w *walkState) resolveForEachChangedItems(node *Node) map[string]bool {
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

func (w *walkState) populateDepsMap(node *Node) {
	depsMap, _ := w.eval.scope[reservedDepsMapVar].(map[string]any)
	if depsMap == nil {
		depsMap = make(map[string]any, len(w.dag.Nodes))
		w.eval.scope[reservedDepsMapVar] = depsMap
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
func (w *walkState) skipNode(node *Node) {
	if prev, ok := w.state.previousScope[node.ID]; ok {
		w.eval.scope[node.ID] = prev
	}
	w.carryForwardKeys(node.ID)
	if w.watcher != nil {
		w.watcher.retainWatches(node.ID)
	}
	w.outputsReady[node.ID] = true
	for _, depIdx := range w.dag.Dependents[node.ID] {
		w.tryDispatch(depIdx)
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
		// Nodes with propagateWhen can handle excluded dependencies —
		// the gate expression decides whether to proceed despite
		// exclusion. Example: rgdStatus has propagateWhen:
		// ${!validation.result.valid || crd.ready()}. When crd is
		// excluded (invalid spec), the gate short-circuits to true
		// and rgdStatus fires to write the error status. Without
		// this check, Excluded propagates unconditionally and the
		// status patch never happens.
		if len(node.PropagateWhen) == 0 || node.ForEach != nil {
			w.plan.SetState(w.dag, node.ID, NodeExcluded)
			w.state.previousPlanStates[node.ID] = NodeExcluded
			delete(w.state.previousEvalHashes, node.ID)
			w.notifyDependents(node.ID)
			return
		}
		// Fall through to propagateWhen evaluation.
	}
	if hasBlocked {
		w.plan.SetState(w.dag, node.ID, NodeBlocked)
		w.state.previousPlanStates[node.ID] = NodeBlocked
		delete(w.state.previousEvalHashes, node.ID)
		w.notifyDependents(node.ID)
		return
	}
	if hasPending {
		w.plan.SetState(w.dag, node.ID, NodePending)
		w.state.previousPlanStates[node.ID] = NodePending
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
				w.plan.SetState(w.dag, node.ID, NodeExcluded)
				w.state.previousPlanStates[node.ID] = NodeExcluded
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
				w.plan.States[node.ID] = NodePending
			}
			for _, depIdx := range w.dag.Dependents[node.ID] {
				w.tryDispatch(depIdx)
			}
			return
		}
	}

	// Step 4: Evaluation check — section-scoped evaluation hashing.
	declaredNodeType := node.Type()
	canHashSkip := declaredNodeType != NodeTypeRef && declaredNodeType != NodeTypeWatch && !w.resyncTriggered[node.ID]
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
				if err := w.eval.evalReadiness(node.ID, node.ReadyWhen); err != nil {
					nodeState = NodeNotReady
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
					propagateHash, hashErr := hashSelfPaths(node, observed)
					if hashErr == nil && propagateHash == "" {
						if m, ok := observed.(map[string]any); ok {
							propagateHash, hashErr = hashDesiredState(m)
						} else {
							data, jsonErr := json.Marshal(observed)
							if jsonErr == nil {
								h := fnv.New64a()
								h.Write(data)
								propagateHash = fmt.Sprintf("%016x", h.Sum64())
							}
						}
					}
					if hashErr == nil && propagateHash != "" {
						if nodeState == NodeReady {
							propagateHash += ":ready=true"
						} else {
							propagateHash += ":ready=false"
						}
						prevHash := w.state.previousSelfHashes[node.ID]
						if prevHash == "" || propagateHash != prevHash {
							for _, depIdx := range w.dag.Dependents[node.ID] {
								w.propagationTriggered[w.dag.Nodes[depIdx].ID] = true
							}
						}
						w.state.previousSelfHashes[node.ID] = propagateHash
					} else {
						// Hash failed — unconditional propagation for safety.
						for _, depIdx := range w.dag.Dependents[node.ID] {
							w.propagationTriggered[w.dag.Nodes[depIdx].ID] = true
						}
					}
				}
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
				w.state.previousPlanStates[node.ID] = NodePending
			} else {
				w.plan.SetState(w.dag, node.ID, NodeError)
				w.state.previousPlanStates[node.ID] = NodeError
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
			// Excluded is definitive absence — the node's previous resources
			// are a legitimate prune candidate, so keys are NOT carried
			// forward. Per 005-reconciliation.md § Prune: "Excluded
			// propagates as Excluded (definitive absence — safe to prune)."
			w.plan.SetState(w.dag, node.ID, NodeExcluded)
			w.state.previousPlanStates[node.ID] = NodeExcluded
			delete(w.state.previousEvalHashes, node.ID)
			w.notifyDependents(node.ID)
			return
		}
	}

	// Build snapshot evaluator for the worker.
	workerEval := w.eval.snapshotFor(node, w.state)

	// Watch incremental cache: pass cached list and collection changes
	// to the worker so reconcileWatch can GET only changed items.
	if node.Type() == NodeTypeWatch {
		workerEval.collectionNodeID = node.ID
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
			workerEval.collectionCachedList = cached
			workerEval.collectionChanges = w.collectionChanges[node.ID]
		} else {
			// First reconcile, resync, or previous incremental error:
			// force full list.
			workerEval.collectionResyncOrFull = true
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
		workerEval.forEachChangedItems = w.resolveForEachChangedItems(node)
	}

	// Dispatch to worker goroutine.
	w.dispatched[idx] = true
	isResync := w.resyncTriggered[node.ID]
	go func(n Node, we *evaluator, nodeType NodeType, resyncCorrection bool) {
		evalStart := time.Now()
		keys, err := w.r.reconcileNode(w.ctx, w.graph, n, nodeType, we, w.watcher, resyncCorrection)
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
		if we.dynamicGVKResolved != nil {
			if gvk, ok := we.dynamicGVKResolved[n.ID]; ok {
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
			forEachUpdates:        we.forEachNewKeys,
			forEachScopes:         we.forEachNewScope,
			forEachHashes:         we.forEachNewHashes,
			forEachItems:          we.forEachNewItems,
			collectionCacheUpdate: we.collectionCacheUpdate(),
			collectionDidFullList: we.collectionDidFullList,
			nodeReadyUpdate:       nodeReadyUpdate,
			resolvedGVK:           resolvedGVK,
		}
	}(*node, workerEval, resolvedNodeType, isResync)
	w.inflight++
}

// resyncInterval returns the effective resync interval for this reconciler.
func (r *GraphReconciler) resyncInterval() time.Duration {
	if r.ResyncInterval > 0 {
		return r.ResyncInterval
	}
	return DefaultResyncInterval
}

// resyncJitter returns the effective resync jitter for this reconciler.
func (r *GraphReconciler) resyncJitter() time.Duration {
	if r.ResyncInterval > 0 {
		// When interval is overridden, use overridden jitter (even if 0).
		return r.ResyncJitter
	}
	return MaxResyncJitter
}

func (r *GraphReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, reconcileErr error) {
	reconcileStart := time.Now()
	logger := log.FromContext(ctx)
	// requeueFloor is an explicit requeue interval independent of resync timers.
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

	// If any owner is being deleted (has deletionTimestamp), self-delete
	// to initiate teardown. This participates in the standard K8s
	// finalizer contract: the owner is held in Terminating by a patch
	// node's finalizer (placed during normal reconciliation), and this
	// detection bridges the gap since K8s GC can't cascade while the
	// owner is still in etcd.
	if r.ownerDeleting(ctx, graph) {
		if err := r.Client.Delete(ctx, graph); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("self-deleting graph for owner teardown: %w", err)
		}
		return ctrl.Result{}, nil
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
			// Commit watches whenever the walk ran, regardless of
			// reconcileErr. A post-walk error (e.g., optimistic lock
			// conflict on status update) should not discard valid watch
			// registrations from the walk. Without this, all events
			// between the failed commit and the next successful commit
			// are silently dropped — the root cause of timer-dependent
			// convergence for CRD Establishment and other status
			// propagation patterns.
			watcher.done(walkAttempted)
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
		// the current generation. Per 005-reconciliation.md § Compilation:
		// "Reconciliation continues on the previous revision if one exists."
		// Fall back to the most recent existing revision so healthy resources
		// keep converging. A typo in the spec should not halt management.
		graphName := graph.GetName()
		namespace := graph.GetNamespace()

		revisions, listErr := listRevisions(ctx, r.Client, graphName, namespace)
		if listErr != nil || len(revisions) == 0 {
			// No previous revision to fall back to — truly stuck.
			if statusErr := r.updateStatus(ctx, graph, &reconcileState{compiled: false, compiledErr: err}); statusErr != nil {
				// Status write failed (transient) — return the status
				// error so controller-runtime retries the write.
				return ctrl.Result{}, fmt.Errorf("updating status after revision error: %w", statusErr)
			}
			// Transient API/network errors (server 5xx, connection
			// refused) justify retry — the operation may succeed on the
			// next attempt. Return error so controller-runtime retries
			// with backoff.
			if isTransientError(err) {
				return ctrl.Result{}, fmt.Errorf("ensuring revision: %w", err)
			}
			// Deterministic business logic failure (invalid CEL, cycle,
			// parse error). Same input always produces the same failure.
			// Status has been written; the next reconcile is triggered by
			// a watch event when the spec changes. Returning error here
			// would cause exponential backoff, delaying status
			// propagation to parent graphs that read this graph's
			// conditions.
			logger.Info("deterministic compilation error; status written, awaiting spec change", "error", err)
			return ctrl.Result{}, nil
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
	effectiveGeneration := pickEffectiveGeneration(graph, activeRevision, compilationErr)

	// -----------------------------------------------------------------------
	// Phase 2: Node reconciliation from the active revision
	// -----------------------------------------------------------------------

	// Parse and compile the active revision's spec (cached by revision name).
	revisionSpec, state, err := r.compileRevision(ctx, graph.GetNamespace(), activeRevision)
	if err != nil {
		if statusErr := r.updateStatus(ctx, graph, &reconcileState{compiled: false, compiledErr: err}); statusErr != nil {
			return ctrl.Result{}, fmt.Errorf("updating status after compilation error: %w", statusErr)
		}
		if isTransientError(err) {
			return ctrl.Result{}, fmt.Errorf("compiling revision: %w", err)
		}
		logger.Info("deterministic compilation error; status written, awaiting spec change", "error", err)
		return ctrl.Result{}, nil
	}

	eval := newEvaluator(state)
	eval.effectiveGeneration = effectiveGeneration
	dag := state.dag
	plan := NewPlanState(dag)

	// Determine which nodes are triggered this reconcile.
	// Per 005-reconciliation.md § Reconcile: nodes evaluate on external
	// triggers or propagation triggers. Otherwise O(1) skip.
	triggered := make(map[string]bool, len(dag.Nodes))
	// resyncTriggered tracks nodes triggered specifically by the resync timer.
	// Per 005-reconciliation.md § Reconcile: "The resync timer bypasses the
	// template-hash check — apply unconditionally." Drift-triggered nodes
	// skip the step 3 evaluation hash check AND force the SSA Patch in step 5,
	// because the question is "does live state match desired state?" not
	// "did inputs change?" — different questions with different cache semantics.
	resyncTriggered := make(map[string]bool, len(dag.Nodes))
	var collectionChanges map[string][]CollectionChange // Watch incremental cache
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
		// Per 005-reconciliation.md § Revision transition: "Nodes that
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
							if v, ok := oldState.previousEvalHashes[node.ID]; ok {
								state.previousEvalHashes[node.ID] = v
							}
							if v, ok := oldState.previousSelfHashes[node.ID]; ok {
								state.previousSelfHashes[node.ID] = v
							}
							if v, ok := oldState.previousKeys[node.ID]; ok {
								state.previousKeys[node.ID] = v
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
		// Watch collection changes: buffered resource keys for
		// incremental cache updates. Drained alongside triggers so the
		// coordinator knows which specific items changed.
		collectionChanges = watcher.drainCollectionChanges()
		// Drift timer triggers: nodes whose consistency timer expired.
		// Per 005-reconciliation.md § Reconcile: "Each node has an
		// in-memory resync timer with a jittered interval (default 30
		// minutes). On expiry, the node runs the full pipeline (steps
		// 1-7). The resync timer bypasses the template-hash check —
		// apply unconditionally."
		for _, node := range dag.Nodes {
			if state.isResyncExpired(node.ID) {
				triggered[node.ID] = true
				resyncTriggered[node.ID] = true
				ResyncTimerFiresTotal.With(graphMetricLabels(
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
	// reconcile at the earliest resync timer expiry.
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
		if compilationErr != nil {
			rstate := &reconcileState{
				compiled:    false,
				compiledErr: compilationErr,
			}
			if err := r.updateStatus(ctx, graph, rstate); err != nil {
				logger.Error(err, "status update (compilation error, no triggers)")
			}
		}
		if next := state.nextResyncExpiry(); !next.IsZero() {
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
	//
	// Record the schema generation before the walk. If a CRD is created
	// during node reconciliation (e.g., the `crd` template node), the
	// generation advances. After the walk, we re-validate compilation to
	// catch child graph type errors immediately — without waiting for a
	// second reconcile cycle.
	var preWalkGen int64
	if r.SchemaGen != nil {
		preWalkGen = r.SchemaGen.Generation()
	}
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
		resyncTriggered:       resyncTriggered,
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

		// Watch incremental-cache integrity: if the worker errored
		// AND did not persist a cache update, the drained
		// CollectionChanges are lost. Mark the node dirty so the next
		// reconcile takes the full-list path to recover authoritative
		// state from the API server. Without this, stale cache can
		// persist for up to the resync interval (default 30m). Per
		// 005-reconciliation.md § Propagation.
		if res.err != nil && node.Type() == NodeTypeWatch &&
			len(res.collectionCacheUpdate) == 0 {
			state.collectionDirty[node.ID] = true
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
			// Under declared-keyword classification, patch→template is an
			// authoring event (patch: → template: spec edit) handled by
			// revision supersession. No runtime reclassification reset.
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

		if r.SchemaGen != nil && node.Type() == NodeTypeTemplate {
			if scopeMap, ok := res.scopeValue.(map[string]any); ok {
				if scopeMap["apiVersion"] == "apiextensions.k8s.io/v1" && scopeMap["kind"] == "CustomResourceDefinition" {
					r.SchemaGen.AdvanceGeneration()
				}
			}
		}

		// Merge node-readiness verdict. Watch nodes carry an explicit
		// update from the worker; all other nodes derive readiness from
		// the result state.
		if eval.nodeReady != nil {
			if res.nodeReadyUpdate != nil {
				eval.nodeReady[res.scopeKey] = *res.nodeReadyUpdate
			} else {
				eval.nodeReady[res.scopeKey] = (res.state == NodeReady)
			}
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
		for nodeID, itemHashes := range res.forEachHashes {
			state.forEachItemHashes[nodeID] = itemHashes
		}

		// Merge Watch cache updates into the shared instance state.
		// Dirty is cleared only when the worker took the full-List path
		// — that's the only thing that recovers from a lost incremental
		// merge. An incremental success against an already-stale cache
		// does not address the staleness. Per 005-reconciliation.md
		// § Propagation.
		for nodeID, cached := range res.collectionCacheUpdate {
			state.collectionCache[nodeID] = cached
			if res.collectionDidFullList {
				delete(state.collectionDirty, nodeID)
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

		// Step 8: Propagation check — hash the specific field paths
		// dependents reference from this node's output, plus propagateWhen
		// state. If the hash differs from the previous reconcile, mark
		// dependents as having a propagation trigger.
		// Per 005-reconciliation.md § Propagation.
		if res.state == NodeReady || res.state == NodeNotReady {
			if observed := eval.scope[node.ID]; observed != nil {
				propagateHash, err := hashSelfPaths(node, observed)
				if err == nil && propagateHash == "" {
					// No SelfPaths (Watch, bare reference) —
					// fall back to hashing the full output. Without this,
					// collection changes would never propagate to forEach.
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
				if err == nil && propagateHash != "" {
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

		// Store evaluation hash for next reconcile's change check (step 3).
		if evalHash, err := hashNodeInputs(node, eval.scope); err == nil && evalHash != "" {
			state.previousEvalHashes[node.ID] = evalHash
		}

		// Record dynamic GVK resolutions. When a dynamic GVK node resolves
		// for the first time or changes, the compilation key will differ on
		// the next reconcile (includes resolved GVKs as hints). Mark for
		// requeue so the next reconcile compiles with the schema available.
		if res.resolvedGVK != nil {
			if state.mergeDynamicGVK(node.ID, *res.resolvedGVK) {
				walk.dynamicGVKChanged = true
				logger.Info("dynamic GVK resolved; will recompile with schema on next reconcile",
					"node", node.ID, "gvk", *res.resolvedGVK)
			}
		}

		// Check dependents: dispatch any whose dependencies are now satisfied.
		for _, depIdx := range dag.Dependents[node.ID] {
			walk.tryDispatch(depIdx)
		}
	}

	// Finalize skipped nodes: nodes that were skipped (outputsReady) and never
	// re-dispatched via propagation trigger still have plan.States = Pending.
	// Restore their previous state for the plan summary (status reporting).
	walkAttempted = true
	finalizeSkippedStates(plan, walk.outputsReady, state.previousPlanStates, func(nodeID string) {
		logger.V(1).Info("skipped node with no prior state — marking Pending", "node", nodeID)
	})

	// Retain previous keys for uncertain-absence nodes. These nodes were never
	// dispatched to workers, so their keys aren't in appliedKeys yet.
	// Without this, their managed resources would appear as prune candidates.
	// Per 005-reconciliation.md § Prune: "Pending and Blocked both represent
	// uncertain absence — previous applied keys are retained, not safe to prune."
	//
	// Belt-and-suspenders: the prune gate also blocks on these states, but key
	// retention is the surgical fallback if the gate logic ever changes.
	for _, node := range dag.Nodes {
		if plan.States[node.ID] == NodeBlocked || plan.States[node.ID] == NodePending {
			walk.carryForwardKeys(node.ID)
		}
	}

	// Retain watches for all DAG nodes not already retained via skipNode
	// or worker dispatch. Without this, the double-buffer swap in doneGraph
	// removes watch index entries for nodes that weren't visited this cycle
	// (e.g., healthy nodes not triggered). Once removed, watch events for
	// those resources are silently dropped until the resync timer fires.
	// This was the structural cause of timer-dependent convergence: partial
	// reconciles (only some nodes triggered) would strip watches from
	// untriggered-but-healthy nodes, making their convergence entirely
	// timer-driven rather than watch-driven.
	if watcher != nil {
		for i, node := range dag.Nodes {
			if !walk.dispatched[i] && !walk.outputsReady[node.ID] {
				watcher.retainWatches(node.ID)
			}
		}
	}

	appliedKeys := walk.appliedKeys
	nodeErrors := walk.nodeErrors
	var nodeNotes []string // informational messages (e.g., FinalizerSkipped) routed to status without gating Ready

	// If the schema generation advanced during the walk (a CRD was created
	// by a template node), re-validate compilation immediately. The schema
	// resolver can now resolve types that were dyn at the start of this
	// reconcile. This catches child graph type errors (e.g., forEach over a
	// non-list field) within the same cycle that creates the CRD, rather
	// than waiting for the next reconcile to detect staleness.
	if r.SchemaGen != nil && r.SchemaGen.Generation() > preWalkGen && compilationErr == nil {
		if _, _, err := r.compileRevision(ctx, graph.GetNamespace(), activeRevision); err != nil {
			compilationErr = err
			logger.Error(err, "post-walk recompilation detected error")
		}
	}

	// Derive aggregate state from the DAG plan
	summary := plan.Summary()

	// Update node state gauge metrics for operator visibility.
	updateNodeStateMetrics(graph.GetName(), graph.GetNamespace(), plan, dag)

	// -----------------------------------------------------------------------
	// Prune resources no longer in the applied set
	// -----------------------------------------------------------------------
	//
	// The applied set is derived from the watch cache — all resources where
	// the Graph's identity label exists in the controller's informer stores.
	// Per 005-reconciliation.md § Prune.
	//
	// Prune candidates = appliedSet - currentKeySet.
	// forEach scale-down, includeWhen toggles, and revision transitions all
	// produce the same diff — one mechanism.
	pruneOK := true
	prunePending := false
	// Per 005-reconciliation.md § Prune: "Uncertain absence (Pending, Blocked,
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
			if _, revState, compileErr := r.compileRevision(ctx, graph.GetNamespace(), rev); compileErr == nil {
				supersededDAGs[rev.GetName()] = revState.dag
			}
		}

		if len(allPreviousKeys) > 0 {
			var deferred []string
			var pruneBlockedReasons []string
			var pruneNotes []string

			// Phase 1: Advance finalization state machines.
			// Produces completedTargets (safe to delete) and protectedKeys
			// (must not prune). Runs BEFORE the prune walk's deletion loop.
			pruneCandidates := collectPruneCandidates(allPreviousKeys, appliedKeys)
			keyToNodeID, nodeIDToKey := buildKeyMaps(dag, supersededDAGs, graph.GetNamespace(), r.Scope)
			finResult, finErr := r.advanceFinalization(ctx, graph, pruneCandidates, keyToNodeID, nodeIDToKey, collectAllDAGs(dag, supersededDAGs), eval, watcher, state)
			if finErr != nil {
				err = finErr
			} else {
				// Merge finalization results.
				pruneBlockedReasons = append(pruneBlockedReasons, finResult.BlockedReasons...)
				pruneNotes = append(pruneNotes, finResult.Notes...)
				deferred = append(deferred, finResult.DeferredTargets...)

				// Phase 2: Pure deletion decisions (no finalization logic).
				var pruneDeferred []string
				var pruneBR []string
				var pruneN []string
				pruneDeferred, pruneBR, pruneN, err = r.pruneRemovedResources(ctx, graph, allPreviousKeys, appliedKeys, dag, supersededDAGs, eval, watcher, finResult)
				deferred = append(deferred, pruneDeferred...)
				pruneBlockedReasons = append(pruneBlockedReasons, pruneBR...)
				pruneNotes = append(pruneNotes, pruneN...)
			}
			// Route structured results:
			//   - blocked reasons become error text and gate Ready (HasBlocked set below)
			//   - notes (FinalizerSkipped) become informational text, Ready stays True
			// Per 005-reconciliation.md § Finalization.
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
				// Per 005-reconciliation.md § Finalization: the controller
				// waits for readyWhen before deleting the target. This floor
				// ensures the gate is re-checked even if the watch event is
				// delayed. Same principle as the NodePending 1s timer,
				// but graph-level (not per-node) so it doesn't touch the
				// resync timer map.
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

	// Reset resync timers for nodes that were dispatched to workers.
	// Per 005-reconciliation.md § Reconcile: "An SSA apply resets the
	// resync timer. A skipped write during normal evaluation (hash match
	// from a watch event or propagation trigger) does not — the timer
	// still fires to catch divergence that the hash cannot detect."
	//
	// Only dispatched nodes (which evaluated and potentially applied via
	// SSA) reset their resync timers. Nodes that were skipped — no
	// trigger, evaluation-hash match, propagateWhen gate, or
	// coordinator-resolved states (Excluded, Blocked) — retain their
	// existing timer so the consistency floor is preserved. Without
	// this guard, frequent reconciles (driven by watch events on other
	// nodes) perpetually reset timers for stable nodes, preventing the
	// resync timer from ever firing.
	//
	// Drift-triggered dispatches always write (applySSA bypasses the
	// apply-hash check when resyncCorrection=true), so resetting after
	// dispatch is correct. Non-drift dispatches may skip the write if
	// the apply-hash matches — the timer reset is at most one-interval
	// imprecise, bounded by the next drift expiry.
	//
	// Pending and SystemError get short timers regardless of dispatch
	// status — these are retry mechanisms, not drift detection.
	// SystemError: transient server failure needs backoff retry.
	// Pending: resolves via watch-driven propagation (upstream status
	// change → watch event → Path 2 refresh → propagation trigger).
	// The standard resync timer is the safety net for edge cases.
	for i, node := range dag.Nodes {
		nodeState := plan.States[node.ID]
		switch nodeState {
		case NodeReady, NodeNotReady:
			if walk.dispatched[i] {
				state.resetResyncTimer(node.ID, r.resyncInterval(), r.resyncJitter())
			}
			// Reset exponential backoff on any non-SystemError state.
			// Per muse: "Reset on any non-SystemError evaluation, not just
			// success. If a node transitions from SystemError to Error,
			// the backoff should reset because the failure mode changed."
			delete(state.systemErrorBackoff, node.ID)
		case NodePending:
			state.resetResyncTimer(node.ID, r.resyncInterval(), r.resyncJitter())
			delete(state.systemErrorBackoff, node.ID)
		case NodeSystemError:
			// Per 005-reconciliation.md § Trigger: "Transient errors
			// (5xx) retry with exponential backoff [1s, resyncInterval]."
			// Double the backoff duration on each consecutive SystemError,
			// capped at the resync interval. Initial backoff is 1s.
			backoff := state.systemErrorBackoff[node.ID]
			if backoff == 0 {
				backoff = 1 * time.Second
			} else {
				backoff *= 2
			}
			cap := r.resyncInterval()
			if backoff > cap {
				backoff = cap
			}
			state.systemErrorBackoff[node.ID] = backoff
			state.resetResyncTimer(node.ID, backoff, 0)
		case NodeError:
			// Per 005-reconciliation.md § Trigger: "Deterministic
			// errors (4xx) are not retried — same inputs produce the same
			// failure. They resolve via changes or resync." The resync timer
			// is the resync path — the designed recovery mechanism for
			// deterministic errors when no external change arrives.
			state.resetResyncTimer(node.ID, r.resyncInterval(), r.resyncJitter())
			delete(state.systemErrorBackoff, node.ID)
		}
	}

	// Schedule next reconcile. Watch events handle convergence — no
	// periodic polling. The resync timer is the consistency floor.
	// Per 005-reconciliation.md § Why Not: "Periodic full-graph resync
	// ... Informer resyncs trigger all nodes simultaneously — correlated,
	// expensive. Per-node resync timers with jitter amortize resync."
	//
	// SystemError nodes have short backoff timers; all other nodes
	// (including Pending) use the standard resync interval.
	//
	// requeueFloor provides an explicit lower bound independent of drift
	// timers — used for graph-level transient conditions (e.g., finalization
	// in progress) that are not associated with any single node.
	requeue := requeueFloor
	if next := state.nextResyncExpiry(); !next.IsZero() {
		if remaining := time.Until(next); remaining > 0 {
			if requeue == 0 || remaining < requeue {
				requeue = remaining
			}
		}
	}
	if requeue > 0 {
		return ctrl.Result{RequeueAfter: requeue}, nil
	}
	// If a dynamic GVK resolved for the first time (or changed), requeue
	// immediately so the next reconcile compiles with the schema available.
	// The compilation key now includes the resolved GVK hints, producing a
	// typed artifact on the next pass.
	if walk.dynamicGVKChanged {
		return ctrl.Result{Requeue: true}, nil
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
		kind  string
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
			if node.HasDynamicGVR() {
				continue // GVR contains CEL — resolved at reconcile time, not startup
			}
			id := node.Identity()
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
			toHydrate[hydrateKey{graph: graph, gvr: gvr, kind: kind}] = struct{}{}
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
		go func(graph graphKey, gvr schema.GroupVersionResource, kind string) {
			defer wg.Done()
			ownerID := graphOwnerID(graph)
			if err := watchMgr.ensureWatch(gvr, kind, ownerID); err != nil {
				logger.Error(err, "failed to hydrate watch", "gvr", gvr, "graph", graph.Name)
			} else {
				logger.V(1).Info("hydrated watch from revision", "gvr", gvr, "graph", graph.Name)
			}
		}(k.graph, k.gvr, k.kind)
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
// resyncInterval overrides the per-node resync timer interval. 0 uses the default (30m).
func SetupWithManager(mgr ctrl.Manager, restConfig *rest.Config, maxWorkers int, resyncInterval time.Duration) (shutdown func(), caches *graphCaches, err error) {
	RegisterMetrics(crmetrics.Registry)

	if maxWorkers <= 0 {
		maxWorkers = DefaultMaxConcurrentReconciles
	}

	metadataClient, err := metadata.NewForConfig(restConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("creating metadata client: %w", err)
	}

	watchChan := make(chan event.GenericEvent, 256)

	watchMgr := newWatchManager(metadataClient, 12*time.Hour, nil, log.Log)
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
	//
	// The HTTP client must inherit TLS settings from rest.Config (CAData,
	// ServerName, ClientCert). Passing nil causes the discovery client to
	// build an internal HTTP client that doesn't pick up TLS config in all
	// environments (notably envtest with self-signed certs). Mirrors
	// upstream pkg/client/set.go which constructs rest.HTTPClientFor once.
	httpClient, err := rest.HTTPClientFor(restConfig)
	if err != nil {
		log.Log.Error(err, "failed to build HTTP client for schema resolver; compile-time type checking disabled for resource nodes")
		httpClient = nil
	}
	schemaResolver, err := schemaresolver.NewCombinedResolver(restConfig, httpClient)
	if err != nil {
		// Schema resolution is an operational dependency — log the failure.
		// All resource nodes will fall back to dyn (no field-level type checking).
		log.Log.Error(err, "failed to create schema resolver; compile-time type checking disabled for resource nodes")
		schemaResolver = nil
	}

	reconciler := &GraphReconciler{
		Client:         mgr.GetClient(),
		APIReader:      mgr.GetAPIReader(),
		SchemaResolver: schemaResolver,
		SchemaGen:      NewSchemaGeneration(),
		Watcher:        coordinator,
		Caches:         newGraphCaches(),
		Resources:      newResourceCache(),
		ResyncInterval:  resyncInterval,
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
		// Per 004-compilation.md § Type Cache: "Any schema change advances [the
		// generation counter]." Only advance when a genuinely unresolved GVK
		// becomes available — not for every first-watched type (core types like
		// ConfigMap are always resolvable and don't represent schema changes).
		if reconciler.SchemaGen != nil {
			reconciler.SchemaGen.AdvanceGeneration()
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

	// Watch CRDs for schema changes. Per 004-compilation.md § Compilation Cache:
	// "Any schema change (CRD installed, updated, removed) advances [the
	// generation counter]." The onNewType callback handles CRD install (first
	// informer for a GVR). This watch handles CRD updates and deletions —
	// schema changes to already-watched types.
	crdGVR := schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}
	crdCtx, crdCancel := context.WithCancel(context.Background())
	crdGenericInformer := metadatainformer.NewFilteredMetadataInformer(metadataClient, crdGVR, metav1.NamespaceAll, 0, cache.Indexers{}, nil)
	crdInformer := crdGenericInformer.Informer()
	crdInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{ //nolint:errcheck
		UpdateFunc: func(_, _ any) {
			if reconciler.SchemaGen != nil {
				reconciler.SchemaGen.AdvanceGeneration()
			}
		},
		DeleteFunc: func(_ any) {
			if reconciler.SchemaGen != nil {
				reconciler.SchemaGen.AdvanceGeneration()
			}
		},
	})
	go crdInformer.RunWithContext(crdCtx)
	_ = crdCancel // stopped when the WatchManager shuts down (same process lifecycle)

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
