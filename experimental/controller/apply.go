// apply.go contains the cluster-mutating operations for the Graph controller:
// resource apply (Owns and Contribute), pruning, deletion ordering, and the
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
	"k8s.io/apimachinery/pkg/labels"
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
// Per the design (003-ownership): kro.run/<namespace>/<name>
func graphFieldOwner(graph *unstructured.Unstructured) client.FieldOwner {
	return client.FieldOwner("kro.run/" + graph.GetNamespace() + "/" + graph.GetName())
}

// thirdPartyFieldManagers returns field manager names on the resource that are
// not this Graph's own manager and not the API server's defaulting manager.
// Only SSA Apply managers are considered — Update managers (from kubectl edit,
// plain client.Update, etc.) don't declare field ownership and shouldn't block
// deletion. Per 003-ownership.md: before deleting an Owns resource, check
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
		if resourceKey(target) == resourceKeyFromObj(node.Template, graph.GetNamespace()) {
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

		// Evaluate the finalizer template.
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
			applied, applyErr := r.applyResource(ctx, graph, evalMap, watcher, finNodeID)
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

// resourceKeyFromObj builds a resource key from a template map, matching
// the key format used by resourceKey(). Used to map template content to
// resource keys for finalizer lookup.
func resourceKeyFromObj(tmpl map[string]any, defaultNS string) string {
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
//   Owns:       group/version/Kind/namespace/name
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

// contributeKeyPrefix distinguishes Contribute keys from Owns keys.
const contributeKeyPrefix = "contribute:"

// contributeStatusSuffix marks that the contribution included status fields.
const contributeStatusSuffix = "+status"

func resourceKey(obj *unstructured.Unstructured) string {
	gvk := obj.GroupVersionKind()
	return strings.Join([]string{gvk.Group, gvk.Version, gvk.Kind, obj.GetNamespace(), obj.GetName()}, "/")
}

// resourceKeyFromTemplate builds an Owns key from template metadata fields.
// This is the static-name equivalent of resourceKey — used when the resource
// hasn't been applied yet (e.g., during spec-based prune diffing).
func resourceKeyFromTemplate(tmpl map[string]any, fallbackNamespace string) string {
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
	gv, _ := schema.ParseGroupVersion(apiVersion)
	gvk := gv.WithKind(kind)
	return strings.Join([]string{gvk.Group, gvk.Version, gvk.Kind, fallbackNamespace, name}, "/")
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
func parseContributeKey(key string) (resourceKey string, hasStatus bool) {
	if !strings.HasPrefix(key, contributeKeyPrefix) {
		return "", false
	}
	rest := strings.TrimPrefix(key, contributeKeyPrefix)
	if strings.HasSuffix(rest, contributeStatusSuffix) {
		return strings.TrimSuffix(rest, contributeStatusSuffix), true
	}
	return rest, false
}

// ---------------------------------------------------------------------------
// Apply
// ---------------------------------------------------------------------------

func (r *GraphReconciler) applyResource(ctx context.Context, graph *unstructured.Unstructured, evalMap map[string]any, watcher *graphWatcher, nodeID string) (*unstructured.Unstructured, error) {
	fieldOwner := graphFieldOwner(graph)
	obj := &unstructured.Unstructured{Object: evalMap}

	if obj.GetNamespace() == "" {
		obj.SetNamespace(graph.GetNamespace())
	}

	// Label managed resources for visibility and selector queries.
	// Labels work for both namespaced and cluster-scoped resources,
	// unlike owner references which require same-scope.
	lbls := obj.GetLabels()
	if lbls == nil {
		lbls = map[string]string{}
	}
	lbls[LabelGraphName] = graph.GetName()
	lbls[LabelGraphNamespace] = graph.GetNamespace()
	obj.SetLabels(lbls)

	// Watch before apply — ensures the metadata informer is running.
	// Use the DAG node ID for the watch registration so that scoped walks
	// can map watch events back to DAG nodes.
	gvr := gvkToGVR(obj.GroupVersionKind())
	if watcher != nil {
		watcher.watchScalar(nodeID, gvr, obj.GetName(), obj.GetNamespace())
	}

	// Content-addressed apply: hash the desired state to detect changes.
	// The hash is computed before adding the hash annotation itself.
	templateHash, err := hashDesiredState(obj.Object)
	if err != nil {
		return nil, fmt.Errorf("hashing template for %s: %w", obj.GetName(), err)
	}
	cacheKey := resourceCacheKey(obj.GetAPIVersion(), obj.GetKind(), obj.GetNamespace(), obj.GetName())

	// Check the resource cache + metadata informer for change detection.
	if cached, ok := r.Resources.get(cacheKey); ok && cached.templateHash == templateHash {
		// Our desired state hasn't changed. Check if the live object has changed
		// (e.g., status updated by another controller) via the metadata informer.
		if watcher != nil {
			liveRV := watcher.getResourceVersion(gvr, obj.GetNamespace(), obj.GetName())
			if liveRV != "" && liveRV == cached.resourceVersion {
				// Nothing changed at all — skip both Patch and GET.
				return &unstructured.Unstructured{Object: cached.object}, nil
			}
		}
		// Metadata changed (status update, etc.) but our template hasn't.
		// Skip the Patch, but GET to refresh the scope with current status.
		readBack := &unstructured.Unstructured{}
		readBack.SetGroupVersionKind(obj.GroupVersionKind())
		if err := r.Client.Get(ctx, client.ObjectKey{Namespace: obj.GetNamespace(), Name: obj.GetName()}, readBack); err != nil {
			if !apierrors.IsNotFound(err) {
				return nil, fmt.Errorf("reading %s: %w", obj.GetName(), err)
			}
			// Object might not exist yet (race), fall through to apply
		} else {
			// Update the cache with fresh data
			r.Resources.set(cacheKey, &cachedObject{
				resourceVersion: readBack.GetResourceVersion(),
				templateHash:    templateHash,
				object:          readBack.Object,
			})
			return readBack, nil
		}
	}

	// Set the template hash annotation for future comparisons.
	annotations := obj.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[templateHashAnnotation] = templateHash
	obj.SetAnnotations(annotations)

	data, err := json.Marshal(obj.Object)
	if err != nil {
		return nil, fmt.Errorf("marshaling: %w", err)
	}

	applied := &unstructured.Unstructured{}
	applied.SetGroupVersionKind(obj.GroupVersionKind())
	applied.SetName(obj.GetName())
	applied.SetNamespace(obj.GetNamespace())

	forceApply := isForceApply(obj)

	// kro label check: if the existing resource has a different Graph's label,
	// require Force to proceed. Prevents accidental cross-Graph ownership.
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(obj.GroupVersionKind())
	if getErr := r.Client.Get(ctx, client.ObjectKey{Namespace: obj.GetNamespace(), Name: obj.GetName()}, existing); getErr == nil {
		existingLabels := existing.GetLabels()
		if existingLabels != nil {
			if ownerGraph, ok := existingLabels[LabelGraphName]; ok && ownerGraph != graph.GetName() {
				if !forceApply {
					return nil, fmt.Errorf("resource %s/%s %s owned by Graph %q, not %q (use kro.run/apply: Force to take ownership): %w",
						obj.GetAPIVersion(), obj.GetKind(), obj.GetName(), ownerGraph, graph.GetName(), ErrFieldConflict)
				}
			}
		}
	}

	var patchOpts []client.PatchOption
	patchOpts = append(patchOpts, fieldOwner)
	if forceApply {
		patchOpts = append(patchOpts, client.ForceOwnership)
	}

	if err := r.Client.Patch(ctx, applied, client.RawPatch(types.ApplyPatchType, data), patchOpts...); err != nil {
		if apierrors.IsConflict(err) {
			return nil, fmt.Errorf("SSA conflict on %s/%s %s: %w: %w", obj.GetAPIVersion(), obj.GetKind(), obj.GetName(), ErrFieldConflict, err)
		}
		return nil, fmt.Errorf("applying %s/%s %s: %w", obj.GetAPIVersion(), obj.GetKind(), obj.GetName(), err)
	}

	// Use the Patch response directly — it contains the full post-apply state.
	// Read back to get status fields that the Patch response may not include.
	readBack := &unstructured.Unstructured{}
	readBack.SetGroupVersionKind(obj.GroupVersionKind())
	if err := r.Client.Get(ctx, client.ObjectKeyFromObject(applied), readBack); err != nil {
		return nil, fmt.Errorf("reading back %s: %w", obj.GetName(), err)
	}

	// Populate the resource cache.
	r.Resources.set(cacheKey, &cachedObject{
		resourceVersion: readBack.GetResourceVersion(),
		templateHash:    templateHash,
		object:          readBack.Object,
	})

	return readBack, nil
}

// applyContribution applies a contribution resource — partial SSA.
// Contributions intentionally write fields on objects someone else owns.
// Auto-splits into two API calls when the template contains status fields:
//   - Regular SSA patch for metadata/spec/other fields (skips .status)
//   - Status subresource patch for .status fields
//
// Hash-gated: if the contribution's evaluated output hasn't changed, the Patch
// is skipped. When it does change, the new state is applied.
// Uses non-force SSA by default; force only with kro.run/apply: Force.
// Surfaces SSA conflicts as ErrFieldConflict.
func (r *GraphReconciler) applyContribution(ctx context.Context, graph *unstructured.Unstructured, evalMap map[string]any, watcher *graphWatcher, nodeID string) (*unstructured.Unstructured, error) {
	fieldOwner := graphFieldOwner(graph)
	obj := &unstructured.Unstructured{Object: evalMap}

	if obj.GetNamespace() == "" {
		obj.SetNamespace(graph.GetNamespace())
	}

	// Watch the target resource for reactive updates.
	// Use the DAG node ID for scoped walk trigger resolution.
	gvr := gvkToGVR(obj.GroupVersionKind())
	if watcher != nil {
		watcher.watchScalar(nodeID, gvr, obj.GetName(), obj.GetNamespace())
	}

	// Content-addressed apply: hash the desired state to detect changes.
	templateHash, err := hashDesiredState(evalMap)
	if err != nil {
		return nil, fmt.Errorf("hashing contribution for %s: %w", obj.GetName(), err)
	}
	cacheKey := resourceCacheKey(obj.GetAPIVersion(), obj.GetKind(), obj.GetNamespace(), obj.GetName())

	// Check cache for hash match — skip Patch if contribution output unchanged.
	if cached, ok := r.Resources.get(cacheKey); ok && cached.templateHash == templateHash {
		if watcher != nil {
			liveRV := watcher.getResourceVersion(gvr, obj.GetNamespace(), obj.GetName())
			if liveRV != "" && liveRV == cached.resourceVersion {
				return &unstructured.Unstructured{Object: cached.object}, nil
			}
		}
		// Metadata changed but our contribution hasn't — GET to refresh.
		readBack := &unstructured.Unstructured{}
		readBack.SetGroupVersionKind(obj.GroupVersionKind())
		if err := r.Client.Get(ctx, client.ObjectKey{Namespace: obj.GetNamespace(), Name: obj.GetName()}, readBack); err != nil {
			if !apierrors.IsNotFound(err) {
				return nil, fmt.Errorf("reading %s: %w", obj.GetName(), err)
			}
			// Object might not exist yet (race), fall through to apply
		} else {
			r.Resources.set(cacheKey, &cachedObject{
				resourceVersion: readBack.GetResourceVersion(),
				templateHash:    templateHash,
				object:          readBack.Object,
			})
			return readBack, nil
		}
	}

	// Target must exist — contributions patch into existing resources.
	targetCheck := &unstructured.Unstructured{}
	targetCheck.SetGroupVersionKind(obj.GroupVersionKind())
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: obj.GetNamespace(), Name: obj.GetName()}, targetCheck); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("contribute target %s/%s %s/%s not found: %w", obj.GetAPIVersion(), obj.GetKind(), obj.GetNamespace(), obj.GetName(), ErrDataPending)
		}
		return nil, fmt.Errorf("checking contribute target %s: %w", obj.GetName(), err)
	}

	forceApply := isForceApply(obj)

	var patchOpts []client.PatchOption
	patchOpts = append(patchOpts, fieldOwner)
	if forceApply {
		patchOpts = append(patchOpts, client.ForceOwnership)
	}

	// Regular SSA for non-status fields. Strip .status to avoid silent
	// stripping by the API server when the status subresource is enabled.
	mainPayload := make(map[string]any, len(evalMap))
	for k, v := range evalMap {
		if k != "status" {
			mainPayload[k] = v
		}
	}
	data, err := json.Marshal(mainPayload)
	if err != nil {
		return nil, fmt.Errorf("marshaling contribution: %w", err)
	}
	target := &unstructured.Unstructured{}
	target.SetGroupVersionKind(obj.GroupVersionKind())
	target.SetName(obj.GetName())
	target.SetNamespace(obj.GetNamespace())
	if err := r.Client.Patch(ctx, target, client.RawPatch(types.ApplyPatchType, data), patchOpts...); err != nil {
		if apierrors.IsConflict(err) {
			return nil, fmt.Errorf("SSA conflict on contribution %s/%s %s: %w: %w", obj.GetAPIVersion(), obj.GetKind(), obj.GetName(), ErrFieldConflict, err)
		}
		return nil, fmt.Errorf("applying contribution %s/%s %s: %w", obj.GetAPIVersion(), obj.GetKind(), obj.GetName(), err)
	}

	// Status subresource patch if .status is present
	if status, ok := evalMap["status"]; ok && status != nil {
		statusPayload := map[string]any{
			"apiVersion": obj.GetAPIVersion(),
			"kind":       obj.GetKind(),
			"metadata": map[string]any{
				"name":      obj.GetName(),
				"namespace": obj.GetNamespace(),
			},
			"status": status,
		}
		data, err := json.Marshal(statusPayload)
		if err != nil {
			return nil, fmt.Errorf("marshaling status contribution: %w", err)
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
		if err := r.Client.Status().Patch(ctx, statusTarget, client.RawPatch(types.ApplyPatchType, data), statusOpts...); err != nil {
			if apierrors.IsConflict(err) {
				return nil, fmt.Errorf("SSA status conflict on contribution %s/%s %s: %w: %w", obj.GetAPIVersion(), obj.GetKind(), obj.GetName(), ErrFieldConflict, err)
			}
			return nil, fmt.Errorf("applying status contribution %s/%s %s: %w", obj.GetAPIVersion(), obj.GetKind(), obj.GetName(), err)
		}
	}

	// Read back the full object to populate scope.
	readBack := &unstructured.Unstructured{}
	readBack.SetGroupVersionKind(obj.GroupVersionKind())
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: obj.GetNamespace(), Name: obj.GetName()}, readBack); err != nil {
		return nil, fmt.Errorf("reading back %s after contribution: %w", obj.GetName(), err)
	}

	r.Resources.set(cacheKey, &cachedObject{
		resourceVersion: readBack.GetResourceVersion(),
		templateHash:    templateHash,
		object:          readBack.Object,
	})

	return readBack, nil
}

