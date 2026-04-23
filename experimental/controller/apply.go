// apply.go contains the cluster-mutating operations for the Graph controller:
// resource apply (Template and Patch), pruning, deletion ordering, and the
// applied set key format.
//
// The reconcile loop in controller.go dispatches nodes; this file executes
// against the Kubernetes API server. All direct Patch, Delete, and ownership
// verification calls are here — controller.go does not touch the cluster
// directly for resource management.
package graphcontroller

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/kubernetes-sigs/kro/experimental/controller/compiler"
	dagpkg "github.com/kubernetes-sigs/kro/experimental/controller/dag"
	graphpkg "github.com/kubernetes-sigs/kro/experimental/controller/graph"
	"github.com/kubernetes-sigs/kro/experimental/controller/watches"
)

// ---------------------------------------------------------------------------
// SSA configuration
// ---------------------------------------------------------------------------

// graphFieldOwner returns the SSA field manager identity for a Graph.
// Per the design (003-ownership): <name>.<namespace>.internal.kro.run
func graphFieldOwner(graph *unstructured.Unstructured) client.FieldOwner {
	return client.FieldOwner(graph.GetName() + "." + graph.GetNamespace() + ".internal.kro.run")
}

// thirdPartyFieldManagers returns field manager names on the resource that are
// not this Graph's own manager and not the API server's defaulting manager.
// Only SSA Apply managers are considered — Update managers (from kubectl edit,
// plain client.Update, etc.) don't declare field ownership and shouldn't block
// deletion. Per 003-ownership.md: before deleting a Template resource, check
// managedFields for other field managers (excluding the API server's own).
func thirdPartyFieldManagers(obj *unstructured.Unstructured, ownFieldManager string) []string {
	managedFields := obj.GetManagedFields()
	if len(managedFields) == 0 {
		return nil
	}

	var thirdParty []string
	for _, mf := range managedFields {
		manager := mf.Manager
		// Only Apply managers count — they declare field ownership via SSA.
		// Update managers (kubectl edit, plain Update) don't declare ownership.
		if mf.Operation != metav1.ManagedFieldsOperationApply {
			continue
		}
		// Exclude our own field manager
		if manager == ownFieldManager {
			continue
		}
		// Exclude the API server's defaulting field managers
		if isAPIServerManager(manager) {
			continue
		}
		thirdParty = append(thirdParty, manager)
	}
	return thirdParty
}

// isAPIServerManager returns true if the field manager name belongs to the
// Kubernetes API server's internal defaulting/admission logic.
func isAPIServerManager(manager string) bool {
	switch manager {
	case "kube-apiserver", "apiserver":
		return true
	default:
		return false
	}
}

// ---------------------------------------------------------------------------
// Finalization
// ---------------------------------------------------------------------------

// deleteByKeys deletes resources identified by applied-set keys. Logs each
// attempt. Ignores NotFound (resource already gone). Used by both prune and
// teardown to clean up finalizer resources after a target is deleted.
func (r *GraphReconciler) deleteByKeys(ctx context.Context, keys []string) {
	logger := log.FromContext(ctx)
	for _, fk := range keys {
		finDel, _, ok := unstructuredFromKey(fk)
		if !ok {
			continue
		}
		if delErr := r.Client.Delete(ctx, finDel); delErr != nil {
			if client.IgnoreNotFound(delErr) != nil {
				logger.V(1).Info("finalizer resource cleanup failed", "key", fk, "error", delErr)
			}
		} else {
			logger.V(1).Info("cleaned up finalizer resource", "key", fk)
			r.Resources.remove(fk)
		}
	}
}

// ---------------------------------------------------------------------------
// Apply / SSA
// ---------------------------------------------------------------------------

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

// ---------------------------------------------------------------------------
// Apply / SSA
// ---------------------------------------------------------------------------

