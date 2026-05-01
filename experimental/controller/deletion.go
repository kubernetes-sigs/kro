// deletion.go contains the shared deletion primitives used by both the teardown
// path (delete.go) and the prune path (prune.go). Both paths perform the same
// per-resource preflight sequence: GET, ownership check, field manager check.
// Extracting this sequence avoids duplicating the ownership and safety logic.
//
// The preflight is separated from the actual delete because teardown runs
// finalization between the check and the delete, while prune delegates
// finalization to advanceFinalization before the prune walk.
package graphcontroller

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/log"

	graphpkg "github.com/ellistarn/kro/experimental/controller/graph"
)

// deletePreflightOutcome describes the result of the preflight check for a
// single resource key.
type deletePreflightOutcome int

const (
	// deleteReady — the resource exists, is owned by this Graph, and has no
	// third-party field managers. Caller may proceed with deletion.
	deleteReady deletePreflightOutcome = iota
	// deleteNotFound — the resource does not exist (already gone).
	deleteNotFound
	// deleteNotOwned — the resource exists but is not owned by this Graph
	// (missing identity label or apply hash annotation).
	deleteNotOwned
	// deleteBlockedByFieldManagers — the resource has third-party field
	// managers; deletion should be deferred.
	deleteBlockedByFieldManagers
	// deleteSkipParseFailed — the resource key could not be parsed.
	deleteSkipParseFailed
)

// deletePreflightResult captures the outcome and metadata from a preflight
// check so the caller can make follow-up decisions (finalization, delete, etc.).
type deletePreflightResult struct {
	Outcome  deletePreflightOutcome
	Obj      *unstructured.Unstructured // live object from GET (nil if not found or parse failed)
	Blockers []string                   // third-party field manager names (when blocked)
}

// deletePreflight performs the shared ownership and safety checks for a
// resource key:
//
//  1. Parse the resource key into an unstructured stub.
//  2. GET from the API server (authoritative read, not cache).
//  3. Verify ownership: apply hash annotation, and optionally identity labels.
//  4. Check for third-party field managers that block deletion.
//
// The caller handles all context-specific logic (finalization, patch release,
// deferred key tracking, FinalizerSkipped notes) and the actual delete call
// based on the returned result.
//
// checkIdentityLabels controls whether the identity-label check is performed.
// The teardown path (delete.go) skips it because it collects keys from revision
// specs and the watch cache — it already knows they belong to this Graph. The
// prune path (prune.go) enables it because previous-applied-key sets can include
// stale entries from other Graphs after label changes.
func (r *GraphReconciler) deletePreflight(
	ctx context.Context,
	key string,
	graph *unstructured.Unstructured,
	checkIdentityLabels bool,
) deletePreflightResult {
	logger := log.FromContext(ctx)

	obj, nn, ok := unstructuredFromKey(key)
	if !ok {
		return deletePreflightResult{Outcome: deleteSkipParseFailed}
	}

	// Direct API server read for authoritative existence/ownership data.
	if err := r.apiReader().Get(ctx, nn, obj); err != nil {
		return deletePreflightResult{Outcome: deleteNotFound}
	}

	// Ownership gate: identity labels.
	if checkIdentityLabels {
		objLabels := obj.GetLabels()
		hasOurLabel := false
		if objLabels != nil {
			for k := range objLabels {
				if graphpkg.IsGraphIdentityLabel(k, graph.GetName(), graph.GetNamespace()) {
					hasOurLabel = true
					break
				}
			}
		}
		if !hasOurLabel {
			return deletePreflightResult{Outcome: deleteNotOwned, Obj: obj}
		}
	}

	// Ownership gate: apply hash annotation.
	objAnnotations := obj.GetAnnotations()
	if objAnnotations == nil || objAnnotations[applyHashAnnotation] == "" {
		logger.V(1).Info("skipping delete for resource without apply hash (never successfully applied)", "key", key)
		return deletePreflightResult{Outcome: deleteNotOwned, Obj: obj}
	}

	// Contributor-aware deletion: check managedFields for other field
	// managers before deleting. If present, deletion is blocked — the
	// finalizer holds until the other manager releases.
	ownManager := string(graphFieldOwner(graph))
	if blockers := thirdPartyFieldManagers(obj, ownManager); len(blockers) > 0 {
		return deletePreflightResult{
			Outcome:  deleteBlockedByFieldManagers,
			Obj:      obj,
			Blockers: blockers,
		}
	}

	return deletePreflightResult{Outcome: deleteReady, Obj: obj}
}

// formatBlockedReason builds the standard TeardownBlocked message for
// third-party field managers. Used by both teardown and prune paths.
func formatBlockedReason(key string, blockers []string) string {
	return fmt.Sprintf("TeardownBlocked: %s (third-party field managers: %s)",
		key, strings.Join(blockers, ", "))
}
