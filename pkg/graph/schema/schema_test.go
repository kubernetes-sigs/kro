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

package schema

import (
	"reflect"
	"testing"

	"k8s.io/kube-openapi/pkg/validation/spec"
)

func TestGetResourceTopLevelFieldNames(t *testing.T) {
	tests := []struct {
		name   string
		schema *spec.Schema
		want   []string
	}{
		{
			name:   "nil schema",
			schema: nil,
			want:   []string{},
		},
		{
			name:   "empty properties",
			schema: &spec.Schema{},
			want:   []string{},
		},
		{
			name: "schema with apiVersion and kind only",
			schema: &spec.Schema{
				SchemaProps: spec.SchemaProps{
					Properties: map[string]spec.Schema{
						"apiVersion": {},
						"kind":       {},
					},
				},
			},
			want: []string{},
		},
		{
			name: "schema with standard kubernetes fields",
			schema: &spec.Schema{
				SchemaProps: spec.SchemaProps{
					Properties: map[string]spec.Schema{
						"apiVersion": {},
						"kind":       {},
						"metadata":   {},
						"spec":       {},
						"status":     {},
					},
				},
			},
			want: []string{"metadata", "spec", "status"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := GetResourceTopLevelFieldNames(tt.schema)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("GetResourceTopLevelFieldNames() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestGetResourceTopLevelFieldNamesNilProperties tests the case where
// Properties is explicitly nil rather than the entire Schema being nil
func TestGetResourceTopLevelFieldNamesNilProperties(t *testing.T) {
	schema := &spec.Schema{
		SchemaProps: spec.SchemaProps{
			Properties: nil,
		},
	}

	got := GetResourceTopLevelFieldNames(schema)
	want := []string{}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("GetResourceTopLevelFieldNames() = %v, want %v", got, want)
	}
}

// TestGetResourceTopLevelFieldNamesSorting specifically tests
// that the returned field names are sorted alphabetically
func TestGetResourceTopLevelFieldNamesSorting(t *testing.T) {
	schema := &spec.Schema{
		SchemaProps: spec.SchemaProps{
			Properties: map[string]spec.Schema{
				"z":          {},
				"a":          {},
				"d":          {},
				"apiVersion": {},
				"kind":       {},
			},
		},
	}

	got := GetResourceTopLevelFieldNames(schema)
	want := []string{"a", "d", "z"}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("GetResourceTopLevelFieldNames() = %v, want %v", got, want)
	}
}
