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

	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// TestInferSchemaTypeFromGoValue exercises every branch of the Go-value type
// switch, including scalars, the nil "any allowed" case, and the unsupported
// type error.
func TestInferSchemaTypeFromGoValue(t *testing.T) {
	tests := []struct {
		name    string
		value   interface{}
		want    *extv1.JSONSchemaProps
		wantErr bool
	}{
		{name: "bool", value: true, want: &extv1.JSONSchemaProps{Type: "boolean"}},
		{name: "int64", value: int64(5), want: &extv1.JSONSchemaProps{Type: "integer"}},
		{name: "uint64", value: uint64(5), want: &extv1.JSONSchemaProps{Type: "integer"}},
		{name: "float64", value: float64(1.5), want: &extv1.JSONSchemaProps{Type: "number"}},
		{name: "string", value: "x", want: &extv1.JSONSchemaProps{Type: "string"}},
		{
			name:  "nil any allowed",
			value: nil,
			want: &extv1.JSONSchemaProps{
				Description:            "the original schema type was optional or nil so any type is allowed",
				XPreserveUnknownFields: new(true),
			},
		},
		{
			name:  "array of strings",
			value: []interface{}{"a", "b"},
			want: &extv1.JSONSchemaProps{
				Type: "array",
				Items: &extv1.JSONSchemaPropsOrArray{
					Schema: &extv1.JSONSchemaProps{Type: "string"},
				},
			},
		},
		{
			name:  "empty array no items",
			value: []interface{}{},
			want:  &extv1.JSONSchemaProps{Type: "array"},
		},
		{
			name:  "object",
			value: map[string]interface{}{"k": int64(1)},
			want: &extv1.JSONSchemaProps{
				Type: "object",
				Properties: map[string]extv1.JSONSchemaProps{
					"k": {Type: "integer"},
				},
			},
		},
		{
			name:    "unsupported type",
			value:   struct{ X int }{X: 1},
			wantErr: true,
		},
		{
			name:    "array with unsupported item",
			value:   []interface{}{struct{}{}},
			wantErr: true,
		},
		{
			name:    "object with unsupported value",
			value:   map[string]interface{}{"bad": struct{}{}},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := inferSchemaTypeFromGoValue(tc.value)
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

// TestInferSchemaFromCELValue covers the public entry point: a nil ref.Val
// errors, while a concrete CEL value flows through to the Go-value inference.
func TestInferSchemaFromCELValue(t *testing.T) {
	tests := []struct {
		name    string
		val     ref.Val
		want    *extv1.JSONSchemaProps
		wantErr bool
	}{
		{name: "nil value errors", val: nil, wantErr: true},
		{name: "string value", val: types.String("hi"), want: &extv1.JSONSchemaProps{Type: "string"}},
		{name: "int value", val: types.Int(3), want: &extv1.JSONSchemaProps{Type: "integer"}},
		{name: "bool value", val: types.Bool(true), want: &extv1.JSONSchemaProps{Type: "boolean"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := inferSchemaFromCELValue(tc.val)
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
