// node.go contains the per-node reconciliation handlers, one per node type.
// The coordinator in controller.go dispatches nodes here; these handlers
// evaluate templates and call into apply.go for cluster mutations.
package graphcontroller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	graphpkg "github.com/kubernetes-sigs/kro/experimental/controller/graph"
	"github.com/kubernetes-sigs/kro/experimental/controller/compiler"
	"github.com/kubernetes-sigs/kro/experimental/controller/watches"
)

// ---------------------------------------------------------------------------
// Node reconciliation methods
// ---------------------------------------------------------------------------

// reconcileNode dispatches to the appropriate handler based on node type.
// NodeType is a parse-time property of the node; no runtime resolution.
//
// resyncCorrection is true when the node was triggered by the resync timer.
// Per 005-reconciliation.md § Reconcile: resync-triggered nodes bypass the
// apply-hash check and apply unconditionally via SSA.
//
// After dispatch, reconcileNode evaluates readyWhen as a post-dispatch step
// for node types that don't handle their own per-item readiness (Definition,
// Template, Patch). Watch and ForEach return early — they handle
// readiness internally (per-item for ForEach, per-collection for Watch).
//
// All paths return (keys, error) with a uniform error contract:
//   - ErrPending: retryable, data not yet available
//   - ErrWaitingForReadiness: applied but readyWhen not satisfied
//   - other error: fatal
func (r *GraphReconciler) reconcileNode(ctx context.Context, graph *unstructured.Unstructured, node graphpkg.Node, nodeType graphpkg.NodeType, eval *evaluator, watcher *watches.GraphWatcher, resyncCorrection bool) ([]string, error) {
	if node.ForEach != nil {
		return r.reconcileForEach(ctx, graph, node, eval, watcher, resyncCorrection)
	}

	switch nodeType {
	case graphpkg.NodeTypeDef:
		if err := r.reconcileDefinition(ctx, node, eval); err != nil {
			return nil, err
		}
	case graphpkg.NodeTypeWatch:
		err := r.reconcileWatch(ctx, graph, node, eval, watcher)
		return nil, err // Watch handles its own readiness
	case graphpkg.NodeTypeRef:
		if err := r.reconcileRef(ctx, graph, node, eval, watcher); err != nil {
			return nil, err
		}
	default: // NodeTypeTemplate, NodeTypePatch
		key, err := r.reconcileApply(ctx, graph, node, nodeType, eval, watcher, resyncCorrection)
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
func (r *GraphReconciler) reconcileDefinition(ctx context.Context, node graphpkg.Node, eval *evaluator) error {
	result, err := eval.toMapNode(node)
	if err != nil {
		return fmt.Errorf("definition %s: %w", node.ID, err)
	}
	eval.scope[node.ID] = result
	// Definitions are always re-evaluated — vacuously updated.
	eval.markUpdated(node.ID, true)
	log.FromContext(ctx).V(1).Info("evaluated definition node", "node", node.ID, "keys", len(result))
	return nil
}

// reconcileRef reads a single existing object from the API server into
// scope. Serves a ref: node — a named dereference into the shared
// kind-scoped informer.
func (r *GraphReconciler) reconcileRef(ctx context.Context, graph *unstructured.Unstructured, node graphpkg.Node, eval *evaluator, watcher *watches.GraphWatcher) error {
	logger := log.FromContext(ctx)

	tmpl, err := eval.toMapNode(node)
	if err != nil {
		return fmt.Errorf("ref %s: %w", node.ID, err)
	}

	gvk := graphpkg.GVKFromMap(tmpl)
	md, _ := tmpl["metadata"].(map[string]any)

	name, _ := md["name"].(string)
	namespace, _ := md["namespace"].(string)
	if namespace == "" {
		namespace = graph.GetNamespace()
	}

	if watcher != nil {
		watcher.WatchScalar(node.ID, gvkToGVR(gvk), gvk.Kind, name, namespace)
	}

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	if err := r.apiReader().Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("ref %s: resource %s %s/%s not found: %w", node.ID, gvk, namespace, name, compiler.ErrPending)
		}
		return fmt.Errorf("reading %s %s/%s: %w", gvk, namespace, name, err)
	}

	eval.scope[node.ID] = graphpkg.NormalizeTypes(obj.Object)
	// Refs are external observations — vacuously updated. They have no
	// graph-managed desired state to be "behind" on.
	eval.markUpdated(node.ID, true)
	logger.V(1).Info("resolved ref", "node", node.ID, "gvk", gvk, "name", obj.GetName())

	return nil
}