// applySSA applies a template via server-side apply (SSA). Handles both
// Template and Patch node types — the nodeType parameter controls:
//   - Identity labels: Template skips if present (forEach children stamp their own),
//     Patch always stamps.
//   - Pre-apply check: Template does a kro label check (cross-Graph ownership guard),
//     Patch checks target existence (patches apply to existing resources).
//   - Cache miss on NotFound: Template clears cache + returns ErrPending,
//     Patch falls through to apply.
//   - Apply hash annotation: Template only (Patch targets are owned by others).
//
// generation is the value to stamp on the identity-generation label. Normally
// matches graph.GetGeneration(); callers pass the reconcile-scoped effective
// generation (the active revision's generation when the current generation
// failed to compile). Explicit parameter rather than reading from graph so
// the choice is visible at the call site.
//
// resyncCorrection bypasses the content-addressed apply hash check.
// Per 005-reconciliation.md § Reconcile: "The resync timer bypasses the
// template-hash check — apply unconditionally, because server-side
// defaulters and mutating webhooks can change fields without changing
// the desired state hash. SSA is idempotent; the apply corrects drift
// as a side effect."
func (r *GraphReconciler) applySSA(ctx context.Context, graph *unstructured.Unstructured, evalMap map[string]any, watcher *watches.GraphWatcher, nodeID string, nodeType graphpkg.NodeType, generation int64, resyncCorrection bool, forceApply bool) (*unstructured.Unstructured, error) {
	fieldOwner := graphFieldOwner(graph)
	obj := &unstructured.Unstructured{Object: evalMap}

	if obj.GetNamespace() == "" {
		// Only default namespace for namespace-scoped resources.
		// Cluster-scoped resources (CRDs, ClusterRoles, etc.) must keep
		// namespace empty — setting it breaks watch event routing because
		// the metadata informer reports events with namespace="" while
		// the scalar index would store namespace="<graph-ns>".
		clusterScoped := false
		if r.Scope != nil {
			if isNS, known := r.Scope.IsNamespaced(obj.GroupVersionKind()); known && !isNS {
				clusterScoped = true
			}
		}
		if !clusterScoped {
			obj.SetNamespace(graph.GetNamespace())
		}
	}

	// Stamp identity labels per 005-reconciliation.md § API Server Interaction.
	generationStr := fmt.Sprintf("%d", generation)
	lbls := obj.GetLabels()
	if lbls == nil {
		lbls = map[string]string{}
	}
	if nodeType == graphpkg.NodeTypeTemplate {
		// Template: skip stamping if identity labels are already present (e.g., forEach
		// children stamp their own child-scoped labels before calling applySSA).
		if !graphpkg.HasGraphIdentityLabels(lbls, graph.GetName(), graph.GetNamespace()) {
			lbls = graphpkg.SetIdentityLabels(lbls, nodeID, graph.GetName(), graph.GetNamespace(), generationStr, graphpkg.NodeTypeTemplate)
			obj.SetLabels(lbls)
		}
	} else {
		// Patch: always stamp identity labels so resources are discoverable via
		// deriveAppliedSet() after controller restart.
		lbls = graphpkg.SetIdentityLabels(lbls, nodeID, graph.GetName(), graph.GetNamespace(), generationStr, graphpkg.NodeTypePatch)
		obj.SetLabels(lbls)
	}

	// Buffer a watch for this resource (flushed at done(true)).
	gvr := gvkToGVR(obj.GroupVersionKind())
	if watcher != nil {
		watcher.WatchScalar(nodeID, gvr, obj.GroupVersionKind().Kind, obj.GetName(), obj.GetNamespace())
	}

	// Content-addressed apply: hash the desired state to detect changes.
	applyHash, err := hashDesiredState(obj.Object)
	if err != nil {
		return nil, fmt.Errorf("hashing template for %s: %w", obj.GetName(), err)
	}
	cacheKey := resourceCacheKey(obj.GetAPIVersion(), obj.GetKind(), obj.GetNamespace(), obj.GetName())

	// Resync correction bypasses the cache check entirely — the resync timer's
	// purpose is to re-apply unconditionally so SSA corrects any live-state
	// divergence from the desired state.
	if !resyncCorrection {
		if cached, ok := r.Resources.get(cacheKey); ok && cached.applyHash == applyHash {
			if watcher != nil {
				liveRV := watcher.GetResourceVersion(gvr, obj.GetNamespace(), obj.GetName())
				if liveRV != "" && liveRV == cached.resourceVersion {
					return &unstructured.Unstructured{Object: cached.object}, nil
				}
			}
			// Watcher RV differs from cached or watcher unavailable —
			// resource changed externally or freshness unknown. Fall
			// through to SSA apply whose Patch response is authoritative.
			// No intermediate GET: the controller-runtime cache can lag
			// the metadata informer, and the SSA Patch response is
			// strictly newer than any cached read.
		}
	} // !resyncCorrection

	// Template: set the apply hash annotation for future comparisons.
	if nodeType == graphpkg.NodeTypeTemplate {
		annotations := obj.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[applyHashAnnotation] = applyHash
		obj.SetAnnotations(annotations)
	}

	// Pre-apply check differs by node type.
	if nodeType == graphpkg.NodeTypeTemplate {
		// kro label check: if the existing resource has a different Graph's identity
		// label, require Force to proceed. Prevents accidental cross-Graph ownership.
		// Read labels from the metadata informer first (fast path, no API call).
		// Fall back to direct API read when the informer hasn't observed the
		// resource yet — preserves the cross-Graph safety guarantee during
		// informer startup or watch reconnect.
		var existingLabels map[string]string
		if watcher != nil {
			existingLabels, _ = watcher.GetLabels(gvr, obj.GetNamespace(), obj.GetName())
		}
		if existingLabels == nil {
			existing := &unstructured.Unstructured{}
			existing.SetGroupVersionKind(obj.GroupVersionKind())
			if getErr := r.apiReader().Get(ctx, client.ObjectKey{Namespace: obj.GetNamespace(), Name: obj.GetName()}, existing); getErr == nil {
				existingLabels = existing.GetLabels()
			}
		}
		if existingLabels != nil {
			if otherGraph, conflict := graphpkg.HasOtherGraphIdentityLabel(existingLabels, graph.GetName(), graph.GetNamespace()); conflict {
				if !forceApply {
					return nil, fmt.Errorf("resource %s/%s %s owned by Graph %q, not %q (use lifecycle.apply: Force to take ownership): %w",
						obj.GetAPIVersion(), obj.GetKind(), obj.GetName(), otherGraph, graph.GetName(), compiler.ErrFieldConflict)
				}
			}
		}
	} else {
		// Patch: target must exist — patches apply to existing resources.
		// Direct API server read to avoid stale cache giving false negatives.
		targetCheck := &unstructured.Unstructured{}
		targetCheck.SetGroupVersionKind(obj.GroupVersionKind())
		if err := r.apiReader().Get(ctx, client.ObjectKey{Namespace: obj.GetNamespace(), Name: obj.GetName()}, targetCheck); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, fmt.Errorf("patch target %s/%s %s/%s not found: %w", obj.GetAPIVersion(), obj.GetKind(), obj.GetNamespace(), obj.GetName(), compiler.ErrPending)
			}
			return nil, fmt.Errorf("checking patch target %s: %w", obj.GetName(), err)
		}
	}

	var patchOpts []client.PatchOption
	patchOpts = append(patchOpts, fieldOwner)
	if forceApply {
		patchOpts = append(patchOpts, client.ForceOwnership)
	}

	// Split status from main payload — status subresource requires a separate patch.
	statusData, hasStatus := obj.Object["status"]
	mainPayload := obj.Object
	if hasStatus && statusData != nil {
		mainPayload = make(map[string]any, len(obj.Object))
		for k, v := range obj.Object {
			if k != "status" {
				mainPayload[k] = v
			}
		}
	}

	data, err := json.Marshal(mainPayload)
	if err != nil {
		return nil, fmt.Errorf("marshaling: %w", err)
	}

	applied := &unstructured.Unstructured{}
	applied.SetGroupVersionKind(obj.GroupVersionKind())
	applied.SetName(obj.GetName())
	applied.SetNamespace(obj.GetNamespace())

	if err := r.Client.Patch(ctx, applied, client.RawPatch(types.ApplyPatchType, data), patchOpts...); err != nil {
		if apierrors.IsConflict(err) {
			return nil, fmt.Errorf("SSA conflict on %s/%s %s: %w: %w", obj.GetAPIVersion(), obj.GetKind(), obj.GetName(), compiler.ErrFieldConflict, err)
		}
		return nil, fmt.Errorf("applying %s/%s %s: %w", obj.GetAPIVersion(), obj.GetKind(), obj.GetName(), err)
	}

	// Status subresource patch if .status is present.
	// The response from whichever patch runs last is the most current
	// server-side state — use it as the scope value instead of doing
	// a separate readback GET (which goes through the controller-runtime
	// cache and can serve stale data).
	readBack := applied
	if hasStatus && statusData != nil {
		statusPayload := map[string]any{
			"apiVersion": obj.GetAPIVersion(),
			"kind":       obj.GetKind(),
			"metadata": map[string]any{
				"name":      obj.GetName(),
				"namespace": obj.GetNamespace(),
			},
			"status": statusData,
		}
		sData, err := json.Marshal(statusPayload)
		if err != nil {
			return nil, fmt.Errorf("marshaling status: %w", err)
		}
		statusApplied := &unstructured.Unstructured{}
		statusApplied.SetGroupVersionKind(obj.GroupVersionKind())
		statusApplied.SetName(obj.GetName())
		statusApplied.SetNamespace(obj.GetNamespace())
		var statusOpts []client.SubResourcePatchOption
		statusOpts = append(statusOpts, fieldOwner)
		if forceApply {
			statusOpts = append(statusOpts, client.ForceOwnership)
		}
		if err := r.Client.Status().Patch(ctx, statusApplied, client.RawPatch(types.ApplyPatchType, sData), statusOpts...); err != nil {
			r.Resources.remove(cacheKey)
			if apierrors.IsConflict(err) {
				return nil, fmt.Errorf("SSA status conflict on %s/%s %s: %w: %w", obj.GetAPIVersion(), obj.GetKind(), obj.GetName(), compiler.ErrFieldConflict, err)
			}
			return nil, fmt.Errorf("applying status %s/%s %s: %w", obj.GetAPIVersion(), obj.GetKind(), obj.GetName(), err)
		}
		// Status Patch response is strictly newer — includes the status
		// we just wrote plus the current main-resource state.
		readBack = statusApplied
	}

	// Eviction release: after a force-apply on a template, evict all third-party
	// Apply-type field managers. Per 003-ownership.md § Release Apply (eviction
	// release): issue a full release impersonating each third-party manager so
	// their managedFields entry is garbage-collected and their solely-owned fields
	// (e.g., identity labels) are deleted from the object.
	if forceApply && nodeType == graphpkg.NodeTypeTemplate {
		ownManager := string(fieldOwner)
		if managers := thirdPartyFieldManagers(readBack, ownManager); len(managers) > 0 {
			hasStatus := templateHasStatus(evalMap)
			for _, mgr := range managers {
				evicted, err := releaseApply(ctx, r.Client, obj.GroupVersionKind(), obj.GetNamespace(), obj.GetName(), client.FieldOwner(mgr), hasStatus)
				if err != nil {
					return nil, fmt.Errorf("eviction release for manager %q on %s: %w", mgr, obj.GetName(), err)
				}
				// Use the last release's Patch response — it reflects
				// the cumulative effect of all evictions.
				if evicted != nil {
					readBack = evicted
				}
				log.FromContext(ctx).Info("evicted field manager", "node", nodeID, "manager", mgr, "name", obj.GetName())
			}
		}
	}

	r.Resources.set(cacheKey, &cachedObject{
		resourceVersion: readBack.GetResourceVersion(),
		applyHash:       applyHash,
		object:          readBack.Object,
	})

	return readBack, nil
}

