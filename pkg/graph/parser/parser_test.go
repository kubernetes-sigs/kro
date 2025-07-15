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

package parser

import (
	"reflect"
	"testing"

	"k8s.io/kube-openapi/pkg/validation/spec"

	"github.com/kro-run/kro/pkg/graph/variable"
)

func TestParseResource(t *testing.T) {
	t.Run("Simple resource with various types", func(t *testing.T) {
		resource := map[string]interface{}{
			"stringField": "${string.value}",
			"intField":    "${int.value}",
			"boolField":   "${bool.value}",
			"nestedObject": map[string]interface{}{
				"nestedString":         "${nested.string}",
				"nestedStringMultiple": "${nested.string1}-${nested.string2}",
			},
			"simpleArray": []interface{}{
				"${array[0]}",
				"${array[1]}",
			},
			"mapField": map[string]interface{}{
				"key1": "${map.key1}",
				"key2": "${map.key2}",
			},
			"specialCharacters": map[string]interface{}{
				"simpleAnnotation":     "${simpleannotation}",
				"doted.annotation.key": "${dotedannotationvalue}",
				"":                     "${emptyannotation}",
				"array.name.with.dots": []interface{}{
					"${value}",
				},
			},
			"schemalessField": map[string]interface{}{
				"key":       "value",
				"something": "${schemaless.value}",
				"nestedSomething": map[string]interface{}{
					"key":    "value",
					"nested": "${schemaless.nested.value}",
				},
			},
		}

		schema := &spec.Schema{
			SchemaProps: spec.SchemaProps{
				Type: []string{"object"},
				Properties: map[string]spec.Schema{
					"stringField": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
					"intField":    {SchemaProps: spec.SchemaProps{Type: []string{"integer"}}},
					"boolField":   {SchemaProps: spec.SchemaProps{Type: []string{"boolean"}}},
					"nestedObject": {
						SchemaProps: spec.SchemaProps{
							Type: []string{"object"},
							Properties: map[string]spec.Schema{
								"nestedString":         {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
								"nestedStringMultiple": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
							},
						},
					},
					"simpleArray": {
						SchemaProps: spec.SchemaProps{
							Type: []string{"array"},
							Items: &spec.SchemaOrArray{
								Schema: &spec.Schema{
									SchemaProps: spec.SchemaProps{Type: []string{"string"}},
								},
							},
						},
					},
					"mapField": {
						SchemaProps: spec.SchemaProps{
							Type: []string{"object"},
							AdditionalProperties: &spec.SchemaOrBool{
								Allows: true,
								Schema: &spec.Schema{
									SchemaProps: spec.SchemaProps{Type: []string{"string"}},
								},
							},
						},
					},
					"specialCharacters": {
						SchemaProps: spec.SchemaProps{
							Type: []string{"object"},
							Properties: map[string]spec.Schema{
								"simpleAnnotation":     {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
								"doted.annotation.key": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
								"":                     {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
								"array.name.with.dots": {
									SchemaProps: spec.SchemaProps{
										Type: []string{"array"},
										Items: &spec.SchemaOrArray{
											Schema: &spec.Schema{
												SchemaProps: spec.SchemaProps{Type: []string{"string"}},
											},
										},
									},
								},
							},
						},
					},
					"schemalessField": {
						SchemaProps: spec.SchemaProps{
							Type: []string{"object"},
						},
						VendorExtensible: spec.VendorExtensible{
							Extensions: spec.Extensions{
								"x-kubernetes-preserve-unknown-fields": true,
							},
						},
					},
				},
			},
		}

		expectedExpressions := []variable.FieldDescriptor{
			{Path: "stringField", Expressions: []string{"string.value"}, ExpectedTypes: []string{"string"}, StandaloneExpression: true},
			{Path: "intField", Expressions: []string{"int.value"}, ExpectedTypes: []string{"integer"}, StandaloneExpression: true},
			{Path: "boolField", Expressions: []string{"bool.value"}, ExpectedTypes: []string{"boolean"}, StandaloneExpression: true},
			{Path: "nestedObject.nestedString", Expressions: []string{"nested.string"}, ExpectedTypes: []string{"string"}, StandaloneExpression: true},
			{Path: "nestedObject.nestedStringMultiple", Expressions: []string{"nested.string1", "nested.string2"}, ExpectedTypes: []string{"string"}, StandaloneExpression: false},
			{Path: "simpleArray[0]", Expressions: []string{"array[0]"}, ExpectedTypes: []string{"string"}, StandaloneExpression: true},
			{Path: "simpleArray[1]", Expressions: []string{"array[1]"}, ExpectedTypes: []string{"string"}, StandaloneExpression: true},
			{Path: "mapField.key1", Expressions: []string{"map.key1"}, ExpectedTypes: []string{"string"}, StandaloneExpression: true},
			{Path: "mapField.key2", Expressions: []string{"map.key2"}, ExpectedTypes: []string{"string"}, StandaloneExpression: true},
			{Path: "specialCharacters.simpleAnnotation", Expressions: []string{"simpleannotation"}, ExpectedTypes: []string{"string"}, StandaloneExpression: true},
			{Path: "specialCharacters[\"doted.annotation.key\"]", Expressions: []string{"dotedannotationvalue"}, ExpectedTypes: []string{"string"}, StandaloneExpression: true},
			{Path: "specialCharacters[\"\"]", Expressions: []string{"emptyannotation"}, ExpectedTypes: []string{"string"}, StandaloneExpression: true},
			{Path: "specialCharacters[\"array.name.with.dots\"][0]", Expressions: []string{"value"}, ExpectedTypes: []string{"string"}, StandaloneExpression: true},
			{Path: "schemalessField.something", Expressions: []string{"schemaless.value"}, ExpectedTypes: []string{"string"}, StandaloneExpression: true},
			{Path: "schemalessField.nestedSomething.nested", Expressions: []string{"schemaless.nested.value"}, ExpectedTypes: []string{"string"}, StandaloneExpression: true},
		}

		expressions, err := ParseResource(resource, schema)
		if err != nil {
			t.Fatalf("ParseResource() error = %v", err)
		}

		if !areEqualExpressionFields(expressions, expectedExpressions) {
			for i, expr := range expressions {
				t.Logf("Got %d: %+v", i, expr)
			}
			for i, expr := range expectedExpressions {
				t.Logf("Want %d: %+v", i, expr)
			}
		}
	})

	t.Run("Invalid type for field", func(t *testing.T) {
		resource := map[string]interface{}{
			"intField": "invalid-integer",
		}

		schema := &spec.Schema{
			SchemaProps: spec.SchemaProps{
				Type: []string{"object"},
				Properties: map[string]spec.Schema{
					"intField": {SchemaProps: spec.SchemaProps{Type: []string{"integer"}}},
				},
			},
		}

		_, err := ParseResource(resource, schema)
		if err == nil {
			t.Errorf("ParseResource() expected error, got nil")
		}
	})
}

func TestTypeMismatches(t *testing.T) {
	testCases := []struct {
		name     string
		resource map[string]interface{}
		schema   *spec.Schema
		wantErr  bool
	}{
		{
			name: "String instead of integer",
			resource: map[string]interface{}{
				"intField": "not an int",
			},
			schema: &spec.Schema{
				SchemaProps: spec.SchemaProps{
					Type: []string{"object"},
					Properties: map[string]spec.Schema{
						"intField": {SchemaProps: spec.SchemaProps{Type: []string{"integer"}}},
					},
				},
			},
			wantErr: true,
		},
		{
			name: "Integer instead of string",
			resource: map[string]interface{}{
				"stringField": 123,
			},
			schema: &spec.Schema{
				SchemaProps: spec.SchemaProps{
					Type: []string{"object"},
					Properties: map[string]spec.Schema{
						"stringField": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
					},
				},
			},
			wantErr: true,
		},
		{
			name: "Boolean instead of number",
			resource: map[string]interface{}{
				"numberField": true,
			},
			schema: &spec.Schema{
				SchemaProps: spec.SchemaProps{
					Type: []string{"object"},
					Properties: map[string]spec.Schema{
						"numberField": {SchemaProps: spec.SchemaProps{Type: []string{"number"}}},
					},
				},
			},
			wantErr: true,
		},
		{
			name: "Array instead of object",
			resource: map[string]interface{}{
				"objectField": []interface{}{"not", "an", "object"},
			},
			schema: &spec.Schema{
				SchemaProps: spec.SchemaProps{
					Type: []string{"object"},
					Properties: map[string]spec.Schema{
						"objectField": {SchemaProps: spec.SchemaProps{Type: []string{"object"}}},
					},
				},
			},
			wantErr: true,
		},
		{
			name: "Object instead of array",
			resource: map[string]interface{}{
				"arrayField": map[string]interface{}{"key": "value"},
			},
			schema: &spec.Schema{
				SchemaProps: spec.SchemaProps{
					Type: []string{"object"},
					Properties: map[string]spec.Schema{
						"arrayField": {SchemaProps: spec.SchemaProps{Type: []string{"array"}}},
					},
				},
			},
			wantErr: true,
		},
		{
			name: "Nil schema",
			resource: map[string]interface{}{
				"field": "value",
			},
			schema:  nil,
			wantErr: true,
		},
		{
			name: "Schema with OneOf",
			resource: map[string]interface{}{
				"field": "value",
			},
			schema: &spec.Schema{
				SchemaProps: spec.SchemaProps{
					Type: []string{"object"},
					Properties: map[string]spec.Schema{
						"field": {
							SchemaProps: spec.SchemaProps{
								Type: []string{"Int", "String"},
								OneOf: []spec.Schema{
									{SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
									{SchemaProps: spec.SchemaProps{Type: []string{"integer"}}},
								},
							},
						},
					},
				},
			},
			wantErr: false,
		},
		{
			name: "Schema with empty type",
			resource: map[string]interface{}{
				"field": "value",
			},
			schema: &spec.Schema{
				SchemaProps: spec.SchemaProps{
					Type: []string{},
				},
			},
			wantErr: true,
		},
		{
			name: "Valid types (no mismatch)",
			resource: map[string]interface{}{
				"stringField": "valid string",
				"intField":    42,
				"boolField":   true,
				"numberField": 3.14,
				"objectField": map[string]interface{}{"key": "value"},
				"arrayField":  []interface{}{1, 2, 3},
			},
			schema: &spec.Schema{
				SchemaProps: spec.SchemaProps{
					Type: []string{"object"},
					Properties: map[string]spec.Schema{
						"stringField": {
							SchemaProps: spec.SchemaProps{Type: []string{"string"}},
						},
						"intField": {
							SchemaProps: spec.SchemaProps{Type: []string{"integer"}},
						},
						"boolField": {
							SchemaProps: spec.SchemaProps{Type: []string{"boolean"}},
						},
						"numberField": {
							SchemaProps: spec.SchemaProps{Type: []string{"number"}},
						},
						"objectField": {
							SchemaProps: spec.SchemaProps{
								Type: []string{"object"},
								Properties: map[string]spec.Schema{
									"key": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
								},
							},
						},
						"arrayField": {
							SchemaProps: spec.SchemaProps{
								Type: []string{"array"},
								Items: &spec.SchemaOrArray{
									Schema: &spec.Schema{
										SchemaProps: spec.SchemaProps{Type: []string{"integer"}},
									},
								},
							},
						},
					},
				},
			},
			wantErr: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseResource(tc.resource, tc.schema)
			if (err != nil) != tc.wantErr {
				t.Errorf("ParseResource() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}
func TestParseWithExpectedSchema(t *testing.T) {
	resource := map[string]interface{}{
		"stringField": "${string.value}",
		"objectField": "${object.value}", // Entire object as a CEL expression
		"nestedObjectField": map[string]interface{}{
			"nestedString": "${nested.string}",
			"nestedObject": map[string]interface{}{
				"deepNested": "${deep.nested}",
			},
		},
		"arrayField": []interface{}{
			"${array[0]}",
			map[string]interface{}{
				"objectInArray": "${object.in.array}",
			},
		},
	}

	stringFieldSchema := spec.Schema{SchemaProps: spec.SchemaProps{Type: []string{"string"}}}
	objectFieldSchema := spec.Schema{
		SchemaProps: spec.SchemaProps{
			Type: []string{"object"},
			Properties: map[string]spec.Schema{
				"key1": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
				"key2": {SchemaProps: spec.SchemaProps{Type: []string{"integer"}}},
			},
		},
	}
	nestedObjectFieldSchema := spec.Schema{
		SchemaProps: spec.SchemaProps{
			Type: []string{"object"},
			Properties: map[string]spec.Schema{
				"nestedString": {
					SchemaProps: spec.SchemaProps{Type: []string{"string"}},
				},
				"nestedObject": {
					SchemaProps: spec.SchemaProps{
						Type: []string{"object"},
						Properties: map[string]spec.Schema{
							"deepNested": {
								SchemaProps: spec.SchemaProps{Type: []string{"string"}},
							},
						},
					},
				},
			},
		},
	}
	arrayFieldSchema := spec.Schema{
		SchemaProps: spec.SchemaProps{
			Type: []string{"array"},
			Items: &spec.SchemaOrArray{
				Schema: &spec.Schema{
					SchemaProps: spec.SchemaProps{
						Type: []string{"object"},
						Properties: map[string]spec.Schema{
							"objectInArray": {
								SchemaProps: spec.SchemaProps{Type: []string{"string"}},
							},
						},
						AdditionalProperties: &spec.SchemaOrBool{Allows: true},
					},
				},
			},
		},
	}
	nestedObjectNestedStringSchema := nestedObjectFieldSchema.Properties["nestedString"]
	deepNestedSchema := nestedObjectFieldSchema.Properties["nestedObject"].Properties["deepNested"]
	objectInArraySchema := arrayFieldSchema.Items.Schema.Properties["objectInArray"]

	schema := &spec.Schema{
		SchemaProps: spec.SchemaProps{
			Type: []string{"object"},
			Properties: map[string]spec.Schema{
				"stringField":       stringFieldSchema,
				"objectField":       objectFieldSchema,
				"nestedObjectField": nestedObjectFieldSchema,
				"arrayField":        arrayFieldSchema,
			},
		},
	}

	expressions, err := ParseResource(resource, schema)
	if err != nil {
		t.Fatalf("ParseResource() error = %v", err)
	}

	expectedExpressions := map[string]variable.FieldDescriptor{
		"stringField":                               {Path: "stringField", Expressions: []string{"string.value"}, ExpectedTypes: []string{"string"}, ExpectedSchema: &stringFieldSchema, StandaloneExpression: true},
		"objectField":                               {Path: "objectField", Expressions: []string{"object.value"}, ExpectedTypes: []string{"object"}, ExpectedSchema: &objectFieldSchema, StandaloneExpression: true},
		"nestedObjectField.nestedString":            {Path: "nestedObjectField.nestedString", Expressions: []string{"nested.string"}, ExpectedTypes: []string{"string"}, ExpectedSchema: &nestedObjectNestedStringSchema, StandaloneExpression: true},
		"nestedObjectField.nestedObject.deepNested": {Path: "nestedObjectField.nestedObject.deepNested", Expressions: []string{"deep.nested"}, ExpectedTypes: []string{"string"}, ExpectedSchema: &deepNestedSchema, StandaloneExpression: true},
		"arrayField[0]":                             {Path: "arrayField[0]", Expressions: []string{"array[0]"}, ExpectedTypes: []string{"object"}, ExpectedSchema: arrayFieldSchema.Items.Schema, StandaloneExpression: true},
		"arrayField[1].objectInArray":               {Path: "arrayField[1].objectInArray", Expressions: []string{"object.in.array"}, ExpectedTypes: []string{"string"}, ExpectedSchema: &objectInArraySchema, StandaloneExpression: true},
	}

	if len(expressions) != len(expectedExpressions) {
		t.Fatalf("Expected %d expressions, got %d", len(expectedExpressions), len(expressions))
	}

	for _, expr := range expressions {
		expected, ok := expectedExpressions[expr.Path]
		if !ok {
			t.Errorf("Unexpected expression path: %s", expr.Path)
			continue
		}

		if !reflect.DeepEqual(expr.Expressions, expected.Expressions) {
			t.Errorf("Path %s: expected expressions %v, got %v", expr.Path, expected.Expressions, expr.Expressions)
		}
		if !areEqualSlices(expr.ExpectedTypes, expected.ExpectedTypes) {
			t.Errorf("Path %s: expected type %s, got %s", expr.Path, expected.ExpectedTypes, expr.ExpectedTypes)
		}
		if expr.StandaloneExpression != expected.StandaloneExpression {
			t.Errorf("Path %s: expected OneShotCEL %v, got %v", expr.Path, expected.StandaloneExpression, expr.StandaloneExpression)
		}
		if !reflect.DeepEqual(expr.ExpectedSchema, expected.ExpectedSchema) {
			t.Errorf("Path %s: schema mismatch", expr.Path)
			t.Errorf("Expected schema: %+v", expected.ExpectedSchema)
			t.Errorf("Got schema: %+v", expr.ExpectedSchema)
		}

		// remove the matched expression from the map
		// NOTE(a-hilaly): since the object is a map, the order of the expressions is not guaranteed
		// so we need to check if all the expected expressions are found.
		delete(expectedExpressions, expr.Path)
	}

	// check if there are any expected expressions that weren't found
	if len(expectedExpressions) > 0 {
		for path := range expectedExpressions {
			t.Errorf("expected expression not found: %s", path)
		}
	}
}

func TestParserEdgeCases(t *testing.T) {
	testCases := []struct {
		name          string
		schema        *spec.Schema
		resource      interface{}
		expectedError string
	}{
		{
			name: "array missing Items.Schema and Properties",
			schema: &spec.Schema{
				SchemaProps: spec.SchemaProps{
					Type:  []string{"array"},
					Items: &spec.SchemaOrArray{},
				},
			},
			resource:      []interface{}{"test"},
			expectedError: "invalid array schema for path : neither Items.Schema nor Properties are defined",
		},
		{
			name: "Type mismatch: string/number",
			schema: &spec.Schema{
				SchemaProps: spec.SchemaProps{
					Type: []string{"string"},
				},
			},
			resource:      42,
			expectedError: "unexpected type for path : int, expectedTypes: [string]",
		},
		{
			name: "Type mismatch: object/array",
			schema: &spec.Schema{
				SchemaProps: spec.SchemaProps{
					Type: []string{"object"},
				},
			},
			resource:      []interface{}{"test"},
			expectedError: "expected array type for path , got [test]",
		},
		{
			name: "Type mismatch: bool/string",
			schema: &spec.Schema{
				SchemaProps: spec.SchemaProps{
					Type: []string{"boolean"},
				},
			},
			resource:      "true",
			expectedError: "expected string type or AdditionalProperties for path , got true",
		},
		{
			name: "Type mismatch integer/float",
			schema: &spec.Schema{
				SchemaProps: spec.SchemaProps{
					Type: []string{"integer"},
				},
			},
			resource:      3.14,
			expectedError: "expected integer type for path , got float64",
		},
		{
			name: "Type mismatch: number/bool",
			schema: &spec.Schema{
				SchemaProps: spec.SchemaProps{
					Type: []string{"number"},
				},
			},
			resource:      true,
			expectedError: "expected number type for path , got bool",
		},
		{
			name: "Type mismatch: array/object",
			schema: &spec.Schema{
				SchemaProps: spec.SchemaProps{
					Type: []string{"array"},
					Items: &spec.SchemaOrArray{
						Schema: &spec.Schema{
							SchemaProps: spec.SchemaProps{
								Type: []string{"string"},
							},
						},
					},
				},
			},
			resource:      map[string]interface{}{"key": "value"},
			expectedError: "expected object type or AdditionalProperties allowed for path , got map[key:value]",
		},
		{
			name: "unknown property for object ..",
			schema: &spec.Schema{
				SchemaProps: spec.SchemaProps{
					Type: []string{"object"},
					Properties: map[string]spec.Schema{
						"name": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
						"age":  {SchemaProps: spec.SchemaProps{Type: []string{"integer"}}},
					},
				},
			},
			resource: map[string]interface{}{
				"name":    "random parrot",
				"surname": "the parrot",
			},
			expectedError: "error getting field schema for path .surname: schema not found for field surname",
		},
		{
			name: "valid schema and resource - no error expected",
			schema: &spec.Schema{
				SchemaProps: spec.SchemaProps{
					Type: []string{"object"},
					Properties: map[string]spec.Schema{
						"name": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
						"age":  {SchemaProps: spec.SchemaProps{Type: []string{"integer"}}},
					},
				},
			},
			resource: map[string]interface{}{
				"name": "John",
				"age":  30,
			},
			expectedError: "",
		},
		{
			name: "schema with x-kubernetes-preserve-unknown-fields",
			schema: &spec.Schema{
				SchemaProps: spec.SchemaProps{
					Type: []string{"object"},
				},
				VendorExtensible: spec.VendorExtensible{
					Extensions: spec.Extensions{
						"x-kubernetes-preserve-unknown-fields": true,
					},
				},
			},
			resource:      map[string]interface{}{"name": "John", "age": 30},
			expectedError: "",
		},
		{
			name: "structured object with nested x-kubernetes-preserve-unknown-fields",
			schema: &spec.Schema{
				SchemaProps: spec.SchemaProps{
					Type: []string{"object"},
					Properties: map[string]spec.Schema{
						"id": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
						"metadata": {
							SchemaProps: spec.SchemaProps{
								Type: []string{"object"},
							},
							VendorExtensible: spec.VendorExtensible{
								Extensions: spec.Extensions{
									"x-kubernetes-preserve-unknown-fields": true,
								},
							},
						},
					},
				},
			},
			resource: map[string]interface{}{"id": "123", "metadata": map[string]interface{}{
				"name": "John", "age": 30, "test": "${test.value}",
			}},
			expectedError: "",
		},
		{
			name: "invalid schema",
			schema: &spec.Schema{
				SchemaProps: spec.SchemaProps{
					Type: []string{"object"},
					Properties: map[string]spec.Schema{
						"name": {SchemaProps: spec.SchemaProps{Type: nil}},
					},
				},
			},
			resource: map[string]interface{}{
				"name": "John",
			},
			expectedError: "schema at path name has no valid type, OneOf, AnyOf, or AdditionalProperties",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseResource(tc.resource, tc.schema, "")
			if tc.expectedError == "" {
				if err != nil {
					t.Errorf("Expected no error, but got: %s", err.Error())
				}
			} else {
				if err == nil {
					t.Errorf("Expected error: %s, but got nil", tc.expectedError)
				} else if err.Error() != tc.expectedError {
					t.Errorf("Expected error: %s, but got: %s", tc.expectedError, err.Error())
				}
			}
		})
	}
}

func TestJoinPathAndFieldName(t *testing.T) {
	tests := []struct {
		name      string
		path      string
		fieldName string
		want      string
	}{
		{"empty path and field", "", "", `[""]`},
		{"empty path", "", "field", "field"},
		{"empty field", "path", "", `path[""]`},
		{"simple join", "path", "field", "path.field"},
		{"dotted field", "path", "field.name", `path["field.name"]`},
		{"empty path with dotted field", "", "field.name", `["field.name"]`},
		{"nested path", "path.to", "field", "path.to.field"},
		{"nested path with dotted field", "path.to", "field.name", `path.to["field.name"]`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := joinPathAndFieldName(tt.path, tt.fieldName)
			if got != tt.want {
				t.Errorf("joinPathAndFieldName(%q, %q) = %q, want %q",
					tt.path, tt.fieldName, got, tt.want)
			}
		})
	}
}

func TestPartScalerTypesShortSpecTypes(t *testing.T) {
	tests := []struct {
		name          string
		shortSpecType []string
		field         interface{}
	}{
		{"int short type for integer", []string{"int"}, 42},
		{"bool short type for boolean", []string{"bool"}, true},
	}

	dummySchema := &spec.Schema{}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseScalarTypes(tt.field, dummySchema, "spec.someitem", tt.shortSpecType)
			if err != nil {
				t.Errorf("Expected %T resolved for %s, but got error: %s",
					tt.field, tt.shortSpecType, err.Error())
			}
		})
	}
}

func TestXKubernetesIntOrString(t *testing.T) {
	schema := &spec.Schema{
		SchemaProps: spec.SchemaProps{
			Type: []string{"object"},
			Properties: map[string]spec.Schema{
				"myField": {
					SchemaProps: spec.SchemaProps{
						// default "integer",
						Type: []string{"integer"},
					},
					VendorExtensible: spec.VendorExtensible{
						Extensions: spec.Extensions{
							"x-kubernetes-int-or-string": true,
						},
					},
				},
			},
		},
	}

	tests := []struct {
		name       string
		resource   map[string]interface{}
		wantErr    bool
		wantErrMsg string
	}{
		{
			name: "Field is integer",
			resource: map[string]interface{}{
				"myField": 42,
			},
			wantErr: false,
		},
		{
			name: "Field is string",
			resource: map[string]interface{}{
				"myField": "forty-two",
			},
			wantErr: false,
		},
		{
			name: "Field is bool (invalid)",
			resource: map[string]interface{}{
				"myField": true,
			},
			wantErr:    true,
			wantErrMsg: "expected integer type for path myField, got bool",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseResource(tc.resource, schema)
			if tc.wantErr && err == nil {
				t.Errorf("Expected error but got none")
			} else if !tc.wantErr && err != nil {
				t.Errorf("Did not expect error but got: %v", err)
			} else if tc.wantErr && err != nil {
				if tc.wantErrMsg != "" && err.Error() != tc.wantErrMsg {
					t.Errorf("Expected error message %q, got %q", tc.wantErrMsg, err.Error())
				}
			}
		})
	}
}

func TestNestedXKubernetesIntOrString(t *testing.T) {
	// Schema: outerObject is an object that has a property "nestedField"
	// that can be either an integer or a string.
	t.Run("Nested x-kubernetes-int-or-string in object", func(t *testing.T) {
		schema := &spec.Schema{
			SchemaProps: spec.SchemaProps{
				Type: []string{"object"},
				Properties: map[string]spec.Schema{
					"outerObject": {
						SchemaProps: spec.SchemaProps{
							Type: []string{"object"},
							Properties: map[string]spec.Schema{
								"nestedField": {
									SchemaProps: spec.SchemaProps{
										Type: []string{"integer"},
									},
									VendorExtensible: spec.VendorExtensible{
										Extensions: spec.Extensions{
											"x-kubernetes-int-or-string": true,
										},
									},
								},
							},
						},
					},
				},
			},
		}

		testCases := []struct {
			name          string
			resource      map[string]interface{}
			wantErr       bool
			expectedError string
		}{
			{
				name: "nestedField as integer",
				resource: map[string]interface{}{
					"outerObject": map[string]interface{}{
						"nestedField": 123,
					},
				},
				wantErr: false,
			},
			{
				name: "nestedField as string",
				resource: map[string]interface{}{
					"outerObject": map[string]interface{}{
						"nestedField": "one-two-three",
					},
				},
				wantErr: false,
			},
			{
				name: "nestedField as bool (invalid)",
				resource: map[string]interface{}{
					"outerObject": map[string]interface{}{
						"nestedField": true,
					},
				},
				wantErr:       true,
				expectedError: "expected integer type for path outerObject.nestedField, got bool",
			},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				_, err := ParseResource(tc.resource, schema)
				if tc.wantErr && err == nil {
					t.Errorf("Expected error, but got none")
				} else if !tc.wantErr && err != nil {
					t.Errorf("Did not expect error, but got: %v", err)
				} else if tc.wantErr && err != nil && tc.expectedError != "" && err.Error() != tc.expectedError {
					t.Errorf("Expected error message %q, got %q", tc.expectedError, err.Error())
				}
			})
		}
	})
}

func TestOneOfAndAnyOf(t *testing.T) {
	testCases := []struct {
		name          string
		schema        *spec.Schema
		resource      interface{}
		wantErr       bool
		expectedError string
	}{
		{
			name: "Valid OneOf - matches first schema (string)",
			schema: &spec.Schema{
				SchemaProps: spec.SchemaProps{
					Type: []string{"object"},
					Properties: map[string]spec.Schema{
						"field": {
							SchemaProps: spec.SchemaProps{
								OneOf: []spec.Schema{
									{SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
									{SchemaProps: spec.SchemaProps{Type: []string{"integer"}}},
								},
							},
						},
					},
				},
			},
			resource: map[string]interface{}{
				"field": "valid string",
			},
			wantErr: false,
		},
		{
			name: "Valid OneOf - matches second schema (integer)",
			schema: &spec.Schema{
				SchemaProps: spec.SchemaProps{
					Type: []string{"object"},
					Properties: map[string]spec.Schema{
						"field": {
							SchemaProps: spec.SchemaProps{
								OneOf: []spec.Schema{
									{SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
									{SchemaProps: spec.SchemaProps{Type: []string{"integer"}}},
								},
							},
						},
					},
				},
			},
			resource: map[string]interface{}{
				"field": 42,
			},
			wantErr: false,
		},
		{
			name: "Invalid OneOf - does not match any schema",
			schema: &spec.Schema{
				SchemaProps: spec.SchemaProps{
					Type: []string{"object"},
					Properties: map[string]spec.Schema{
						"field": {
							SchemaProps: spec.SchemaProps{
								OneOf: []spec.Schema{
									{SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
									{SchemaProps: spec.SchemaProps{Type: []string{"integer"}}},
								},
							},
						},
					},
				},
			},
			resource: map[string]interface{}{
				"field": true,
			},
			wantErr:       true,
			expectedError: "expected integer type for path field, got bool",
		},
		{
			name: "Valid AnyOf - matches one schema (string)",
			schema: &spec.Schema{
				SchemaProps: spec.SchemaProps{
					Type: []string{"object"},
					Properties: map[string]spec.Schema{
						"field": {
							SchemaProps: spec.SchemaProps{
								AnyOf: []spec.Schema{
									{SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
									{SchemaProps: spec.SchemaProps{Type: []string{"integer"}}},
								},
							},
						},
					},
				},
			},
			resource: map[string]interface{}{
				"field": "valid string",
			},
			wantErr: false,
		},
		{
			name: "Valid AnyOf - matches one schema (integer)",
			schema: &spec.Schema{
				SchemaProps: spec.SchemaProps{
					Type: []string{"object"},
					Properties: map[string]spec.Schema{
						"field": {
							SchemaProps: spec.SchemaProps{
								AnyOf: []spec.Schema{
									{SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
									{SchemaProps: spec.SchemaProps{Type: []string{"integer"}}},
								},
							},
						},
					},
				},
			},
			resource: map[string]interface{}{
				"field": 42,
			},
			wantErr: false,
		},
		{
			name: "Invalid AnyOf - does not match any schema",
			schema: &spec.Schema{
				SchemaProps: spec.SchemaProps{
					Type: []string{"object"},
					Properties: map[string]spec.Schema{
						"field": {
							SchemaProps: spec.SchemaProps{
								AnyOf: []spec.Schema{
									{SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
									{SchemaProps: spec.SchemaProps{Type: []string{"integer"}}},
								},
							},
						},
					},
				},
			},
			resource: map[string]interface{}{
				"field": true,
			},
			wantErr:       true,
			expectedError: "expected integer type for path field, got bool",
		},
		{
			name: "Nested OneOf - valid nested schema (string)",
			schema: &spec.Schema{
				SchemaProps: spec.SchemaProps{
					Type: []string{"object"},
					Properties: map[string]spec.Schema{
						"nestedField": {
							SchemaProps: spec.SchemaProps{
								Type: []string{"object"},
								Properties: map[string]spec.Schema{
									"innerField": {
										SchemaProps: spec.SchemaProps{
											OneOf: []spec.Schema{
												{SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
												{SchemaProps: spec.SchemaProps{Type: []string{"integer"}}},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			resource: map[string]interface{}{
				"nestedField": map[string]interface{}{
					"innerField": "valid string",
				},
			},
			wantErr: false,
		},
		{
			name: "Nested OneOf - invalid nested schema",
			schema: &spec.Schema{
				SchemaProps: spec.SchemaProps{
					Type: []string{"object"},
					Properties: map[string]spec.Schema{
						"nestedField": {
							SchemaProps: spec.SchemaProps{
								Type: []string{"object"},
								Properties: map[string]spec.Schema{
									"innerField": {
										SchemaProps: spec.SchemaProps{
											OneOf: []spec.Schema{
												{SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
												{SchemaProps: spec.SchemaProps{Type: []string{"integer"}}},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			resource: map[string]interface{}{
				"nestedField": map[string]interface{}{
					"innerField": true,
				},
			},
			wantErr:       true,
			expectedError: "expected integer type for path nestedField.innerField, got bool",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseResource(tc.resource, tc.schema, "")
			if tc.wantErr && err == nil {
				t.Errorf("Expected error but got none")
			} else if !tc.wantErr && err != nil {
				t.Errorf("Did not expect error but got: %v", err)
			} else if tc.wantErr && err != nil && tc.expectedError != "" && err.Error() != tc.expectedError {
				t.Errorf("Expected error message %q, got %q", tc.expectedError, err.Error())
			}
		})
	}
}
func TestOneOfWithStructuralConstraints(t *testing.T) {
	t.Run("networkRef style schema with structural constraints", func(t *testing.T) {
		schema := &spec.Schema{
			SchemaProps: spec.SchemaProps{
				Type: []string{"object"},
				Properties: map[string]spec.Schema{
					"networkRef": {
						SchemaProps: spec.SchemaProps{
							OneOf: []spec.Schema{
								{
									SchemaProps: spec.SchemaProps{
										Not: &spec.Schema{
											SchemaProps: spec.SchemaProps{
												Required: []string{"external"},
											},
										},
										Required: []string{"name"},
									},
								},
								{
									SchemaProps: spec.SchemaProps{
										Not: &spec.Schema{
											SchemaProps: spec.SchemaProps{
												AnyOf: []spec.Schema{
													{SchemaProps: spec.SchemaProps{Required: []string{"name"}}},
													{SchemaProps: spec.SchemaProps{Required: []string{"namespace"}}},
												},
											},
										},
										Required: []string{"external"},
									},
								},
							},
							Properties: map[string]spec.Schema{
								"name": {
									SchemaProps: spec.SchemaProps{
										Type: []string{"string"},
									},
								},
								"external": {
									SchemaProps: spec.SchemaProps{
										Type: []string{"string"},
									},
								},
								"namespace": {
									SchemaProps: spec.SchemaProps{
										Type: []string{"string"},
									},
								},
							},
						},
					},
				},
			},
		}

		resource := map[string]interface{}{
			"networkRef": map[string]interface{}{
				"name": "${network.metadata.name}",
			},
		}

		expressions, err := ParseResource(resource, schema)
		if err != nil {
			t.Fatalf("ParseResource() error = %v", err)
		}

		if len(expressions) != 1 {
			t.Fatalf("Expected 1 expression, got %d", len(expressions))
		}

		expected := variable.FieldDescriptor{
			Path:                 "networkRef.name",
			Expressions:          []string{"network.metadata.name"},
			ExpectedTypes:        []string{"string"},
			StandaloneExpression: true,
		}

		if !reflect.DeepEqual(expressions[0].Path, expected.Path) {
			t.Errorf("Expected path %s, got %s", expected.Path, expressions[0].Path)
		}

		if !reflect.DeepEqual(expressions[0].Expressions, expected.Expressions) {
			t.Errorf("Expressions mismatch: got %v, want %v", expressions[0].Expressions, expected.Expressions)
		}

		if !reflect.DeepEqual(expressions[0].ExpectedTypes, expected.ExpectedTypes) {
			t.Errorf("ExpectedTypes mismatch: got %v, want %v", expressions[0].ExpectedTypes, expected.ExpectedTypes)
		}

		if expressions[0].StandaloneExpression != expected.StandaloneExpression {
			t.Errorf("StandaloneExpression mismatch: got %v, want %v", expressions[0].StandaloneExpression, expected.StandaloneExpression)
		}
	})

	t.Run("networkRef with external reference", func(t *testing.T) {
		schema := &spec.Schema{
			SchemaProps: spec.SchemaProps{
				Type: []string{"object"},
				Properties: map[string]spec.Schema{
					"networkRef": {
						SchemaProps: spec.SchemaProps{
							OneOf: []spec.Schema{
								{
									SchemaProps: spec.SchemaProps{
										Not: &spec.Schema{
											SchemaProps: spec.SchemaProps{
												Required: []string{"external"},
											},
										},
										Required: []string{"name"},
									},
								},
								{
									SchemaProps: spec.SchemaProps{
										Not: &spec.Schema{
											SchemaProps: spec.SchemaProps{
												AnyOf: []spec.Schema{
													{SchemaProps: spec.SchemaProps{Required: []string{"name"}}},
													{SchemaProps: spec.SchemaProps{Required: []string{"namespace"}}},
												},
											},
										},
										Required: []string{"external"},
									},
								},
							},
							Properties: map[string]spec.Schema{
								"name": {
									SchemaProps: spec.SchemaProps{
										Type: []string{"string"},
									},
								},
								"external": {
									SchemaProps: spec.SchemaProps{
										Type: []string{"string"},
									},
								},
								"namespace": {
									SchemaProps: spec.SchemaProps{
										Type: []string{"string"},
									},
								},
							},
						},
					},
				},
			},
		}

		resource := map[string]interface{}{
			"networkRef": map[string]interface{}{
				"external": "${network.selfLink}",
			},
		}

		expressions, err := ParseResource(resource, schema)
		if err != nil {
			t.Fatalf("ParseResource() error = %v", err)
		}

		if len(expressions) != 1 {
			t.Fatalf("Expected 1 expression, got %d", len(expressions))
		}

		expected := variable.FieldDescriptor{
			Path:                 "networkRef.external",
			Expressions:          []string{"network.selfLink"},
			ExpectedTypes:        []string{"string"},
			StandaloneExpression: true,
		}

		if !reflect.DeepEqual(expressions[0].Path, expected.Path) {
			t.Errorf("Expected path %s, got %s", expected.Path, expressions[0].Path)
		}
		if !reflect.DeepEqual(expressions[0].Expressions, expected.Expressions) {
			t.Errorf("Expected expressions %v, got %v", expected.Expressions, expressions[0].Expressions)
		}
		if !reflect.DeepEqual(expressions[0].ExpectedTypes, expected.ExpectedTypes) {
			t.Errorf("Expected types %v, got %v", expected.ExpectedTypes, expressions[0].ExpectedTypes)
		}

		if expressions[0].StandaloneExpression != expected.StandaloneExpression {
			t.Errorf("StandaloneExpression mismatch: got %v, want %v", expressions[0].StandaloneExpression, expected.StandaloneExpression)
		}
	})
}

func TestPreserveUnknownFields(t *testing.T) {
	testCases := []struct {
		name                string
		schema              *spec.Schema
		resource            map[string]interface{}
		wantErr             bool
		expectedError       string
		expectedExpressions []variable.FieldDescriptor
	}{
		{
			name: "schema with no type but x-kubernetes-preserve-unknown-fields",
			schema: &spec.Schema{
				VendorExtensible: spec.VendorExtensible{
					Extensions: spec.Extensions{
						"x-kubernetes-preserve-unknown-fields": true,
					},
				},
			},
			resource: map[string]interface{}{
				"spec": map[string]interface{}{
					"template": "${template.value}",
				},
			},
			wantErr: false,
			expectedExpressions: []variable.FieldDescriptor{
				{
					Path:                 "spec.template",
					Expressions:          []string{"template.value"},
					ExpectedTypes:        []string{"any"},
					StandaloneExpression: true,
				},
			},
		},
		{
			name: "schema with no type but x-kubernetes-preserve-unknown-fields, expression in nested object",
			schema: &spec.Schema{
				VendorExtensible: spec.VendorExtensible{
					Extensions: spec.Extensions{
						"x-kubernetes-preserve-unknown-fields": true,
					},
				},
			},
			resource: map[string]interface{}{
				"spec": map[string]interface{}{
					"field1": "noisy string",
					"template": map[string]interface{}{
						"nested": []interface{}{
							map[string]interface{}{
								"key": "${template.value}",
							},
						},
					},
				},
			},
			wantErr: false,
			expectedExpressions: []variable.FieldDescriptor{
				{
					Path:                 "spec.template.nested[0].key",
					Expressions:          []string{"template.value"},
					ExpectedTypes:        []string{"any"},
					StandaloneExpression: true,
				},
			},
		},
		{
			name: "pulumi-style mixed schema",
			schema: &spec.Schema{
				SchemaProps: spec.SchemaProps{
					Type: []string{"object"},
					Properties: map[string]spec.Schema{
						"program": {
							SchemaProps: spec.SchemaProps{
								Type: []string{"object"},
								Properties: map[string]spec.Schema{
									"resources": {
										SchemaProps: spec.SchemaProps{
											Type: []string{"object"},
											AdditionalProperties: &spec.SchemaOrBool{
												Allows: true,
												Schema: &spec.Schema{
													SchemaProps: spec.SchemaProps{
														Type: []string{"object"},
														Properties: map[string]spec.Schema{
															"properties": {
																VendorExtensible: spec.VendorExtensible{
																	Extensions: spec.Extensions{
																		"x-kubernetes-preserve-unknown-fields": true,
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
							},
						},
					},
				},
			},
			resource: map[string]interface{}{
				"program": map[string]interface{}{
					"resources": map[string]interface{}{
						"app": map[string]interface{}{
							"properties": map[string]interface{}{
								"spec": map[string]interface{}{
									"name":   "${schema.spec.name}",
									"region": "${schema.spec.region}",
									"services": []interface{}{
										map[string]interface{}{
											"name":          "${schema.spec.name}-service",
											"instanceCount": "${schema.spec.instanceCount}",
										},
									},
								},
							},
						},
					},
				},
			},
			wantErr: false,
			expectedExpressions: []variable.FieldDescriptor{
				{
					Path:                 "program.resources.app.properties.spec.name",
					Expressions:          []string{"schema.spec.name"},
					ExpectedTypes:        []string{"any"},
					StandaloneExpression: true,
				},
				{
					Path:                 "program.resources.app.properties.spec.region",
					Expressions:          []string{"schema.spec.region"},
					ExpectedTypes:        []string{"any"},
					StandaloneExpression: true,
				},
				{
					Path:                 "program.resources.app.properties.spec.services[0].name",
					Expressions:          []string{"schema.spec.name"},
					ExpectedTypes:        []string{"any"},
					StandaloneExpression: false,
				},
				{
					Path:                 "program.resources.app.properties.spec.services[0].instanceCount",
					Expressions:          []string{"schema.spec.instanceCount"},
					ExpectedTypes:        []string{"any"},
					StandaloneExpression: true,
				},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			expressions, err := ParseResource(tc.resource, tc.schema)
			if tc.wantErr {
				if err == nil {
					t.Error("Expected error but got none")
				} else if tc.expectedError != "" && err.Error() != tc.expectedError {
					t.Errorf("Expected error message %q, got %q", tc.expectedError, err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("Did not expect error but got: %v", err)
					return
				}

				if len(expressions) != len(tc.expectedExpressions) {
					t.Errorf("Expected %d expressions, got %d", len(tc.expectedExpressions), len(expressions))
					t.Errorf("Got expressions:")
					for _, expr := range expressions {
						t.Errorf("  %+v", expr)
					}
					return
				}

				// Create maps for easier comparison
				actualMap := make(map[string]variable.FieldDescriptor)
				expectedMap := make(map[string]variable.FieldDescriptor)

				for _, expr := range expressions {
					actualMap[expr.Path] = expr
				}
				for _, expr := range tc.expectedExpressions {
					expectedMap[expr.Path] = expr
				}

				for path, expectedExpr := range expectedMap {
					actualExpr, ok := actualMap[path]
					if !ok {
						t.Errorf("Missing expected expression for path %s", path)
						continue
					}

					if !reflect.DeepEqual(actualExpr.Expressions, expectedExpr.Expressions) {
						t.Errorf("Path %s: expected expressions %v, got %v", path, expectedExpr.Expressions, actualExpr.Expressions)
					}
					if !reflect.DeepEqual(actualExpr.ExpectedTypes, expectedExpr.ExpectedTypes) {
						t.Errorf("Path %s: expected types %v, got %v", path, expectedExpr.ExpectedTypes, actualExpr.ExpectedTypes)
					}
					if actualExpr.StandaloneExpression != expectedExpr.StandaloneExpression {
						t.Errorf("Path %s: expected StandaloneExpression %v, got %v", path, expectedExpr.StandaloneExpression, actualExpr.StandaloneExpression)
					}
				}
			}
		})
	}
}

func TestCollectTypesFromSubSchemas(t *testing.T) {
	testCases := []struct {
		name       string
		subSchemas []spec.Schema
		wantTypes  []string
	}{
		{
			name: "simple types without constraints",
			subSchemas: []spec.Schema{
				{SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
				{SchemaProps: spec.SchemaProps{Type: []string{"integer"}}},
			},
			wantTypes: []string{"string", "integer"},
		},
		{
			name: "with Required constraint",
			subSchemas: []spec.Schema{
				{SchemaProps: spec.SchemaProps{
					Type:     []string{"string"},
					Required: []string{"field"},
				}},
			},
			wantTypes: []string{"object", "string"},
		},
		{
			name: "with Not constraint",
			subSchemas: []spec.Schema{
				{SchemaProps: spec.SchemaProps{
					Type: []string{"string"},
					Not: &spec.Schema{
						SchemaProps: spec.SchemaProps{Type: []string{"integer"}},
					},
				}},
			},
			wantTypes: []string{"object", "string"},
		},
		{
			name: "with both Required and Not constraints",
			subSchemas: []spec.Schema{
				{SchemaProps: spec.SchemaProps{
					Type:     []string{"string"},
					Required: []string{"field"},
					Not: &spec.Schema{
						SchemaProps: spec.SchemaProps{Type: []string{"integer"}},
					},
				}},
			},
			wantTypes: []string{"object", "string"},
		},
		{
			name: "duplicate types",
			subSchemas: []spec.Schema{
				{SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
				{SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
				{SchemaProps: spec.SchemaProps{
					Type:     []string{"string"},
					Required: []string{"field"},
				}},
			},
			wantTypes: []string{"object", "string"},
		},
		{
			name: "empty type",
			subSchemas: []spec.Schema{
				{SchemaProps: spec.SchemaProps{Type: []string{""}}},
			},
			wantTypes: []string{},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			gotTypes := collectTypesFromSubSchemas(tc.subSchemas)
			if !areEqualSlices(gotTypes, tc.wantTypes) {
				t.Errorf("collectTypesFromSubSchemas() = %v, want %v", gotTypes, tc.wantTypes)
			}
		})
	}
}
