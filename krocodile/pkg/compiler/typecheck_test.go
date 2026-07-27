// Copyright 2026 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License. You may
// obtain a copy of the License at
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

	"github.com/google/cel-go/cel"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiservercel "k8s.io/apiserver/pkg/cel"
	"k8s.io/kube-openapi/pkg/validation/spec"

	expv1alpha1 "sigs.k8s.io/krocodile/api/v1alpha1"
	"sigs.k8s.io/krocodile/pkg/compiler/fieldpath"
	"sigs.k8s.io/krocodile/pkg/compiler/schema"
	"sigs.k8s.io/krocodile/pkg/compiler/variable"
	"sigs.k8s.io/krocodile/pkg/testutil/generator"
)

// TestExpectedTypeForField exercises the schema-walking + provider lookup
// path that turns a (descriptor, root schema) pair into a CEL type.
func TestExpectedTypeForField(t *testing.T) {
	t.Parallel()
	stringFieldSchema := &spec.Schema{SchemaProps: spec.SchemaProps{
		Type: []string{"object"},
		Properties: map[string]spec.Schema{
			"known": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
		},
	}}
	cases := []struct {
		name string
		desc *variable.FieldDescriptor
		root *spec.Schema
		want *cel.Type
	}{
		{name: "nil root returns dyn", desc: &variable.FieldDescriptor{Path: "spec.x"}, root: nil, want: cel.DynType},
		{name: "unknown field returns dyn", desc: &variable.FieldDescriptor{Path: "missing"}, root: stringFieldSchema, want: cel.DynType},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bc := &buildContext{
				schemaCache: schema.NewCache(),
				declTypes:   map[*spec.Schema]*apiservercel.DeclType{},
			}
			got := expectedTypeForField(bc, tc.desc, tc.root, "x")
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestLookupSchemaAtField covers the helper behind expectedTypeForField:
// direct property lookup, additionalProperties fallback, array-Items walk.
func TestLookupSchemaAtField(t *testing.T) {
	t.Parallel()
	additionalProps := &spec.Schema{SchemaProps: spec.SchemaProps{
		Type: []string{"object"},
		AdditionalProperties: &spec.SchemaOrBool{
			Allows: true,
			Schema: &spec.Schema{SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
		},
	}}
	plain := &spec.Schema{SchemaProps: spec.SchemaProps{Type: []string{"object"}}}
	cases := []struct {
		name     string
		root     *spec.Schema
		field    string
		wantNil  bool
		wantType string
		wantSelf bool
	}{
		{name: "nil schema", root: nil, field: "x", wantNil: true},
		{name: "empty field returns self", root: plain, field: "", wantSelf: true},
		{name: "additionalProperties resolves", root: additionalProps, field: "anything", wantType: "string"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := lookupSchemaAtField(schema.NewCache(), tc.root, tc.field)
			if tc.wantNil {
				assert.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			if tc.wantSelf {
				assert.Same(t, tc.root, got)
				return
			}
			assert.True(t, got.Type.Contains(tc.wantType))
		})
	}
}

// TestIteratorFingerprint asserts ordering insensitivity and type
// sensitivity of the cache key.
func TestIteratorFingerprint(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		a         map[string]*cel.Type
		b         map[string]*cel.Type
		wantEqual bool
	}{
		{
			name:      "reorder is equal",
			a:         map[string]*cel.Type{"a": cel.StringType, "b": cel.IntType},
			b:         map[string]*cel.Type{"b": cel.IntType, "a": cel.StringType},
			wantEqual: true,
		},
		{
			name:      "different type differs",
			a:         map[string]*cel.Type{"r": cel.StringType},
			b:         map[string]*cel.Type{"r": cel.IntType},
			wantEqual: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			eq := iteratorFingerprint(tc.a) == iteratorFingerprint(tc.b)
			assert.Equal(t, tc.wantEqual, eq)
		})
	}
}

// TestResolveSchemaAndTypeName covers index segments (array dereference) and
// the index-on-non-array error path.
func TestResolveSchemaAndTypeName(t *testing.T) {
	t.Parallel()
	arrayOfString := &spec.Schema{SchemaProps: spec.SchemaProps{
		Type: []string{"object"},
		Properties: map[string]spec.Schema{
			"items": {SchemaProps: spec.SchemaProps{
				Type: []string{"array"},
				Items: &spec.SchemaOrArray{Schema: &spec.Schema{
					SchemaProps: spec.SchemaProps{Type: []string{"string"}},
				}},
			}},
		},
	}}
	scalarField := &spec.Schema{SchemaProps: spec.SchemaProps{
		Type: []string{"object"},
		Properties: map[string]spec.Schema{
			"name": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
		},
	}}
	cases := []struct {
		name         string
		root         *spec.Schema
		path         string
		wantErr      bool
		wantLeafType string
		wantInName   string
	}{
		{name: "array index", root: arrayOfString, path: "items[0]", wantLeafType: "string", wantInName: "@idx"},
		{name: "index on non-array", root: scalarField, path: "name[0]", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			segs, err := fieldpath.Parse(tc.path)
			require.NoError(t, err)
			leaf, name, err := resolveSchemaAndTypeName(schema.NewCache(), segs, tc.root, "x")
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.True(t, leaf.Type.Contains(tc.wantLeafType))
			assert.Contains(t, name, tc.wantInName)
		})
	}
}

// TestCompile_TypeMismatch ensures the per-expression type check actually
// rejects assignments that would silently pass otherwise. Driven through the
// full Compile pipeline so the rows reflect real call paths.
func TestCompile_TypeMismatch(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		graph   *expv1alpha1.Graph
		wantErr string
	}{
		{
			name: "string into object",
			graph: generator.NewGraph("g",
				generator.WithTemplate("cm", configMap("source")),
				generator.WithTemplate("rq", map[string]any{
					"apiVersion": "v1", "kind": "ResourceQuota",
					"metadata": map[string]any{"name": "rq"},
					"spec":     map[string]any{"hard": "${cm.metadata.name}"},
				}),
			),
			wantErr: "type mismatch",
		},
		{
			name: "string template stays string at metadata.name",
			graph: generator.NewGraph("g",
				generator.WithTemplate("cm", configMap("source")),
				generator.WithTemplate("p", map[string]any{
					"apiVersion": "v1", "kind": "Pod",
					"metadata": map[string]any{"name": "${cm.metadata.name}-suffix"},
					"spec":     map[string]any{"containers": []any{map[string]any{"name": "c", "image": "nginx"}}},
				}),
			),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := newTestCompiler(t)
			_, err := c.Compile(tc.graph)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}
