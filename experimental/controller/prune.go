// prune.go computes and executes the prune walk — removing resources no longer
// in the applied set. Prune candidates are the diff between previous and current
// applied keys. Deletion follows reverse topological order from the DAG.
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

// collectPruneCandidates returns keys in previousKeys but not in currentKeys.
func collectPruneCandidates(previousKeys map[string]bool, currentKeys []string) []string {
	currentSet := map[string]bool{}
	for _, k := range currentKeys {
		currentSet[k] = true
	}
	var candidates []string
	for key := range previousKeys {
		if !currentSet[key] {
			candidates = append(candidates, key)
		}
	}
	return candidates
}

// buildKeyMaps creates bidirectional node-ID ↔ resource-key mappings from all DAGs.
func buildKeyMaps(dag *dagpkg.DAG, supersededDAGs map[string]*dagpkg.DAG, namespace string, scope GVKScopeResolver) (keyToNodeID map[string]string, nodeIDToKey map[string]string) {
	keyToNodeID = map[string]string{}
	nodeIDToKey = map[string]string{}
	for _, d := range collectAllDAGs(dag, supersededDAGs) {
		for _, node := range d.Nodes {
			if node.Identity() != nil {
				if rk := staticResourceKey(node.Identity(), namespace, scope); rk != "" {
					keyToNodeID[rk] = node.ID
					nodeIDToKey[node.ID] = rk
				}
			}
		}
	}
	return
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

// pruneRemovedResources deletes or releases resources no longer in the applied
// set. Returns (deferredKeys, blockedReasons, notes, err):
//   - deferredKeys: keys of resources whose deletion was deferred this cycle
//     (finalization in progress, blocked by third-party field managers, etc.).
//     The caller must include these in deferredPruneKeys for the next
//     reconcile so they remain visible as prune candidates, AND must not GC
//     superseded revisions while any deferral is active — the superseded DAG's
//     finalizer relationships are still needed to complete the sequence.
//   - blockedReasons: TeardownBlocked messages distinguishing third-party
//     field managers, finalizer creation failure, and readyWhen failure.
//     These become error text in the Ready condition — they gate Ready.
//   - notes: informational messages (e.g., FinalizerSkipped) that don't gate
//     Ready. Routed to reconcileState.nodeNotes at the caller so healthy
//     graphs with notes still report Ready=True with the note in the message.
//   - err: hard error from an API call; sets pruneOK=false at the call site.
func (r *GraphReconciler) pruneRemovedResources(ctx context.Context, graph *unstructured.Unstructured, previousKeys map[string]bool, currentKeys []string, dag *dagpkg.DAG, supersededDAGs map[string]*dagpkg.DAG, finResult *finalizationResult) ([]string, []string, []string, error) {
	logger := log.FromContext(ctx)
	var deferredKeys []string
	var blockedReasons []string // TeardownBlocked messages — gates Ready
	var notes []string          // informational messages (FinalizerSkipped) — does not gate Ready
	// deferredDeletes collects finalizer resource keys whose targets were
	// successfully deleted in this walk. These are processed after the walk
	// completes — not inline — to avoid corrupting forEach finalization
	// where children must survive the readiness check before cleanup.
	//
	// Invariant: a key appears here only when:
	//   1. runFinalization returned ready=true
	//   2. The target was successfully deleted (r.Client.Delete succeeded)
	//   3. No state has changed since (synchronous execution)
	var deferredDeletes []string

	// Build current key set for fast lookup
	currentSet := map[string]bool{}
	for _, k := range currentKeys {
		currentSet[k] = true
	}

	// Collect all DAGs (active + superseded) for finalizer lookups.
	// The old revision's finalizes declarations govern prune of its resources.
	allDAGs := []*dagpkg.DAG{}
	if dag != nil {
		allDAGs = append(allDAGs, dag)
	}
	for _, d := range supersededDAGs {
		allDAGs = append(allDAGs, d)
	}

	// Build resource-key-to-node-ID map and reverse from ALL DAGs.
	keyToNodeID := map[string]string{}
	nodeIDToKey := map[string]string{}
	for _, d := range allDAGs {
		for _, node := range d.Nodes {
			if node.Identity() != nil {
				if rk := staticResourceKey(node.Identity(), graph.GetNamespace(), r.Scope); rk != "" {
					keyToNodeID[rk] = node.ID
					nodeIDToKey[node.ID] = rk
				}
			}
		}
	}

	// finalizerProtected: resources that must not be pruned. Computed by
	// advanceFinalization which ran before this function.
	finalizerProtected := finResult.ProtectedKeys

	fieldOwner := graphFieldOwner(graph)

	// Collect prune candidates: keys in previous but not current.
	var pruneCandidates []string
	for key := range previousKeys {
		if !currentSet[key] {
			pruneCandidates = append(pruneCandidates, key)
		}
	}

	// Sort prune candidates in reverse dependency order so dependents are
	// removed before their dependencies.
	pruneCandidates = pruneOrder(pruneCandidates, allDAGs, graph.GetNamespace(), r.Scope)

	for _, key := range pruneCandidates {
		// Defer deletion of resources that are dependencies of in-flight
		// finalizer nodes — they must remain alive while the finalizer runs.
		if finalizerProtected[key] {
			logger.Info("prune deferred: resource is protected by in-flight finalization",
				"key", key)
			deferredKeys = append(deferredKeys, key)
			continue
		}

		// Patch keys use release apply (release fields), not delete.
		if resKey, hasStatus := parsePatchKey(key); resKey != "" {
			gvk, nn := parseResourceKey(resKey)
			if gvk.Kind == "" {
				continue
			}
			if _, err := releaseApply(ctx, r.Client, gvk, nn.Namespace, nn.Name, fieldOwner, hasStatus); err != nil {
				logger.Error(err, "releasing contribution fields", "key", resKey)
			} else {
				logger.Info("released contribution fields", "key", resKey)
				r.Resources.remove(resKey)
			}
			continue
		}

		// Template keys: delete the resource.
		obj, nn, ok := unstructuredFromKey(key)
		if !ok {
			continue
		}

		// Check if it exists and is ours before deleting.
		// Direct API server read for authoritative ownership data.
		if err := r.apiReader().Get(ctx, nn, obj); err != nil {
			// Target already gone — nothing to delete.
			r.Resources.remove(key)
			continue // already gone
		}

		// Verify ownership: must have our identity label and apply hash
		objLabels := obj.GetLabels()
		hasOurLabel := false
		if objLabels != nil {
			for key := range objLabels {
				if graphpkg.IsGraphIdentityLabel(key, graph.GetName(), graph.GetNamespace()) {
					hasOurLabel = true
					break
				}
			}
		}
		if !hasOurLabel {
			continue // not ours
		}
		objAnnotations := obj.GetAnnotations()
		if objAnnotations == nil || objAnnotations[applyHashAnnotation] == "" {
			continue // never successfully applied by us
		}

		// Contributor-aware deletion: check managedFields for other field
		// managers before deleting. If present, deletion is blocked — the
		// resource stays in the applied set for retry on the next reconcile.
		ownManager := string(graphFieldOwner(graph))
		if blockers := thirdPartyFieldManagers(obj, ownManager); len(blockers) > 0 {
			logger.Info("prune blocked: resource has other field managers",
				"key", key, "blockers", blockers)
			blockedReasons = append(blockedReasons, fmt.Sprintf(
				"TeardownBlocked: %s (third-party field managers: %s)",
				key, strings.Join(blockers, ", ")))
			deferredKeys = append(deferredKeys, key)
			continue // resource stays in applied set — retry next reconcile
		}

		// Finalization check: if this target has finalizers, only delete if
		// advanceFinalization marked it complete. Otherwise skip — finalization
		// handles deferral and blocking reasons.
		nodeID := keyToNodeID[key]
		hasFinalizers := false
		for _, d := range allDAGs {
			if fins, ok := d.Finalizers[nodeID]; ok && len(fins) > 0 {
				hasFinalizers = true
				break
			}
		}
		if hasFinalizers && !finResult.CompletedTargets[key] {
			continue // finalization not complete — already deferred by advanceFinalization
		}

		if err := r.Client.Delete(ctx, obj); err != nil {
			if client.IgnoreNotFound(err) != nil {
				return deferredKeys, blockedReasons, notes, fmt.Errorf("pruning %s: %w", key, err)
			}
		} else {
			logger.Info("pruned resource", "key", key)
			r.Resources.remove(key)
			// Clean up finalization children now that target is deleted.
			if childKeys, ok := finResult.ChildKeysToCleanup[key]; ok {
				deferredDeletes = append(deferredDeletes, childKeys...)
			}
		}
	}

	// Clean up finalization children whose targets were successfully deleted.
	// Failures are non-fatal — children become normal prune candidates next cycle.
	r.deleteByKeys(ctx, deferredDeletes)

	return deferredKeys, blockedReasons, notes, nil
}

// findManagedResourceKeys discovers dynamically-named resources (forEach, CEL
// names) by listing resources with our graph-name label. Returns resource keys.
//
// The GVK list is derived from the revision specs — every resource template
// declares its apiVersion and kind, so we know exactly which types to scan.
// This avoids a hardcoded GVK list that would silently miss new resource types.
func (r *GraphReconciler) findManagedResourceKeys(ctx context.Context, graph *unstructured.Unstructured) ([]string, error) {
	// Collect unique GVKs from all revisions for this Graph.
	revisions, err := listRevisions(ctx, r.Client, graph.GetName(), graph.GetNamespace())
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

	var keys []string
	suffix := graphpkg.GraphLabelSuffix(graph.GetName(), graph.GetNamespace())
	for gvk := range gvkSet {
		list := &unstructured.UnstructuredList{}
		listGVK := gvk
		listGVK.Kind = gvk.Kind + "List"
		list.SetGroupVersionKind(listGVK)

		// Cannot use label selector with DNS subdomain keys — list all and
		// filter client-side. This only runs during teardown (not hot path).
		if err := r.apiReader().List(ctx, list, &client.ListOptions{
			Namespace: graph.GetNamespace(),
		}); err != nil {
			continue // skip GVKs we can't list (e.g., CRD deleted)
		}

		for _, item := range list.Items {
			itemLabels := item.GetLabels()
			for key := range itemLabels {
				if strings.HasSuffix(key, suffix) {
					keys = append(keys, resourceKey(&item))
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
			// Also check patch keys against their underlying resource key.
			if resKey, _ := parsePatchKey(key); resKey != "" {
				pos, ok = keyPosition[resKey]
			}
		}
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

// deletionOrder returns resource keys ordered for deletion: reverse
// topological order from the DAG. Delegates to pruneOrder for the actual
// ordering logic.
//
// Per 005-reconciliation.md § Teardown: "Ordering comes from the
// active revision's DAG [...] If the revision was deleted (ownerReference
// cascade race), the controller regenerates the DAG from spec."
//
// preferredDAG is the active revision's DAG (already compiled elsewhere in
// the teardown path). When non-nil it is used directly. When nil, the DAG
// is regenerated from the live Graph spec as a fallback — this covers the
// ownerReference cascade race where the revision was deleted before the
// Graph. Returns an error only if the fallback compile fails.
//
// Per the design: "Teardown is blocked until ordering is available —
// never degrade to unordered deletion."
func (r *GraphReconciler) deletionOrder(graph *unstructured.Unstructured, keys []string, preferredDAG *dagpkg.DAG) ([]string, error) {
	if preferredDAG != nil {
		return pruneOrder(keys, []*dagpkg.DAG{preferredDAG}, graph.GetNamespace(), r.Scope), nil
	}
	graphSpec, err := graphpkg.ExtractGraphSpec(graph.Object)
	if err != nil {
		return nil, fmt.Errorf("extracting graph spec for deletion order: %w", err)
	}
	dag, err := dagpkg.BuildDAG(graphSpec.Nodes, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("building DAG for deletion order: %w", err)
	}
	return pruneOrder(keys, []*dagpkg.DAG{dag}, graph.GetNamespace(), r.Scope), nil
}
