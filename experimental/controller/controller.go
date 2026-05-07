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
	Metrics        *MetricStore               // per-controller metric store; nil = metrics disabled
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
	graphKey  string // "namespace/name" — used by metric store
	// Metric store reference — persists across reconcile cycles.
	metricStore *MetricStore
}

func newReconcileScope(graph *unstructured.Unstructured, watcher *watches.GraphWatcher, metricStore *MetricStore) *reconcileScope {
	return &reconcileScope{
		graph:       graph,
		watcher:     watcher,
		name:        graph.GetName(),
		namespace:   graph.GetNamespace(),
		graphKey:    graph.GetNamespace() + "/" + graph.GetName(),
		metricStore: metricStore,
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

	// Set up watch handle for this reconcile cycle.
	var watcher *watches.GraphWatcher
	if r.Watcher != nil {
		watcher = r.Watcher.ForGraph(req.NamespacedName)
	}

	// -----------------------------------------------------------------------
	// 5–6. Resolve revision (ensure + compile)
	// -----------------------------------------------------------------------
	rev, err := r.resolveRevision(ctx, graph)
	if err != nil {
		return rev.result, err
	}
	if rev.earlyReturn {
		return rev.result, nil
	}

	// -----------------------------------------------------------------------
	// 7. Propagate the DAG
	// -----------------------------------------------------------------------
	rs := newReconcileScope(graph, watcher, r.Metrics)
	wp := r.reconcilePropagate(ctx, rs, rev.state, rev.eval, rev.dag, rev.plan)

	// Post-propagation recompile check: if the propagation created a CRD, re-validate
	// compilation to catch child graph type errors within the same cycle.
	if wp.needsRecompile && rev.compilationErr == nil {
		if _, _, err := r.compileRevision(ctx, graph.GetNamespace(), rev.activeRevision); err != nil {
			rev.compilationErr = err
			logger.Error(err, "post-walk recompilation detected error")
		}
	}

	// -----------------------------------------------------------------------
	// 8. Prune removed resources
	// -----------------------------------------------------------------------
	pr := r.reconcilePrune(ctx, rs, rev.state, rev.eval, rev.dag, wp.keys, rev.supersededRevisions, &wp.summary)

	// -----------------------------------------------------------------------
	// 9. Update status
	// -----------------------------------------------------------------------
	rstate := &reconcileState{
		compiled:    rev.compilationErr == nil,
		compiledErr: rev.compilationErr,
		planSummary: wp.summary,
		nodeErrors:  append(wp.nodeErrors, pr.errors...),
		nodeNotes:   pr.notes,
	}
	if err := r.updateStatus(ctx, graph, rstate); err != nil {
		logger.Error(err, "status update")
	}

	allReady := rstate.compiled && rstate.planSummary.IsClean()
	r.gcSupersededRevisions(ctx, rev.activeRevision, rev.supersededRevisions, allReady, pr.ok && !pr.pending)

	// -----------------------------------------------------------------------
	// 10. Requeue if not fully ready
	// -----------------------------------------------------------------------
	return r.determineRequeue(wp.needsRecompile, pr.pending, allReady)
}

// revisionResult bundles the outputs of resolveRevision so the caller can
// destructure a single return value instead of many.
type revisionResult struct {
	activeRevision      *unstructured.Unstructured
	supersededRevisions []*unstructured.Unstructured
	compilationErr      error
	state               *instanceState
	eval                *evaluator
	dag                 *dagpkg.DAG
	plan                *PlanState
	// earlyReturn is true when resolveRevision already handled the response
	// (e.g., deterministic compilation error). The caller should return result/nil.
	earlyReturn bool
	result      ctrl.Result
}

// resolveRevision combines phases 5 (ensure revision) and 6 (compile revision).
// It answers "what are we reconciling?" by producing a compiled DAG, evaluator,
// and plan — or signals an early return when compilation fails deterministically.
func (r *GraphReconciler) resolveRevision(ctx context.Context, graph *unstructured.Unstructured) (revisionResult, error) {
	logger := log.FromContext(ctx)

	// --- Phase 5: Ensure revision ---
	activeRevision, supersededRevisions, err := r.ensureRevision(ctx, graph)
	var compilationErr error
	if err != nil {
		graphName := graph.GetName()
		namespace := graph.GetNamespace()

		revisions, listErr := listRevisions(ctx, r.Client, graphName, namespace)
		if listErr != nil || len(revisions) == 0 {
			if statusErr := r.updateStatus(ctx, graph, &reconcileState{compiled: false, compiledErr: err}); statusErr != nil {
				return revisionResult{}, fmt.Errorf("updating status after revision error: %w", statusErr)
			}
			if isTransientError(err) {
				return revisionResult{}, fmt.Errorf("ensuring revision: %w", err)
			}
			logger.Info("deterministic compilation error; status written, awaiting spec change", "error", err)
			return revisionResult{earlyReturn: true}, nil
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

	// --- Phase 6: Compile revision ---
	_, state, err := r.compileRevision(ctx, graph.GetNamespace(), activeRevision)
	if err != nil {
		if statusErr := r.updateStatus(ctx, graph, &reconcileState{compiled: false, compiledErr: err}); statusErr != nil {
			return revisionResult{}, fmt.Errorf("updating status after compilation error: %w", statusErr)
		}
		if isTransientError(err) {
			return revisionResult{}, fmt.Errorf("compiling revision: %w", err)
		}
		logger.Info("deterministic compilation error; status written, awaiting spec change", "error", err)
		return revisionResult{earlyReturn: true}, nil
	}

	eval := newEvaluator(state)
	eval.effectiveGeneration = effectiveGeneration
	dag := state.compilation.dag
	plan := NewPlanState(dag)

	return revisionResult{
		activeRevision:      activeRevision,
		supersededRevisions: supersededRevisions,
		compilationErr:      compilationErr,
		state:               state,
		eval:                eval,
		dag:                 dag,
		plan:                plan,
	}, nil
}

// determineRequeue decides the ctrl.Result based on post-reconcile state.
func (r *GraphReconciler) determineRequeue(needsRecompile, prunePending, allReady bool) (ctrl.Result, error) {
	// Dynamic GVK change needs immediate recompile.
	if needsRecompile {
		return ctrl.Result{Requeue: true}, nil
	}

	// Finalization in progress — short requeue as a consistency floor.
	if prunePending {
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
	return r.propagate(ctx, rs, state, eval, dag, plan)
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
// previous-applied-key universe from two sources (watch cache and superseded
// revision static keys), diffs against the current walk's applied keys, and
// deletes the difference.
//
// No cross-cycle state: prune candidates are derived entirely from
// cluster-observable state (watch cache label scan + revision specs).
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

	// Prune stale metrics: any metric in a superseded revision but
	// absent from the current DAG is orphaned (node removed or metric renamed).
	if r.Metrics != nil && len(supersededRevisions) > 0 {
		r.pruneStaleMetrics(ctx, rs, dag, supersededRevisions)
	}
	cluster := r.cluster()
	allPreviousKeys := map[string]Applied{}
	logger.V(1).Info("prune gate open", "superseded", len(supersededRevisions))

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

// pruneStaleMetrics removes prometheus metrics that existed in superseded revisions
// but are absent from the current DAG. Mirrors resource pruning: propagation
// creates/updates metrics, prune removes them when their nodes disappear.
func (r *GraphReconciler) pruneStaleMetrics(ctx context.Context, rs *reconcileScope, dag *dagpkg.DAG, supersededRevisions []*unstructured.Unstructured) {
	logger := log.FromContext(ctx)

	// Collect metric names from current DAG.
	currentMetrics := map[string]bool{}
	if dag != nil {
		for _, node := range dag.Nodes {
			if node.Metric != nil {
				currentMetrics[node.Metric.Name] = true
			}
		}
	}

	// Collect metric names from superseded revisions.
	for _, rev := range supersededRevisions {
		spec, err := extractRevisionSpec(rev)
		if err != nil {
			continue
		}
		for _, node := range spec.Nodes {
			if node.Metric != nil && !currentMetrics[node.Metric.Name] {
				logger.Info("pruning stale metric", "metric", node.Metric.Name, "fromRevision", rev.GetName())
				r.Metrics.Remove(rs.graphKey, node.Metric.Name)
			}
		}
	}
}
