package graphcontroller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ellistarn/kro/experimental/controller/compiler"
	dagpkg "github.com/ellistarn/kro/experimental/controller/dag"
	"github.com/ellistarn/kro/experimental/controller/graph"
)

// ---------------------------------------------------------------------------
// Deferred expression analysis tests
// ---------------------------------------------------------------------------

func TestDeferredExpressionAnalysis(t *testing.T) {
	t.Run("valid deferred expression in Graph CR template", func(t *testing.T) {
		// A forEach node producing a Graph CR with $${...} expressions
		// referencing child node IDs. Should compile successfully.
		spec := &graph.GraphSpec{Nodes: []graph.Node{
			watchNode("items", map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
			}),
			node(graph.Node{
				ID:      "children",
				ForEach: &graph.ForEachBinding{VarName: "item", Expr: "${items}"},
				Template: map[string]any{
					"apiVersion": "experimental.kro.run/v1alpha1",
					"kind":       "Graph",
					"metadata":   map[string]any{"name": "child-${item.metadata.name}"},
					"spec": map[string]any{
						"nodes": []any{
							map[string]any{
								"id": "schema",
								"ref": map[string]any{
									"apiVersion": "v1",
									"kind":       "ConfigMap",
									"metadata":   map[string]any{"name": "$${schema.metadata.name}"},
								},
							},
							map[string]any{
								"id": "deploy",
								"template": map[string]any{
									"apiVersion": "apps/v1",
									"kind":       "Deployment",
									"metadata":   map[string]any{"name": "$${schema.spec.name}"},
								},
							},
						},
					},
				},
			}, graph.NodeTypeTemplate),
		}}
		_, err := compiler.CompileGraphSpec(spec, nil)
		require.NoError(t, err)
	})

	t.Run("syntax error in deferred expression", func(t *testing.T) {
		// A $${...} expression with invalid CEL syntax should fail
		// at the parent's compile time.
		spec := &graph.GraphSpec{Nodes: []graph.Node{
			watchNode("items", map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
			}),
			node(graph.Node{
				ID:      "children",
				ForEach: &graph.ForEachBinding{VarName: "item", Expr: "${items}"},
				Template: map[string]any{
					"apiVersion": "experimental.kro.run/v1alpha1",
					"kind":       "Graph",
					"metadata":   map[string]any{"name": "child-${item.metadata.name}"},
					"spec": map[string]any{
						"nodes": []any{
							map[string]any{
								"id": "schema",
								"ref": map[string]any{
									"apiVersion": "v1",
									"kind":       "ConfigMap",
									"metadata":   map[string]any{"name": "$${+++ bad syntax}"},
								},
							},
						},
					},
				},
			}, graph.NodeTypeTemplate),
		}}
		_, err := compiler.CompileGraphSpec(spec, nil)
		require.Error(t, err)
		assert.ErrorIs(t, err, compiler.ErrInvalidExpression)
		assert.Contains(t, err.Error(), "deferred depth 1")
		assert.Contains(t, err.Error(), "children")
	})

	t.Run("undeclared reference in deferred expression with known scope", func(t *testing.T) {
		// A $${...} expression referencing a node ID that doesn't exist
		// in the child Graph's scope should fail when the scope is known.
		spec := &graph.GraphSpec{Nodes: []graph.Node{
			watchNode("items", map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
			}),
			node(graph.Node{
				ID:      "children",
				ForEach: &graph.ForEachBinding{VarName: "item", Expr: "${items}"},
				Template: map[string]any{
					"apiVersion": "experimental.kro.run/v1alpha1",
					"kind":       "Graph",
					"metadata":   map[string]any{"name": "child-${item.metadata.name}"},
					"spec": map[string]any{
						"nodes": []any{
							map[string]any{
								"id": "schema",
								"ref": map[string]any{
									"apiVersion": "v1",
									"kind":       "ConfigMap",
									"metadata":   map[string]any{"name": "$${nonexistent.metadata.name}"},
								},
							},
						},
					},
				},
			}, graph.NodeTypeTemplate),
		}}
		_, err := compiler.CompileGraphSpec(spec, nil)
		require.Error(t, err)
		assert.ErrorIs(t, err, compiler.ErrInvalidExpression)
		assert.Contains(t, err.Error(), "nonexistent")
	})

	t.Run("deferred forEach variable in scope", func(t *testing.T) {
		// A child Graph's forEach variable should be in scope for
		// deferred expressions.
		spec := &graph.GraphSpec{Nodes: []graph.Node{
			watchNode("items", map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
			}),
			node(graph.Node{
				ID:      "children",
				ForEach: &graph.ForEachBinding{VarName: "item", Expr: "${items}"},
				Template: map[string]any{
					"apiVersion": "experimental.kro.run/v1alpha1",
					"kind":       "Graph",
					"metadata":   map[string]any{"name": "child-${item.metadata.name}"},
					"spec": map[string]any{
						"nodes": []any{
							map[string]any{
								"id": "source",
								"watch": map[string]any{
									"apiVersion": "v1",
									"kind":       "ConfigMap",
								},
							},
							map[string]any{
								"id":      "workers",
								"forEach": []any{map[string]any{"w": "$${source}"}},
								"template": map[string]any{
									"apiVersion": "v1",
									"kind":       "ConfigMap",
									"metadata":   map[string]any{"name": "$${w.metadata.name}"},
								},
							},
						},
					},
				},
			}, graph.NodeTypeTemplate),
		}}
		_, err := compiler.CompileGraphSpec(spec, nil)
		require.NoError(t, err)
	})

	t.Run("non-Graph-CR template with deferred expressions skips scope check", func(t *testing.T) {
		// $${...} in a non-Graph-CR template has no extractable child
		// scope. Validation uses an empty scope — only custom functions
		// are available. An expression using plural() should pass.
		spec := &graph.GraphSpec{Nodes: []graph.Node{
			defNode("data", map[string]any{"kind": "Widget"}),
			node(graph.Node{
				ID: "cm",
				Template: map[string]any{
					"apiVersion": "v1",
					"kind":       "ConfigMap",
					"data":       map[string]any{"plural": "$${plural('Widget')}"},
				},
			}, graph.NodeTypeTemplate),
		}}
		_, err := compiler.CompileGraphSpec(spec, nil)
		require.NoError(t, err)
	})

	t.Run("deferred expression with custom function plural", func(t *testing.T) {
		// $${plural(k.spec.kind)} should compile — k is a child node ID,
		// plural() is a custom function available in the child scope.
		spec := &graph.GraphSpec{Nodes: []graph.Node{
			watchNode("items", map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
			}),
			node(graph.Node{
				ID:      "children",
				ForEach: &graph.ForEachBinding{VarName: "item", Expr: "${items}"},
				Template: map[string]any{
					"apiVersion": "experimental.kro.run/v1alpha1",
					"kind":       "Graph",
					"metadata":   map[string]any{"name": "child-${item.metadata.name}"},
					"spec": map[string]any{
						"nodes": []any{
							map[string]any{
								"id": "k",
								"ref": map[string]any{
									"apiVersion": "v1",
									"kind":       "ConfigMap",
									"metadata":   map[string]any{"name": "test"},
								},
							},
							map[string]any{
								"id": "crd",
								"template": map[string]any{
									"apiVersion": "v1",
									"kind":       "ConfigMap",
									"metadata":   map[string]any{"name": "$${plural(k.spec.kind).lowerAscii()}"},
								},
							},
						},
					},
				},
			}, graph.NodeTypeTemplate),
		}}
		_, err := compiler.CompileGraphSpec(spec, nil)
		require.NoError(t, err)
	})
}

