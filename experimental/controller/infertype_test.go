package graphcontroller

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/kube-openapi/pkg/validation/spec"

	krocel "github.com/kubernetes-sigs/kro/pkg/cel"
)

// templateNode builds a Template-classified Node from a template map. Test-only helper;
// production code sets the classification via parseNodeList.
func templateNode(id string, tmpl map[string]any) Node {
	return Node{ID: id, Template: tmpl, nodeType: NodeTypeTemplate}
}

// defNode builds a Definition-classified Node from a map of values.
func defNode(id string, body map[string]any) Node {
	return Node{ID: id, Def: body, nodeType: NodeTypeDef}
}

// watchNode builds a Watch-classified Node (collection) from a body.
func watchNode(id string, body map[string]any) Node {
	return Node{ID: id, Watch: body, nodeType: NodeTypeWatch}
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
			dt := inferFieldType("test.field", tt.value)
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
		dt := inferObjectType(krocel.TypeNamePrefix+"cfg", tmpl)

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
		dt := inferObjectType(krocel.TypeNamePrefix+"naming", tmpl)

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
		dt := inferObjectType(krocel.TypeNamePrefix+"test", tmpl)

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
		dt := inferObjectType(krocel.TypeNamePrefix+"test", tmpl)

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
		nodes := []Node{
			{ID: "naming", Def: map[string]any{"prefix": "myapp"}, nodeType: NodeTypeDef},
		}
		ts := resolveNodeTypes(nodes, nil)

		assert.Len(t, ts.definitionTypes, 1)
		assert.Contains(t, ts.definitionTypes, "naming")
		assert.Empty(t, ts.untypedIDs)
	})

	t.Run("resource nodes without resolver are untyped", func(t *testing.T) {
		nodes := []Node{
			{ID: "deploy", Template: map[string]any{
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
				"metadata":   map[string]any{"name": "test"},
			}, nodeType: NodeTypeTemplate},
		}
		ts := resolveNodeTypes(nodes, nil)

		assert.Empty(t, ts.definitionTypes)
		assert.Contains(t, ts.untypedIDs, "deploy")
	})

	t.Run("forEach iterator variables are untyped", func(t *testing.T) {
		nodes := []Node{
			{
				ID:       "items",
				ForEach:  &ForEachBinding{VarName: "item", Expr: "${spec.items}"},
				Def:      map[string]any{"name": "${item}"},
				nodeType: NodeTypeDef},
		}
		ts := resolveNodeTypes(nodes, nil)

		assert.Contains(t, ts.definitionTypes, "items")
		assert.True(t, ts.forEachDefinitions["items"])
		assert.Contains(t, ts.untypedIDs, "item")
	})

	t.Run("mixed definitions and resources", func(t *testing.T) {
		nodes := []Node{
			{ID: "naming", Def: map[string]any{"prefix": "app"}, nodeType: NodeTypeDef},
			{ID: "deploy", Template: map[string]any{
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
				"metadata":   map[string]any{"name": "${naming.prefix}"},
			}, nodeType: NodeTypeTemplate},
		}
		ts := resolveNodeTypes(nodes, nil)

		assert.Len(t, ts.definitionTypes, 1)
		assert.Contains(t, ts.definitionTypes, "naming")
		assert.Contains(t, ts.untypedIDs, "deploy")
	})
}

// ---------------------------------------------------------------------------
// Compile-time validation tests (the key behavioral tests)
// ---------------------------------------------------------------------------

