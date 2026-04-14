// apply.go contains the cluster-mutating operations for the Graph controller:
// resource apply (Own and Contribute), pruning, deletion ordering, and the
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
)

// ---------------------------------------------------------------------------
// SSA configuration
// ---------------------------------------------------------------------------

// applyAnnotation controls the SSA strategy per resource.
// Absent (default) → non-force SSA. applyAnnotationForce → force SSA.
const applyAnnotation = "kro.run/apply"

// applyAnnotationForce is the annotation value that enables force SSA.
const applyAnnotationForce = "Force"

// isForceApply checks whether a resource opts into force SSA via the
// kro.run/apply annotation.
func isForceApply(obj *unstructured.Unstructured) bool {
	ann := obj.GetAnnotations()
	return ann != nil && ann[applyAnnotation] == applyAnnotationForce
}

// graphFieldOwner returns the SSA field manager identity for a Graph.
// Per the design (003-ownership): experimental.kro.run/<namespace>/<name>
func graphFieldOwner(graph *unstructured.Unstructured) client.FieldOwner {
	return client.FieldOwner("experimental.kro.run/" + graph.GetNamespace() + "/" + graph.GetName())
}

// thirdPartyFieldManagers returns field manager names on the resource that are
// not this Graph's own manager and not the API server's defaulting manager.
// Only SSA Apply managers are considered — Update managers (from kubectl edit,
// plain client.Update, etc.) don't declare field ownership and shouldn't block
// deletion. Per 003-ownership.md: before deleting an Own resource, check
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

// runFinalization executes the finalization sequence for a prune candidate.
// Returns (true, nil) when all finalizer resources are ready and the target
// can be deleted. Returns (false, nil) when finalizers are in progress.
// Returns (false, err) when a finalizer can't be created.
//
// The sequence is recoverable — no state machine. Each call derives position
// from cluster state:
//  1. Finalizer resource doesn't exist → create it
//  2. Finalizer resource exists, readyWhen false → in progress
//  3. Finalizer resource exists, readyWhen true → this finalizer is done
//  4. All finalizers done → target can be deleted
func (r *GraphReconciler) runFinalization(
	ctx context.Context,
	graph *unstructured.Unstructured,
	target *unstructured.Unstructured,
	finalizerNodeIDs []string,
	dag *DAG,
	eval *evaluator,
	watcher *graphWatcher,
) (bool, []string, error) {
	logger := log.FromContext(ctx)
	var createdKeys []string

	// Put the target's data in scope so finalizer templates can reference it.
	// The target is still alive (no deletionTimestamp).
	for _, node := range dag.Nodes {
		if resourceKey(target) == resolvedResourceKey(node.Template, graph.GetNamespace()) {
			eval.scope[node.ID] = normalizeTypes(target.Object)
			break
		}
	}

	allReady := true
	for _, finNodeID := range finalizerNodeIDs {
		idx, ok := dag.Index[finNodeID]
		if !ok {
			return false, createdKeys, fmt.Errorf("finalizer node %q not found in DAG", finNodeID)
		}
		finNode := &dag.Nodes[idx]

		// forEach + finalizes: expand the collection and create one resource per item.
		if finNode.ForEach != nil {
			ready, fKeys, err := r.runForEachFinalization(ctx, graph, finNode, dag, eval, watcher)
			createdKeys = append(createdKeys, fKeys...)
			if err != nil {
				return false, createdKeys, err
			}
			if !ready {
				allReady = false
			}
			continue
		}

		// Single-resource finalizer (original path).
		evalMap, err := eval.toMap(finNode.Template)
		if err != nil {
			return false, createdKeys, fmt.Errorf("evaluating finalizer template %s: %w", finNodeID, err)
		}

		// Check if the finalizer resource already exists.
		finObj := &unstructured.Unstructured{Object: evalMap}
		if finObj.GetNamespace() == "" {
			finObj.SetNamespace(graph.GetNamespace())
		}
		existing := &unstructured.Unstructured{}
		existing.SetGroupVersionKind(finObj.GroupVersionKind())
		err = r.Client.Get(ctx, client.ObjectKey{
			Namespace: finObj.GetNamespace(),
			Name:      finObj.GetName(),
		}, existing)

		if err != nil {
			if !apierrors.IsNotFound(err) {
				return false, createdKeys, fmt.Errorf("checking finalizer resource %s: %w", finNodeID, err)
			}
			// Step 1: Finalizer resource doesn't exist — create it.
			logger.Info("creating finalizer resource", "finalizer", finNodeID,
				"target", target.GetName())
			applied, applyErr := r.applySSA(ctx, graph, evalMap, watcher, finNodeID, ReferenceOwn, false)
			if applyErr != nil {
				return false, createdKeys, fmt.Errorf("creating finalizer resource %s: %w", finNodeID, applyErr)
			}
			createdKeys = append(createdKeys, resourceKey(applied))
			eval.scope[finNodeID] = applied.Object
			allReady = false
			continue
		}

		// Finalizer resource exists — put it in scope and check readyWhen.
		eval.scope[finNodeID] = normalizeTypes(existing.Object)

		if len(finNode.ReadyWhen) > 0 {
			if err := eval.checkReadiness(finNode.ReadyWhen, eval.scope[finNodeID], finNodeID); err != nil {
				// Step 2: readyWhen not satisfied — in progress.
				logger.V(1).Info("finalizer not ready", "finalizer", finNodeID)
				allReady = false
				continue
			}
		}
		// Step 3: Finalizer is ready.
		logger.V(1).Info("finalizer ready", "finalizer", finNodeID)
	}

	return allReady, createdKeys, nil
}

