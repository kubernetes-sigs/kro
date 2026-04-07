// Package graphcontroller implements a proof-of-concept Graph controller.
//
// The controller watches Graph custom resources and reconciles them by:
//  1. Parsing spec.resources into a DAG (dependency graph from CEL expression analysis)
//  2. Compiling all CEL expressions eagerly (cached per Graph spec generation)
//  3. Walking the DAG in topological order, evaluating pre-compiled CEL programs
//  4. Applying evaluated templates to the API server via server-side apply
//  5. Reading back created resources to populate scope for downstream expressions
//  6. Checking readyWhen/includeWhen conditions and propagating exclusion through the DAG
//  7. Registering dynamic watches on externalRef targets and owned resources
package graphcontroller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/metadata"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

var GraphGVK = schema.GroupVersionKind{
	Group:   "kro.run",
	Version: "v1alpha1",
	Kind:    "Graph",
}

const (
	fieldOwner = "graph-controller"
	finalizer  = "kro.run/graph-controller"

	// defaultRequeueAfter is used when a resource is not yet ready
	// and we need to wait for the dynamic watch to trigger. This is
	// a fallback — the watch should fire first in most cases.
	defaultRequeueAfter = 1 * time.Second
)

// GraphReconciler reconciles Graph objects.
type GraphReconciler struct {
	Client    client.Client
	Watcher   *WatchCoordinator // nil = no dynamic watches (backward compat with existing tests)
	Caches    *graphCaches      // per-Graph compiled expression caches
	Resources *resourceCache    // per-resource full object cache
}

