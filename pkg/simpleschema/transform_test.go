// Copyright 2025 The Kube Resource Orchestrator Authors
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
	"testing"

	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
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
						Type: "object",
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
			name: "Schema with multiple enum types",
			obj: map[string]interface{}{
				"logLevel": "string | enum=\"debug,info,warn,error\" default=\"info\"",
				"features": map[string]interface{}{
					"logFormat": "string | enum=\"json,text,csv\" default=\"json\"",
					"errorCode": "integer | enum=\"400,404,500\" default=500",
				},
			},
			want: &extv1.JSONSchemaProps{
				Type: "object",
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
						Type: "object",
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ToOpenAPISpec(tt.obj, tt.types)
			if (err != nil) != tt.wantErr {
				t.Errorf("BuildOpenAPISchema() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("BuildOpenAPISchema() = %+v, want %+v", got, tt.want)
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
			name: "Valid types",
			obj: map[string]interface{}{
				"Person": map[string]interface{}{
					"name": "string",
					"age":  "integer",
					"address": map[string]interface{}{
						"street": "string",
						"city":   "string",
					},
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
