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

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	krocel "github.com/kubernetes-sigs/kro/pkg/graphengine/cel"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/compiler"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/compiler/variable"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/runtime/resolver"
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

	// ignored caches the IsIgnored result for the current reconcile so
	// repeated calls don't re-walk the dependency tree.
	ignored      *bool
	ignoredCause error
}

// ID returns the node's identifier.
func (n *Node) ID() string { return n.spec.ID }

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

// IsCollection mirrors compiler.Node.IsCollection.
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
		return *n.ignored, n.ignoredCause
	}
	ignored, err := n.computeIgnored()
	n.ignored = &ignored
	n.ignoredCause = err
	return ignored, err
}

func (n *Node) computeIgnored() (bool, error) {
	// Contagious: if any dep is ignored, so are we.
	for _, dep := range n.deps {
		ignored, err := dep.IsIgnored()
		if err != nil {
			return false, fmt.Errorf("dep %q: %w", dep.ID(), err)
		}
		if ignored {
			return true, nil
		}
	}
	// AND-fold the local includeWhen list. Empty list = always included.
	for _, expr := range n.spec.IncludeWhen {
		v, err := expr.Eval(n.rt.scope)
		if err != nil {
			if isCELDataPending(err) {
				return false, fmt.Errorf("node %q: includeWhen %q: %w (%w)", n.spec.ID, expr.UserExpression(), err, ErrDataPending)
			}
			return false, fmt.Errorf("node %q: includeWhen %q: %w", n.spec.ID, expr.UserExpression(), err)
		}
		b, ok := v.(bool)
		if !ok {
			return false, fmt.Errorf("node %q: includeWhen %q returned %T, want bool", n.spec.ID, expr.UserExpression(), v)
		}
		if !b {
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
	// readyWhen evaluation needs the node's observed value. If the
	// executor hasn't called SetObserved yet, treat as pending so the
	// reconciler requeues.
	if n.observed == nil {
		return fmt.Errorf("node %q: no observed state: %w", n.spec.ID, ErrWaitingForReadiness)
	}

	// For singletons, evaluate against scope (the node's value already
	// published). For collections, evaluate readyWhen per instance with
	// the iteration value bound to the node ID — readyWhen for collections
	// is "every element is ready", expressed via CEL's all().
	for _, expr := range n.spec.ReadyWhen {
		v, err := expr.Eval(n.rt.scope)
		if err != nil {
			if isCELDataPending(err) {
				return fmt.Errorf("node %q: readyWhen %q: %w (%w)", n.spec.ID, expr.UserExpression(), err, ErrWaitingForReadiness)
			}
			return fmt.Errorf("node %q: readyWhen %q: %w", n.spec.ID, expr.UserExpression(), err)
		}
		b, ok := v.(bool)
		if !ok {
			return fmt.Errorf("node %q: readyWhen %q returned %T, want bool", n.spec.ID, expr.UserExpression(), v)
		}
		if !b {
			return fmt.Errorf("node %q: readyWhen %q is false: %w", n.spec.ID, expr.UserExpression(), ErrWaitingForReadiness)
		}
	}
	return nil
}

// Resolve renders the node into one or more unstructured objects. For a
// non-collection node the result has length 1. For a node with forEach
// the result is one rendered object per cartesian-product combination of
// the axes' evaluated lists, capped at MaxCollectionSize.
//
// Resolve evaluates against the Runtime's current scope but does not
// write to it; the caller decides whether to publish the rendered or
// the cluster-observed value back via SetObserved + Runtime.Set.
func (n *Node) Resolve() ([]*unstructured.Unstructured, error) {
	rows, err := n.expand()
	if err != nil {
		return nil, fmt.Errorf("node %q: %w", n.spec.ID, err)
	}
	out := make([]*unstructured.Unstructured, 0, len(rows))
	for _, bindings := range rows {
		obj, err := n.renderOne(bindings)
		if err != nil {
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
		for k, v := range n.rt.scope {
			scope[k] = v
		}
		for k, v := range bindings {
			scope[k] = v
		}
	}

	out := n.spec.Object.DeepCopy()
	if len(n.spec.Variables) == 0 {
		return out, nil
	}

	data := make(map[string]any, len(n.spec.Variables))
	for _, v := range n.spec.Variables {
		val, err := v.Expression.Eval(scope)
		if err != nil {
			if isCELDataPending(err) {
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
	return out, nil
}

// expand evaluates each forEach axis against the runtime scope and
// returns the cartesian product of bindings, capped at MaxCollectionSize.
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
			return nil, fmt.Errorf("forEach %q: %w", axis.Name, err)
		}
		dims = append(dims, evaluatedDimension{name: axis.Name, values: items})
	}
	return cartesianProduct(dims, n.rt.maxCollectionSize)
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

// toFieldDescriptors strips ResourceField wrappers down to the bare
// FieldDescriptor list the resolver expects.
func toFieldDescriptors(vars []*variable.ResourceField) []variable.FieldDescriptor {
	out := make([]variable.FieldDescriptor, len(vars))
	for i, v := range vars {
		out[i] = v.FieldDescriptor
	}
	return out
}
