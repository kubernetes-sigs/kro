// delete.go implements Graph deletion — ordered teardown, finalization,
// and patch field release. Separated from controller.go because deletion
// is a distinct lifecycle protocol with its own invariants (ordering,
// finalizers, orphan semantics) and does not share walk state with the
// normal reconcile path.
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

	// List revisions early — needed for key extraction and finalization.
	// Cache eviction is deferred to after finalization (which calls
	// compileRevision and would re-add entries if evicted too early).
	// Errors are logged but do not block teardown: the watch cache
	// (deriveAppliedSet) and live Graph spec provide fallback paths for
	// key discovery and ordering. Blocking here would leave the Graph
	// with a finalizer that can never clear if the list call is
	// permanently failing.
	revisions, listErr := listRevisions(ctx, r.Client, graph.GetName(), graph.GetNamespace())
	if listErr != nil {
		logger.Error(listErr, "listing revisions during teardown; proceeding with watch cache and live spec fallbacks")
	}

	// Clean up all metric time series for this Graph via partial match.
	// Covers every node that ever emitted a metric, even if revision specs
	// are no longer parseable.
	deleteGraphMetricsForGraph(graph.GetName(), graph.GetNamespace())

	// Collect all managed resource keys from all revisions for this Graph.
	// Keys come from two sources:
	// 1. Watch cache — informer stores scanned for identity labels
	// 2. Static spec extraction (fallback for resources not in cache)
	ownKeys := map[string]bool{}
	patchKeys := map[string]bool{} // key → hasStatus encoded in the key

	// Build a map from resource keys to hasStatus by scanning revision specs.
	// This recovers the +status suffix that the watch cache cannot provide.
	// Per 003-ownership.md § Status Subresource: "Releases only target the
	// subresources the template actually applied to."
	patchStatusMap := map[string]bool{} // resource key → hasStatus
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
					patchStatusMap[key] = true
				}
			}
			// Also extract static template keys (merged from separate loop).
			nodeType := node.Type()
			if nodeType == graphpkg.NodeTypeRef || nodeType == graphpkg.NodeTypeWatch {
				continue // read-only
			}
			if node.Finalizes != "" {
				continue // dormant during normal operation
			}
			if key := staticResourceKey(node.Identity(), graph.GetNamespace(), r.Scope); key != "" {
				ownKeys[key] = true
			}
		}
	}

	// Derive applied set from watch cache if available.
	// Must run BEFORE removeGraph — removeGraph releases watch ownership,
	// which can stop informers if this Graph is the sole watcher of a GVR.
	// deriveAppliedSet needs those informers to scan for identity labels on
	// Patch targets.
	//
	// deriveAppliedSet keys may have incorrect Kind casing (metadata informers
	// don't always populate TypeMeta, falling back to singularize which doesn't
	// preserve CamelCase). Build a normalizedKey→staticKey map to correct this.
	normalizedToStatic := make(map[string]string, len(staticKeys))
	for sk := range staticKeys {
		normalizedToStatic[strings.ToLower(sk)] = sk
	}
	if r.Watcher != nil {
		appliedSet := r.Watcher.Watches.DeriveAppliedSet(graph.GetName(), graph.GetNamespace())
		for key, entry := range appliedSet {
			// Correct the key's Kind casing by matching against static keys.
			if corrected, ok := normalizedToStatic[strings.ToLower(key)]; ok {
				key = corrected
			}
			if entry.NodeType == graphpkg.NodeTypePatch {
				// For patch keys, encode hasStatus from revision spec scan.
				cKey := patchKeyPrefix + key
				if patchStatusMap[key] {
					cKey += patchStatusSuffix
				}
				patchKeys[cKey] = true
			} else {
				ownKeys[key] = true
			}
		}
	}

	// Release watch state now that patch keys have been collected.
	if r.Watcher != nil {
		r.Watcher.RemoveGraph(types.NamespacedName{Name: graph.GetName(), Namespace: graph.GetNamespace()})
	}

	fieldOwner := graphFieldOwner(graph)

	// Convert template keys to slice for ordered deletion.
	var keys []string
	for k := range ownKeys {
		keys = append(keys, k)
	}

	// Also include dynamically-named resources found by label selector.
	// This catches forEach-stamped resources that aren't in the static spec.
	// Errors are logged but do not block teardown — the static keys and
	// deriveAppliedSet already cover statically-named resources. Only
	// dynamically-named resources (forEach children, CEL-computed names)
	// risk being orphaned if this call fails, and the watch cache usually
	// covers those too.
	dynamicKeys, findErr := r.findManagedResourceKeys(ctx, graph)
	if findErr != nil {
		logger.Error(findErr, "finding dynamically-named resources during teardown; forEach children may be orphaned if not in watch cache")
	}
	for _, k := range dynamicKeys {
		if !ownKeys[k] {
			keys = append(keys, k)
		}
	}

	// Compile the active revision (if available) to get the DAG for
	// finalizer relationships, deletion ordering, and an evaluator for
	// template rendering. listRevisions returns ascending by generation,
	// so the active revision is the last element. Per
	// 005-reconciliation.md § Teardown: "Ordering comes from the
	// active revision's DAG." Compiled BEFORE deletionOrder so the
	// already-compiled DAG can drive ordering rather than re-parsing the
	// live Graph spec.
	var teardownDAG *dagpkg.DAG
	var teardownEval *evaluator
	var teardownCompileErr error
	if len(revisions) > 0 {
		active := revisions[len(revisions)-1]
		if _, state, compileErr := r.compileRevision(ctx, graph.GetNamespace(), active); compileErr == nil {
			teardownDAG = state.dag
			teardownEval = newEvaluator(state)
			// During teardown, the effective generation is the active
			// revision's generation — the graph's live generation is
			// irrelevant because we're not applying new state, just
			// stamping any finalizer resources we need to create.
			teardownEval.effectiveGeneration = revisionGeneration(active)
		} else {
			// Per 005-reconciliation.md § Teardown, ordering comes from
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
		// Per the design (005-reconciliation): teardown is blocked until
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
	// Per 005-reconciliation.md § Finalization, these three causes have
	// different remediation actions; collapsing them into one message sends
	// operators chasing the wrong problem.
	var teardownBlockedReasons []string
	// teardownNotes accumulates informational notes (e.g., FinalizerSkipped)
	// that don't block teardown but are operationally useful. Per
	// 005-reconciliation.md § Finalization: "The Graph's status surfaces
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

		// Shared ownership + field manager preflight check.
		// Teardown skips identity-label verification because keys are
		// collected from revision specs and the watch cache — they are
		// already known to belong to this Graph.
		pf := r.deletePreflight(ctx, key, graph, false)
		switch pf.Outcome {
		case deleteSkipParseFailed:
			continue
		case deleteNotFound:
			// Target absent is not a teardown block — the design
			// classifies it as FinalizerSkipped when a finalizer was
			// declared, or a silent no-op otherwise.
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
			continue
		case deleteNotOwned:
			continue
		case deleteBlockedByFieldManagers:
			logger.Info("teardown blocked: resource has other field managers",
				"key", key, "blockers", pf.Blockers)
			teardownBlockedReasons = append(teardownBlockedReasons,
				formatBlockedReason(key, pf.Blockers))
			continue
		}

		// deleteReady: resource exists, is ours, no third-party field managers.
		// Run finalization before issuing the delete.
		var finKeys []string
		if teardownDAG != nil && teardownEval != nil {
			nodeID := keyToNodeID[key]
			if finalizerNodeIDs, ok := teardownDAG.Finalizers[nodeID]; ok && len(finalizerNodeIDs) > 0 {
				ready, fk, finErr := r.runFinalization(ctx, graph, pf.Obj, nodeID, finalizerNodeIDs, teardownDAG, teardownEval, nil)
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
		if err := r.Client.Delete(ctx, pf.Obj); err != nil {
			if client.IgnoreNotFound(err) != nil {
				return ctrl.Result{}, fmt.Errorf("deleting managed resource %s: %w", key, err)
			}
		} else {
			logger.Info("deleted managed resource", "key", key)

			// Clean up finalizer resources after the target is deleted.
			if teardownDAG != nil {
				nodeID := keyToNodeID[key]
				if finalizerNodeIDs, ok := teardownDAG.Finalizers[nodeID]; ok {
					// Collect static-name finalizer keys for cleanup.
					var staticFinKeys []string
					for _, finNodeID := range finalizerNodeIDs {
						if finIdx, ok2 := teardownDAG.Index[finNodeID]; ok2 {
							finNode := &teardownDAG.Nodes[finIdx]
							if finNode.Identity() != nil && finNode.ForEach == nil {
								if fk := staticResourceKey(finNode.Identity(), graph.GetNamespace(), r.Scope); fk != "" {
									staticFinKeys = append(staticFinKeys, fk)
								}
							}
						}
					}
					r.deleteByKeys(ctx, staticFinKeys)
					// Clean up forEach finalizer children via their tracked keys.
					r.deleteByKeys(ctx, finKeys)
				}
			}
		}
	}

	// Pass 2: Verify managed resources that we actually deleted are gone.
	// Only check resources that had our apply hash — others (e.g., conflicted
	// resources that were never successfully applied) are not our responsibility.
	for key := range deletedKeys {
		check, _, ok := unstructuredFromKey(key)
		if !ok {
			continue
		}
		if err := r.apiReader().Get(ctx, client.ObjectKeyFromObject(check), check); err == nil {
			logger.V(1).Info("waiting for managed resource to be deleted", "key", key)
			return ctrl.Result{RequeueAfter: 500 * time.Millisecond}, nil
		}
	}

	// If any resource deletion was blocked, surface each distinct reason so
	// operators can triage. Per 005-reconciliation.md § Teardown:
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
			PlanSummary: dagpkg.PlanSummary{HasBlocked: true},
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

	// Release Patch fields via SSA release apply. Runs AFTER template
	// deletion so that lifecycle patches (e.g., a finalizer on an owner
	// object) are released only after managed resources are gone.
	// Per 003-ownership: Patch never deletes — it releases field
	// ownership so the actual owner retains the resource.
	for key := range patchKeys {
		resKey, hasStatus := parsePatchKey(key)
		if resKey == "" {
			continue
		}
		gvk, nn := parseResourceKey(resKey)
		if gvk.Kind == "" {
			continue
		}
		if _, err := releaseApply(ctx, r.Client, gvk, nn.Namespace, nn.Name, fieldOwner, hasStatus); err != nil {
			logger.Error(err, "releasing patch fields during teardown", "key", resKey)
		} else {
			logger.Info("released patch fields during teardown", "key", resKey)
		}
	}

	// Pass 3: Delete all GraphRevisions.
	for _, rev := range revisions {
		if err := deleteRevision(ctx, r.Client, rev); err != nil {
			if client.IgnoreNotFound(err) != nil {
				logger.Error(err, "deleting revision", "revision", rev.GetName())
			}
		} else {
			logger.Info("deleted revision", "revision", rev.GetName())
		}
	}

	// Clean up expression caches AFTER finalization and revision deletion.
	// This must happen after compileRevision calls in the teardown phase,
	// which re-populate the cache to get the DAG and evaluator for
	// finalization. Evicting earlier would be immediately undone.
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
//
// When a Graph has ownerReferences, the controller participates in the
// owner's deletion lifecycle. If any owner enters Terminating (has a
// deletionTimestamp), the controller self-deletes the Graph to initiate
// ordered teardown. This bridges the gap between K8s GC cascade (which
// requires the owner to be fully gone) and the blocking finalizer pattern
// (where a patch node holds the owner in Terminating).

// ownerDeleting returns true if any of the Graph's owners has a non-zero
// deletionTimestamp. This triggers self-deletion so the Graph can tear
// down managed resources via its normal reconcileDelete path.
func (r *GraphReconciler) ownerDeleting(ctx context.Context, graph *unstructured.Unstructured) bool {
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
		if err := r.apiReader().Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: graph.GetNamespace()}, owner); err != nil {
			continue // Owner gone or unreachable — not our trigger
		}
		if !owner.GetDeletionTimestamp().IsZero() {
			return true
		}
	}
	return false
}
