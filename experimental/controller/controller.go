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
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/cel/openapi/resolver"

	"github.com/ellistarn/kro/experimental/controller/compiler"
	dagpkg "github.com/ellistarn/kro/experimental/controller/dag"
	"github.com/ellistarn/kro/experimental/controller/watches"
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

const (
	finalizer = "experimental.kro.run/graph-controller"

	// systemErrorRequeueInterval is the retry interval for Graphs with
	// nodes in SystemError state. Used by delete.go for teardown retries.
	systemErrorRequeueInterval = 5 * time.Second

	// finalizationRequeueInterval is the consistency floor for finalization
	// in progress. The primary trigger is a watch event on the gate
	// resource (when readyWhen's dependencies change). This timer ensures
	// the gate is re-checked even if that watch event is slow.
	finalizationRequeueInterval = 5 * time.Second

	// notReadyRequeueInterval is the requeue interval when the graph is
	// not fully ready. Watch events handle most convergence; this is a
	// safety net for edge cases where events are missed.
	notReadyRequeueInterval = 5 * time.Second

	// DefaultMaxConcurrentReconciles is the number of reconcile workers.
	// Multiple workers keep the API server busy — each reconcile does
	// SSA applies, GETs, and informer syncs that can block. 16 is a
	// heuristic — high enough to keep a typical API server busy under
	// normal graph workloads, tune if needed.
	DefaultMaxConcurrentReconciles = 16
)

// GraphReconciler reconciles Graph objects.
type GraphReconciler struct {
	Client         client.Client
	APIReader      client.Reader              // direct API server reader — bypasses cache for managed resources
	SchemaResolver resolver.SchemaResolver    // nil = all resource nodes fall back to dyn
	SchemaGen      *compiler.SchemaGeneration // nil = no generation tracking; never triggers recompilation
	Watcher        *watches.WatchCoordinator  // nil = no dynamic watches (backward compat with existing tests)
	Caches         *InstanceMap               // per-revision compiled expression caches
	Scope          *scopeResolver             // nil = unknown scope; staticResourceKey falls back to namespace-substitution heuristic
}

// reconcileScope bundles the per-reconcile identity context that threads
// through every call in the walk and prune paths. Created once at the
// start of each Reconcile call. Fields are read-only after construction.
type reconcileScope struct {
	graph   *unstructured.Unstructured
	watcher *watches.GraphWatcher
	// Pre-derived fields to avoid repeated calls.
	name      string
	namespace string
}

func newReconcileScope(graph *unstructured.Unstructured, watcher *watches.GraphWatcher) *reconcileScope {
	return &reconcileScope{
		graph:     graph,
		watcher:   watcher,
		name:      graph.GetName(),
		namespace: graph.GetNamespace(),
	}
}

