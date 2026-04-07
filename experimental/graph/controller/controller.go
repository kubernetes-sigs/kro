// Package graphcontroller implements a proof-of-concept Graph controller.
//
// The controller watches Graph custom resources and reconciles them in two phases:
//
// Phase 1 — Revision management:
//  1. Ensure a GraphRevision exists for the current Graph spec generation
//  2. If the spec changed, materialize + compile + create a new revision
//  3. Manage revision activation (old stays Active until new revision converges)
//
// Phase 2 — Resource reconciliation (from the active revision):
//  1. Parse the active revision's spec into a DAG
//  2. Walk the DAG in topological order, evaluating pre-compiled CEL programs
//  3. Apply evaluated templates via server-side apply
//  4. Prune resources removed between revisions
//  5. Update revision and Graph status
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
	Caches    *graphCaches      // per-revision compiled expression caches
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

	// -----------------------------------------------------------------------
	// Phase 1: Revision management
	// -----------------------------------------------------------------------
	//
	// Ensure a GraphRevision exists for the current generation. If the spec
	// changed (generation bumped), materialize a new revision. A revision can
	// only be created if compilation succeeds — its existence proves validity.

	activeRevision, previousRevision, err := r.ensureRevision(ctx, graph)
	if err != nil {
		// Compilation or materialization failure — no revision created.
		// Report the error on the Graph and return.
		logger.Error(err, "ensuring revision")
		_ = r.updateStatus(ctx, graph, &reconcileState{accepted: false, acceptedErr: err})
		return ctrl.Result{}, err
	}

	// -----------------------------------------------------------------------
	// Phase 2: Resource reconciliation from the active revision
	// -----------------------------------------------------------------------

	// Parse and compile the active revision's spec (cached by revision name).
	revisionSpec, cache, err := r.compileRevision(activeRevision)
	if err != nil {
		logger.Error(err, "compiling revision spec")
		_ = r.updateStatus(ctx, graph, &reconcileState{accepted: false, acceptedErr: err})
		return ctrl.Result{}, err
	}

	eval := newEvaluator(cache)
	dag := cache.dag
	plan := NewPlanState(dag)
	var appliedKeys []string

	// Walk DAG in topological order: observe, plan, execute.
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

	// Derive aggregate state from the DAG plan
	hasDataPending, hasNotReady, _, _, hasConflict, readyCount := plan.Summary()
	needsRequeue := hasDataPending || hasNotReady

	// Collect detected contributions for status reporting.
	var contributions []string
	for id := range dag.Contributions {
		contributions = append(contributions, id)
	}

	// -----------------------------------------------------------------------
	// Prune resources removed between revisions
	// -----------------------------------------------------------------------
	if !hasDataPending && previousRevision != nil {
		if err := r.pruneRemovedResources(ctx, graph, previousRevision, appliedKeys); err != nil {
			logger.Error(err, "pruning removed resources")
		}
	}

	// -----------------------------------------------------------------------
	// Update status on Graph and revision
	// -----------------------------------------------------------------------
	rstate := &reconcileState{
		accepted:       true,
		resourceCount:  len(revisionSpec.Resources),
		appliedCount:   readyCount,
		needsRequeue:   needsRequeue,
		hasDataPending: hasDataPending,
		hasNotReady:    hasNotReady,
		hasConflict:    hasConflict,
		contributions:  contributions,
	}
	if err := r.updateStatus(ctx, graph, rstate); err != nil {
		logger.Error(err, "status update")
	}

	// Update revision status conditions
	allReady := !needsRequeue && !hasConflict && rstate.accepted
	r.updateRevisionStatus(ctx, activeRevision, previousRevision, allReady)

	if needsRequeue {
		return ctrl.Result{RequeueAfter: defaultRequeueAfter}, nil
	}
	return ctrl.Result{}, nil
}

// ---------------------------------------------------------------------------
// Phase 1: Revision management
// ---------------------------------------------------------------------------

