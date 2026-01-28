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
	"reflect"
	"sort"
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
			name: "Map of arrays",
			obj: map[string]interface{}{
				"tags": "map[string][]string",
			},
			want: &extv1.JSONSchemaProps{
				Type: "object",
				Properties: map[string]extv1.JSONSchemaProps{
					"tags": {
						Type: "object",
						AdditionalProperties: &extv1.JSONSchemaPropsOrBool{
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
			wantErr: false,
		},
		{
			name: "Array of maps",
			obj: map[string]interface{}{
				"configs": "[]map[string]integer",
			},
			want: &extv1.JSONSchemaProps{
				Type: "object",
				Properties: map[string]extv1.JSONSchemaProps{
					"configs": {
						Type: "array",
						Items: &extv1.JSONSchemaPropsOrArray{
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
			name: "Custom types with nested collections",
			obj: map[string]interface{}{
				"data": "Record",
			},
			types: map[string]interface{}{
				"Record": map[string]interface{}{
					"tags":   "map[string][]string",
					"matrix": "[][]integer",
				},
			},
			want: &extv1.JSONSchemaProps{
				Type: "object",
				Properties: map[string]extv1.JSONSchemaProps{
					"data": {
						Type: "object",
						Properties: map[string]extv1.JSONSchemaProps{
							"tags": {
								Type: "object",
								AdditionalProperties: &extv1.JSONSchemaPropsOrBool{
									Schema: &extv1.JSONSchemaProps{
										Type: "array",
										Items: &extv1.JSONSchemaPropsOrArray{
											Schema: &extv1.JSONSchemaProps{Type: "string"},
										},
									},
								},
							},
							"matrix": {
								Type: "array",
								Items: &extv1.JSONSchemaPropsOrArray{
									Schema: &extv1.JSONSchemaProps{
										Type: "array",
										Items: &extv1.JSONSchemaPropsOrArray{
											Schema: &extv1.JSONSchemaProps{Type: "integer"},
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
				"values": "object | default={\"a\":\"b\"}",
			},
			want: &extv1.JSONSchemaProps{
				Type:    "object",
				Default: &extv1.JSON{Raw: []byte("{}")},
				Properties: map[string]extv1.JSONSchemaProps{
					"values": {
						Type:                   "object",
						XPreserveUnknownFields: ptr.To(true),
						Default:                &extv1.JSON{Raw: []byte("{\"a\":\"b\"}")},
					},
				},
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
			name: "Cyclic dependency in custom types",
			obj: map[string]interface{}{
				"data": "TypeA",
			},
			types: map[string]interface{}{
				"TypeA": "TypeB",
				"TypeB": "TypeA",
			},
			wantErr: true,
		},
		{
			name: "Undefined custom type reference",
			obj: map[string]interface{}{
				"data": "string",
			},
			types: map[string]interface{}{
				"TypeA": "UndefinedType",
			},
			wantErr: true,
		},
		{
			name: "Invalid custom type spec",
			obj: map[string]interface{}{
				"data": "string",
			},
			types: map[string]interface{}{
				"BadType": 123,
			},
			wantErr: true,
		},
		{
			name: "Invalid string custom type",
			obj: map[string]interface{}{
				"data": "string",
			},
			types: map[string]interface{}{
				"BadType": "[]",
			},
			wantErr: true,
		},
		{
			name: "Invalid map custom type",
			obj: map[string]interface{}{
				"data": "string",
			},
			types: map[string]interface{}{
				"BadType": map[string]interface{}{
					"field": 123,
				},
			},
			wantErr: true,
		},
		{
			name: "Invalid field spec type",
			obj: map[string]interface{}{
				"data": 123,
			},
			wantErr: true,
		},
		{
			name: "Invalid string custom type triggers buildFieldFromString error",
			obj: map[string]interface{}{
				"data": "string",
			},
			types: map[string]interface{}{
				"BadType": "string | minLength=abc",
			},
			wantErr: true,
		},
		{
			name: "Invalid map custom type triggers buildSchema error",
			obj: map[string]interface{}{
				"data": "string",
			},
			types: map[string]interface{}{
				"BadType": map[string]interface{}{
					"field": "string | minLength=abc",
				},
			},
			wantErr: true,
		},
		{
			name: "Invalid nested field triggers buildSchema error",
			obj: map[string]interface{}{
				"outer": map[string]interface{}{
					"inner": "string | minLength=abc",
				},
			},
			wantErr: true,
		},
		{
			name: "Invalid field type string",
			obj: map[string]interface{}{
				"data": "string | =bad",
			},
			wantErr: true,
		},
		{
			name: "Slice with undefined element type",
			obj: map[string]interface{}{
				"items": "[]UndefinedType",
			},
			wantErr: true,
		},
		{
			name: "Map with undefined value type",
			obj: map[string]interface{}{
				"data": "map[string]UndefinedType",
			},
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

func TestDefaultPropagation(t *testing.T) {
	tests := []struct {
		name    string
		obj     map[string]interface{}
		types   map[string]interface{}
		want    *extv1.JSONSchemaProps
		wantErr bool
	}{
		{
			name: "child defaults propagate to parent",
			obj: map[string]interface{}{
				"timeout": "integer | default=30",
				"retries": "integer | default=3",
			},
			want: &extv1.JSONSchemaProps{
				Type:    "object",
				Default: &extv1.JSON{Raw: []byte("{}")},
				Properties: map[string]extv1.JSONSchemaProps{
					"timeout": {
						Type:    "integer",
						Default: &extv1.JSON{Raw: []byte("30")},
					},
					"retries": {
						Type:    "integer",
						Default: &extv1.JSON{Raw: []byte("3")},
					},
				},
			},
		},
		{
			name: "required field blocks parent default",
			obj: map[string]interface{}{
				"name":    "string | required=true",
				"timeout": "integer | default=30",
			},
			want: &extv1.JSONSchemaProps{
				Type:     "object",
				Required: []string{"name"},
				Properties: map[string]extv1.JSONSchemaProps{
					"name": {Type: "string"},
					"timeout": {
						Type:    "integer",
						Default: &extv1.JSON{Raw: []byte("30")},
					},
				},
			},
		},
		{
			name: "no defaults means no parent default",
			obj: map[string]interface{}{
				"name":    "string",
				"timeout": "integer",
			},
			want: &extv1.JSONSchemaProps{
				Type: "object",
				Properties: map[string]extv1.JSONSchemaProps{
					"name":    {Type: "string"},
					"timeout": {Type: "integer"},
				},
			},
		},
		{
			name: "custom type with defaults",
			obj: map[string]interface{}{
				"config": "Config",
			},
			types: map[string]interface{}{
				"Config": map[string]interface{}{
					"timeout": "integer | default=30",
					"retries": "integer | default=3",
				},
			},
			want: &extv1.JSONSchemaProps{
				Type:    "object",
				Default: &extv1.JSON{Raw: []byte("{}")},
				Properties: map[string]extv1.JSONSchemaProps{
					"config": {
						Type:    "object",
						Default: &extv1.JSON{Raw: []byte("{}")},
						Properties: map[string]extv1.JSONSchemaProps{
							"timeout": {
								Type:    "integer",
								Default: &extv1.JSON{Raw: []byte("30")},
							},
							"retries": {
								Type:    "integer",
								Default: &extv1.JSON{Raw: []byte("3")},
							},
						},
					},
				},
			},
		},
		{
			name: "custom type with required blocks default",
			obj: map[string]interface{}{
				"config": "Config",
			},
			types: map[string]interface{}{
				"Config": map[string]interface{}{
					"name":    "string | required=true",
					"timeout": "integer | default=30",
				},
			},
			want: &extv1.JSONSchemaProps{
				Type: "object",
				Properties: map[string]extv1.JSONSchemaProps{
					"config": {
						Type:     "object",
						Required: []string{"name"},
						Properties: map[string]extv1.JSONSchemaProps{
							"name": {Type: "string"},
							"timeout": {
								Type:    "integer",
								Default: &extv1.JSON{Raw: []byte("30")},
							},
						},
					},
				},
			},
		},
		{
			name: "nested custom types with defaults",
			obj: map[string]interface{}{
				"app": "App",
			},
			types: map[string]interface{}{
				"Config": map[string]interface{}{
					"port": "integer | default=8080",
				},
				"App": map[string]interface{}{
					"name":   "string",
					"config": "Config",
				},
			},
			want: &extv1.JSONSchemaProps{
				Type:    "object",
				Default: &extv1.JSON{Raw: []byte("{}")},
				Properties: map[string]extv1.JSONSchemaProps{
					"app": {
						Type:    "object",
						Default: &extv1.JSON{Raw: []byte("{}")},
						Properties: map[string]extv1.JSONSchemaProps{
							"name": {Type: "string"},
							"config": {
								Type:    "object",
								Default: &extv1.JSON{Raw: []byte("{}")},
								Properties: map[string]extv1.JSONSchemaProps{
									"port": {
										Type:    "integer",
										Default: &extv1.JSON{Raw: []byte("8080")},
									},
								},
							},
						},
					},
				},
			},
		},
		{
			name: "deeply nested required blocks default at that level only",
			obj: map[string]interface{}{
				"outer": map[string]interface{}{
					"inner": map[string]interface{}{
						"required_field": "string | required=true",
						"optional_field": "integer | default=42",
					},
					"sibling": "string | default=\"hello\"",
				},
			},
			want: &extv1.JSONSchemaProps{
				Type:    "object",
				Default: &extv1.JSON{Raw: []byte("{}")},
				Properties: map[string]extv1.JSONSchemaProps{
					"outer": {
						Type:    "object",
						Default: &extv1.JSON{Raw: []byte("{}")},
						Properties: map[string]extv1.JSONSchemaProps{
							"inner": {
								Type:     "object",
								Required: []string{"required_field"},
								Properties: map[string]extv1.JSONSchemaProps{
									"required_field": {Type: "string"},
									"optional_field": {
										Type:    "integer",
										Default: &extv1.JSON{Raw: []byte("42")},
									},
								},
							},
							"sibling": {
								Type:    "string",
								Default: &extv1.JSON{Raw: []byte("\"hello\"")},
							},
						},
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ToOpenAPISpec(tt.obj, tt.types)
			if (err != nil) != tt.wantErr {
				t.Errorf("ToOpenAPISpec() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ToOpenAPISpec() mismatch:\ngot:  %+v\nwant: %+v", got, tt.want)
			}
		})
	}
}

func TestFromOpenAPISpec(t *testing.T) {
	_, err := FromOpenAPISpec(nil)
	if err == nil {
		t.Error("FromOpenAPISpec() expected error, got nil")
	}
	if err.Error() != "not implemented" {
		t.Errorf("FromOpenAPISpec() expected 'not implemented' error, got %v", err)
	}
}

func TestComplexSchemaE2E(t *testing.T) {
	types := map[string]interface{}{
		"RequiredString": "string | required=true",
		"Port":           "integer | default=8080 minimum=1 maximum=65535",
		"DatabaseConfig": map[string]interface{}{
			"host":     "string | required=true",
			"port":     "Port",
			"username": "string | required=true",
			"password": "string | required=true",
			"database": "string | default=\"postgres\"",
		},
		"RetryConfig": map[string]interface{}{
			"maxRetries":  "integer | default=3 minimum=0 maximum=10",
			"backoffMs":   "integer | default=1000",
			"exponential": "boolean | default=true",
		},
		"ServiceEndpoint": map[string]interface{}{
			"name":    "RequiredString",
			"url":     "string | required=true pattern=\"^https?://\"",
			"timeout": "integer | default=30 minimum=1 maximum=300",
		},
		"Label": map[string]interface{}{
			"key":   "string | required=true minLength=1 maxLength=63",
			"value": "string | default=\"\"",
		},
	}

	obj := map[string]interface{}{
		"name":        "RequiredString",
		"environment": "string | default=\"development\" enum=\"development,staging,production\" description=\"Deployment environment\"",
		"database":    "DatabaseConfig",
		"retry":       "RetryConfig",
		"endpoints":   "[]ServiceEndpoint | minItems=1",
		"annotations": "map[string]string",
		"labels":      "[]Label | uniqueItems=true",
		"clusterId":   "string | immutable=true",
		"replicas":    "integer | default=1 minimum=0 maximum=100 validation=\"self <= 10 || self % 2 == 0\"",
		"resources": map[string]interface{}{
			"cpu":    "string | default=\"100m\" pattern=\"^[0-9]+m?$\"",
			"memory": "string | default=\"128Mi\" pattern=\"^[0-9]+(Mi|Gi)$\"",
		},
	}

	want := &extv1.JSONSchemaProps{
		Type:     "object",
		Required: []string{"name"}, // propagated from RequiredString custom type
		Properties: map[string]extv1.JSONSchemaProps{
			"name": {Type: "string"},
			"environment": {
				Type:        "string",
				Default:     &extv1.JSON{Raw: []byte(`"development"`)},
				Description: "Deployment environment",
				Enum: []extv1.JSON{
					{Raw: []byte(`"development"`)},
					{Raw: []byte(`"staging"`)},
					{Raw: []byte(`"production"`)},
				},
			},
			"database": {
				Type:     "object",
				Required: []string{"host", "password", "username"}, // no Default:{} because has required fields
				Properties: map[string]extv1.JSONSchemaProps{
					"host":     {Type: "string"},
					"username": {Type: "string"},
					"password": {Type: "string"},
					"port": {
						Type:    "integer",
						Default: &extv1.JSON{Raw: []byte("8080")},
						Minimum: ptr.To(1.0),
						Maximum: ptr.To(65535.0),
					},
					"database": {
						Type:    "string",
						Default: &extv1.JSON{Raw: []byte(`"postgres"`)},
					},
				},
			},
			"retry": {
				Type:    "object",
				Default: &extv1.JSON{Raw: []byte("{}")}, // gets Default:{} because all fields have defaults
				Properties: map[string]extv1.JSONSchemaProps{
					"maxRetries": {
						Type:    "integer",
						Default: &extv1.JSON{Raw: []byte("3")},
						Minimum: ptr.To(0.0),
						Maximum: ptr.To(10.0),
					},
					"backoffMs": {
						Type:    "integer",
						Default: &extv1.JSON{Raw: []byte("1000")},
					},
					"exponential": {
						Type:    "boolean",
						Default: &extv1.JSON{Raw: []byte("true")},
					},
				},
			},
			"endpoints": {
				Type:     "array",
				MinItems: ptr.To[int64](1),
				Items: &extv1.JSONSchemaPropsOrArray{
					Schema: &extv1.JSONSchemaProps{
						Type:     "object",
						Required: []string{"name", "url"},
						Properties: map[string]extv1.JSONSchemaProps{
							"name": {Type: "string"},
							"url": {
								Type:    "string",
								Pattern: "^https?://",
							},
							"timeout": {
								Type:    "integer",
								Default: &extv1.JSON{Raw: []byte("30")},
								Minimum: ptr.To(1.0),
								Maximum: ptr.To(300.0),
							},
						},
					},
				},
			},
			"annotations": {
				Type: "object",
				AdditionalProperties: &extv1.JSONSchemaPropsOrBool{
					Schema: &extv1.JSONSchemaProps{Type: "string"},
				},
			},
			"labels": {
				Type:      "array",
				XListType: ptr.To("set"),
				Items: &extv1.JSONSchemaPropsOrArray{
					Schema: &extv1.JSONSchemaProps{
						Type:     "object",
						Required: []string{"key"},
						Properties: map[string]extv1.JSONSchemaProps{
							"key": {
								Type:      "string",
								MinLength: ptr.To[int64](1),
								MaxLength: ptr.To[int64](63),
							},
							"value": {
								Type:    "string",
								Default: &extv1.JSON{Raw: []byte(`""`)},
							},
						},
					},
				},
			},
			"clusterId": {
				Type: "string",
				XValidations: []extv1.ValidationRule{
					{Rule: "self == oldSelf", Message: "field is immutable"},
				},
			},
			"replicas": {
				Type:    "integer",
				Default: &extv1.JSON{Raw: []byte("1")},
				Minimum: ptr.To(0.0),
				Maximum: ptr.To(100.0),
				XValidations: []extv1.ValidationRule{
					{Rule: "self <= 10 || self % 2 == 0", Message: "validation failed"},
				},
			},
			"resources": {
				Type:    "object",
				Default: &extv1.JSON{Raw: []byte("{}")},
				Properties: map[string]extv1.JSONSchemaProps{
					"cpu": {
						Type:    "string",
						Default: &extv1.JSON{Raw: []byte(`"100m"`)},
						Pattern: "^[0-9]+m?$",
					},
					"memory": {
						Type:    "string",
						Default: &extv1.JSON{Raw: []byte(`"128Mi"`)},
						Pattern: "^[0-9]+(Mi|Gi)$",
					},
				},
			},
		},
	}

	got, err := ToOpenAPISpec(obj, types)
	if err != nil {
		t.Fatalf("ToOpenAPISpec() error = %v", err)
	}
	sortRequiredFields(want)
	sortRequiredFields(got)
	assert.Equal(t, want, got)
}

// sortRequiredFields normalizes Required slices for comparison (map iteration order is non-deterministic)
func sortRequiredFields(schema *extv1.JSONSchemaProps) {
	if schema == nil {
		return
	}
	sort.Strings(schema.Required)
	for name, prop := range schema.Properties {
		sortRequiredFields(&prop)
		schema.Properties[name] = prop
	}
	if schema.Items != nil && schema.Items.Schema != nil {
		sortRequiredFields(schema.Items.Schema)
	}
}
