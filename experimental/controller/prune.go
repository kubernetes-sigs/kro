// prune.go computes and executes the prune walk — removing resources no longer
// in the applied set. Prune candidates are the diff between previous and current
// applied keys. Deletion follows reverse topological order from the DAG.
//
// pruneResources is the single code path for both:
//   - Normal prune: candidates = keys in previous but not current
//   - Teardown: candidates = all managed keys, currentKeys = empty
//
// The prune walk handles: template deletion, patch field release, contributor-
// aware blocking (third-party field managers), finalization sequencing, and
// dynamic resource discovery for forEach/CEL-named resources.
package graphcontroller

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	dagpkg "github.com/ellistarn/kro/experimental/controller/dag"
	graphpkg "github.com/ellistarn/kro/experimental/controller/graph"
)

// pruneOutcome describes what happened to a single prune candidate.
type pruneOutcome int

const (
	pruneDeleted  pruneOutcome = iota // template resource deleted
	pruneReleased                     // patch fields released
	pruneBlocked                      // third-party managers block deletion
	pruneDeferred                     // finalization not ready
	pruneSkipped                      // not owned or not found
)

// pruneResult carries the output of pruneResources to the caller.
type pruneResult struct {
	Outcomes       map[string]pruneOutcome // key → what happened
	BlockedReasons []string                // TeardownBlocked messages
	Notes          []string                // informational (FinalizerSkipped)
	Err            error                   // hard API error
}

