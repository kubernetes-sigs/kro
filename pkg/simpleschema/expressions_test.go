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

package simpleschema

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// An RGD-style status block mixes SimpleSchema types with CEL expressions. The
// expressions' types depend on the resources they reference, which a converter
// without a schema resolver cannot know; with AllowExpressionFields they must
// become untyped (preserve-unknown-fields) properties while everything else is
// converted exactly as in a spec block.
func TestToOpenAPISpecAllowExpressionFields(t *testing.T) {
	untyped := extv1.JSONSchemaProps{XPreserveUnknownFields: new(true)}

	status := map[string]any{
		"items":       "integer",
		"conditions":  "[]Condition",
		"ready":       "${deployment.status.readyReplicas > 0}",
		"url":         "http://${service.spec.clusterIP}:8080",
		"withPipes":   "${a || b}",
		"kroList":     []any{"${runtime.newCondition({type: 'Ready'})}", "${runtime.newCondition({type: 'Other'})}"},
		"nested":      map[string]any{"count": "integer", "phase": "${deployment.status.phase}"},
		"requiredTag": "string | required=true",
		// "${" inside a marker value is not an expression: the type position
		// holds a real type, so the field stays typed and keeps its marker.
		"literalDefault": `string | default="${x}"`,
		"described":      `[]string | description="set from ${VAR}"`,
	}
	customTypes := map[string]any{
		"Condition": map[string]any{
			"type":   "string | required=true",
			"status": "string | required=true",
		},
	}

	got, err := ToOpenAPISpec(status, customTypes, AllowExpressionFields())
	require.NoError(t, err)

	assert.Equal(t, "object", got.Type)
	assert.Equal(t, []string{"requiredTag"}, got.Required)
	assert.Equal(t, extv1.JSONSchemaProps{Type: "integer"}, got.Properties["items"])
	assert.Equal(t, extv1.JSONSchemaProps{Type: "string"}, got.Properties["requiredTag"])
	assert.Equal(t, "array", got.Properties["conditions"].Type, "custom types resolve in status blocks too")
	assert.Equal(t, []string{"status", "type"}, got.Properties["conditions"].Items.Schema.Required)

	for _, field := range []string{"ready", "url", "withPipes", "kroList"} {
		assert.Equal(t, untyped, got.Properties[field], "field %q holds an expression and must be untyped", field)
	}
	nested := got.Properties["nested"]
	assert.Equal(t, "object", nested.Type)
	assert.Equal(t, extv1.JSONSchemaProps{Type: "integer"}, nested.Properties["count"])
	assert.Equal(t, untyped, nested.Properties["phase"], "expression handling must apply at every nesting level")

	assert.Equal(t, extv1.JSONSchemaProps{Type: "string", Default: &extv1.JSON{Raw: []byte(`"${x}"`)}}, got.Properties["literalDefault"])
	assert.Equal(t, "array", got.Properties["described"].Type)
	assert.Equal(t, "set from ${VAR}", got.Properties["described"].Description)
}

func TestToOpenAPISpecAllowExpressionFieldsRejectsOtherLists(t *testing.T) {
	for name, value := range map[string]any{
		"list of type names":     []any{"string", "integer"},
		"list with a non-string": []any{"${a}", 1},
		"empty list":             []any{},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ToOpenAPISpec(map[string]any{"f": value}, nil, AllowExpressionFields())
			require.Error(t, err)
			assert.Contains(t, err.Error(), "field f: a list value must contain only CEL expressions")
		})
	}
}

// Without the option the behaviour is unchanged: an expression is not a type.
func TestToOpenAPISpecRejectsExpressionsByDefault(t *testing.T) {
	_, err := ToOpenAPISpec(map[string]any{"ready": "${deployment.status.readyReplicas > 0}"}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "field ready:")

	_, err = ToOpenAPISpec(map[string]any{"conditions": []any{"${runtime.newCondition({type: 'Ready'})}"}}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "field conditions: unexpected type: []interface {}")
}

// The option must not leak into custom type definitions: a "${" inside a type
// can only be a mistake, and types are shared with the strictly-parsed spec.
func TestToOpenAPISpecAllowExpressionFieldsKeepsCustomTypesStrict(t *testing.T) {
	customTypes := map[string]any{
		"Owner": map[string]any{"team": "${schema.spec.team}"},
	}
	_, err := ToOpenAPISpec(map[string]any{"owner": "Owner"}, customTypes, AllowExpressionFields())
	require.Error(t, err)
	// The expression is read as a type name: the dependency DAG rejects it as an
	// unknown type before any schema is built.
	assert.Contains(t, err.Error(), "${schema.spec.team}")
}