// ---------------------------------------------------------------------------
// Prune + delete ordering
// ---------------------------------------------------------------------------

// pruneRemovedResources deletes managed resources that were previously applied
// but are no longer in the current reconcile's key set.
// The prune candidate set is: previousKeys - currentKeys.
//
// For Owns resources, this issues a Delete. For Contribute resources
// (prefixed with "contribute:"), this issues a skeleton apply to release
// field ownership without deleting the target.
//
// If a prune candidate has finalizer nodes declared (via `finalizes`), the
// finalization sequence runs before deletion: create the finalizer resource,
// wait for readyWhen, then delete the target. See 004-graph-execution.md § Finalization.
//
// supersededDAGs provides DAGs from superseded revisions for cross-revision
// finalizer lookups. The old revision's finalizes declarations govern its
// resources — see 004-graph-execution.md § Prune Ordering.
func (r *GraphReconciler) pruneRemovedResources(ctx context.Context, graph *unstructured.Unstructured, previousKeys map[string]bool, currentKeys []string, dag *DAG, supersededDAGs map[string]*DAG, eval *evaluator, watcher *graphWatcher) ([]string, error) {
	logger := log.FromContext(ctx)
	var finalizerKeys []string

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
				if rk := resourceKeyFromTemplate(node.Template, graph.GetNamespace()); rk != "" {
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
	deferredKeys := map[string]bool{}
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
					deferredKeys[dk] = true
				}
			}
		}
	}

	fieldOwner := graphFieldOwner(graph)

	for key := range previousKeys {
		if currentSet[key] {
			continue // still exists in current cycle
		}

		// Defer deletion of resources that are dependencies of in-flight
		// finalizer nodes — they must remain alive while the finalizer runs.
		if deferredKeys[key] {
			logger.Info("prune deferred: resource is a dependency of an in-flight finalizer",
				"key", key)
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

		// Owns keys: delete the resource.
		gvk, nn := parseResourceKey(key)
		if gvk.Kind == "" {
			continue
		}

		// Check if it exists and is ours before deleting.
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(gvk)
		if err := r.Client.Get(ctx, nn, obj); err != nil {
			continue // already gone
		}

		// Verify ownership: must have our graph-name label and template hash
		objLabels := obj.GetLabels()
		if objLabels == nil || objLabels[LabelGraphName] != graph.GetName() {
			continue // not ours
		}
		objAnnotations := obj.GetAnnotations()
		if objAnnotations == nil || objAnnotations[templateHashAnnotation] == "" {
			continue // never successfully applied by us
		}

		// Contributor-aware deletion: check managedFields for other field
		// managers before deleting. If present, deletion is blocked — the
		// resource stays in the applied set for retry on the next reconcile.
		ownManager := string(graphFieldOwner(graph))
		if blockers := thirdPartyFieldManagers(obj, ownManager); len(blockers) > 0 {
			logger.Info("prune blocked: resource has other field managers",
				"key", key, "blockers", blockers)
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
				continue // block deletion until all finalizers ready
			}
			logger.Info("finalization complete", "key", key)
		}

		if err := r.Client.Delete(ctx, obj); err != nil {
			if client.IgnoreNotFound(err) != nil {
				return finalizerKeys, fmt.Errorf("pruning %s: %w", key, err)
			}
		} else {
			logger.Info("pruned resource", "key", key)
			r.Resources.remove(key)
		}
	}

	return finalizerKeys, nil
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
			shape := DetectShape(node.Template)
			if shape == ShapeWatch || shape == ShapeCollectionWatch {
				continue // read-only shapes don't create resources
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
	for gvk := range gvkSet {
		list := &unstructured.UnstructuredList{}
		listGVK := gvk
		listGVK.Kind = gvk.Kind + "List"
		list.SetGroupVersionKind(listGVK)

		selector := labels.SelectorFromSet(map[string]string{
			LabelGraphName: graph.GetName(),
		})

		if err := r.Client.List(ctx, list, &client.ListOptions{
			Namespace:     graph.GetNamespace(),
			LabelSelector: selector,
		}); err != nil {
			continue // skip GVKs we can't list (e.g., CRD deleted)
		}

		for _, item := range list.Items {
			keys = append(keys, resourceKey(&item))
		}
	}

	return keys, nil
}

