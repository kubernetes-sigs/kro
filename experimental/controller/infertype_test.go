package graphcontroller

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/kube-openapi/pkg/validation/spec"

	"github.com/ellistarn/kro/experimental/controller/compiler"
	krocel "github.com/kubernetes-sigs/kro/pkg/cel"

	"github.com/ellistarn/kro/experimental/controller/graph"
)

// templateNode builds a Template-classified Node from a template map. Test-only helper;
// production code sets the classification via parseNodeList.
func templateNode(id string, tmpl map[string]any) graph.Node {
	return node(graph.Node{ID: id, Template: tmpl}, graph.NodeTypeTemplate)
}

// defNode builds a Definition-classified Node from a map of values.
func defNode(id string, body map[string]any) graph.Node {
	return node(graph.Node{ID: id, Def: body}, graph.NodeTypeDef)
}

// watchNode builds a Watch-classified Node (collection) from a body.
func watchNode(id string, body map[string]any) graph.Node {
	return node(graph.Node{ID: id, Watch: body}, graph.NodeTypeWatch)
}

// ---------------------------------------------------------------------------
// inferFieldType unit tests
// ---------------------------------------------------------------------------

func TestInferFieldType(t *testing.T) {
	tests := []struct {
		name     string
		value    any
		wantType string // CelType().String()
	}{
		// Literal strings
		{name: "literal string", value: "hello", wantType: "string"},
		{name: "empty string", value: "", wantType: "string"},

		// Expression strings
		{name: "standalone expression", value: "${spec.name}", wantType: "dyn"},
		{name: "embedded expression", value: "prefix-${spec.name}", wantType: "string"},
		{name: "embedded expression suffix", value: "${spec.name}-suffix", wantType: "string"},
		{name: "multi expression", value: "${a}-${b}", wantType: "string"},
		{name: "deferred expression", value: "$${spec.name}", wantType: "string"},

		// Booleans
		{name: "true", value: true, wantType: "bool"},
		{name: "false", value: false, wantType: "bool"},

		// Numbers
		{name: "integer", value: int64(42), wantType: "int"},
		{name: "go int", value: 42, wantType: "int"},
		{name: "float64 integer", value: float64(42), wantType: "int"},
		{name: "float64 decimal", value: 3.14, wantType: "double"},

		// Nil
		{name: "nil", value: nil, wantType: "dyn"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dt := compiler.InferFieldType("test.field", tt.value)
			assert.Equal(t, tt.wantType, dt.CelType().String())
		})
	}
}

func TestInferObjectType(t *testing.T) {
	t.Run("flat template", func(t *testing.T) {
		tmpl := map[string]any{
			"region": "us-west-2",
			"count":  int64(3),
			"debug":  true,
		}
		dt := compiler.InferObjectType(krocel.TypeNamePrefix+"cfg", tmpl)

		assert.True(t, dt.IsObject())
		assert.Equal(t, krocel.TypeNamePrefix+"cfg", dt.TypeName())

		// Verify fields exist with correct types.
		region, ok := dt.Fields["region"]
		require.True(t, ok, "field 'region' should exist")
		assert.Equal(t, "string", region.Type.CelType().String())

		count, ok := dt.Fields["count"]
		require.True(t, ok, "field 'count' should exist")
		assert.Equal(t, "int", count.Type.CelType().String())

		debug, ok := dt.Fields["debug"]
		require.True(t, ok, "field 'debug' should exist")
		assert.Equal(t, "bool", debug.Type.CelType().String())
	})

	t.Run("nested template", func(t *testing.T) {
		tmpl := map[string]any{
			"prefix": "myapp",
			"metadata": map[string]any{
				"env":    "prod",
				"region": "us-west-2",
			},
		}
		dt := compiler.InferObjectType(krocel.TypeNamePrefix+"naming", tmpl)

		metadata, ok := dt.Fields["metadata"]
		require.True(t, ok)
		assert.True(t, metadata.Type.IsObject())
		// Path-based naming: nested types get unique names.
		assert.Equal(t, krocel.TypeNamePrefix+"naming.metadata", metadata.Type.TypeName())

		env, ok := metadata.Type.Fields["env"]
		require.True(t, ok)
		assert.Equal(t, "string", env.Type.CelType().String())
	})

	t.Run("expression fields are dyn", func(t *testing.T) {
		tmpl := map[string]any{
			"literal": "hello",
			"dynamic": "${spec.name}",
			"mixed":   "prefix-${spec.name}",
		}
		dt := compiler.InferObjectType(krocel.TypeNamePrefix+"test", tmpl)

		literal := dt.Fields["literal"]
		assert.Equal(t, "string", literal.Type.CelType().String())

		dynamic := dt.Fields["dynamic"]
		assert.Equal(t, "dyn", dynamic.Type.CelType().String())

		mixed := dt.Fields["mixed"]
		assert.Equal(t, "string", mixed.Type.CelType().String())
	})

	t.Run("array fields", func(t *testing.T) {
		tmpl := map[string]any{
			"ports":  []any{int64(80), int64(443)},
			"names":  []any{"a", "b"},
			"empty":  []any{},
			"nested": []any{map[string]any{"name": "x"}},
		}
		dt := compiler.InferObjectType(krocel.TypeNamePrefix+"test", tmpl)

		ports := dt.Fields["ports"]
		assert.True(t, ports.Type.IsList())

		names := dt.Fields["names"]
		assert.True(t, names.Type.IsList())

		empty := dt.Fields["empty"]
		assert.True(t, empty.Type.IsList())

		nested := dt.Fields["nested"]
		assert.True(t, nested.Type.IsList())
	})
}

