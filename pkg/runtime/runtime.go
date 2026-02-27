// Copyright 2025 The Kubernetes Authors.
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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	krocel "github.com/kubernetes-sigs/kro/pkg/cel"
	"github.com/kubernetes-sigs/kro/pkg/graph"
	"github.com/kubernetes-sigs/kro/pkg/graph/variable"
)

// Compile-time check: Runtime must implement Interface.
var _ Interface = (*Runtime)(nil)

// Interface defines the minimal runtime operations needed by the controller.
type Interface interface {
	// Nodes returns nodes in topological order (instance excluded).
	Nodes() []*Node

	// Instance returns the instance node.
	Instance() *Node
}

// Runtime is the execution context for a single reconciliation.
// It holds nodes in topological order and provides access to the instance node.
// Expression deduplication is done during FromGraph construction via a local cache.
type Runtime struct {
	order     []string
	nodes     map[string]*Node
	instance  *Node
	rgdConfig graph.RGDConfig
}

// FromGraph creates a new Runtime from a Graph and instance.
// This is called at the start of each reconciliation.
func FromGraph(g *graph.Graph, instance *unstructured.Unstructured, rgdConfig graph.RGDConfig) (*Runtime, error) {
	instanceObj := instance.DeepCopy()

	rt := &Runtime{
		order:     g.TopologicalOrder,
		nodes:     make(map[string]*Node),
		rgdConfig: rgdConfig,
	}

	// Expression cache for non-iteration expressions only.
	// Iteration expressions are not cached because they're evaluated per-item
	// with different iterator bindings each time. Cache key is the original string.
	expressionsCache := make(map[string]*expressionEvaluationState)

	// Helper to get or create expression state. Only caches non-iteration expressions.
	// The Expression contains the pre-compiled Program from build time.
	getOrCreateExpr := func(expr *krocel.Expression, kind variable.ResourceVariableKind, deps []string) *expressionEvaluationState {
		// Don't cache iteration expressions - they need fresh evaluation per iteration.
		if kind.IsIteration() {
			return &expressionEvaluationState{
				Expression:   expr,
				Dependencies: deps,
				Kind:         kind,
			}
		}
		if cached, ok := expressionsCache[expr.Original]; ok {
			return cached
		}
		state := &expressionEvaluationState{
			Expression:   expr,
			Dependencies: deps,
			Kind:         kind,
		}
		expressionsCache[expr.Original] = state
		return state
	}

	// Phase 1: Create all nodes first (without deps wired).
	for _, id := range rt.order {
		rt.nodes[id] = &Node{
			Spec:      g.Nodes[id].DeepCopy(),
			deps:      make(map[string]*Node),
			rgdConfig: rgdConfig,
		}
	}

	// Create instance node.
	instNode := &Node{
		Spec:      g.Instance.DeepCopy(),
		deps:      make(map[string]*Node),
		rgdConfig: rgdConfig,
	}
	instNode.SetObserved([]*unstructured.Unstructured{instanceObj})
	rt.instance = instNode

	// Phase 2: Wire up dependencies for each node.
	// Inject instance node as "schema" dep for static expression evaluation.
	for _, id := range rt.order {
		node := rt.nodes[id]
		node.deps[graph.InstanceNodeID] = instNode
		for _, depID := range node.Spec.Meta.Dependencies {
			if dep, ok := rt.nodes[depID]; ok {
				node.deps[depID] = dep
			}
		}
	}

	// Wire up instance node dependencies.
	for _, depID := range instNode.Spec.Meta.Dependencies {
		if dep, ok := rt.nodes[depID]; ok {
			instNode.deps[depID] = dep
		}
	}

	// Phase 3: Wire up expressions for all nodes.
	for _, id := range rt.order {
		node := rt.nodes[id]

		for _, expr := range node.Spec.IncludeWhen {
			state := getOrCreateExpr(expr, variable.ResourceVariableKindIncludeWhen, nil)
			node.includeWhenExprs = append(node.includeWhenExprs, state)
		}

		for _, expr := range node.Spec.ReadyWhen {
			state := getOrCreateExpr(expr, variable.ResourceVariableKindReadyWhen, []string{id})
			node.readyWhenExprs = append(node.readyWhenExprs, state)
		}

		for _, dim := range node.Spec.ForEach {
			state := getOrCreateExpr(dim.Expression, variable.ResourceVariableKindIteration, node.Spec.Meta.Dependencies)
			node.forEachExprs = append(node.forEachExprs, state)
		}

		for _, v := range node.Spec.Variables {
			node.templateVars = append(node.templateVars, v)
			for _, expr := range v.Expressions {
				state := getOrCreateExpr(expr, v.Kind, expr.References)
				node.templateExprs = append(node.templateExprs, state)
			}
		}
	}

	// Instance status variables (if any) use the same cache.
	for _, v := range instNode.Spec.Variables {
		instNode.templateVars = append(instNode.templateVars, v)
		for _, expr := range v.Expressions {
			state := getOrCreateExpr(expr, v.Kind, expr.References)
			instNode.templateExprs = append(instNode.templateExprs, state)
		}
	}

	return rt, nil
}

// Nodes returns nodes in topological order (instance excluded).
func (r *Runtime) Nodes() []*Node {
	result := make([]*Node, 0, len(r.order))
	for _, id := range r.order {
		result = append(result, r.nodes[id])
	}
	return result
}

// Instance returns the instance node.
func (r *Runtime) Instance() *Node {
	return r.instance
}
