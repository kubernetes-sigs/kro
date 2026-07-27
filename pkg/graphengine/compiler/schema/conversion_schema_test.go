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

package schema

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/kube-openapi/pkg/validation/spec"

	krocel "github.com/kubernetes-sigs/kro/pkg/graphengine/cel"
)

// TestConvertJSONSchemaPropsToSpecSchema walks the major branches of the
// extv1 -> spec.Schema converter: nil input, scalar metadata carry-over, the
// two Items shapes, the allOf/oneOf/anyOf/not combinators, nested properties,
// both additionalProperties forms, and the x-kubernetes-preserve-unknown-fields
// extension.
func TestConvertJSONSchemaPropsToSpecSchema(t *testing.T) {
	preserve := true

	tests := []struct {
		name  string
		props *extv1.JSONSchemaProps
	}{
		{
			name:  "nil props returns nil",
			props: nil,
		},
		{
			name: "scalar with metadata",
			props: &extv1.JSONSchemaProps{
				Type:        "string",
				Description: "a field",
				Pattern:     "^x$",
				Required:    []string{"foo"},
			},
		},
		{
			name: "items single schema",
			props: &extv1.JSONSchemaProps{
				Type: "array",
				Items: &extv1.JSONSchemaPropsOrArray{
					Schema: &extv1.JSONSchemaProps{Type: "string"},
				},
			},
		},
		{
			name: "items tuple schemas",
			props: &extv1.JSONSchemaProps{
				Type: "array",
				Items: &extv1.JSONSchemaPropsOrArray{
					JSONSchemas: []extv1.JSONSchemaProps{
						{Type: "string"},
						{Type: "integer"},
					},
				},
			},
		},
		{
			name: "allOf oneOf anyOf not combinators",
			props: &extv1.JSONSchemaProps{
				AllOf: []extv1.JSONSchemaProps{{Type: "string"}},
				OneOf: []extv1.JSONSchemaProps{{Type: "integer"}},
				AnyOf: []extv1.JSONSchemaProps{{Type: "boolean"}},
				Not:   &extv1.JSONSchemaProps{Type: "number"},
			},
		},
		{
			name: "nested properties",
			props: &extv1.JSONSchemaProps{
				Type: "object",
				Properties: map[string]extv1.JSONSchemaProps{
					"name": {Type: "string"},
				},
			},
		},
		{
			name: "additionalProperties schema",
			props: &extv1.JSONSchemaProps{
				Type: "object",
				AdditionalProperties: &extv1.JSONSchemaPropsOrBool{
					Schema: &extv1.JSONSchemaProps{Type: "string"},
				},
			},
		},
		{
			name: "additionalProperties allows",
			props: &extv1.JSONSchemaProps{
				Type: "object",
				AdditionalProperties: &extv1.JSONSchemaPropsOrBool{
					Allows: true,
				},
			},
		},
		{
			name: "preserve unknown fields extension",
			props: &extv1.JSONSchemaProps{
				Type:                   "object",
				XPreserveUnknownFields: &preserve,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ConvertJSONSchemaPropsToSpecSchema(tc.props)
			require.NoError(t, err)

			if tc.props == nil {
				assert.Nil(t, got)
				return
			}
			require.NotNil(t, got)

			switch tc.name {
			case "scalar with metadata":
				assert.Equal(t, spec.StringOrArray{"string"}, got.Type)
				assert.Equal(t, "a field", got.Description)
				assert.Equal(t, "^x$", got.Pattern)
				assert.Equal(t, []string{"foo"}, got.Required)
			case "items single schema":
				require.NotNil(t, got.Items)
				require.NotNil(t, got.Items.Schema)
				assert.Equal(t, spec.StringOrArray{"string"}, got.Items.Schema.Type)
			case "items tuple schemas":
				require.NotNil(t, got.Items)
				require.Len(t, got.Items.Schemas, 2)
				assert.Equal(t, spec.StringOrArray{"string"}, got.Items.Schemas[0].Type)
				assert.Equal(t, spec.StringOrArray{"integer"}, got.Items.Schemas[1].Type)
			case "allOf oneOf anyOf not combinators":
				require.Len(t, got.AllOf, 1)
				require.Len(t, got.OneOf, 1)
				require.Len(t, got.AnyOf, 1)
				require.NotNil(t, got.Not)
				assert.Equal(t, spec.StringOrArray{"number"}, got.Not.Type)
			case "nested properties":
				prop, ok := got.Properties["name"]
				require.True(t, ok)
				assert.Equal(t, spec.StringOrArray{"string"}, prop.Type)
			case "additionalProperties schema":
				require.NotNil(t, got.AdditionalProperties)
				require.NotNil(t, got.AdditionalProperties.Schema)
				assert.Equal(t, spec.StringOrArray{"string"}, got.AdditionalProperties.Schema.Type)
			case "additionalProperties allows":
				require.NotNil(t, got.AdditionalProperties)
				assert.True(t, got.AdditionalProperties.Allows)
				assert.Nil(t, got.AdditionalProperties.Schema)
			case "preserve unknown fields extension":
				v, ok := got.Extensions[krocel.XKubernetesPreserveUnknownFields]
				require.True(t, ok)
				assert.Equal(t, true, v)
			}
		})
	}
}
