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

	"github.com/google/cel-go/cel"
	krocel "github.com/kubernetes-sigs/kro/pkg/cel"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiservercel "k8s.io/apiserver/pkg/cel"
	"k8s.io/apiserver/pkg/cel/openapi"
	"k8s.io/kube-openapi/pkg/validation/spec"
	"k8s.io/utils/ptr"
)

func TestInferSchemaFromCELType_Primitives(t *testing.T) {
	tests := []struct {
		name    string
		celType *cel.Type
		want    *extv1.JSONSchemaProps
	}{
		{
			name:    "convert bool",
			celType: cel.BoolType,
			want:    &extv1.JSONSchemaProps{Type: "boolean"},
		},
		{
			name:    "convert int",
			celType: cel.IntType,
			want:    &extv1.JSONSchemaProps{Type: "integer"},
		},
		{
			name:    "convert uint",
			celType: cel.UintType,
			want:    &extv1.JSONSchemaProps{Type: "integer"},
		},
		{
			name:    "convert double",
			celType: cel.DoubleType,
			want:    &extv1.JSONSchemaProps{Type: "number"},
		},
		{
			name:    "convert string",
			celType: cel.StringType,
			want:    &extv1.JSONSchemaProps{Type: "string"},
		},
		{
			name:    "convert bytes",
			celType: cel.BytesType,
			want:    &extv1.JSONSchemaProps{Type: "string", Format: "byte"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := inferSchemaFromCELType(tt.celType, nil)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestInferSchemaFromCELType_Collections(t *testing.T) {
	tests := []struct {
		name    string
		celType *cel.Type
		want    *extv1.JSONSchemaProps
	}{
		{
			name:    "list of strings",
			celType: cel.ListType(cel.StringType),
			want: &extv1.JSONSchemaProps{
				Type: "array",
				Items: &extv1.JSONSchemaPropsOrArray{
					Schema: &extv1.JSONSchemaProps{Type: "string"},
				},
			},
		},
		{
			name:    "list of ints",
			celType: cel.ListType(cel.IntType),
			want: &extv1.JSONSchemaProps{
				Type: "array",
				Items: &extv1.JSONSchemaPropsOrArray{
					Schema: &extv1.JSONSchemaProps{Type: "integer"},
				},
			},
		},
		{
			name:    "map of string to int",
			celType: cel.MapType(cel.StringType, cel.IntType),
			want: &extv1.JSONSchemaProps{
				Type: "object",
				AdditionalProperties: &extv1.JSONSchemaPropsOrBool{
					Schema: &extv1.JSONSchemaProps{Type: "integer"},
				},
			},
		},
		{
			name:    "map of string to string",
			celType: cel.MapType(cel.StringType, cel.StringType),
			want: &extv1.JSONSchemaProps{
				Type: "object",
				AdditionalProperties: &extv1.JSONSchemaPropsOrBool{
					Schema: &extv1.JSONSchemaProps{Type: "string"},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := inferSchemaFromCELType(tt.celType, nil)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
func TestGenerateSchemaFromCELTypes_Timestamp(t *testing.T) {
	typeMap := map[string]*cel.Type{
		"ts": cel.TimestampType,
	}

	result, err := GenerateSchemaFromCELTypes(typeMap, nil)
	require.NoError(t, err)
	require.NotNil(t, result)

	prop, ok := result.Properties["ts"]
	require.True(t, ok)

	assert.Equal(t, "string", prop.Type)
	assert.Equal(t, "date-time", prop.Format)
	assert.Equal(t, "Timestamp representing a creation time", prop.Description)
}

func TestGenerateSchemaFromCELTypes_Complex(t *testing.T) {
	addressFields := map[string]*apiservercel.DeclField{
		"street": apiservercel.NewDeclField("street", apiservercel.StringType, true, nil, nil),
		"city":   apiservercel.NewDeclField("city", apiservercel.StringType, true, nil, nil),
		"zip":    apiservercel.NewDeclField("zip", apiservercel.IntType, false, nil, nil),
	}
	addressType := apiservercel.NewObjectType("Address", addressFields)

	userFields := map[string]*apiservercel.DeclField{
		"name":     apiservercel.NewDeclField("name", apiservercel.StringType, true, nil, nil),
		"age":      apiservercel.NewDeclField("age", apiservercel.IntType, true, nil, nil),
		"active":   apiservercel.NewDeclField("active", apiservercel.BoolType, false, nil, nil),
		"score":    apiservercel.NewDeclField("score", apiservercel.DoubleType, false, nil, nil),
		"tags":     apiservercel.NewDeclField("tags", apiservercel.NewListType(apiservercel.StringType, -1), false, nil, nil),
		"metadata": apiservercel.NewDeclField("metadata", apiservercel.NewMapType(apiservercel.StringType, apiservercel.StringType, -1), false, nil, nil),
		"counts":   apiservercel.NewDeclField("counts", apiservercel.NewMapType(apiservercel.StringType, apiservercel.IntType, -1), false, nil, nil),
		"address":  apiservercel.NewDeclField("address", addressType, false, nil, nil),
		"extra":    apiservercel.NewDeclField("extra", apiservercel.DynType, false, nil, nil),
	}
	userType := apiservercel.NewObjectType("User", userFields)
	provider := krocel.NewDeclTypeProvider(userType, addressType)

	typeMap := map[string]*cel.Type{
		"user": userType.CelType(),
	}

	result, err := GenerateSchemaFromCELTypes(typeMap, provider)
	require.NoError(t, err)
	require.NotNil(t, result)

	expected := &extv1.JSONSchemaProps{
		Type: "object",
		Properties: map[string]extv1.JSONSchemaProps{
			"user": {
				Type: "object",
				Properties: map[string]extv1.JSONSchemaProps{
					"name":   {Type: "string"},
					"age":    {Type: "integer"},
					"active": {Type: "boolean"},
					"score":  {Type: "number"},
					"tags": {
						Type: "array",
						Items: &extv1.JSONSchemaPropsOrArray{
							Schema: &extv1.JSONSchemaProps{Type: "string"},
						},
					},
					"metadata": {
						Type: "object",
						AdditionalProperties: &extv1.JSONSchemaPropsOrBool{
							Schema: &extv1.JSONSchemaProps{Type: "string"},
						},
					},
					"counts": {
						Type: "object",
						AdditionalProperties: &extv1.JSONSchemaPropsOrBool{
							Schema: &extv1.JSONSchemaProps{Type: "integer"},
						},
					},
					"address": {
						Type: "object",
						Properties: map[string]extv1.JSONSchemaProps{
							"street": {Type: "string"},
							"city":   {Type: "string"},
							"zip":    {Type: "integer"},
						},
					},
					"extra": {
						XPreserveUnknownFields: ptr.To(true),
					},
				},
			},
		},
	}

	assert.Equal(t, expected, result)
}

func TestExtractSchemaFromDeclTypeWithCycleDetection(t *testing.T) {
	tests := []struct {
		name          string
		setupDeclType func() *apiservercel.DeclType
		preVisited    func(*apiservercel.DeclType) map[string]bool
		wantErr       bool
		wantErrMsg    string
		validate      func(*testing.T, *extv1.JSONSchemaProps)
	}{
		{
			name: "no cycle - simple object",
			setupDeclType: func() *apiservercel.DeclType {
				schema := &spec.Schema{}
				schema.Type = []string{"object"}
				schema.Properties = map[string]spec.Schema{
					"name":  {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
					"value": {SchemaProps: spec.SchemaProps{Type: []string{"integer"}}},
				}
				return openapi.SchemaDeclType(schema, false).MaybeAssignTypeName("SimpleType")
			},
			preVisited: func(*apiservercel.DeclType) map[string]bool {
				return make(map[string]bool)
			},
			wantErr: false,
			validate: func(t *testing.T, result *extv1.JSONSchemaProps) {
				assert.Equal(t, "object", result.Type)
				assert.NotNil(t, result.Properties)

				nameProp, hasName := result.Properties["name"]
				assert.True(t, hasName)
				assert.Equal(t, "string", nameProp.Type)

				valueProp, hasValue := result.Properties["value"]
				assert.True(t, hasValue)
				assert.Equal(t, "integer", valueProp.Type)
			},
		},
		{
			name: "cycle detected - error out",
			setupDeclType: func() *apiservercel.DeclType {
				schema := &spec.Schema{}
				schema.Type = []string{"object"}
				schema.Properties = map[string]spec.Schema{
					"name": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
				}
				return openapi.SchemaDeclType(schema, false).MaybeAssignTypeName("CyclicType")
			},
			preVisited: func(dt *apiservercel.DeclType) map[string]bool {
				// Pre-mark the type as visited to simulate a cycle
				visited := make(map[string]bool)
				typeName := dt.CelType().String()
				visited[typeName] = true
				return visited
			},
			wantErr:    true,
			wantErrMsg: "cyclic type reference detected",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			declType := tt.setupDeclType()
			visited := tt.preVisited(declType)

			result, err := extractSchemaFromDeclTypeWithCycleDetection(declType, visited)

			if tt.wantErr {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErrMsg)
				assert.Nil(t, result)
			} else {
				assert.NoError(t, err)
				assert.NotNil(t, result)
				if tt.validate != nil {
					tt.validate(t, result)
				}
			}
		})
	}
}