func (r *GraphReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, reconcileErr error) {
	logger := log.FromContext(ctx)

	// 1. Get the Graph
	graph := &unstructured.Unstructured{}
	graph.SetGroupVersionKind(GraphGVK)
	if err := r.Client.Get(ctx, req.NamespacedName, graph); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Handle deletion
	if !graph.GetDeletionTimestamp().IsZero() {
		return r.reconcileDelete(ctx, graph)
	}

	// Add finalizer
	if !controllerutil.ContainsFinalizer(graph, finalizer) {
		controllerutil.AddFinalizer(graph, finalizer)
		if err := r.Client.Update(ctx, graph); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Set up watch tracking for this reconcile cycle.
	var watcher *graphWatcher
	if r.Watcher != nil {
		watcher = r.Watcher.forGraph(req.NamespacedName)
		defer func() {
			watcher.done(reconcileErr == nil)
		}()
	}

	// 2. Parse spec and compile (or reuse cached).
	// The compilation phase parses the spec, builds the DAG, and compiles all
	// CEL expressions. Everything is cached per Graph spec generation.
	cacheKey := req.NamespacedName.String()
	cache := r.Caches.get(cacheKey)
	if cache == nil || cache.generation != graph.GetGeneration() {
		graphSpec, err := extractGraphSpec(graph.Object)
		if err != nil {
			logger.Error(err, "extracting graph spec")
			_ = r.updateStatus(ctx, graph, &reconcileState{reconcileErr: err}, nil, nil)
			return ctrl.Result{}, err
		}
		cache, err = compileGraph(graphSpec, graph.GetGeneration())
		if err != nil {
			logger.Error(err, "compiling graph expressions")
			_ = r.updateStatus(ctx, graph, &reconcileState{reconcileErr: err}, nil, nil)
			return ctrl.Result{}, err
		}
		r.Caches.set(cacheKey, cache)
		logger.V(1).Info("compiled graph expressions",
			"generation", graph.GetGeneration(),
			"expressions", len(cache.programs))
	}

	eval := newEvaluator(cache)
	graphSpec := cache.spec
	dag := cache.dag
	plan := NewPlanState(dag)
	var appliedKeys []string

	// 4. Walk DAG in topological order: observe, plan, execute.
	for _, idx := range dag.TopologicalOrder {
		node := &dag.Nodes[idx]
		res := node.Resource

		// Skip if already excluded by contagious propagation
		if plan.States[res.ID] == NodeExcluded {
			logger.V(1).Info("skipping contagiously excluded resource", "resource", res.ID)
			continue
		}

		// Check if dependencies allow processing
		if canProcess, blockedBy := plan.CanProcess(node); !canProcess {
			logger.V(1).Info("skipping resource due to blocked dependency",
				"resource", res.ID, "blockedBy", blockedBy)
			plan.SetState(dag, res.ID, NodeExcluded)
			continue
		}

		// Evaluate includeWhen
		if len(res.IncludeWhen) > 0 {
			included, err := eval.includeWhen(res.IncludeWhen)
			if err != nil {
				if errors.Is(err, ErrDataPending) {
					logger.V(1).Info("includeWhen data pending", "resource", res.ID)
					plan.SetState(dag, res.ID, NodeDataPending)
					continue
				}
				logger.Error(err, "evaluating includeWhen", "resource", res.ID)
				return ctrl.Result{}, err
			}
			if !included {
				logger.V(1).Info("resource excluded by includeWhen", "resource", res.ID)
				plan.SetState(dag, res.ID, NodeExcluded)
				continue
			}
		}

		// Dispatch by resource type. All handlers return (keys, error) with
		// the same error contract: ErrDataPending, ErrWaitingForReadiness, or fatal.
		keys, err := r.reconcileResource(ctx, graph, res, eval, watcher)
		if err != nil {
			if errors.Is(err, ErrDataPending) {
				logger.V(1).Info("resource data pending", "resource", res.ID)
				plan.SetState(dag, res.ID, NodeDataPending)
				continue
			}
			if errors.Is(err, ErrWaitingForReadiness) {
				logger.V(1).Info("resource not ready", "resource", res.ID)
				plan.SetState(dag, res.ID, NodeNotReady)
				appliedKeys = append(appliedKeys, keys...)
				continue
			}
			if errors.Is(err, ErrFieldConflict) {
				logger.Info("resource field conflict", "resource", res.ID, "error", err)
				plan.SetState(dag, res.ID, NodeConflict)
				continue
			}
			return ctrl.Result{}, fmt.Errorf("resource %s: %w", res.ID, err)
		}
		appliedKeys = append(appliedKeys, keys...)
		plan.SetState(dag, res.ID, NodeReady)
	}

	// 5. Derive aggregate state from the DAG plan
	hasDataPending, hasNotReady, _, _, hasConflict, readyCount := plan.Summary()
	needsRequeue := hasDataPending || hasNotReady

	// 6. Prune + update status in a single read-modify-write sequence.
	// Both operations need a fresh read of the Graph object. Consolidating
	// them avoids a redundant GET.
	rstate := &reconcileState{
		resourceCount:  len(graphSpec.Resources),
		appliedCount:   readyCount,
		needsRequeue:   needsRequeue,
		hasDataPending: hasDataPending,
		hasNotReady:    hasNotReady,
		hasConflict:    hasConflict,
	}
	if err := r.pruneAndUpdateStatus(ctx, graph, appliedKeys, hasDataPending, rstate, graphSpec.StatusTemplate, eval); err != nil {
		logger.Error(err, "prune and status update")
	}

	if needsRequeue {
		return ctrl.Result{RequeueAfter: defaultRequeueAfter}, nil
	}
	return ctrl.Result{}, nil
}

// ---------------------------------------------------------------------------
// Resource reconciliation methods
// ---------------------------------------------------------------------------

// reconcileResource dispatches to the appropriate handler based on resource type.
// All paths return (keys, error) with a uniform error contract:
//   - ErrDataPending: retryable, data not yet available
//   - ErrWaitingForReadiness: applied but readyWhen not satisfied
//   - other error: fatal
func (r *GraphReconciler) reconcileResource(ctx context.Context, graph *unstructured.Unstructured, res Resource, eval *evaluator, watcher *graphWatcher) ([]string, error) {
	switch {
	case res.ForEach != nil:
		return r.reconcileForEach(ctx, graph, res, eval, watcher)
	case res.ExternalRef != nil:
		err := r.reconcileExternalRef(ctx, graph, res, eval, watcher)
		return nil, err
	case res.Template != nil:
		key, err := r.reconcileTemplate(ctx, graph, res, eval, watcher)
		if key != "" {
			return []string{key}, err
		}
		return nil, err
	default:
		return nil, nil
	}
}

// reconcileExternalRef reads an existing object (or collection) from the API server into scope.
func (r *GraphReconciler) reconcileExternalRef(ctx context.Context, graph *unstructured.Unstructured, res Resource, eval *evaluator, watcher *graphWatcher) error {
	logger := log.FromContext(ctx)

	ref, err := eval.toMap(res.ExternalRef)
	if err != nil {
		return fmt.Errorf("externalRef %s: %w", res.ID, err)
	}

	apiVersion, _ := ref["apiVersion"].(string)
	kind, _ := ref["kind"].(string)
	gv, _ := schema.ParseGroupVersion(apiVersion)
	gvk := gv.WithKind(kind)
	md, _ := ref["metadata"].(map[string]any)

	// selector → collection
	if md != nil {
		if selectorRaw, hasSelector := md["selector"]; hasSelector {
			return r.reconcileExternalRefSelector(ctx, graph, res, eval, gvk, selectorRaw, watcher)
		}
	}

	// name → single object
	name, _ := md["name"].(string)
	namespace, _ := md["namespace"].(string)
	if namespace == "" {
		namespace = graph.GetNamespace()
	}

	// Register watch BEFORE reading
	if watcher != nil {
		watcher.watchScalar(res.ID, gvkToGVR(gvk), name, namespace)
	}

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	if err := r.Client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, obj); err != nil {
		return fmt.Errorf("reading %s/%s %s/%s: %w", apiVersion, kind, namespace, name, err)
	}

	eval.scope[res.ID] = normalizeTypes(obj.Object)
	logger.V(1).Info("resolved externalRef", "resource", res.ID, "gvk", gvk, "name", obj.GetName())

	// Check readyWhen
	if len(res.ReadyWhen) > 0 {
		if err := eval.checkReadiness(res.ReadyWhen, eval.scope[res.ID], res.ID); err != nil {
			return err
		}
		logger.V(1).Info("externalRef ready", "resource", res.ID)
	}

	return nil
}

