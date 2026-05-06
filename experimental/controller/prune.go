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
// Only pruneDeferred and pruneBlocked are significant — they are consumed
// by collectDeferredKeys to determine retry candidates. pruneDeleted is
// recorded for logging but not consumed by any downstream logic.
type pruneOutcome int

const (
	pruneNone     pruneOutcome = iota // no outcome recorded; used for results already tracked elsewhere (e.g., finalization phase)
	pruneDeleted                      // template resource deleted
	pruneSkipped                      // no action needed (not found, not owned, parse failed)
	pruneBlocked                      // third-party managers block deletion
	pruneDeferred                     // finalization not ready
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
	if state.prune.activeFinalization == nil {
		state.prune.activeFinalization = map[string]*finalizationEntry{}
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

	opts := pruneOpts{
		ProtectedKeys:       finResult.ProtectedKeys,
		CompletedTargets:    finResult.CompletedTargets,
		ChildKeysToCleanup:  finResult.ChildKeysToCleanup,
		CheckIdentityLabels: checkIdentityLabels,
		FieldOwner:          graphFieldOwner(rs.graph),
		KeyToNodeID:         keyToNodeID,
		DAGs:                dags,
	}

	// deferredDeletes collects finalizer resource keys whose targets were
	// successfully deleted in this walk. Processed after the walk completes.
	var deferredDeletes []string

	for _, candidate := range ordered {
		if candidate.Key == "" {
			continue
		}
		// Skip keys that are in the current applied set.
		if _, inCurrent := currentKeys[candidate.Key]; inCurrent {
			continue
		}

		cr := c.pruneCandidate(ctx, rs, candidate, opts)
		if cr.Outcome != pruneNone {
			result.Outcomes[candidate.Key] = cr.Outcome
		}
		if len(cr.BlockedReasons) > 0 {
			result.BlockedReasons = append(result.BlockedReasons, cr.BlockedReasons...)
		}
		if len(cr.Notes) > 0 {
			result.Notes = append(result.Notes, cr.Notes...)
		}
		if len(cr.ChildKeysToDelete) > 0 {
			deferredDeletes = append(deferredDeletes, cr.ChildKeysToDelete...)
		}
		if cr.Err != nil {
			result.Err = cr.Err
			return result
		}
	}

	// Clean up finalization children whose targets were successfully deleted.
	// Sort in reverse topological order so dependents are deleted before their
	// dependencies (design: 005-reconciliation.md § Finalization).
	if len(deferredDeletes) > 1 {
		keyPosition, maxPos := buildKeyPositionMap(dags, rs.namespace, c.scope)
		sort.Slice(deferredDeletes, func(i, j int) bool {
			pi, oki := keyPosition[deferredDeletes[i]]
			if !oki {
				pi = maxPos + 1
			}
			pj, okj := keyPosition[deferredDeletes[j]]
			if !okj {
				pj = maxPos + 1
			}
			return pi > pj
		})
	}
	c.deleteByKeys(ctx, deferredDeletes)

	return result
}

// ---------------------------------------------------------------------------
// Per-candidate prune helper
// ---------------------------------------------------------------------------

// pruneOpts carries read-only context from finalization into pruneCandidate.
type pruneOpts struct {
	ProtectedKeys       map[string]bool     // keys to skip (active finalization children)
	CompletedTargets    map[string]bool     // targets whose finalization is done
	ChildKeysToCleanup  map[string][]string // target key → child keys to delete after target
	CheckIdentityLabels bool                // prune mode (true) vs teardown mode (false)
	FieldOwner          client.FieldOwner   // field manager name for this graph
	KeyToNodeID         map[string]string   // resource key → DAG node ID
	DAGs                []*dagpkg.DAG       // all DAGs for finalizer lookup
}

// pruneCandidateResult carries the outcome of processing a single candidate.
type pruneCandidateResult struct {
	Outcome         pruneOutcome // 0 means no outcome recorded (patch release, skipped internally)
	BlockedReasons  []string
	Notes           []string
	ChildKeysToDelete []string // finalization children to clean up
	Err             error      // hard API error — caller should abort
}

// pruneCandidate processes a single prune candidate: dispatches by node type
// (patch release, template delete, finalization-child cleanup).
func (c *clusterAccess) pruneCandidate(
	ctx context.Context,
	rs *reconcileScope,
	candidate Applied,
	opts pruneOpts,
) pruneCandidateResult {
	logger := log.FromContext(ctx)
	key := candidate.Key

	// Defer deletion of resources protected by in-flight finalization.
	if opts.ProtectedKeys[key] {
		logger.Info("prune deferred: resource is protected by in-flight finalization",
			"key", key)
		return pruneCandidateResult{Outcome: pruneDeferred}
	}

	// Dispatch based on NodeType.
	switch candidate.NodeType {
	case graphpkg.NodeTypePatch:
		// Patch keys use release apply (release fields), not delete.
		gvk, nn := parseResourceKey(key)
		if gvk.Kind == "" {
			return pruneCandidateResult{}
		}
		if _, err := releaseApply(ctx, c.client, gvk, nn.Namespace, nn.Name, opts.FieldOwner, candidate.HasStatus); err != nil {
			logger.Error(err, "releasing contribution fields", "key", key)
		} else {
			logger.Info("released contribution fields", "key", key)
		}
		return pruneCandidateResult{}

	default:
		return c.pruneCandidateTemplate(ctx, rs, key, opts)
	}
}

// pruneCandidateTemplate handles the template-type candidate path: preflight
// check, finalization gating, and deletion.
func (c *clusterAccess) pruneCandidateTemplate(
	ctx context.Context,
	rs *reconcileScope,
	key string,
	opts pruneOpts,
) pruneCandidateResult {
	logger := log.FromContext(ctx)

	pf := c.deletePreflight(ctx, key, rs, opts.CheckIdentityLabels)
	switch pf.Outcome {
	case deleteSkipParseFailed:
		return pruneCandidateResult{Outcome: pruneSkipped}
	case deleteNotFound:
		return c.pruneCandidateNotFound(ctx, key, opts)
	case deleteNotOwned:
		return pruneCandidateResult{Outcome: pruneSkipped}
	case deleteBlockedByFieldManagers:
		logger.Info("prune blocked: resource has other field managers",
			"key", key, "blockers", pf.Blockers)
		return pruneCandidateResult{
			Outcome:        pruneBlocked,
			BlockedReasons: []string{formatBlockedReason(key, pf.Blockers)},
		}
	}

	// deleteReady: resource exists, is ours, no third-party field managers.
	// Finalization check: if this target has finalizers, only delete if
	// advanceFinalization marked it complete.
	nodeID := opts.KeyToNodeID[key]
	if hasFinalizers(nodeID, opts.DAGs) && !opts.CompletedTargets[key] {
		return pruneCandidateResult{} // already tracked as pruneDeferred in Phase 1
	}

	if err := c.client.Delete(ctx, pf.Obj); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return pruneCandidateResult{Err: fmt.Errorf("pruning %s: %w", key, err)}
		}
	} else {
		logger.Info("pruned resource", "key", key)
		var childKeys []string
		if ck, ok := opts.ChildKeysToCleanup[key]; ok {
			childKeys = ck
		}
		return pruneCandidateResult{Outcome: pruneDeleted, ChildKeysToDelete: childKeys}
	}
	return pruneCandidateResult{}
}