// runForEachFinalization handles the forEach + finalizes case: expand a
// collection and create one finalizer resource per item. All children must
// reach readyWhen before the target can be deleted.
//
// Finalization children are self-contained and intentionally do not participate
// in the coordinator's forEach state tracking (forEachNewKeys, forEachNewScope,
// etc.). They are ephemeral artifacts of the finalization protocol — created to
// gate target deletion and cleaned up after. Their lifecycle is bounded by the
// finalization sequence, not by ongoing reconciliation.
func (r *GraphReconciler) runForEachFinalization(
	ctx context.Context,
	graph *unstructured.Unstructured,
	finNode *Node,
	dag *DAG,
	eval *evaluator,
	watcher *graphWatcher,
) (bool, []string, error) {
	logger := log.FromContext(ctx)
	var createdKeys []string
	allReady := true

	for varName, collectionExpr := range finNode.ForEach {
		collection, err := eval.evalString(collectionExpr)
		if err != nil {
			return false, createdKeys, fmt.Errorf("forEach finalizer %s: evaluating collection %q: %w", finNode.ID, collectionExpr, err)
		}

		items, ok := collection.([]any)
		if !ok {
			items = []any{collection}
		}
		logger.Info("forEach finalization expanding", "finalizer", finNode.ID, "var", varName, "count", len(items))

		for _, item := range items {
			innerScope := copyScope(eval.scope)
			innerScope[varName] = item
			innerEval := eval.withScope(innerScope)

			evalMap, err := innerEval.toMap(finNode.Template)
			if err != nil {
				return false, createdKeys, fmt.Errorf("forEach finalizer %s item: %w", finNode.ID, err)
			}

			// Set namespace default.
			childObj := &unstructured.Unstructured{Object: evalMap}
			if childObj.GetNamespace() == "" {
				childObj.SetNamespace(graph.GetNamespace())
			}

			// Stamp forEach child identity labels.
			gvk := childObj.GroupVersionKind()
			gv, _ := schema.ParseGroupVersion(childObj.GetAPIVersion())
			generation := fmt.Sprintf("%d", graph.GetGeneration())
			lbls := childObj.GetLabels()
			if lbls == nil {
				lbls = map[string]string{}
			}
			lbls = setForEachChildIdentityLabels(
				lbls, finNode.ID,
				childObj.GetName(), childObj.GetNamespace(),
				gvk.Kind, gv.Group,
				graph.GetName(), graph.GetNamespace(),
				generation, ReferenceOwn,
			)
			childObj.SetLabels(lbls)
			evalMap = childObj.Object

			// Check if this child already exists.
			existing := &unstructured.Unstructured{}
			existing.SetGroupVersionKind(childObj.GroupVersionKind())
			getErr := r.Client.Get(ctx, client.ObjectKey{
				Namespace: childObj.GetNamespace(),
				Name:      childObj.GetName(),
			}, existing)

			if getErr != nil {
				if !apierrors.IsNotFound(getErr) {
					return false, createdKeys, fmt.Errorf("checking forEach finalizer child %s/%s: %w", finNode.ID, childObj.GetName(), getErr)
				}
				// Child doesn't exist — create it.
				logger.Info("creating forEach finalizer child",
					"finalizer", finNode.ID, "name", childObj.GetName())
				applied, applyErr := r.applySSA(ctx, graph, evalMap, watcher, finNode.ID, ReferenceOwn, false)
				if applyErr != nil {
					return false, createdKeys, fmt.Errorf("creating forEach finalizer child %s/%s: %w", finNode.ID, childObj.GetName(), applyErr)
				}
				createdKeys = append(createdKeys, resourceKey(applied))
				allReady = false
				continue
			}

			// Child exists — check readyWhen.
			createdKeys = append(createdKeys, resourceKey(existing))
			if len(finNode.ReadyWhen) > 0 {
				innerEval.scope[finNode.ID] = normalizeTypes(existing.Object)
				if err := innerEval.checkReadiness(finNode.ReadyWhen, innerEval.scope[finNode.ID], finNode.ID); err != nil {
					logger.V(1).Info("forEach finalizer child not ready",
						"finalizer", finNode.ID, "name", existing.GetName())
					allReady = false
					continue
				}
			}
			logger.V(1).Info("forEach finalizer child ready",
				"finalizer", finNode.ID, "name", existing.GetName())
		}
	}

	return allReady, createdKeys, nil
}

