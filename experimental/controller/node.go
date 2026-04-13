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
// Per 004-graph-execution.md § The Walk: drift-triggered nodes bypass the
// template-hash check and apply unconditionally via SSA.
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
		return nil, r.reconcileDefinition(ctx, node, eval)
	case ReferenceWatchKind:
		err := r.reconcileCollectionWatch(ctx, graph, node, eval, watcher)
		return nil, err
	case ReferenceWatch:
		err := r.reconcileWatch(ctx, graph, node, eval, watcher)
		return nil, err
	case ReferenceContribute:
		key, err := r.reconcileContribute(ctx, graph, node, eval, watcher, driftCorrection)
		if key != "" {
			return []string{key}, err
		}
		return nil, err
	default: // ReferenceOwn
		key, err := r.reconcileOwn(ctx, graph, node, eval, watcher, driftCorrection)
		if key != "" {
			return []string{key}, err
		}
		return nil, err
	}
}

// reconcileDefinition evaluates a definition node — resolves values from the template
// (literals and/or CEL expressions) and enters the result into scope as
// map[string]any. No Kubernetes API calls are made.
func (r *GraphReconciler) reconcileDefinition(ctx context.Context, node Node, eval *evaluator) error {
	result, err := eval.toMap(node.Template)
	if err != nil {
		return fmt.Errorf("definition %s: %w", node.ID, err)
	}
	eval.scope[node.ID] = result
	log.FromContext(ctx).V(1).Info("evaluated definition node", "node", node.ID, "keys", len(result))

	if len(node.ReadyWhen) > 0 {
		if err := eval.checkReadiness(node.ReadyWhen, eval.scope[node.ID], node.ID); err != nil {
			eval.markReady(node.ID, false)
			return err
		}
	}
	eval.markReady(node.ID, true)

	return nil
}

// resolveReference determines Own vs Contribute for an Unresolved node by
// checking whether the target resource exists. Absent → Own, exists → check
// the kro label. If the resource has this Graph's label, it's Own (we created
// it on a previous revision). If it has no kro label or another Graph's label,
// it's Contribute. Force annotation always resolves to Own.
func (r *GraphReconciler) resolveReference(ctx context.Context, graph *unstructured.Unstructured, node Node, eval *evaluator) (Reference, error) {
	logger := log.FromContext(ctx)

	evalMap, err := eval.toMap(node.Template)
	if err != nil {
		// Template can't evaluate yet — expressions unresolvable.
		// Return Unresolved so it's retried next reconcile.
		return ReferenceUnresolved, fmt.Errorf("resolving reference for %s: %w", node.ID, err)
	}

	// Force annotation is an explicit ownership claim — always Own.
	obj := &unstructured.Unstructured{Object: evalMap}
	if isForceApply(obj) {
		logger.V(1).Info("reference resolved: Own (Force annotation)", "node", node.ID)
		return ReferenceOwn, nil
	}

	if obj.GetNamespace() == "" {
		obj.SetNamespace(graph.GetNamespace())
	}

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(obj.GroupVersionKind())
	err = r.Client.Get(ctx, client.ObjectKey{Namespace: obj.GetNamespace(), Name: obj.GetName()}, existing)
	if err != nil {
		if apierrors.IsNotFound(err) {
			logger.V(1).Info("reference resolved: Own (resource absent)", "node", node.ID)
			return ReferenceOwn, nil
		}
		return ReferenceUnresolved, fmt.Errorf("checking resource existence for reference detection %s: %w", node.ID, err)
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
				logger.V(1).Info("reference resolved: Contribute (resource has our contribute label)", "node", node.ID)
				return ReferenceContribute, nil
			}
			logger.V(1).Info("reference resolved: Own (resource has our identity label)", "node", node.ID)
			return ReferenceOwn, nil
		}
	}

	logger.V(1).Info("reference resolved: Contribute (resource exists, not ours)", "node", node.ID)
	return ReferenceContribute, nil
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
			return fmt.Errorf("watch %s: resource %s/%s %s/%s not found: %w", node.ID, apiVersion, kind, namespace, name, ErrPending)
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
		items[i] = normalizeTypes(item.Object)
	}
	eval.scope[node.ID] = items
	logger.V(1).Info("resolved collection watch", "node", node.ID, "gvk", gvk, "count", len(items))

	// Per 001-graph.md: "A collection watch's .ready() returns true when the
	// node's readyWhen conditions pass (evaluated once against the whole array,
	// not per-item)."
	//
	// Readiness is determined by readyWhen outcome (or absence of readyWhen).
	// __ready is stamped AFTER readyWhen evaluation — one code path, not two
	// compensating ones.
	ready := true
	if len(node.ReadyWhen) > 0 {
		if err := eval.checkReadiness(node.ReadyWhen, eval.scope[node.ID], node.ID); err != nil {
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

// reconcileOwn evaluates and applies an Own template, checks readyWhen.
// driftCorrection bypasses the template-hash check in applyResource.
func (r *GraphReconciler) reconcileOwn(ctx context.Context, graph *unstructured.Unstructured, node Node, eval *evaluator, watcher *graphWatcher, driftCorrection bool) (string, error) {
	logger := log.FromContext(ctx)

	evalMap, err := eval.toMap(node.Template)
	if err != nil {
		return "", fmt.Errorf("template %s: %w", node.ID, err)
	}

	applied, err := r.applyResource(ctx, graph, evalMap, watcher, node.ID, driftCorrection)
	if err != nil {
		return "", err
	}

	eval.scope[node.ID] = normalizeTypes(applied.Object)
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
// driftCorrection bypasses the template-hash check in applyContribution.
func (r *GraphReconciler) reconcileContribute(ctx context.Context, graph *unstructured.Unstructured, node Node, eval *evaluator, watcher *graphWatcher, driftCorrection bool) (string, error) {
	logger := log.FromContext(ctx)

	evalMap, err := eval.toMap(node.Template)
	if err != nil {
		return "", fmt.Errorf("contribute %s: %w", node.ID, err)
	}

	applied, err := r.applyContribution(ctx, graph, evalMap, watcher, node.ID, driftCorrection)
	if err != nil {
		return "", err
	}
	eval.scope[node.ID] = normalizeTypes(applied.Object)
	logger.V(1).Info("contributed to resource", "node", node.ID, "gvk", applied.GroupVersionKind(), "name", applied.GetName())

	// Evaluate readyWhen if present, matching the reconcileOwn pattern.
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
	// to release fields) from Own keys (delete).
	hasStatus := evalMap["status"] != nil
	key := contributeKey(applied, hasStatus)
	return key, nil
}