// deletionOrder returns resource keys ordered for deletion: reverse
// topological order from the DAG. Rebuilds the DAG from the Graph spec.
// Maps resource keys to topological positions by matching kind/name.
// Unmatched keys (dynamic names, forEach) are deleted first (highest
// position) since their dependencies are unknown.
//
// Returns an error if the Graph spec cannot be parsed or the DAG cannot
// be built. Per the design (004-graph-execution): "Teardown is blocked
// until ordering is available — never degrade to unordered deletion."
func (r *GraphReconciler) deletionOrder(graph *unstructured.Unstructured, keys []string) ([]string, error) {
	graphSpec, err := extractGraphSpec(graph.Object)
	if err != nil {
		return nil, fmt.Errorf("extracting graph spec for deletion order: %w", err)
	}
	dag, err := BuildDAG(graphSpec.Nodes)
	if err != nil {
		return nil, fmt.Errorf("building DAG for deletion order: %w", err)
	}

	// Build a map from node index → topological position. Position 0 is
	// the first node in apply order (no dependencies); higher positions
	// depend on lower ones. Reverse topological = delete highest first.
	topoPosition := make(map[int]int, len(dag.TopologicalOrder))
	for pos, nodeIdx := range dag.TopologicalOrder {
		topoPosition[nodeIdx] = pos
	}

	// Map kind/name to topological position from static template metadata.
	kindNameToPosition := map[string]int{}
	for i, node := range dag.Nodes {
		tmpl := node.Template
		if tmpl == nil {
			continue
		}
		kind, _ := tmpl["kind"].(string)
		md, _ := tmpl["metadata"].(map[string]any)
		if md == nil {
			continue
		}
		name, _ := md["name"].(string)
		if kind == "" || name == "" || strings.Contains(name, "${") {
			continue
		}
		kindNameToPosition[kind+"/"+name] = topoPosition[i]
	}

	type scored struct {
		key      string
		position int
	}
	scoredKeys := make([]scored, 0, len(keys))
	for _, key := range keys {
		if key == "" {
			continue
		}
		gvk, nn := parseResourceKey(key)
		pos, ok := kindNameToPosition[gvk.Kind+"/"+nn.Name]
		if !ok {
			pos = len(dag.Nodes) // unmatched → deleted first
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
	return result, nil
}

// ---------------------------------------------------------------------------
// Applied set — annotation-based tracking of what keys a revision wrote
// ---------------------------------------------------------------------------

// setAppliedSet writes the applied key set as a JSON annotation on the revision.
func setAppliedSet(ctx context.Context, c client.Client, revision *unstructured.Unstructured, keys []string) error {
	sorted := make([]string, len(keys))
	copy(sorted, keys)
	sort.Strings(sorted)

	data, err := json.Marshal(sorted)
	if err != nil {
		return fmt.Errorf("marshaling applied set: %w", err)
	}

	newValue := string(data)

	latest, err := getRevision(ctx, c, revision.GetName(), revision.GetNamespace())
	if err != nil {
		return fmt.Errorf("re-fetching revision for applied set: %w", err)
	}

	annotations := latest.GetAnnotations()
	if annotations != nil && annotations[AnnotationAppliedSet] == newValue {
		return nil
	}

	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[AnnotationAppliedSet] = newValue
	latest.SetAnnotations(annotations)
	return c.Update(ctx, latest)
}

func getAppliedSet(revision *unstructured.Unstructured) []string {
	annotations := revision.GetAnnotations()
	if annotations == nil {
		return nil
	}
	raw, ok := annotations[AnnotationAppliedSet]
	if !ok || raw == "" {
		return nil
	}
	var keys []string
	if err := json.Unmarshal([]byte(raw), &keys); err != nil {
		return nil
	}
	return keys
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
