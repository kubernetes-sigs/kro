// node.go contains the per-node reconciliation handlers, one per reference
// type. The coordinator in controller.go dispatches nodes here; these
// handlers evaluate templates and call into apply.go for cluster mutations.
package graphcontroller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// ---------------------------------------------------------------------------
// Node reconciliation methods
// ---------------------------------------------------------------------------

// reconcileNode dispatches to the appropriate handler based on node reference type.
// The reference must be resolved before calling this function — Unresolved
// references are resolved by the coordinator before dispatching to workers.
//
// driftCorrection is true when the node was triggered by the drift timer.
// Per 004-graph-reconciliation.md § Reconcile: drift-triggered nodes bypass the
// apply-hash check and apply unconditionally via SSA.
//
// After dispatch, reconcileNode evaluates readyWhen as a post-dispatch step
// for node types that don't handle their own per-item readiness (Definition,
// Watch, Own, Contribute). WatchKind and ForEach return early — they
// handle readiness internally (per-item for ForEach, per-collection for
// WatchKind).
//
// All paths return (keys, error) with a uniform error contract:
//   - ErrPending: retryable, data not yet available
//   - ErrWaitingForReadiness: applied but readyWhen not satisfied
//   - other error: fatal
func (r *GraphReconciler) reconcileNode(ctx context.Context, graph *unstructured.Unstructured, node Node, ref Reference, eval *evaluator, watcher *graphWatcher, driftCorrection bool) ([]string, error) {
	if node.ForEach != nil {
		return r.reconcileForEach(ctx, graph, node, eval, watcher, driftCorrection)
	}

	switch ref {
	case ReferenceDefinition:
		if err := r.reconcileDefinition(ctx, node, eval); err != nil {
			return nil, err
		}
	case ReferenceWatchKind:
		err := r.reconcileWatchKind(ctx, graph, node, eval, watcher)
		return nil, err // WatchKind handles its own readiness
	case ReferenceWatch:
		if err := r.reconcileWatch(ctx, graph, node, eval, watcher); err != nil {
			return nil, err
		}
	default: // ReferenceOwn, ReferenceContribute
		key, err := r.reconcileApply(ctx, graph, node, ref, eval, watcher, driftCorrection)
		if err != nil {
			if key != "" {
				return []string{key}, err
			}
			return nil, err
		}
		return []string{key}, eval.evalReadiness(node.ID, node.ReadyWhen)
	}

	// Post-dispatch readyWhen for Definition and Watch (no keys to return).
	return nil, eval.evalReadiness(node.ID, node.ReadyWhen)
}

// reconcileDefinition evaluates a definition node — resolves values from the template
// (literals and/or CEL expressions) and enters the result into scope as
// map[string]any. No Kubernetes API calls are made.
func (r *GraphReconciler) reconcileDefinition(ctx context.Context, node Node, eval *evaluator) error {
	result, err := eval.toMapNode(node)
	if err != nil {
		return fmt.Errorf("definition %s: %w", node.ID, err)
	}
	eval.scope[node.ID] = result
	log.FromContext(ctx).V(1).Info("evaluated definition node", "node", node.ID, "keys", len(result))
	return nil
}

// resolveReference determines Own vs Contribute for an Unresolved node by
// checking whether the target resource exists. Absent → Own, exists → check
// the kro label. If the resource has this Graph's label, it's Own (we created
// it on a previous revision). If it has no kro label or another Graph's label,
// it's Contribute. Force annotation always resolves to Own.
func (r *GraphReconciler) resolveReference(ctx context.Context, graph *unstructured.Unstructured, node Node, eval *evaluator) (Reference, error) {
	logger := log.FromContext(ctx)

	evalMap, err := eval.toMapNode(node)
	if err != nil {
		// Template can't evaluate yet — expressions unresolvable.
		// Return Unresolved so it's retried next reconcile.
		return ReferenceUnresolved, fmt.Errorf("resolving reference for %s: %w", node.ID, err)
	}

	obj := &unstructured.Unstructured{Object: evalMap}
	if isForceApply(obj) {
		logger.V(1).Info("reference resolved: Own (Force annotation)", "node", node.ID)
		return ReferenceOwn, nil
	}

	if obj.GetNamespace() == "" {
		obj.SetNamespace(graph.GetNamespace())
	}

	ref, err := r.classifyReference(ctx, graph, obj.GroupVersionKind(), obj.GetNamespace(), obj.GetName())
	if err != nil {
		return ReferenceUnresolved, fmt.Errorf("checking resource existence for reference detection %s: %w", node.ID, err)
	}
	logger.V(1).Info("reference resolved", "node", node.ID, "ref", ref)
	return ref, nil
}

