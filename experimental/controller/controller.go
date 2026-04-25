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
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gobuffalo/flect"
	"github.com/prometheus/client_golang/prometheus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/cel/openapi/resolver"

	"github.com/kubernetes-sigs/kro/experimental/controller/compiler"
	dagpkg "github.com/kubernetes-sigs/kro/experimental/controller/dag"
	graphpkg "github.com/kubernetes-sigs/kro/experimental/controller/graph"
	"github.com/kubernetes-sigs/kro/experimental/controller/watches"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
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
	SchemaGen      *compiler.SchemaGeneration       // nil = no generation tracking; never triggers recompilation
	Watcher        *watches.WatchCoordinator       // nil = no dynamic watches (backward compat with existing tests)
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
	logger.V(1).Info("reconcile started")
	defer func() {
		if reconcileErr != nil {
			logger.Error(reconcileErr, "reconcile failed", "duration", time.Since(reconcileStart))
		} else {
			logger.V(1).Info("reconcile complete", "duration", time.Since(reconcileStart))
		}
	}()
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
		logger.Info("self-deleted graph — owner is terminating")
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
	var watcher *watches.GraphWatcher
	var walkAttempted bool
	if r.Watcher != nil {
		watcher = r.Watcher.ForGraph(req.NamespacedName)
		defer func() {
			// Commit watches whenever the walk ran, regardless of
			// reconcileErr. A post-walk error (e.g., optimistic lock
			// conflict on status update) should not discard valid watch
			// registrations from the walk. Without this, all events
			// between the failed commit and the next successful commit
			// are silently dropped — the root cause of timer-dependent
			// convergence for CRD Establishment and other status
			// propagation patterns.
			watcher.Done(walkAttempted)
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
	plan := dagpkg.NewPlanState(dag)

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
	var collectionChanges map[string][]watches.CollectionChange // Watch incremental cache
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
		watchTriggers := watcher.DrainTriggers()
		for nodeID := range watchTriggers {
			triggered[nodeID] = true
		}
		// Watch collection changes: buffered resource keys for
		// incremental cache updates. Drained alongside triggers so the
		// coordinator knows which specific items changed.
		collectionChanges = watcher.DrainCollectionChanges()
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
			if prevState == dagpkg.NodeSystemError {
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
		logger.V(1).Info("no nodes triggered — skipping walk")
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
		nodeKeys:             make(map[string][]string, len(dag.Nodes)),
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
		if res.err != nil && node.Type() == graphpkg.NodeTypeWatch &&
			len(res.collectionCacheUpdate) == 0 {
			state.collectionDirty[node.ID] = true
		}

		// Error handling: block dependents, continue independent branches.
		// Classify the API error to determine the plan state — NodeError
		// for client errors (4xx), NodeSystemError for server/infra
		// failures (5xx/timeout/network). Both retry; the distinction
		// flows into the status condition for operator triage.
		if res.state == dagpkg.NodeError {
			info := classifyAPIError(res.err)
			prevState := state.previousPlanStates[node.ID]
			plan.SetState(dag, node.ID, info.state)
			state.previousPlanStates[node.ID] = info.state
			walk.nodeErrors = append(walk.nodeErrors, fmt.Sprintf("%s: %s", node.ID, info.reason))
			logger.V(0).Info("error on node", "node", node.ID,
				"previousState", prevState, "state", info.state, "reason", info.reason, "error", res.err)
			// Retain previous keys — the resource may still exist in the cluster.
			walk.carryForwardKeys(node.ID)
			// Dispatch dependents — tryDispatch will see the error state
			// and mark them as Blocked via the dependency check.
			for _, depIdx := range dag.Dependents[node.ID] {
				walk.tryDispatch(depIdx)
			}
			continue
		}
		if res.state == dagpkg.NodeConflict {
			prevState := state.previousPlanStates[node.ID]
			plan.SetState(dag, node.ID, dagpkg.NodeConflict)
			state.previousPlanStates[node.ID] = dagpkg.NodeConflict
			state.previousScope[node.ID] = res.scopeValue
			state.previousKeys[node.ID] = res.keys
			walk.nodeErrors = append(walk.nodeErrors, fmt.Sprintf("%s: field conflict", node.ID))
			logger.V(0).Info("conflict on node", "node", node.ID, "previousState", prevState, "error", res.err)
			walk.carryForwardKeys(node.ID)
			for _, depIdx := range dag.Dependents[node.ID] {
				walk.tryDispatch(depIdx)
			}
			continue
		}
		if res.state == dagpkg.NodePending {
			prevState := state.previousPlanStates[node.ID]
			plan.SetState(dag, node.ID, dagpkg.NodePending)
			// Under declared-keyword classification, patch→template is an
			// authoring event (patch: → template: spec edit) handled by
			// revision supersession. No runtime reclassification reset.
			state.previousPlanStates[node.ID] = dagpkg.NodePending
			state.previousScope[node.ID] = res.scopeValue
			state.previousKeys[node.ID] = res.keys
			logger.V(1).Info("data pending for node", "node", node.ID, "previousState", prevState, "error", res.err)
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

		if r.SchemaGen != nil && node.Type() == graphpkg.NodeTypeTemplate {
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
				eval.nodeReady[res.scopeKey] = (res.state == dagpkg.NodeReady)
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
		prevState := state.previousPlanStates[node.ID]
		plan.SetState(dag, node.ID, res.state)
		if prevState != res.state {
			logger.V(1).Info("node state transition", "node", node.ID,
				"previousState", prevState, "newState", res.state, "duration", res.evalDuration)
		}

		// Surface readyWhen expression errors. Per 001-graph.md: readyWhen
		// errors produce NodeNotReady (not NodeError), so they don't gate
		// dependents. But the user needs to know their expression is broken
		// and won't self-heal — log it and include in nodeErrors for status.
		if res.state == dagpkg.NodeNotReady && res.err != nil && errors.Is(res.err, compiler.ErrReadyWhenFailed) {
			walk.nodeErrors = append(walk.nodeErrors, fmt.Sprintf("%s: %s", node.ID, res.err.Error()))
			logger.V(0).Info("readyWhen expression error (not gating dependents)",
				"node", node.ID, "error", res.err)
		}

		if res.state == dagpkg.NodeReady || res.state == dagpkg.NodeNotReady {
			walk.nodeKeys[node.ID] = res.keys
		} else {
			// Non-success states that reach here (e.g., dagpkg.NodeNotReady with keys) —
			// retain previous keys since the resource may still exist.
			walk.carryForwardKeys(node.ID)
		}

		// Step 8: Propagation check — hash the specific field paths
		// dependents reference from this node's output, plus propagateWhen
		// state. If the hash differs from the previous reconcile, mark
		// dependents as having a propagation trigger.
		// Per 005-reconciliation.md § Propagation.
		if res.state == dagpkg.NodeReady || res.state == dagpkg.NodeNotReady {
			if observed := eval.scope[node.ID]; observed != nil {
				walk.propagateIfChanged(node, observed, res.state)
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
		// ReadinessDependents: nodes that consume .ready() without a hard
		// DAG edge (e.g., rgdInstanceStatus). When this node's readiness
		// changes, these consumers must re-evaluate. propagationTriggered
		// was set above (Step 8) so tryDispatch bypasses the skip check.
		for _, depIdx := range dag.ReadinessDependents[node.ID] {
			walk.tryDispatch(depIdx)
		}
	}

	// Finalize skipped nodes: nodes that were skipped (outputsReady) and never
	// re-dispatched via propagation trigger still have plan.States = Pending.
	// Restore their previous state for the plan summary (status reporting).
	walkAttempted = true

	// Stale readiness dispatch: nodes dispatched while readiness deps were
	// inflight evaluated with stale .ready() values. Deposit explicit
	// triggers for these nodes so the next reconcile re-evaluates them
	// with fresh scope. The requeueFloor ensures a follow-up reconcile
	// fires promptly even if no watch events arrive.
	if len(walk.staleReadinessDeps) > 0 && watcher != nil {
		for nodeID := range walk.staleReadinessDeps {
			watcher.DepositTrigger(nodeID)
		}
		if requeueFloor == 0 || requeueFloor > time.Second {
			requeueFloor = time.Second
		}
	}

	dagpkg.FinalizeSkippedStates(plan, walk.outputsReady, state.previousPlanStates, func(nodeID string) {
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
		if plan.States[node.ID] == dagpkg.NodeBlocked || plan.States[node.ID] == dagpkg.NodePending {
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
				watcher.RetainWatches(node.ID)
			}
		}
	}

	// Flatten per-node keys into the applied key set for the prune phase.
	// Last-writer-wins: if a node was carried forward (skip) and then
	// dispatched (propagation trigger) in the same walk, only the dispatch
	// result survives. This prevents stale keys from blocking prune
	// candidates when forEach scales to zero children.
	var appliedKeys []string
	for _, keys := range walk.nodeKeys {
		appliedKeys = append(appliedKeys, keys...)
	}
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
			appliedSet := r.Watcher.Watches.DeriveAppliedSet(graph.GetName(), graph.GetNamespace())
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
		supersededDAGs := map[string]*dagpkg.DAG{}
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
				case dagpkg.NodeSystemError:
					summary.HasSystemError = true
				case dagpkg.NodeConflict:
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
	allReady := rstate.compiled && !rstate.PlanSummary.HasPending && !rstate.PlanSummary.HasNotReady &&
		!rstate.PlanSummary.HasBlocked && !rstate.PlanSummary.HasConflict && !rstate.PlanSummary.HasError && !rstate.PlanSummary.HasSystemError
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
		case dagpkg.NodeReady, dagpkg.NodeNotReady:
			if walk.dispatched[i] {
				state.resetResyncTimer(node.ID, r.resyncInterval(), r.resyncJitter())
			}
			// Reset exponential backoff on any non-SystemError state.
			// Per muse: "Reset on any non-SystemError evaluation, not just
			// success. If a node transitions from SystemError to Error,
			// the backoff should reset because the failure mode changed."
			delete(state.systemErrorBackoff, node.ID)
		case dagpkg.NodePending:
			state.resetResyncTimer(node.ID, r.resyncInterval(), r.resyncJitter())
			delete(state.systemErrorBackoff, node.ID)
		case dagpkg.NodeSystemError:
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
		case dagpkg.NodeError:
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