// ensureRevision guarantees that a GraphRevision exists for the current Graph
// generation. Returns the active revision to reconcile from, and the previous
// revision (if any) for prune diffing.
//
// On first reconcile (no revisions exist): creates revision, returns it as active.
// On spec change (new generation): creates new revision, returns it as active
// and the old active revision as previous.
// On steady state: returns existing active revision, nil previous.
func (r *GraphReconciler) ensureRevision(ctx context.Context, graph *unstructured.Unstructured) (active *unstructured.Unstructured, previous *unstructured.Unstructured, err error) {
	logger := log.FromContext(ctx)
	graphName := graph.GetName()
	namespace := graph.GetNamespace()
	generation := graph.GetGeneration()

	// Check if a revision already exists for this generation
	revName := revisionName(graphName, generation)
	existing, err := getRevision(ctx, r.Client, revName, namespace)
	if err == nil {
		// Revision exists for this generation. Find previous (if any).
		prev, _ := r.findPreviousRevision(ctx, graphName, namespace, generation)
		return existing, prev, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, nil, fmt.Errorf("checking revision %s: %w", revName, err)
	}

	// No revision for this generation. Parse, compile, and create one.
	// If compilation fails, no revision is created — the failure is reported
	// on the Graph. A revision can only exist if processing succeeded.
	graphSpec, err := extractGraphSpec(graph.Object)
	if err != nil {
		return nil, nil, fmt.Errorf("extracting graph spec: %w", err)
	}

	// Compile to verify validity before creating the revision.
	_, err = compileGraph(graphSpec, generation)
	if err != nil {
		return nil, nil, err
	}

	// Materialize the revision
	revision := materialize(graph, graphSpec)
	if err := createRevision(ctx, r.Client, revision); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Race: another reconcile created it. Fetch and use it.
			existing, getErr := getRevision(ctx, r.Client, revName, namespace)
			if getErr != nil {
				return nil, nil, fmt.Errorf("fetching existing revision after race: %w", getErr)
			}
			prev, _ := r.findPreviousRevision(ctx, graphName, namespace, generation)
			return existing, prev, nil
		}
		return nil, nil, fmt.Errorf("creating revision %s: %w", revName, err)
	}
	logger.Info("created revision", "revision", revName, "generation", generation)

	// Set initial conditions on the new revision
	_ = setRevisionCondition(ctx, r.Client, revision, RevisionConditionPropagated, ConditionTrue, "Propagated", "Controller is reconciling from this revision")

	// Find previous active revision for prune diffing
	prev, _ := r.findPreviousRevision(ctx, graphName, namespace, generation)

	// Re-fetch the revision to get the server-assigned metadata
	active, err = getRevision(ctx, r.Client, revName, namespace)
	if err != nil {
		return nil, nil, fmt.Errorf("re-fetching revision %s: %w", revName, err)
	}

	return active, prev, nil
}

// findPreviousRevision finds the most recent revision before the given
// generation that was active.
func (r *GraphReconciler) findPreviousRevision(ctx context.Context, graphName, namespace string, currentGen int64) (*unstructured.Unstructured, error) {
	revisions, err := listRevisions(ctx, r.Client, graphName, namespace)
	if err != nil {
		return nil, err
	}

	// Walk backwards through sorted revisions (ascending by generation)
	for i := len(revisions) - 1; i >= 0; i-- {
		rev := revisions[i]
		gen := revisionGeneration(rev)
		if gen < currentGen {
			return rev, nil
		}
	}
	return nil, nil
}

