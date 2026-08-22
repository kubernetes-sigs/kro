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

// Package runtime is the per-reconciliation execution context. It owns the
// CEL evaluation scope, exposes nodes in topological order, and lets a
// caller resolve each node's desired state. Nodes carry observed cluster
// state once the executor has set it, which feeds readyWhen checks and
// downstream CEL evaluation. The package does not talk to the cluster —
// that responsibility lives in pkg/executor.
package runtime

import (
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apiserver/pkg/cel/openapi"
	"k8s.io/kube-openapi/pkg/validation/spec"

	expv1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	celunstructured "github.com/kubernetes-sigs/kro/pkg/cel/unstructured"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/compiler"
	"github.com/kubernetes-sigs/kro/pkg/metrics"
)

// Runtime is the per-reconciliation execution state. It carries the
// compiled Program, a reference to the source Graph, a scope map that
// is mutated as the executor walks nodes, and a node graph wired with
// dependency pointers so contagious ignore checks and readiness gating
// can walk transitively.
//
// A Runtime is single-use: construct one per reconciliation, walk it,
// then discard. Not safe for concurrent use.
type Runtime struct {
	program *compiler.Program
	graph   *expv1alpha1.Graph
	scope   map[string]any
	nodes   []*Node
	byID    map[string]*Node

	// applyOrders maps node ID to its one-based reverse-topological layer.
	// Higher numbers are deleted
	// first (dependents before dependencies). Persisted on each managed
	// resource as metadata.ApplyOrderAnnotation for the deletion path.
	applyOrders map[string]int

	// maxCollectionSize caps forEach expansion for this Runtime. Per-
	// Runtime rather than package-global so concurrent reconciles with
	// different intended caps don't race. Zero disables the cap (with
	// the overflow safeguard in cartesianProduct still active).
	maxCollectionSize int

	// nodeObjectOverrides replaces a literal Def node's compiled payload with
	// a per-Runtime value at render time. This is what lets one compiled
	// Program be shared across every instance of a revision: the `schema`
	// node is compiled with an empty, schema-typed payload (no instance data),
	// and each reconcile injects its own instance data here. The override is
	// deep-copied on render, and lives on the per-Runtime Node wrapper — never
	// on the shared Program — so concurrent reconciles cannot leak data into
	// one another.
	nodeObjectOverrides map[string]*unstructured.Unstructured
}

// Option configures a Runtime at construction time.
type Option func(*Runtime)

// WithMaxCollectionSize overrides the default forEach expansion cap.
// Use 0 to disable the cap entirely.
func WithMaxCollectionSize(n int) Option {
	return func(r *Runtime) { r.maxCollectionSize = n }
}

// WithNodeObjectOverride replaces the literal payload of the named Def node
// with obj when the Runtime renders it, instead of the value baked into the
// compiled Program. This decouples per-instance data from the compiled
// Program so one Program (compiled with an empty, schema-typed node) safely
// serves every instance of a revision. obj is used as the node's rendered
// value verbatim (deep-copied on render); nothing is written back to the
// shared Program.
func WithNodeObjectOverride(nodeID string, obj *unstructured.Unstructured) Option {
	return func(r *Runtime) {
		if r.nodeObjectOverrides == nil {
			r.nodeObjectOverrides = make(map[string]*unstructured.Unstructured)
		}
		r.nodeObjectOverrides[nodeID] = obj
	}
}

// WithSeedScope pre-populates the runtime scope with a snapshot of another
// scope. Used to construct a child (subgraph) Runtime seeded from its
// parent's scope: child expressions can read captured parent values, and a
// child node that publishes the same name shadows the inherited one. The
// seed is copied, so later mutations to the child scope never reach back
// into the parent.
func WithSeedScope(seed map[string]any) Option {
	return func(r *Runtime) {
		for k, v := range seed {
			r.scope[k] = v
		}
	}
}

// MaxCollectionSize returns this Runtime's forEach expansion cap.
func (r *Runtime) MaxCollectionSize() int { return r.maxCollectionSize }

