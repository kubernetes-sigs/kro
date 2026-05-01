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
	APIReader      client.Reader                   // direct API server reader — bypasses cache for managed resources
	SchemaResolver resolver.SchemaResolver         // nil = all resource nodes fall back to dyn
	SchemaGen      *compiler.SchemaGeneration      // nil = no generation tracking; never triggers recompilation
	Watcher        *watches.WatchCoordinator       // nil = no dynamic watches (backward compat with existing tests)
	Caches         *InstanceMap                    // per-revision compiled expression caches
	Scope          GVKScopeResolver                // nil = unknown scope; staticResourceKey falls back to namespace-substitution heuristic
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
	// 3. Handle owner lifecycle — self-delete if owner is terminating
	// -----------------------------------------------------------------------
	if r.ownerDeleting(ctx, graph) {
		if err := r.Client.Delete(ctx, graph); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("self-deleting graph for owner teardown: %w", err)
		}
		logger.Info("self-deleted graph — owner is terminating")
		return ctrl.Result{}, nil
	}

	// -----------------------------------------------------------------------
	// 4. Ensure finalizer
	// -----------------------------------------------------------------------
	if !controllerutil.ContainsFinalizer(graph, finalizer) {
		controllerutil.AddFinalizer(graph, finalizer)
		if err := r.Client.Update(ctx, graph); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Set up watch tracking for this reconcile cycle.
	var watcher *watches.GraphWatcher
	walkAttempted := false
	if r.Watcher != nil {
		watcher = r.Watcher.ForGraph(req.NamespacedName)
		defer func() {
			// Commit watches whenever the walk ran, regardless of
			// reconcileErr. A post-walk error (e.g., optimistic lock
			// conflict on status update) should not discard valid watch
			// registrations from the walk.
			watcher.Done(walkAttempted)
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
	dag := state.dag
	plan := dagpkg.NewPlanState(dag)

	// On revision transition, transfer previousAppliedKeys from superseded
	// revisions so the prune phase knows what the old revision applied.
	isRevisionTransition := len(supersededRevisions) > 0
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

	// -----------------------------------------------------------------------
	// 7. Walk the DAG
	// -----------------------------------------------------------------------
	var preWalkGen int64
	if r.SchemaGen != nil {
		preWalkGen = r.SchemaGen.Generation()
	}

	rs := newReconcileScope(graph, watcher)
	walkRes := r.walk(ctx, rs, state, eval, dag, plan)
	walkAttempted = true

	// Post-walk recompile check: if schema generation advanced during
	// the walk (a CRD was created), re-validate compilation to catch
	// child graph type errors within the same cycle.
	if r.SchemaGen != nil && r.SchemaGen.Generation() > preWalkGen && compilationErr == nil {
		if _, _, err := r.compileRevision(ctx, graph.GetNamespace(), activeRevision); err != nil {
			compilationErr = err
			logger.Error(err, "post-walk recompilation detected error")
		}
	}

	appliedKeys := walkRes.keys
	nodeErrors := walkRes.nodeErrors
	var nodeNotes []string
	summary := walkRes.summary

	// Merge walkResult into instanceState for next reconcile.
	state.previousPlanStates = walkRes.plan
	state.previousKeys = walkRes.nodeKeys

	// -----------------------------------------------------------------------
	// 8. Prune removed resources
	// -----------------------------------------------------------------------
	cluster := r.cluster()
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
			appliedSet := r.Watcher.Watches.DeriveAppliedSet(rs.name, rs.namespace)
			for key := range appliedSet {
				allPreviousKeys[key] = true
			}
		}

		// Include the previous reconcile's applied keys to cover the
		// informer lag window.
		for k := range state.previousAppliedKeys {
			allPreviousKeys[k] = true
		}
		// Include keys whose deletion was deferred in the previous reconcile.
		for _, k := range state.deferredPruneKeys {
			allPreviousKeys[k] = true
		}
		// Update the previous key set for the next reconcile.
		state.updateAppliedKeys(appliedKeys)

		// Extract static keys from superseded revisions for resources
		// that may not yet be in the informer cache.
		supersededDAGs := map[string]*dagpkg.DAG{}
		for _, rev := range supersededRevisions {
			if revSpec, err := extractRevisionSpec(rev); err == nil {
				for _, node := range revSpec.Nodes {
					if node.Finalizes != "" {
						continue
					}
					if key := staticResourceKey(node.Identity(), rs.namespace, cluster.scope); key != "" {
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
			candidates := collectPruneCandidates(allPreviousKeys, appliedKeys)
			allDAGs := collectAllDAGs(dag, supersededDAGs)

			pr := cluster.pruneResources(ctx, rs, candidates, appliedKeys, allDAGs, eval, state, true)

			nodeErrors = append(nodeErrors, pr.BlockedReasons...)
			nodeNotes = append(nodeNotes, pr.Notes...)
			if len(pr.BlockedReasons) > 0 {
				summary.HasBlocked = true
			}
			if len(pr.DeferredKeys) > 0 {
				prunePending = true
				state.deferredPruneKeys = make([]string, len(pr.DeferredKeys))
				copy(state.deferredPruneKeys, pr.DeferredKeys)
			} else {
				state.deferredPruneKeys = nil
			}
			if pr.Err != nil {
				logger.Error(pr.Err, "pruning removed resources")
				pruneOK = false
				info := classifyAPIError(pr.Err)
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
	// 9. Update status
	// -----------------------------------------------------------------------
	var topoOrder map[string]any
	if compilationErr == nil && len(state.compiled.ChildTopologies) > 0 {
		nodes := make([]string, len(dag.TopologicalOrder))
		for i, idx := range dag.TopologicalOrder {
			nodes[i] = dag.Nodes[idx].ID
		}
		topoOrder = map[string]any{"nodes": nodes}
		for nodeID, childOrder := range state.compiled.ChildTopologies {
			topoOrder[nodeID] = childOrder
		}
	}

	rstate := &reconcileState{
		compiled:         compilationErr == nil,
		compiledErr:      compilationErr,
		planSummary:      summary,
		nodeErrors:       nodeErrors,
		nodeNotes:        nodeNotes,
		topologicalOrder: topoOrder,
	}
	if err := r.updateStatus(ctx, graph, rstate); err != nil {
		logger.Error(err, "status update")
	}

	allReady := rstate.compiled && !rstate.planSummary.HasPending && !rstate.planSummary.HasNotReady &&
		!rstate.planSummary.HasBlocked && !rstate.planSummary.HasConflict && !rstate.planSummary.HasError && !rstate.planSummary.HasSystemError
	r.gcSupersededRevisions(ctx, activeRevision, supersededRevisions, allReady, pruneOK && !prunePending)

	// -----------------------------------------------------------------------
	// 10. Requeue if not fully ready
	// -----------------------------------------------------------------------

	// Dynamic GVK change needs immediate recompile.
	if walkRes.needsRecompile {
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
