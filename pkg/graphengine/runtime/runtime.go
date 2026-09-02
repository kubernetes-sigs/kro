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
	"maps"
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
		maps.Copy(r.scope, seed)
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

	nodeCount := 0
	if prog != nil {
		nodeCount = len(prog.Nodes)
	}

	rt := &Runtime{
		program:           prog,
		graph:             g,
		scope:             make(map[string]any, nodeCount),
		byID:              make(map[string]*Node, nodeCount),
		maxCollectionSize: DefaultMaxCollectionSize,
	}
	for _, opt := range opts {
		opt(rt)
	}
	if prog == nil {
		return rt
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
	// Drop any seeded scope value that collides with a child-LOCAL node ID.
	// WithSeedScope(parent.Scope()) copies the parent's whole scope so child
	// expressions can read captured parent values; but if the child declares
	// its own node with the same ID, that seeded parent value would shadow-
	// leak — the child would resolve the stale ANCESTOR value until it
	// publishes its own. Deleting the seed makes an unpublished child-local
	// node correctly absent (data-pending) rather than resolving to an
	// inherited value. A node carrying an objectOverride is exempt: its seed
	// IS its own designated value (the top-level `schema` node is seeded with
	// the instance's data AND overridden), not an inherited sibling value.
	for id := range rt.byID {
		if _, seeded := rt.scope[id]; !seeded {
			continue
		}
		if _, overridden := rt.nodeObjectOverrides[id]; overridden {
			continue
		}
		delete(rt.scope, id)
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
	// node can resolve before its target publishes. Seed each SINGLETON soft
	// target with an empty object so an optional access (id.?field / id[?"k"])
	// yields optional.none() — the field is omitted — rather than a "no such
	// attribute" error that would data-pend the node. Once the target applies,
	// publishScope overwrites the seed with the live value.
	//
	// A COLLECTION soft target is deliberately left UNBOUND, not seeded with an
	// empty list. An empty-list seed reads as a concrete, resolved-empty
	// collection: size(coll) evaluates to 0 and filter/map yield [], so a status
	// or includeWhen/readyWhen expression computes a false, definite result
	// (e.g. workerCount: "0") for a collection whose source hasn't resolved yet —
	// even when existing children are still retained. Leaving the name unbound
	// makes any reference (size/filter/map/id.field) surface a "no such
	// attribute" error that IsCELDataPending classifies as data-pending, so the
	// dependent node and its status defer until the collection genuinely
	// resolves. This matches v0.9.3, which left the field unset rather than
	// reporting a concrete empty value. Once the target applies, publishScope
	// binds the live collection.
	for _, n := range rt.nodes {
		for _, softID := range n.spec.SoftDepIDs() {
			if _, seeded := rt.scope[softID]; seeded {
				continue
			}
			if dep, ok := rt.byID[softID]; ok && dep.IsCollection() {
				// Unresolved collection soft-dep: leave unbound so references
				// data-pend rather than reading as a concrete empty collection.
				continue
			}
			rt.scope[softID] = map[string]any{}
		}
	}
	// Compute one-based reverse-topological apply orders. Nodes are
	// already in topological order (dependencies precede dependents), so a
	// single forward pass yields order(n) = 1 + max(order(deps)); leaves = 1.
	// Def nodes (e.g. the synthetic `schema`/instance node) are excluded from
	// the depth calculation and never stamped: apply orders cover only real
	// resource nodes.
	//
	// These absolute layer values are REVISION-LOCAL, not a stable identifier:
	// they depend on the graph's shape this revision, so an equivalent resource
	// can carry a different number across revisions (e.g. after an unrelated node
	// is added/removed). Only the RELATIVE ordering within one apply is
	// contractual (a dependent is always a higher wave than its dependency); the
	// number itself is advisory. The deletion path treats the annotation
	// accordingly — it groups by descending wave and tolerates mixed/absent
	// values (unparseable or non-positive fall back to wave 0), so objects
	// stamped by different revisions still delete safely (see
	// controller/instance/deletion.go highestDeletionWave).
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
	if !isTemplateOrRef || sc == nil || val == nil {
		return val
	}
	switch v := val.(type) {
	case *unstructured.Unstructured:
		if v == nil {
			return nil
		}
		return celunstructured.UnstructuredToVal(v.Object, &openapi.Schema{Schema: sc})
	case unstructured.Unstructured:
		return celunstructured.UnstructuredToVal(v.Object, &openapi.Schema{Schema: sc})
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
			} else if u, ok := item.(*unstructured.Unstructured); ok && u != nil {
				list[i] = celunstructured.UnstructuredToVal(u.Object, &openapi.Schema{Schema: itemSchema})
			} else {
				list[i] = item
			}
		}
		return list
	default:
		return val
	}
}
