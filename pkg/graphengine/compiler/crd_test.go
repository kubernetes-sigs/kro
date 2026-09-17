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
	memory "k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/restmapper"

	"github.com/kubernetes-sigs/kro/pkg/graphengine/testutil/generator"
	testk8s "github.com/kubernetes-sigs/kro/pkg/testutil/k8s"
)

// newRealCRDSchemaCompiler returns a test compiler whose resolver serves the
// real apiextensions CustomResourceDefinition schema (spec.versions and the
// recursive JSONSchemaProps under openAPIV3Schema) instead of the minimal
// hand-written one, so CRD templates are type-checked against the shape a
// live cluster reports.
func newRealCRDSchemaCompiler(t *testing.T) *Compiler {
	t.Helper()
	resolver, disco, err := testk8s.NewFakeResolverWithRealCRDSchema()
	require.NoError(t, err)
	rm := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(disco))
	return &Compiler{schemaResolver: resolver, restMapper: rm}
}

// crdTemplate builds a CustomResourceDefinition template whose
// versions[0].schema.openAPIV3Schema is set to schema (a static map or a CEL
// expression string).
func crdTemplate(name string, openAPIV3Schema any, overrides map[string]any) map[string]any {
	tmpl := map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition",
		"metadata": map[string]any{"name": name},
		"spec": map[string]any{
			"group": "example.com",
			"names": map[string]any{"kind": "App", "plural": "apps"},
			"scope": "Namespaced",
			"versions": []any{map[string]any{
				"name": "v1alpha1", "served": true, "storage": true,
				"schema": map[string]any{"openAPIV3Schema": openAPIV3Schema},
			}},
		},
	}
	for k, v := range overrides {
		tmpl["spec"].(map[string]any)[k] = v
	}
	return tmpl
}