func TestDefinitionFieldValidation(t *testing.T) {
	t.Run("valid field access compiles", func(t *testing.T) {
		spec := &GraphSpec{Nodes: []Node{
			{ID: "naming", Def: map[string]any{"prefix": "myapp"}, nodeType: NodeTypeDef},
			{ID: "deploy", Template: map[string]any{
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
				"metadata":   map[string]any{"name": "${naming.prefix + '-deploy'}"},
			}, nodeType: NodeTypeTemplate},
		}}
		_, err := compileGraphSpec(spec, nil)
		require.NoError(t, err)
	})

	t.Run("wrong field on definition fails compilation", func(t *testing.T) {
		spec := &GraphSpec{Nodes: []Node{
			{ID: "naming", Def: map[string]any{"prefix": "myapp"}, nodeType: NodeTypeDef},
			{ID: "deploy", Template: map[string]any{
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
				"metadata":   map[string]any{"name": "${naming.typo}"},
			}, nodeType: NodeTypeTemplate},
		}}
		_, err := compileGraphSpec(spec, nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "naming.typo")
	})

	t.Run("nested field access compiles", func(t *testing.T) {
		spec := &GraphSpec{Nodes: []Node{
			{ID: "cfg", Def: map[string]any{
				"metadata": map[string]any{
					"env":    "prod",
					"region": "us-west-2",
				},
			}, nodeType: NodeTypeDef},
			{ID: "deploy", Template: map[string]any{
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
				"metadata":   map[string]any{"name": "${cfg.metadata.env + '-deploy'}"},
			}, nodeType: NodeTypeTemplate},
		}}
		_, err := compileGraphSpec(spec, nil)
		require.NoError(t, err)
	})

	t.Run("wrong nested field fails compilation", func(t *testing.T) {
		spec := &GraphSpec{Nodes: []Node{
			{ID: "cfg", Def: map[string]any{
				"metadata": map[string]any{
					"env": "prod",
				},
			}, nodeType: NodeTypeDef},
			{ID: "deploy", Template: map[string]any{
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
				"metadata":   map[string]any{"name": "${cfg.metadata.typo}"},
			}, nodeType: NodeTypeTemplate},
		}}
		_, err := compileGraphSpec(spec, nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "typo")
	})

	t.Run("dyn expression fields allow any access", func(t *testing.T) {
		// A definition with an expression-valued field should be dyn,
		// allowing downstream expressions to access any sub-field.
		spec := &GraphSpec{Nodes: []Node{
			{ID: "upstream", Template: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]any{"name": "cfg"},
			}, nodeType: NodeTypeTemplate},
			{ID: "derived", Def: map[string]any{
				"data": "${upstream.data}",
			}, nodeType: NodeTypeDef},
			{ID: "consumer", Template: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]any{"name": "${derived.data.someKey}"},
			}, nodeType: NodeTypeTemplate},
		}}
		_, err := compileGraphSpec(spec, nil)
		require.NoError(t, err)
	})

	t.Run("cross-definition validation", func(t *testing.T) {
		// Definition A → Definition B → resource node.
		spec := &GraphSpec{Nodes: []Node{
			{ID: "a", Def: map[string]any{"prefix": "app"}, nodeType: NodeTypeDef},
			{ID: "b", Def: map[string]any{"full": "${a.prefix + '-svc'}"}, nodeType: NodeTypeDef},
			{ID: "svc", Template: map[string]any{
				"apiVersion": "v1",
				"kind":       "Service",
				"metadata":   map[string]any{"name": "${b.full}"},
			}, nodeType: NodeTypeTemplate},
		}}
		_, err := compileGraphSpec(spec, nil)
		require.NoError(t, err)
	})

	t.Run("cross-definition wrong field fails", func(t *testing.T) {
		spec := &GraphSpec{Nodes: []Node{
			{ID: "a", Def: map[string]any{"prefix": "app"}, nodeType: NodeTypeDef},
			{ID: "b", Def: map[string]any{"full": "${a.typo}"}, nodeType: NodeTypeDef},
		}}
		_, err := compileGraphSpec(spec, nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "a.typo")
	})

	t.Run("type refinement narrows def expression field from dyn to string", func(t *testing.T) {
		// b.full is ${a.prefix + '-svc'} which returns string.
		// After refinement, b.full is typed as string, not dyn.
		// Verify: .startsWith() is a string method, not available on dyn.
		spec := &GraphSpec{Nodes: []Node{
			{ID: "a", Def: map[string]any{"prefix": "app"}, nodeType: NodeTypeDef},
			{ID: "b", Def: map[string]any{"full": "${a.prefix + '-svc'}"}, nodeType: NodeTypeDef},
		}}
		compiled, err := compileGraphSpec(spec, nil)
		require.NoError(t, err)

		// The refined environment should type b.full as string.
		parsed, issues := compiled.env.Parse("b.full.startsWith('app')")
		require.Nil(t, issues.Err())
		ast, issues := compiled.env.Check(parsed)
		require.Nil(t, issues.Err(), "b.full should be string after refinement, supporting .startsWith()")
		assert.Equal(t, "bool", ast.OutputType().String())
	})

	t.Run("type refinement catches field access error on narrowed string", func(t *testing.T) {
		// b.full is narrowed to string. Accessing .nonExistentField is a
		// type error on string that was invisible against dyn.
		spec := &GraphSpec{Nodes: []Node{
			{ID: "a", Def: map[string]any{"prefix": "app"}, nodeType: NodeTypeDef},
			{ID: "b", Def: map[string]any{"full": "${a.prefix + '-svc'}"}, nodeType: NodeTypeDef},
			{ID: "svc", Template: map[string]any{
				"apiVersion": "v1",
				"kind":       "Service",
				"metadata":   map[string]any{"name": "${b.full.nonExistentField}"},
			}, nodeType: NodeTypeTemplate},
		}}
		_, err := compileGraphSpec(spec, nil)
		require.Error(t, err, "b.full is string after narrowing; field access should fail")
		assert.ErrorIs(t, err, ErrInvalidExpression)
		assert.Contains(t, err.Error(), "type refinement")
	})

	t.Run("type refinement allows valid narrowed access", func(t *testing.T) {
		// b.full is narrowed to string. Using string operations should work.
		spec := &GraphSpec{Nodes: []Node{
			{ID: "a", Def: map[string]any{"prefix": "app"}, nodeType: NodeTypeDef},
			{ID: "b", Def: map[string]any{"full": "${a.prefix + '-svc'}"}, nodeType: NodeTypeDef},
			{ID: "svc", Template: map[string]any{
				"apiVersion": "v1",
				"kind":       "Service",
				"metadata":   map[string]any{"name": "${b.full.upperAscii()}"},
			}, nodeType: NodeTypeTemplate},
		}}
		_, err := compileGraphSpec(spec, nil)
		require.NoError(t, err, "b.full is string after narrowing; string methods should work")
	})

	t.Run("readyWhen can access definition fields", func(t *testing.T) {
		spec := &GraphSpec{Nodes: []Node{
			{
				ID:        "cfg",
				Def:       map[string]any{"count": "3"},
				ReadyWhen: []string{"${cfg.count == '3'}"},
				nodeType:  NodeTypeDef},
		}}
		_, err := compileGraphSpec(spec, nil)
		require.NoError(t, err)
	})

	t.Run("forEach definition compiles as list type", func(t *testing.T) {
		spec := &GraphSpec{Nodes: []Node{
			{ID: "upstream", Template: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"data":       map[string]any{"items": "a,b,c"},
			}, nodeType: NodeTypeTemplate},
			{
				ID:       "items",
				ForEach:  &ForEachBinding{VarName: "item", Expr: "${upstream.data.items}"},
				Def:      map[string]any{"name": "${item}", "port": int64(80)},
				nodeType: NodeTypeDef},
		}}
		_, err := compileGraphSpec(spec, nil)
		require.NoError(t, err)
	})
}