// resolvedResourceKey builds a resource key from an already-evaluated
// template map, matching the key format used by resourceKey(). Unlike
// staticResourceKey, this operates on templates where CEL expressions have
// already been resolved — it reads metadata.namespace directly and never
// skips names containing ${...}. Used for finalizer target lookup during
// prune/teardown.
func resolvedResourceKey(tmpl map[string]any, defaultNS string) string {
	if tmpl == nil {
		return ""
	}
	apiVersion, _ := tmpl["apiVersion"].(string)
	kind, _ := tmpl["kind"].(string)
	md, _ := tmpl["metadata"].(map[string]any)
	if md == nil {
		return ""
	}
	name, _ := md["name"].(string)
	ns, _ := md["namespace"].(string)
	if ns == "" {
		ns = defaultNS
	}
	if apiVersion == "" || kind == "" || name == "" {
		return ""
	}
	gvk := schema.FromAPIVersionAndKind(apiVersion, kind)
	return fmt.Sprintf("%s/%s/%s/%s/%s", gvk.Group, gvk.Version, gvk.Kind, ns, name)
}

// ---------------------------------------------------------------------------
// Applied set key format
// ---------------------------------------------------------------------------
//
// Keys in the applied set identify resources the controller has written to.
// Two formats:
//
//   Own:        group/version/Kind/namespace/name
//   Contribute: contribute:group/version/Kind/namespace/name[+status]
//
// The "contribute:" prefix distinguishes resources where cleanup means
// skeleton apply (release field ownership) from resources where cleanup
// means delete. The "+status" suffix marks contributions that included
// status subresource fields, so skeleton apply must release both the
// main resource and the status subresource.
//
// resourceKey, contributeKey, and parseContributeKey are the sole
// constructors and parsers for these formats.

// contributeKeyPrefix distinguishes Contribute keys from Own keys.
const contributeKeyPrefix = "contribute:"

// contributeStatusSuffix marks that the contribution included status fields.
const contributeStatusSuffix = "+status"

func resourceKey(obj *unstructured.Unstructured) string {
	gvk := obj.GroupVersionKind()
	return strings.Join([]string{gvk.Group, gvk.Version, gvk.Kind, obj.GetNamespace(), obj.GetName()}, "/")
}