// reconcileWatch reads a collection of resources matching a selector into scope.
//
// Per 005-reconciliation.md § Propagation: "When a single resource changes,
// update the cached list incrementally rather than re-listing — O(1) per
// event, not O(matching)." The evaluator carries the cached list and buffered
// collection changes from the coordinator. On incremental path, only changed
// items are GET'd and merged. On resync or first reconcile, a full List is
// performed and the cache is replaced.
func (r *GraphReconciler) reconcileWatch(ctx context.Context, graph *unstructured.Unstructured, node graphpkg.Node, eval *evaluator, watcher *watches.GraphWatcher) error {
	logger := log.FromContext(ctx)

	tmpl, err := eval.toMapNode(node)
	if err != nil {
		return fmt.Errorf("watch %s: %w", node.ID, err)
	}

	gvk := graphpkg.GVKFromMap(tmpl)

	var selectorRaw any
	if sel, ok := tmpl["selector"]; ok {
		selectorRaw = sel
	} else if md, ok := tmpl["metadata"].(map[string]any); ok {
		selectorRaw = md["selector"]
	}

	var labelSelector labels.Selector
	switch sel := selectorRaw.(type) {
	case map[string]any:
		// Detect structured selector (matchLabels/matchExpressions) vs flat key=value map.
		_, hasMatchLabels := sel["matchLabels"]
		_, hasMatchExpressions := sel["matchExpressions"]
		if hasMatchLabels || hasMatchExpressions {
			labelSelector = parseLabelSelector(sel)
		} else {
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
		}
	default:
		labelSelector = labels.Everything()
	}

	// Watch namespace follows k8s list/watch semantics: absent
	// metadata.namespace means all namespaces, matching ListOptions,
	// informer caches, and every client library. An explicit namespace
	// narrows the watch to one namespace. The Graph's own namespace is
	// never used as a default — a Watch's scope is its targets, not
	// where the Graph object lives.
	watchNamespace := ""
	if md, ok := tmpl["metadata"].(map[string]any); ok {
		if ns, ok := md["namespace"]; ok {
			if nsStr, ok := ns.(string); ok {
				watchNamespace = nsStr
			}
		}
	}

	if watcher != nil {
		watcher.WatchCollection(node.ID, gvkToGVR(gvk), gvk.Kind, watchNamespace, labelSelector)
	}

	var items []any

	// Incremental path: cached list exists and collection changes are available.
	// GET only the changed items and merge into the cached list.
	if eval.dispatch.collectionCachedList != nil && !eval.dispatch.collectionResyncOrFull {
		var err error
		items, err = mergeCollectionChanges(
			ctx, r.apiReader(), eval.dispatch.collectionCachedList, eval.dispatch.collectionChanges,
			gvk, labelSelector,
		)
		if err != nil {
			return fmt.Errorf("watch %s: %w", node.ID, err)
		}

		logger.V(1).Info("resolved watch (incremental)", "node", node.ID, "gvk", gvk,
			"cachedCount", len(eval.dispatch.collectionCachedList), "changes", len(eval.dispatch.collectionChanges),
			"resultCount", len(items))
	} else {
		// Full list path: first reconcile, resync timer, or no cache.
		listGVK := gvk
		listGVK.Kind = gvk.Kind + "List"

		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(listGVK)
		if err := r.apiReader().List(ctx, list, &client.ListOptions{
			LabelSelector: labelSelector,
			Namespace:     watchNamespace,
		}); err != nil {
			return fmt.Errorf("listing %s with selector %s: %w", gvk, labelSelector, err)
		}

		items = make([]any, len(list.Items))
		for i, item := range list.Items {
			items[i] = graphpkg.NormalizeTypes(item.Object)
		}
		// Mark that this worker took the full-List path. The coordinator
		// uses this to clear the collectionDirty flag — only a successful
		// full re-List recovers from a lost incremental merge.
		eval.dispatch.collectionDidFullList = true
		logger.V(1).Info("resolved watch (full list)", "node", node.ID, "gvk", gvk, "count", len(items))
	}

	// Store the updated cache for the coordinator to persist.
	eval.dispatch.collectionUpdatedCache = items

	eval.scope[node.ID] = items

	// Per 001-graph.md: "A Watch's .ready() returns true when the
	// node's readyWhen conditions pass (evaluated once against the whole
	// array, not per-item)." The verdict is stored in eval.nodeReady so
	// the AST rewrite of `<wk_id>.ready()` can surface it — including
	// for empty collections, where per-item `__ready` stamping has
	// nothing to attach to.
	ready := true
	if len(node.ReadyWhen) > 0 {
		if err := eval.evalReadinessConditions(node.ReadyWhen, node.ID); err != nil {
			ready = false
			// Set __ready on items (preserves scalar/forEach semantics
			// for code paths that still consult per-item readiness),
			// stamp the sidecar for the AST-rewritten path, then return
			// the error. Items stay in scope so downstream nodes can
			// still reference the data.
			for _, item := range items {
				if m, ok := item.(map[string]any); ok {
					m["__ready"] = false
					// Watch items are external observations — vacuously
					// updated. They have no graph-managed desired state.
					m["__updated"] = true
				}
			}
			if eval.nodeReady != nil {
				eval.nodeReady[node.ID] = false
			}
			return err
		}
	}
	for _, item := range items {
		if m, ok := item.(map[string]any); ok {
			m["__ready"] = ready
			// Watch items are external observations — vacuously updated.
			// They have no graph-managed desired state to be "behind" on.
			m["__updated"] = true
		}
	}
	if eval.nodeReady != nil {
		eval.nodeReady[node.ID] = ready
	}

	return nil
}