// ---------------------------------------------------------------------------
// forEach return-type validation tests
// ---------------------------------------------------------------------------

func TestForEachReturnTypeValidation(t *testing.T) {
	t.Run("list expression accepted", func(t *testing.T) {
		// A definition node producing a list, referenced by forEach.
		spec := &GraphSpec{Nodes: []Node{
			defNode("data", map[string]any{"items": []any{"a", "b"}}),
			{
				ID:       "worker",
				ForEach:  &ForEachBinding{VarName: "item", Expr: "${data.items}"},
				Def:      map[string]any{"name": "${item}"},
				nodeType: NodeTypeDef,
			},
		}}
		_, err := compileGraphSpec(spec, nil)
		require.NoError(t, err)
	})

	t.Run("string expression rejected", func(t *testing.T) {
		// A definition node producing a string — forEach over a string
		// is always wrong.
		spec := &GraphSpec{Nodes: []Node{
			defNode("data", map[string]any{"name": "hello"}),
			{
				ID:       "worker",
				ForEach:  &ForEachBinding{VarName: "item", Expr: "${data.name}"},
				Def:      map[string]any{"v": "${item}"},
				nodeType: NodeTypeDef,
			},
		}}
		_, err := compileGraphSpec(spec, nil)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrInvalidExpression)
		assert.Contains(t, err.Error(), "must return a list")
		assert.Contains(t, err.Error(), "worker")
	})

	t.Run("dyn expression accepted", func(t *testing.T) {
		// An unresolved template node — typed as dyn. The compiler
		// can't prove it's wrong, so it must accept it.
		spec := &GraphSpec{Nodes: []Node{
			templateNode("source", map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
			}),
			{
				ID:       "worker",
				ForEach:  &ForEachBinding{VarName: "item", Expr: "${source}"},
				Def:      map[string]any{"v": "${item}"},
				nodeType: NodeTypeDef,
			},
		}}
		_, err := compileGraphSpec(spec, nil)
		require.NoError(t, err)
	})

	t.Run("interpolated expression rejected", func(t *testing.T) {
		// A forEach expression that's an interpolated string, not a
		// standalone ${...}. Interpolation always produces a string.
		spec := &GraphSpec{Nodes: []Node{
			defNode("data", map[string]any{"items": []any{"a", "b"}}),
			{
				ID:       "worker",
				ForEach:  &ForEachBinding{VarName: "item", Expr: "prefix-${data.items}"},
				Def:      map[string]any{"v": "${item}"},
				nodeType: NodeTypeDef,
			},
		}}
		_, err := compileGraphSpec(spec, nil)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrInvalidExpression)
		assert.Contains(t, err.Error(), "standalone")
	})

	t.Run("literal string rejected", func(t *testing.T) {
		// A forEach expression with no ${...} at all.
		spec := &GraphSpec{Nodes: []Node{
			{
				ID:       "worker",
				ForEach:  &ForEachBinding{VarName: "item", Expr: "just a string"},
				Def:      map[string]any{"v": "${item}"},
				nodeType: NodeTypeDef,
			},
		}}
		_, err := compileGraphSpec(spec, nil)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrInvalidExpression)
		assert.Contains(t, err.Error(), "must contain")
	})

	t.Run("deferred expression skipped", func(t *testing.T) {
		// A $${...} forEach is deferred to the child graph. The parent
		// compiler must not reject it.
		spec := &GraphSpec{Nodes: []Node{
			defNode("data", map[string]any{"items": []any{"a", "b"}}),
			{
				ID:       "worker",
				ForEach:  &ForEachBinding{VarName: "item", Expr: "$${data.items}"},
				Def:      map[string]any{"v": "literal"},
				nodeType: NodeTypeDef,
			},
		}}
		_, err := compileGraphSpec(spec, nil)
		require.NoError(t, err)
	})

	t.Run("map expression rejected", func(t *testing.T) {
		// A definition producing a map — forEach over a map is wrong.
		spec := &GraphSpec{Nodes: []Node{
			defNode("data", map[string]any{"kv": map[string]any{"a": "1"}}),
			{
				ID:       "worker",
				ForEach:  &ForEachBinding{VarName: "item", Expr: "${data.kv}"},
				Def:      map[string]any{"v": "${item}"},
				nodeType: NodeTypeDef,
			},
		}}
		_, err := compileGraphSpec(spec, nil)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrInvalidExpression)
		assert.Contains(t, err.Error(), "must return a list")
	})
}

