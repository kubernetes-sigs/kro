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
	"sort"
	"testing"
)

func compareExpressionFields(a, b []CELField) bool {
	if len(a) != len(b) {
		return false
	}

	sort.Slice(a, func(i, j int) bool { return a[i].Path < a[j].Path })
	sort.Slice(b, func(i, j int) bool { return b[i].Path < b[j].Path })

	for i := range a {
		if !equalStrings(a[i].Expressions, b[i].Expressions) ||
			a[i].ExpectedType != b[i].ExpectedType ||
			a[i].Path != b[i].Path ||
			a[i].StandaloneExpression != b[i].StandaloneExpression {
			return false
		}
	}
	return true
}

func TestParseSchemalessResource(t *testing.T) {
	tests := []struct {
		name     string
		resource map[string]interface{}
		want     []CELField
		wantErr  bool
	}{
		{
			name: "Simple string field",
			resource: map[string]interface{}{
				"field": "${resource.value}",
			},
			want: []CELField{
				{
					Expressions:          []string{"resource.value"},
					ExpectedType:         "any",
					Path:                 ".field",
					StandaloneExpression: true,
				},
			},
			wantErr: false,
		},
		{
			name: "Nested map",
			resource: map[string]interface{}{
				"outer": map[string]interface{}{
					"inner": "${nested.value}",
				},
			},
			want: []CELField{
				{
					Expressions:          []string{"nested.value"},
					ExpectedType:         "any",
					Path:                 ".outer.inner",
					StandaloneExpression: true,
				},
			},
			wantErr: false,
		},
		{
			name: "array field",
			resource: map[string]interface{}{
				"array": []interface{}{
					"${array[0]}",
					"${array[1]}",
				},
			},
			want: []CELField{
				{
					Expressions:          []string{"array[0]"},
					ExpectedType:         "any",
					Path:                 ".array[0]",
					StandaloneExpression: true,
				},
				{
					Expressions:          []string{"array[1]"},
					ExpectedType:         "any",
					Path:                 ".array[1]",
					StandaloneExpression: true,
				},
			},
			wantErr: false,
		},
		{
			name: "Multiple expressions in string",
			resource: map[string]interface{}{
				"field": "Start ${expr1} middle ${expr2} end",
			},
			want: []CELField{
				{
					Expressions:  []string{"expr1", "expr2"},
					ExpectedType: "any",
					Path:         ".field",
				},
			},
			wantErr: false,
		},
		{
			name: "Mixed types",
			resource: map[string]interface{}{
				"string": "${string.value}",
				"number": 42,
				"bool":   true,
				"nested": map[string]interface{}{
					"array": []interface{}{
						"${array.value}",
						123,
					},
				},
			},
			want: []CELField{
				{
					Expressions:          []string{"string.value"},
					ExpectedType:         "any",
					Path:                 ".string",
					StandaloneExpression: true,
				},
				{
					Expressions:          []string{"array.value"},
					ExpectedType:         "any",
					Path:                 ".nested.array[0]",
					StandaloneExpression: true,
				},
			},
			wantErr: false,
		},
		{
			name:     "Empty resource",
			resource: map[string]interface{}{},
			want:     []CELField{},
			wantErr:  false,
		},
		{
			name: "Nested expression (should error)",
			resource: map[string]interface{}{
				"field": "${outer(${inner})}",
			},
			want:    nil,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseSchemalessResource(tt.resource)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseSchemalessResource() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !compareExpressionFields(got, tt.want) {
				t.Errorf("ParseSchemalessResource() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseSchemalessResourceEdgeCases(t *testing.T) {
	tests := []struct {
		name     string
		resource map[string]interface{}
		want     []CELField
		wantErr  bool
	}{
		{
			name: "Deeply nested structure",
			resource: map[string]interface{}{
				"level1": map[string]interface{}{
					"level2": map[string]interface{}{
						"level3": map[string]interface{}{
							"level4": "${deeply.nested.value}",
						},
					},
				},
			},
			want: []CELField{
				{
					Expressions:          []string{"deeply.nested.value"},
					ExpectedType:         "any",
					Path:                 ".level1.level2.level3.level4",
					StandaloneExpression: true,
				},
			},
			wantErr: false,
		},
		{
			name: "Array with mixed types",
			resource: map[string]interface{}{
				"array": []interface{}{
					"${expr1}",
					42,
					true,
					map[string]interface{}{
						"nested": "${expr2}",
					},
				},
			},
			want: []CELField{
				{
					Expressions:          []string{"expr1"},
					ExpectedType:         "any",
					Path:                 ".array[0]",
					StandaloneExpression: true,
				},
				{
					Expressions:          []string{"expr2"},
					ExpectedType:         "any",
					Path:                 ".array[3].nested",
					StandaloneExpression: true,
				},
			},
			wantErr: false,
		},
		{
			name: "Empty string expressions",
			resource: map[string]interface{}{
				"empty1": "${}",
				"empty2": "${    }",
			},
			want: []CELField{
				{
					Expressions:          []string{""},
					ExpectedType:         "any",
					Path:                 ".empty1",
					StandaloneExpression: true,
				},
				{
					Expressions:          []string{"    "},
					ExpectedType:         "any",
					Path:                 ".empty2",
					StandaloneExpression: true,
				},
			},
			wantErr: false,
		},
		{
			name: "Incomplete expressions",
			resource: map[string]interface{}{
				"incomplete1": "${incomplete",
				"incomplete2": "incomplete}",
				"incomplete3": "$not_an_expression",
			},
			want:    []CELField{},
			wantErr: false,
		},
		{
			name: "Complex strcture with various expressions combinations",
			resource: map[string]interface{}{
				"string": "${string.value}",
				"number": 42,
				"bool":   true,
				"nested": map[string]interface{}{
					"array": []interface{}{
						"${array.value}",
						123,
					},
				},
				"complex": map[string]interface{}{
					"field": "Start ${expr1} middle ${expr2} end",
					"nested": map[string]interface{}{
						"inner": "${nested.value}",
					},
					"array": []interface{}{
						"${expr3-incmplete",
						"${expr4}",
						"${expr5}",
					},
				},
			},
			want: []CELField{
				{
					Expressions:          []string{"string.value"},
					ExpectedType:         "any",
					Path:                 ".string",
					StandaloneExpression: true,
				},
				{
					Expressions:          []string{"array.value"},
					ExpectedType:         "any",
					Path:                 ".nested.array[0]",
					StandaloneExpression: true,
				},
				{
					Expressions:  []string{"expr1", "expr2"},
					ExpectedType: "any",
					Path:         ".complex.field",
				},
				{
					Expressions:          []string{"nested.value"},
					ExpectedType:         "any",
					Path:                 ".complex.nested.inner",
					StandaloneExpression: true,
				},
				{
					Expressions:          []string{"expr4"},
					ExpectedType:         "any",
					Path:                 ".complex.array[1]",
					StandaloneExpression: true,
				},
				{
					Expressions:          []string{"expr5"},
					ExpectedType:         "any",
					Path:                 ".complex.array[2]",
					StandaloneExpression: true,
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseSchemalessResource(tt.resource)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseSchemalessResource() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !compareExpressionFields(got, tt.want) {
				t.Errorf("ParseSchemalessResource() = %v, want %v", got, tt.want)
			}
		})
	}
}
