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
func (r *GraphReconciler) pruneRemovedResources(ctx context.Context, graph *unstructured.Unstructured, previousKeys map[string]bool, currentKeys []string) error {
	logger := log.FromContext(ctx)

	// Build current key set for fast lookup
	currentSet := map[string]bool{}
	for _, k := range currentKeys {
		currentSet[k] = true
	}

	fieldOwner := graphFieldOwner(graph)

	for key := range previousKeys {
		if currentSet[key] {
			continue // still exists in current cycle
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

		if err := r.Client.Delete(ctx, obj); err != nil {
			if client.IgnoreNotFound(err) != nil {
				return fmt.Errorf("pruning %s: %w", key, err)
			}
		} else {
			logger.Info("pruned resource", "key", key)
			r.Resources.remove(key)
		}
	}

	return nil
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
			if shape != ShapeOwns {
				continue // only Owns templates create resources we own
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