// pruneResources handles both normal prune (partial candidates) and teardown
// (all keys are candidates). Returns a pruneResult with outcomes, blocked
// reasons, notes, and errors.
//
// checkIdentityLabels controls whether the identity-label check is performed
// in deletePreflight:
//   - Teardown (false): keys come from revision specs + watch cache — trusted.
//   - Prune (true): previous-applied-key sets can include stale entries.
func (c *clusterAccess) pruneResources(
	ctx context.Context,
	rs *reconcileScope,
	candidates []Applied,
	currentKeys map[string]Applied,
	dags []*dagpkg.DAG,
	eval *evaluator,
	state *instanceState,
	checkIdentityLabels bool,
) *pruneResult {
	logger := log.FromContext(ctx)
	result := &pruneResult{
		Outcomes: map[string]pruneOutcome{},
	}

	if len(candidates) == 0 {
		return result
	}

	// Build resource-key-to-node-ID maps from all DAGs.
	keyToNodeID := map[string]string{}
	nodeIDToKey := map[string]string{}
	for _, d := range dags {
		for _, node := range d.Nodes {
			if node.Identity() != nil {
				if rk := staticResourceKey(node.Identity(), rs.namespace, c.scope); rk != "" {
					keyToNodeID[rk] = node.ID
					nodeIDToKey[node.ID] = rk
				}
			}
		}
	}

	// Phase 1: Advance finalization state machines for all candidates that
	// have finalizer nodes. This produces completedTargets (safe to delete),
	// protectedKeys (must not prune), and child cleanup info.
	if state.activeFinalization == nil {
		state.activeFinalization = map[string]*finalizationEntry{}
	}

	// Build candidate key slice for finalization (plain resource keys).
	candidateKeys := make([]string, len(candidates))
	for i, c := range candidates {
		candidateKeys[i] = c.Key
	}

	finResult, finErr := c.advanceFinalization(ctx, rs, candidateKeys, keyToNodeID, nodeIDToKey, dags, eval, state)
	if finErr != nil {
		result.Err = finErr
		return result
	}
	result.BlockedReasons = append(result.BlockedReasons, finResult.BlockedReasons...)
	result.Notes = append(result.Notes, finResult.Notes...)
	for _, dt := range finResult.DeferredTargets {
		result.Outcomes[dt] = pruneDeferred
	}

	// Phase 2: Order candidates for deletion (reverse topological).
	ordered := pruneOrderApplied(candidates, dags, rs.namespace, c.scope)

	fieldOwner := graphFieldOwner(rs.graph)

	// deferredDeletes collects finalizer resource keys whose targets were
	// successfully deleted in this walk. Processed after the walk completes.
	var deferredDeletes []string

	for _, candidate := range ordered {
		key := candidate.Key
		if key == "" {
			continue
		}
		// Skip keys that are in the current applied set.
		if _, inCurrent := currentKeys[key]; inCurrent {
			continue
		}

		// Defer deletion of resources protected by in-flight finalization.
		if finResult.ProtectedKeys[key] {
			logger.Info("prune deferred: resource is protected by in-flight finalization",
				"key", key)
			result.Outcomes[key] = pruneDeferred
			continue
		}

		// Dispatch based on NodeType.
		switch candidate.NodeType {
		case graphpkg.NodeTypePatch:
			// Patch keys use release apply (release fields), not delete.
			gvk, nn := parseResourceKey(key)
			if gvk.Kind == "" {
				result.Outcomes[key] = pruneSkipped
				continue
			}
			if _, err := releaseApply(ctx, c.client, gvk, nn.Namespace, nn.Name, fieldOwner, candidate.HasStatus); err != nil {
				logger.Error(err, "releasing contribution fields", "key", key)
			} else {
				logger.Info("released contribution fields", "key", key)
			}
			result.Outcomes[key] = pruneReleased
			continue

		default:
			// Template keys: shared ownership + field manager preflight check.
			pf := c.deletePreflight(ctx, key, rs, checkIdentityLabels)
			switch pf.Outcome {
			case deleteSkipParseFailed:
				result.Outcomes[key] = pruneSkipped
				continue
			case deleteNotFound:
				result.Outcomes[key] = pruneSkipped
				// Target absent — clean up finalization children if mid-finalization.
				if childKeys, ok := finResult.ChildKeysToCleanup[key]; ok {
					deferredDeletes = append(deferredDeletes, childKeys...)
				}
				// Emit FinalizerSkipped note if this target had finalizers.
				nodeID := keyToNodeID[key]
				if nodeID != "" {
					for _, d := range dags {
						if finalizerNodeIDs, ok := d.Finalizers[nodeID]; ok && len(finalizerNodeIDs) > 0 {
							logger.Info("finalization skipped: target resource does not exist",
								"key", key, "finalizers", finalizerNodeIDs)
							result.Notes = append(result.Notes,
								fmt.Sprintf("FinalizerSkipped: %s (target absent)", key))
							break
						}
					}
				}
				continue
			case deleteNotOwned:
				result.Outcomes[key] = pruneSkipped
				continue
			case deleteBlockedByFieldManagers:
				logger.Info("prune blocked: resource has other field managers",
					"key", key, "blockers", pf.Blockers)
				result.BlockedReasons = append(result.BlockedReasons,
					formatBlockedReason(key, pf.Blockers))
				result.Outcomes[key] = pruneBlocked
				continue
			}

			// deleteReady: resource exists, is ours, no third-party field managers.
			// Finalization check: if this target has finalizers, only delete if
			// advanceFinalization marked it complete.
			nodeID := keyToNodeID[key]
			hasFinalizers := false
			for _, d := range dags {
				if fins, ok := d.Finalizers[nodeID]; ok && len(fins) > 0 {
					hasFinalizers = true
					break
				}
			}
			if hasFinalizers && !finResult.CompletedTargets[key] {
				continue // finalization not complete — already deferred by advanceFinalization
			}

			if err := c.client.Delete(ctx, pf.Obj); err != nil {
				if client.IgnoreNotFound(err) != nil {
					result.Err = fmt.Errorf("pruning %s: %w", key, err)
					return result
				}
			} else {
				logger.Info("pruned resource", "key", key)
				result.Outcomes[key] = pruneDeleted
				// Clean up finalization children after the target is deleted.
				if childKeys, ok := finResult.ChildKeysToCleanup[key]; ok {
					deferredDeletes = append(deferredDeletes, childKeys...)
				}
			}
		}
	}

	// Clean up finalization children whose targets were successfully deleted.
	c.deleteByKeys(ctx, deferredDeletes)

	return result
}

// collectPruneCandidates returns Applied entries from prev whose Key is not in curr.
func collectPruneCandidates(prev map[string]Applied, curr map[string]Applied) []Applied {
	var candidates []Applied
	for key, a := range prev {
		if _, ok := curr[key]; !ok {
			candidates = append(candidates, a)
		}
	}
	return candidates
}

// collectDeferredKeys extracts Applied entries from allPrevious whose outcome
// is pruneDeferred or pruneBlocked (need to be retried next reconcile).
func collectDeferredKeys(outcomes map[string]pruneOutcome, allPrevious map[string]Applied) []Applied {
	var deferred []Applied
	for key, outcome := range outcomes {
		if outcome == pruneDeferred || outcome == pruneBlocked {
			if a, ok := allPrevious[key]; ok {
				deferred = append(deferred, a)
			}
		}
	}
	return deferred
}