// reconcileExternalRefSelector reads a collection matching a label selector.
func (r *GraphReconciler) reconcileExternalRefSelector(ctx context.Context, graph *unstructured.Unstructured, res Resource, eval *evaluator, gvk schema.GroupVersionKind, selectorRaw any, watcher *graphWatcher) error {
	logger := log.FromContext(ctx)

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
		watcher.watchCollection(res.ID, gvkToGVR(gvk), graph.GetNamespace(), labelSelector)
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
	eval.scope[res.ID] = items
	logger.V(1).Info("resolved externalRef selector", "resource", res.ID, "gvk", gvk, "count", len(items))
	return nil
}

// reconcileForEach iterates a collection and stamps the template per item.
func (r *GraphReconciler) reconcileForEach(ctx context.Context, graph *unstructured.Unstructured, res Resource, eval *evaluator, watcher *graphWatcher) ([]string, error) {
	logger := log.FromContext(ctx)
	var keys []string

	for varName, collectionExpr := range res.ForEach {
		collection, err := eval.evalString(collectionExpr)
		if err != nil {
			if isDataPending(err) {
				return nil, fmt.Errorf("evaluating collection %q: %w", collectionExpr, ErrDataPending)
			}
			return nil, fmt.Errorf("evaluating collection %q: %w", collectionExpr, err)
		}

		items, ok := collection.([]any)
		if !ok {
			items = []any{collection}
		}
		logger.V(1).Info("forEach expanding", "resource", res.ID, "var", varName, "count", len(items))

		var allApplied []any
		for _, item := range items {
			if res.Template == nil {
				continue
			}
			innerScope := copyScope(eval.scope)
			innerScope[varName] = item
			innerEval := eval.withScope(innerScope)

			evalMap, err := innerEval.toMap(res.Template)
			if err != nil {
				return nil, fmt.Errorf("forEach %s item: %w", res.ID, err)
			}

			applied, err := r.applyResource(ctx, graph, evalMap, watcher)
			if err != nil {
				return keys, fmt.Errorf("applying %s item: %w", res.ID, err)
			}
			allApplied = append(allApplied, applied.Object)
			keys = append(keys, resourceKey(applied))
		}
		eval.scope[res.ID] = allApplied
	}

	// Check readyWhen per-item: all items must pass for the collection to be Ready.
	// Uses the same checkReadiness as singletons — the resource ID in scope resolves
	// to a single applied item for each evaluation. Apply-all-then-gate: all items
	// are applied and in scope before any readiness check, so scale-up isn't
	// serialized through prior item readiness.
	if len(res.ReadyWhen) > 0 {
		for _, applied := range eval.scope[res.ID].([]any) {
			if err := eval.checkReadiness(res.ReadyWhen, applied, res.ID); err != nil {
				return keys, err // ErrWaitingForReadiness — all items applied, but not all ready
			}
		}
		logger.V(1).Info("all forEach items ready", "resource", res.ID)
	}

	return keys, nil
}