// ---------------------------------------------------------------------------
// Pre-compilation tests
// ---------------------------------------------------------------------------

func TestPreCompileChildGraph(t *testing.T) {
	t.Run("child Graph spec with valid expressions compiles", func(t *testing.T) {
		// A parent template producing a child Graph where the child's
		// expressions are valid after dollar stripping.
		spec := &graph.GraphSpec{Nodes: []graph.Node{
			watchNode("items", map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
			}),
			node(graph.Node{
				ID:      "children",
				ForEach: &graph.ForEachBinding{VarName: "item", Expr: "${items}"},
				Template: map[string]any{
					"apiVersion": "experimental.kro.run/v1alpha1",
					"kind":       "Graph",
					"metadata":   map[string]any{"name": "child-${item.metadata.name}"},
					"spec": map[string]any{
						"nodes": []any{
							map[string]any{
								"id": "schema",
								"ref": map[string]any{
									"apiVersion": "v1",
									"kind":       "ConfigMap",
									"metadata":   map[string]any{"name": "$${schema.metadata.name}"},
								},
							},
							map[string]any{
								"id": "deploy",
								"template": map[string]any{
									"apiVersion": "apps/v1",
									"kind":       "Deployment",
									"metadata":   map[string]any{"name": "$${schema.spec.appName}"},
								},
								"readyWhen": []any{"$${deploy.status.ready == true}"},
							},
						},
					},
				},
			}, graph.NodeTypeTemplate),
		}}
		_, err := compiler.CompileGraphSpec(spec, nil)
		require.NoError(t, err)
	})

	t.Run("child Graph with DAG cycle is rejected at parent compile time", func(t *testing.T) {
		// A child Graph where node A depends on B and B depends on A.
		// The cycle should be caught during parent compilation.
		spec := &graph.GraphSpec{Nodes: []graph.Node{
			watchNode("items", map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
			}),
			node(graph.Node{
				ID:      "children",
				ForEach: &graph.ForEachBinding{VarName: "item", Expr: "${items}"},
				Template: map[string]any{
					"apiVersion": "experimental.kro.run/v1alpha1",
					"kind":       "Graph",
					"metadata":   map[string]any{"name": "child-${item.metadata.name}"},
					"spec": map[string]any{
						"nodes": []any{
							map[string]any{
								"id": "a",
								"template": map[string]any{
									"apiVersion": "v1",
									"kind":       "ConfigMap",
									"data":       map[string]any{"val": "$${b.spec.value}"},
								},
							},
							map[string]any{
								"id": "b",
								"template": map[string]any{
									"apiVersion": "v1",
									"kind":       "ConfigMap",
									"data":       map[string]any{"val": "$${a.spec.value}"},
								},
							},
						},
					},
				},
			}, graph.NodeTypeTemplate),
		}}
		_, err := compiler.CompileGraphSpec(spec, nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "child graph")
		assert.Contains(t, err.Error(), "cycle")
	})

	t.Run("child Graph with invalid expression in child body", func(t *testing.T) {
		// A child node's template has a parse error that should be caught
		// during compilation (deferred analysis catches it first since
		// it runs before pre-compilation).
		spec := &graph.GraphSpec{Nodes: []graph.Node{
			watchNode("items", map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
			}),
			node(graph.Node{
				ID:      "children",
				ForEach: &graph.ForEachBinding{VarName: "item", Expr: "${items}"},
				Template: map[string]any{
					"apiVersion": "experimental.kro.run/v1alpha1",
					"kind":       "Graph",
					"metadata":   map[string]any{"name": "child-${item.metadata.name}"},
					"spec": map[string]any{
						"nodes": []any{
							map[string]any{
								"id": "broken",
								"template": map[string]any{
									"apiVersion": "v1",
									"kind":       "ConfigMap",
									"data":       map[string]any{"val": "$${!!! parse error}"},
								},
							},
						},
					},
				},
			}, graph.NodeTypeTemplate),
		}}
		_, err := compiler.CompileGraphSpec(spec, nil)
		require.Error(t, err)
		assert.ErrorIs(t, err, compiler.ErrInvalidExpression)
		assert.Contains(t, err.Error(), "children") // parent node attribution
	})
}