// pruneCandidateNotFound handles the case where the target resource doesn't
// exist during preflight — cleans up finalization children and emits notes.
func (c *clusterAccess) pruneCandidateNotFound(
	ctx context.Context,
	key string,
	opts pruneOpts,
) pruneCandidateResult {
	logger := log.FromContext(ctx)
	var cr pruneCandidateResult
	cr.Outcome = pruneSkipped

	// Target absent — clean up finalization children if mid-finalization.
	if childKeys, ok := opts.ChildKeysToCleanup[key]; ok {
		cr.ChildKeysToDelete = childKeys
	}

	// Emit FinalizerSkipped note if this target had finalizers.
	nodeID := opts.KeyToNodeID[key]
	if nodeID != "" && hasFinalizers(nodeID, opts.DAGs) {
		logger.Info("finalization skipped: target resource does not exist", "key", key)
		cr.Notes = append(cr.Notes,
			fmt.Sprintf("FinalizerSkipped: %s (target absent)", key))
	}
	return cr
}

// hasFinalizers reports whether any DAG declares finalizer nodes for the given
// node ID.
func hasFinalizers(nodeID string, dags []*dagpkg.DAG) bool {
	for _, d := range dags {
		if fins, ok := d.Finalizers[nodeID]; ok && len(fins) > 0 {
			return true
		}
	}
	return false
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

// buildKeyPositionMap builds a map from resource key → topological position
// across all DAGs. Uses the highest position found across all DAGs for each
// key. Returns the position map and the maximum position observed.
func buildKeyPositionMap(dags []*dagpkg.DAG, defaultNS string, scope *scopeResolver) (map[string]int, int) {
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

	return keyPosition, maxPosition
}

// pruneOrderApplied sorts Applied entries in reverse dependency order using
// all available DAGs (active + superseded). Dependents are deleted before
// their dependencies. Keys that don't match any DAG node are placed first
// (highest position — deleted first).
func pruneOrderApplied(candidates []Applied, dags []*dagpkg.DAG, defaultNS string, scope *scopeResolver) []Applied {
	keyPosition, maxPosition := buildKeyPositionMap(dags, defaultNS, scope)

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

// hasIdentityLabel reports whether any label in labels matches the graph
// identity label for the given graph name and namespace.
func hasIdentityLabel(labels map[string]string, graphName, namespace string) bool {
	for k := range labels {
		if graphpkg.IsGraphIdentityLabel(k, graphName, namespace) {
			return true
		}
	}
	return false
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
		if !hasIdentityLabel(obj.GetLabels(), rs.name, rs.namespace) {
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
			if !hasIdentityLabel(obj.GetLabels(), rs.name, rs.namespace) {
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