// classifyReference determines Own vs Contribute for a resource by checking
// whether it exists and examining its identity labels. This is the shared
// classification logic used by both resolveReference (single nodes) and
// resolveForEachChildReference (forEach children).
//
// Decision tree:
//   - Resource absent → Own (we'll create it)
//   - Resource exists with this Graph's contribute label → Contribute
//   - Resource exists with this Graph's own label → Own (we created it previously)
//   - Resource exists without any of this Graph's labels → Contribute
//
// Returns a non-nil error only for transient failures (network, server error).
// NotFound is not an error — it maps to Own.
func (r *GraphReconciler) classifyReference(ctx context.Context, graph *unstructured.Unstructured, gvk schema.GroupVersionKind, namespace, name string) (Reference, error) {
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(gvk)
	err := r.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, existing)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return ReferenceOwn, nil
		}
		return ReferenceOwn, err
	}

	// Resource exists. Check if this Graph created it (identity label match).
	// Per 003-ownership.md: Contribute templates also stamp identity labels
	// (with reference=contribute) so the applied set can find them on restart.
	// We must check the REFERENCE VALUE — not just label presence — to
	// distinguish Contribute (reference=contribute) from Own (reference=own).
	// Without this check, a Contribute node misidentifies as Own on the second
	// reconcile, triggering the kro label conflict check against co-contributing
	// Graphs.
	existingLabels := existing.GetLabels()
	for key, val := range existingLabels {
		if isGraphIdentityLabel(key, graph.GetName(), graph.GetNamespace()) {
			if val == ReferenceContribute.String() {
				return ReferenceContribute, nil
			}
			return ReferenceOwn, nil
		}
	}

	return ReferenceContribute, nil
}

// reconcileWatch reads a single existing object from the API server into scope.
func (r *GraphReconciler) reconcileWatch(ctx context.Context, graph *unstructured.Unstructured, node Node, eval *evaluator, watcher *graphWatcher) error {
	logger := log.FromContext(ctx)

	tmpl, err := eval.toMapNode(node)
	if err != nil {
		return fmt.Errorf("watch %s: %w", node.ID, err)
	}

	apiVersion, _ := tmpl["apiVersion"].(string)
	kind, _ := tmpl["kind"].(string)
	gv, _ := schema.ParseGroupVersion(apiVersion)
	gvk := gv.WithKind(kind)
	md, _ := tmpl["metadata"].(map[string]any)

	name, _ := md["name"].(string)
	namespace, _ := md["namespace"].(string)
	if namespace == "" {
		namespace = graph.GetNamespace()
	}

	if watcher != nil {
		watcher.watchScalar(node.ID, gvkToGVR(gvk), name, namespace)
	}

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	if err := r.Client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("watch %s: resource %s/%s %s/%s not found: %w", node.ID, apiVersion, kind, namespace, name, ErrPending)
		}
		return fmt.Errorf("reading %s/%s %s/%s: %w", apiVersion, kind, namespace, name, err)
	}

	eval.scope[node.ID] = normalizeTypes(obj.Object)
	logger.V(1).Info("resolved watch", "node", node.ID, "gvk", gvk, "name", obj.GetName())

	return nil
}