// reconcileTemplate evaluates and applies a template, checks readyWhen.
func (r *GraphReconciler) reconcileTemplate(ctx context.Context, graph *unstructured.Unstructured, res Resource, eval *evaluator, watcher *graphWatcher) (string, error) {
	logger := log.FromContext(ctx)

	evalMap, err := eval.toMap(res.Template)
	if err != nil {
		return "", fmt.Errorf("template %s: %w", res.ID, err)
	}

	// Contributions auto-split: regular SSA for metadata/spec fields,
	// plus status subresource patch if .status is present.
	// This absorbs the Kubernetes subresource split — Graph authors don't
	// need to know about it.
	if res.Contribution {
		applied, err := r.applyContribution(ctx, graph, evalMap, watcher)
		if err != nil {
			return "", err
		}
		eval.scope[res.ID] = applied.Object
		logger.V(1).Info("contributed to resource", "resource", res.ID, "gvk", applied.GroupVersionKind(), "name", applied.GetName())
		return "", nil
	}

	applied, err := r.applyResource(ctx, graph, evalMap, watcher)
	if err != nil {
		return "", err
	}

	eval.scope[res.ID] = applied.Object
	key := resourceKey(applied)
	logger.V(1).Info("applied resource", "resource", res.ID, "gvk", applied.GroupVersionKind(), "name", applied.GetName(), "uid", applied.GetUID())

	// Check readyWhen
	if len(res.ReadyWhen) > 0 {
		if err := eval.checkReadiness(res.ReadyWhen, eval.scope[res.ID], res.ID); err != nil {
			return key, err
		}
		logger.V(1).Info("resource ready", "resource", res.ID)
	}

	return key, nil
}

// ---------------------------------------------------------------------------
// Apply + prune + delete
// ---------------------------------------------------------------------------

func (r *GraphReconciler) applyResource(ctx context.Context, graph *unstructured.Unstructured, evalMap map[string]any, watcher *graphWatcher) (*unstructured.Unstructured, error) {
	obj := &unstructured.Unstructured{Object: evalMap}

	if obj.GetNamespace() == "" {
		obj.SetNamespace(graph.GetNamespace())
	}

	// Label managed resources for visibility and selector queries.
	// Labels work for both namespaced and cluster-scoped resources,
	// unlike owner references which require same-scope.
	labels := obj.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels["kro.run/graph-name"] = graph.GetName()
	labels["kro.run/graph-namespace"] = graph.GetNamespace()
	obj.SetLabels(labels)

	// Watch before apply — ensures the metadata informer is running.
	gvr := gvkToGVR(obj.GroupVersionKind())
	if watcher != nil {
		watcher.watchScalar("owned/"+obj.GetName(), gvr, obj.GetName(), obj.GetNamespace())
	}

	// Content-addressed apply: hash the desired state to detect changes.
	// The hash is computed before adding the hash annotation itself.
	templateHash := hashDesiredState(obj.Object)
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
			// Object might not exist yet (race), fall through to apply
			if client.IgnoreNotFound(err) == nil {
				goto apply
			}
			return nil, fmt.Errorf("reading %s: %w", obj.GetName(), err)
		}
		// Update the cache with fresh data
		r.Resources.set(cacheKey, &cachedObject{
			resourceVersion: readBack.GetResourceVersion(),
			templateHash:    templateHash,
			object:          readBack.Object,
		})
		return readBack, nil
	}

