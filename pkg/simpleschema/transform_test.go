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

package simpleschema

import (
	"fmt"
	"reflect"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/utils/ptr"
)

func TestBuildOpenAPISchema(t *testing.T) {
	tests := []struct {
		name    string
		obj     map[string]interface{}
		types   map[string]interface{}
		want    *extv1.JSONSchemaProps
		wantErr bool
	}{
		{
			name: "Complex nested schema",
			obj: map[string]interface{}{
				"name": "string | required=true",
				"age":  "integer | default=18",
				"contacts": map[string]interface{}{
					"email":   "string",
					"phone":   "string | default=\"000-000-0000\"",
					"address": "Address",
				},
				"tags":       "[]string",
				"metadata":   "map[string]string",
				"scores":     "[]integer",
				"attributes": "map[string]boolean",
				"friends":    "[]Person",
			},
			types: map[string]interface{}{
				"Address": map[string]interface{}{
					"street":  "string",
					"city":    "string",
					"country": "string",
				},
				"Person": map[string]interface{}{
					"name": "string",
					"age":  "integer",
				},
			},
			want: &extv1.JSONSchemaProps{
				Type:     "object",
				Required: []string{"name"},
				Properties: map[string]extv1.JSONSchemaProps{
					"name": {Type: "string"},
					"age": {
						Type:    "integer",
						Default: &extv1.JSON{Raw: []byte("18")},
					},
					"contacts": {
						Type:    "object",
						Default: &extv1.JSON{Raw: []byte("{}")},
						Properties: map[string]extv1.JSONSchemaProps{
							"email": {Type: "string"},
							"phone": {
								Type:    "string",
								Default: &extv1.JSON{Raw: []byte("\"000-000-0000\"")},
							},
							"address": {
								Type: "object",
								Properties: map[string]extv1.JSONSchemaProps{
									"street":  {Type: "string"},
									"city":    {Type: "string"},
									"country": {Type: "string"},
								},
							},
						},
					},
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
					"scores": {
						Type: "array",
						Items: &extv1.JSONSchemaPropsOrArray{
							Schema: &extv1.JSONSchemaProps{Type: "integer"},
						},
					},
					"attributes": {
						Type: "object",
						AdditionalProperties: &extv1.JSONSchemaPropsOrBool{
							Schema: &extv1.JSONSchemaProps{Type: "boolean"},
						},
					},
					"friends": {
						Type: "array",
						Items: &extv1.JSONSchemaPropsOrArray{
							Schema: &extv1.JSONSchemaProps{
								Type: "object",
								Properties: map[string]extv1.JSONSchemaProps{
									"name": {Type: "string"},
									"age":  {Type: "integer"},
								},
							},
						},
					},
				},
			},
			wantErr: false,
		},
		{
			name: "Schema with complex map",
			obj: map[string]interface{}{
				"config": "map[string]map[string]integer",
			},
			want: &extv1.JSONSchemaProps{
				Type: "object",
				Properties: map[string]extv1.JSONSchemaProps{
					"config": {
						Type: "object",
						AdditionalProperties: &extv1.JSONSchemaPropsOrBool{
							Schema: &extv1.JSONSchemaProps{
								Type: "object",
								AdditionalProperties: &extv1.JSONSchemaPropsOrBool{
									Schema: &extv1.JSONSchemaProps{Type: "integer"},
								},
							},
						},
					},
				},
			},
			wantErr: false,
		},
		{
			name: "Schema with complex array",
			obj: map[string]interface{}{
				"matrix": "[][]float",
			},
			want: &extv1.JSONSchemaProps{
				Type: "object",
				Properties: map[string]extv1.JSONSchemaProps{
					"matrix": {
						Type: "array",
						Items: &extv1.JSONSchemaPropsOrArray{
							Schema: &extv1.JSONSchemaProps{
								Type: "array",
								Items: &extv1.JSONSchemaPropsOrArray{
									Schema: &extv1.JSONSchemaProps{Type: "float"},
								},
							},
						},
					},
				},
			},
			wantErr: false,
		},
		{
			name: "Schema with array of objects",
			obj: map[string]interface{}{
				"items": "[]object",
			},
			want: &extv1.JSONSchemaProps{
				Type: "object",
				Properties: map[string]extv1.JSONSchemaProps{
					"items": {
						Type: "array",
						Items: &extv1.JSONSchemaPropsOrArray{
							Schema: &extv1.JSONSchemaProps{
								Type:                   "object",
								XPreserveUnknownFields: ptr.To(true),
							},
						},
					},
				},
			},
			wantErr: false,
		},
		{
			name: "Schema with nested array of objects",
			obj: map[string]interface{}{
				"matrix": "[][]object",
			},
			want: &extv1.JSONSchemaProps{
				Type: "object",
				Properties: map[string]extv1.JSONSchemaProps{
					"matrix": {
						Type: "array",
						Items: &extv1.JSONSchemaPropsOrArray{
							Schema: &extv1.JSONSchemaProps{
								Type: "array",
								Items: &extv1.JSONSchemaPropsOrArray{
									Schema: &extv1.JSONSchemaProps{
										Type:                   "object",
										XPreserveUnknownFields: ptr.To(true),
									},
								},
							},
						},
					},
				},
			},
			wantErr: false,
		},
		{
			name: "Schema with invalid type",
			obj: map[string]interface{}{
				"invalid": "unknownType",
			},
			want:    nil,
			wantErr: true,
		},
		{
			name: "Nested slices",
			obj: map[string]interface{}{
				"matrix": "[][][]string",
			},
			want: &extv1.JSONSchemaProps{
				Type: "object",
				Properties: map[string]extv1.JSONSchemaProps{
					"matrix": {
						Type: "array",
						Items: &extv1.JSONSchemaPropsOrArray{
							Schema: &extv1.JSONSchemaProps{
								Type: "array",
								Items: &extv1.JSONSchemaPropsOrArray{
									Schema: &extv1.JSONSchemaProps{
										Type: "array",
										Items: &extv1.JSONSchemaPropsOrArray{
											Schema: &extv1.JSONSchemaProps{Type: "string"},
										},
									},
								},
							},
						},
					},
				},
			},
			wantErr: false,
		},
		{
			name: "Nested slices with custom type",
			obj: map[string]interface{}{
				"matrix": "[][][]Person",
			},
			types: map[string]interface{}{
				"Person": map[string]interface{}{
					"name": "string",
					"age":  "integer",
				},
			},
			want: &extv1.JSONSchemaProps{
				Type: "object",
				Properties: map[string]extv1.JSONSchemaProps{
					"matrix": {
						Type: "array",
						Items: &extv1.JSONSchemaPropsOrArray{
							Schema: &extv1.JSONSchemaProps{
								Type: "array",
								Items: &extv1.JSONSchemaPropsOrArray{
									Schema: &extv1.JSONSchemaProps{
										Type: "array",
										Items: &extv1.JSONSchemaPropsOrArray{
											Schema: &extv1.JSONSchemaProps{
												Type: "object",
												Properties: map[string]extv1.JSONSchemaProps{
													"name": {Type: "string"},
													"age":  {Type: "integer"},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			wantErr: false,
		},
		{
			name: "Nested maps",
			obj: map[string]interface{}{
				"matrix": "map[string]map[string]map[string]Person",
			},
			types: map[string]interface{}{
				"Person": map[string]interface{}{
					"name": "string",
					"age":  "integer",
				},
			},
			want: &extv1.JSONSchemaProps{
				Type: "object",
				Properties: map[string]extv1.JSONSchemaProps{
					"matrix": {
						Type: "object",
						AdditionalProperties: &extv1.JSONSchemaPropsOrBool{
							Schema: &extv1.JSONSchemaProps{
								Type: "object",
								AdditionalProperties: &extv1.JSONSchemaPropsOrBool{
									Schema: &extv1.JSONSchemaProps{
										Type: "object",
										AdditionalProperties: &extv1.JSONSchemaPropsOrBool{
											Schema: &extv1.JSONSchemaProps{
												Type: "object",
												Properties: map[string]extv1.JSONSchemaProps{
													"name": {Type: "string"},
													"age":  {Type: "integer"},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			wantErr: false,
		},
		{
			name: "Schema with map of objects",
			obj: map[string]interface{}{
				"config": "map[string]object",
			},
			want: &extv1.JSONSchemaProps{
				Type: "object",
				Properties: map[string]extv1.JSONSchemaProps{
					"config": {
						Type: "object",
						AdditionalProperties: &extv1.JSONSchemaPropsOrBool{
							Schema: &extv1.JSONSchemaProps{
								Type:                   "object",
								XPreserveUnknownFields: ptr.To(true),
							},
						},
					},
				},
			},
			wantErr: false,
		},
		{
			name: "Schema with nested map of objects",
			obj: map[string]interface{}{
				"config": "map[string]map[string]object",
			},
			want: &extv1.JSONSchemaProps{
				Type: "object",
				Properties: map[string]extv1.JSONSchemaProps{
					"config": {
						Type: "object",
						AdditionalProperties: &extv1.JSONSchemaPropsOrBool{
							Schema: &extv1.JSONSchemaProps{
								Type: "object",
								AdditionalProperties: &extv1.JSONSchemaPropsOrBool{
									Schema: &extv1.JSONSchemaProps{
										Type:                   "object",
										XPreserveUnknownFields: ptr.To(true),
									},
								},
							},
						},
					},
				},
			},
			wantErr: false,
		},
		{
			name: "Schema with multiple enum types",
			obj: map[string]interface{}{
				"logLevel": "string | enum=\"debug,info,warn,error\" default=\"info\"",
				"features": map[string]interface{}{
					"logFormat": "string | enum=\"json,text,csv\" default=\"json\"",
					"errorCode": "integer | enum=\"400,404,500\" default=500",
				},
			},
			want: &extv1.JSONSchemaProps{
				Type:    "object",
				Default: &extv1.JSON{Raw: []byte("{}")},
				Properties: map[string]extv1.JSONSchemaProps{
					"logLevel": {
						Type:    "string",
						Default: &extv1.JSON{Raw: []byte("\"info\"")},
						Enum: []extv1.JSON{
							{Raw: []byte("\"debug\"")},
							{Raw: []byte("\"info\"")},
							{Raw: []byte("\"warn\"")},
							{Raw: []byte("\"error\"")},
						},
					},
					"features": {
						Type:    "object",
						Default: &extv1.JSON{Raw: []byte("{}")},
						Properties: map[string]extv1.JSONSchemaProps{
							"logFormat": {
								Type:    "string",
								Default: &extv1.JSON{Raw: []byte("\"json\"")},
								Enum: []extv1.JSON{
									{Raw: []byte("\"json\"")},
									{Raw: []byte("\"text\"")},
									{Raw: []byte("\"csv\"")},
								},
							},
							"errorCode": {
								Type:    "integer",
								Default: &extv1.JSON{Raw: []byte("500")},
								Enum: []extv1.JSON{
									{Raw: []byte("400")},
									{Raw: []byte("404")},
									{Raw: []byte("500")},
								},
							},
						},
					},
				},
			},
			wantErr: false,
		},
		{
			name: "invalid enum type",
			obj: map[string]interface{}{
				"threshold": "integer | enum=\"1,2,three\"",
			},
			want:    nil,
			wantErr: true,
		},
		{
			name: "Invalid integer enum - empty values",
			obj: map[string]interface{}{
				"errorCode": "integer | enum=\"1,,3\"",
			},
			want:    nil,
			wantErr: true,
		},
		{
			name: "Invalid integer enum - parsing failure",
			obj: map[string]interface{}{
				"errorCode": "integer | enum=\"1,2,3,abc\"",
			},
			want:    nil,
			wantErr: true,
		},
		{
			name: "invalid string enum marker",
			obj: map[string]interface{}{
				"status": "string | enum=\"a,b,,c\"",
			},
			want:    nil,
			wantErr: true,
		},
		{
			name: "Object with unknown fields",
			obj: map[string]interface{}{
				"values": "object",
			},
			want: &extv1.JSONSchemaProps{
				Type: "object",
				Properties: map[string]extv1.JSONSchemaProps{
					"values": {
						Type:                   "object",
						XPreserveUnknownFields: ptr.To(true),
					},
				},
			},
			wantErr: false,
		},
		{
			name: "Object with unknown fields in combination with required marker",
			obj: map[string]interface{}{
				"values": "object | required=true",
			},
			want: &extv1.JSONSchemaProps{
				Type: "object",
				Properties: map[string]extv1.JSONSchemaProps{
					"values": {
						Type:                   "object",
						XPreserveUnknownFields: ptr.To(true),
					},
				},
				Required: []string{"values"},
			},
			wantErr: false,
		},
		{
			name: "Object with unknown fields in combination with default marker",
			obj: map[string]interface{}{
				"values": "object | default={\"a\": \"b\"}",
			},
			want: &extv1.JSONSchemaProps{
				Type: "object",
				Properties: map[string]extv1.JSONSchemaProps{
					"values": {
						Type:                   "object",
						XPreserveUnknownFields: ptr.To(true),
						Default:                &extv1.JSON{Raw: []byte("{\"a\": \"b\"}")},
					},
				},
				Default: &extv1.JSON{Raw: []byte("{}")},
			},
			wantErr: false,
		},
		{
			name: "Simple string validation",
			obj: map[string]interface{}{
				"name": `string | validation="self.name != 'invalid'"`,
			},
			want: &extv1.JSONSchemaProps{
				Type: "object",
				Properties: map[string]extv1.JSONSchemaProps{
					"name": {
						Type: "string",
						XValidations: []extv1.ValidationRule{
							{
								Rule:    "self.name != 'invalid'",
								Message: "validation failed",
							},
						},
					},
				},
			},
			wantErr: false,
		},
		{
			name: "Multiple field validations",
			obj: map[string]interface{}{
				"age":  `integer | validation="self.age >= 0 && self.age <= 120"`,
				"name": `string | validation="self.name.length() >= 3"`,
			},
			want: &extv1.JSONSchemaProps{
				Type: "object",
				Properties: map[string]extv1.JSONSchemaProps{
					"age": {
						Type: "integer",
						XValidations: []extv1.ValidationRule{
							{
								Rule:    "self.age >= 0 && self.age <= 120",
								Message: "validation failed",
							},
						},
					},
					"name": {
						Type: "string",
						XValidations: []extv1.ValidationRule{
							{
								Rule:    "self.name.length() >= 3",
								Message: "validation failed",
							},
						},
					},
				},
			},
			wantErr: false,
		},
		{
			name: "Empty validation",
			obj: map[string]interface{}{
				"age": `integer | validation=""`,
			},
			want:    nil,
			wantErr: true,
		},
		{
			name: "Simple immutable field",
			obj: map[string]interface{}{
				"id": "string | immutable=true",
			},
			want: &extv1.JSONSchemaProps{
				Type: "object",
				Properties: map[string]extv1.JSONSchemaProps{
					"id": {
						Type: "string",
						XValidations: []extv1.ValidationRule{
							{
								Rule:    "self == oldSelf",
								Message: "field is immutable",
							},
						},
					},
				},
			},
			wantErr: false,
		},
		{
			name: "Simple immutable field with false value",
			obj: map[string]interface{}{
				"name": "string | immutable=false",
			},
			want: &extv1.JSONSchemaProps{
				Type: "object",
				Properties: map[string]extv1.JSONSchemaProps{
					"name": {
						Type: "string",
					},
				},
			},
			wantErr: false,
		},
		{
			name: "Immutable with other markers",
			obj: map[string]interface{}{
				"resourceId": `string | required=true immutable=true description="Unique resource identifier"`,
			},
			want: &extv1.JSONSchemaProps{
				Type:     "object",
				Required: []string{"resourceId"},
				Properties: map[string]extv1.JSONSchemaProps{
					"resourceId": {
						Type:        "string",
						Description: "Unique resource identifier",
						XValidations: []extv1.ValidationRule{
							{
								Rule:    "self == oldSelf",
								Message: "field is immutable",
							},
						},
					},
				},
			},
			wantErr: false,
		},
		{
			name: "Invalid immutable value",
			obj: map[string]interface{}{
				"id": "string | immutable=invalid",
			},
			want:    nil,
			wantErr: true,
		},
		{
			name: "Custom simple type (required)",
			obj: map[string]interface{}{
				"myValue": "myType",
			},
			types: map[string]interface{}{
				"myType": "string | required=true description=\"my description\"",
			},
			want: &extv1.JSONSchemaProps{
				Type: "object",
				Properties: map[string]extv1.JSONSchemaProps{
					"myValue": {
						Type:        "string",
						Description: "my description",
					},
				},
				Required: []string{"myValue"},
			},
			wantErr: false,
		},
		{
			name: "Invalid Required Marker value",
			obj: map[string]interface{}{
				"data": "string | required=invalid",
			},
			want:    nil,
			wantErr: true,
		},
		{
			name: "Required Marker handling",
			obj: map[string]interface{}{
				"req_1": "string | required=true",
			},
			want: &extv1.JSONSchemaProps{
				Type: "object",
				Properties: map[string]extv1.JSONSchemaProps{
					"req_1": {Type: "string"},
				},
				Required: []string{"req_1"},
			},
			wantErr: false,
		},
		{
			name: "String field with pattern validation",
			obj: map[string]interface{}{
				"email": "string | pattern=\"^[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\\.[a-zA-Z]{2,}$\"",
			},
			want: &extv1.JSONSchemaProps{
				Type: "object",
				Properties: map[string]extv1.JSONSchemaProps{
					"email": {
						Type:    "string",
						Pattern: "^[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\\.[a-zA-Z]{2,}$",
					},
				},
			},
			wantErr: false,
		},
		{
			name: "String field with minLength and maxLength",
			obj: map[string]interface{}{
				"username": "string | minLength=3 maxLength=20",
			},
			want: &extv1.JSONSchemaProps{
				Type: "object",
				Properties: map[string]extv1.JSONSchemaProps{
					"username": {
						Type:      "string",
						MinLength: ptr.To(int64(3)),
						MaxLength: ptr.To(int64(20)),
					},
				},
			},
			wantErr: false,
		},
		{
			name: "String field with all validation markers",
			obj: map[string]interface{}{
				"code": "string | pattern=\"^[A-Z]{2}[0-9]{4}$\" minLength=6 maxLength=6 description=\"Country code format\"",
			},
			want: &extv1.JSONSchemaProps{
				Type: "object",
				Properties: map[string]extv1.JSONSchemaProps{
					"code": {
						Type:        "string",
						Pattern:     "^[A-Z]{2}[0-9]{4}$",
						MinLength:   ptr.To(int64(6)),
						MaxLength:   ptr.To(int64(6)),
						Description: "Country code format",
					},
				},
			},
			wantErr: false,
		},
		{
			name: "Array field with uniqueItems true",
			obj: map[string]interface{}{
				"tags": "[]string | uniqueItems=true",
			},
			want: &extv1.JSONSchemaProps{
				Type: "object",
				Properties: map[string]extv1.JSONSchemaProps{
					"tags": {
						Type: "array",
						Items: &extv1.JSONSchemaPropsOrArray{
							Schema: &extv1.JSONSchemaProps{Type: "string"},
						},
						XListType: ptr.To("set"),
					},
				},
			},
			wantErr: false,
		},
		{
			name: "Array field with uniqueItems false",
			obj: map[string]interface{}{
				"comments": "[]string | uniqueItems=false",
			},
			want: &extv1.JSONSchemaProps{
				Type: "object",
				Properties: map[string]extv1.JSONSchemaProps{
					"comments": {
						Type: "array",
						Items: &extv1.JSONSchemaPropsOrArray{
							Schema: &extv1.JSONSchemaProps{Type: "string"},
						},
					},
				},
			},
			wantErr: false,
		},
		{
			name: "Complex object with multiple new validation markers",
			obj: map[string]interface{}{
				"user": map[string]interface{}{
					"email":    "string | pattern=\"^[\\w\\.-]+@[\\w\\.-]+\\.\\w+$\"",
					"username": "string | minLength=3 maxLength=15 pattern=\"^[a-zA-Z0-9_]+$\"",
					"roles":    "[]string | uniqueItems=true",
					"tags":     "[]string | uniqueItems=false",
				},
			},
			want: &extv1.JSONSchemaProps{
				Type: "object",
				Properties: map[string]extv1.JSONSchemaProps{
					"user": {
						Type: "object",
						Properties: map[string]extv1.JSONSchemaProps{
							"email": {
								Type:    "string",
								Pattern: "^[\\w\\.-]+@[\\w\\.-]+\\.\\w+$",
							},
							"username": {
								Type:      "string",
								Pattern:   "^[a-zA-Z0-9_]+$",
								MinLength: ptr.To(int64(3)),
								MaxLength: ptr.To(int64(15)),
							},
							"roles": {
								Type: "array",
								Items: &extv1.JSONSchemaPropsOrArray{
									Schema: &extv1.JSONSchemaProps{Type: "string"},
								},
								XListType: ptr.To("set"),
							},
							"tags": {
								Type: "array",
								Items: &extv1.JSONSchemaPropsOrArray{
									Schema: &extv1.JSONSchemaProps{Type: "string"},
								},
							},
						},
					},
				},
			},
			wantErr: false,
		},
		{
			name: "Array field with minItems",
			obj: map[string]interface{}{
				"items": "[]string | minItems=2",
			},
			want: &extv1.JSONSchemaProps{
				Type: "object",
				Properties: map[string]extv1.JSONSchemaProps{
					"items": {
						Type:     "array",
						MinItems: ptr.To(int64(2)),
						Items: &extv1.JSONSchemaPropsOrArray{
							Schema: &extv1.JSONSchemaProps{Type: "string"},
						},
					},
				},
			},
			wantErr: false,
		},
		{
			name: "Array field with maxItems",
			obj: map[string]interface{}{
				"tags": "[]string | maxItems=10",
			},
			want: &extv1.JSONSchemaProps{
				Type: "object",
				Properties: map[string]extv1.JSONSchemaProps{
					"tags": {
						Type:     "array",
						MaxItems: ptr.To(int64(10)),
						Items: &extv1.JSONSchemaPropsOrArray{
							Schema: &extv1.JSONSchemaProps{Type: "string"},
						},
					},
				},
			},
			wantErr: false,
		},
		{
			name: "Array field with minItems and maxItems",
			obj: map[string]interface{}{
				"priorities": "[]integer | minItems=1 maxItems=5",
			},
			want: &extv1.JSONSchemaProps{
				Type: "object",
				Properties: map[string]extv1.JSONSchemaProps{
					"priorities": {
						Type:     "array",
						MinItems: ptr.To(int64(1)),
						MaxItems: ptr.To(int64(5)),
						Items: &extv1.JSONSchemaPropsOrArray{
							Schema: &extv1.JSONSchemaProps{Type: "integer"},
						},
					},
				},
			},
			wantErr: false,
		},
		{
			name: "Array field with all validation markers",
			obj: map[string]interface{}{
				"codes": "[]string | uniqueItems=true minItems=2 maxItems=8",
			},
			want: &extv1.JSONSchemaProps{
				Type: "object",
				Properties: map[string]extv1.JSONSchemaProps{
					"codes": {
						Type:     "array",
						MinItems: ptr.To(int64(2)),
						MaxItems: ptr.To(int64(8)),
						Items: &extv1.JSONSchemaPropsOrArray{
							Schema: &extv1.JSONSchemaProps{Type: "string"},
						},
						XListType: ptr.To("set"),
					},
				},
			},
			wantErr: false,
		},
		{
			name: "Array field with zero minItems",
			obj: map[string]interface{}{
				"optional": "[]string | minItems=0",
			},
			want: &extv1.JSONSchemaProps{
				Type: "object",
				Properties: map[string]extv1.JSONSchemaProps{
					"optional": {
						Type:     "array",
						MinItems: ptr.To(int64(0)),
						Items: &extv1.JSONSchemaPropsOrArray{
							Schema: &extv1.JSONSchemaProps{Type: "string"},
						},
					},
				},
			},
			wantErr: false,
		},
		{
			name: "Invalid pattern regex",
			obj: map[string]interface{}{
				"invalid": "string | pattern=\"[unclosed\"",
			},
			want:    nil,
			wantErr: true,
		},
		{
			name: "Pattern marker on non-string type",
			obj: map[string]interface{}{
				"invalid": "integer | pattern=\"[0-9]+\"",
			},
			want:    nil,
			wantErr: true,
		},
		{
			name: "MinLength marker on non-string type",
			obj: map[string]interface{}{
				"invalid": "integer | minLength=5",
			},
			want:    nil,
			wantErr: true,
		},
		{
			name: "MaxLength marker on non-string type",
			obj: map[string]interface{}{
				"invalid": "boolean | maxLength=10",
			},
			want:    nil,
			wantErr: true,
		},
		{
			name: "UniqueItems marker on non-array type",
			obj: map[string]interface{}{
				"invalid": "string | uniqueItems=true",
			},
			want:    nil,
			wantErr: true,
		},
		{
			name: "Invalid minLength value",
			obj: map[string]interface{}{
				"invalid": "string | minLength=abc",
			},
			want:    nil,
			wantErr: true,
		},
		{
			name: "Invalid maxLength value",
			obj: map[string]interface{}{
				"invalid": "string | maxLength=xyz",
			},
			want:    nil,
			wantErr: true,
		},
		{
			name: "Invalid uniqueItems value",
			obj: map[string]interface{}{
				"invalid": "[]string | uniqueItems=invalid",
			},
			want:    nil,
			wantErr: true,
		},
		{
			name: "MinItems marker on non-array type",
			obj: map[string]interface{}{
				"invalid": "string | minItems=5",
			},
			want:    nil,
			wantErr: true,
		},
		{
			name: "MaxItems marker on non-array type",
			obj: map[string]interface{}{
				"invalid": "integer | maxItems=10",
			},
			want:    nil,
			wantErr: true,
		},
		{
			name: "Invalid minItems value",
			obj: map[string]interface{}{
				"invalid": "[]string | minItems=abc",
			},
			want:    nil,
			wantErr: true,
		},
		{
			name: "Invalid maxItems value",
			obj: map[string]interface{}{
				"invalid": "[]string | maxItems=xyz",
			},
			want:    nil,
			wantErr: true,
		},
		{
			name: "Empty pattern value",
			obj: map[string]interface{}{
				"invalid": "string | pattern=\"\"",
			},
			want:    nil,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ToOpenAPISpec(tt.obj, tt.types)
			if (err != nil) != tt.wantErr {
				t.Errorf("BuildOpenAPISchema() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			assert.Equal(t, got, tt.want)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("BuildOpenAPISchema() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestApplyMarkers_Required(t *testing.T) {
	transformer := newTransformer()

	tests := []struct {
		value    string
		required bool
		err      error
	}{
		{"true", true, nil},
		{"True", true, nil},
		{"TRUE", true, nil},
		{"1", true, nil},
		{"false", false, nil},
		{"False", false, nil},
		{"FALSE", false, nil},
		{"invalid", false, fmt.Errorf("failed to parse required marker value: strconv.ParseBool: parsing \"invalid\": invalid syntax")},
		{"no", false, fmt.Errorf("failed to parse required marker value: strconv.ParseBool: parsing \"no\": invalid syntax")},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("Required Marker %s", tt.value), func(t *testing.T) {
			parentSchema := &extv1.JSONSchemaProps{}
			markers := []*Marker{{MarkerType: MarkerTypeRequired, Value: tt.value}}
			err := transformer.applyMarkers(nil, markers, "myFieldName", parentSchema)
			if err != nil && err.Error() != tt.err.Error() {
				t.Errorf("ApplyMarkers() error = %q, expected error %q", err, tt.err)
			}
			// If there was no error, check if the required field was added to the parent schema.
			if err == nil && tt.required != slices.Contains(parentSchema.Required, "myFieldName") {
				t.Errorf("ApplyMarkers() = %v, want %v", parentSchema.Required, tt.required)
			}
		})
	}
}

func TestLoadPreDefinedTypes(t *testing.T) {
	transformer := newTransformer()

	tests := []struct {
		name    string
		obj     map[string]interface{}
		want    map[string]predefinedType
		wantErr bool
	}{
		{
			name: "Valid types with cross-references",
			obj: map[string]interface{}{
				"Person": map[string]interface{}{
					"name": "string",
					"age":  "integer",
					"address": map[string]interface{}{
						"street": "string",
						"city":   "string",
					},
					"company": "Company",
				},
				"Company": map[string]interface{}{
					"name":      "string",
					"employees": "[]string",
				},
			},
			want: map[string]predefinedType{
				"Person": {
					Schema: extv1.JSONSchemaProps{
						Type: "object",
						Properties: map[string]extv1.JSONSchemaProps{
							"name": {Type: "string"},
							"age":  {Type: "integer"},
							"address": {
								Type: "object",
								Properties: map[string]extv1.JSONSchemaProps{
									"street": {Type: "string"},
									"city":   {Type: "string"},
								},
							},
							"company": {
								Type: "object",
								Properties: map[string]extv1.JSONSchemaProps{
									"name": {Type: "string"},
									"employees": {
										Type: "array",
										Items: &extv1.JSONSchemaPropsOrArray{
											Schema: &extv1.JSONSchemaProps{
												Type: "string",
											},
										},
									},
								},
							},
						},
					},
					Required: false,
				},
				"Company": {
					Schema: extv1.JSONSchemaProps{
						Type: "object",
						Properties: map[string]extv1.JSONSchemaProps{
							"name": {Type: "string"},
							"employees": {
								Type: "array",
								Items: &extv1.JSONSchemaPropsOrArray{
									Schema: &extv1.JSONSchemaProps{
										Type: "string",
									},
								},
							},
						},
					},
					Required: false,
				},
			},
			wantErr: false,
		},
		{
			name: "Invalid types with cyclic references",
			obj: map[string]interface{}{
				"Person": map[string]interface{}{
					"name": "string",
					"age":  "integer",
					"address": map[string]interface{}{
						"street": "string",
						"city":   "string",
					},
					"company": "Company",
				},
				"Company": map[string]interface{}{
					"name":      "string",
					"employees": "[]Person",
				},
			},
			want:    map[string]predefinedType{},
			wantErr: true,
		},
		{
			name: "Simple type alias",
			obj: map[string]interface{}{
				"alias": "string",
			},
			want: map[string]predefinedType{
				"alias": {
					Schema: extv1.JSONSchemaProps{
						Type: "string",
					},
					Required: false,
				},
			},
			wantErr: false,
		},
		{
			name: "Simple type alias with markers",
			obj: map[string]interface{}{
				"alias": "string | required=true default=\"test\"",
			},
			want: map[string]predefinedType{
				"alias": {
					Schema: extv1.JSONSchemaProps{
						Type:    "string",
						Default: &extv1.JSON{Raw: []byte("\"test\"")},
					},
					Required: true,
				},
			},
			wantErr: false,
		},
		{
			name: "Invalid type",
			obj: map[string]interface{}{
				"invalid": 123,
			},
			want:    map[string]predefinedType{},
			wantErr: true,
		},
		{
			name: "Nested cross-references",
			obj: map[string]interface{}{
				"Service": map[string]interface{}{
					"target": "Deployment",
				},
				"Deployment": map[string]interface{}{
					"config": "ConfigMap | required=true",
				},
				"ConfigMap": map[string]interface{}{
					"data": "string",
				},
			},
			want: map[string]predefinedType{
				"ConfigMap": {
					Schema: extv1.JSONSchemaProps{
						Type: "object",
						Properties: map[string]extv1.JSONSchemaProps{
							"data": {Type: "string"},
						},
					},
					Required: false,
				},
				"Deployment": {
					Schema: extv1.JSONSchemaProps{
						Type: "object",
						Properties: map[string]extv1.JSONSchemaProps{
							"config": {
								Type: "object",
								Properties: map[string]extv1.JSONSchemaProps{
									"data": {Type: "string"},
								},
							},
						},
						Required: []string{"config"},
					},
					Required: false,
				},
				"Service": {
					Schema: extv1.JSONSchemaProps{
						Type: "object",
						Properties: map[string]extv1.JSONSchemaProps{
							"target": {
								Type: "object",
								Properties: map[string]extv1.JSONSchemaProps{
									"config": {
										Type: "object",
										Properties: map[string]extv1.JSONSchemaProps{
											"data": {Type: "string"},
										},
									},
								},
								Required: []string{"config"},
							},
						},
					},
					Required: false,
				},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := transformer.loadPreDefinedTypes(tt.obj)
			if (err != nil) != tt.wantErr {
				t.Fatalf("LoadPreDefinedTypes() error = %v", err)
				return
			}
			if !reflect.DeepEqual(transformer.preDefinedTypes, tt.want) {
				t.Errorf("LoadPreDefinedTypes() = %+v, want %+v", transformer.preDefinedTypes, tt.want)
			}
		})
	}
}
