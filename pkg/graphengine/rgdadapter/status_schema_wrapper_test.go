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

	"github.com/google/cel-go/common/types/ref"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apiserver/pkg/cel/openapi"
	"k8s.io/kube-openapi/pkg/validation/spec"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	krocel "github.com/kubernetes-sigs/kro/pkg/cel"
	celunstructured "github.com/kubernetes-sigs/kro/pkg/cel/unstructured"
)

// fmtSchema is a helper: an object property of the given type/format.
func fmtSchema(typ, format string) spec.Schema {
	return spec.Schema{SchemaProps: spec.SchemaProps{Type: []string{typ}, Format: format}}
}

// schemaVarWith builds the `schema` instance schema used to wrap the scope
// value: an object with spec.{token: byte, createdAt: date-time} plus an
// open metadata and status object.
func schemaVarWith() *spec.Schema {
	return &spec.Schema{SchemaProps: spec.SchemaProps{
		Type: []string{"object"},
		Properties: map[string]spec.Schema{
			"spec": {SchemaProps: spec.SchemaProps{
				Type: []string{"object"},
				Properties: map[string]spec.Schema{
					"token":     fmtSchema("string", "byte"),
					"createdAt": fmtSchema("string", "date-time"),
				},
			}},
			"metadata": {SchemaProps: spec.SchemaProps{
				Type:                 []string{"object"},
				AdditionalProperties: &spec.SchemaOrBool{Allows: true},
			}},
			"status": {SchemaProps: spec.SchemaProps{
				Type:                 []string{"object"},
				AdditionalProperties: &spec.SchemaOrBool{Allows: true},
			}},
		},
	}}
}

// schemaWithBuiltinConditions previously lost the schema-aware wrapper: it
// asserted the scope value was a bare map[string]any, which fails on the
// ref.Val produced by celunstructured.UnstructuredToVal, dropped spec/metadata,
// and returned an untyped map so schema.* byte/date-time fields lost their CEL
// types. This test proves the overlay now preserves both the spec/metadata AND
// the schema-derived typing.
func TestSchemaWithBuiltinConditions_PreservesSchemaTyping(t *testing.T) {
	t.Parallel()

	obj := map[string]any{
		"spec": map[string]any{
			"token":     "aGVsbG8=", // base64("hello")
			"createdAt": "2024-01-02T03:04:05Z",
		},
		"metadata": map[string]any{"name": "demo"},
		// A pre-existing wire status that must be replaced by the built-ins.
		"status": map[string]any{"conditions": []any{}},
	}

	schemaVar := schemaVarWith()
	// The scope value as seeded by instanceSeedScopeOption: a schema-aware
	// ref.Val, NOT a bare map.
	scopeVal := celunstructured.UnstructuredToVal(obj, &openapi.Schema{Schema: schemaVar})

	builtins := []v1alpha1.Condition{{Type: "Ready", Status: "True"}}

	out := schemaWithBuiltinConditions(scopeVal, builtins, schemaVar)

	// The overlay must return a schema-aware ref.Val (the wrapper survives),
	// not a raw map.
	rv, ok := out.(ref.Val)
	require.True(t, ok, "conditions overlay must preserve the schema-aware ref.Val wrapper")

	// Underlying object still carries spec + metadata (not dropped).
	underlying, ok := rv.Value().(map[string]any)
	require.True(t, ok)
	assert.Contains(t, underlying, "spec", "spec must survive the conditions overlay")
	assert.Contains(t, underlying, "metadata", "metadata must survive the conditions overlay")

	// status is replaced by the built-in conditions.
	status, ok := underlying["status"].(map[string]any)
	require.True(t, ok)
	conds, ok := status["conditions"].([]any)
	require.True(t, ok)
	require.Len(t, conds, 1)
	assert.Equal(t, "Ready", conds[0].(map[string]any)["type"])

	// Schema-derived typing survives: the byte/date-time fields evaluate to
	// their typed CEL values, not raw strings. Prove it end-to-end via
	// compiled CEL expressions against the scope.
	env, err := krocel.DefaultEnvironment(krocel.WithResourceIDs([]string{SchemaNodeID}))
	require.NoError(t, err)
	scope := map[string]any{SchemaNodeID: out}

	// string(schema.spec.token) decodes the bytes; a raw base64 string would
	// return the base64 text unchanged (or fail string(bytes)).
	prog, err := compileCEL(env, "string(schema.spec.token)", 0)
	require.NoError(t, err)
	tokenVal, _, err := prog.Eval(scope)
	require.NoError(t, err)
	assert.Equal(t, "hello", tokenVal.Value(),
		"byte field must decode as bytes, proving typing survived the overlay")

	// A date-time field is a Timestamp: getFullYear works only on timestamps.
	prog, err = compileCEL(env, "schema.spec.createdAt.getFullYear()", 0)
	require.NoError(t, err)
	yearVal, _, err := prog.Eval(scope)
	require.NoError(t, err)
	assert.EqualValues(t, 2024, yearVal.Value(),
		"date-time field must be a Timestamp, proving typing survived the overlay")
}

// TestSchemaWithBuiltinConditions_SchemalessFallback verifies the schemaless
// path (nil schemaVarSchema): the overlay returns a plain map with spec and
// metadata preserved and status replaced.
func TestSchemaWithBuiltinConditions_SchemalessFallback(t *testing.T) {
	t.Parallel()
	obj := map[string]any{
		"spec":     map[string]any{"name": "app"},
		"metadata": map[string]any{"name": "demo"},
	}
	builtins := []v1alpha1.Condition{{Type: "Ready", Status: "True"}}

	out := schemaWithBuiltinConditions(obj, builtins, nil)
	m, ok := out.(map[string]any)
	require.True(t, ok, "schemaless path returns a plain map")
	assert.Contains(t, m, "spec")
	assert.Contains(t, m, "metadata")
	status := m["status"].(map[string]any)
	assert.Len(t, status["conditions"], 1)
}
