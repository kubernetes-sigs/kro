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

	"github.com/kro-run/kro/pkg/graph/variable"
)

func areEqualExpressionFields(a, b []variable.FieldDescriptor) bool {
	if len(a) != len(b) {
		return false
	}

	sort.Slice(a, func(i, j int) bool { return a[i].Path < a[j].Path })
	sort.Slice(b, func(i, j int) bool { return b[i].Path < b[j].Path })

	for i := range a {
		if !equalStrings(a[i].Expressions, b[i].Expressions) ||
			!areEqualSlices(a[i].ExpectedTypes, b[i].ExpectedTypes) ||
			a[i].Path != b[i].Path ||
			a[i].StandaloneExpression != b[i].StandaloneExpression {
			return false
		}
	}
	return true
}

// areEqualSlices checks if two string slices contain the same elements, regardless of order.
// It returns true if both slices have the same length and contain the same set of unique elements,
// and false otherwise. This function treats the slices as sets, so duplicate elements are ignored.
//
// Parameters:
//   - slice1
//   - slice2
//
// Returns:
//   - bool: true if the slices contain the same elements, false otherwise
func areEqualSlices(slice1, slice2 []string) bool {
	if len(slice1) != len(slice2) {
		return false
	}

	elementSet := make(map[string]bool)
	for _, s := range slice1 {
		elementSet[s] = true
	}

	// Check if all elements from slice2 are in the set
	for _, s := range slice2 {
		if !elementSet[s] {
			return false
		}
		// Remove the element to ensure uniqueness
		delete(elementSet, s)
	}
	return len(elementSet) == 0
}

func TestParseSchemalessResource(t *testing.T) {
	tests := []struct {
		name     string
		resource map[string]interface{}
		want     []variable.FieldDescriptor
		wantErr  bool
	}{
		{
			name: "Simple string field",
			resource: map[string]interface{}{
				"field": "${resource.value}",
			},
			want: []variable.FieldDescriptor{
				{
					Expressions:          []string{"resource.value"},
					ExpectedTypes:        []string{"any"},
					Path:                 "field",
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
			want: []variable.FieldDescriptor{
				{
					Expressions:          []string{"nested.value"},
					ExpectedTypes:        []string{"any"},
					Path:                 "outer.inner",
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
			want: []variable.FieldDescriptor{
				{
					Expressions:          []string{"array[0]"},
					ExpectedTypes:        []string{"any"},
					Path:                 "array[0]",
					StandaloneExpression: true,
				},
				{
					Expressions:          []string{"array[1]"},
					ExpectedTypes:        []string{"any"},
					Path:                 "array[1]",
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
			want: []variable.FieldDescriptor{
				{
					Expressions:   []string{"expr1", "expr2"},
					ExpectedTypes: []string{"any"},
					Path:          "field",
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
			want: []variable.FieldDescriptor{
				{
					Expressions:          []string{"string.value"},
					ExpectedTypes:        []string{"any"},
					Path:                 "string",
					StandaloneExpression: true,
				},
				{
					Expressions:          []string{"array.value"},
					ExpectedTypes:        []string{"any"},
					Path:                 "nested.array[0]",
					StandaloneExpression: true,
				},
			},
			wantErr: false,
		},
		{
			name:     "Empty resource",
			resource: map[string]interface{}{},
			want:     []variable.FieldDescriptor{},
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
			if !areEqualExpressionFields(got, tt.want) {
				t.Errorf("ParseSchemalessResource() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseSchemalessResourceEdgeCases(t *testing.T) {
	tests := []struct {
		name     string
		resource map[string]interface{}
		want     []variable.FieldDescriptor
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
			want: []variable.FieldDescriptor{
				{
					Expressions:          []string{"deeply.nested.value"},
					ExpectedTypes:        []string{"any"},
					Path:                 "level1.level2.level3.level4",
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
			want: []variable.FieldDescriptor{
				{
					Expressions:          []string{"expr1"},
					ExpectedTypes:        []string{"any"},
					Path:                 "array[0]",
					StandaloneExpression: true,
				},
				{
					Expressions:          []string{"expr2"},
					ExpectedTypes:        []string{"any"},
					Path:                 "array[3].nested",
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
			want: []variable.FieldDescriptor{
				{
					Expressions:          []string{""},
					ExpectedTypes:        []string{"any"},
					Path:                 "empty1",
					StandaloneExpression: true,
				},
				{
					Expressions:          []string{"    "},
					ExpectedTypes:        []string{"any"},
					Path:                 "empty2",
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
			want:    []variable.FieldDescriptor{},
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
			want: []variable.FieldDescriptor{
				{
					Expressions:          []string{"string.value"},
					ExpectedTypes:        []string{"any"},
					Path:                 "string",
					StandaloneExpression: true,
				},
				{
					Expressions:          []string{"array.value"},
					ExpectedTypes:        []string{"any"},
					Path:                 "nested.array[0]",
					StandaloneExpression: true,
				},
				{
					Expressions:   []string{"expr1", "expr2"},
					ExpectedTypes: []string{"any"},
					Path:          "complex.field",
				},
				{
					Expressions:          []string{"nested.value"},
					ExpectedTypes:        []string{"any"},
					Path:                 "complex.nested.inner",
					StandaloneExpression: true,
				},
				{
					Expressions:          []string{"expr4"},
					ExpectedTypes:        []string{"any"},
					Path:                 "complex.array[1]",
					StandaloneExpression: true,
				},
				{
					Expressions:          []string{"expr5"},
					ExpectedTypes:        []string{"any"},
					Path:                 "complex.array[2]",
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
			if !areEqualExpressionFields(got, tt.want) {
				t.Errorf("ParseSchemalessResource() = %v, want %v", got, tt.want)
			}
		})
	}
}
