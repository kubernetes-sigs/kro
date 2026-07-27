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
	expv1alpha1 "sigs.k8s.io/krocodile/api/v1alpha1"
	"sigs.k8s.io/krocodile/pkg/compiler"
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

	// maxCollectionSize caps forEach expansion for this Runtime. Per-
	// Runtime rather than package-global so concurrent reconciles with
	// different intended caps don't race. Zero disables the cap (with
	// the overflow safeguard in cartesianProduct still active).
	maxCollectionSize int
}

// Option configures a Runtime at construction time.
type Option func(*Runtime)

// WithMaxCollectionSize overrides the default forEach expansion cap.
// Use 0 to disable the cap entirely.
func WithMaxCollectionSize(n int) Option {
	return func(r *Runtime) { r.maxCollectionSize = n }
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
	// Phase 1: build the node wrappers in topological order so callers
	// see them in apply order.
	rt.nodes = make([]*Node, 0, len(prog.TopologicalOrder))
	for _, id := range prog.TopologicalOrder {
		n := &Node{spec: prog.Nodes[id], rt: rt, deps: map[string]*Node{}}
		rt.nodes = append(rt.nodes, n)
		rt.byID[id] = n
	}
	// Phase 2: wire dependency pointers. Each Node carries pointers to
	// the Node objects it depends on (not just IDs) so IsIgnored /
	// CheckReadiness can recurse without a runtime lookup loop.
	for _, n := range rt.nodes {
		for _, depID := range n.spec.Dependencies {
			if dep, ok := rt.byID[depID]; ok {
				n.deps[depID] = dep
			}
		}
	}
	return rt
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
// it via CEL expressions like ${id.field}. The id is not validated — the
// only legitimate caller is the executor, which iterates node IDs from
// Nodes() and writes them straight back.
func (r *Runtime) Set(id string, value any) {
	r.scope[id] = value
}