// ---------------------------------------------------------------------------
// Dollar-stripping tests
// ---------------------------------------------------------------------------

func TestStripDeferralLevel(t *testing.T) {
	t.Run("strips one dollar from deferred", func(t *testing.T) {
		got := compiler.StripDeferralLevel("$${expr}")
		assert.Equal(t, "${expr}", got)
	})

	t.Run("preserves single dollar", func(t *testing.T) {
		got := compiler.StripDeferralLevel("${expr}")
		assert.Equal(t, "__kro_parent_expr__", got)
	})

	t.Run("strips one dollar from triple", func(t *testing.T) {
		got := compiler.StripDeferralLevel("$$${expr}")
		assert.Equal(t, "$${expr}", got)
	})

	t.Run("handles embedded expressions", func(t *testing.T) {
		got := compiler.StripDeferralLevel("prefix-$${a}-$${b}-suffix")
		assert.Equal(t, "prefix-${a}-${b}-suffix", got)
	})

	t.Run("handles mixed depth", func(t *testing.T) {
		got := compiler.StripDeferralLevel("${parent}-$${child}")
		assert.Equal(t, "__kro_parent_expr__-${child}", got)
	})

	t.Run("recurses into maps", func(t *testing.T) {
		input := map[string]any{
			"name": "$${k.spec.kind}",
			"nested": map[string]any{
				"value": "$${k.spec.group}",
			},
		}
		got := compiler.StripDeferralLevel(input).(map[string]any)
		assert.Equal(t, "${k.spec.kind}", got["name"])
		nested := got["nested"].(map[string]any)
		assert.Equal(t, "${k.spec.group}", nested["value"])
	})

	t.Run("recurses into lists", func(t *testing.T) {
		input := []any{"$${a}", "$${b}"}
		got := compiler.StripDeferralLevel(input).([]any)
		assert.Equal(t, "${a}", got[0])
		assert.Equal(t, "${b}", got[1])
	})

	t.Run("preserves non-string values", func(t *testing.T) {
		input := map[string]any{
			"count": int64(42),
			"flag":  true,
		}
		got := compiler.StripDeferralLevel(input).(map[string]any)
		assert.Equal(t, int64(42), got["count"])
		assert.Equal(t, true, got["flag"])
	})
}