// cluster creates a clusterAccess that bundles the API server dependencies
// needed by the execution layer (apply.go, node.go, prune.go, finalization.go).
func (r *GraphReconciler) cluster() *clusterAccess {
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	return &clusterAccess{
		client: r.Client,
		reader: reader,
		scope:  r.Scope,
	}
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

	// -----------------------------------------------------------------------
	// 1. Get the Graph
	// -----------------------------------------------------------------------
	graph := &unstructured.Unstructured{}
	graph.SetGroupVersionKind(GraphGVK)
	if err := r.Client.Get(ctx, req.NamespacedName, graph); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// -----------------------------------------------------------------------
	// 2. Handle deletion
	// -----------------------------------------------------------------------
	if !graph.GetDeletionTimestamp().IsZero() {
		return r.reconcileDelete(ctx, graph)
	}

	// -----------------------------------------------------------------------
	// 3. Ensure finalizer — must run before owner lifecycle check so that
	//    self-delete triggers reconcileDelete (teardown) instead of immediate
	//    garbage collection.
	// -----------------------------------------------------------------------
	if !controllerutil.ContainsFinalizer(graph, finalizer) {
		controllerutil.AddFinalizer(graph, finalizer)
		if err := r.Client.Update(ctx, graph); err != nil {
			return ctrl.Result{}, err
		}
	}

	// -----------------------------------------------------------------------
	// 4. Handle owner lifecycle — self-delete if owner is terminating
	// -----------------------------------------------------------------------
	if r.ownerDeleting(ctx, graph) {
		if err := r.Client.Delete(ctx, graph); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("self-deleting graph for owner teardown: %w", err)
		}
		logger.Info("self-deleted graph — owner is terminating")
		return ctrl.Result{}, nil
	}

	// Set up watch tracking for this reconcile cycle.
	var watcher *watches.GraphWatcher
	propagateAttempted := false
	if r.Watcher != nil {
		watcher = r.Watcher.ForGraph(req.NamespacedName)
		defer func() {
			// Commit watches whenever the walk ran, regardless of
			// reconcileErr. A post-walk error (e.g., optimistic lock
			// conflict on status update) should not discard valid watch
			// registrations from the walk.
			watcher.Done(propagateAttempted)
		}()
	}

	// -----------------------------------------------------------------------
	// 5. Ensure revision
	// -----------------------------------------------------------------------
	activeRevision, supersededRevisions, err := r.ensureRevision(ctx, graph)
	var compilationErr error
	if err != nil {
		graphName := graph.GetName()
		namespace := graph.GetNamespace()

		revisions, listErr := listRevisions(ctx, r.Client, graphName, namespace)
		if listErr != nil || len(revisions) == 0 {
			if statusErr := r.updateStatus(ctx, graph, &reconcileState{compiled: false, compiledErr: err}); statusErr != nil {
				return ctrl.Result{}, fmt.Errorf("updating status after revision error: %w", statusErr)
			}
			if isTransientError(err) {
				return ctrl.Result{}, fmt.Errorf("ensuring revision: %w", err)
			}
			logger.Info("deterministic compilation error; status written, awaiting spec change", "error", err)
			return ctrl.Result{}, nil
		}
		activeRevision = revisions[len(revisions)-1]
		supersededRevisions = nil
		compilationErr = err
		logger.Error(err, "compilation failed for current generation")
		logger.Info("falling back to previous revision",
			"revision", activeRevision.GetName(),
			"generation", revisionGeneration(activeRevision))
	}

	effectiveGeneration := pickEffectiveGeneration(graph, activeRevision, compilationErr)

	// -----------------------------------------------------------------------
	// 6. Compile revision
	// -----------------------------------------------------------------------
	_, state, err := r.compileRevision(ctx, graph.GetNamespace(), activeRevision)
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
	dag := state.compilation.dag
	plan := NewPlanState(dag)

	// On revision transition, transfer previousAppliedKeys from superseded
	// revisions so the prune phase knows what the old revision applied.
	isRevisionTransition := len(supersededRevisions) > 0
	if isRevisionTransition && state.prune.previousAppliedKeys == nil {
		for _, rev := range supersededRevisions {
			oldKey := rev.GetNamespace() + "/" + rev.GetName()
			if oldState := r.Caches.get(oldKey); oldState != nil {
				for k, a := range oldState.prune.previousAppliedKeys {
					if state.prune.previousAppliedKeys == nil {
						state.prune.previousAppliedKeys = make(map[string]Applied)
					}
					state.prune.previousAppliedKeys[k] = a
				}
			}
		}
	}

	// -----------------------------------------------------------------------
	// 7. Propagate the DAG
	// -----------------------------------------------------------------------
	rs := newReconcileScope(graph, watcher)
	wp := r.reconcilePropagate(ctx, rs, state, eval, dag, plan)
	propagateAttempted = true

	// Post-propagation recompile check: if the propagation created a CRD, re-validate
	// compilation to catch child graph type errors within the same cycle.
	if wp.needsRecompile && compilationErr == nil {
		if _, _, err := r.compileRevision(ctx, graph.GetNamespace(), activeRevision); err != nil {
			compilationErr = err
			logger.Error(err, "post-walk recompilation detected error")
		}
	}

	// -----------------------------------------------------------------------
	// 8. Prune removed resources
	// -----------------------------------------------------------------------
	pr := r.reconcilePrune(ctx, rs, state, eval, dag, wp.keys, supersededRevisions, &wp.summary)

	// -----------------------------------------------------------------------
	// 9. Update status
	// -----------------------------------------------------------------------
	rstate := &reconcileState{
		compiled:    compilationErr == nil,
		compiledErr: compilationErr,
		planSummary: wp.summary,
		nodeErrors:  append(wp.nodeErrors, pr.errors...),
		nodeNotes:   pr.notes,
	}
	if err := r.updateStatus(ctx, graph, rstate); err != nil {
		logger.Error(err, "status update")
	}

	allReady := rstate.compiled && rstate.planSummary.IsClean()
	r.gcSupersededRevisions(ctx, activeRevision, supersededRevisions, allReady, pr.ok && !pr.pending)

	// -----------------------------------------------------------------------
	// 10. Requeue if not fully ready
	// -----------------------------------------------------------------------

	// Dynamic GVK change needs immediate recompile.
	if wp.needsRecompile {
		return ctrl.Result{Requeue: true}, nil
	}

	// Finalization in progress — short requeue as a consistency floor.
	if pr.pending {
		return ctrl.Result{RequeueAfter: finalizationRequeueInterval}, nil
	}

	// If not all ready, requeue after a short interval as a safety net.
	// Watch events handle the primary convergence path.
	if !allReady {
		return ctrl.Result{RequeueAfter: notReadyRequeueInterval}, nil
	}

	return ctrl.Result{}, nil
}

// reconcilePropagate executes the DAG propagation and transfers carry-forward state.
// It evaluates every node in topological order, applying templates via SSA
// and recording scope for downstream CEL expressions.
func (r *GraphReconciler) reconcilePropagate(
	ctx context.Context,
	rs *reconcileScope,
	state *instanceState,
	eval *evaluator,
	dag *dagpkg.DAG,
	plan *PlanState,
) *propagateResult {
	propagateRes := r.propagate(ctx, rs, state, eval, dag, plan)

	// Transfer carry-forward state for next reconcile.
	state.propagate.previousPlanStates = propagateRes.plan
	state.propagate.previousKeys = propagateRes.nodeKeys

	return propagateRes
}

