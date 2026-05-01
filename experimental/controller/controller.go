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
	"strings"
	"time"

	"github.com/gobuffalo/flect"
	"github.com/prometheus/client_golang/prometheus"
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
	APIReader      client.Reader                   // direct API server reader — bypasses cache for managed resources
	SchemaResolver resolver.SchemaResolver         // nil = all resource nodes fall back to dyn
	SchemaGen      *compiler.SchemaGeneration      // nil = no generation tracking; never triggers recompilation
	Watcher        *watches.WatchCoordinator       // nil = no dynamic watches (backward compat with existing tests)
	Caches         *graphCaches                    // per-revision compiled expression caches
	Resources      *resourceCache                  // per-resource full object cache
	Scope          GVKScopeResolver                // nil = unknown scope; staticResourceKey falls back to namespace-substitution heuristic
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

	// Observe reconcile duration on return.
	defer func() {
		ReconcileDurationSeconds.With(prometheus.Labels{
			"graph_name":      graph.GetName(),
			"graph_namespace": graph.GetNamespace(),
		}).Observe(time.Since(reconcileStart).Seconds())
	}()

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

	// On revision transition, transfer previous state from the superseded
	// revision's cache for unchanged nodes. The evaluation hash check will
	// then skip unchanged nodes.
	if isRevisionTransition {
		changedNodes := diffRevisionNodes(revisionSpec, supersededRevisions)
		if changedNodes != nil {
			baseline := supersededRevisions[len(supersededRevisions)-1]
			oldKey := baseline.GetNamespace() + "/" + baseline.GetName()
			if oldState := r.Caches.get(oldKey); oldState != nil {
				for _, node := range dag.Nodes {
					if !changedNodes[node.ID] {
						if v, ok := oldState.previousScope[node.ID]; ok {
							state.previousScope[node.ID] = v
						}
						if oldState.previousPlanStates != nil {
							if v, ok := oldState.previousPlanStates.States[node.ID]; ok {
								if state.previousPlanStates == nil {
									state.previousPlanStates = &dagpkg.PlanState{States: make(map[string]dagpkg.NodeState)}
								}
								state.previousPlanStates.States[node.ID] = v
							}
						}
					}
				}
			}
		}
		// Clean up metric series for nodes removed between revisions.
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

	// Drain watch triggers/collection changes so they don't accumulate.
	// The simplified walk evaluates all nodes every cycle.
	if watcher != nil {
		_ = watcher.DrainTriggers()
		_ = watcher.DrainCollectionChanges()
	}

	// -----------------------------------------------------------------------
	// 7. Walk the DAG
	// -----------------------------------------------------------------------
	var preWalkGen int64
	if r.SchemaGen != nil {
		preWalkGen = r.SchemaGen.Generation()
	}

	walkRes := r.walk(ctx, graph, state, eval, dag, plan, watcher)
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
			pruneCandidates := collectPruneCandidates(allPreviousKeys, appliedKeys)
			keyToNodeID, nodeIDToKey := buildKeyMaps(dag, supersededDAGs, graph.GetNamespace(), r.Scope)
			finResult, finErr := r.advanceFinalization(ctx, graph, pruneCandidates, keyToNodeID, nodeIDToKey, collectAllDAGs(dag, supersededDAGs), eval, watcher, state)
			if finErr != nil {
				err = finErr
			} else {
				pruneBlockedReasons = append(pruneBlockedReasons, finResult.BlockedReasons...)
				pruneNotes = append(pruneNotes, finResult.Notes...)
				deferred = append(deferred, finResult.DeferredTargets...)

				// Phase 2: Pure deletion decisions.
				var pruneDeferred []string
				var pruneBR []string
				var pruneN []string
				pruneDeferred, pruneBR, pruneN, err = r.pruneRemovedResources(ctx, graph, allPreviousKeys, appliedKeys, dag, supersededDAGs, finResult)
				deferred = append(deferred, pruneDeferred...)
				pruneBlockedReasons = append(pruneBlockedReasons, pruneBR...)
				pruneNotes = append(pruneNotes, pruneN...)
			}
			nodeErrors = append(nodeErrors, pruneBlockedReasons...)
			nodeNotes = append(nodeNotes, pruneNotes...)
			// Persist prune notes so they're visible for one additional reconcile.
			state.previousPruneNotes = pruneNotes
			if len(pruneBlockedReasons) > 0 {
				summary.HasBlocked = true
			}
			if len(deferred) > 0 {
				prunePending = true
				state.deferredPruneKeys = make([]string, len(deferred))
				copy(state.deferredPruneKeys, deferred)
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

	// Carry forward prune notes from the previous cycle if no new notes were
	// generated. This ensures transient notes (e.g., FinalizerSkipped) are
	// visible for at least one additional reconcile, preventing the status
	// update from being overwritten before the operator can observe it.
	if len(nodeNotes) == 0 && len(state.previousPruneNotes) > 0 {
		nodeNotes = state.previousPruneNotes
		state.previousPruneNotes = nil // consumed — don't carry forward again
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
		PlanSummary:      summary,
		nodeErrors:       nodeErrors,
		nodeNotes:        nodeNotes,
		topologicalOrder: topoOrder,
	}
	if err := r.updateStatus(ctx, graph, rstate); err != nil {
		logger.Error(err, "status update")
	}

	allReady := rstate.compiled && !rstate.PlanSummary.HasPending && !rstate.PlanSummary.HasNotReady &&
		!rstate.PlanSummary.HasBlocked && !rstate.PlanSummary.HasConflict && !rstate.PlanSummary.HasError && !rstate.PlanSummary.HasSystemError
	r.updateRevisionStatus(ctx, activeRevision, supersededRevisions, allReady, pruneOK && !prunePending)

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