func TestExtractLiteralGVK(t *testing.T) {
	t.Run("literal GVK", func(t *testing.T) {
		gvk := extractLiteralGVK(map[string]any{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
		})
		require.NotNil(t, gvk)
		assert.Equal(t, "apps", gvk.Group)
		assert.Equal(t, "v1", gvk.Version)
		assert.Equal(t, "Deployment", gvk.Kind)
	})

	t.Run("expression apiVersion returns nil", func(t *testing.T) {
		gvk := extractLiteralGVK(map[string]any{
			"apiVersion": "${spec.apiVersion}",
			"kind":       "Deployment",
		})
		assert.Nil(t, gvk)
	})

	t.Run("missing kind returns nil", func(t *testing.T) {
		gvk := extractLiteralGVK(map[string]any{
			"apiVersion": "v1",
		})
		assert.Nil(t, gvk)
	})

	t.Run("nil template returns nil", func(t *testing.T) {
		gvk := extractLiteralGVK(nil)
		assert.Nil(t, gvk)
	})

	t.Run("core type", func(t *testing.T) {
		gvk := extractLiteralGVK(map[string]any{
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
			dt := inferStringType(tt.input)
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
	ts := resolveNodeTypes([]Node{
		{ID: "alpha", Def: map[string]any{
			"nested": map[string]any{"x": "hello"},
		}, nodeType: NodeTypeDef},
		{ID: "beta", Def: map[string]any{
			"nested": map[string]any{"y": int64(42)},
		}, nodeType: NodeTypeDef},
	}, nil)

	alphaDT := ts.definitionTypes["alpha"]
	betaDT := ts.definitionTypes["beta"]

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
	ctx := t.Context()

	spec := &GraphSpec{Nodes: []Node{
		{ID: "cfg", Def: map[string]any{
			"region": "us-west-2",
			"count":  int64(3),
		}, nodeType: NodeTypeDef},
		{ID: "derived", Def: map[string]any{
			"name": "${cfg.region + '-app'}",
		}, nodeType: NodeTypeDef},
	}}

	compiled, err := compileGraphSpec(spec, nil)
	require.NoError(t, err)

	eval := &evaluator{compiled: compiled, scope: map[string]any{}}

	// Reconcile first definition.
	err = r.reconcileDefinition(ctx, spec.Nodes[0], eval)
	require.NoError(t, err)

	cfgResult, ok := eval.scope["cfg"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "us-west-2", cfgResult["region"])
	assert.Equal(t, int64(3), cfgResult["count"])

	// Reconcile second definition that references the first.
	err = r.reconcileDefinition(ctx, spec.Nodes[1], eval)
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
		nodes := []Node{
			{ID: "cm", Template: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]any{"name": "test"},
				"data":       map[string]any{"key": "value"},
			}, nodeType: NodeTypeTemplate},
		}
		ts := resolveNodeTypes(nodes, resolver)

		// The ConfigMap node should be resolved via schema.
		assert.Contains(t, ts.resourceSchemas, "cm")
		assert.Empty(t, ts.untypedIDs)
	})

	t.Run("mixed schema and definition types compile together", func(t *testing.T) {
		graphSpec := &GraphSpec{Nodes: []Node{
			{ID: "cm", Template: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]any{"name": "test"},
				"data":       map[string]any{"key": "value"},
			}, nodeType: NodeTypeTemplate},
			{ID: "naming", Def: map[string]any{
				"prefix": "myapp",
			}, nodeType: NodeTypeDef},
			{ID: "consumer", Template: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]any{"name": "${naming.prefix + '-cfg'}"},
				"data":       map[string]any{"ref": "${cm.data}"},
			}, nodeType: NodeTypeTemplate},
		}}
		typeInfo := resolveNodeTypes(graphSpec.Nodes, resolver)
		_, err := compileGraphSpec(graphSpec, typeInfo)
		require.NoError(t, err)
	})

	t.Run("unresolved CRD falls back to dyn and tracks GVK", func(t *testing.T) {
		nodes := []Node{
			{ID: "custom", Template: map[string]any{
				"apiVersion": "custom.example.com/v1",
				"kind":       "Widget",
				"metadata":   map[string]any{"name": "test"},
			}, nodeType: NodeTypeTemplate},
		}
		ts := resolveNodeTypes(nodes, resolver)

		// Widget is not in the stub resolver — should fall back to dyn.
		assert.Empty(t, ts.resourceSchemas)
		assert.Contains(t, ts.untypedIDs, "custom")

		// The unresolved GVK should be tracked for CRD watch.
		require.Len(t, ts.unresolvedGVKs, 1)
		assert.Equal(t, "Widget", ts.unresolvedGVKs[0].Kind)
		assert.Equal(t, "custom.example.com", ts.unresolvedGVKs[0].Group)
	})
}