// ---------------------------------------------------------------------------
// resolveNodeTypes unit tests
// ---------------------------------------------------------------------------

func TestResolveNodeTypes(t *testing.T) {
	t.Run("definition nodes are typed", func(t *testing.T) {
		nodes := []graph.Node{
			node(graph.Node{ID: "naming", Def: map[string]any{"prefix": "myapp"}}, graph.NodeTypeDef),
		}
		ts := compiler.ResolveNodeTypes(nodes, nil)

		assert.Len(t, ts.DefinitionTypes, 1)
		assert.Contains(t, ts.DefinitionTypes, "naming")
		assert.Empty(t, ts.UntypedIDs)
	})

	t.Run("resource nodes without resolver are untyped", func(t *testing.T) {
		nodes := []graph.Node{
			node(graph.Node{ID: "deploy", Template: map[string]any{
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
				"metadata":   map[string]any{"name": "test"},
			}}, graph.NodeTypeTemplate),
		}
		ts := compiler.ResolveNodeTypes(nodes, nil)

		assert.Empty(t, ts.DefinitionTypes)
		assert.Contains(t, ts.UntypedIDs, "deploy")
	})

	t.Run("forEach iterator variables are untyped", func(t *testing.T) {
		nodes := []graph.Node{
			node(graph.Node{
				ID:      "items",
				ForEach: &graph.ForEachBinding{VarName: "item", Expr: "${spec.items}"},
				Def:     map[string]any{"name": "${item}"},
			}, graph.NodeTypeDef),
		}
		ts := compiler.ResolveNodeTypes(nodes, nil)

		assert.Contains(t, ts.DefinitionTypes, "items")
		assert.True(t, ts.ForEachDefinitions["items"])
		assert.Contains(t, ts.UntypedIDs, "item")
	})

	t.Run("mixed definitions and resources", func(t *testing.T) {
		nodes := []graph.Node{
			node(graph.Node{ID: "naming", Def: map[string]any{"prefix": "app"}}, graph.NodeTypeDef),
			node(graph.Node{ID: "deploy", Template: map[string]any{
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
				"metadata":   map[string]any{"name": "${naming.prefix}"},
			}}, graph.NodeTypeTemplate),
		}
		ts := compiler.ResolveNodeTypes(nodes, nil)

		assert.Len(t, ts.DefinitionTypes, 1)
		assert.Contains(t, ts.DefinitionTypes, "naming")
		assert.Contains(t, ts.UntypedIDs, "deploy")
	})
}

// ---------------------------------------------------------------------------
// Compile-time validation tests (the key behavioral tests)
// ---------------------------------------------------------------------------