// New constructs a Runtime around the supplied Program and source Graph.
// Dependency pointers are wired so each Node can walk its transitive
// upstream set without touching the Program directly.
func New(prog *compiler.Program, g *expv1alpha1.Graph, opts ...Option) *Runtime {
	start := time.Now()
	defer func() {
		metrics.RuntimeCreationTotal.Inc()
		metrics.RuntimeCreationDuration.Observe(time.Since(start).Seconds())
	}()

	rt := &Runtime{
		program:           prog,
		graph:             g,
		scope:             make(map[string]any, len(prog.Nodes)),
		byID:              make(map[string]*Node, len(prog.Nodes)),
		maxCollectionSize: DefaultMaxCollectionSize,
	}
	for _, opt := range opts {
		opt(rt)
	}
	// Build the node wrappers in topological order so callers
	// see them in apply order.
	rt.nodes = make([]*Node, 0, len(prog.TopologicalOrder))
	for _, id := range prog.TopologicalOrder {
		n := &Node{spec: prog.Nodes[id], rt: rt, deps: map[string]*Node{}}
		rt.nodes = append(rt.nodes, n)
		rt.byID[id] = n
	}
	// Apply per-node payload overrides now that byID is populated. Used to
	// inject an instance's `schema` data into a Program compiled with an empty
	// schema node (see WithNodeObjectOverride).
	for id, obj := range rt.nodeObjectOverrides {
		if n, ok := rt.byID[id]; ok {
			n.objectOverride = obj
		}
	}
	// Wire dependency pointers. Each Node carries pointers to
	// the Node objects it depends on (not just IDs) so IsIgnored /
	// CheckReadiness can recurse without a runtime lookup loop.
	for _, n := range rt.nodes {
		for _, depID := range n.spec.HardDepIDs() {
			if dep, ok := rt.byID[depID]; ok {
				n.deps[depID] = dep
			}
		}
	}
	// Soft dependencies carry no edge and no ordering, so a soft-referencing
	// node can resolve before its target publishes. Seed each soft target with
	// an empty object so an optional access (id.?field / id[?"k"]) yields
	// optional.none() — the field is omitted — rather than a "no such attribute"
	// error that would data-pend the node. Once the target applies, publishScope
	// overwrites the seed with the live value.
	for _, n := range rt.nodes {
		for _, softID := range n.spec.SoftDepIDs() {
			if _, seeded := rt.scope[softID]; !seeded {
				rt.scope[softID] = map[string]any{}
			}
		}
	}
	// Compute one-based reverse-topological apply orders. Nodes are
	// already in topological order (dependencies precede dependents), so a
	// single forward pass yields order(n) = 1 + max(order(deps)); leaves = 1.
	// Def nodes (e.g. the synthetic `schema`/instance node) are excluded from
	// the depth calculation and never stamped: apply orders cover only real
	// resource nodes.
	rt.applyOrders = make(map[string]int, len(rt.nodes))
	for _, n := range rt.nodes {
		if n.Kind() == compiler.NodeKindDef {
			continue
		}
		order := 1
		for _, depID := range n.spec.HardDepIDs() {
			dep, ok := rt.byID[depID]
			if !ok || dep.Kind() == compiler.NodeKindDef {
				continue
			}
			if d := rt.applyOrders[depID]; d+1 > order {
				order = d + 1
			}
		}
		rt.applyOrders[n.ID()] = order
	}
	return rt
}

// ApplyOrder returns the one-based reverse-topological layer for the node,
// used to persist the deletion wave as metadata.ApplyOrderAnnotation. The
// bool is false for unknown node IDs.
func (r *Runtime) ApplyOrder(nodeID string) (int, bool) {
	o, ok := r.applyOrders[nodeID]
	return o, ok
}

// Program returns the compiled Program backing this Runtime.
func (r *Runtime) Program() *compiler.Program { return r.program }

// Graph returns the source Graph object (used for namespace defaulting).
func (r *Runtime) Graph() *expv1alpha1.Graph { return r.graph }

// Nodes returns the nodes in topological order. The slice is owned by
// the Runtime; callers must not mutate it.
func (r *Runtime) Nodes() []*Node { return r.nodes }

// Node returns the node with the given ID, or nil if no such node exists.
func (r *Runtime) Node(id string) *Node { return r.byID[id] }

// Scope returns the current scope map. The map is live — mutating it
// affects subsequent Resolve calls. Most callers should use Set instead.
func (r *Runtime) Scope() map[string]any { return r.scope }

// Set publishes value under id in the scope so downstream nodes can read
// it via CEL expressions like ${id.field}. Values with known OpenAPI schemas
// on Template and Ref nodes are wrapped via UnstructuredToVal so CEL format
// annotations (such as format: "byte") are respected at runtime.
func (r *Runtime) Set(id string, value any) {
	var sc *spec.Schema
	isTemplateOrRef := false
	if r.program != nil {
		sc = r.program.NodeSchemas[id]
	}
	if node, ok := r.byID[id]; ok {
		isTemplateOrRef = node.Kind() == compiler.NodeKindTemplate || node.Kind() == compiler.NodeKindRef
	}
	r.scope[id] = wrapValueForScope(value, sc, isTemplateOrRef)
}

func wrapValueForScope(val any, sc *spec.Schema, isTemplateOrRef bool) any {
	if !isTemplateOrRef || sc == nil {
		return val
	}
	switch v := val.(type) {
	case map[string]any:
		return celunstructured.UnstructuredToVal(v, &openapi.Schema{Schema: sc})
	case []any:
		itemSchema := sc
		if sc.Type.Contains("array") && sc.Items != nil && sc.Items.Schema != nil {
			itemSchema = sc.Items.Schema
		}
		list := make([]any, len(v))
		for i, item := range v {
			if m, ok := item.(map[string]any); ok {
				list[i] = celunstructured.UnstructuredToVal(m, &openapi.Schema{Schema: itemSchema})
			} else {
				list[i] = item
			}
		}
		return list
	default:
		return val
	}
}