// reconcileApply evaluates and applies a Template or Patch node.
// The nodeType parameter controls SSA behavior (identity labels, pre-apply
// checks) and applied set key format: Template keys use resourceKey
// (prune → delete), Patch keys use patchKey (prune → release apply to
// release fields). See applySSA for the full type-dependent behavior.
// resyncCorrection bypasses the apply-hash check in applySSA.
func (r *GraphReconciler) reconcileApply(ctx context.Context, graph *unstructured.Unstructured, node graphpkg.Node, nodeType graphpkg.NodeType, eval *evaluator, watcher *watches.GraphWatcher, resyncCorrection bool) (string, error) {
	logger := log.FromContext(ctx)

	evalMap, err := eval.toMapNode(node)
	if err != nil {
		return "", fmt.Errorf("%s %s: %w", nodeType, node.ID, err)
	}

	applied, err := r.applySSA(ctx, graph, evalMap, watcher, node.ID, nodeType, eval.effectiveGeneration, resyncCorrection, node.Lifecycle.ForceApply())
	if err != nil {
		return "", err
	}

	// Per 004-compilation.md § Deferred Types: record the resolved GVK for
	// dynamic GVK nodes. The staleness check compares this against what was
	// compiled — if different, recompilation is needed on the next reconcile.
	if eval.dispatch.dynamicGVKResolved != nil && node.HasDynamicGVR() {
		eval.dispatch.dynamicGVKResolved[node.ID] = applied.GroupVersionKind()
	}

	eval.scope[node.ID] = graphpkg.NormalizeTypes(applied.Object)
	// Just applied with effectiveGeneration — resource is on the latest generation.
	eval.markUpdated(node.ID, true)
	// Side effects (apply, delete, create) log at V(0) so operators see
	// which resources are being managed at default verbosity.
	logger.Info("applied resource", "node", node.ID, "nodeType", nodeType,
		"gvk", applied.GroupVersionKind(), "name", applied.GetName())

	if nodeType == graphpkg.NodeTypePatch {
		// Track the patch in the applied set with a "patch:" prefix.
		// This lets prune and teardown distinguish patch keys (release apply
		// to release fields) from template keys (delete).
		hasStatus := evalMap["status"] != nil
		return patchKey(applied, hasStatus), nil
	}
	return resourceKey(applied), nil
}

// ---------------------------------------------------------------------------
// Collection merge — incremental Watch cache update
// ---------------------------------------------------------------------------