func TestDefinitionFieldValidation(t *testing.T) {
	t.Run("valid field access compiles", func(t *testing.T) {
		spec := &graph.GraphSpec{Nodes: []graph.Node{
			node(graph.Node{ID: "naming", Def: map[string]any{"prefix": "myapp"}}, graph.NodeTypeDef),
			node(graph.Node{ID: "deploy", Template: map[string]any{
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
				"metadata":   map[string]any{"name": "${naming.prefix + '-deploy'}"},
			}}, graph.NodeTypeTemplate),
		}}
		_, err := compiler.CompileGraphSpec(spec, nil)
		require.NoError(t, err)
	})

	t.Run("wrong field on definition fails compilation", func(t *testing.T) {
		spec := &graph.GraphSpec{Nodes: []graph.Node{
			node(graph.Node{ID: "naming", Def: map[string]any{"prefix": "myapp"}}, graph.NodeTypeDef),
			node(graph.Node{ID: "deploy", Template: map[string]any{
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
				"metadata":   map[string]any{"name": "${naming.typo}"},
			}}, graph.NodeTypeTemplate),
		}}
		_, err := compiler.CompileGraphSpec(spec, nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "naming.typo")
	})

	t.Run("nested field access compiles", func(t *testing.T) {
		spec := &graph.GraphSpec{Nodes: []graph.Node{
			node(graph.Node{ID: "cfg", Def: map[string]any{
				"metadata": map[string]any{
					"env":    "prod",
					"region": "us-west-2",
				},
			}}, graph.NodeTypeDef),
			node(graph.Node{ID: "deploy", Template: map[string]any{
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
				"metadata":   map[string]any{"name": "${cfg.metadata.env + '-deploy'}"},
			}}, graph.NodeTypeTemplate),
		}}
		_, err := compiler.CompileGraphSpec(spec, nil)
		require.NoError(t, err)
	})

	t.Run("wrong nested field fails compilation", func(t *testing.T) {
		spec := &graph.GraphSpec{Nodes: []graph.Node{
			node(graph.Node{ID: "cfg", Def: map[string]any{
				"metadata": map[string]any{
					"env": "prod",
				},
			}}, graph.NodeTypeDef),
			node(graph.Node{ID: "deploy", Template: map[string]any{
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
				"metadata":   map[string]any{"name": "${cfg.metadata.typo}"},
			}}, graph.NodeTypeTemplate),
		}}
		_, err := compiler.CompileGraphSpec(spec, nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "typo")
	})

	t.Run("dyn expression fields allow any access", func(t *testing.T) {
		// A definition with an expression-valued field should be dyn,
		// allowing downstream expressions to access any sub-field.
		spec := &graph.GraphSpec{Nodes: []graph.Node{
			node(graph.Node{ID: "upstream", Template: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]any{"name": "cfg"},
			}}, graph.NodeTypeTemplate),
			node(graph.Node{ID: "derived", Def: map[string]any{
				"data": "${upstream.data}",
			}}, graph.NodeTypeDef),
			node(graph.Node{ID: "consumer", Template: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]any{"name": "${derived.data.someKey}"},
			}}, graph.NodeTypeTemplate),
		}}
		_, err := compiler.CompileGraphSpec(spec, nil)
		require.NoError(t, err)
	})

	t.Run("cross-definition validation", func(t *testing.T) {
		// Definition A → Definition B → resource node.
		spec := &graph.GraphSpec{Nodes: []graph.Node{
			node(graph.Node{ID: "a", Def: map[string]any{"prefix": "app"}}, graph.NodeTypeDef),
			node(graph.Node{ID: "b", Def: map[string]any{"full": "${a.prefix + '-svc'}"}}, graph.NodeTypeDef),
			node(graph.Node{ID: "svc", Template: map[string]any{
				"apiVersion": "v1",
				"kind":       "Service",
				"metadata":   map[string]any{"name": "${b.full}"},
			}}, graph.NodeTypeTemplate),
		}}
		_, err := compiler.CompileGraphSpec(spec, nil)
		require.NoError(t, err)
	})

	t.Run("cross-definition wrong field fails", func(t *testing.T) {
		spec := &graph.GraphSpec{Nodes: []graph.Node{
			node(graph.Node{ID: "a", Def: map[string]any{"prefix": "app"}}, graph.NodeTypeDef),
			node(graph.Node{ID: "b", Def: map[string]any{"full": "${a.typo}"}}, graph.NodeTypeDef),
		}}
		_, err := compiler.CompileGraphSpec(spec, nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "a.typo")
	})

	t.Run("type refinement narrows def expression field from dyn to string", func(t *testing.T) {
		// b.full is ${a.prefix + '-svc'} which returns string.
		// After refinement, b.full is typed as string, not dyn.
		// Verify: .startsWith() is a string method, not available on dyn.
		spec := &graph.GraphSpec{Nodes: []graph.Node{
			node(graph.Node{ID: "a", Def: map[string]any{"prefix": "app"}}, graph.NodeTypeDef),
			node(graph.Node{ID: "b", Def: map[string]any{"full": "${a.prefix + '-svc'}"}}, graph.NodeTypeDef),
		}}
		compiled, err := compiler.CompileGraphSpec(spec, nil)
		require.NoError(t, err)

		// The refined environment should type b.full as string.
		parsed, issues := compiled.Env().Parse("b.full.startsWith('app')")
		require.Nil(t, issues.Err())
		ast, issues := compiled.Env().Check(parsed)
		require.Nil(t, issues.Err(), "b.full should be string after refinement, supporting .startsWith()")
		assert.Equal(t, "bool", ast.OutputType().String())
	})

	t.Run("type refinement catches field access error on narrowed string", func(t *testing.T) {
		// b.full is narrowed to string. Accessing .nonExistentField is a
		// type error on string that was invisible against dyn.
		spec := &graph.GraphSpec{Nodes: []graph.Node{
			node(graph.Node{ID: "a", Def: map[string]any{"prefix": "app"}}, graph.NodeTypeDef),
			node(graph.Node{ID: "b", Def: map[string]any{"full": "${a.prefix + '-svc'}"}}, graph.NodeTypeDef),
			node(graph.Node{ID: "svc", Template: map[string]any{
				"apiVersion": "v1",
				"kind":       "Service",
				"metadata":   map[string]any{"name": "${b.full.nonExistentField}"},
			}}, graph.NodeTypeTemplate),
		}}
		_, err := compiler.CompileGraphSpec(spec, nil)
		require.Error(t, err, "b.full is string after narrowing; field access should fail")
		assert.ErrorIs(t, err, compiler.ErrInvalidExpression)
		assert.Contains(t, err.Error(), "type refinement")
	})

	t.Run("type refinement allows valid narrowed access", func(t *testing.T) {
		// b.full is narrowed to string. Using string operations should work.
		spec := &graph.GraphSpec{Nodes: []graph.Node{
			node(graph.Node{ID: "a", Def: map[string]any{"prefix": "app"}}, graph.NodeTypeDef),
			node(graph.Node{ID: "b", Def: map[string]any{"full": "${a.prefix + '-svc'}"}}, graph.NodeTypeDef),
			node(graph.Node{ID: "svc", Template: map[string]any{
				"apiVersion": "v1",
				"kind":       "Service",
				"metadata":   map[string]any{"name": "${b.full.upperAscii()}"},
			}}, graph.NodeTypeTemplate),
		}}
		_, err := compiler.CompileGraphSpec(spec, nil)
		require.NoError(t, err, "b.full is string after narrowing; string methods should work")
	})

	t.Run("readyWhen can access definition fields", func(t *testing.T) {
		spec := &graph.GraphSpec{Nodes: []graph.Node{
			node(graph.Node{
				ID:        "cfg",
				Def:       map[string]any{"count": "3"},
				ReadyWhen: []string{"${cfg.count == '3'}"},
			}, graph.NodeTypeDef),
		}}
		_, err := compiler.CompileGraphSpec(spec, nil)
		require.NoError(t, err)
	})

	t.Run("forEach definition compiles as list type", func(t *testing.T) {
		spec := &graph.GraphSpec{Nodes: []graph.Node{
			node(graph.Node{ID: "upstream", Template: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"data":       map[string]any{"items": "a,b,c"},
			}}, graph.NodeTypeTemplate),
			node(graph.Node{
				ID:      "items",
				ForEach: &graph.ForEachBinding{VarName: "item", Expr: "${upstream.data.items}"},
				Def:     map[string]any{"name": "${item}", "port": int64(80)},
			}, graph.NodeTypeDef),
		}}
		_, err := compiler.CompileGraphSpec(spec, nil)
		require.NoError(t, err)
	})
}

