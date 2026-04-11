// node.go contains the per-node reconciliation handlers, one per template
// shape. The coordinator in controller.go dispatches nodes here; these
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

// reconcileNode dispatches to the appropriate handler based on node shape.
// The shape must be resolved before calling this function — Deferred shapes
// are resolved by the coordinator before dispatching to workers.
//
// All paths return (keys, error) with a uniform error contract:
//   - ErrDataPending: retryable, data not yet available
//   - ErrWaitingForReadiness: applied but readyWhen not satisfied
//   - other error: fatal
func (r *GraphReconciler) reconcileNode(ctx context.Context, graph *unstructured.Unstructured, node Node, shape TemplateShape, eval *evaluator, watcher *graphWatcher) ([]string, error) {
	if node.ForEach != nil {
		return r.reconcileForEach(ctx, graph, node, eval, watcher)
	}

	switch shape {
	case ShapeCollectionWatch:
		err := r.reconcileCollectionWatch(ctx, graph, node, eval, watcher)
		return nil, err
	case ShapeWatch:
		err := r.reconcileWatch(ctx, graph, node, eval, watcher)
		return nil, err
	case ShapeContribute:
		key, err := r.reconcileContribute(ctx, graph, node, eval, watcher)
		if key != "" {
			return []string{key}, err
		}
		return nil, err
	default: // ShapeOwns
		key, err := r.reconcileOwns(ctx, graph, node, eval, watcher)
		if key != "" {
			return []string{key}, err
		}
		return nil, err
	}
}

// resolveShape determines Owns vs Contribute for a Deferred node by checking
// whether the target resource exists. Absent → Owns, exists → check the kro
// label. If the resource has this Graph's label, it's Owns (we created it on
// a previous revision). If it has no kro label or another Graph's label, it's
// Contribute. Force annotation always resolves to Owns.
func (r *GraphReconciler) resolveShape(ctx context.Context, graph *unstructured.Unstructured, node Node, eval *evaluator) (TemplateShape, error) {
	logger := log.FromContext(ctx)

	evalMap, err := eval.toMap(node.Template)
	if err != nil {
		// Template can't evaluate yet — expressions unresolvable.
		// Return Deferred so it's retried next reconcile.
		return ShapeDeferred, fmt.Errorf("resolving shape for %s: %w", node.ID, err)
	}

	// Force annotation is an explicit ownership claim — always Owns.
	obj := &unstructured.Unstructured{Object: evalMap}
	if isForceApply(obj) {
		logger.V(1).Info("shape resolved: Owns (Force annotation)", "node", node.ID)
		return ShapeOwns, nil
	}

	if obj.GetNamespace() == "" {
		obj.SetNamespace(graph.GetNamespace())
	}

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(obj.GroupVersionKind())
	err = r.Client.Get(ctx, client.ObjectKey{Namespace: obj.GetNamespace(), Name: obj.GetName()}, existing)
	if err != nil {
		if apierrors.IsNotFound(err) {
			logger.V(1).Info("shape resolved: Owns (resource absent)", "node", node.ID)
			return ShapeOwns, nil
		}
		return ShapeDeferred, fmt.Errorf("checking resource existence for shape detection %s: %w", node.ID, err)
	}

	// Resource exists. Check if this Graph created it (identity label match).
	existingLabels := existing.GetLabels()
	for key := range existingLabels {
		if isGraphIdentityLabel(key, graph.GetName(), graph.GetNamespace()) {
			logger.V(1).Info("shape resolved: Owns (resource has our identity label)", "node", node.ID)
			return ShapeOwns, nil
		}
	}

	logger.V(1).Info("shape resolved: Contribute (resource exists, not ours)", "node", node.ID)
	return ShapeContribute, nil
}

// reconcileWatch reads a single existing object from the API server into scope.
func (r *GraphReconciler) reconcileWatch(ctx context.Context, graph *unstructured.Unstructured, node Node, eval *evaluator, watcher *graphWatcher) error {
	logger := log.FromContext(ctx)

	tmpl, err := eval.toMap(node.Template)
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
			return fmt.Errorf("watch %s: resource %s/%s %s/%s not found: %w", node.ID, apiVersion, kind, namespace, name, ErrDataPending)
		}
		return fmt.Errorf("reading %s/%s %s/%s: %w", apiVersion, kind, namespace, name, err)
	}

	eval.scope[node.ID] = normalizeTypes(obj.Object)
	logger.V(1).Info("resolved watch", "node", node.ID, "gvk", gvk, "name", obj.GetName())

	if len(node.ReadyWhen) > 0 {
		if err := eval.checkReadiness(node.ReadyWhen, eval.scope[node.ID], node.ID); err != nil {
			eval.markReady(node.ID, false)
			return err
		}
	}
	eval.markReady(node.ID, true)

	return nil
}

