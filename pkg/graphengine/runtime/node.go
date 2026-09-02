// Copyright 2026 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package runtime

import (
	"errors"
	"fmt"
	"maps"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/kube-openapi/pkg/validation/spec"

	krocel "github.com/kubernetes-sigs/kro/pkg/cel"
	"github.com/kubernetes-sigs/kro/pkg/cel/sentinels"
	"github.com/kubernetes-sigs/kro/pkg/graph/fieldpath"
	"github.com/kubernetes-sigs/kro/pkg/graph/resolver"
	"github.com/kubernetes-sigs/kro/pkg/graph/variable"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/compiler"
	"github.com/kubernetes-sigs/kro/pkg/metrics"
)

// Node is the runtime view of a single compiled Node. It carries the
// dependency graph wiring, the cluster-observed state (set by the
// executor after apply), and the resolution / readiness / inclusion
// primitives used by the executor's main loop.
type Node struct {
	spec *compiler.Node
	rt   *Runtime

	// deps holds pointers to the Node wrappers this node depends on.
	// Wired at Runtime.New time so IsIgnored / CheckReadiness can recurse
	// without going through a map lookup loop.
	deps map[string]*Node

	// observed carries the cluster-side state after the executor has
	// applied or read this node. Nil before any SetObserved call; an
	// empty slice after observing a collection with zero items.
	observed []*unstructured.Unstructured

	// objectOverride, when non-nil, replaces spec.Object as the base payload
	// this node renders from. Set at Runtime.New time via
	// WithNodeObjectOverride so a Program compiled with an empty literal node
	// (no instance data) can be given the per-instance value at runtime.
	objectOverride *unstructured.Unstructured

	// ignored caches the IsIgnored result for the current reconcile so
	// repeated calls don't re-walk the dependency tree.
	ignored *bool
}

// ID returns the node's identifier.
func (n *Node) ID() string { return n.spec.ID }

// FirstUnreadyDep returns the ID of the first direct dependency of n that has
// not reached a ready state this cycle, and whether such a dependency exists.
// Avoids allocating an intermediate slice.
func (n *Node) FirstUnreadyDep(ready map[string]bool) (string, bool) {
	for id := range n.deps {
		if !ready[id] {
			return id, true
		}
	}
	return "", false
}

// Kind returns the node's compiled kind.
func (n *Node) Kind() compiler.NodeKind { return n.spec.Kind }

// Spec returns the underlying compiler.Node. Callers must not mutate it.
func (n *Node) Spec() *compiler.Node { return n.spec }

// GVR returns the target GroupVersionResource. Zero-valued for Def nodes.
func (n *Node) GVR() schema.GroupVersionResource { return n.spec.GVR }

// Namespaced reports whether the node's GVR is namespace-scoped. Always
// false for Def nodes and dynamic-GVK templates (resolved at apply time).
func (n *Node) Namespaced() bool { return n.spec.Namespaced }

// DynamicGVK reports whether the node's target GVK is a CEL expression,
// so its GVR and REST scope must be resolved per rendered object at apply
// time rather than read from the compiled spec.
func (n *Node) DynamicGVK() bool { return n.spec.DynamicGVK }

// Subresource returns the target subresource a patch node contributes to
// ("status" or ""). Empty for every other kind.
func (n *Node) Subresource() string { return n.spec.Subresource }

// SelfWatchExempt reports whether drift-watch registration should be skipped
// for this node's target (compiler.Node.SelfWatchExempt). True for the RGD
// adapter's synthesized author-status writeback node, which targets the
// reconciled instance's own status subresource — watching it would
// self-retrigger the instance's reconcile.
func (n *Node) SelfWatchExempt() bool { return n.spec.SelfWatchExempt }

// IsCollection reports whether the node expands into a collection,
// delegating to compiler.Node.IsCollection.
func (n *Node) IsCollection() bool { return n.spec.IsCollection() }