// ---------------------------------------------------------------------------
// Prune + delete ordering
// ---------------------------------------------------------------------------

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
func (r *GraphReconciler) pruneRemovedResources(ctx context.Context, graph *unstructured.Unstructured, previousKeys map[string]bool, currentKeys []string, dag *dagpkg.DAG, supersededDAGs map[string]*dagpkg.DAG, eval *evaluator, watcher *watches.GraphWatcher, finResult *finalizationResult) ([]string, []string, []string, error) {
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
	r.cleanupFinalizationChildren(ctx, deferredDeletes)

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
	dag, err := dagpkg.BuildDAG(graphSpec.Nodes, nil)
	if err != nil {
		return nil, fmt.Errorf("building DAG for deletion order: %w", err)
	}
	return pruneOrder(keys, []*dagpkg.DAG{dag}, graph.GetNamespace(), r.Scope), nil
}

// ---------------------------------------------------------------------------
// Release apply — release field ownership without deleting the object
// ---------------------------------------------------------------------------

func releaseApply(ctx context.Context, c client.Client, gvk schema.GroupVersionKind, namespace, name string, fieldOwner client.FieldOwner, hasStatus bool) (*unstructured.Unstructured, error) {
	apiVersion := gvk.Group + "/" + gvk.Version
	if gvk.Group == "" {
		apiVersion = gvk.Version
	}
	release := map[string]any{
		"apiVersion": apiVersion,
		"kind":       gvk.Kind,
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
		},
	}

	data, err := json.Marshal(release)
	if err != nil {
		return nil, fmt.Errorf("marshaling release: %w", err)
	}

	target := &unstructured.Unstructured{}
	target.SetGroupVersionKind(gvk)
	target.SetName(name)
	target.SetNamespace(namespace)
	if err := c.Patch(ctx, target, client.RawPatch(types.ApplyPatchType, data), fieldOwner, client.ForceOwnership); err != nil {
		if apierrors.IsNotFound(err) {
			// Resource already deleted — nothing to release.
			// Callers must check the returned object for nil before use.
			return nil, nil
		}
		return nil, fmt.Errorf("release apply for %s/%s: %w", namespace, name, err)
	}

	// result holds the most recent Patch response — the authoritative
	// server-side state after this release.
	result := target

	if hasStatus {
		// The status subresource endpoint only processes fields under .status.
		// An identity-only release body (apiVersion/kind/metadata) is ignored by
		// the status endpoint — no fields are claimed, so no ownership is released.
		// Include "status": {} so SSA releases all previously-owned status fields.
		release["status"] = map[string]any{}
		statusData, err := json.Marshal(release)
		if err != nil {
			return nil, fmt.Errorf("marshaling status release: %w", err)
		}
		statusTarget := &unstructured.Unstructured{}
		statusTarget.SetGroupVersionKind(gvk)
		statusTarget.SetName(name)
		statusTarget.SetNamespace(namespace)
		if err := c.Status().Patch(ctx, statusTarget, client.RawPatch(types.ApplyPatchType, statusData), fieldOwner, client.ForceOwnership); err != nil {
			if !apierrors.IsNotFound(err) {
				return nil, fmt.Errorf("status release apply for %s/%s: %w", namespace, name, err)
			}
		} else {
			// Status Patch response is strictly newer.
			result = statusTarget
		}
	}

	return result, nil
}