// reconcileCollectionWatch reads a collection of resources matching a selector into scope.
func (r *GraphReconciler) reconcileCollectionWatch(ctx context.Context, graph *unstructured.Unstructured, node Node, eval *evaluator, watcher *graphWatcher) error {
	logger := log.FromContext(ctx)

	tmpl, err := eval.toMap(node.Template)
	if err != nil {
		return fmt.Errorf("collectionWatch %s: %w", node.ID, err)
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

	if watcher != nil {
		watcher.watchCollection(node.ID, gvkToGVR(gvk), graph.GetNamespace(), labelSelector)
	}

	listGVK := gvk
	listGVK.Kind = gvk.Kind + "List"

	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(listGVK)
	if err := r.Client.List(ctx, list, &client.ListOptions{
		LabelSelector: labelSelector,
		Namespace:     graph.GetNamespace(),
	}); err != nil {
		return fmt.Errorf("listing %s with selector %s: %w", gvk, labelSelector, err)
	}

	items := make([]any, len(list.Items))
	for i, item := range list.Items {
		normalized := normalizeTypes(item.Object)
		if m, ok := normalized.(map[string]any); ok {
			m["__ready"] = true // Collection watch items are ready on read
		}
		items[i] = normalized
	}
	eval.scope[node.ID] = items
	logger.V(1).Info("resolved collection watch", "node", node.ID, "gvk", gvk, "count", len(items))
	return nil
}

// reconcileOwns evaluates and applies an Owns template, checks readyWhen.
func (r *GraphReconciler) reconcileOwns(ctx context.Context, graph *unstructured.Unstructured, node Node, eval *evaluator, watcher *graphWatcher) (string, error) {
	logger := log.FromContext(ctx)

	evalMap, err := eval.toMap(node.Template)
	if err != nil {
		return "", fmt.Errorf("template %s: %w", node.ID, err)
	}

	applied, err := r.applyResource(ctx, graph, evalMap, watcher, node.ID)
	if err != nil {
		return "", err
	}

	eval.scope[node.ID] = applied.Object
	key := resourceKey(applied)
	logger.V(1).Info("applied resource", "node", node.ID, "gvk", applied.GroupVersionKind(), "name", applied.GetName())

	if len(node.ReadyWhen) > 0 {
		if err := eval.checkReadiness(node.ReadyWhen, eval.scope[node.ID], node.ID); err != nil {
			eval.markReady(node.ID, false)
			return key, err
		}
	}
	eval.markReady(node.ID, true)

	return key, nil
}

// reconcileContribute evaluates and applies a Contribute template.
func (r *GraphReconciler) reconcileContribute(ctx context.Context, graph *unstructured.Unstructured, node Node, eval *evaluator, watcher *graphWatcher) (string, error) {
	logger := log.FromContext(ctx)

	evalMap, err := eval.toMap(node.Template)
	if err != nil {
		return "", fmt.Errorf("contribute %s: %w", node.ID, err)
	}

	applied, err := r.applyContribution(ctx, graph, evalMap, watcher, node.ID)
	if err != nil {
		return "", err
	}
	eval.scope[node.ID] = applied.Object
	logger.V(1).Info("contributed to resource", "node", node.ID, "gvk", applied.GroupVersionKind(), "name", applied.GetName())

	// Evaluate readyWhen if present, matching the reconcileOwns pattern.
	// Without readyWhen, applied = ready.
	if len(node.ReadyWhen) > 0 {
		if err := eval.checkReadiness(node.ReadyWhen, eval.scope[node.ID], node.ID); err != nil {
			eval.markReady(node.ID, false)
			// Track the contribution in the applied set with a "contribute:" prefix.
			hasStatus := evalMap["status"] != nil
			key := contributeKey(applied, hasStatus)
			return key, err
		}
	}
	eval.markReady(node.ID, true)

	// Track the contribution in the applied set with a "contribute:" prefix.
	// This lets prune and teardown distinguish Contribute keys (skeleton apply
	// to release fields) from Owns keys (delete).
	hasStatus := evalMap["status"] != nil
	key := contributeKey(applied, hasStatus)
	return key, nil
}