// Observed returns the cluster-observed state for this node, or nil if
// SetObserved has not been called yet.
func (n *Node) Observed() []*unstructured.Unstructured { return n.observed }

// SetObserved records the cluster-side state for this node. Called by
// the executor after a successful apply (or for Def, after rendering).
// Collection nodes are aligned to desired order via orderedIntersection
// so downstream readyWhen sees the same order across reconciles.
func (n *Node) SetObserved(observed []*unstructured.Unstructured, desired []*unstructured.Unstructured) {
	if n.IsCollection() && len(desired) > 0 {
		n.observed = orderedIntersection(observed, desired)
		return
	}
	n.observed = observed
}

// IsIgnored reports whether this node should be skipped this reconcile.
// A node is ignored when:
//   - any of its dependencies is ignored (contagious propagation), OR
//   - any of its own includeWhen expressions evaluates to false
//
// The result is memoized within a single Runtime so repeated checks
// during apply don't re-walk the dependency tree or re-evaluate CEL.
func (n *Node) IsIgnored() (bool, error) {
	if n.ignored != nil {
		return *n.ignored, nil
	}
	ignored, err := n.computeIgnored()
	if err == nil {
		n.ignored = &ignored
	}
	return ignored, err
}

func (n *Node) computeIgnored() (bool, error) {
	metrics.NodeIgnoredCheckTotal.Inc()
	// Contagious: if any dep is ignored, so are we.
	for _, dep := range n.deps {
		ignored, err := dep.IsIgnored()
		if err != nil {
			return false, fmt.Errorf("dep %q: %w", dep.ID(), err)
		}
		if ignored {
			metrics.NodeIgnoredTotal.Inc()
			return true, nil
		}
	}
	// AND-fold the local includeWhen list. Empty list = always included.
	for _, expr := range n.spec.IncludeWhen {
		v, err := expr.Eval(n.rt.scope)
		if err != nil {
			if IsCELDataPending(err) {
				return false, fmt.Errorf("node %q: includeWhen %q: %w (%w)", n.spec.ID, expr.UserExpression(), err, ErrDataPending)
			}
			return false, fmt.Errorf("node %q: includeWhen %q: %w", n.spec.ID, expr.UserExpression(), err)
		}
		b, ok := v.(bool)
		if !ok {
			return false, fmt.Errorf("node %q: includeWhen %q returned %T, want bool", n.spec.ID, expr.UserExpression(), v)
		}
		if !b {
			metrics.NodeIgnoredTotal.Inc()
			return true, nil
		}
	}
	return false, nil
}

// CheckReadiness evaluates readyWhen against the node's observed state.
// Returns nil when the node is ready (or has no readyWhen conditions),
// ErrWaitingForReadiness when the cluster hasn't converged yet, or a
// hard error for user-facing mistakes (non-bool result, eval error that
// isn't data-pending).
//
// Ignored nodes are treated as ready — dependents shouldn't block on
// something we never tried to apply.
func (n *Node) CheckReadiness() error {
	ignored, err := n.IsIgnored()
	if err != nil {
		return fmt.Errorf("node %q: %w", n.spec.ID, err)
	}
	if ignored {
		return nil
	}
	// Empty readyWhen means "ready as soon as applied". Skip the
	// observed-state check entirely so nodes that don't declare
	// readiness don't accidentally gate dependents.
	if len(n.spec.ReadyWhen) == 0 {
		return nil
	}
	metrics.NodeReadyCheckTotal.Inc()
	// readyWhen evaluation needs the node's observed value. If the
	// executor hasn't called SetObserved yet, treat as pending so the
	// reconciler requeues.
	if n.observed == nil {
		metrics.NodeNotReadyTotal.Inc()
		return fmt.Errorf("node %q: no observed state: %w", n.spec.ID, ErrWaitingForReadiness)
	}

	// Collections evaluate readyWhen per element with `each` bound to the
	// item, AND-folded across every item and every expression. A non-
	// collection node evaluates each expression once against scope.
	if n.IsCollection() {
		return n.checkCollectionReadiness()
	}

	// For singletons, evaluate against scope (the node's value already
	// published).
	for _, expr := range n.spec.ReadyWhen {
		v, err := expr.Eval(n.rt.scope)
		if err != nil {
			if IsCELDataPending(err) {
				metrics.NodeNotReadyTotal.Inc()
				return fmt.Errorf("node %q: readyWhen %q: %w (%w)", n.spec.ID, expr.UserExpression(), err, ErrWaitingForReadiness)
			}
			return fmt.Errorf("node %q: readyWhen %q: %w", n.spec.ID, expr.UserExpression(), err)
		}
		b, ok := v.(bool)
		if !ok {
			return fmt.Errorf("node %q: readyWhen %q returned %T, want bool", n.spec.ID, expr.UserExpression(), v)
		}
		if !b {
			metrics.NodeNotReadyTotal.Inc()
			return fmt.Errorf("node %q: readyWhen %q is false: %w", n.spec.ID, expr.UserExpression(), ErrWaitingForReadiness)
		}
	}
	return nil
}

