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

package parser

import (
	"sort"
	"strings"
	"testing"

	"k8s.io/kube-openapi/pkg/validation/spec"

	krocel "github.com/kubernetes-sigs/kro/pkg/cel"
	schemacache "github.com/kubernetes-sigs/kro/pkg/graph/schema"
	"github.com/kubernetes-sigs/kro/pkg/graph/variable"
)

// newSchema creates a spec.Schema with properly initialized VendorExtensible
// to avoid nil pointer panics in the OpenAPI library
func newSchema(props spec.SchemaProps) spec.Schema {
	return spec.Schema{
		SchemaProps: props,
		VendorExtensible: spec.VendorExtensible{
			Extensions: spec.Extensions{},
		},
	}
}

func TestParseResource(t *testing.T) {
	t.Run("Simple resource with various types", func(t *testing.T) {
		resource := map[string]any{
			"stringField": "${string.value}",
			"intField":    "${int.value}",
			"boolField":   "${bool.value}",
			"nestedObject": map[string]any{
				"nestedString":         "${nested.string}",
				"nestedStringMultiple": "${nested.string1}-${nested.string2}",
			},
			"simpleArray": []any{
				"${array[0]}",
				"${array[1]}",
			},
			"mapField": map[string]any{
				"key1": "${map.key1}",
				"key2": "${map.key2}",
			},
			"specialCharacters": map[string]any{
				"simpleAnnotation":     "${simpleannotation}",
				"doted.annotation.key": "${dotedannotationvalue}",
				"":                     "${emptyannotation}",
				"array.name.with.dots": []any{
					"${value}",
				},
			},
			"schemalessField": map[string]any{
				"key":       "value",
				"something": "${schemaless.value}",
				"nestedSomething": map[string]any{
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
			{Path: "stringField", Expression: krocel.NewUncompiled("string.value")},
			{Path: "intField", Expression: krocel.NewUncompiled("int.value")},
			{Path: "boolField", Expression: krocel.NewUncompiled("bool.value")},
			{Path: "nestedObject.nestedString", Expression: krocel.NewUncompiled("nested.string")},
			{Path: "nestedObject.nestedStringMultiple", Expression: krocel.NewUncompiled("(nested.string1) + \"-\" + (nested.string2)")},
			{Path: "simpleArray[0]", Expression: krocel.NewUncompiled("array[0]")},
			{Path: "simpleArray[1]", Expression: krocel.NewUncompiled("array[1]")},
			{Path: "mapField.key1", Expression: krocel.NewUncompiled("map.key1")},
			{Path: "mapField.key2", Expression: krocel.NewUncompiled("map.key2")},
			{Path: "specialCharacters.simpleAnnotation", Expression: krocel.NewUncompiled("simpleannotation")},
			{Path: "specialCharacters[\"doted.annotation.key\"]", Expression: krocel.NewUncompiled("dotedannotationvalue")},
			{Path: "specialCharacters[\"\"]", Expression: krocel.NewUncompiled("emptyannotation")},
			{Path: "specialCharacters[\"array.name.with.dots\"][0]", Expression: krocel.NewUncompiled("value")},
			{Path: "schemalessField.something", Expression: krocel.NewUncompiled("schemaless.value")},
			{Path: "schemalessField.nestedSomething.nested", Expression: krocel.NewUncompiled("schemaless.nested.value")},
		}

		expressions, err := New(schemacache.NewCache()).ParseResource(resource, schema)
		if err != nil {
			t.Fatalf("ParseResource() error = %v", err)
		}

		// sort both slices to ensure consistent ordering
		sort.Slice(expressions, func(i, j int) bool { return expressions[i].Path < expressions[j].Path })
		sort.Slice(expectedExpressions, func(i, j int) bool { return expectedExpressions[i].Path < expectedExpressions[j].Path })

		// first check the length
		if len(expressions) != len(expectedExpressions) {
			t.Fatalf("Expected %d expressions, got %d", len(expectedExpressions), len(expressions))
		}

		// compare each expression individually for better error messages
		for i := range expectedExpressions {
			expected := expectedExpressions[i]
			actual := expressions[i]

			if actual.Path != expected.Path {
				t.Errorf("Expression[%d] path mismatch:\n  got:  %s\n  want: %s", i, actual.Path, expected.Path)
			}

			if actual.Expression.Original != expected.Expression.Original {
				t.Errorf(
					"Expression[%d] expressions mismatch for path %s:\n  got:  %v\n  want: %v",
					i, expected.Path, actual.Expression.Original, expected.Expression.Original,
				)
			}
		}
	})

	t.Run("Invalid type for field", func(t *testing.T) {
		resource := map[string]any{
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

		_, err := New(schemacache.NewCache()).ParseResource(resource, schema)
		if err == nil {
			t.Errorf("ParseResource() expected error, got nil")
		}
	})
}

func TestTypeMismatches(t *testing.T) {
	testCases := []struct {
		name          string
		resource      map[string]any
		schema        *spec.Schema
		wantErr       bool
		expectedError string
	}{
		{
			name: "String instead of integer",
			resource: map[string]any{
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
			wantErr:       true,
			expectedError: "expected integer type for path intField, got string",
		},
		{
			name: "Integer instead of string",
			resource: map[string]any{
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
			wantErr:       true,
			expectedError: "expected string type for path stringField, got integer",
		},
		{
			name: "Boolean instead of number",
			resource: map[string]any{
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
			wantErr:       true,
			expectedError: "expected number type for path numberField, got boolean",
		},
		{
			name: "Array instead of object",
			resource: map[string]any{
				"objectField": []any{"not", "an", "object"},
			},
			schema: &spec.Schema{
				SchemaProps: spec.SchemaProps{
					Type: []string{"object"},
					Properties: map[string]spec.Schema{
						"objectField": {SchemaProps: spec.SchemaProps{Type: []string{"object"}}},
					},
				},
			},
			wantErr:       true,
			expectedError: "expected object type for path objectField, got array",
		},
		{
			name: "Object instead of array",
			resource: map[string]any{
				"arrayField": map[string]any{"key": "value"},
			},
			schema: &spec.Schema{
				SchemaProps: spec.SchemaProps{
					Type: []string{"object"},
					Properties: map[string]spec.Schema{
						"arrayField": {SchemaProps: spec.SchemaProps{Type: []string{"array"}}},
					},
				},
			},
			wantErr:       true,
			expectedError: "expected array type for path arrayField, got object",
		},
		{
			name: "Nested field type mismatch - string instead of number at 3 levels",
			resource: map[string]any{
				"level1": map[string]any{
					"level2": map[string]any{
						"numberField": "not-a-number",
					},
				},
			},
			schema: &spec.Schema{
				SchemaProps: spec.SchemaProps{
					Type: []string{"object"},
					Properties: map[string]spec.Schema{
						"level1": {
							SchemaProps: spec.SchemaProps{
								Type: []string{"object"},
								Properties: map[string]spec.Schema{
									"level2": {
										SchemaProps: spec.SchemaProps{
											Type: []string{"object"},
											Properties: map[string]spec.Schema{
												"numberField": {
													SchemaProps: spec.SchemaProps{
														Type: []string{"number"},
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
			wantErr:       true,
			expectedError: "expected number type for path level1.level2.numberField, got string",
		},
		{
			name: "Nil schema",
			resource: map[string]any{
				"field": "value",
			},
			schema:  nil,
			wantErr: true,
		},
		{
			name: "Schema with OneOf",
			resource: map[string]any{
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
			resource: map[string]any{
				"field": "value",
			},
			schema: &spec.Schema{
				SchemaProps: spec.SchemaProps{
					Type: []string{},
				},
			},
			wantErr:       true,
			expectedError: "schema at path  has no valid type, OneOf, AnyOf, or AdditionalProperties",
		},
		{
			name: "Valid types (no mismatch)",
			resource: map[string]any{
				"stringField": "valid string",
				"intField":    42,
				"boolField":   true,
				"numberField": 3.14,
				"objectField": map[string]any{"key": "value"},
				"arrayField":  []any{1, 2, 3},
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
			_, err := New(schemacache.NewCache()).ParseResource(tc.resource, tc.schema)
			if (err != nil) != tc.wantErr {
				t.Errorf("ParseResource() error = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil && tc.expectedError != "" && !strings.Contains(err.Error(), tc.expectedError) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.expectedError)
			}
		})
	}
}

func TestParseWithExpectedSchema(t *testing.T) {
	resource := map[string]any{
		"stringField": "${string.value}",
		"objectField": "${object.value}", // Entire object as a CEL expression
		"nestedObjectField": map[string]any{
			"nestedString": "${nested.string}",
			"nestedObject": map[string]any{
				"deepNested": "${deep.nested}",
			},
		},
		"arrayField": []any{
			"${array[0]}",
			map[string]any{
				"objectInArray": "${object.in.array}",
			},
		},
	}

	stringFieldSchema := newSchema(spec.SchemaProps{Type: []string{"string"}})
	objectFieldSchema := newSchema(spec.SchemaProps{
		Type: []string{"object"},
		Properties: map[string]spec.Schema{
			"key1": newSchema(spec.SchemaProps{Type: []string{"string"}}),
			"key2": newSchema(spec.SchemaProps{Type: []string{"integer"}}),
		},
	})
	nestedObjectFieldSchema := newSchema(spec.SchemaProps{
		Type: []string{"object"},
		Properties: map[string]spec.Schema{
			"nestedString": newSchema(spec.SchemaProps{Type: []string{"string"}}),
			"nestedObject": newSchema(spec.SchemaProps{
				Type: []string{"object"},
				Properties: map[string]spec.Schema{
					"deepNested": newSchema(spec.SchemaProps{Type: []string{"string"}}),
				},
			}),
		},
	})
	arrayFieldSchema := newSchema(spec.SchemaProps{
		Type: []string{"array"},
		Items: &spec.SchemaOrArray{
			Schema: new(newSchema(spec.SchemaProps{
				Type: []string{"object"},
				Properties: map[string]spec.Schema{
					"objectInArray": newSchema(spec.SchemaProps{Type: []string{"string"}}),
				},
				AdditionalProperties: &spec.SchemaOrBool{
					Allows: true,
					Schema: &spec.Schema{
						VendorExtensible: spec.VendorExtensible{
							Extensions: spec.Extensions{},
						},
					},
				},
			})),
		},
	})

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
		VendorExtensible: spec.VendorExtensible{
			Extensions: spec.Extensions{},
		},
	}

	expressions, err := New(schemacache.NewCache()).ParseResource(resource, schema)
	if err != nil {
		t.Fatalf("ParseResource() error = %v", err)
	}

	expectedExpressions := map[string]variable.FieldDescriptor{
		"stringField":                               {Path: "stringField", Expression: krocel.NewUncompiled("string.value")},
		"objectField":                               {Path: "objectField", Expression: krocel.NewUncompiled("object.value")},
		"nestedObjectField.nestedString":            {Path: "nestedObjectField.nestedString", Expression: krocel.NewUncompiled("nested.string")},
		"nestedObjectField.nestedObject.deepNested": {Path: "nestedObjectField.nestedObject.deepNested", Expression: krocel.NewUncompiled("deep.nested")},
		"arrayField[0]":                             {Path: "arrayField[0]", Expression: krocel.NewUncompiled("array[0]")},
		"arrayField[1].objectInArray":               {Path: "arrayField[1].objectInArray", Expression: krocel.NewUncompiled("object.in.array")},
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

		if expr.Expression.Original != expected.Expression.Original {
			t.Errorf("Path %s: expected expressions %v, got %v", expr.Path, expected.Expression.Original, expr.Expression.Original)
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

// TestParseResourceAtPath verifies the path prefix is applied to both extracted
// descriptor paths and error messages, which is why the method exists (so a
// selector sub-object validates with diagnostics rooted at metadata.selector).
func TestParseResourceAtPath(t *testing.T) {
	schema := newSchema(spec.SchemaProps{
		Type: []string{"object"},
		Properties: map[string]spec.Schema{
			"matchLabels": newSchema(spec.SchemaProps{
				Type:                 []string{"object"},
				AdditionalProperties: &spec.SchemaOrBool{Allows: true, Schema: new(newSchema(spec.SchemaProps{Type: []string{"string"}}))},
			}),
		},
	})

	fds, err := New(schemacache.NewCache()).ParseResourceAtPath(map[string]interface{}{
		"matchLabels": map[string]interface{}{"app": "${schema.spec.tier}"},
	}, &schema, "metadata.selector")
	if err != nil {
		t.Fatalf("ParseResourceAtPath() error = %v", err)
	}
	if len(fds) != 1 || fds[0].Path != "metadata.selector.matchLabels.app" {
		t.Fatalf("descriptor path = %+v, want metadata.selector.matchLabels.app", fds)
	}

	_, err = New(schemacache.NewCache()).ParseResourceAtPath(map[string]interface{}{
		"matchLabels": "notAnObject", // string where object is expected
	}, &schema, "metadata.selector")
	if err == nil || !strings.Contains(err.Error(), "metadata.selector.matchLabels") {
		t.Errorf("error = %v, want prefixed path metadata.selector.matchLabels", err)
	}
}

func TestParserEdgeCases(t *testing.T) {
	testCases := []struct {
		name          string
		schema        *spec.Schema
		resource      any
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
			resource:      []any{"test"},
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
			expectedError: "expected string type for path , got integer",
		},
		{
			name: "Type mismatch: object/array",
			schema: &spec.Schema{
				SchemaProps: spec.SchemaProps{
					Type: []string{"object"},
				},
			},
			resource:      []any{"test"},
			expectedError: "expected object type for path , got array",
		},
		{
			name: "Type mismatch: bool/string",
			schema: &spec.Schema{
				SchemaProps: spec.SchemaProps{
					Type: []string{"boolean"},
				},
			},
			resource:      "true",
			expectedError: "expected boolean type for path , got string",
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
			expectedError: "expected number type for path , got boolean",
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
			resource:      map[string]any{"key": "value"},
			expectedError: "expected array type for path , got object",
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
			resource: map[string]any{
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
			resource: map[string]any{
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
			resource:      map[string]any{"name": "John", "age": 30},
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
			resource: map[string]any{"id": "123", "metadata": map[string]any{
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
			resource: map[string]any{
				"name": "John",
			},
			expectedError: "schema at path name has no valid type, OneOf, AnyOf, or AdditionalProperties",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(schemacache.NewCache()).parseResource(tc.resource, tc.schema, "")
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
		name   string
		schema *spec.Schema
		field  any
	}{
		{"int short type for integer", &spec.Schema{SchemaProps: spec.SchemaProps{Type: []string{"int"}}}, 42},
		{"bool short type for boolean", &spec.Schema{SchemaProps: spec.SchemaProps{Type: []string{"bool"}}}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			expectedTypes, err := getExpectedTypes(tt.schema)
			if err != nil {
				t.Fatalf("getExpectedTypes() error = %v", err)
			}
			_, err = parseScalarTypes(tt.field, tt.schema, "spec.someitem", expectedTypes)
			if err != nil {
				t.Errorf("Expected %T resolved for %v, but got error: %s",
					tt.field, tt.schema.Type, err.Error())
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
		resource   map[string]any
		wantErr    bool
		wantErrMsg string
	}{
		{
			name: "Field is integer",
			resource: map[string]any{
				"myField": 42,
			},
			wantErr: false,
		},
		{
			name: "Field is string",
			resource: map[string]any{
				"myField": "forty-two",
			},
			wantErr: false,
		},
		{
			name: "Field is bool (invalid)",
			resource: map[string]any{
				"myField": true,
			},
			wantErr:    true,
			wantErrMsg: "expected string or integer type for path myField, got boolean",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(schemacache.NewCache()).ParseResource(tc.resource, schema)
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
			resource      map[string]any
			wantErr       bool
			expectedError string
		}{
			{
				name: "nestedField as integer",
				resource: map[string]any{
					"outerObject": map[string]any{
						"nestedField": 123,
					},
				},
				wantErr: false,
			},
			{
				name: "nestedField as string",
				resource: map[string]any{
					"outerObject": map[string]any{
						"nestedField": "one-two-three",
					},
				},
				wantErr: false,
			},
			{
				name: "nestedField as bool (invalid)",
				resource: map[string]any{
					"outerObject": map[string]any{
						"nestedField": true,
					},
				},
				wantErr:       true,
				expectedError: "expected string or integer type for path outerObject.nestedField, got boolean",
			},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				_, err := New(schemacache.NewCache()).ParseResource(tc.resource, schema)
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
		resource      any
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
			resource: map[string]any{
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
			resource: map[string]any{
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
			resource: map[string]any{
				"field": true,
			},
			wantErr:       true,
			expectedError: "expected string or integer type for path field, got boolean",
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
			resource: map[string]any{
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
			resource: map[string]any{
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
			resource: map[string]any{
				"field": true,
			},
			wantErr:       true,
			expectedError: "expected string or integer type for path field, got boolean",
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
			resource: map[string]any{
				"nestedField": map[string]any{
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
			resource: map[string]any{
				"nestedField": map[string]any{
					"innerField": true,
				},
			},
			wantErr:       true,
			expectedError: "expected string or integer type for path nestedField.innerField, got boolean",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(schemacache.NewCache()).parseResource(tc.resource, tc.schema, "")
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

		resource := map[string]any{
			"networkRef": map[string]any{
				"name": "${network.metadata.name}",
			},
		}

		expressions, err := New(schemacache.NewCache()).ParseResource(resource, schema)
		if err != nil {
			t.Fatalf("ParseResource() error = %v", err)
		}

		if len(expressions) != 1 {
			t.Fatalf("Expected 1 expression, got %d", len(expressions))
		}

		expected := variable.FieldDescriptor{
			Path:       "networkRef.name",
			Expression: krocel.NewUncompiled("network.metadata.name"),
		}

		if expressions[0].Path != expected.Path {
			t.Errorf("Expected path %s, got %s", expected.Path, expressions[0].Path)
		}

		if expressions[0].Expression.Original != expected.Expression.Original {
			t.Errorf("Expressions mismatch: got %v, want %v", expressions[0].Expression.Original, expected.Expression.Original)
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

		resource := map[string]any{
			"networkRef": map[string]any{
				"external": "${network.selfLink}",
			},
		}

		expressions, err := New(schemacache.NewCache()).ParseResource(resource, schema)
		if err != nil {
			t.Fatalf("ParseResource() error = %v", err)
		}

		if len(expressions) != 1 {
			t.Fatalf("Expected 1 expression, got %d", len(expressions))
		}

		expected := variable.FieldDescriptor{
			Path:       "networkRef.external",
			Expression: krocel.NewUncompiled("network.selfLink"),
		}

		if expressions[0].Path != expected.Path {
			t.Errorf("Expected path %s, got %s", expected.Path, expressions[0].Path)
		}
		if expressions[0].Expression.Original != expected.Expression.Original {
			t.Errorf("Expected expressions %v, got %v", expected.Expression.Original, expressions[0].Expression.Original)
		}
	})
}

func TestPreserveUnknownFields(t *testing.T) {
	testCases := []struct {
		name                string
		schema              *spec.Schema
		resource            map[string]any
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
			resource: map[string]any{
				"spec": map[string]any{
					"template": "${template.value}",
				},
			},
			wantErr: false,
			expectedExpressions: []variable.FieldDescriptor{
				{
					Path:       "spec.template",
					Expression: krocel.NewUncompiled("template.value"),
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
			resource: map[string]any{
				"spec": map[string]any{
					"field1": "noisy string",
					"template": map[string]any{
						"nested": []any{
							map[string]any{
								"key": "${template.value}",
							},
						},
					},
				},
			},
			wantErr: false,
			expectedExpressions: []variable.FieldDescriptor{
				{
					Path:       "spec.template.nested[0].key",
					Expression: krocel.NewUncompiled("template.value"),
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
			resource: map[string]any{
				"program": map[string]any{
					"resources": map[string]any{
						"app": map[string]any{
							"properties": map[string]any{
								"spec": map[string]any{
									"name":   "${schema.spec.name}",
									"region": "${schema.spec.region}",
									"services": []any{
										map[string]any{
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
					Path:       "program.resources.app.properties.spec.name",
					Expression: krocel.NewUncompiled("schema.spec.name"),
				},
				{
					Path:       "program.resources.app.properties.spec.region",
					Expression: krocel.NewUncompiled("schema.spec.region"),
				},
				{
					Path:       "program.resources.app.properties.spec.services[0].name",
					Expression: krocel.NewUncompiled("(schema.spec.name) + \"-service\""),
				},
				{
					Path:       "program.resources.app.properties.spec.services[0].instanceCount",
					Expression: krocel.NewUncompiled("schema.spec.instanceCount"),
				},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			expressions, err := New(schemacache.NewCache()).ParseResource(tc.resource, tc.schema)
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

					if actualExpr.Expression.Original != expectedExpr.Expression.Original {
						t.Errorf("Path %s: expected expressions %v, got %v", path, expectedExpr.Expression.Original, actualExpr.Expression.Original)
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

// TestEmptyBracesInExpressions tests the regression where strings.Trim() was
// incorrectly stripping {} from expressions. This bug affected ternary expressions
// with empty map literals like: condition ? value : {}
func TestEmptyBracesInExpressions(t *testing.T) {
	testCases := []struct {
		name             string
		resource         map[string]any
		schema           *spec.Schema
		expectedExprPath string // Path where we expect to find the expression
		expectedExpr     string // The exact expression we expect (without ${})
	}{
		{
			name: "Ternary with empty map literal",
			resource: map[string]any{
				"annotations": "${includeAnnotations ? annotations : {}}",
			},
			schema: &spec.Schema{
				SchemaProps: spec.SchemaProps{
					Type: []string{"object"},
					Properties: map[string]spec.Schema{
						"annotations": {
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
					},
				},
			},
			expectedExprPath: "annotations",
			expectedExpr:     "includeAnnotations ? annotations : {}",
		},
		{
			name: "Complex ternary with has() and empty map",
			resource: map[string]any{
				"metadata": map[string]any{
					"annotations": "${has(schema.annotations) && includeAnnotations ? schema.annotations : {}}",
				},
			},
			schema: &spec.Schema{
				SchemaProps: spec.SchemaProps{
					Type: []string{"object"},
					Properties: map[string]spec.Schema{
						"metadata": {
							SchemaProps: spec.SchemaProps{
								Type: []string{"object"},
								Properties: map[string]spec.Schema{
									"annotations": {
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
								},
							},
						},
					},
				},
			},
			expectedExprPath: "metadata.annotations",
			expectedExpr:     "has(schema.annotations) && includeAnnotations ? schema.annotations : {}",
		},
		{
			name: "Ternary with empty maps on both sides",
			resource: map[string]any{
				"config": "${condition ? {} : {}}",
			},
			schema: &spec.Schema{
				SchemaProps: spec.SchemaProps{
					Type: []string{"object"},
					Properties: map[string]spec.Schema{
						"config": {
							SchemaProps: spec.SchemaProps{
								Type: []string{"object"},
							},
						},
					},
				},
			},
			expectedExprPath: "config",
			expectedExpr:     "condition ? {} : {}",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			fields, err := New(schemacache.NewCache()).ParseResource(tc.resource, tc.schema)
			if err != nil {
				t.Fatalf("ParseResource() error = %v", err)
			}

			// Find the field descriptor for the expected path
			found := false
			for _, field := range fields {
				if field.Path == tc.expectedExprPath {
					found = true
					if field.Expression.Original != tc.expectedExpr {
						t.Errorf("Expression mismatch:\ngot:  %q\nwant: %q", field.Expression.Original, tc.expectedExpr)
					}
				}
			}
			if !found {
				t.Errorf("Expected to find field descriptor for path %q", tc.expectedExprPath)
			}
		})
	}
}

func TestBuildStringTemplate(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		matches []exprMatch
		want    string
	}{
		{
			name:    "prefix only",
			input:   "prefix-${expr}",
			matches: []exprMatch{{expr: "expr", start: 7, end: 14}},
			want:    `"prefix-" + (expr)`,
		},
		{
			name:    "suffix only",
			input:   "${expr}-suffix",
			matches: []exprMatch{{expr: "expr", start: 0, end: 7}},
			want:    `(expr) + "-suffix"`,
		},
		{
			name:    "prefix and suffix",
			input:   "prefix-${expr}-suffix",
			matches: []exprMatch{{expr: "expr", start: 7, end: 14}},
			want:    `"prefix-" + (expr) + "-suffix"`,
		},
		{
			name:  "multiple expressions",
			input: "a-${expr1}-b-${expr2}-c",
			matches: []exprMatch{
				{expr: "expr1", start: 2, end: 10},
				{expr: "expr2", start: 13, end: 21},
			},
			want: `"a-" + (expr1) + "-b-" + (expr2) + "-c"`,
		},
		{
			name:  "adjacent expressions",
			input: "${expr1}${expr2}",
			matches: []exprMatch{
				{expr: "expr1", start: 0, end: 8},
				{expr: "expr2", start: 8, end: 16},
			},
			want: `(expr1) + (expr2)`,
		},
		{
			name:    "literal with quotes",
			input:   `say "hello" ${expr}`,
			matches: []exprMatch{{expr: "expr", start: 12, end: 19}},
			want:    `"say \"hello\" " + (expr)`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildStringTemplate(tt.input, tt.matches)
			if got != tt.want {
				t.Errorf("buildStringTemplate() = %q, want %q", got, tt.want)
			}
		})
	}
}
