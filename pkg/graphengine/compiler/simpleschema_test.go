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

	"github.com/kubernetes-sigs/kro/pkg/graphengine/testutil/generator"
)

// TestCompile_SimpleSchemaToOpenAPI covers the Graph compiler's handling
// of the namespaced simpleschema.toOpenAPI() function: the inspector must
// treat `simpleschema` as a function namespace rather than an unknown node id,
// the typed env must accept object-typed def fields as arguments, and the
// dependency on the def node feeding the call must still be recorded.
func TestCompile_SimpleSchemaToOpenAPI(t *testing.T) {
	t.Parallel()

	rgdSchemaDef := map[string]any{
		"kind": "App",
		"spec": map[string]any{
			"name":     "string | required=true",
			"replicas": "integer | default=1",
			"owner":    "Owner",
		},
		"types": map[string]any{
			"Owner": map[string]any{"team": "string"},
		},
	}

	t.Run("def-fed call inside a non-CRD template compiles", func(t *testing.T) {
		t.Parallel()
		g := generator.NewGraph("g",
			generator.WithDef("rgdSchema", rgdSchemaDef),
			generator.WithTemplate("cm", map[string]any{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]any{"name": "schema-${rgdSchema.kind.lowerAscii()}"},
				"data": map[string]any{
					"openapi":         "${json.marshal(simpleschema.toOpenAPI(rgdSchema))}",
					"propertyCount":   "${string(size(simpleschema.toOpenAPI(rgdSchema).properties.spec.properties))}",
					"literalArgument": "${json.marshal(simpleschema.toOpenAPI({'spec': {'name': 'string'}}))}",
				},
			}),
		)
		prog, err := newTestCompiler(t).Compile(g)
		require.NoError(t, err)
		require.NotNil(t, prog)
		assert.Equal(t, []string{"rgdSchema"}, prog.Nodes["cm"].HardDepIDs(),
			"the def feeding the conversion must be a hard dependency")
		assert.Equal(t, []string{"rgdSchema", "cm"}, prog.TopologicalOrder)
	})

	t.Run("wrong arity is rejected at compile time", func(t *testing.T) {
		t.Parallel()
		g := generator.NewGraph("g",
			generator.WithDef("rgdSchema", rgdSchemaDef),
			generator.WithTemplate("cm", map[string]any{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]any{"name": "schema"},
				"data": map[string]any{
					// Two arguments: the function takes the whole schema block.
					"openapi": "${json.marshal(simpleschema.toOpenAPI(rgdSchema.spec, rgdSchema.types))}",
				},
			}),
		)
		_, err := newTestCompiler(t).Compile(g)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "found no matching overload")
	})
}
