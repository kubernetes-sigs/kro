// Copyright Amazon.com Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//	http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package parser

import (
	"reflect"
	"testing"

	"github.com/aws-controllers-k8s/symphony/internal/typesystem/variable"
	"k8s.io/kube-openapi/pkg/validation/spec"
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
				},
			},
		}

		expectedExpressions := []variable.FieldDescriptor{
			{Path: "stringField", Expressions: []string{"string.value"}, ExpectedType: "string", StandaloneExpression: true},
			{Path: "intField", Expressions: []string{"int.value"}, ExpectedType: "integer", StandaloneExpression: true},
			{Path: "boolField", Expressions: []string{"bool.value"}, ExpectedType: "boolean", StandaloneExpression: true},
			{Path: "nestedObject.nestedString", Expressions: []string{"nested.string"}, ExpectedType: "string", StandaloneExpression: true},
			{Path: "nestedObject.nestedStringMultiple", Expressions: []string{"nested.string1", "nested.string2"}, ExpectedType: "string", StandaloneExpression: false},
			{Path: "simpleArray[0]", Expressions: []string{"array[0]"}, ExpectedType: "string", StandaloneExpression: true},
			{Path: "simpleArray[1]", Expressions: []string{"array[1]"}, ExpectedType: "string", StandaloneExpression: true},
			{Path: "mapField.key1", Expressions: []string{"map.key1"}, ExpectedType: "string", StandaloneExpression: true},
			{Path: "mapField.key2", Expressions: []string{"map.key2"}, ExpectedType: "string", StandaloneExpression: true},
			{Path: "specialCharacters.simpleAnnotation", Expressions: []string{"simpleannotation"}, ExpectedType: "string", StandaloneExpression: true},
			{Path: "specialCharacters[\"doted.annotation.key\"]", Expressions: []string{"dotedannotationvalue"}, ExpectedType: "string", StandaloneExpression: true},
			{Path: "specialCharacters[\"\"]", Expressions: []string{"emptyannotation"}, ExpectedType: "string", StandaloneExpression: true},
			{Path: "specialCharacters[\"array.name.with.dots\"][0]", Expressions: []string{"value"}, ExpectedType: "string", StandaloneExpression: true},
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
			name: "Null value for non-nullable field",
			resource: map[string]interface{}{
				"nonNullableField": nil,
			},
			schema: &spec.Schema{
				SchemaProps: spec.SchemaProps{
					Type: []string{"object"},
					Properties: map[string]spec.Schema{
						"nonNullableField": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
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
		"stringField":                               {Path: "stringField", Expressions: []string{"string.value"}, ExpectedType: "string", ExpectedSchema: &stringFieldSchema, StandaloneExpression: true},
		"objectField":                               {Path: "objectField", Expressions: []string{"object.value"}, ExpectedType: "object", ExpectedSchema: &objectFieldSchema, StandaloneExpression: true},
		"nestedObjectField.nestedString":            {Path: "nestedObjectField.nestedString", Expressions: []string{"nested.string"}, ExpectedType: "string", ExpectedSchema: &nestedObjectNestedStringSchema, StandaloneExpression: true},
		"nestedObjectField.nestedObject.deepNested": {Path: "nestedObjectField.nestedObject.deepNested", Expressions: []string{"deep.nested"}, ExpectedType: "string", ExpectedSchema: &deepNestedSchema, StandaloneExpression: true},
		"arrayField[0]":                             {Path: "arrayField[0]", Expressions: []string{"array[0]"}, ExpectedType: "object", ExpectedSchema: arrayFieldSchema.Items.Schema, StandaloneExpression: true},
		"arrayField[1].objectInArray":               {Path: "arrayField[1].objectInArray", Expressions: []string{"object.in.array"}, ExpectedType: "string", ExpectedSchema: &objectInArraySchema, StandaloneExpression: true},
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
		if expr.ExpectedType != expected.ExpectedType {
			t.Errorf("Path %s: expected type %s, got %s", expr.Path, expected.ExpectedType, expr.ExpectedType)
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
			expectedError: "unexpected type for path : int",
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
			expectedError: "expected string type or AdditionalProperties allowed for path , got true",
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