// collectAllDAGs returns the active DAG plus all superseded DAGs.
func collectAllDAGs(dag *dagpkg.DAG, supersededDAGs map[string]*dagpkg.DAG) []*dagpkg.DAG {
	allDAGs := []*dagpkg.DAG{}
	if dag != nil {
		allDAGs = append(allDAGs, dag)
	}
	for _, d := range supersededDAGs {
		allDAGs = append(allDAGs, d)
	}
	return allDAGs
}

// findManagedResourceKeys discovers dynamically-named resources (forEach, CEL
// names) by listing resources with our graph-name label. Returns Applied entries.
//
// The GVK list is derived from the revision specs — every resource template
// declares its apiVersion and kind, so we know exactly which types to scan.
// This avoids a hardcoded GVK list that would silently miss new resource types.
func (c *clusterAccess) findManagedResourceKeys(ctx context.Context, rs *reconcileScope) ([]Applied, error) {
	// Collect unique GVKs from all revisions for this Graph.
	revisions, err := listRevisions(ctx, c.client, rs.name, rs.namespace)
	if err != nil {
		return nil, err
	}

	gvkSet := map[schema.GroupVersionKind]bool{}
	for _, rev := range revisions {
		spec, err := extractRevisionSpec(rev)
		if err != nil {
			continue
		}
		for _, node := range spec.Nodes {
			if node.Identity() == nil {
				continue
			}
			nodeType := node.Type()
			if nodeType == graphpkg.NodeTypeRef || nodeType == graphpkg.NodeTypeWatch {
				continue // read-only references don't create resources
			}
			id := node.Identity()
			gvk := graphpkg.GVKFromMap(id)
			if gvk.Kind == "" {
				continue
			}
			gvkSet[gvk] = true
		}
	}

	var keys []Applied
	suffix := graphpkg.GraphLabelSuffix(rs.name, rs.namespace)
	for gvk := range gvkSet {
		list := &unstructured.UnstructuredList{}
		listGVK := gvk
		listGVK.Kind = gvk.Kind + "List"
		list.SetGroupVersionKind(listGVK)

		// Cannot use label selector with DNS subdomain keys — list all and
		// filter client-side. This only runs during teardown (not hot path).
		if err := c.reader.List(ctx, list, &client.ListOptions{
			Namespace: rs.namespace,
		}); err != nil {
			continue // skip GVKs we can't list (e.g., CRD deleted)
		}

		for _, item := range list.Items {
			itemLabels := item.GetLabels()
			for labelKey, labelValue := range itemLabels {
				if strings.HasSuffix(labelKey, suffix) {
					rk := resourceKey(&item)
					nodeType := graphpkg.NodeTypeTemplate // default
					if nt, ok := graphpkg.NodeTypeFromLabelValue(labelValue); ok {
						nodeType = nt
					}
					keys = append(keys, Applied{
						Key:       rk,
						NodeType:  nodeType,
						HasStatus: false, // unknown from labels
					})
					break
				}
			}
		}
	}

	return keys, nil
}

// pruneOrder sorts resource keys in reverse dependency order using all
// available DAGs (active + superseded). Dependents are deleted before
// their dependencies. Keys that don't match any DAG node are placed first
// (highest position — deleted first).
func pruneOrder(keys []string, dags []*dagpkg.DAG, defaultNS string, scope GVKScopeResolver) []string {
	// Build a map from resource key → topological position across all DAGs.
	// Use the highest position found across all DAGs for each key.
	keyPosition := map[string]int{}
	maxPosition := 0

	for _, d := range dags {
		// Build position map for this DAG.
		topoPosition := make(map[int]int, len(d.TopologicalOrder))
		for pos, nodeIdx := range d.TopologicalOrder {
			topoPosition[nodeIdx] = pos
		}

		for i, node := range d.Nodes {
			if node.Identity() == nil {
				continue
			}
			rk := staticResourceKey(node.Identity(), defaultNS, scope)
			if rk == "" {
				continue
			}
			pos := topoPosition[i]
			if pos > maxPosition {
				maxPosition = pos
			}
			if existing, ok := keyPosition[rk]; !ok || pos > existing {
				keyPosition[rk] = pos
			}
		}
	}

	type scored struct {
		key      string
		position int
	}
	scoredKeys := make([]scored, 0, len(keys))
	for _, key := range keys {
		pos, ok := keyPosition[key]
		if !ok {
			pos = maxPosition + 1 // unmatched → deleted first
		}
		scoredKeys = append(scoredKeys, scored{key: key, position: pos})
	}

	// Reverse topological: highest position first.
	sort.Slice(scoredKeys, func(i, j int) bool {
		return scoredKeys[i].position > scoredKeys[j].position
	})

	result := make([]string, len(scoredKeys))
	for i, s := range scoredKeys {
		result[i] = s.key
	}
	return result
}

