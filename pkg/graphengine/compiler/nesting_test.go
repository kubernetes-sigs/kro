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

package compiler

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	k8sschema "k8s.io/apimachinery/pkg/runtime/schema"

	expv1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/testutil/generator"
)

// TestCompile_Nesting covers nested-graph compilation: the subgraph compiles
// into its own SubProgram frame, captures of parent nodes become the subgraph
// node's dependencies, shadowing resolves to the nearest frame, and the
// no-mix rule rejects expressions that span two scopes.
func TestCompile_Nesting(t *testing.T) {
	t.Parallel()

	t.Run("subgraph compiles into a SubProgram with its own nodes", func(t *testing.T) {
		t.Parallel()
		child := generator.NewGraph("child",
			generator.WithDef("inner", map[string]any{"k": "v"}),
			generator.WithTemplate("cm", map[string]any{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]any{"name": "${inner.k}"},
			}),
		)
		g := generator.NewGraph("g",
			generator.WithNamespace("default"),
			generator.WithDef("seed", map[string]any{"x": "1"}),
			generator.WithSubgraph("sub", child),
		)
		prog, err := newTestCompiler(t).Compile(g)
		require.NoError(t, err)

		sub := prog.Nodes["sub"]
		require.NotNil(t, sub)
		assert.Equal(t, NodeKindGraph, sub.Kind)
		require.NotNil(t, sub.SubProgram, "subgraph compiles into a child program")
		assert.Contains(t, sub.SubProgram.Nodes, "inner")
		assert.Contains(t, sub.SubProgram.Nodes, "cm")
		// The ConfigMap inside the child contributes to the root's schema deps.
		assert.Contains(t, prog.RequiredGroupKinds, k8sschema.GroupKind{Group: "", Kind: "ConfigMap"})
	})

	t.Run("captured parent node becomes a subgraph dependency", func(t *testing.T) {
		t.Parallel()
		// The child captures the parent's `cfg` def.
		child := generator.NewGraph("child",
			generator.WithTemplate("cm", map[string]any{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]any{"name": "${cfg.name}"},
			}),
		)
		g := generator.NewGraph("g",
			generator.WithNamespace("default"),
			generator.WithDef("cfg", map[string]any{"name": "from-parent"}),
			generator.WithSubgraph("sub", child),
		)
		prog, err := newTestCompiler(t).Compile(g)
		require.NoError(t, err)

		sub := prog.Nodes["sub"]
		require.NotNil(t, sub)
		assert.Contains(t, sub.Dependencies, "cfg", "capture of parent node creates a dependency")
		// cfg is applied before the subgraph runs.
		assert.Less(t, indexOf(prog.TopologicalOrder, "cfg"), indexOf(prog.TopologicalOrder, "sub"))
	})

	t.Run("parent references a child output through the subgraph node ID", func(t *testing.T) {
		t.Parallel()
		child := generator.NewGraph("child",
			generator.WithDef("out", map[string]any{"value": "child-data"}),
		)
		g := generator.NewGraph("g",
			generator.WithNamespace("default"),
			generator.WithSubgraph("sub", child),
			generator.WithTemplate("cm", map[string]any{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]any{"name": "cm"},
				"data":     map[string]any{"v": "${sub.out.value}"},
			}),
		)
		prog, err := newTestCompiler(t).Compile(g)
		require.NoError(t, err)
		assert.Contains(t, prog.Nodes["cm"].Dependencies, "sub")
		assert.Less(t, indexOf(prog.TopologicalOrder, "sub"), indexOf(prog.TopologicalOrder, "cm"))
	})

	t.Run("a child name shadows a parent name of the same id", func(t *testing.T) {
		t.Parallel()
		// Both frames declare `cfg`. The child's expression resolves to the
		// child's cfg (depth 0), so it does NOT capture the parent — no
		// dependency edge from the subgraph to the parent cfg.
		child := generator.NewGraph("child",
			generator.WithDef("cfg", map[string]any{"name": "inner"}),
			generator.WithTemplate("cm", map[string]any{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]any{"name": "${cfg.name}"},
			}),
		)
		g := generator.NewGraph("g",
			generator.WithNamespace("default"),
			generator.WithDef("cfg", map[string]any{"name": "outer"}),
			generator.WithSubgraph("sub", child),
		)
		prog, err := newTestCompiler(t).Compile(g)
		require.NoError(t, err)
		assert.NotContains(t, prog.Nodes["sub"].Dependencies, "cfg",
			"shadowed parent cfg is not captured; the child binds its own")
	})

	t.Run("no-mix: one expression cannot reference both scopes", func(t *testing.T) {
		t.Parallel()
		child := generator.NewGraph("child",
			generator.WithDef("local", map[string]any{"a": "x"}),
			generator.WithTemplate("cm", map[string]any{
				"apiVersion": "v1", "kind": "ConfigMap",
				// mixes child-local `local` with captured parent `cfg`.
				"metadata": map[string]any{"name": "${local.a + cfg.name}"},
			}),
		)
		g := generator.NewGraph("g",
			generator.WithNamespace("default"),
			generator.WithDef("cfg", map[string]any{"name": "p"}),
			generator.WithSubgraph("sub", child),
		)
		_, err := newTestCompiler(t).Compile(g)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "mixes node references from different graph scopes")
	})

	t.Run("deep nesting bubbles a grandparent capture up two frames", func(t *testing.T) {
		t.Parallel()
		grandchild := generator.NewGraph("gc",
			generator.WithTemplate("cm", map[string]any{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]any{"name": "${root.name}"}, // captures grandparent
			}),
		)
		child := generator.NewGraph("child",
			generator.WithSubgraph("mid", grandchild),
		)
		g := generator.NewGraph("g",
			generator.WithNamespace("default"),
			generator.WithDef("root", map[string]any{"name": "deep"}),
			generator.WithSubgraph("outer", child),
		)
		prog, err := newTestCompiler(t).Compile(g)
		require.NoError(t, err)
		// The capture bubbles: the outer subgraph depends on root.
		assert.Contains(t, prog.Nodes["outer"].Dependencies, "root")
	})

	t.Run("unknown identifier in a subgraph is rejected", func(t *testing.T) {
		t.Parallel()
		child := generator.NewGraph("child",
			generator.WithTemplate("cm", map[string]any{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]any{"name": "${nope.field}"},
			}),
		)
		g := generator.NewGraph("g",
			generator.WithNamespace("default"),
			generator.WithDef("seed", map[string]any{"x": "1"}),
			generator.WithSubgraph("sub", child),
		)
		_, err := newTestCompiler(t).Compile(g)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unknown identifier")
	})

	t.Run("forEach on a graph node is rejected", func(t *testing.T) {
		t.Parallel()
		child := generator.NewGraph("child", generator.WithDef("d", map[string]any{"k": "v"}))
		g := generator.NewGraph("g",
			generator.WithNamespace("default"),
			generator.WithDef("seed", map[string]any{"items": []any{"a"}}),
			generator.WithSubgraph("sub", child),
			generator.WithReadyWhen(), // no-op, keep last node = sub
		)
		// Attach a forEach to the subgraph node directly.
		g.Spec.Nodes[len(g.Spec.Nodes)-1].ForEach = []expv1alpha1.ForEachDimension{{"i": "${seed.items}"}}
		_, err := newTestCompiler(t).Compile(g)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "forEach is not supported on graph nodes")
	})

	t.Run("empty subgraph is rejected", func(t *testing.T) {
		t.Parallel()
		g := generator.NewGraph("g",
			generator.WithNamespace("default"),
			generator.WithDef("seed", map[string]any{"x": "1"}),
			generator.WithSubgraph("sub", generator.NewGraph("empty")),
		)
		_, err := newTestCompiler(t).Compile(g)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "at least one node")
	})
}