// ---------------------------------------------------------------------------
// forEach return-type validation tests
// ---------------------------------------------------------------------------

func TestForEachReturnTypeValidation(t *testing.T) {
	t.Run("list expression accepted", func(t *testing.T) {
		// A definition node producing a list, referenced by forEach.
		spec := &graph.GraphSpec{Nodes: []graph.Node{
			defNode("data", map[string]any{"items": []any{"a", "b"}}),
			node(graph.Node{
				ID:      "worker",
				ForEach: &graph.ForEachBinding{VarName: "item", Expr: "${data.items}"},
				Def:     map[string]any{"name": "${item}"},
			}, graph.NodeTypeDef),
		}}
		_, err := compiler.CompileGraphSpec(spec, nil)
		require.NoError(t, err)
	})

	t.Run("string expression rejected", func(t *testing.T) {
		// A definition node producing a string — forEach over a string
		// is always wrong.
		spec := &graph.GraphSpec{Nodes: []graph.Node{
			defNode("data", map[string]any{"name": "hello"}),
			node(graph.Node{
				ID:      "worker",
				ForEach: &graph.ForEachBinding{VarName: "item", Expr: "${data.name}"},
				Def:     map[string]any{"v": "${item}"},
			}, graph.NodeTypeDef),
		}}
		_, err := compiler.CompileGraphSpec(spec, nil)
		require.Error(t, err)
		assert.ErrorIs(t, err, compiler.ErrInvalidExpression)
		assert.Contains(t, err.Error(), "must return a list")
		assert.Contains(t, err.Error(), "worker")
	})

	t.Run("dyn expression accepted", func(t *testing.T) {
		// An unresolved template node — typed as dyn. The compiler
		// can't prove it's wrong, so it must accept it.
		spec := &graph.GraphSpec{Nodes: []graph.Node{
			templateNode("source", map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
			}),
			node(graph.Node{
				ID:      "worker",
				ForEach: &graph.ForEachBinding{VarName: "item", Expr: "${source}"},
				Def:     map[string]any{"v": "${item}"},
			}, graph.NodeTypeDef),
		}}
		_, err := compiler.CompileGraphSpec(spec, nil)
		require.NoError(t, err)
	})

	t.Run("interpolated expression rejected", func(t *testing.T) {
		// A forEach expression that's an interpolated string, not a
		// standalone ${...}. Interpolation always produces a string.
		spec := &graph.GraphSpec{Nodes: []graph.Node{
			defNode("data", map[string]any{"items": []any{"a", "b"}}),
			node(graph.Node{
				ID:      "worker",
				ForEach: &graph.ForEachBinding{VarName: "item", Expr: "prefix-${data.items}"},
				Def:     map[string]any{"v": "${item}"},
			}, graph.NodeTypeDef),
		}}
		_, err := compiler.CompileGraphSpec(spec, nil)
		require.Error(t, err)
		assert.ErrorIs(t, err, compiler.ErrInvalidExpression)
		assert.Contains(t, err.Error(), "standalone")
	})

	t.Run("literal string rejected", func(t *testing.T) {
		// A forEach expression with no ${...} at all.
		spec := &graph.GraphSpec{Nodes: []graph.Node{
			node(graph.Node{
				ID:      "worker",
				ForEach: &graph.ForEachBinding{VarName: "item", Expr: "just a string"},
				Def:     map[string]any{"v": "${item}"},
			}, graph.NodeTypeDef),
		}}
		_, err := compiler.CompileGraphSpec(spec, nil)
		require.Error(t, err)
		assert.ErrorIs(t, err, compiler.ErrInvalidExpression)
		assert.Contains(t, err.Error(), "must contain")
	})

	t.Run("deferred expression skipped", func(t *testing.T) {
		// A $${...} forEach is deferred to the child graph. The parent
		// compiler must not reject it.
		spec := &graph.GraphSpec{Nodes: []graph.Node{
			defNode("data", map[string]any{"items": []any{"a", "b"}}),
			node(graph.Node{
				ID:      "worker",
				ForEach: &graph.ForEachBinding{VarName: "item", Expr: "$${data.items}"},
				Def:     map[string]any{"v": "literal"},
			}, graph.NodeTypeDef),
		}}
		_, err := compiler.CompileGraphSpec(spec, nil)
		require.NoError(t, err)
	})

	t.Run("map expression rejected", func(t *testing.T) {
		// A definition producing a map — forEach over a map is wrong.
		spec := &graph.GraphSpec{Nodes: []graph.Node{
			defNode("data", map[string]any{"kv": map[string]any{"a": "1"}}),
			node(graph.Node{
				ID:      "worker",
				ForEach: &graph.ForEachBinding{VarName: "item", Expr: "${data.kv}"},
				Def:     map[string]any{"v": "${item}"},
			}, graph.NodeTypeDef),
		}}
		_, err := compiler.CompileGraphSpec(spec, nil)
		require.Error(t, err)
		assert.ErrorIs(t, err, compiler.ErrInvalidExpression)
		assert.Contains(t, err.Error(), "must return a list")
	})
}

