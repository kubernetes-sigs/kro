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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiservercel "k8s.io/apiserver/pkg/cel"
)

// TestPrimitiveTypeToSchema covers the full primitive name switch, including
// bytes, null_type, and the unknown fallthrough.
func TestPrimitiveTypeToSchema(t *testing.T) {
	tests := []struct {
		name     string
		typeName string
		want     *extv1.JSONSchemaProps
		wantOK   bool
	}{
		{name: "bool", typeName: "bool", want: &extv1.JSONSchemaProps{Type: "boolean"}, wantOK: true},
		{name: "int", typeName: "int", want: &extv1.JSONSchemaProps{Type: "integer"}, wantOK: true},
		{name: "uint", typeName: "uint", want: &extv1.JSONSchemaProps{Type: "integer"}, wantOK: true},
		{name: "double", typeName: "double", want: &extv1.JSONSchemaProps{Type: "number"}, wantOK: true},
		{name: "string", typeName: "string", want: &extv1.JSONSchemaProps{Type: "string"}, wantOK: true},
		{name: "bytes", typeName: "bytes", want: &extv1.JSONSchemaProps{Type: "string", Format: "byte"}, wantOK: true},
		{
			name:     "null_type",
			typeName: "null_type",
			want: &extv1.JSONSchemaProps{
				Description:            "null type - any value allowed",
				XPreserveUnknownFields: new(true),
			},
			wantOK: true,
		},
		{name: "unknown", typeName: "widget", want: nil, wantOK: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := primitiveTypeToSchema(tc.typeName)
			assert.Equal(t, tc.wantOK, ok)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestInferSchemaFromCELType_Fallbacks drives the branches reached when no
// DeclTypeProvider is supplied (nil provider): collections fall back to their
// CEL type parameters, parameterless / dyn / unknown kinds get permissive
// schemas, and a nil type errors.
func TestInferSchemaFromCELType_Fallbacks(t *testing.T) {
	tests := []struct {
		name    string
		celType *cel.Type
		want    *extv1.JSONSchemaProps
		wantErr bool
	}{
		{name: "nil type errors", celType: nil, wantErr: true},
		{
			name:    "list fallback to element param",
			celType: cel.ListType(cel.BoolType),
			want: &extv1.JSONSchemaProps{
				Type: "array",
				Items: &extv1.JSONSchemaPropsOrArray{
					Schema: &extv1.JSONSchemaProps{Type: "boolean"},
				},
			},
		},
		{
			name:    "map fallback to value param",
			celType: cel.MapType(cel.StringType, cel.BoolType),
			want: &extv1.JSONSchemaProps{
				Type: "object",
				AdditionalProperties: &extv1.JSONSchemaPropsOrBool{
					Schema: &extv1.JSONSchemaProps{Type: "boolean"},
				},
			},
		},
		{
			name:    "dyn kind permissive",
			celType: cel.DynType,
			want:    &extv1.JSONSchemaProps{XPreserveUnknownFields: new(true)},
		},
		{
			name: "struct kind fallback permissive object",
			celType: apiservercel.NewObjectType("FallbackStruct", map[string]*apiservercel.DeclField{
				"a": apiservercel.NewDeclField("a", apiservercel.StringType, true, nil, nil),
			}).CelType(),
			want: &extv1.JSONSchemaProps{
				Type:                 "object",
				AdditionalProperties: &extv1.JSONSchemaPropsOrBool{Allows: true},
			},
		},
		{
			name:    "timestamp kind",
			celType: cel.TimestampType,
			want: &extv1.JSONSchemaProps{
				Description: "Timestamp representing a creation time",
				Type:        "string",
				Format:      "date-time",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := inferSchemaFromCELType(tc.celType, nil)
			if tc.wantErr {
				require.Error(t, err)
				assert.Nil(t, got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestGenerateSchemaFromCELTypes_Error confirms that a nil type in the map
// surfaces a wrapped error naming the offending path.
func TestGenerateSchemaFromCELTypes_Error(t *testing.T) {
	tests := []struct {
		name    string
		typeMap map[string]*cel.Type
		wantErr bool
	}{
		{
			name:    "nil type errors",
			typeMap: map[string]*cel.Type{"bad": nil},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := GenerateSchemaFromCELTypes(tc.typeMap, nil)
			if tc.wantErr {
				require.Error(t, err)
				assert.Nil(t, got)
				return
			}
			require.NoError(t, err)
		})
	}
}

// TestExtractSchemaFromDeclType_EdgeCases covers the non-object branches of
// the DeclType walker: a nil DeclType yields a permissive schema, a list
// DeclType wraps its element, a map DeclType becomes an object with
// additionalProperties, and a scalar (non-object) DeclType maps to its
// primitive schema.
func TestExtractSchemaFromDeclType_EdgeCases(t *testing.T) {
	tests := []struct {
		name     string
		declType *apiservercel.DeclType
		validate func(t *testing.T, got *extv1.JSONSchemaProps)
	}{
		{
			name:     "nil decl type permissive",
			declType: nil,
			validate: func(t *testing.T, got *extv1.JSONSchemaProps) {
				require.NotNil(t, got.XPreserveUnknownFields)
				assert.True(t, *got.XPreserveUnknownFields)
			},
		},
		{
			name:     "list decl type wraps element",
			declType: apiservercel.NewListType(apiservercel.StringType, -1),
			validate: func(t *testing.T, got *extv1.JSONSchemaProps) {
				assert.Equal(t, "array", got.Type)
				require.NotNil(t, got.Items)
				require.NotNil(t, got.Items.Schema)
				assert.Equal(t, "string", got.Items.Schema.Type)
			},
		},
		{
			name:     "map decl type becomes object",
			declType: apiservercel.NewMapType(apiservercel.StringType, apiservercel.IntType, -1),
			validate: func(t *testing.T, got *extv1.JSONSchemaProps) {
				assert.Equal(t, "object", got.Type)
				require.NotNil(t, got.AdditionalProperties)
				require.NotNil(t, got.AdditionalProperties.Schema)
				assert.Equal(t, "integer", got.AdditionalProperties.Schema.Type)
			},
		},
		{
			name:     "scalar decl type maps to primitive",
			declType: apiservercel.StringType,
			validate: func(t *testing.T, got *extv1.JSONSchemaProps) {
				assert.Equal(t, "string", got.Type)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := extractSchemaFromDeclTypeWithCycleDetection(tc.declType, make(map[string]bool))
			require.NoError(t, err)
			require.NotNil(t, got)
			tc.validate(t, got)
		})
	}
}

// TestExtractSchemaFromDeclType_NestedFieldError ensures that an error
// surfacing while walking a struct field (here a self-referential cycle)
// propagates up wrapped with the field name.
func TestExtractSchemaFromDeclType_NestedFieldError(t *testing.T) {
	selfField := apiservercel.NewDeclField("self", nil, false, nil, nil)
	selfType := apiservercel.NewObjectType("SelfRef", map[string]*apiservercel.DeclField{
		"self": selfField,
	})
	// Point the field's type back at the enclosing object to create a cycle.
	selfField.Type = selfType

	_, err := extractSchemaFromDeclTypeWithCycleDetection(selfType, make(map[string]bool))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "self")
}

// TestExtractSchemaFromDeclType_NilFieldSkipped verifies a nil entry in an
// object's field map is skipped rather than dereferenced.
func TestExtractSchemaFromDeclType_NilFieldSkipped(t *testing.T) {
	objType := apiservercel.NewObjectType("WithNilField", map[string]*apiservercel.DeclField{
		"ok":  apiservercel.NewDeclField("ok", apiservercel.StringType, true, nil, nil),
		"nil": nil,
	})

	got, err := extractSchemaFromDeclTypeWithCycleDetection(objType, make(map[string]bool))
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "object", got.Type)
	_, hasOK := got.Properties["ok"]
	assert.True(t, hasOK)
	_, hasNil := got.Properties["nil"]
	assert.False(t, hasNil)
}