// staticResourceKey builds a resource key from an unevaluated template's
// metadata fields. Skips templates with CEL expressions in the name
// (can't determine key statically). Uses the template's literal
// metadata.namespace when present; falls back to fallbackNamespace when
// the namespace is absent, empty, or contains ${...} expressions.
// This is the spec-time equivalent of resourceKey — used during prune
// diffing and revision spec scanning where templates haven't been evaluated.
func staticResourceKey(tmpl map[string]any, fallbackNamespace string) string {
	apiVersion, _ := tmpl["apiVersion"].(string)
	kind, _ := tmpl["kind"].(string)
	md, _ := tmpl["metadata"].(map[string]any)
	if md == nil {
		return ""
	}
	name, _ := md["name"].(string)
	if name == "" || strings.Contains(name, "${") {
		return "" // dynamic name — can't determine key statically
	}
	ns, _ := md["namespace"].(string)
	if ns == "" || strings.Contains(ns, "${") {
		ns = fallbackNamespace
	}
	gv, _ := schema.ParseGroupVersion(apiVersion)
	gvk := gv.WithKind(kind)
	return strings.Join([]string{gvk.Group, gvk.Version, gvk.Kind, ns, name}, "/")
}

func parseResourceKey(key string) (schema.GroupVersionKind, types.NamespacedName) {
	parts := strings.SplitN(key, "/", 5)
	if len(parts) != 5 {
		return schema.GroupVersionKind{}, types.NamespacedName{}
	}
	return schema.GroupVersionKind{Group: parts[0], Version: parts[1], Kind: parts[2]},
		types.NamespacedName{Namespace: parts[3], Name: parts[4]}
}

// contributeKey builds a Contribute applied set key.
func contributeKey(obj *unstructured.Unstructured, hasStatus bool) string {
	key := contributeKeyPrefix + resourceKey(obj)
	if hasStatus {
		key += contributeStatusSuffix
	}
	return key
}

// parseContributeKey extracts the resource key and status flag from a
// contribute applied set key. Returns ("", false) if not a contribute key.
func parseContributeKey(key string) (resKey string, hasStatus bool) {
	if !strings.HasPrefix(key, contributeKeyPrefix) {
		return "", false
	}
	rest := strings.TrimPrefix(key, contributeKeyPrefix)
	if strings.HasSuffix(rest, contributeStatusSuffix) {
		return strings.TrimSuffix(rest, contributeStatusSuffix), true
	}
	return rest, false
}

// templateHasStatus returns true if a template map contains a non-nil
// status field. Used during teardown to determine whether skeleton apply
// must also release the status subresource.
func templateHasStatus(tmpl map[string]any) bool {
	s, ok := tmpl["status"]
	return ok && s != nil
}

// ---------------------------------------------------------------------------
// Apply
// ---------------------------------------------------------------------------