// reconcileWatchKind reads a collection of resources matching a selector into scope.
//
// Per 004-graph-reconciliation.md § Propagation: "When a single resource changes,
// update the cached list incrementally rather than re-listing — O(1) per
// event, not O(matching)." The evaluator carries the cached list and buffered
// collection changes from the coordinator. On incremental path, only changed
// items are GET'd and merged. On drift or first reconcile, a full List is
// performed and the cache is replaced.
func (r *GraphReconciler) reconcileWatchKind(ctx context.Context, graph *unstructured.Unstructured, node Node, eval *evaluator, watcher *graphWatcher) error {
	logger := log.FromContext(ctx)

	tmpl, err := eval.toMapNode(node)
	if err != nil {
		return fmt.Errorf("watchKind %s: %w", node.ID, err)
	}

	apiVersion, _ := tmpl["apiVersion"].(string)
	kind, _ := tmpl["kind"].(string)
	gv, _ := schema.ParseGroupVersion(apiVersion)
	gvk := gv.WithKind(kind)

	var selectorRaw any
	if sel, ok := tmpl["selector"]; ok {
		selectorRaw = sel
	} else if md, ok := tmpl["metadata"].(map[string]any); ok {
		selectorRaw = md["selector"]
	}

	var labelSelector labels.Selector
	switch sel := selectorRaw.(type) {
	case map[string]any:
		matchLabels := map[string]string{}
		for k, v := range sel {
			if vs, ok := v.(string); ok {
				matchLabels[k] = vs
			}
		}
		if len(matchLabels) > 0 {
			labelSelector = labels.SelectorFromSet(matchLabels)
		} else {
			labelSelector = labels.Everything()
		}
	default:
		labelSelector = labels.Everything()
	}

	// Resolve namespace for WatchKind. Default is the Graph's own namespace
	// for test isolation and scoping. If the template explicitly sets
	// metadata.namespace, use that value — empty string means all namespaces,
	// needed for cluster-scoped resources or cross-namespace watching.
	watchNamespace := graph.GetNamespace()
	if md, ok := tmpl["metadata"].(map[string]any); ok {
		if ns, ok := md["namespace"]; ok {
			if nsStr, ok := ns.(string); ok {
				watchNamespace = nsStr
			}
		}
	}

	if watcher != nil {
		watcher.watchKind(node.ID, gvkToGVR(gvk), watchNamespace, labelSelector)
	}

	var items []any

	// Incremental path: cached list exists and collection changes are available.
	// GET only the changed items and merge into the cached list.
	if eval.watchKindCachedList != nil && !eval.watchKindDriftOrFull {
		items = make([]any, len(eval.watchKindCachedList))
		copy(items, eval.watchKindCachedList)

		if len(eval.watchKindChanges) > 0 {
			// Deduplicate changes by namespace/name — only the latest event
			// for each resource matters. Multiple events between reconciles
			// collapse into one GET.
			type changeKey struct{ namespace, name string }
			dedupedChanges := make(map[changeKey]CollectionChange)
			for _, change := range eval.watchKindChanges {
				dedupedChanges[changeKey{change.Namespace, change.Name}] = change
			}

			for ck, change := range dedupedChanges {
				if change.EventType == WatchEventDelete {
					// Remove deleted item from cached list.
					filtered := items[:0]
					for _, item := range items {
						if m, ok := item.(map[string]any); ok {
							md, _ := m["metadata"].(map[string]any)
							itemName, _ := md["name"].(string)
							itemNS, _ := md["namespace"].(string)
							if itemName == ck.name && itemNS == ck.namespace {
								continue // remove
							}
						}
						filtered = append(filtered, item)
					}
					items = filtered
					continue
				}

				// Add or Update: GET the full object and merge.
				obj := &unstructured.Unstructured{}
				obj.SetGroupVersionKind(gvk)
				if err := r.Client.Get(ctx, types.NamespacedName{
					Name:      ck.name,
					Namespace: ck.namespace,
				}, obj); err != nil {
					if apierrors.IsNotFound(err) {
						// Resource was deleted between event and reconcile — remove from cache.
						filtered := items[:0]
						for _, item := range items {
							if m, ok := item.(map[string]any); ok {
								md, _ := m["metadata"].(map[string]any)
								itemName, _ := md["name"].(string)
								itemNS, _ := md["namespace"].(string)
								if itemName == ck.name && itemNS == ck.namespace {
									continue
								}
							}
							filtered = append(filtered, item)
						}
						items = filtered
						continue
					}
					return fmt.Errorf("watchKind %s: getting changed resource %s/%s: %w", node.ID, ck.namespace, ck.name, err)
				}

				normalized := normalizeTypes(obj.Object)

				// Check if item already exists in the list (update) or is new (add).
				found := false
				for i, item := range items {
					if m, ok := item.(map[string]any); ok {
						md, _ := m["metadata"].(map[string]any)
						itemName, _ := md["name"].(string)
						itemNS, _ := md["namespace"].(string)
						if itemName == ck.name && itemNS == ck.namespace {
							items[i] = normalized
							found = true
							break
						}
					}
				}
				if !found {
					items = append(items, normalized)
				}
			}
		}

		logger.V(1).Info("resolved watchKind (incremental)", "node", node.ID, "gvk", gvk,
			"cachedCount", len(eval.watchKindCachedList), "changes", len(eval.watchKindChanges),
			"resultCount", len(items))
	} else {
		// Full list path: first reconcile, drift timer, or no cache.
		listGVK := gvk
		listGVK.Kind = gvk.Kind + "List"

		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(listGVK)
		if err := r.Client.List(ctx, list, &client.ListOptions{
			LabelSelector: labelSelector,
			Namespace:     watchNamespace,
		}); err != nil {
			return fmt.Errorf("listing %s with selector %s: %w", gvk, labelSelector, err)
		}

		items = make([]any, len(list.Items))
		for i, item := range list.Items {
			items[i] = normalizeTypes(item.Object)
		}
		logger.V(1).Info("resolved watchKind (full list)", "node", node.ID, "gvk", gvk, "count", len(items))
	}

	// Store the updated cache for the coordinator to persist.
	eval.watchKindUpdatedCache = items

	eval.scope[node.ID] = items

	// Per 001-graph.md: "A WatchKind's .ready() returns true when the
	// node's readyWhen conditions pass (evaluated once against the whole array,
	// not per-item)."
	//
	// Readiness is determined by readyWhen outcome (or absence of readyWhen).
	// __ready is stamped AFTER readyWhen evaluation — one code path, not two
	// compensating ones.
	ready := true
	if len(node.ReadyWhen) > 0 {
		if err := eval.checkReadiness(node.ReadyWhen, node.ID); err != nil {
			ready = false
			// Set __ready on items, then return the error. Items stay in
			// scope so downstream nodes can still reference the data.
			for _, item := range items {
				if m, ok := item.(map[string]any); ok {
					m["__ready"] = false
				}
			}
			return err
		}
	}
	for _, item := range items {
		if m, ok := item.(map[string]any); ok {
			m["__ready"] = ready
		}
	}

	return nil
}