// ---------------------------------------------------------------------------
// Cache eviction tests
// ---------------------------------------------------------------------------

func TestEvictUnresolved(t *testing.T) {
	t.Run("evicts compilations with unresolved GVKs", func(t *testing.T) {
		caches := newGraphCaches()

		// Create a compiled graph with unresolved GVKs.
		unresolvedCompiled := &compiledGraph{
			compilationKey: "hash-unresolved",
			unresolvedGVKs: []schema.GroupVersionKind{{Group: "example.com", Version: "v1", Kind: "Foo"}},
		}
		state := &instanceState{compiled: unresolvedCompiled}
		caches.set("default/my-graph-g00001", state)

		// Create a compiled graph without unresolved GVKs.
		resolvedCompiled := &compiledGraph{
			compilationKey: "hash-resolved",
		}
		resolvedState := &instanceState{compiled: resolvedCompiled}
		caches.set("default/other-graph-g00001", resolvedState)

		compiledCount, instanceCount := caches.CacheSizes()
		assert.Equal(t, 2, compiledCount)
		assert.Equal(t, 2, instanceCount)

		// Evict for the GVR that matches the unresolved GVK.
		fooGVR := gvkToGVR(schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "Foo"})
		affected := caches.evictUnresolved(fooGVR)

		// The unresolved compiledGraph is removed from the compiled cache.
		// The instanceState stays in instances with compiled == nil.
		compiledCount, instanceCount = caches.CacheSizes()
		assert.Equal(t, 1, compiledCount)
		assert.Equal(t, 2, instanceCount) // instance stays, compiled is nil

		// The resolved graph should still be cached.
		assert.NotNil(t, caches.getCompiled("hash-resolved"))
		assert.Nil(t, caches.getCompiled("hash-unresolved"))

		// The evicted instance's compiled pointer should be nil.
		evictedState := caches.get("default/my-graph-g00001")
		assert.NotNil(t, evictedState)
		assert.Nil(t, evictedState.compiled)

		// The resolved instance should be unaffected.
		assert.NotNil(t, caches.get("default/other-graph-g00001").compiled)

		// Affected should contain the graph key.
		require.Len(t, affected, 1)
		assert.Equal(t, "my-graph", affected[0].Name)
		assert.Equal(t, "default", affected[0].Namespace)
	})

	t.Run("no-op when no unresolved compilations", func(t *testing.T) {
		caches := newGraphCaches()

		resolvedCompiled := &compiledGraph{compilationKey: "hash-resolved"}
		caches.set("default/graph-g00001", &instanceState{compiled: resolvedCompiled})

		// No unresolved compilations → no-op regardless of GVR.
		affected := caches.evictUnresolved(gvkToGVR(schema.GroupVersionKind{
			Group: "example.com", Version: "v1", Kind: "Widget",
		}))
		assert.Empty(t, affected)

		compiledCount, _ := caches.CacheSizes()
		assert.Equal(t, 1, compiledCount)
	})

	// Regression: before the fix, evictUnresolved ignored the incoming GVR and
	// evicted every compiled graph with any unresolved GVK. Installing one CRD
	// could thunder-herd-recompile Graphs still waiting on a different CRD.
	// The filtered version only evicts compiled graphs whose unresolved set
	// includes the GVR that just became watchable.
	t.Run("RegressionGVRFilter_onlyEvictsMatchingGVR", func(t *testing.T) {
		caches := newGraphCaches()

		fooCompiled := &compiledGraph{
			compilationKey: "hash-foo",
			unresolvedGVKs: []schema.GroupVersionKind{
				{Group: "example.com", Version: "v1", Kind: "Foo"},
			},
		}
		caches.set("default/foo-graph-g00001", &instanceState{compiled: fooCompiled})

		barCompiled := &compiledGraph{
			compilationKey: "hash-bar",
			unresolvedGVKs: []schema.GroupVersionKind{
				{Group: "example.com", Version: "v1", Kind: "Bar"},
			},
		}
		caches.set("default/bar-graph-g00001", &instanceState{compiled: barCompiled})

		// Foo becomes available — only the foo graph should be evicted.
		fooGVR := gvkToGVR(schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "Foo"})
		affected := caches.evictUnresolved(fooGVR)
		require.Len(t, affected, 1)
		assert.Equal(t, "foo-graph", affected[0].Name)

		// Bar's compiledGraph must still be cached — it's waiting on Bar, not Foo.
		assert.NotNil(t, caches.getCompiled("hash-bar"))
		assert.Nil(t, caches.getCompiled("hash-foo"))

		barState := caches.get("default/bar-graph-g00001")
		require.NotNil(t, barState)
		assert.NotNil(t, barState.compiled, "bar graph's compiled pointer must be intact")
	})
}