func TestExtractLiteralGVK(t *testing.T) {
	t.Run("literal GVK", func(t *testing.T) {
		gvk := compiler.ExtractLiteralGVK(map[string]any{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
		})
		require.NotNil(t, gvk)
		assert.Equal(t, "apps", gvk.Group)
		assert.Equal(t, "v1", gvk.Version)
		assert.Equal(t, "Deployment", gvk.Kind)
	})

	t.Run("expression apiVersion returns nil", func(t *testing.T) {
		gvk := compiler.ExtractLiteralGVK(map[string]any{
			"apiVersion": "${spec.apiVersion}",
			"kind":       "Deployment",
		})
		assert.Nil(t, gvk)
	})

	t.Run("missing kind returns nil", func(t *testing.T) {
		gvk := compiler.ExtractLiteralGVK(map[string]any{
			"apiVersion": "v1",
		})
		assert.Nil(t, gvk)
	})

	t.Run("nil template returns nil", func(t *testing.T) {
		gvk := compiler.ExtractLiteralGVK(nil)
		assert.Nil(t, gvk)
	})

	t.Run("core type", func(t *testing.T) {
		gvk := compiler.ExtractLiteralGVK(map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
		})
		require.NotNil(t, gvk)
		assert.Equal(t, "", gvk.Group)
		assert.Equal(t, "v1", gvk.Version)
		assert.Equal(t, "ConfigMap", gvk.Kind)
	})
}

// ---------------------------------------------------------------------------
// inferStringType tests
// ---------------------------------------------------------------------------