// reconcileApply evaluates and applies an Own or Contribute template.
// The ref parameter controls SSA behavior (identity labels, pre-apply checks)
// and applied set key format: Own keys use resourceKey (prune → delete),
// Contribute keys use contributeKey (prune → skeleton apply to release fields).
// See applySSA for the full ref-dependent behavior.
// driftCorrection bypasses the apply-hash check in applySSA.
func (r *GraphReconciler) reconcileApply(ctx context.Context, graph *unstructured.Unstructured, node Node, ref Reference, eval *evaluator, watcher *graphWatcher, driftCorrection bool) (string, error) {
	logger := log.FromContext(ctx)

	evalMap, err := eval.toMapNode(node)
	if err != nil {
		return "", fmt.Errorf("%s %s: %w", ref, node.ID, err)
	}

	applied, err := r.applySSA(ctx, graph, evalMap, watcher, node.ID, ref, driftCorrection)
	if err != nil {
		return "", err
	}

	eval.scope[node.ID] = normalizeTypes(applied.Object)
	logger.V(1).Info("applied resource", "node", node.ID, "ref", ref,
		"gvk", applied.GroupVersionKind(), "name", applied.GetName())

	if ref == ReferenceContribute {
		// Track the contribution in the applied set with a "contribute:" prefix.
		// This lets prune and teardown distinguish Contribute keys (skeleton apply
		// to release fields) from Own keys (delete).
		hasStatus := evalMap["status"] != nil
		return contributeKey(applied, hasStatus), nil
	}
	return resourceKey(applied), nil
}