apply:
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

	if err := r.Client.Patch(ctx, applied, client.RawPatch(types.ApplyPatchType, data), client.FieldOwner(fieldOwner)); err != nil {
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

// applyContribution applies a contribution resource — partial SSA with ForceOwnership.
// Contributions intentionally write fields on objects someone else owns.
// Auto-splits into two API calls when the template contains status fields:
//   - Regular SSA patch for metadata/spec/other fields (skips .status)
//   - Status subresource patch for .status fields
//
// Hash-gated: if the contribution's evaluated output hasn't changed, the Patch
// is skipped. When it does change, force-apply writes the new state.
func (r *GraphReconciler) applyContribution(ctx context.Context, graph *unstructured.Unstructured, evalMap map[string]any, watcher *graphWatcher) (*unstructured.Unstructured, error) {
	obj := &unstructured.Unstructured{Object: evalMap}

	if obj.GetNamespace() == "" {
		obj.SetNamespace(graph.GetNamespace())
	}

	// Watch the target resource for reactive updates
	gvr := gvkToGVR(obj.GroupVersionKind())
	if watcher != nil {
		watcher.watchScalar("contrib/"+obj.GetName(), gvr, obj.GetName(), obj.GetNamespace())
	}

	// Content-addressed apply: hash the desired state to detect changes.
	templateHash := hashDesiredState(evalMap)
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
			if client.IgnoreNotFound(err) == nil {
				goto apply
			}
			return nil, fmt.Errorf("reading %s: %w", obj.GetName(), err)
		}
		r.Resources.set(cacheKey, &cachedObject{
			resourceVersion: readBack.GetResourceVersion(),
			templateHash:    templateHash,
			object:          readBack.Object,
		})
		return readBack, nil
	}

