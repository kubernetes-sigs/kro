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
	"testing"

	krocel "github.com/kubernetes-sigs/kro/pkg/cel"
	"github.com/kubernetes-sigs/kro/pkg/graph/variable"
)

func areEqualExpressionFields(a, b []variable.FieldDescriptor) bool {
	if len(a) != len(b) {
		return false
	}

	sort.Slice(a, func(i, j int) bool { return a[i].Path < a[j].Path })
	sort.Slice(b, func(i, j int) bool { return b[i].Path < b[j].Path })

	for i := range a {
		if a[i].Expression.Original != b[i].Expression.Original ||
			a[i].Path != b[i].Path {
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
		name                string
		resource            map[string]interface{}
		expressionsWant     []variable.FieldDescriptor
		plainFieldPathsWant []string
		wantErr             bool
	}{
		{
			name: "Simple string field",
			resource: map[string]interface{}{
				"field":        "${resource.value}",
				"anotherField": "value",
			},
			expressionsWant: []variable.FieldDescriptor{
				{
					Expression: krocel.NewUncompiled("resource.value"),
					Path:       "field",
				},
			},
			plainFieldPathsWant: []string{"anotherField"},
			wantErr:             false,
		},
		{
			name: "Nested map",
			resource: map[string]interface{}{
				"outer": map[string]interface{}{
					"inner":        "${nested.value}",
					"anotherInner": "nestedValue",
				},
			},
			expressionsWant: []variable.FieldDescriptor{
				{
					Expression: krocel.NewUncompiled("nested.value"),
					Path:       "outer.inner",
				},
			},
			plainFieldPathsWant: []string{"outer.anotherInner"},
			wantErr:             false,
		},
		{
			name: "array field",
			resource: map[string]interface{}{
				"array": []interface{}{
					"${array[0]}",
					"${array[1]}",
				},
			},
			expressionsWant: []variable.FieldDescriptor{
				{
					Expression: krocel.NewUncompiled("array[0]"),
					Path:       "array[0]",
				},
				{
					Expression: krocel.NewUncompiled("array[1]"),
					Path:       "array[1]",
				},
			},
			wantErr: false,
		},
		{
			name: "Multiple expressions in string",
			resource: map[string]interface{}{
				"field": "Start ${expr1} middle ${expr2} end",
			},
			expressionsWant: []variable.FieldDescriptor{
				{
					Expression: krocel.NewUncompiled("\"Start \" + (expr1) + \" middle \" + (expr2) + \" end\""),
					Path:       "field",
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
			expressionsWant: []variable.FieldDescriptor{
				{
					Expression: krocel.NewUncompiled("string.value"),
					Path:       "string",
				},
				{
					Expression: krocel.NewUncompiled("array.value"),
					Path:       "nested.array[0]",
				},
			},
			plainFieldPathsWant: []string{"number", "bool", "nested.array[1]"},
			wantErr:             false,
		},
		{
			name:            "Empty resource",
			resource:        map[string]interface{}{},
			expressionsWant: []variable.FieldDescriptor{},
			wantErr:         false,
		},
		{
			name: "Nested expression (should error)",
			resource: map[string]interface{}{
				"field": "${outer(${inner})}",
			},
			expressionsWant: nil,
			wantErr:         true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			expressionsGot, plainFieldPathsGot, err := ParseSchemalessResource(tt.resource)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseSchemalessResource() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !areEqualExpressionFields(expressionsGot, tt.expressionsWant) {
				t.Errorf("ParseSchemalessResource() expressions = %v, want %v", expressionsGot, tt.expressionsWant)
			}
			if !areEqualSlices(plainFieldPathsGot, tt.plainFieldPathsWant) {
				t.Errorf("ParseSchemalessResource() plainFieldPaths = %v, want %v", plainFieldPathsGot, tt.plainFieldPathsWant)
			}
		})
	}
}

func TestParseSchemalessResourceEdgeCases(t *testing.T) {
	tests := []struct {
		name                string
		resource            map[string]interface{}
		expressionsWant     []variable.FieldDescriptor
		plainFieldPathsWant []string
		wantErr             bool
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
			expressionsWant: []variable.FieldDescriptor{
				{
					Expression: krocel.NewUncompiled("deeply.nested.value"),
					Path:       "level1.level2.level3.level4",
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
			expressionsWant: []variable.FieldDescriptor{
				{
					Expression: krocel.NewUncompiled("expr1"),
					Path:       "array[0]",
				},
				{
					Expression: krocel.NewUncompiled("expr2"),
					Path:       "array[3].nested",
				},
			},
			plainFieldPathsWant: []string{"array[1]", "array[2]"},
			wantErr:             false,
		},
		{
			name: "Empty string expressions",
			resource: map[string]interface{}{
				"empty1": "${}",
				"empty2": "${    }",
			},
			expressionsWant: []variable.FieldDescriptor{
				{
					Expression: krocel.NewUncompiled(""),
					Path:       "empty1",
				},
				{
					Expression: krocel.NewUncompiled("    "),
					Path:       "empty2",
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
			expressionsWant:     []variable.FieldDescriptor{},
			plainFieldPathsWant: []string{"incomplete1", "incomplete2", "incomplete3"},
			wantErr:             false,
		},
		{
			name: "Complex structure with various expressions combinations",
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
			expressionsWant: []variable.FieldDescriptor{
				{
					Expression: krocel.NewUncompiled("string.value"),
					Path:       "string",
				},
				{
					Expression: krocel.NewUncompiled("array.value"),
					Path:       "nested.array[0]",
				},
				{
					Expression: krocel.NewUncompiled("\"Start \" + (expr1) + \" middle \" + (expr2) + \" end\""),
					Path:       "complex.field",
				},
				{
					Expression: krocel.NewUncompiled("nested.value"),
					Path:       "complex.nested.inner",
				},
				{
					Expression: krocel.NewUncompiled("expr4"),
					Path:       "complex.array[1]",
				},
				{
					Expression: krocel.NewUncompiled("expr5"),
					Path:       "complex.array[2]",
				},
			},
			plainFieldPathsWant: []string{"number", "bool", "nested.array[1]", "complex.array[0]"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			expressionsGot, plainFieldPathsGot, err := ParseSchemalessResource(tt.resource)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseSchemalessResource() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !areEqualExpressionFields(expressionsGot, tt.expressionsWant) {
				t.Errorf("ParseSchemalessResource() expressions = %v, want %v", expressionsGot, tt.expressionsWant)
			}
			if !areEqualSlices(plainFieldPathsGot, tt.plainFieldPathsWant) {
				t.Errorf("ParseSchemalessResource() plainFieldPaths = %v, want %v", plainFieldPathsGot, tt.plainFieldPathsWant)
			}
		})
	}
}