// pruneOrderApplied sorts Applied entries in reverse dependency order.
// Same algorithm as pruneOrder but operates on Applied entries.
func pruneOrderApplied(candidates []Applied, dags []*dagpkg.DAG, defaultNS string, scope GVKScopeResolver) []Applied {
	// Build a map from resource key → topological position across all DAGs.
	keyPosition := map[string]int{}
	maxPosition := 0

	for _, d := range dags {
		topoPosition := make(map[int]int, len(d.TopologicalOrder))
		for pos, nodeIdx := range d.TopologicalOrder {
			topoPosition[nodeIdx] = pos
		}

		for i, node := range d.Nodes {
			if node.Identity() == nil {
				continue
			}
			rk := staticResourceKey(node.Identity(), defaultNS, scope)
			if rk == "" {
				continue
			}
			pos := topoPosition[i]
			if pos > maxPosition {
				maxPosition = pos
			}
			if existing, ok := keyPosition[rk]; !ok || pos > existing {
				keyPosition[rk] = pos
			}
		}
	}

	type scored struct {
		applied  Applied
		position int
	}
	scoredKeys := make([]scored, 0, len(candidates))
	for _, a := range candidates {
		pos, ok := keyPosition[a.Key]
		if !ok {
			pos = maxPosition + 1
		}
		scoredKeys = append(scoredKeys, scored{applied: a, position: pos})
	}

	sort.Slice(scoredKeys, func(i, j int) bool {
		return scoredKeys[i].position > scoredKeys[j].position
	})

	result := make([]Applied, len(scoredKeys))
	for i, s := range scoredKeys {
		result[i] = s.applied
	}
	return result
}

// ---------------------------------------------------------------------------
// Delete preflight
// ---------------------------------------------------------------------------

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
	// (missing identity label).
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
func (c *clusterAccess) deletePreflight(
	ctx context.Context,
	key string,
	rs *reconcileScope,
	checkIdentityLabels bool,
) deletePreflightResult {
	obj, nn, ok := unstructuredFromKey(key)
	if !ok {
		return deletePreflightResult{Outcome: deleteSkipParseFailed}
	}

	// Direct API server read for authoritative existence/ownership data.
	if err := c.reader.Get(ctx, nn, obj); err != nil {
		return deletePreflightResult{Outcome: deleteNotFound}
	}

	// Ownership gate: identity labels.
	if checkIdentityLabels {
		objLabels := obj.GetLabels()
		hasOurLabel := false
		if objLabels != nil {
			for k := range objLabels {
				if graphpkg.IsGraphIdentityLabel(k, rs.name, rs.namespace) {
					hasOurLabel = true
					break
				}
			}
		}
		if !hasOurLabel {
			return deletePreflightResult{Outcome: deleteNotOwned, Obj: obj}
		}
	}

	// Contributor-aware deletion: check managedFields for other field
	// managers before deleting. If present, deletion is blocked — the
	// finalizer holds until the other manager releases.
	//
	// Exception: during teardown (checkIdentityLabels=false), resources
	// that the controller never successfully applied (no identity labels)
	// are skipped rather than blocking. A conflicted resource that was
	// never owned should not prevent Graph deletion.
	ownManager := string(graphFieldOwner(rs.graph))
	if blockers := thirdPartyFieldManagers(obj, ownManager); len(blockers) > 0 {
		// If we're in teardown mode AND the resource has no identity labels,
		// skip it — we never owned it, so we shouldn't block teardown on it.
		if !checkIdentityLabels {
			objLabels := obj.GetLabels()
			hasOurLabel := false
			if objLabels != nil {
				for k := range objLabels {
					if graphpkg.IsGraphIdentityLabel(k, rs.name, rs.namespace) {
						hasOurLabel = true
						break
					}
				}
			}
			if !hasOurLabel {
				return deletePreflightResult{Outcome: deleteNotOwned, Obj: obj}
			}
		}
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
