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

package rgdadapter

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/kube-openapi/pkg/validation/spec"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/compiler"
	graphruntime "github.com/kubernetes-sigs/kro/pkg/graphengine/runtime"
)

func objectSchema() *spec.Schema {
	return &spec.Schema{SchemaProps: spec.SchemaProps{
		Type: spec.StringOrArray{"object"},
		Properties: map[string]spec.Schema{
			"name": {SchemaProps: spec.SchemaProps{Type: spec.StringOrArray{"string"}}},
		},
	}}
}

func arrayOfObjectsSchema() *spec.Schema {
	return &spec.Schema{SchemaProps: spec.SchemaProps{
		Type:  spec.StringOrArray{"array"},
		Items: &spec.SchemaOrArray{Schema: objectSchema()},
	}}
}

// runtimeWithSchemas builds a Runtime around a hand-written Program so a test
// can control NodeSchemas exactly, which the compiler otherwise derives.
func runtimeWithSchemas(
	t *testing.T,
	schemas map[string]*spec.Schema,
	values map[string]any,
) *graphruntime.Runtime {
	t.Helper()
	prog := &compiler.Program{
		Nodes:            map[string]*compiler.Node{},
		TopologicalOrder: []string{},
		NodeSchemas:      schemas,
	}
	rt := graphruntime.New(prog, &v1alpha1.Graph{})
	for id, v := range values {
		rt.Set(id, v)
	}
	return rt
}

// schemaAwareScope re-wraps published node values with their OpenAPI schema so
// CEL sees correctly typed values rather than raw maps — that typing is what
// makes conversions like string(secret.data.key) work, since the schema marks
// the field as byte-encoded. Anything without a schema has to pass through
// untouched rather than being dropped or mangled.
func TestSchemaAwareScope(t *testing.T) {
	t.Parallel()

	t.Run("a nil runtime returns the scope unchanged", func(t *testing.T) {
		t.Parallel()
		raw := map[string]any{"cm": map[string]any{"name": "x"}}
		assert.Equal(t, raw, schemaAwareScope(raw, nil))
	})

	t.Run("no node schemas returns the scope unchanged", func(t *testing.T) {
		t.Parallel()
		raw := map[string]any{"cm": map[string]any{"name": "x"}}
		rt := runtimeWithSchemas(t, nil, raw)
		assert.Equal(t, raw, schemaAwareScope(raw, rt))
	})

	t.Run("a node with no schema of its own passes through", func(t *testing.T) {
		t.Parallel()
		untyped := map[string]any{"name": "x"}
		raw := map[string]any{
			"typed":   map[string]any{"name": "y"},
			"untyped": untyped,
		}
		rt := runtimeWithSchemas(t,
			map[string]*spec.Schema{"typed": objectSchema()}, raw)

		out := schemaAwareScope(raw, rt)
		assert.Equal(t, untyped, out["untyped"],
			"a node the compiler could not type must survive verbatim")
		assert.NotEqual(t, raw["typed"], out["typed"],
			"a typed node must be re-wrapped rather than left as a raw map")
	})

	t.Run("a nil schema entry passes through", func(t *testing.T) {
		t.Parallel()
		raw := map[string]any{"cm": map[string]any{"name": "x"}}
		rt := runtimeWithSchemas(t, map[string]*spec.Schema{"cm": nil}, raw)

		out := schemaAwareScope(raw, rt)
		assert.Equal(t, raw["cm"], out["cm"])
	})

	t.Run("a collection is wrapped with the element schema, not the array wrapper", func(t *testing.T) {
		t.Parallel()
		// Each item is an element object. Wrapping an object with the array
		// schema would make UnstructuredToVal reject it as "expected an array",
		// so the element schema has to be unwrapped from Items first.
		raw := map[string]any{"pods": []any{
			map[string]any{"name": "a"},
			map[string]any{"name": "b"},
		}}
		rt := runtimeWithSchemas(t,
			map[string]*spec.Schema{"pods": arrayOfObjectsSchema()}, raw)

		out := schemaAwareScope(raw, rt)
		list, ok := out["pods"].([]any)
		require.True(t, ok, "a collection must stay a list")
		require.Len(t, list, 2, "no element may be dropped")
		for i, item := range list {
			assert.NotNil(t, item, "element %d must be wrapped, not nilled", i)
		}
	})

	t.Run("a non-map element in a collection passes through", func(t *testing.T) {
		t.Parallel()
		raw := map[string]any{"names": []any{"a", "b"}}
		rt := runtimeWithSchemas(t,
			map[string]*spec.Schema{"names": arrayOfObjectsSchema()}, raw)

		out := schemaAwareScope(raw, rt)
		list, ok := out["names"].([]any)
		require.True(t, ok)
		assert.Equal(t, []any{"a", "b"}, list,
			"scalar elements have no object schema to apply and must be untouched")
	})

	t.Run("a scalar node value passes through", func(t *testing.T) {
		t.Parallel()
		raw := map[string]any{"count": 3}
		rt := runtimeWithSchemas(t,
			map[string]*spec.Schema{"count": objectSchema()}, raw)

		out := schemaAwareScope(raw, rt)
		assert.Equal(t, 3, out["count"],
			"a value that is neither a map nor a list must not be re-wrapped")
	})
}