// ---------------------------------------------------------------------------
// Child scope extraction tests
// ---------------------------------------------------------------------------

func TestExtractChildScope(t *testing.T) {
	t.Run("extracts node IDs from Graph CR template", func(t *testing.T) {
		body := map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{"id": "schema", "ref": map[string]any{}},
					map[string]any{"id": "deploy", "template": map[string]any{}},
					map[string]any{"id": "svc", "template": map[string]any{}},
				},
			},
		}
		scope := compiler.ExtractChildScopeFromBody(body)
		assert.Equal(t, []string{"schema", "deploy", "svc"}, scope.NodeIDs)
	})

	t.Run("extracts forEach variables from child nodes", func(t *testing.T) {
		body := map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{"id": "source", "watch": map[string]any{}},
					map[string]any{
						"id":       "workers",
						"forEach":  []any{map[string]any{"w": "$${source}"}},
						"template": map[string]any{},
					},
				},
			},
		}
		scope := compiler.ExtractChildScopeFromBody(body)
		assert.Equal(t, []string{"source", "workers"}, scope.NodeIDs)
		assert.Equal(t, []string{"w"}, scope.ForEachVars)
	})

	t.Run("returns empty scope for non-Graph-CR", func(t *testing.T) {
		body := map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
		}
		scope := compiler.ExtractChildScopeFromBody(body)
		assert.Empty(t, scope.NodeIDs)
		assert.Empty(t, scope.ForEachVars)
	})

	t.Run("returns empty scope when spec.nodes is an expression", func(t *testing.T) {
		body := map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"spec": map[string]any{
				"nodes": "$${[...] + k.spec.nodes}",
			},
		}
		scope := compiler.ExtractChildScopeFromBody(body)
		assert.Empty(t, scope.NodeIDs)
	})

	t.Run("returns empty scope for expression-valued apiVersion", func(t *testing.T) {
		body := map[string]any{
			"apiVersion": "${spec.apiVersion}",
			"kind":       "Graph",
		}
		scope := compiler.ExtractChildScopeFromBody(body)
		assert.Empty(t, scope.NodeIDs)
	})
}

// ---------------------------------------------------------------------------
// Compilation key tests
// ---------------------------------------------------------------------------