// checkCollectionReadiness evaluates a collection node's readyWhen list
// per observed item, with `each` bound to that item's object, and AND-folds
// the result across all items and all expressions. observed is the applied/
// SSA-response set recorded by the executor (no live cluster re-read).
//
// Count gate: an empty observed set means either the collection legitimately
// expanded to zero items (ready) or nothing has landed yet. Because the
// executor records observed = applied, an empty set here is a
// resolved-empty collection and is treated as ready.
func (n *Node) checkCollectionReadiness() error {
	if len(n.observed) == 0 {
		return nil
	}
	var itemSc *spec.Schema
	if n.rt.program != nil {
		if sc, ok := n.rt.program.NodeSchemas[n.ID()]; ok && sc != nil {
			if sc.Type.Contains("array") && sc.Items != nil && sc.Items.Schema != nil {
				itemSc = sc.Items.Schema
			} else {
				itemSc = sc
			}
		}
	}

	scope := make(map[string]any, len(n.rt.scope)+1)
	maps.Copy(scope, n.rt.scope)

	for i, obj := range n.observed {
		scope[compiler.EachVarName] = wrapValueForScope(obj.Object, itemSc, true)
		for _, expr := range n.spec.ReadyWhen {
			v, err := expr.Eval(scope)
			if err != nil {
				if IsCELDataPending(err) {
					return fmt.Errorf("node %q: readyWhen %q (item %d): %w (%w)", n.spec.ID, expr.UserExpression(), i, err, ErrWaitingForReadiness)
				}
				return fmt.Errorf("node %q: readyWhen %q (item %d): %w", n.spec.ID, expr.UserExpression(), i, err)
			}
			b, ok := v.(bool)
			if !ok {
				return fmt.Errorf("node %q: readyWhen %q (item %d) returned %T, want bool", n.spec.ID, expr.UserExpression(), i, v)
			}
			if !b {
				metrics.NodeNotReadyTotal.Inc()
				return fmt.Errorf("node %q: readyWhen %q (item %d) is false: %w", n.spec.ID, expr.UserExpression(), i, ErrWaitingForReadiness)
			}
		}
	}
	return nil
}