apply:
	// Regular SSA for non-status fields. Strip .status to avoid silent
	// stripping by the API server when the status subresource is enabled.
	// Contributions use ForceOwnership — they intentionally write to
	// objects managed by others.
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
	if err := r.Client.Patch(ctx, target, client.RawPatch(types.ApplyPatchType, data), client.ForceOwnership, client.FieldOwner(fieldOwner)); err != nil {
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
		if err := r.Client.Status().Patch(ctx, statusTarget, client.RawPatch(types.ApplyPatchType, data), client.ForceOwnership, client.FieldOwner(fieldOwner)); err != nil {
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

const appliedResourcesAnnotation = "internal.kro.run/applied-resources"

func resourceKey(obj *unstructured.Unstructured) string {
	gvk := obj.GroupVersionKind()
	return strings.Join([]string{gvk.Group, gvk.Version, gvk.Kind, obj.GetNamespace(), obj.GetName()}, "/")
}

func parseResourceKey(key string) (schema.GroupVersionKind, types.NamespacedName) {
	parts := strings.SplitN(key, "/", 5)
	if len(parts) != 5 {
		return schema.GroupVersionKind{}, types.NamespacedName{}
	}
	return schema.GroupVersionKind{Group: parts[0], Version: parts[1], Kind: parts[2]},
		types.NamespacedName{Namespace: parts[3], Name: parts[4]}
}

// pruneAndUpdateStatus consolidates prune and status update into a single
// read-modify-write sequence on the Graph object. One GET instead of two.
func (r *GraphReconciler) pruneAndUpdateStatus(ctx context.Context, graph *unstructured.Unstructured, currentKeys []string, hasDataPending bool, state *reconcileState, statusTemplate map[string]any, eval *evaluator) error {
	logger := log.FromContext(ctx)

	// Single GET of the Graph object for both prune and status.
	latest := &unstructured.Unstructured{}
	latest.SetGroupVersionKind(GraphGVK)
	if err := r.Client.Get(ctx, client.ObjectKeyFromObject(graph), latest); err != nil {
		return fmt.Errorf("reading latest for prune+status: %w", err)
	}

	// --- Prune stale resources (only safe when all resources were resolved) ---
	if !hasDataPending {
		annotations := latest.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}

		previousKeysStr := annotations[appliedResourcesAnnotation]
		var previousKeys []string
		if previousKeysStr != "" {
			previousKeys = strings.Split(previousKeysStr, ";")
		}

		currentSet := map[string]bool{}
		for _, k := range currentKeys {
			currentSet[k] = true
		}

		for _, prevKey := range previousKeys {
			if currentSet[prevKey] || prevKey == "" {
				continue
			}
			gvk, nn := parseResourceKey(prevKey)
			if gvk.Kind == "" {
				continue
			}
			obj := &unstructured.Unstructured{}
			obj.SetGroupVersionKind(gvk)
			obj.SetName(nn.Name)
			obj.SetNamespace(nn.Namespace)

			// Check if we successfully owned this resource (has our hash annotation)
			if err := r.Client.Get(ctx, nn, obj); err != nil {
				continue // already gone
			}
			objAnnotations := obj.GetAnnotations()
			if objAnnotations == nil || objAnnotations[templateHashAnnotation] == "" {
				logger.V(1).Info("skipping prune for resource without template hash", "key", prevKey)
				continue
			}

			if err := r.Client.Delete(ctx, obj); err != nil {
				if client.IgnoreNotFound(err) != nil {
					return fmt.Errorf("pruning %s: %w", prevKey, err)
				}
			} else {
				logger.Info("pruned stale resource", "key", prevKey)
				r.Resources.remove(prevKey)
			}
		}

		// Only write the annotation if the key set actually changed.
		// This prevents a spurious write → resourceVersion bump → watch
		// event → re-reconcile loop in steady state.
		sort.Strings(currentKeys)
		newKeysStr := strings.Join(currentKeys, ";")
		if newKeysStr != previousKeysStr {
			annotations[appliedResourcesAnnotation] = newKeysStr
			latest.SetAnnotations(annotations)

			if err := r.Client.Update(ctx, latest); err != nil {
				return fmt.Errorf("updating applied resources annotation: %w", err)
			}

			// Re-read for status update (annotation Update bumps resourceVersion)
			if err := r.Client.Get(ctx, client.ObjectKeyFromObject(graph), latest); err != nil {
				return fmt.Errorf("re-reading after annotation update: %w", err)
			}
		}
	}

	// --- Update status ---
	return r.updateStatusOnLatest(ctx, latest, state, statusTemplate, eval)
}

func (r *GraphReconciler) reconcileDelete(ctx context.Context, graph *unstructured.Unstructured) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if r.Watcher != nil {
		r.Watcher.removeGraph(types.NamespacedName{Name: graph.GetName(), Namespace: graph.GetNamespace()})
	}

	// Clean up the expression cache for this Graph.
	cacheKey := types.NamespacedName{Name: graph.GetName(), Namespace: graph.GetNamespace()}.String()
	r.Caches.remove(cacheKey)

	// Clean up the resource cache.
	r.Resources.removeAll()

	// Actively delete all managed resources tracked in the annotation.
	// Without owner references, the finalizer is responsible for cleanup.
	// Two passes: first issue deletes, then verify all are gone.
	// If any resource still exists (e.g., a child Graph with its own finalizer),
	// requeue to wait for it to finish its own cleanup chain.
	annotations := graph.GetAnnotations()
	var keys []string
	if annotations != nil {
		keysStr := annotations[appliedResourcesAnnotation]
		if keysStr != "" {
			keys = strings.Split(keysStr, ";")
		}
	}

	// Pass 1: Issue deletes for all managed resources.
	// Only delete resources that have our template hash annotation — proof
	// that the controller successfully applied to them. Resources that were
	// in NodeConflict from the start (pre-existing, never successfully
	// applied) are left alone.
	for _, key := range keys {
		if key == "" {
			continue
		}
		gvk, nn := parseResourceKey(key)
		if gvk.Kind == "" {
			continue
		}
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(gvk)
		obj.SetName(nn.Name)
		obj.SetNamespace(nn.Namespace)

		// Check if we successfully owned this resource (has our hash annotation)
		if err := r.Client.Get(ctx, nn, obj); err != nil {
			continue // already gone
		}
		annotations := obj.GetAnnotations()
		if annotations == nil || annotations[templateHashAnnotation] == "" {
			logger.V(1).Info("skipping delete for resource without template hash (never successfully applied)", "key", key)
			continue
		}

		if err := r.Client.Delete(ctx, obj); err != nil {
			if client.IgnoreNotFound(err) != nil {
				return ctrl.Result{}, fmt.Errorf("deleting managed resource %s: %w", key, err)
			}
		} else {
			logger.V(1).Info("deleted managed resource", "key", key)
		}
	}

	// Pass 2: Verify all managed resources are actually gone.
	// Child Graphs have their own finalizers — they may still exist after Delete
	// while they clean up their own children. Wait for the full chain to unwind.
	for _, key := range keys {
		if key == "" {
			continue
		}
		gvk, nn := parseResourceKey(key)
		if gvk.Kind == "" {
			continue
		}
		check := &unstructured.Unstructured{}
		check.SetGroupVersionKind(gvk)
		if err := r.Client.Get(ctx, nn, check); err == nil {
			// Resource still exists (finalizer hasn't completed yet) — requeue
			logger.V(1).Info("waiting for managed resource to be deleted", "key", key)
			return ctrl.Result{RequeueAfter: 500 * time.Millisecond}, nil
		}
	}

	controllerutil.RemoveFinalizer(graph, finalizer)
	if err := r.Client.Update(ctx, graph); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// SetupWithManager registers the controller with the manager.
func (r *GraphReconciler) SetupWithManager(mgr ctrl.Manager) error {
	graphObj := &unstructured.Unstructured{}
	graphObj.SetGroupVersionKind(GraphGVK)
	return ctrl.NewControllerManagedBy(mgr).
		For(graphObj).
		Named("graph").
		Complete(r)
}

// gvkToGVR converts a GVK to a GVR using simple pluralization.
func gvkToGVR(gvk schema.GroupVersionKind) schema.GroupVersionResource {
	resource := strings.ToLower(gvk.Kind) + "s"
	switch strings.ToLower(gvk.Kind) {
	case "ingress":
		resource = "ingresses"
	}
	return schema.GroupVersionResource{
		Group:    gvk.Group,
		Version:  gvk.Version,
		Resource: resource,
	}
}

// TestEnv holds the components needed to run integration tests against
// the Graph controller with a real envtest API server.
type TestEnv struct {
	// Shutdown stops the watch manager. Call this in test cleanup.
	Shutdown func()
}

// SetupWithManagerForTest wires the Graph controller into a controller-runtime
// manager with dynamic watch support. Returns a TestEnv for cleanup.
//
// This encapsulates the internal watch machinery so test packages don't need
// access to unexported types.
func SetupWithManagerForTest(mgr ctrl.Manager, metadataClient metadata.Interface) *TestEnv {
	watchChan := make(chan event.GenericEvent, 256)

	watchMgr := newWatchManager(metadataClient, 0, nil, log.Log)
	coordinator := newWatchCoordinator(watchMgr, func(graph graphKey) {
		obj := &unstructured.Unstructured{}
		obj.SetName(graph.Name)
		obj.SetNamespace(graph.Namespace)
		watchChan <- event.GenericEvent{Object: obj}
	}, log.Log)
	watchMgr.onEvent = coordinator.routeEvent

	reconciler := &GraphReconciler{
		Client:    mgr.GetClient(),
		Watcher:   coordinator,
		Caches:    newGraphCaches(),
		Resources: newResourceCache(),
	}

	graphObj := &unstructured.Unstructured{}
	graphObj.SetGroupVersionKind(GraphGVK)

	if err := ctrl.NewControllerManagedBy(mgr).
		For(graphObj).
		Named("graph").
		WatchesRawSource(source.Channel(watchChan, &handler.EnqueueRequestForObject{})).
		Complete(reconciler); err != nil {
		panic("setting up graph controller: " + err.Error())
	}

	return &TestEnv{Shutdown: watchMgr.shutdown}
}