// prunePhaseResult carries the outputs of reconcilePrune back to the caller.
type prunePhaseResult struct {
	ok      bool     // true unless a hard API error occurred
	pending bool     // true if deferred keys remain (finalization in progress)
	errors  []string // node-level error messages for status
	notes   []string // informational messages for status
}

// reconcilePrune removes resources that are no longer in the applied set.
// It gates on summary flags (uncertain absence blocks pruning), builds the
// full previous-applied-key universe from three sources (watch cache,
// carry-forward, superseded revisions), diffs against the current walk's
// applied keys, and deletes the difference.
//
// Side effects: mutates summary (sets HasBlocked/HasError/HasSystemError/
// HasConflict on prune failures) and state.prune (updates carry-forward keys).
func (r *GraphReconciler) reconcilePrune(
	ctx context.Context,
	rs *reconcileScope,
	state *instanceState,
	eval *evaluator,
	dag *dagpkg.DAG,
	appliedKeys []Applied,
	supersededRevisions []*unstructured.Unstructured,
	summary *PlanSummary,
) prunePhaseResult {
	logger := log.FromContext(ctx)
	result := prunePhaseResult{ok: true}

	// Per 005-reconciliation.md § Prune: "Uncertain absence (Pending, Blocked,
	// Error, SystemError) blocks pruning — the resource might reappear once the
	// blocker resolves."
	if summary.HasUncertainty() {
		return result
	}

	cluster := r.cluster()
	allPreviousKeys := map[string]Applied{}
	logger.V(1).Info("prune gate open", "previousAppliedKeys", len(state.prune.previousAppliedKeys), "deferredPruneKeys", len(state.prune.deferredPruneKeys), "superseded", len(supersededRevisions))

	// Derive the applied set from the watch cache.
	if r.Watcher != nil {
		appliedSet := r.Watcher.DeriveAppliedSet(rs.name, rs.namespace)
		for key, entry := range appliedSet {
			allPreviousKeys[key] = Applied{
				Key:      key,
				NodeType: entry.NodeType,
			}
		}
	}

	// Include the previous reconcile's applied keys to cover the
	// informer lag window.
	for k, a := range state.prune.previousAppliedKeys {
		allPreviousKeys[k] = a
	}
	// Include keys whose deletion was deferred in the previous reconcile.
	for _, a := range state.prune.deferredPruneKeys {
		allPreviousKeys[a.Key] = a
	}
	// Update the previous key set for the next reconcile.
	state.prune.updateAppliedKeys(appliedKeys)

	// Extract static keys from superseded revisions for resources
	// that may not yet be in the informer cache.
	supersededDAGs := map[string]*dagpkg.DAG{}
	for key, a := range extractStaticKeysFromRevisions(supersededRevisions, rs.namespace, cluster.scope) {
		allPreviousKeys[key] = a
	}
	for _, rev := range supersededRevisions {
		// Compile superseded revisions to access their finalizer relationships.
		if _, revState, compileErr := r.compileRevision(ctx, rs.namespace, rev); compileErr == nil {
			supersededDAGs[rev.GetName()] = revState.compilation.dag
		}
	}

	if len(allPreviousKeys) == 0 {
		return result
	}

	// Build currentSet from walk applied keys.
	currentSet := map[string]Applied{}
	for _, a := range appliedKeys {
		currentSet[a.Key] = a
	}
	candidates := collectPruneCandidates(allPreviousKeys, currentSet)
	allDAGs := []*dagpkg.DAG{}
	if dag != nil {
		allDAGs = append(allDAGs, dag)
	}
	for _, d := range supersededDAGs {
		allDAGs = append(allDAGs, d)
	}

	pr := cluster.pruneResources(ctx, rs, candidates, currentSet, allDAGs, eval, state, true)

	result.errors = append(result.errors, pr.BlockedReasons...)
	result.notes = append(result.notes, pr.Notes...)
	if len(pr.BlockedReasons) > 0 {
		summary.HasBlocked = true
	}
	deferredKeys := collectDeferredKeys(pr.Outcomes, allPreviousKeys)
	if len(deferredKeys) > 0 {
		result.pending = true
		state.prune.deferredPruneKeys = deferredKeys
	} else {
		state.prune.deferredPruneKeys = nil
	}
	if pr.Err != nil {
		logger.Error(pr.Err, "pruning removed resources")
		result.ok = false
		info := classifyAPIError(pr.Err)
		switch info.state {
		case NodeSystemError:
			summary.HasSystemError = true
		case NodeConflict:
			summary.HasConflict = true
		default:
			summary.HasError = true
		}
		result.errors = append(result.errors, fmt.Sprintf("prune: %s", info.reason))
	}
	return result
}
