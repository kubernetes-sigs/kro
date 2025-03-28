// Copyright 2025 The Kube Resource Orchestrator Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package simpleschema

import (
	"reflect"
	"testing"

	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

func TestBuildOpenAPISchema(t *testing.T) {
	transformer := newTransformer()

	// Load pre-defined types
	err := transformer.loadPreDefinedTypes(map[string]interface{}{
		"Address": map[string]interface{}{
			"street":  "string",
			"city":    "string",
			"country": "string",
		},
		"Person": map[string]interface{}{
			"name": "string",
			"age":  "integer",
		},
	})
	if err != nil {
		t.Fatalf("Failed to load pre-defined types: %v", err)
	}

	tests := []struct {
		name    string
		obj     map[string]interface{}
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := transformer.buildOpenAPISchema(tt.obj)
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

	preDefinedTypes := map[string]interface{}{
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
	}

	err := transformer.loadPreDefinedTypes(preDefinedTypes)
	if err != nil {
		t.Fatalf("LoadPreDefinedTypes() error = %v", err)
	}

	if len(transformer.preDefinedTypes) != 2 {
		t.Errorf("LoadPreDefinedTypes() loaded %d types, want 2", len(transformer.preDefinedTypes))
	}

	// Check Person type
	personType, ok := transformer.preDefinedTypes["Person"]
	if !ok {
		t.Errorf("LoadPreDefinedTypes() did not load 'Person' type")
	}

	expectedPersonType := extv1.JSONSchemaProps{
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
	}

	if !reflect.DeepEqual(personType, expectedPersonType) {
		t.Errorf("LoadPreDefinedTypes() 'Person' type = %v, want %v", personType, expectedPersonType)
	}

	// Check Company type
	companyType, ok := transformer.preDefinedTypes["Company"]
	if !ok {
		t.Errorf("LoadPreDefinedTypes() did not load 'Company' type")
	}

	expectedCompanyType := extv1.JSONSchemaProps{
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
	}

	if !reflect.DeepEqual(companyType, expectedCompanyType) {
		t.Errorf("LoadPreDefinedTypes() 'Company' type = %v, want %v", companyType, expectedCompanyType)
	}
}