// mergeCollectionChanges applies buffered collection changes to a cached list
// and returns a new list. The cached list is read-only — all mutations happen
// on an internally-owned slice. This makes cache corruption structurally
// impossible regardless of how callers handle the returned list.
//
// Per 005-reconciliation.md § Propagation: "When a single resource
// changes, update the cached list incrementally rather than re-listing —
// O(1) per event, not O(matching)."
func mergeCollectionChanges(
	ctx context.Context,
	k8s client.Reader,
	cached []any,
	changes []watches.CollectionChange,
	gvk schema.GroupVersionKind,
	selector labels.Selector,
) ([]any, error) {
	// Own the allocation. The cached list goes in read-only, a new list comes out.
	items := make([]any, len(cached))
	copy(items, cached)

	if len(changes) == 0 {
		return items, nil
	}

	// Deduplicate changes by namespace/name — only the latest event
	// for each resource matters. Multiple events between reconciles
	// collapse into one GET.
	type changeKey struct{ namespace, name string }
	deduped := make(map[changeKey]watches.CollectionChange, len(changes))
	for _, change := range changes {
		deduped[changeKey{change.Namespace, change.Name}] = change
	}

	for ck, change := range deduped {
		if change.EventType == watches.WatchEventDelete {
			items = removeItem(items, ck.namespace, ck.name)
			continue
		}

		// Add or Update: GET the full object and merge.
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(gvk)
		if err := k8s.Get(ctx, types.NamespacedName{
			Name:      ck.name,
			Namespace: ck.namespace,
		}, obj); err != nil {
			if apierrors.IsNotFound(err) {
				// Resource was deleted between event and reconcile.
				items = removeItem(items, ck.namespace, ck.name)
				continue
			}
			return nil, fmt.Errorf("getting changed resource %s/%s: %w", ck.namespace, ck.name, err)
		}

		// Re-check selector membership after GET. The watch informer is
		// broad (no server-side label filter), so events arrive for
		// resources whose labels changed in any direction — including
		// resources that were never in the collection. A resource that
		// doesn't match the selector must be removed from the cached
		// list (if present) rather than merged.
		if !selector.Matches(labels.Set(obj.GetLabels())) {
			items = removeItem(items, ck.namespace, ck.name)
			continue
		}

		normalized := graphpkg.NormalizeTypes(obj.Object)
		if idx := findIndex(items, ck.namespace, ck.name); idx >= 0 {
			items[idx] = normalized
		} else {
			items = append(items, normalized)
		}
	}

	return items, nil
}

// findIndex returns the index of an item in a []any collection matching the
// given namespace and name via metadata extraction. Returns -1 if not found.
func findIndex(items []any, namespace, name string) int {
	for i, item := range items {
		if m, ok := item.(map[string]any); ok {
			md, _ := m["metadata"].(map[string]any)
			itemName, _ := md["name"].(string)
			itemNS, _ := md["namespace"].(string)
			if itemName == name && itemNS == namespace {
				return i
			}
		}
	}
	return -1
}

// removeItem returns items with the matching namespace/name entry removed.
// Items that aren't maps or lack metadata are kept — if we can't identify
// them, dropping them risks data loss.
func removeItem(items []any, namespace, name string) []any {
	idx := findIndex(items, namespace, name)
	if idx < 0 {
		return items
	}
	return append(items[:idx], items[idx+1:]...)
}

// parseLabelSelector converts a structured selector map (with matchLabels
// and/or matchExpressions) into a labels.Selector. This supports the full
// Kubernetes LabelSelector semantics used by ExternalRef collections.
func parseLabelSelector(sel map[string]any) labels.Selector {
	var reqs []labels.Requirement

	// Parse matchLabels: {"matchLabels": {"key": "value", ...}}
	if ml, ok := sel["matchLabels"].(map[string]any); ok {
		for k, v := range ml {
			if vs, ok := v.(string); ok {
				req, err := labels.NewRequirement(k, selection.Equals, []string{vs})
				if err == nil {
					reqs = append(reqs, *req)
				}
			}
		}
	}

	// Parse matchExpressions: [{"key": "k", "operator": "In", "values": ["v1","v2"]}]
	if me, ok := sel["matchExpressions"].([]any); ok {
		for _, expr := range me {
			em, ok := expr.(map[string]any)
			if !ok {
				continue
			}
			key, _ := em["key"].(string)
			opStr, _ := em["operator"].(string)
			var values []string
			if vals, ok := em["values"].([]any); ok {
				for _, v := range vals {
					if vs, ok := v.(string); ok {
						values = append(values, vs)
					}
				}
			}

			var op selection.Operator
			switch opStr {
			case "In":
				op = selection.In
			case "NotIn":
				op = selection.NotIn
			case "Exists":
				op = selection.Exists
			case "DoesNotExist":
				op = selection.DoesNotExist
			default:
				continue
			}

			req, err := labels.NewRequirement(key, op, values)
			if err == nil {
				reqs = append(reqs, *req)
			}
		}
	}

	if len(reqs) == 0 {
		return labels.Everything()
	}
	return labels.NewSelector().Add(reqs...)
}