// applySSA applies a template via server-side apply (SSA). Handles both Own
// and Contribute references — the ref parameter controls:
//   - Identity labels: Own skips if present (forEach children stamp their own),
//     Contribute always stamps.
//   - Pre-apply check: Own does a kro label check (cross-Graph ownership guard),
//     Contribute checks target existence (contributions patch into existing resources).
//   - Cache miss on NotFound: Own clears cache + returns ErrPending,
//     Contribute falls through to apply.
//   - Apply hash annotation: Own only (Contribute targets are owned by others).
//
// driftCorrection bypasses the content-addressed apply hash check.
// Per 004-graph-execution.md § The Walk: "The drift timer bypasses the
// template-hash check — apply unconditionally, because server-side
// defaulters and mutating webhooks can change fields without changing
// the desired state hash. SSA is idempotent; the apply corrects drift
// as a side effect."
func (r *GraphReconciler) applySSA(ctx context.Context, graph *unstructured.Unstructured, evalMap map[string]any, watcher *graphWatcher, nodeID string, ref Reference, driftCorrection bool) (*unstructured.Unstructured, error) {
	fieldOwner := graphFieldOwner(graph)
	obj := &unstructured.Unstructured{Object: evalMap}

	if obj.GetNamespace() == "" {
		obj.SetNamespace(graph.GetNamespace())
	}

	// Stamp identity labels per 004-graph-execution.md § Storage model.
	generation := fmt.Sprintf("%d", graph.GetGeneration())
	lbls := obj.GetLabels()
	if lbls == nil {
		lbls = map[string]string{}
	}
	if ref == ReferenceOwn {
		// Own: skip stamping if identity labels are already present (e.g., forEach
		// children stamp their own child-scoped labels before calling applySSA).
		if !hasGraphIdentityLabels(lbls, graph.GetName(), graph.GetNamespace()) {
			lbls = setIdentityLabels(lbls, nodeID, graph.GetName(), graph.GetNamespace(), generation, ReferenceOwn)
			obj.SetLabels(lbls)
		}
	} else {
		// Contribute: always stamp identity labels so resources are discoverable via
		// deriveAppliedSet() after controller restart.
		lbls = setIdentityLabels(lbls, nodeID, graph.GetName(), graph.GetNamespace(), generation, ReferenceContribute)
		obj.SetLabels(lbls)
	}

	// Buffer a watch for this resource (flushed at done(true)).
	gvr := gvkToGVR(obj.GroupVersionKind())
	if watcher != nil {
		watcher.watchScalar(nodeID, gvr, obj.GetName(), obj.GetNamespace())
	}

	// Content-addressed apply: hash the desired state to detect changes.
	applyHash, err := hashDesiredState(obj.Object)
	if err != nil {
		return nil, fmt.Errorf("hashing template for %s: %w", obj.GetName(), err)
	}
	cacheKey := resourceCacheKey(obj.GetAPIVersion(), obj.GetKind(), obj.GetNamespace(), obj.GetName())

	// Drift correction bypasses the cache check entirely — the drift timer's
	// purpose is to re-apply unconditionally so SSA corrects any live-state
	// divergence from the desired state.
	if !driftCorrection {
		if cached, ok := r.Resources.get(cacheKey); ok && cached.applyHash == applyHash {
			if watcher != nil {
				liveRV := watcher.getResourceVersion(gvr, obj.GetNamespace(), obj.GetName())
				if liveRV != "" && liveRV == cached.resourceVersion {
					return &unstructured.Unstructured{Object: cached.object}, nil
				}
			}
			readBack := &unstructured.Unstructured{}
			readBack.SetGroupVersionKind(obj.GroupVersionKind())
			if err := r.Client.Get(ctx, client.ObjectKey{Namespace: obj.GetNamespace(), Name: obj.GetName()}, readBack); err != nil {
				if !apierrors.IsNotFound(err) {
					return nil, fmt.Errorf("reading %s: %w", obj.GetName(), err)
				}
				if ref == ReferenceOwn {
					// Own: externally deleted. Clear cache + ErrPending.
					r.Resources.remove(cacheKey)
					return nil, fmt.Errorf("resource %s externally deleted: %w", obj.GetName(), ErrPending)
				}
				// Contribute: object might not exist yet (race), fall through to apply
			} else {
				r.Resources.set(cacheKey, &cachedObject{
					resourceVersion: readBack.GetResourceVersion(),
					applyHash:       applyHash,
					object:          readBack.Object,
				})
				return readBack, nil
			}
		}
	} // !driftCorrection

	// Own: set the apply hash annotation for future comparisons.
	if ref == ReferenceOwn {
		annotations := obj.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[applyHashAnnotation] = applyHash
		obj.SetAnnotations(annotations)
	}

	forceApply := isForceApply(obj)

	// Pre-apply check differs by reference type.
	if ref == ReferenceOwn {
		// kro label check: if the existing resource has a different Graph's identity
		// label, require Force to proceed. Prevents accidental cross-Graph ownership.
		existing := &unstructured.Unstructured{}
		existing.SetGroupVersionKind(obj.GroupVersionKind())
		if getErr := r.Client.Get(ctx, client.ObjectKey{Namespace: obj.GetNamespace(), Name: obj.GetName()}, existing); getErr == nil {
			existingLabels := existing.GetLabels()
			if otherGraph, found := hasOtherGraphIdentityLabel(existingLabels, graph.GetName(), graph.GetNamespace()); found {
				if !forceApply {
					return nil, fmt.Errorf("resource %s/%s %s owned by Graph %q, not %q (use kro.run/apply: Force to take ownership): %w",
						obj.GetAPIVersion(), obj.GetKind(), obj.GetName(), otherGraph, graph.GetName(), ErrFieldConflict)
				}
			}
		}
	} else {
		// Contribute: target must exist — contributions patch into existing resources.
		targetCheck := &unstructured.Unstructured{}
		targetCheck.SetGroupVersionKind(obj.GroupVersionKind())
		if err := r.Client.Get(ctx, client.ObjectKey{Namespace: obj.GetNamespace(), Name: obj.GetName()}, targetCheck); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, fmt.Errorf("contribute target %s/%s %s/%s not found: %w", obj.GetAPIVersion(), obj.GetKind(), obj.GetNamespace(), obj.GetName(), ErrPending)
			}
			return nil, fmt.Errorf("checking contribute target %s: %w", obj.GetName(), err)
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
			return nil, fmt.Errorf("SSA conflict on %s/%s %s: %w: %w", obj.GetAPIVersion(), obj.GetKind(), obj.GetName(), ErrFieldConflict, err)
		}
		return nil, fmt.Errorf("applying %s/%s %s: %w", obj.GetAPIVersion(), obj.GetKind(), obj.GetName(), err)
	}

	// Status subresource patch if .status is present.
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
		statusTarget := &unstructured.Unstructured{}
		statusTarget.SetGroupVersionKind(obj.GroupVersionKind())
		statusTarget.SetName(obj.GetName())
		statusTarget.SetNamespace(obj.GetNamespace())
		var statusOpts []client.SubResourcePatchOption
		statusOpts = append(statusOpts, fieldOwner)
		if forceApply {
			statusOpts = append(statusOpts, client.ForceOwnership)
		}
		if err := r.Client.Status().Patch(ctx, statusTarget, client.RawPatch(types.ApplyPatchType, sData), statusOpts...); err != nil {
			r.Resources.remove(cacheKey)
			if apierrors.IsConflict(err) {
				return nil, fmt.Errorf("SSA status conflict on %s/%s %s: %w: %w", obj.GetAPIVersion(), obj.GetKind(), obj.GetName(), ErrFieldConflict, err)
			}
			return nil, fmt.Errorf("applying status %s/%s %s: %w", obj.GetAPIVersion(), obj.GetKind(), obj.GetName(), err)
		}
	}

	// Read back the full object to populate scope.
	readBack := &unstructured.Unstructured{}
	readBack.SetGroupVersionKind(obj.GroupVersionKind())
	if err := r.Client.Get(ctx, client.ObjectKeyFromObject(applied), readBack); err != nil {
		return nil, fmt.Errorf("reading back %s: %w", obj.GetName(), err)
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
// set. Returns (finalizerKeys, deferredKeys, err):
//   - finalizerKeys: keys of finalizer resources created this cycle
//   - deferredKeys: keys of resources whose deletion was deferred this cycle
//     (finalization in progress, blocked by third-party field managers, etc.).
//     The caller must include these in deferredPruneKeys for the next
//     reconcile so they remain visible as prune candidates, AND must not GC
//     superseded revisions while any deferral is active — the superseded DAG's
//     finalizer relationships are still needed to complete the sequence.
//   - err: hard error from an API call; sets pruneOK=false at the call site.
func (r *GraphReconciler) pruneRemovedResources(ctx context.Context, graph *unstructured.Unstructured, previousKeys map[string]bool, currentKeys []string, dag *DAG, supersededDAGs map[string]*DAG, eval *evaluator, watcher *graphWatcher) ([]string, []string, error) {
	logger := log.FromContext(ctx)
	var finalizerKeys []string
	var deferredKeys []string

	// Build current key set for fast lookup
	currentSet := map[string]bool{}
	for _, k := range currentKeys {
		currentSet[k] = true
	}

	// Collect all DAGs (active + superseded) for finalizer lookups.
	// The old revision's finalizes declarations govern prune of its resources.
	allDAGs := []*DAG{}
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
			if node.Template != nil {
				if rk := staticResourceKey(node.Template, graph.GetNamespace()); rk != "" {
					keyToNodeID[rk] = node.ID
					nodeIDToKey[node.ID] = rk
				}
			}
		}
	}

	// findFinalizers looks up finalizer node IDs for a target across all DAGs.
	// Returns the first DAG that declares finalizers for this target, plus the IDs.
	findFinalizers := func(nodeID string) (*DAG, []string) {
		for _, d := range allDAGs {
			if fins, ok := d.Finalizers[nodeID]; ok && len(fins) > 0 {
				return d, fins
			}
		}
		return nil, nil
	}

	// Build the set of keys that must be deferred because an in-flight
	// finalizer references them. If a prune target has finalizer nodes,
	// each finalizer node's direct dependencies (other than the target
	// itself) are deferred — they must remain alive while the finalizer
	// is running.
	finalizerDeps := map[string]bool{}
	for key := range previousKeys {
		if currentSet[key] {
			continue
		}
		nodeID := keyToNodeID[key]
		finDAG, finalizerNodeIDs := findFinalizers(nodeID)
		if finDAG == nil {
			continue
		}
		for _, finNodeID := range finalizerNodeIDs {
			finIdx, exists := finDAG.Index[finNodeID]
			if !exists {
				continue
			}
			for depID := range finDAG.Nodes[finIdx].Dependencies {
				if depID == nodeID {
					continue // skip the target itself
				}
				if dk, ok := nodeIDToKey[depID]; ok {
					finalizerDeps[dk] = true
				}
			}
		}
	}

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
	pruneCandidates = pruneOrder(pruneCandidates, allDAGs, graph.GetNamespace())

	for _, key := range pruneCandidates {
		// Defer deletion of resources that are dependencies of in-flight
		// finalizer nodes — they must remain alive while the finalizer runs.
		if finalizerDeps[key] {
			logger.Info("prune deferred: resource is a dependency of an in-flight finalizer",
				"key", key)
			deferredKeys = append(deferredKeys, key)
			continue
		}

		// Contribute keys use skeleton apply (release fields), not delete.
		if resKey, hasStatus := parseContributeKey(key); resKey != "" {
			gvk, nn := parseResourceKey(resKey)
			if gvk.Kind == "" {
				continue
			}
			if err := skeletonApply(ctx, r.Client, gvk, nn.Namespace, nn.Name, fieldOwner, hasStatus); err != nil {
				logger.Error(err, "releasing contribution fields", "key", resKey)
			} else {
				logger.Info("released contribution fields", "key", resKey)
				r.Resources.remove(resKey)
			}
			continue
		}

		// Own keys: delete the resource.
		gvk, nn := parseResourceKey(key)
		if gvk.Kind == "" {
			continue
		}

		// Check if it exists and is ours before deleting.
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(gvk)
		if err := r.Client.Get(ctx, nn, obj); err != nil {
			// Per 004-graph-execution.md § Finalization: "If the target resource
			// does not exist in the cluster (creation failed, already deleted
			// externally), there is nothing to finalize. The controller skips
			// finalization and proceeds with cleanup."
			if nodeID := keyToNodeID[key]; nodeID != "" {
				if _, finalizerNodeIDs := findFinalizers(nodeID); len(finalizerNodeIDs) > 0 {
					logger.Info("finalization skipped: target resource does not exist",
						"key", key, "finalizers", finalizerNodeIDs)
				}
			}
			r.Resources.remove(key)
			continue // already gone
		}

		// Verify ownership: must have our identity label and apply hash
		objLabels := obj.GetLabels()
		hasOurLabel := false
		if objLabels != nil {
			for key := range objLabels {
				if isGraphIdentityLabel(key, graph.GetName(), graph.GetNamespace()) {
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
			deferredKeys = append(deferredKeys, key)
			continue // resource stays in applied set — retry next reconcile
		}

		// Finalization: if this target has finalizer nodes (from any revision),
		// run the finalization sequence before deleting.
		nodeID := keyToNodeID[key]
		if finDAG, finalizerNodeIDs := findFinalizers(nodeID); finDAG != nil {
			ready, fKeys, err := r.runFinalization(ctx, graph, obj, finalizerNodeIDs, finDAG, eval, watcher)
			finalizerKeys = append(finalizerKeys, fKeys...)
			if err != nil {
				logger.Error(err, "finalization failed", "key", key)
				continue // block deletion — TeardownBlocked
			}
			if !ready {
				logger.Info("finalization in progress — deletion deferred",
					"key", key, "finalizers", finalizerNodeIDs)
				deferredKeys = append(deferredKeys, key)
				continue // block deletion until all finalizers ready
			}
			logger.Info("finalization complete", "key", key)
		}

		if err := r.Client.Delete(ctx, obj); err != nil {
			if client.IgnoreNotFound(err) != nil {
				return finalizerKeys, deferredKeys, fmt.Errorf("pruning %s: %w", key, err)
			}
		} else {
			logger.Info("pruned resource", "key", key)
			r.Resources.remove(key)
		}
	}

	return finalizerKeys, deferredKeys, nil
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
			if node.Template == nil {
				continue
			}
			ref := DetectReference(node.Template)
			if ref == ReferenceWatch || ref == ReferenceWatchKind {
				continue // read-only references don't create resources
			}
			apiVersion, _ := node.Template["apiVersion"].(string)
			kind, _ := node.Template["kind"].(string)
			if apiVersion == "" || kind == "" {
				continue
			}
			gv, _ := schema.ParseGroupVersion(apiVersion)
			gvkSet[gv.WithKind(kind)] = true
		}
	}

	var keys []string
	suffix := graphLabelSuffix(graph.GetName(), graph.GetNamespace())
	for gvk := range gvkSet {
		list := &unstructured.UnstructuredList{}
		listGVK := gvk
		listGVK.Kind = gvk.Kind + "List"
		list.SetGroupVersionKind(listGVK)

		// Cannot use label selector with DNS subdomain keys — list all and
		// filter client-side. This only runs during teardown (not hot path).
		if err := r.Client.List(ctx, list, &client.ListOptions{
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
func pruneOrder(keys []string, dags []*DAG, defaultNS string) []string {
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
			if node.Template == nil {
				continue
			}
			rk := staticResourceKey(node.Template, defaultNS)
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
			// Also check contribute keys against their underlying resource key.
			if resKey, _ := parseContributeKey(key); resKey != "" {
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
// topological order from the DAG. Rebuilds the DAG from the Graph spec.
// Delegates to pruneOrder for the actual ordering logic.
//
// Returns an error if the Graph spec cannot be parsed or the DAG cannot
// be built. Per the design (004-graph-execution): "Teardown is blocked
// until ordering is available — never degrade to unordered deletion."
func (r *GraphReconciler) deletionOrder(graph *unstructured.Unstructured, keys []string) ([]string, error) {
	graphSpec, err := extractGraphSpec(graph.Object)
	if err != nil {
		return nil, fmt.Errorf("extracting graph spec for deletion order: %w", err)
	}
	dag, err := BuildDAG(graphSpec.Nodes, nil)
	if err != nil {
		return nil, fmt.Errorf("building DAG for deletion order: %w", err)
	}
	return pruneOrder(keys, []*DAG{dag}, graph.GetNamespace()), nil
}

// ---------------------------------------------------------------------------
// Skeleton apply — release field ownership without deleting the object
// ---------------------------------------------------------------------------

func skeletonApply(ctx context.Context, c client.Client, gvk schema.GroupVersionKind, namespace, name string, fieldOwner client.FieldOwner, hasStatus bool) error {
	apiVersion := gvk.Group + "/" + gvk.Version
	if gvk.Group == "" {
		apiVersion = gvk.Version
	}
	skeleton := map[string]any{
		"apiVersion": apiVersion,
		"kind":       gvk.Kind,
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
		},
	}

	data, err := json.Marshal(skeleton)
	if err != nil {
		return fmt.Errorf("marshaling skeleton: %w", err)
	}

	target := &unstructured.Unstructured{}
	target.SetGroupVersionKind(gvk)
	target.SetName(name)
	target.SetNamespace(namespace)
	if err := c.Patch(ctx, target, client.RawPatch(types.ApplyPatchType, data), fieldOwner, client.ForceOwnership); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("skeleton apply for %s/%s: %w", namespace, name, err)
	}

	if hasStatus {
		statusData, err := json.Marshal(skeleton)
		if err != nil {
			return fmt.Errorf("marshaling status skeleton: %w", err)
		}
		statusTarget := &unstructured.Unstructured{}
		statusTarget.SetGroupVersionKind(gvk)
		statusTarget.SetName(name)
		statusTarget.SetNamespace(namespace)
		if err := c.Status().Patch(ctx, statusTarget, client.RawPatch(types.ApplyPatchType, statusData), fieldOwner, client.ForceOwnership); err != nil {
			if !apierrors.IsNotFound(err) {
				return fmt.Errorf("skeleton status apply for %s/%s: %w", namespace, name, err)
			}
		}
	}

	return nil
}