func TestInferStringType(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"hello", "string"},
		{"", "string"},
		{"${x}", "dyn"},
		{"${a.b.c}", "dyn"},
		{"prefix-${x}", "string"},
		{"${x}-suffix", "string"},
		{"${x}-${y}", "string"},
		{"$${x}", "string"},           // deferred expression
		{"no dollars here", "string"}, // plain text
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			dt := compiler.InferStringType(tt.input)
			assert.Equal(t, tt.want, dt.CelType().String())
		})
	}
}

// ---------------------------------------------------------------------------
// Path-based naming tests (collision prevention)
// ---------------------------------------------------------------------------

func TestPathBasedNamingPreventsCollisions(t *testing.T) {
	// Two definitions with different nested structures should produce
	// distinct types that don't shadow each other.
	ts := compiler.ResolveNodeTypes([]graph.Node{
		node(graph.Node{ID: "alpha", Def: map[string]any{
			"nested": map[string]any{"x": "hello"},
		}}, graph.NodeTypeDef),
		node(graph.Node{ID: "beta", Def: map[string]any{
			"nested": map[string]any{"y": int64(42)},
		}}, graph.NodeTypeDef),
	}, nil)

	alphaDT := ts.DefinitionTypes["alpha"]
	betaDT := ts.DefinitionTypes["beta"]

	// Root types have distinct names.
	assert.NotEqual(t, alphaDT.TypeName(), betaDT.TypeName())

	// Nested types have distinct names.
	alphaNested := alphaDT.Fields["nested"].Type
	betaNested := betaDT.Fields["nested"].Type
	assert.NotEqual(t, alphaNested.TypeName(), betaNested.TypeName())
	assert.Equal(t, krocel.TypeNamePrefix+"alpha.nested", alphaNested.TypeName())
	assert.Equal(t, krocel.TypeNamePrefix+"beta.nested", betaNested.TypeName())

	// Each nested type has its own fields.
	_, hasX := alphaNested.Fields["x"]
	_, hasY := betaNested.Fields["y"]
	assert.True(t, hasX)
	assert.True(t, hasY)
}

// ---------------------------------------------------------------------------
// End-to-end: definition typing doesn't break runtime behavior
// ---------------------------------------------------------------------------

func TestDefinitionTypingRuntimeCompat(t *testing.T) {
	// Verify that typed definitions still work correctly at runtime —
	// the compile-time types gate validation but runtime evaluation is
	// structural (map key lookup).
	r := &GraphReconciler{}
	_ = r // reconcileDefinition doesn't need clusterAccess
	ctx := t.Context()

	spec := &graph.GraphSpec{Nodes: []graph.Node{
		node(graph.Node{ID: "cfg", Def: map[string]any{
			"region": "us-west-2",
			"count":  int64(3),
		}}, graph.NodeTypeDef),
		node(graph.Node{ID: "derived", Def: map[string]any{
			"name": "${cfg.region + '-app'}",
		}}, graph.NodeTypeDef),
	}}

	compiled, err := compiler.CompileGraphSpec(spec, nil)
	require.NoError(t, err)

	eval := &evaluator{compiled: compiled, scope: map[string]any{}}

	// Reconcile first definition.
	err = reconcileDefinition(ctx, spec.Nodes[0], eval)
	require.NoError(t, err)

	cfgResult, ok := eval.scope["cfg"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "us-west-2", cfgResult["region"])
	assert.Equal(t, int64(3), cfgResult["count"])

	// Reconcile second definition that references the first.
	err = reconcileDefinition(ctx, spec.Nodes[1], eval)
	require.NoError(t, err)

	derivedResult, ok := eval.scope["derived"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "us-west-2-app", derivedResult["name"])
}

// ---------------------------------------------------------------------------
// Schema resolution integration test (stub resolver)
// ---------------------------------------------------------------------------

// stubResolver is a test SchemaResolver that returns pre-configured schemas.
type stubResolver struct {
	schemas map[schema.GroupVersionKind]*spec.Schema
}

func (s *stubResolver) ResolveSchema(gvk schema.GroupVersionKind) (*spec.Schema, error) {
	if schema, ok := s.schemas[gvk]; ok {
		return schema, nil
	}
	return nil, fmt.Errorf("schema not found for %v", gvk)
}

