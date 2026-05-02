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
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/ellistarn/kro/experimental/controller/compiler"
	graphpkg "github.com/ellistarn/kro/experimental/controller/graph"
)

// ---------------------------------------------------------------------------
// Node reconciliation methods
// ---------------------------------------------------------------------------

// nodeOutput is the return value of reconcileNode. It replaces the previous
// ([]string, *forEachCarryForward, error) tuple so forEach-specific state doesn't
// thread through a generic interface — most node types leave forEach nil.
type nodeOutput struct {
	keys    []Applied
	forEach *forEachCarryForward // nil for non-forEach nodes
}

// reconcileNode dispatches to the appropriate handler based on node type.
// NodeType is a parse-time property of the node; no runtime resolution.
//
// prevForEachState carries forEach state from the previous reconcile. It is
// only used when the node has a forEach clause; nil otherwise.
//
// After dispatch, reconcileNode evaluates readyWhen as a post-dispatch step
// for node types that don't handle their own per-item readiness (Definition,
// Template, Patch). Watch and ForEach return early — they handle
// readiness internally (per-item for ForEach, per-collection for Watch).
//
// Error contract:
//   - ErrPending: retryable, data not yet available
//   - ErrWaitingForReadiness: applied but readyWhen not satisfied
//   - other error: fatal
func (c *clusterAccess) reconcileNode(ctx context.Context, rs *reconcileScope, node graphpkg.Node, eval *evaluator, resyncCorrection bool, prevForEachState *forEachCarryForward) (*nodeOutput, error) {
	if node.ForEach != nil {
		return c.reconcileForEach(ctx, rs, node, eval, resyncCorrection, prevForEachState)
	}

	nodeType := node.Type()
	switch nodeType {
	case graphpkg.NodeTypeDef:
		if err := c.reconcileDefinition(ctx, node, eval); err != nil {
			return nil, err
		}
	case graphpkg.NodeTypeWatch:
		err := c.reconcileWatch(ctx, rs, node, eval)
		return &nodeOutput{}, err // Watch handles its own readiness
	case graphpkg.NodeTypeRef:
		if err := c.reconcileRef(ctx, rs, node, eval); err != nil {
			return nil, err
		}
	default: // NodeTypeTemplate, NodeTypePatch
		appliedEntry, err := c.reconcileApply(ctx, rs, node, eval, resyncCorrection)
		if err != nil {
			if appliedEntry.Key != "" {
				return &nodeOutput{keys: []Applied{appliedEntry}}, err
			}
			return nil, err
		}
		return &nodeOutput{keys: []Applied{appliedEntry}}, eval.evalReadiness(node.ID, node.ReadyWhen)
	}

	// Post-dispatch readyWhen for Definition and Ref (no keys to return).
	return &nodeOutput{}, eval.evalReadiness(node.ID, node.ReadyWhen)
}

// reconcileDefinition evaluates a definition node — resolves values from the template
// (literals and/or CEL expressions) and enters the result into scope as
// map[string]any. No Kubernetes API calls are made.
func (c *clusterAccess) reconcileDefinition(ctx context.Context, node graphpkg.Node, eval *evaluator) error {
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
func (c *clusterAccess) reconcileRef(ctx context.Context, rs *reconcileScope, node graphpkg.Node, eval *evaluator) error {
	logger := log.FromContext(ctx)

	tmpl, err := eval.toMapNode(node)
	if err != nil {
		return fmt.Errorf("ref %s: %w", node.ID, err)
	}

	gvk := graphpkg.GVKFromMap(tmpl)
	md, _ := tmpl["metadata"].(map[string]any)

	name, _ := md["name"].(string)
	namespace, _ := md["namespace"].(string)
	// Only default namespace for namespace-scoped resources.
	// Cluster-scoped resources (CRDs, ClusterRoles, etc.) must keep
	// namespace empty — setting it breaks watch event routing because
	// the metadata informer reports events with namespace="" while
	// the scalar index would store namespace="<graph-ns>".
	namespace = defaultNamespace(gvk, namespace, rs.namespace, c.scope)

	if rs.watcher != nil {
		rs.watcher.WatchScalar(node.ID, gvkToGVR(gvk), gvk.Kind, name, namespace)
	}

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	if err := c.reader.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, obj); err != nil {
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
// A full List is performed every reconcile cycle — no incremental caching.
func (c *clusterAccess) reconcileWatch(ctx context.Context, rs *reconcileScope, node graphpkg.Node, eval *evaluator) error {
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

	if rs.watcher != nil {
		rs.watcher.WatchCollection(node.ID, gvkToGVR(gvk), gvk.Kind, watchNamespace, labelSelector)
	}

	// Full list every cycle — simple and correct.
	listGVK := gvk
	listGVK.Kind = gvk.Kind + "List"

	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(listGVK)
	if err := c.reader.List(ctx, list, &client.ListOptions{
		LabelSelector: labelSelector,
		Namespace:     watchNamespace,
	}); err != nil {
		return fmt.Errorf("listing %s with selector %s: %w", gvk, labelSelector, err)
	}

	items := make([]any, len(list.Items))
	for i, item := range list.Items {
		items[i] = graphpkg.NormalizeTypes(item.Object)
	}
	logger.V(1).Info("resolved watch", "node", node.ID, "gvk", gvk, "count", len(items))

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
// The node type controls SSA behavior (identity labels, pre-apply
// checks) and cleanup semantics: Template resources are deleted on prune,
// Patch resources have their fields released via release apply.
// See applySSA for the full type-dependent behavior.
func (c *clusterAccess) reconcileApply(ctx context.Context, rs *reconcileScope, node graphpkg.Node, eval *evaluator, resyncCorrection bool) (Applied, error) {
	logger := log.FromContext(ctx)

	nodeType := node.Type()

	evalMap, err := eval.toMapNode(node)
	if err != nil {
		return Applied{}, fmt.Errorf("%s %s: %w", nodeType, node.ID, err)
	}

	applied, err := c.applySSA(ctx, rs, evalMap, node.ID, nodeType, eval.effectiveGeneration, resyncCorrection, node.Lifecycle.ForceApply())
	if err != nil {
		return Applied{}, err
	}

	eval.scope[node.ID] = graphpkg.NormalizeTypes(applied.Object)
	// Just applied with effectiveGeneration — resource is on the latest generation.
	eval.markUpdated(node.ID, true)
	// Side effects (apply, delete, create) log at V(0) so operators see
	// which resources are being managed at default verbosity.
	logger.Info("applied resource", "node", node.ID, "nodeType", nodeType,
		"gvk", applied.GroupVersionKind(), "name", applied.GetName())

	return Applied{
		Key:       resourceKey(applied),
		NodeType:  nodeType,
		HasStatus: evalMap["status"] != nil,
	}, nil
}

// ---------------------------------------------------------------------------
// Label selector parsing
// ---------------------------------------------------------------------------

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
