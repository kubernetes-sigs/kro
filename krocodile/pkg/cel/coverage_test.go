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

package cel

import (
	"testing"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiservercel "k8s.io/apiserver/pkg/cel"
	"k8s.io/apiserver/pkg/cel/openapi"
	"k8s.io/kube-openapi/pkg/validation/spec"
)

// --- expression.go ---

func TestExpression_Constructors(t *testing.T) {
	t.Run("NewUncompiled", func(t *testing.T) {
		e := NewUncompiled("schema.spec.name")
		assert.Equal(t, "schema.spec.name", e.Original)
		assert.Nil(t, e.Program)
		assert.Nil(t, e.References)
	})
	t.Run("NewUncompiledSlice", func(t *testing.T) {
		s := NewUncompiledSlice("a", "b", "c")
		require.Len(t, s, 3)
		assert.Equal(t, "a", s[0].Original)
		assert.Equal(t, "c", s[2].Original)
	})
}

func TestExpression_UserExpression(t *testing.T) {
	cases := []struct {
		name string
		expr *Expression
		want string
	}{
		{
			name: "falls back to Original",
			expr: &Expression{Original: `"x" + y`},
			want: `"x" + y`,
		},
		{
			name: "prefers OriginalTemplate",
			expr: &Expression{Original: `"prefix-" + y`, OriginalTemplate: "prefix-${y}"},
			want: "prefix-${y}",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.expr.UserExpression())
		})
	}
}

func TestExpression_Eval(t *testing.T) {
	env, err := DefaultEnvironment(WithResourceIDs([]string{"x"}))
	require.NoError(t, err)

	compile := func(t *testing.T, src string) cel.Program {
		t.Helper()
		ast, issues := env.Compile(src)
		require.NoError(t, issues.Err())
		prg, err := env.Program(ast)
		require.NoError(t, err)
		return prg
	}

	t.Run("success", func(t *testing.T) {
		e := &Expression{Original: "x + 1", Program: compile(t, "x + 1")}
		got, err := e.Eval(map[string]any{"x": int64(41)})
		require.NoError(t, err)
		assert.Equal(t, int64(42), got)
	})

	t.Run("eval error surfaces wrapped", func(t *testing.T) {
		// Division by zero produces an eval-time error.
		e := &Expression{Original: "x / 0", Program: compile(t, "x / 0")}
		_, err := e.Eval(map[string]any{"x": int64(1)})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "eval")
	})
}

// --- environment.go typed constructors ---

// simpleObjectSchema builds an object schema with a couple of typed fields,
// enough to drive SchemaDeclTypeWithMetadata and the typed-environment path.
func simpleObjectSchema() *spec.Schema {
	return &spec.Schema{SchemaProps: spec.SchemaProps{
		Type: []string{"object"},
		Properties: map[string]spec.Schema{
			"name":     {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
			"replicas": {SchemaProps: spec.SchemaProps{Type: []string{"integer"}}},
		},
		Required: []string{"name"},
	}}
}

func TestTypedEnvironmentConstructors(t *testing.T) {
	schemas := map[string]*spec.Schema{"schema": simpleObjectSchema()}

	t.Run("TypedEnvironment", func(t *testing.T) {
		env, err := TypedEnvironment(schemas)
		require.NoError(t, err)
		require.NotNil(t, env)
		_, issues := env.Compile("schema.name")
		assert.NoError(t, issues.Err())
	})

	t.Run("TypedEnvironmentWithProvider", func(t *testing.T) {
		env, provider, err := TypedEnvironmentWithProvider(schemas)
		require.NoError(t, err)
		require.NotNil(t, env)
		require.NotNil(t, provider)
	})

	t.Run("TypedEnvironmentWithIDsAndProvider", func(t *testing.T) {
		env, provider, err := TypedEnvironmentWithIDsAndProvider(schemas, []string{"extra"})
		require.NoError(t, err)
		require.NotNil(t, env)
		require.NotNil(t, provider)
		// The typed var and the dyn id should both type-check.
		_, issues := env.Compile("schema.replicas")
		assert.NoError(t, issues.Err())
		_, issues = env.Compile("extra")
		assert.NoError(t, issues.Err())
	})
}

func TestWithTypedResources(t *testing.T) {
	a := map[string]*spec.Schema{"a": simpleObjectSchema()}
	b := map[string]*spec.Schema{"b": simpleObjectSchema()}

	t.Run("nil map assigns", func(t *testing.T) {
		opts := &envOptions{}
		WithTypedResources(a)(opts)
		assert.Contains(t, opts.typedResources, "a")
	})
	t.Run("existing map merges", func(t *testing.T) {
		opts := &envOptions{}
		WithTypedResources(a)(opts)
		WithTypedResources(b)(opts)
		assert.Contains(t, opts.typedResources, "a")
		assert.Contains(t, opts.typedResources, "b")
	})
}

func TestWithListVariables(t *testing.T) {
	opts := &envOptions{}
	WithListVariables([]string{"items", "ports"})(opts)
	assert.Len(t, opts.customDeclarations, 2)

	// The declared variables should support list macros in a real env.
	env, err := DefaultEnvironment(WithListVariables([]string{"items"}))
	require.NoError(t, err)
	_, issues := env.Compile("items.all(i, i != null)")
	assert.NoError(t, issues.Err())
}

func TestListElementType(t *testing.T) {
	cases := []struct {
		name    string
		input   *cel.Type
		want    *cel.Type
		wantErr bool
	}{
		{name: "list of string", input: cel.ListType(cel.StringType), want: cel.StringType},
		{name: "list of int", input: cel.ListType(cel.IntType), want: cel.IntType},
		{name: "non-list (map)", input: cel.MapType(cel.StringType, cel.IntType), wantErr: true},
		{name: "non-list (scalar)", input: cel.StringType, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ListElementType(tc.input)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.True(t, tc.want.IsExactType(got), "got %v", got)
		})
	}
}