// compileRevision parses and compiles a revision's spec, using the cache
// keyed by namespace/revision-name. The namespace prefix prevents collisions
// when two Graphs in different namespaces share the same name (and thus
// produce identically-named revisions).
func (r *GraphReconciler) compileRevision(revision *unstructured.Unstructured) (*GraphSpec, *graphCache, error) {
	cacheKey := revision.GetNamespace() + "/" + revision.GetName()
	cache := r.Caches.get(cacheKey)
	if cache != nil {
		return cache.spec, cache, nil
	}

	spec, err := extractRevisionSpec(revision)
	if err != nil {
		return nil, nil, err
	}

	generation := revisionGeneration(revision)
	cache, err = compileGraph(spec, generation)
	if err != nil {
		return nil, nil, err
	}

	r.Caches.set(cacheKey, cache)
	return spec, cache, nil
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
	// Contribution detection is from the template shape (only metadata/status
	// keys) — determined at compile time and stored in the DAG.
	if eval.cache.dag.Contributions[res.ID] {
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
	lbls := obj.GetLabels()
	if lbls == nil {
		lbls = map[string]string{}
	}
	lbls[LabelGraphName] = graph.GetName()
	lbls[LabelGraphNamespace] = graph.GetNamespace()
	obj.SetLabels(lbls)

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

// pruneRemovedResources deletes managed resources that exist in the previous
// revision but not in the current applied set. This replaces the old
// annotation-based prune tracking — the revision IS the record.
func (r *GraphReconciler) pruneRemovedResources(ctx context.Context, graph *unstructured.Unstructured, previousRevision *unstructured.Unstructured, currentKeys []string) error {
	logger := log.FromContext(ctx)

	// Parse the previous revision's spec to get its resource templates
	prevSpec, err := extractRevisionSpec(previousRevision)
	if err != nil {
		return fmt.Errorf("parsing previous revision spec: %w", err)
	}

	// Build current key set for fast lookup
	currentSet := map[string]bool{}
	for _, k := range currentKeys {
		currentSet[k] = true
	}

	// Check the concrete resource keys from the previous revision's spec.
	// For each resource with a static name, verify it should still exist.
	prevGenStr := fmt.Sprintf("%d", revisionGeneration(previousRevision))
	for _, res := range prevSpec.Resources {
		if res.Template == nil {
			continue
		}
		// Extract static name from template metadata
		md, _ := res.Template["metadata"].(map[string]any)
		if md == nil {
			continue
		}
		name, _ := md["name"].(string)
		if name == "" || strings.Contains(name, "${") {
			// Dynamic name — can't determine prune target from spec alone.
			// These are handled by the current key set comparison below.
			continue
		}

		apiVersion, _ := res.Template["apiVersion"].(string)
		kind, _ := res.Template["kind"].(string)

		// Build the resource key as would be produced during reconciliation
		gv, _ := schema.ParseGroupVersion(apiVersion)
		gvk := gv.WithKind(kind)
		key := strings.Join([]string{gvk.Group, gvk.Version, gvk.Kind, graph.GetNamespace(), name}, "/")

		if currentSet[key] {
			continue // still exists in current revision
		}

		// Resource was in previous revision but not applied in current cycle.
		// Check if it exists and is ours before deleting.
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(gvk)
		if err := r.Client.Get(ctx, types.NamespacedName{Name: name, Namespace: graph.GetNamespace()}, obj); err != nil {
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

		// Verify it's from the previous generation (not updated by current revision)
		if genLabel, ok := objLabels[LabelGraphGeneration]; ok && genLabel != prevGenStr {
			continue // already updated to a different generation
		}

		if err := r.Client.Delete(ctx, obj); err != nil {
			if client.IgnoreNotFound(err) != nil {
				return fmt.Errorf("pruning %s: %w", key, err)
			}
		} else {
			logger.Info("pruned resource from previous revision", "key", key)
			r.Resources.remove(key)
		}
	}

	return nil
}

// updateRevisionStatus updates the conditions on the active and previous
// revisions based on the reconcile outcome.
//
// TODO: Implement old revision garbage collection. The design specifies that
// old revisions are pruned when they no longer have resources in the cluster.
// Currently revisions accumulate across Graph updates (but are cleaned up on
// Graph deletion). After activation, list non-active revisions and delete
// those whose unique resources no longer exist in the cluster.
// See: experimental/docs/design/graph/002-revisions.md § Lifecycle
func (r *GraphReconciler) updateRevisionStatus(ctx context.Context, active, previous *unstructured.Unstructured, allReady bool) {
	logger := log.FromContext(ctx)

	if allReady {
		// All resources are ready — activate this revision
		if err := setRevisionCondition(ctx, r.Client, active, RevisionConditionReady, ConditionTrue, "Ready", "All resources reconciled"); err != nil {
			logger.V(1).Info("failed to set revision Ready", "error", err)
		}
		if revisionConditionStatus(active, RevisionConditionActive) != ConditionTrue {
			if err := setRevisionCondition(ctx, r.Client, active, RevisionConditionActive, ConditionTrue, "Active", "This is the current revision"); err != nil {
				logger.V(1).Info("failed to set revision Active", "error", err)
			}
			// Deactivate the previous revision
			if previous != nil {
				if err := setRevisionCondition(ctx, r.Client, previous, RevisionConditionActive, ConditionFalse, "Superseded", "Superseded by newer revision"); err != nil {
					logger.V(1).Info("failed to deactivate previous revision", "error", err)
				}
			}
		}
	} else {
		// Resources still converging — mark as not yet ready.
		// Use Unknown (not False) to distinguish "not yet evaluated" from
		// "evaluated and failed." Per the design: Ready starts Unknown,
		// converges to True when fully propagated.
		if err := setRevisionCondition(ctx, r.Client, active, RevisionConditionReady, ConditionUnknown, "Progressing", "Resources not yet fully reconciled"); err != nil {
			logger.V(1).Info("failed to set revision Ready=Unknown", "error", err)
		}
	}
}

// ---------------------------------------------------------------------------
// Deletion
// ---------------------------------------------------------------------------

func (r *GraphReconciler) reconcileDelete(ctx context.Context, graph *unstructured.Unstructured) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if r.Watcher != nil {
		r.Watcher.removeGraph(types.NamespacedName{Name: graph.GetName(), Namespace: graph.GetNamespace()})
	}

	// Clean up all expression caches for this Graph's revisions.
	revisions, _ := listRevisions(ctx, r.Client, graph.GetName(), graph.GetNamespace())
	for _, rev := range revisions {
		r.Caches.remove(rev.GetNamespace() + "/" + rev.GetName())
	}

	// Clean up the resource cache.
	r.Resources.removeAll()

	// Collect all managed resource keys from all revisions for this Graph.
	// This ensures we clean up resources from any revision, not just the latest.
	allKeys := map[string]bool{}
	for _, rev := range revisions {
		spec, err := extractRevisionSpec(rev)
		if err != nil {
			continue
		}
		for _, res := range spec.Resources {
			if res.Template == nil {
				continue
			}
			// Skip contribution templates — they write to objects owned by
			// someone else. The Graph controller must not delete them on
			// teardown; it only wrote partial metadata/status fields.
			if isContributionTemplate(res.Template) {
				continue
			}
			md, _ := res.Template["metadata"].(map[string]any)
			if md == nil {
				continue
			}
			name, _ := md["name"].(string)
			if name == "" || strings.Contains(name, "${") {
				continue // dynamic names — need label-based lookup
			}
			apiVersion, _ := res.Template["apiVersion"].(string)
			kind, _ := res.Template["kind"].(string)
			gv, _ := schema.ParseGroupVersion(apiVersion)
			gvk := gv.WithKind(kind)
			key := strings.Join([]string{gvk.Group, gvk.Version, gvk.Kind, graph.GetNamespace(), name}, "/")
			allKeys[key] = true
		}
	}

	// Convert to slice for ordered deletion
	var keys []string
	for k := range allKeys {
		keys = append(keys, k)
	}

	// Also include dynamically-named resources found by label selector.
	// This catches forEach-stamped resources that aren't in the static spec.
	dynamicKeys, _ := r.findManagedResourceKeys(ctx, graph)
	for _, k := range dynamicKeys {
		if !allKeys[k] {
			keys = append(keys, k)
		}
	}

	// Pass 1: Issue deletes in reverse topological order.
	// Track which keys we actually attempted to delete (had our hash).
	deletedKeys := map[string]bool{}
	deleteOrder := r.deletionOrder(graph, keys)
	for _, key := range deleteOrder {
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
		objAnnotations := obj.GetAnnotations()
		if objAnnotations == nil || objAnnotations[templateHashAnnotation] == "" {
			logger.V(1).Info("skipping delete for resource without template hash (never successfully applied)", "key", key)
			continue
		}

		deletedKeys[key] = true
		if err := r.Client.Delete(ctx, obj); err != nil {
			if client.IgnoreNotFound(err) != nil {
				return ctrl.Result{}, fmt.Errorf("deleting managed resource %s: %w", key, err)
			}
		} else {
			logger.V(1).Info("deleted managed resource", "key", key)
		}
	}

	// Pass 2: Verify managed resources that we actually deleted are gone.
	// Only check resources that had our template hash — others (e.g., conflicted
	// resources that were never successfully applied) are not our responsibility.
	for key := range deletedKeys {
		gvk, nn := parseResourceKey(key)
		if gvk.Kind == "" {
			continue
		}
		check := &unstructured.Unstructured{}
		check.SetGroupVersionKind(gvk)
		if err := r.Client.Get(ctx, nn, check); err == nil {
			logger.V(1).Info("waiting for managed resource to be deleted", "key", key)
			return ctrl.Result{RequeueAfter: 500 * time.Millisecond}, nil
		}
	}

	// Pass 3: Delete all GraphRevisions.
	for _, rev := range revisions {
		if err := deleteRevision(ctx, r.Client, rev); err != nil {
			if client.IgnoreNotFound(err) != nil {
				logger.Error(err, "deleting revision", "revision", rev.GetName())
			}
		} else {
			logger.V(1).Info("deleted revision", "revision", rev.GetName())
		}
	}

	controllerutil.RemoveFinalizer(graph, finalizer)
	if err := r.Client.Update(ctx, graph); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
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
		for _, res := range spec.Resources {
			if res.Template == nil {
				continue
			}
			if isContributionTemplate(res.Template) {
				continue // contributions don't create resources we own
			}
			apiVersion, _ := res.Template["apiVersion"].(string)
			kind, _ := res.Template["kind"].(string)
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

// deletionOrder returns resource keys ordered for deletion: reverse dependency
// order from the DAG. Rebuilds the DAG from the Graph spec. Maps resource keys
// to DAG positions by matching kind/name. Unmatched keys are deleted first.
func (r *GraphReconciler) deletionOrder(graph *unstructured.Unstructured, keys []string) []string {
	graphSpec, err := extractGraphSpec(graph.Object)
	if err != nil {
		return keys
	}
	dag, err := BuildDAG(graphSpec.Resources)
	if err != nil {
		return keys
	}

	// Map kind/name to DAG index from static template metadata.
	kindNameToIndex := map[string]int{}
	for i, node := range dag.Nodes {
		tmpl := node.Resource.Template
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
		kindNameToIndex[kind+"/"+name] = i
	}

	type scored struct {
		key   string
		index int
	}
	scored_keys := make([]scored, 0, len(keys))
	for _, key := range keys {
		if key == "" {
			continue
		}
		gvk, nn := parseResourceKey(key)
		idx, ok := kindNameToIndex[gvk.Kind+"/"+nn.Name]
		if !ok {
			idx = len(dag.Nodes) // unmatched → deleted first
		}
		scored_keys = append(scored_keys, scored{key: key, index: idx})
	}

	sort.Slice(scored_keys, func(i, j int) bool {
		return scored_keys[i].index > scored_keys[j].index
	})

	result := make([]string, len(scored_keys))
	for i, s := range scored_keys {
		result[i] = s.key
	}
	return result
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