func TestCompilationKey(t *testing.T) {
	t.Run("same expressions different concrete values produce same key", func(t *testing.T) {
		spec1 := &graph.GraphSpec{Nodes: []graph.Node{
			node(graph.Node{ID: "schema", Ref: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]any{"name": "app-us-west-2"},
			}}, graph.NodeTypeRef),
			node(graph.Node{ID: "deploy", Template: map[string]any{
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
				"metadata":   map[string]any{"name": "${schema.spec.name}-deploy"},
			}}, graph.NodeTypeTemplate),
		}}
		spec2 := &graph.GraphSpec{Nodes: []graph.Node{
			node(graph.Node{ID: "schema", Ref: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]any{"name": "app-eu-west-1"},
			}}, graph.NodeTypeRef),
			node(graph.Node{ID: "deploy", Template: map[string]any{
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
				"metadata":   map[string]any{"name": "${schema.spec.name}-deploy"},
			}}, graph.NodeTypeTemplate),
		}}
		key1 := spec1.CompilationKey()
		key2 := spec2.CompilationKey()
		assert.Equal(t, key1, key2, "specs with same structure but different concrete values should have same compilation key")
		// But the full content hash should differ.
		assert.NotEqual(t, spec1.Hash(), spec2.Hash(), "content hashes should differ")
	})

	t.Run("different expressions produce different keys", func(t *testing.T) {
		spec1 := &graph.GraphSpec{Nodes: []graph.Node{
			node(graph.Node{ID: "a", Template: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"data":       map[string]any{"name": "${a.spec.name}"},
			}}, graph.NodeTypeTemplate),
		}}
		spec2 := &graph.GraphSpec{Nodes: []graph.Node{
			node(graph.Node{ID: "a", Template: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"data":       map[string]any{"name": "${a.spec.other}"},
			}}, graph.NodeTypeTemplate),
		}}
		assert.NotEqual(t, spec1.CompilationKey(), spec2.CompilationKey())
	})

	t.Run("different node IDs produce different keys", func(t *testing.T) {
		spec1 := &graph.GraphSpec{Nodes: []graph.Node{
			node(graph.Node{ID: "alpha", Template: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
			}}, graph.NodeTypeTemplate),
		}}
		spec2 := &graph.GraphSpec{Nodes: []graph.Node{
			node(graph.Node{ID: "beta", Template: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
			}}, graph.NodeTypeTemplate),
		}}
		assert.NotEqual(t, spec1.CompilationKey(), spec2.CompilationKey())
	})

	t.Run("different conditions produce different keys", func(t *testing.T) {
		spec1 := &graph.GraphSpec{Nodes: []graph.Node{
			node(graph.Node{ID: "a", Template: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
			}, ReadyWhen: []string{"${a.status.ready}"}}, graph.NodeTypeTemplate),
		}}
		spec2 := &graph.GraphSpec{Nodes: []graph.Node{
			node(graph.Node{ID: "a", Template: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
			}, ReadyWhen: []string{"${a.status.available}"}}, graph.NodeTypeTemplate),
		}}
		assert.NotEqual(t, spec1.CompilationKey(), spec2.CompilationKey())
	})
}

// ---------------------------------------------------------------------------
// dagpkg.AssembleDAG isolation tests
// ---------------------------------------------------------------------------

func TestAssembleDAG_IsolationBetweenInstances(t *testing.T) {
	// Two DAGs assembled from the same topology must not interfere.
	// Mutating per-node data (Dependencies, SelfPaths) on one instance
	// must not affect the other or the shared topology.
	spec := &graph.GraphSpec{Nodes: []graph.Node{
		templateNode("a", map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"data":       map[string]any{"ref": "${b.spec.value}"},
		}),
		templateNode("b", map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
		}),
	}}
	compiled, err := compiler.CompileGraphSpec(spec, nil)
	require.NoError(t, err)

	// Two instances with different concrete values but same structure.
	nodes1 := []graph.Node{
		templateNode("a", map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"data": map[string]any{"ref": "${b.spec.value}", "name": "instance-1"},
		}),
		templateNode("b", map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "b-instance-1"},
		}),
	}
	nodes2 := []graph.Node{
		templateNode("a", map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"data": map[string]any{"ref": "${b.spec.value}", "name": "instance-2"},
		}),
		templateNode("b", map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "b-instance-2"},
		}),
	}

	dag1 := dagpkg.AssembleDAG(nodes1, compiled.Topology)
	dag2 := dagpkg.AssembleDAG(nodes2, compiled.Topology)

	// Verify both DAGs have the same topology.
	assert.Equal(t, dag1.TopologicalOrder, dag2.TopologicalOrder)
	assert.Equal(t, dag1.Index, dag2.Index)

	// Verify different concrete values.
	assert.NotEqual(t, dag1.Nodes[0].Template["data"].(map[string]any)["name"],
		dag2.Nodes[0].Template["data"].(map[string]any)["name"])

	// Mutate instance 1's per-node data — must not affect instance 2.
	dag1.Nodes[0].Dependencies["injected"] = graph.DepHard
	dag1.Nodes[1].SelfPaths = append(dag1.Nodes[1].SelfPaths, graph.FieldPath{"injected"})

	_, injectedExists := dag2.Nodes[0].Dependencies["injected"]
	assert.False(t, injectedExists,
		"mutation of dag1 Dependencies must not leak to dag2")
	assert.NotContains(t, dag2.Nodes[1].SelfPaths, graph.FieldPath{"injected"},
		"mutation of dag1 SelfPaths must not leak to dag2")

	// Verify the shared topology is also unaffected.
	_, topoInjected := compiled.Topology.NodeDeps(0)["injected"]
	assert.False(t, topoInjected,
		"mutation of dag1 must not corrupt shared topology nodeDeps")
}
