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
	"testing"

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
				},
			},
		}

		expectedExpressions := []ExpressionField{
			{Path: ".stringField", Expressions: []string{"string.value"}, ExpectedType: "string", OneShotCEL: true},
			{Path: ".intField", Expressions: []string{"int.value"}, ExpectedType: "integer", OneShotCEL: true},
			{Path: ".boolField", Expressions: []string{"bool.value"}, ExpectedType: "boolean", OneShotCEL: true},
			{Path: ".nestedObject.nestedString", Expressions: []string{"nested.string"}, ExpectedType: "string", OneShotCEL: true},
			{Path: ".nestedObject.nestedStringMultiple", Expressions: []string{"nested.string1", "nested.string2"}, ExpectedType: "string", OneShotCEL: false},
			{Path: ".simpleArray[0]", Expressions: []string{"array[0]"}, ExpectedType: "string", OneShotCEL: true},
			{Path: ".simpleArray[1]", Expressions: []string{"array[1]"}, ExpectedType: "string", OneShotCEL: true},
			{Path: ".mapField.key1", Expressions: []string{"map.key1"}, ExpectedType: "string", OneShotCEL: true},
			{Path: ".mapField.key2", Expressions: []string{"map.key2"}, ExpectedType: "string", OneShotCEL: true},
		}

		expressions, err := ParseResource(resource, schema)
		if err != nil {
			t.Fatalf("ParseResource() error = %v", err)
		}

		if !compareExpressionFields(expressions, expectedExpressions) {
			t.Errorf("ParseResource() got = %v, want %v", expressions, expectedExpressions)

			for i, expr := range expressions {
				t.Logf("Got: %d: %+v", i, expr)
			}
			for i, expr := range expectedExpressions {
				t.Logf("Want: %d: %+v", i, expr)
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