func TestSchemaResolutionIntegration(t *testing.T) {
	// Build a minimal OpenAPI schema for a ConfigMap-like resource with
	// data.myKey as a string field.
	cmSchema := &spec.Schema{
		SchemaProps: spec.SchemaProps{
			Type: []string{"object"},
			Properties: map[string]spec.Schema{
				"apiVersion": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
				"kind":       {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
				"metadata":   {SchemaProps: spec.SchemaProps{Type: []string{"object"}}},
				"data": {
					SchemaProps: spec.SchemaProps{
						Type: []string{"object"},
						AdditionalProperties: &spec.SchemaOrBool{
							Allows: true,
							Schema: &spec.Schema{
								SchemaProps: spec.SchemaProps{Type: []string{"string"}},
							},
						},
					},
				},
			},
		},
	}

	resolver := &stubResolver{
		schemas: map[schema.GroupVersionKind]*spec.Schema{
			{Group: "", Version: "v1", Kind: "ConfigMap"}: cmSchema,
		},
	}

	t.Run("resolved schema enables field validation", func(t *testing.T) {
		nodes := []graph.Node{
			node(graph.Node{ID: "cm", Template: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]any{"name": "test"},
				"data":       map[string]any{"key": "value"},
			}}, graph.NodeTypeTemplate),
		}
		ts := compiler.ResolveNodeTypes(nodes, resolver)

		// The ConfigMap node should be resolved via schema.
		assert.Contains(t, ts.ResourceSchemas, "cm")
		assert.Empty(t, ts.UntypedIDs)
	})

	t.Run("mixed schema and definition types compile together", func(t *testing.T) {
		graphSpec := &graph.GraphSpec{Nodes: []graph.Node{
			node(graph.Node{ID: "cm", Template: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]any{"name": "test"},
				"data":       map[string]any{"key": "value"},
			}}, graph.NodeTypeTemplate),
			node(graph.Node{ID: "naming", Def: map[string]any{
				"prefix": "myapp",
			}}, graph.NodeTypeDef),
			node(graph.Node{ID: "consumer", Template: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]any{"name": "${naming.prefix + '-cfg'}"},
				"data":       map[string]any{"ref": "${cm.data}"},
			}}, graph.NodeTypeTemplate),
		}}
		typeInfo := compiler.ResolveNodeTypes(graphSpec.Nodes, resolver)
		_, err := compiler.CompileGraphSpec(graphSpec, typeInfo)
		require.NoError(t, err)
	})

	t.Run("unresolved CRD falls back to dyn and tracks GVK", func(t *testing.T) {
		nodes := []graph.Node{
			node(graph.Node{ID: "custom", Template: map[string]any{
				"apiVersion": "custom.example.com/v1",
				"kind":       "Widget",
				"metadata":   map[string]any{"name": "test"},
			}}, graph.NodeTypeTemplate),
		}
		ts := compiler.ResolveNodeTypes(nodes, resolver)

		// Widget is not in the stub resolver — should fall back to dyn.
		assert.Empty(t, ts.ResourceSchemas)
		assert.Contains(t, ts.UntypedIDs, "custom")

		// The unresolved GVK should be tracked for CRD watch.
		require.Len(t, ts.UnresolvedGVKs, 1)
		assert.Equal(t, "Widget", ts.UnresolvedGVKs[0].Kind)
		assert.Equal(t, "custom.example.com", ts.UnresolvedGVKs[0].Group)
	})
}

// ---------------------------------------------------------------------------
// Recompilation on schema change
// ---------------------------------------------------------------------------

func TestRecompilationOnSchemaChange(t *testing.T) {
	// Simulates: CRD not available → compile with dyn → CRD installed →
	// evict → recompile with schema → type error detected.

	widgetGVK := schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "Widget"}

	emptyResolver := &stubResolver{schemas: map[schema.GroupVersionKind]*spec.Schema{}}

	graphSpec := &graph.GraphSpec{Nodes: []graph.Node{
		node(graph.Node{ID: "widget", Template: map[string]any{
			"apiVersion": "example.com/v1",
			"kind":       "Widget",
			"metadata":   map[string]any{"name": "test"},
		}}, graph.NodeTypeTemplate),
		// Consumer is a definition — no GVK to resolve, so the only
		// unresolved GVK is Widget.
		node(graph.Node{ID: "consumer", Def: map[string]any{
			"name": "${widget.status.typo}",
		}}, graph.NodeTypeDef),
	}}

	// Phase 1: compile with empty resolver — Widget falls back to dyn.
	// "${widget.status.typo}" compiles fine because widget is dyn.
	typeInfo := compiler.ResolveNodeTypes(graphSpec.Nodes, emptyResolver)
	require.Len(t, typeInfo.UnresolvedGVKs, 1)
	assert.Equal(t, widgetGVK, typeInfo.UnresolvedGVKs[0])

	compiled, err := compiler.CompileGraphSpec(graphSpec, typeInfo)
	require.NoError(t, err, "should compile with dyn — no type checking on widget")
	assert.Len(t, compiled.UnresolvedGVKs, 1)

	// Phase 2: CRD is installed — Widget schema now available.
	widgetSchema := &spec.Schema{
		SchemaProps: spec.SchemaProps{
			Type: []string{"object"},
			Properties: map[string]spec.Schema{
				"apiVersion": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
				"kind":       {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
				"metadata":   {SchemaProps: spec.SchemaProps{Type: []string{"object"}}},
				"status": {SchemaProps: spec.SchemaProps{
					Type: []string{"object"},
					Properties: map[string]spec.Schema{
						"ready": {SchemaProps: spec.SchemaProps{Type: []string{"boolean"}}},
					},
				}},
			},
		},
	}
	fullResolver := &stubResolver{schemas: map[schema.GroupVersionKind]*spec.Schema{
		widgetGVK: widgetSchema,
	}}

	// Phase 3: recompile with the real schema — "widget.status.typo" should
	// now fail because Widget's status only has "ready", not "typo".
	newTypeInfo := compiler.ResolveNodeTypes(graphSpec.Nodes, fullResolver)
	assert.Empty(t, newTypeInfo.UnresolvedGVKs, "Widget should now be resolved")
	assert.Contains(t, newTypeInfo.ResourceSchemas, "widget")

	_, err = compiler.CompileGraphSpec(graphSpec, newTypeInfo)
	require.Error(t, err, "should fail — 'typo' is not a field on Widget.status")
	assert.Contains(t, err.Error(), "typo")
}