func TestInstanceKeyToGraphKey(t *testing.T) {
	tests := []struct {
		key      string
		wantName string
		wantNS   string
		wantOK   bool
	}{
		{"default/my-graph-g00001", "my-graph", "default", true},
		{"kube-system/complex-name-here-g00042", "complex-name-here", "kube-system", true},
		{"bad-format", "", "", false},
		{"ns/no-generation-suffix", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			gk, ok := instanceKeyToGraphKey(tt.key)
			assert.Equal(t, tt.wantOK, ok)
			if ok {
				assert.Equal(t, tt.wantName, gk.Name)
				assert.Equal(t, tt.wantNS, gk.Namespace)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Recompilation on schema change
// ---------------------------------------------------------------------------

func TestRecompilationOnSchemaChange(t *testing.T) {
	// Simulates: CRD not available → compile with dyn → CRD installed →
	// evict → recompile with schema → type error detected.

	widgetGVK := schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "Widget"}

	emptyResolver := &stubResolver{schemas: map[schema.GroupVersionKind]*spec.Schema{}}

	graphSpec := &GraphSpec{Nodes: []Node{
		{ID: "widget", Template: map[string]any{
			"apiVersion": "example.com/v1",
			"kind":       "Widget",
			"metadata":   map[string]any{"name": "test"},
		}, nodeType: NodeTypeTemplate},
		// Consumer is a definition — no GVK to resolve, so the only
		// unresolved GVK is Widget.
		{ID: "consumer", Def: map[string]any{
			"name": "${widget.status.typo}",
		}, nodeType: NodeTypeDef},
	}}

	// Phase 1: compile with empty resolver — Widget falls back to dyn.
	// "${widget.status.typo}" compiles fine because widget is dyn.
	typeInfo := resolveNodeTypes(graphSpec.Nodes, emptyResolver)
	require.Len(t, typeInfo.unresolvedGVKs, 1)
	assert.Equal(t, widgetGVK, typeInfo.unresolvedGVKs[0])

	compiled, err := compileGraphSpec(graphSpec, typeInfo)
	require.NoError(t, err, "should compile with dyn — no type checking on widget")
	assert.Len(t, compiled.unresolvedGVKs, 1)

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
	newTypeInfo := resolveNodeTypes(graphSpec.Nodes, fullResolver)
	assert.Empty(t, newTypeInfo.unresolvedGVKs, "Widget should now be resolved")
	assert.Contains(t, newTypeInfo.resourceSchemas, "widget")

	_, err = compileGraphSpec(graphSpec, newTypeInfo)
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
		graphSpec := &GraphSpec{Nodes: []Node{
			defNode("data", map[string]any{"appName": "myapp"}),
			{ID: "deploy", Template: map[string]any{
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
				"metadata":   map[string]any{"name": "test"},
				"spec":       map[string]any{"name": "${data.appName}"},
			}, nodeType: NodeTypeTemplate},
		}}
		ts := resolveNodeTypes(graphSpec.Nodes, resolver)
		_, err := compileGraphSpec(graphSpec, ts)
		require.NoError(t, err)
	})

	t.Run("string expression into integer field rejected", func(t *testing.T) {
		graphSpec := &GraphSpec{Nodes: []Node{
			defNode("data", map[string]any{"count": "three"}),
			{ID: "deploy", Template: map[string]any{
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
				"metadata":   map[string]any{"name": "test"},
				"spec":       map[string]any{"replicas": "${data.count}"},
			}, nodeType: NodeTypeTemplate},
		}}
		ts := resolveNodeTypes(graphSpec.Nodes, resolver)
		_, err := compileGraphSpec(graphSpec, ts)
		require.Error(t, err, "string expression into integer field should fail")
		assert.ErrorIs(t, err, ErrInvalidExpression)
		assert.Contains(t, err.Error(), "replicas")
		assert.Contains(t, err.Error(), "string")
		assert.Contains(t, err.Error(), "integer")
	})

	t.Run("dyn expression into integer field accepted permissively", func(t *testing.T) {
		// When the expression type is dyn, we can't prove it wrong.
		graphSpec := &GraphSpec{Nodes: []Node{
			templateNode("source", map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
			}),
			{ID: "deploy", Template: map[string]any{
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
				"metadata":   map[string]any{"name": "test"},
				"spec":       map[string]any{"replicas": "${source.spec.replicas}"},
			}, nodeType: NodeTypeTemplate},
		}}
		ts := resolveNodeTypes(graphSpec.Nodes, resolver)
		_, err := compileGraphSpec(graphSpec, ts)
		require.NoError(t, err, "dyn expression should be accepted permissively")
	})
}