// --- schemas.go: SchemaDeclTypeWithMetadata across schema shapes ---

func declType(t *testing.T, s *spec.Schema, root bool) *apiservercel.DeclType {
	t.Helper()
	if s == nil {
		// Pass a typed-nil common.Schema to exercise the early nil check.
		return SchemaDeclTypeWithMetadata(nil, root)
	}
	return SchemaDeclTypeWithMetadata(&openapi.Schema{Schema: s}, root)
}

func TestSchemaDeclTypeWithMetadata(t *testing.T) {
	maxLen := int64(64)
	maxItems := int64(10)
	maxProps := int64(5)
	negative := int64(-3)

	cases := []struct {
		name   string
		schema *spec.Schema
		root   bool
		assert func(t *testing.T, dt *apiservercel.DeclType)
	}{
		{
			name:   "nil schema returns nil",
			schema: nil,
			assert: func(t *testing.T, dt *apiservercel.DeclType) { assert.Nil(t, dt) },
		},
		{
			name:   "boolean",
			schema: &spec.Schema{SchemaProps: spec.SchemaProps{Type: []string{"boolean"}}},
			assert: func(t *testing.T, dt *apiservercel.DeclType) { require.NotNil(t, dt) },
		},
		{
			name:   "number",
			schema: &spec.Schema{SchemaProps: spec.SchemaProps{Type: []string{"number"}}},
			assert: func(t *testing.T, dt *apiservercel.DeclType) { require.NotNil(t, dt) },
		},
		{
			name:   "integer",
			schema: &spec.Schema{SchemaProps: spec.SchemaProps{Type: []string{"integer"}}},
			assert: func(t *testing.T, dt *apiservercel.DeclType) { require.NotNil(t, dt) },
		},
		{
			name:   "plain string",
			schema: &spec.Schema{SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
			assert: func(t *testing.T, dt *apiservercel.DeclType) { require.NotNil(t, dt) },
		},
		{
			name:   "string with maxLength",
			schema: &spec.Schema{SchemaProps: spec.SchemaProps{Type: []string{"string"}, MaxLength: &maxLen}},
			assert: func(t *testing.T, dt *apiservercel.DeclType) {
				require.NotNil(t, dt)
				assert.Equal(t, maxLen*4, dt.MaxElements)
			},
		},
		{
			name: "string enum",
			schema: &spec.Schema{SchemaProps: spec.SchemaProps{
				Type: []string{"string"},
				Enum: []interface{}{"short", "a-much-longer-value"},
			}},
			assert: func(t *testing.T, dt *apiservercel.DeclType) {
				require.NotNil(t, dt)
				assert.Equal(t, int64(len("a-much-longer-value")), dt.MaxElements)
			},
		},
		{
			name:   "string byte format",
			schema: &spec.Schema{SchemaProps: spec.SchemaProps{Type: []string{"string"}, Format: "byte"}},
			assert: func(t *testing.T, dt *apiservercel.DeclType) { require.NotNil(t, dt) },
		},
		{
			name:   "string byte format with maxLength",
			schema: &spec.Schema{SchemaProps: spec.SchemaProps{Type: []string{"string"}, Format: "byte", MaxLength: &maxLen}},
			assert: func(t *testing.T, dt *apiservercel.DeclType) {
				require.NotNil(t, dt)
				assert.Equal(t, maxLen, dt.MaxElements)
			},
		},
		{
			name:   "string duration format",
			schema: &spec.Schema{SchemaProps: spec.SchemaProps{Type: []string{"string"}, Format: "duration"}},
			assert: func(t *testing.T, dt *apiservercel.DeclType) { require.NotNil(t, dt) },
		},
		{
			name:   "string date format",
			schema: &spec.Schema{SchemaProps: spec.SchemaProps{Type: []string{"string"}, Format: "date"}},
			assert: func(t *testing.T, dt *apiservercel.DeclType) { require.NotNil(t, dt) },
		},
		{
			name:   "string date-time format",
			schema: &spec.Schema{SchemaProps: spec.SchemaProps{Type: []string{"string"}, Format: "date-time"}},
			assert: func(t *testing.T, dt *apiservercel.DeclType) { require.NotNil(t, dt) },
		},
		{
			name: "array of strings",
			schema: &spec.Schema{SchemaProps: spec.SchemaProps{
				Type:  []string{"array"},
				Items: &spec.SchemaOrArray{Schema: &spec.Schema{SchemaProps: spec.SchemaProps{Type: []string{"string"}}}},
			}},
			assert: func(t *testing.T, dt *apiservercel.DeclType) {
				require.NotNil(t, dt)
				assert.True(t, dt.IsList())
			},
		},
		{
			name: "array with explicit maxItems",
			schema: &spec.Schema{SchemaProps: spec.SchemaProps{
				Type:     []string{"array"},
				MaxItems: &maxItems,
				Items:    &spec.SchemaOrArray{Schema: &spec.Schema{SchemaProps: spec.SchemaProps{Type: []string{"string"}}}},
			}},
			assert: func(t *testing.T, dt *apiservercel.DeclType) {
				require.NotNil(t, dt)
				assert.Equal(t, maxItems, dt.MaxElements)
			},
		},
		{
			name: "array with negative maxItems clamps to zero",
			schema: &spec.Schema{SchemaProps: spec.SchemaProps{
				Type:     []string{"array"},
				MaxItems: &negative,
				Items:    &spec.SchemaOrArray{Schema: &spec.Schema{SchemaProps: spec.SchemaProps{Type: []string{"string"}}}},
			}},
			assert: func(t *testing.T, dt *apiservercel.DeclType) {
				require.NotNil(t, dt)
				assert.Equal(t, int64(0), dt.MaxElements)
			},
		},
		{
			name:   "array without items returns nil",
			schema: &spec.Schema{SchemaProps: spec.SchemaProps{Type: []string{"array"}}},
			assert: func(t *testing.T, dt *apiservercel.DeclType) { assert.Nil(t, dt) },
		},
		{
			name: "object with properties",
			schema: &spec.Schema{SchemaProps: spec.SchemaProps{
				Type: []string{"object"},
				Properties: map[string]spec.Schema{
					"name": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
				},
				Required: []string{"name"},
			}},
			assert: func(t *testing.T, dt *apiservercel.DeclType) {
				require.NotNil(t, dt)
				assert.True(t, dt.IsObject())
			},
		},
		{
			name: "object additionalProperties map",
			schema: &spec.Schema{SchemaProps: spec.SchemaProps{
				Type: []string{"object"},
				AdditionalProperties: &spec.SchemaOrBool{
					Allows: true,
					Schema: &spec.Schema{SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
				},
			}},
			assert: func(t *testing.T, dt *apiservercel.DeclType) {
				require.NotNil(t, dt)
				assert.True(t, dt.IsMap())
			},
		},
		{
			name: "object additionalProperties map with maxProperties",
			schema: &spec.Schema{SchemaProps: spec.SchemaProps{
				Type:          []string{"object"},
				MaxProperties: &maxProps,
				AdditionalProperties: &spec.SchemaOrBool{
					Allows: true,
					Schema: &spec.Schema{SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
				},
			}},
			assert: func(t *testing.T, dt *apiservercel.DeclType) {
				require.NotNil(t, dt)
				assert.True(t, dt.IsMap())
				assert.Equal(t, maxProps, dt.MaxElements)
			},
		},
		{
			name:   "int-or-string",
			schema: intOrStringSchemaSpec(),
			assert: func(t *testing.T, dt *apiservercel.DeclType) { require.NotNil(t, dt) },
		},
		{
			name:   "int-or-string with maxLength",
			schema: withMaxLength(intOrStringSchemaSpec(), 16),
			assert: func(t *testing.T, dt *apiservercel.DeclType) {
				require.NotNil(t, dt)
				assert.Equal(t, int64(16*4), dt.MaxElements)
			},
		},
		{
			name:   "untyped schema returns nil",
			schema: &spec.Schema{SchemaProps: spec.SchemaProps{}},
			assert: func(t *testing.T, dt *apiservercel.DeclType) { assert.Nil(t, dt) },
		},
		{
			name:   "resource root adds type meta",
			schema: simpleObjectSchema(),
			root:   true,
			assert: func(t *testing.T, dt *apiservercel.DeclType) {
				require.NotNil(t, dt)
				assert.True(t, dt.IsObject())
			},
		},
		{
			name:   "preserve unknown fields object",
			schema: preserveUnknownObjectSpec(),
			assert: func(t *testing.T, dt *apiservercel.DeclType) {
				require.NotNil(t, dt)
				assert.Equal(t, "true", dt.Metadata[XKubernetesPreserveUnknownFields])
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.assert(t, declType(t, tc.schema, tc.root))
		})
	}
}

func intOrStringSchemaSpec() *spec.Schema {
	trueVal := true
	return &spec.Schema{VendorExtensible: spec.VendorExtensible{
		Extensions: spec.Extensions{"x-kubernetes-int-or-string": trueVal},
	}}
}

func withMaxLength(s *spec.Schema, n int64) *spec.Schema {
	s.MaxLength = &n
	return s
}

func preserveUnknownObjectSpec() *spec.Schema {
	trueVal := true
	return &spec.Schema{
		SchemaProps: spec.SchemaProps{
			Type: []string{"object"},
			Properties: map[string]spec.Schema{
				"known": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
			},
		},
		VendorExtensible: spec.VendorExtensible{
			Extensions: spec.Extensions{"x-kubernetes-preserve-unknown-fields": trueVal},
		},
	}
}

// --- types.go: DeclTypeProvider methods ---

func buildProvider(t *testing.T) *DeclTypeProvider {
	t.Helper()
	dt := declType(t, simpleObjectSchema(), false)
	require.NotNil(t, dt)
	dt = dt.MaybeAssignTypeName(TypeNamePrefix + "schema")
	provider := NewDeclTypeProvider(dt)
	registry := types.NewEmptyRegistry()
	wrapped, err := provider.WithTypeProvider(registry)
	require.NoError(t, err)
	return wrapped
}

func TestDeclTypeProvider_Methods(t *testing.T) {
	provider := buildProvider(t)

	t.Run("FindDeclType found", func(t *testing.T) {
		dt, found := provider.FindDeclType(TypeNamePrefix + "schema")
		assert.True(t, found)
		assert.NotNil(t, dt)
	})

	t.Run("FindDeclType scalar", func(t *testing.T) {
		dt, found := provider.FindDeclType("string")
		assert.True(t, found)
		assert.NotNil(t, dt)
	})

	t.Run("FindDeclType missing", func(t *testing.T) {
		_, found := provider.FindDeclType("does.not.exist")
		assert.False(t, found)
	})

	t.Run("FindStructType found", func(t *testing.T) {
		typ, found := provider.FindStructType(TypeNamePrefix + "schema")
		assert.True(t, found)
		assert.NotNil(t, typ)
	})

	t.Run("FindStructFieldType found", func(t *testing.T) {
		ft, found := provider.FindStructFieldType(TypeNamePrefix+"schema", "name")
		assert.True(t, found)
		assert.NotNil(t, ft)
	})

	t.Run("FindStructFieldType missing field", func(t *testing.T) {
		_, found := provider.FindStructFieldType(TypeNamePrefix+"schema", "nope")
		assert.False(t, found)
	})

	t.Run("FindStructFieldNames returns empty", func(t *testing.T) {
		names, found := provider.FindStructFieldNames(TypeNamePrefix + "schema")
		assert.Empty(t, names)
		assert.False(t, found)
	})

	t.Run("TypeNames includes schema", func(t *testing.T) {
		names := provider.TypeNames()
		assert.Contains(t, names, TypeNamePrefix+"schema")
	})

	t.Run("NativeToValue", func(t *testing.T) {
		v := provider.NativeToValue("hello")
		assert.Equal(t, types.String("hello"), v)
	})

	t.Run("NewValue delegates", func(t *testing.T) {
		// NewValue delegates to the wrapped type provider. The registry has no
		// such type, so it returns an error val rather than panicking.
		v := provider.NewValue("does.not.exist", map[string]ref.Val{})
		assert.NotNil(t, v)
	})
}

func TestDeclTypeProvider_NilReceiver(t *testing.T) {
	var rt *DeclTypeProvider

	t.Run("FindStructType", func(t *testing.T) {
		_, found := rt.FindStructType("x")
		assert.False(t, found)
	})
	t.Run("FindDeclType", func(t *testing.T) {
		_, found := rt.FindDeclType("x")
		assert.False(t, found)
	})
	t.Run("WithTypeProvider", func(t *testing.T) {
		got, err := rt.WithTypeProvider(types.NewEmptyRegistry())
		require.NoError(t, err)
		assert.Nil(t, got)
	})
	t.Run("EnvOptions", func(t *testing.T) {
		opts, err := rt.EnvOptions(types.NewEmptyRegistry())
		require.NoError(t, err)
		assert.Empty(t, opts)
	})
}

func TestDeclTypeProvider_EnvOptions(t *testing.T) {
	dt := declType(t, simpleObjectSchema(), false).MaybeAssignTypeName(TypeNamePrefix + "schema")
	provider := NewDeclTypeProvider(dt)
	opts, err := provider.EnvOptions(types.NewEmptyRegistry())
	require.NoError(t, err)
	assert.Len(t, opts, 2)
}

func TestDeclTypeProvider_FindStructFieldType_Keyword(t *testing.T) {
	// A field named after a CEL reserved keyword ("namespace") is stored
	// escaped as "__namespace__"; with recognizeKeywordAsFieldName the
	// provider should resolve it via the un-escaped name.
	s := &spec.Schema{SchemaProps: spec.SchemaProps{
		Type: []string{"object"},
		Properties: map[string]spec.Schema{
			"namespace": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
		},
	}}
	dt := declType(t, s, false).MaybeAssignTypeName(TypeNamePrefix + "meta")
	provider := NewDeclTypeProvider(dt)
	provider.SetRecognizeKeywordAsFieldName(true)
	wrapped, err := provider.WithTypeProvider(types.NewEmptyRegistry())
	require.NoError(t, err)

	ft, found := wrapped.FindStructFieldType(TypeNamePrefix+"meta", "namespace")
	assert.True(t, found)
	assert.NotNil(t, ft)
}

func TestDeclTypeProvider_FindStructFieldType_DynamicMap(t *testing.T) {
	// An additionalProperties map nested under a root object gets a path-based
	// type name via FieldTypeMap. A lookup of any field name on the map type
	// resolves to the element type (dynamic-map branch).
	s := &spec.Schema{SchemaProps: spec.SchemaProps{
		Type: []string{"object"},
		Properties: map[string]spec.Schema{
			"labels": {SchemaProps: spec.SchemaProps{
				Type: []string{"object"},
				AdditionalProperties: &spec.SchemaOrBool{
					Allows: true,
					Schema: &spec.Schema{SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
				},
			}},
		},
	}}
	dt := declType(t, s, false).MaybeAssignTypeName(TypeNamePrefix + "root")
	provider := NewDeclTypeProvider(dt)
	wrapped, err := provider.WithTypeProvider(types.NewEmptyRegistry())
	require.NoError(t, err)

	// The map type is registered under the "<root>.labels" path.
	ft, found := wrapped.FindStructFieldType(TypeNamePrefix+"root.labels", "any-key")
	assert.True(t, found)
	assert.NotNil(t, ft)
}

func TestFindScalar(t *testing.T) {
	cases := []struct {
		name    string
		typ     string
		wantNil bool
	}{
		{name: "bool", typ: apiservercel.BoolType.TypeName()},
		{name: "bytes", typ: apiservercel.BytesType.TypeName()},
		{name: "double", typ: apiservercel.DoubleType.TypeName()},
		{name: "duration", typ: apiservercel.DurationType.TypeName()},
		{name: "int", typ: apiservercel.IntType.TypeName()},
		{name: "null", typ: apiservercel.NullType.TypeName()},
		{name: "string", typ: apiservercel.StringType.TypeName()},
		{name: "timestamp", typ: apiservercel.TimestampType.TypeName()},
		{name: "uint", typ: apiservercel.UintType.TypeName()},
		{name: "list", typ: apiservercel.ListType.TypeName()},
		{name: "map", typ: apiservercel.MapType.TypeName()},
		{name: "unknown returns nil", typ: "not-a-scalar", wantNil: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := findScalar(tc.typ)
			if tc.wantNil {
				assert.Nil(t, got)
				return
			}
			assert.NotNil(t, got)
		})
	}
}

func TestNewDeclTypeProvider_NilRootTypes(t *testing.T) {
	// rootTypes == nil yields an empty provider; nil entries are skipped.
	assert.NotNil(t, NewDeclTypeProvider())
	provider := NewDeclTypeProvider(nil, nil)
	assert.Empty(t, provider.TypeNames())
}

// --- compatibility.go: remaining branches ---

func TestResolveDeclTypeFromPath_Edges(t *testing.T) {
	provider := buildProvider(t)
	cases := []struct {
		name    string
		path    string
		wantNil bool
	}{
		{name: "nil provider path", path: "", wantNil: true},
		{name: "unknown root", path: "nonexistent.spec", wantNil: true},
		{name: "resolve root", path: TypeNamePrefix + "schema"},
		{name: "resolve field", path: TypeNamePrefix + "schema.name"},
		{name: "missing field", path: TypeNamePrefix + "schema.missing", wantNil: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveDeclTypeFromPath(tc.path, provider)
			if tc.wantNil {
				assert.Nil(t, got)
				return
			}
			assert.NotNil(t, got)
		})
	}
}

func TestResolveDeclTypeFromPath_NilProvider(t *testing.T) {
	assert.Nil(t, resolveDeclTypeFromPath("x.y", nil))
}

func TestResolveDeclTypeFromPath_ListTraversal(t *testing.T) {
	// Root object with a list-of-object field exercises the @idx element-type
	// traversal and the array-index "[..]" stripping in resolveDeclTypeFromPath.
	s := &spec.Schema{SchemaProps: spec.SchemaProps{
		Type: []string{"object"},
		Properties: map[string]spec.Schema{
			"items": {SchemaProps: spec.SchemaProps{
				Type: []string{"array"},
				Items: &spec.SchemaOrArray{Schema: &spec.Schema{SchemaProps: spec.SchemaProps{
					Type: []string{"object"},
					Properties: map[string]spec.Schema{
						"id": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
					},
				}}},
			}},
		},
	}}
	dt := declType(t, s, false).MaybeAssignTypeName(TypeNamePrefix + "root")
	provider := NewDeclTypeProvider(dt)
	wrapped, err := provider.WithTypeProvider(types.NewEmptyRegistry())
	require.NoError(t, err)

	cases := []struct {
		name    string
		path    string
		wantNil bool
	}{
		{name: "elem via @idx", path: TypeNamePrefix + "root.items.@idx"},
		{name: "field via @idx", path: TypeNamePrefix + "root.items.@idx.id"},
		{name: "array index stripped", path: TypeNamePrefix + "root.items[0]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveDeclTypeFromPath(tc.path, wrapped)
			if tc.wantNil {
				assert.Nil(t, got)
				return
			}
			assert.NotNil(t, got)
		})
	}
}

func TestAreStructTypesCompatible_NoProvider(t *testing.T) {
	// With a nil provider, struct compatibility falls back to permissive true.
	a := apiservercel.NewObjectType(TypeNamePrefix+"a", map[string]*apiservercel.DeclField{
		"x": apiservercel.NewDeclField("x", apiservercel.StringType, false, nil, nil),
	})
	ok, err := areStructTypesCompatible(a.CelType(), a.CelType(), nil)
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestAreListTypesCompatible_ElementIncompatible(t *testing.T) {
	// Element kinds differ (string vs int) -> incompatible with wrapped error.
	ok, err := areListTypesCompatible(cel.ListType(cel.StringType), cel.ListType(cel.IntType), nil)
	assert.False(t, ok)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "list element type incompatible")
}
