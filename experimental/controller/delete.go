// delete.go implements Graph deletion — ordered teardown, finalization,
// and patch field release. Separated from controller.go because deletion
// is a distinct lifecycle protocol with its own invariants (ordering,
// finalizers, orphan semantics) and does not share walk state with the
// normal reconcile path.
//
// Per 005-reconciliation.md: "When a Graph is deleted, all nodes become
// prune candidates; full prune algorithm runs." The actual deletion logic
// lives in pruneResources (prune.go); this file collects the managed key
// set and delegates.
package graphcontroller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	dagpkg "github.com/ellistarn/kro/experimental/controller/dag"
	graphpkg "github.com/ellistarn/kro/experimental/controller/graph"
)

func (r *GraphReconciler) reconcileDelete(ctx context.Context, graph *unstructured.Unstructured) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	teardownStart := time.Now()
	logger.V(1).Info("teardown started")
	cluster := r.cluster()

	// List revisions early — needed for key extraction and finalization.
	revisions, listErr := listRevisions(ctx, r.Client, graph.GetName(), graph.GetNamespace())
	if listErr != nil {
		logger.Error(listErr, "listing revisions during teardown; proceeding with watch cache and live spec fallbacks")
	}

	// -----------------------------------------------------------------------
	// Collect all managed resource keys from all revisions for this Graph.
	// Keys come from three sources:
	// 1. Revision specs — static extraction
	// 2. Watch cache — informer stores scanned for identity labels
	// 3. Label selector — dynamic resources (forEach, CEL names)
	// -----------------------------------------------------------------------
	allKeys := map[string]Applied{}

	// Build static keys from revision specs.
	staticKeys := map[string]bool{}
	for _, rev := range revisions {
		spec, err := extractRevisionSpec(rev)
		if err != nil {
			continue
		}
		for _, node := range spec.Nodes {
			if node.Identity() == nil {
				continue
			}
			if key := staticResourceKey(node.Identity(), graph.GetNamespace(), cluster.scope); key != "" {
				staticKeys[key] = true
			}
			nodeType := node.Type()
			if nodeType == graphpkg.NodeTypeRef || nodeType == graphpkg.NodeTypeWatch {
				continue
			}
			if node.Finalizes != "" {
				continue
			}
			if key := staticResourceKey(node.Identity(), graph.GetNamespace(), cluster.scope); key != "" {
				allKeys[key] = Applied{
					Key:       key,
					NodeType:  nodeType,
					HasStatus: templateHasStatus(node.Payload()),
				}
			}
		}
	}

	// Derive applied set from watch cache (before releasing watch state).
	normalizedToStatic := make(map[string]string, len(staticKeys))
	for sk := range staticKeys {
		normalizedToStatic[strings.ToLower(sk)] = sk
	}
	if r.Watcher != nil {
		appliedSet := r.Watcher.DeriveAppliedSet(graph.GetName(), graph.GetNamespace())
		for key, entry := range appliedSet {
			if corrected, ok := normalizedToStatic[strings.ToLower(key)]; ok {
				key = corrected
			}
			// Derive HasStatus from revision specs when available.
			hasStatus := false
			if existing, ok := allKeys[key]; ok {
				hasStatus = existing.HasStatus
			}
			allKeys[key] = Applied{
				Key:       key,
				NodeType:  entry.NodeType,
				HasStatus: hasStatus,
			}
		}
	}

	// Release watch state now that keys have been collected.
	if r.Watcher != nil {
		r.Watcher.RemoveGraph(types.NamespacedName{Name: graph.GetName(), Namespace: graph.GetNamespace()})
	}

	// Include dynamically-named resources found by label selector.
	rs := newReconcileScope(graph, nil) // watcher is handled specially in teardown
	dynamicKeys, findErr := cluster.findManagedResourceKeys(ctx, rs)
	if findErr != nil {
		logger.Error(findErr, "finding dynamically-named resources during teardown; forEach children may be orphaned if not in watch cache")
	}
	for _, a := range dynamicKeys {
		if _, exists := allKeys[a.Key]; !exists {
			allKeys[a.Key] = a
		}
	}

	// Convert to candidate slice.
	candidates := make([]Applied, 0, len(allKeys))
	for _, a := range allKeys {
		candidates = append(candidates, a)
	}

	// -----------------------------------------------------------------------
	// Compile the active revision for ordering + finalization.
	// -----------------------------------------------------------------------
	var teardownDAGs []*dagpkg.DAG
	var teardownEval *evaluator
	var teardownCompileErr error
	if len(revisions) > 0 {
		active := revisions[len(revisions)-1]
		if _, istate, compileErr := r.compileRevision(ctx, graph.GetNamespace(), active); compileErr == nil {
			teardownDAGs = []*dagpkg.DAG{istate.dag}
			teardownEval = newEvaluator(istate)
			teardownEval.effectiveGeneration = revisionGeneration(active)
		} else {
			teardownCompileErr = compileErr
			logger.Error(compileErr, "active revision failed to compile during teardown; falling back to live Graph spec",
				"revision", active.GetName())
		}
	}

	// Ensure ordering — never degrade to unordered deletion.
	if len(teardownDAGs) == 0 {
		graphSpec, err := graphpkg.ExtractGraphSpec(graph.Object)
		if err != nil {
			logger.Error(err, "cannot determine deletion order, requeueing")
			return ctrl.Result{RequeueAfter: systemErrorRequeueInterval}, nil
		}
		dag, err := dagpkg.BuildDAG(graphSpec.Nodes, nil, nil)
		if err != nil {
			logger.Error(err, "cannot determine deletion order, requeueing")
			return ctrl.Result{RequeueAfter: systemErrorRequeueInterval}, nil
		}
		teardownDAGs = []*dagpkg.DAG{dag}
	}

	// Build a minimal instanceState for advanceFinalization.
	teardownState := &instanceState{
		activeFinalization: map[string]*finalizationEntry{},
	}

	// -----------------------------------------------------------------------
	// Delegate to pruneResources: all keys are candidates, currentKeys empty.
	// Teardown skips identity-label verification (checkIdentityLabels=false).
	// -----------------------------------------------------------------------
	pr := cluster.pruneResources(ctx, rs, candidates, nil, teardownDAGs, teardownEval, teardownState, false)

	// Verify deleted resources are gone — only check keys with pruneDeleted outcome.
	for _, a := range candidates {
		if a.Key == "" {
			continue
		}
		// Only verify template resources that were actually deleted.
		outcome, hasOutcome := pr.Outcomes[a.Key]
		if a.NodeType == graphpkg.NodeTypePatch {
			continue // patches are released, not deleted
		}
		if hasOutcome && outcome != pruneDeleted {
			continue // not deleted — skip verification
		}
		if !hasOutcome {
			// No outcome recorded — may have been deleted. Verify.
		}
		check, _, ok := unstructuredFromKey(a.Key)
		if !ok {
			continue
		}
		if err := cluster.reader.Get(ctx, client.ObjectKeyFromObject(check), check); err == nil {
			logger.V(1).Info("waiting for managed resource to be deleted", "key", a.Key)
			return ctrl.Result{RequeueAfter: 500 * time.Millisecond}, nil
		}
	}

	// If any resource deletion was blocked, surface each distinct reason.
	if len(pr.BlockedReasons) > 0 {
		logger.Info("teardown blocked", "reasons", pr.BlockedReasons)
		nodeErrors := append([]string{}, pr.BlockedReasons...)
		if teardownCompileErr != nil {
			nodeErrors = append(nodeErrors,
				fmt.Sprintf("active revision compile failed: %s", teardownCompileErr))
		}
		if statusErr := r.updateStatus(ctx, graph, &reconcileState{
			compiled:    true,
			planSummary: dagpkg.PlanSummary{HasBlocked: true},
			nodeErrors:  nodeErrors,
			nodeNotes:   pr.Notes,
		}); statusErr != nil {
			logger.Error(statusErr, "updating status during teardown")
		}
		return ctrl.Result{RequeueAfter: systemErrorRequeueInterval}, nil
	}
	// Log FinalizerSkipped notes.
	for _, note := range pr.Notes {
		logger.Info("teardown note", "note", note)
	}

	if pr.Err != nil {
		return ctrl.Result{}, pr.Err
	}

	// Delete all GraphRevisions.
	for _, rev := range revisions {
		if err := deleteRevision(ctx, r.Client, rev); err != nil {
			if client.IgnoreNotFound(err) != nil {
				logger.Error(err, "deleting revision", "revision", rev.GetName())
			}
		} else {
			logger.Info("deleted revision", "revision", rev.GetName())
		}
	}

	// Clean up expression caches.
	for _, rev := range revisions {
		r.Caches.remove(rev.GetNamespace() + "/" + rev.GetName())
	}

	controllerutil.RemoveFinalizer(graph, finalizer)
	if err := r.Client.Update(ctx, graph); err != nil {
		return ctrl.Result{}, err
	}
	logger.V(1).Info("teardown complete", "duration", time.Since(teardownStart))
	return ctrl.Result{}, nil
}

// ---------------------------------------------------------------------------
// Owner lifecycle
// ---------------------------------------------------------------------------

func (r *GraphReconciler) ownerDeleting(ctx context.Context, graph *unstructured.Unstructured) bool {
	reader := r.cluster().reader
	for _, ref := range graph.GetOwnerReferences() {
		gv, err := schema.ParseGroupVersion(ref.APIVersion)
		if err != nil {
			continue
		}
		owner := &unstructured.Unstructured{}
		owner.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   gv.Group,
			Version: gv.Version,
			Kind:    ref.Kind,
		})
		if err := reader.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: graph.GetNamespace()}, owner); err != nil {
			continue
		}
		if !owner.GetDeletionTimestamp().IsZero() {
			return true
		}
	}
	return false
}