// TestCompile_CRDTemplateExpressions covers CEL expressions in
// CustomResourceDefinition templates. CRDs are parsed schemalessly because the
// typed parser cannot walk the recursive JSONSchemaProps schema; the compiler
// used to additionally reject any expression outside metadata.*, which made it
// impossible for a Graph to synthesize a Kind's openAPIV3Schema. Expressions are
// now accepted anywhere and still type-checked against the real CRD schema where
// the path resolves.
func TestCompile_CRDTemplateExpressions(t *testing.T) {
	t.Parallel()

	rgdSchemaDef := map[string]any{
		"kind": "App", "group": "example.com", "version": "v1alpha1",
	}
	// A heterogeneous map literal is map(string, dyn) and therefore assignable
	// to the JSONSchemaProps-typed openAPIV3Schema field.
	const specSchema = `{"type": "object", "required": ["name"], "properties": {"name": {"type": "string"}}}`

	t.Run("openAPIV3Schema from a CEL expression compiles against the real CRD schema", func(t *testing.T) {
		t.Parallel()
		g := generator.NewGraph("g",
			generator.WithDef("rgdSchema", rgdSchemaDef),
			generator.WithTemplate("crd", crdTemplate(
				"${rgdSchema.kind.lowerAscii() + 's.' + rgdSchema.group}",
				`${{"type": "object", "properties": {"apiVersion": {"type": "string"}, "kind": {"type": "string"}, "metadata": {"type": "object"}, "spec": `+specSchema+`}}}`,
				map[string]any{
					"group": "${rgdSchema.group}",
					"names": map[string]any{"kind": "${rgdSchema.kind}", "plural": "${rgdSchema.kind.lowerAscii() + 's'}"},
				},
			)),
		)
		prog, err := newRealCRDSchemaCompiler(t).Compile(g)
		require.NoError(t, err)
		crd := prog.Nodes["crd"]
		assert.Equal(t, []string{"rgdSchema"}, crd.HardDepIDs())
		paths := make([]string, 0, len(crd.Variables))
		for _, v := range crd.Variables {
			paths = append(paths, v.Path)
		}
		assert.ElementsMatch(t, []string{
			"metadata.name", "spec.group", "spec.names.kind", "spec.names.plural",
			"spec.versions[0].schema.openAPIV3Schema",
		}, paths, "every expression in the CRD template must be extracted, not just metadata.*")
	})

	t.Run("expression nested inside a static openAPIV3Schema compiles", func(t *testing.T) {
		t.Parallel()
		g := generator.NewGraph("g",
			generator.WithDef("rgdSchema", rgdSchemaDef),
			generator.WithTemplate("crd", crdTemplate("apps.example.com", map[string]any{
				"type": "object",
				"properties": map[string]any{
					"apiVersion": map[string]any{"type": "string"},
					"kind":       map[string]any{"type": "string"},
					"metadata":   map[string]any{"type": "object"},
					"spec":       "${" + specSchema + "}",
					"status":     map[string]any{"type": "object", "x-kubernetes-preserve-unknown-fields": true},
				},
			}, nil)),
		)
		prog, err := newRealCRDSchemaCompiler(t).Compile(g)
		require.NoError(t, err)
		require.Len(t, prog.Nodes["crd"].Variables, 1)
		assert.Equal(t, "spec.versions[0].schema.openAPIV3Schema.properties.spec", prog.Nodes["crd"].Variables[0].Path)
	})

	t.Run("non-metadata expressions are still type-checked against the CRD schema", func(t *testing.T) {
		t.Parallel()
		g := generator.NewGraph("g",
			generator.WithDef("rgdSchema", rgdSchemaDef),
			generator.WithTemplate("crd", crdTemplate("apps.example.com",
				map[string]any{"type": "object", "x-kubernetes-preserve-unknown-fields": true},
				map[string]any{"group": "${1 + 1}"},
			)),
		)
		_, err := newRealCRDSchemaCompiler(t).Compile(g)
		require.Error(t, err)
		assert.Contains(t, err.Error(), `type mismatch in node "crd" at path "spec.group"`)
		assert.Contains(t, err.Error(), `returns "int" but expected "string"`)
	})

	t.Run("homogeneous map literal is rejected against the JSONSchemaProps type", func(t *testing.T) {
		// A CEL map literal whose values are all strings has type
		// map(string, string); JSONSchemaProps has non-string fields, so the
		// structural check refuses it. Authors must produce a dyn-valued map
		// (heterogeneous literal or dyn(...)). Pinned so the documentation's
		// guidance stays accurate.
		t.Parallel()
		g := generator.NewGraph("g",
			generator.WithDef("rgdSchema", rgdSchemaDef),
			generator.WithTemplate("crd", crdTemplate("apps.example.com", `${{"type": "object"}}`, nil)),
		)
		_, err := newRealCRDSchemaCompiler(t).Compile(g)
		require.Error(t, err)
		assert.Contains(t, err.Error(), `returns "map(string, string)"`)

		g = generator.NewGraph("g",
			generator.WithDef("rgdSchema", rgdSchemaDef),
			generator.WithTemplate("crd", crdTemplate("apps.example.com", `${dyn({"type": "object"})}`, nil)),
		)
		_, err = newRealCRDSchemaCompiler(t).Compile(g)
		require.NoError(t, err, "wrapping the literal in dyn() must be accepted")
	})

	t.Run("backwards compatible: metadata-only and static CRD templates compile unchanged", func(t *testing.T) {
		t.Parallel()
		for _, c := range []*Compiler{newTestCompiler(t), newRealCRDSchemaCompiler(t)} {
			g := generator.NewGraph("g",
				generator.WithDef("rgdSchema", rgdSchemaDef),
				generator.WithTemplate("dynamicName", crdTemplate("${rgdSchema.kind.lowerAscii() + 's.' + rgdSchema.group}",
					map[string]any{"type": "object", "x-kubernetes-preserve-unknown-fields": true}, nil)),
				generator.WithTemplate("static", crdTemplate("widgets.example.com",
					map[string]any{"type": "object", "x-kubernetes-preserve-unknown-fields": true},
					map[string]any{"names": map[string]any{"kind": "Widget", "plural": "widgets"}})),
			)
			prog, err := c.Compile(g)
			require.NoError(t, err)
			require.Len(t, prog.Nodes["dynamicName"].Variables, 1)
			assert.Equal(t, "metadata.name", prog.Nodes["dynamicName"].Variables[0].Path)
			assert.Equal(t, []string{"rgdSchema"}, prog.Nodes["dynamicName"].HardDepIDs())
			assert.Empty(t, prog.Nodes["static"].Variables)
			assert.Empty(t, prog.Nodes["static"].HardDepIDs())
		}
	})

	t.Run("malformed expression inside a CRD is still a parse error", func(t *testing.T) {
		t.Parallel()
		g := generator.NewGraph("g",
			generator.WithTemplate("crd", crdTemplate("apps.example.com",
				map[string]any{"type": "object", "x-kubernetes-preserve-unknown-fields": true},
				map[string]any{"group": "${outer(${inner})}"},
			)),
		)
		_, err := newRealCRDSchemaCompiler(t).Compile(g)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "parse template payload")
	})
}