// Resolve renders the node into one or more unstructured objects. For a
// non-collection node the result has length 1. For a node with forEach
// the result is one rendered object per cartesian-product combination of
// the axes' evaluated lists, rejected when it would exceed MaxCollectionSize.
//
// Resolve evaluates against the Runtime's current scope but does not
// write to it; the caller decides whether to publish the rendered or
// the cluster-observed value back via SetObserved + Runtime.Set.
func (n *Node) Resolve() ([]*unstructured.Unstructured, error) {
	start := time.Now()
	defer func() {
		metrics.NodeEvalTotal.Inc()
		metrics.NodeEvalDuration.Observe(time.Since(start).Seconds())
	}()

	rows, err := n.expand()
	if err != nil {
		metrics.NodeEvalErrorsTotal.Inc()
		return nil, fmt.Errorf("node %q: %w", n.spec.ID, err)
	}
	out := make([]*unstructured.Unstructured, 0, len(rows))
	for _, bindings := range rows {
		obj, err := n.renderOne(bindings)
		if err != nil {
			metrics.NodeEvalErrorsTotal.Inc()
			return nil, err
		}
		out = append(out, obj)
	}
	// For template-kind collections, defend against the case where
	// identity-field expressions silently produce duplicate names — kro
	// catches it at compile time via iterator-coverage analysis, but
	// runtime values can still collide if def-sourced values overlap.
	if n.spec.Kind == compiler.NodeKindTemplate && n.IsCollection() {
		if err := validateUniqueIdentities(out); err != nil {
			metrics.NodeEvalErrorsTotal.Inc()
			return nil, fmt.Errorf("node %q: %w", n.spec.ID, err)
		}
	}
	return out, nil
}

// renderOne produces one unstructured by evaluating every Variable
// against (scope ∪ bindings) and handing the (expression text → value)
// map to the resolver. Per-instance scope is layered: when bindings is
// non-empty a fresh map is built so iteration values don't leak across
// instances.
func (n *Node) renderOne(bindings map[string]any) (*unstructured.Unstructured, error) {
	scope := n.rt.scope
	if len(bindings) > 0 {
		scope = make(map[string]any, len(n.rt.scope)+len(bindings))
		maps.Copy(scope, n.rt.scope)
		maps.Copy(scope, bindings)
	}

	src := n.spec.Object
	if n.objectOverride != nil {
		src = n.objectOverride
	}
	if src == nil {
		return nil, fmt.Errorf("node %q has no template object to render", n.spec.ID)
	}
	out := src.DeepCopy()
	if len(n.spec.Variables) == 0 {
		return out, nil
	}

	data := make(map[string]any, len(n.spec.Variables))
	// omitArrayFields collects the map-key paths of enclosing array fields whose
	// element was data-pending under tolerance. We cannot omit a single array
	// element (cleanOmitSentinels would drop it and shift every later index
	// down), so we drop the whole enclosing array field after Resolve — see
	// below.
	var omitArrayFields [][]string
	for _, v := range n.spec.Variables {
		val, err := v.Expression.Eval(scope)
		if err != nil {
			if IsCELDataPending(err) {
				if n.spec.TolerateDataPending {
					if fields, ok := enclosingArrayFieldPath(v.Path); ok {
						// The pending value lives inside an array (e.g.
						// status.foo[2] or status.conditions[1].type). A single
						// array element cannot be omitted without shifting later
						// indices, so instead we omit the whole enclosing array
						// field (status.foo / status.conditions) for this render.
						// The array reappears complete on the next reconcile once
						// the upstream resolves. Stash a placeholder so the resolver
						// still sees data for this expression, then delete the
						// enclosing field after Resolve.
						data[v.Expression.Original] = sentinels.Omit{}
						omitArrayFields = append(omitArrayFields, fields)
						continue
					}
					// A pending map property (no array index in its path): omit just
					// this field and keep rendering the rest. The resolver strips
					// the sentinel so a data-pending map field disappears rather
					// than data-pending the whole node.
					if isObjectProperty(v.Path) {
						data[v.Expression.Original] = sentinels.Omit{}
						continue
					}
				}
				return nil, fmt.Errorf("node %q: eval %q at %q: %w (%w)", n.spec.ID, v.Expression.UserExpression(), v.Path, err, ErrDataPending)
			}
			return nil, fmt.Errorf("node %q: eval %q at %q: %w", n.spec.ID, v.Expression.UserExpression(), v.Path, err)
		}
		data[v.Expression.Original] = val
	}

	summary := resolver.NewResolver(out.Object, data).Resolve(toFieldDescriptors(n.spec.Variables))
	if len(summary.Errors) > 0 {
		return nil, fmt.Errorf("node %q: resolve: %w", n.spec.ID, errors.Join(summary.Errors...))
	}
	// Drop enclosing array fields after Resolve so the outcome is independent of
	// what sibling elements resolved into the same array this cycle.
	for _, fields := range omitArrayFields {
		unstructured.RemoveNestedField(out.Object, fields...)
	}
	return out, nil
}