// ---------------------------------------------------------------------------
// Expression-to-field compatibility tests
// ---------------------------------------------------------------------------

func TestExprFieldCompat(t *testing.T) {
	// Schema: spec.replicas is integer, spec.name is string.
	deploySchema := &spec.Schema{
		SchemaProps: spec.SchemaProps{
			Type: []string{"object"},
			Properties: map[string]spec.Schema{
				"apiVersion": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
				"kind":       {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
				"metadata":   {SchemaProps: spec.SchemaProps{Type: []string{"object"}}},
				"spec": {
					SchemaProps: spec.SchemaProps{
						Type: []string{"object"},
						Properties: map[string]spec.Schema{
							"replicas": {SchemaProps: spec.SchemaProps{Type: []string{"integer"}}},
							"name":     {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
						},
					},
				},
			},
		},
	}
	deployGVK := schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}
	resolver := &stubResolver{schemas: map[schema.GroupVersionKind]*spec.Schema{
		deployGVK: deploySchema,
	}}

	t.Run("string expression into string field accepted", func(t *testing.T) {
		graphSpec := &graph.GraphSpec{Nodes: []graph.Node{
			defNode("data", map[string]any{"appName": "myapp"}),
			node(graph.Node{ID: "deploy", Template: map[string]any{
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
				"metadata":   map[string]any{"name": "test"},
				"spec":       map[string]any{"name": "${data.appName}"},
			}}, graph.NodeTypeTemplate),
		}}
		ts := compiler.ResolveNodeTypes(graphSpec.Nodes, resolver)
		_, err := compiler.CompileGraphSpec(graphSpec, ts)
		require.NoError(t, err)
	})

	t.Run("string expression into integer field rejected", func(t *testing.T) {
		graphSpec := &graph.GraphSpec{Nodes: []graph.Node{
			defNode("data", map[string]any{"count": "three"}),
			node(graph.Node{ID: "deploy", Template: map[string]any{
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
				"metadata":   map[string]any{"name": "test"},
				"spec":       map[string]any{"replicas": "${data.count}"},
			}}, graph.NodeTypeTemplate),
		}}
		ts := compiler.ResolveNodeTypes(graphSpec.Nodes, resolver)
		_, err := compiler.CompileGraphSpec(graphSpec, ts)
		require.Error(t, err, "string expression into integer field should fail")
		assert.ErrorIs(t, err, compiler.ErrInvalidExpression)
		assert.Contains(t, err.Error(), "replicas")
		assert.Contains(t, err.Error(), "string")
		assert.Contains(t, err.Error(), "integer")
	})

	t.Run("dyn expression into integer field accepted permissively", func(t *testing.T) {
		// When the expression type is dyn, we can't prove it wrong.
		graphSpec := &graph.GraphSpec{Nodes: []graph.Node{
			templateNode("source", map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
			}),
			node(graph.Node{ID: "deploy", Template: map[string]any{
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
				"metadata":   map[string]any{"name": "test"},
				"spec":       map[string]any{"replicas": "${source.spec.replicas}"},
			}}, graph.NodeTypeTemplate),
		}}
		ts := compiler.ResolveNodeTypes(graphSpec.Nodes, resolver)
		_, err := compiler.CompileGraphSpec(graphSpec, ts)
		require.NoError(t, err, "dyn expression should be accepted permissively")
	})
}