// expand evaluates each forEach axis against the runtime scope and
// returns the cartesian product of bindings, rejected when it would exceed MaxCollectionSize.
// The zero-axes case yields a single empty binding row so non-collection
// nodes still produce exactly one rendered output.
func (n *Node) expand() ([]map[string]any, error) {
	if len(n.spec.ForEach) == 0 {
		return []map[string]any{{}}, nil
	}
	dims := make([]evaluatedDimension, 0, len(n.spec.ForEach))
	for _, axis := range n.spec.ForEach {
		items, err := evalList(axis.Expression, n.rt.scope)
		if err != nil {
			// A forEach axis referencing not-yet-published upstream data is a
			// soft, retryable condition — mirror the scalar renderOne path
			// (which wraps ErrDataPending) rather than turning a normal pending
			// field into a permanent hard graph failure.
			if IsCELDataPending(err) {
				return nil, fmt.Errorf("forEach %q: %w (%w)", axis.Name, err, ErrDataPending)
			}
			return nil, fmt.Errorf("forEach %q: %w", axis.Name, err)
		}
		dims = append(dims, evaluatedDimension{name: axis.Name, values: items})
	}
	rows, err := cartesianProduct(dims, n.rt.maxCollectionSize)
	if err != nil {
		return nil, err
	}
	metrics.CollectionSize.Observe(float64(len(rows)))
	return rows, nil
}

// evalList evaluates expr against scope and asserts the result is a Go
// slice. Returns a clear error when the runtime value isn't list-shaped
// — most callers reach this only when the static type analysis fell
// back to dyn (e.g. forEach over a Def node).
func evalList(expr *krocel.Expression, scope map[string]any) ([]any, error) {
	val, err := expr.Eval(scope)
	if err != nil {
		return nil, fmt.Errorf("eval: %w", err)
	}
	switch v := val.(type) {
	case []any:
		return v, nil
	case nil:
		return nil, fmt.Errorf("expected list, got nil")
	default:
		return nil, fmt.Errorf("expected list, got %T", val)
	}
}

// isObjectProperty returns true if path targets an object (map) property
// rather than an indexed slice/array element.
func isObjectProperty(path string) bool {
	segments, err := fieldpath.Parse(path)
	if err != nil || len(segments) == 0 {
		return false
	}
	return segments[len(segments)-1].Index < 0
}

// enclosingArrayFieldPath computes, for a path that indexes into an array
// (e.g. status.foo[2], status.bar.foo[2], status.matrix[1][2]), the map-key
// segments naming the enclosing array field — the prefix of Name segments up
// to (but excluding) the first array index. It returns those segment names
// and true when path contains an array index, or nil and false when path is a
// plain map property. Examples:
//   - status.foo[2]       -> [status foo]
//   - status.bar.foo[2]   -> [status bar foo]
//   - status.matrix[1][2] -> [status matrix]
func enclosingArrayFieldPath(path string) ([]string, bool) {
	segments, err := fieldpath.Parse(path)
	if err != nil {
		return nil, false
	}
	var names []string
	for _, seg := range segments {
		if seg.Index >= 0 {
			return names, len(names) > 0
		}
		names = append(names, seg.Name)
	}
	return nil, false
}

// toFieldDescriptors strips ResourceField wrappers down to the bare
// FieldDescriptor list the resolver expects.
func toFieldDescriptors(vars []*variable.ResourceField) []variable.FieldDescriptor {
	out := make([]variable.FieldDescriptor, len(vars))
	for i, v := range vars {
		out[i] = v.FieldDescriptor
	}
	return out
}
