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

package resolver

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"

	krocel "github.com/kubernetes-sigs/kro/pkg/cel"
	"github.com/kubernetes-sigs/kro/pkg/cel/sentinels"
	"github.com/kubernetes-sigs/kro/pkg/graph/variable"
)

func TestGetValueFromPath(t *testing.T) {
	tests := []struct {
		name     string
		resource map[string]any
		path     string
		want     any
		wantErr  bool
	}{
		{
			name: "simple field",
			resource: map[string]any{
				"field": "prefix${value1}suffix${value2}",
			},
			path:    "field",
			want:    "prefix${value1}suffix${value2}",
			wantErr: false,
		},
		{
			name: "nested field",
			resource: map[string]any{
				"spec": map[string]any{
					"template": map[string]any{
						"containers": []any{
							map[string]any{
								"image": "${image.name}:${image.tag}",
							},
						},
					},
				},
			},
			path:    `spec["template"]["containers"][0]["image"]`,
			want:    "${image.name}:${image.tag}",
			wantErr: false,
		},
		{
			name: "array access",
			resource: map[string]any{
				"items": []any{
					"${value1}",
					"${value2}",
					"${value3}",
				},
			},
			path:    "items[1]",
			want:    "${value2}",
			wantErr: false,
		},
		{
			name: "mixed quotes and dots",
			resource: map[string]any{
				"spec": map[string]any{
					"my.field.name": "${complex.value}",
				},
			},
			path:    `spec["my.field.name"]`,
			want:    "${complex.value}",
			wantErr: false,
		},
		{
			name: "deep nested arrays",
			resource: map[string]any{
				"metadata": map[string]any{
					"annotations": []any{
						map[string]any{
							"values": []any{
								"${annotation1}",
								"${annotation2}",
							},
						},
					},
				},
			},
			path:    `metadata["annotations"][0]["values"][1]`,
			want:    "${annotation2}",
			wantErr: false,
		},
		{
			name: "nonexistent key",
			resource: map[string]any{
				"field": "${value}",
			},
			path:    "nonexistent",
			want:    nil,
			wantErr: true,
		},
		{
			name: "invalid array index",
			resource: map[string]any{
				"items": []any{"${value}"},
			},
			path:    "items[10]",
			want:    nil,
			wantErr: true,
		},
		{
			name: "invalid type conversion",
			resource: map[string]any{
				"field":        "${value}",
				"field.nested": "invalid",
			},
			path:    "field.nested",
			want:    nil,
			wantErr: true,
		},
		{
			name: "invalid path parse error",
			resource: map[string]any{
				"field": "value",
			},
			path:    `[invalid["path"]`,
			want:    nil,
			wantErr: true,
		},
		{
			name: "expected array but got map",
			resource: map[string]any{
				"field": map[string]any{
					"nested": "value",
				},
			},
			path:    "field[0]",
			want:    nil,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewResolver(tt.resource, nil)
			got, err := r.getValueFromPath(tt.path)

			if (err != nil) != tt.wantErr {
				t.Errorf("getValueFromPath() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr && got != tt.want {
				t.Errorf("getValueFromPath() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSetValueAtPath(t *testing.T) {
	tests := []struct {
		name     string
		resource map[string]any
		path     string
		value    any
		wantErr  bool
		want     map[string]any
	}{
		{
			name:     "set top level field",
			resource: map[string]any{},
			path:     "name",
			value:    "test-value",
			want: map[string]any{
				"name": "test-value",
			},
		},
		{
			name: "set nested field",
			resource: map[string]any{
				"spec": map[string]any{},
			},
			path:  `spec.replicas`,
			value: 3,
			want: map[string]any{
				"spec": map[string]any{
					"replicas": 3,
				},
			},
		},
		{
			name:     "create intermediate structures",
			resource: map[string]any{},
			path:     `spec.template.metadata.name`,
			value:    "my-pod",
			want: map[string]any{
				"spec": map[string]any{
					"template": map[string]any{
						"metadata": map[string]any{
							"name": "my-pod",
						},
					},
				},
			},
		},
		{
			name:     "create intermediate structures - quoted field names",
			resource: map[string]any{},
			path:     `spec.template.metadata.annotations["custom.annotation.name"]`,
			value:    "my-pod",
			want: map[string]any{
				"spec": map[string]any{
					"template": map[string]any{
						"metadata": map[string]any{
							"annotations": map[string]any{
								"custom.annotation.name": "my-pod",
							},
						},
					},
				},
			},
		},
		{
			name: "set array element",
			resource: map[string]any{
				"containers": []any{
					map[string]any{"name": "container1"},
				},
			},
			path:  "containers[1]",
			value: map[string]any{"name": "container2"},
			want: map[string]any{
				"containers": []any{
					map[string]any{"name": "container1"},
					map[string]any{"name": "container2"},
				},
			},
		},
		{
			name:     "create array and set element",
			resource: map[string]any{},
			path:     `spec.containers[0].ports[0].containerPort`,
			value:    8080,
			want: map[string]any{
				"spec": map[string]any{
					"containers": []any{
						map[string]any{
							"ports": []any{
								map[string]any{
									"containerPort": 8080,
								},
							},
						},
					},
				},
			},
		},
		{
			name: "extend existing array",
			resource: map[string]any{
				"args": []any{"arg1"},
			},
			path:  "args[2]",
			value: "arg3",
			want: map[string]any{
				"args": []any{
					"arg1",
					nil,
					"arg3",
				},
			},
		},
		{
			name: "overwrite existing value",
			resource: map[string]any{
				"metadata": map[string]any{
					"name": "old-name",
				},
			},
			path:  `metadata["name"]`,
			value: "new-name",
			want: map[string]any{
				"metadata": map[string]any{
					"name": "new-name",
				},
			},
		},
		{
			name:     "invalid path format",
			resource: map[string]any{},
			path:     `[invalid["path"]`,
			value:    "value",
			wantErr:  true,
			want:     map[string]any{},
		},
		{
			name:     "empty path returns early",
			resource: map[string]any{"existing": "value"},
			path:     "",
			value:    "ignored",
			wantErr:  false,
			want:     map[string]any{"existing": "value"},
		},
		{
			name: "expected map but got string",
			resource: map[string]any{
				"field": "string-value",
			},
			path:    "field.nested",
			value:   "test",
			wantErr: true,
			want: map[string]any{
				"field": "string-value",
			},
		},
		{
			name: "expected map but got array for field access",
			resource: map[string]any{
				"field": []any{"a", "b"},
			},
			path:    "field.nested",
			value:   "test",
			wantErr: true,
			want: map[string]any{
				"field": []any{"a", "b"},
			},
		},
		{
			name: "array segment on non-array non-nil value",
			resource: map[string]any{
				"field": "string-not-array",
			},
			path:    "field[0]",
			value:   "test",
			wantErr: true,
			want: map[string]any{
				"field": "string-not-array",
			},
		},
		{
			name:     "nested arrays and field at the end",
			resource: map[string]any{},
			path:     `matrix[0][0][0].value`,
			value:    "test",
			want: map[string]any{
				"matrix": []any{
					[]any{
						[]any{
							map[string]any{
								"value": "test",
							},
						},
					},
				},
			},
		},
		{
			name: "nested arraaaaays",
			resource: map[string]any{
				"matrix": []any{
					[]any{},
				},
			},
			// Making this work made me go crazy.
			value: "catch-me-if-you-can",
			path:  `matrix[0][0][0][0][3]`,
			want: map[string]any{
				"matrix": []any{
					[]any{
						[]any{
							[]any{
								[]any{
									nil,
									nil,
									nil,
									"catch-me-if-you-can",
								},
							},
						},
					},
				},
			},
		},
		{
			name: "array segment on nil value creates array",
			resource: map[string]any{
				"spec": map[string]any{
					"items": nil,
				},
			},
			path:  "spec.items[0]",
			value: "first",
			want: map[string]any{
				"spec": map[string]any{
					"items": []any{"first"},
				},
			},
		},
		{
			name: "nested nil array creation",
			resource: map[string]any{
				"data": map[string]any{
					"matrix": []any{nil},
				},
			},
			path:  "data.matrix[0][2]",
			value: "deep",
			want: map[string]any{
				"data": map[string]any{
					"matrix": []any{
						[]any{nil, nil, "deep"},
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewResolver(tt.resource, nil)
			err := r.setValueAtPath(tt.path, tt.value)

			if (err != nil) != tt.wantErr {
				t.Errorf("setValueAtPath() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr && !reflect.DeepEqual(tt.resource, tt.want) {
				t.Errorf("setValueAtPath() got = %v, want %v", tt.resource, tt.want)
			}
		})
	}
}

func TestResolveField(t *testing.T) {
	tests := []struct {
		name     string
		resource map[string]any
		data     map[string]any
		field    variable.FieldDescriptor
		want     ResolutionResult
	}{
		{
			name: "non data provided",
			resource: map[string]any{
				"spec": map[string]any{
					"field": "${notProvided}",
				},
			},
			field: variable.FieldDescriptor{
				Path:       "spec.field",
				Expression: krocel.NewUncompiled("notProvided"),
			},
			want: ResolutionResult{
				Path:     "spec.field",
				Resolved: false,
				Error:    fmt.Errorf("no data provided for expression: notProvided"),
			},
		},
		{
			name: "standalone expression simple path",
			resource: map[string]any{
				"spec": map[string]any{
					"field": "${value}",
				},
			},
			data: map[string]any{
				"value": []float64{1, 2, 3.5},
			},
			field: variable.FieldDescriptor{
				Path:       "spec.field",
				Expression: krocel.NewUncompiled("value"),
			},
			want: ResolutionResult{
				Path:     "spec.field",
				Resolved: true,
				Replaced: []float64{1, 2, 3.5},
			},
		},
		{
			name: "array path with standalone expression",
			resource: map[string]any{
				"spec": map[string]any{
					"array": []any{
						"${value}",
					},
				},
			},
			data: map[string]any{
				"value": "resolved",
			},
			field: variable.FieldDescriptor{
				Path:       "spec.array[0]",
				Expression: krocel.NewUncompiled("value"),
			},
			want: ResolutionResult{
				Path:     "spec.array[0]",
				Resolved: true,
				Replaced: "resolved",
			},
		},
		{
			name: "error - missing data for expression",
			resource: map[string]any{
				"spec": map[string]any{
					"field": "${missing}",
				},
			},
			data: map[string]any{},
			field: variable.FieldDescriptor{
				Path:       "spec.field",
				Expression: krocel.NewUncompiled("missing"),
			},
			want: ResolutionResult{
				Path:  "spec.field",
				Error: fmt.Errorf("no data provided for expression: missing"),
			},
		},
		{
			name: "error - invalid path",
			resource: map[string]any{
				"spec": map[string]any{},
			},
			data: map[string]any{
				"value": "resolved",
			},
			field: variable.FieldDescriptor{
				Path:       "spec.nonexistent.field",
				Expression: krocel.NewUncompiled("value"),
			},
			want: ResolutionResult{
				Path:  "spec.nonexistent.field",
				Error: fmt.Errorf("error getting value: key not found: nonexistent"),
			},
		},
		{
			name: "deeply nested array path",
			resource: map[string]any{
				"spec": map[string]any{
					"nested": map[string]any{
						"array": []any{
							map[string]any{
								"field": "${value}",
							},
						},
					},
				},
			},
			data: map[string]any{
				"value": "papa-ou-t-es",
			},
			field: variable.FieldDescriptor{
				Path:       "spec.nested.array[0].field",
				Expression: krocel.NewUncompiled("value"),
			},
			want: ResolutionResult{
				Path:     "spec.nested.array[0].field",
				Resolved: true,
				Replaced: "papa-ou-t-es",
			},
		},
		{
			name: "error - leading dot in path fails consistently",
			resource: map[string]any{
				"field": "${value}",
			},
			data: map[string]any{
				"value": "resolved",
			},
			field: variable.FieldDescriptor{
				Path:       ".field",
				Expression: krocel.NewUncompiled("value"),
			},
			want: ResolutionResult{
				Path:     ".field",
				Resolved: false,
				Error:    fmt.Errorf("error getting value: invalid path '.field': empty field name at position 0"),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewResolver(tt.resource, tt.data)
			got := r.resolveField(tt.field)

			assert.Equal(t, tt.want.Path, got.Path)
			assert.Equal(t, tt.want.Resolved, got.Resolved)
			assert.Equal(t, tt.want.Replaced, got.Replaced)

			if tt.want.Error != nil {
				assert.EqualError(t, got.Error, tt.want.Error.Error())
			} else {
				assert.NoError(t, got.Error)
			}

			if tt.want.Resolved {
				value, err := r.getValueFromPath(tt.field.Path)
				assert.NoError(t, err)
				assert.Equal(t, tt.want.Replaced, value)
			}
		})
	}
}

func TestResolveDynamicArrayIndexes(t *testing.T) {
	resource := map[string]any{
		"spec": map[string]any{
			"array": []any{
				"value1",
				"${value}",
				"value3",
			},
		},
	}

	data := map[string]any{
		"value": "replaced",
	}

	field := variable.FieldDescriptor{
		Path:       "spec.array[1]",
		Expression: krocel.NewUncompiled("value"),
	}

	r := NewResolver(resource, data)
	got := r.resolveField(field)

	assert.True(t, got.Resolved)
	assert.Equal(t, "replaced", got.Replaced)

	array, ok := r.resource["spec"].(map[string]any)["array"].([]any)
	assert.True(t, ok)

	// Verify that the array was updated and that other elements were not affected
	assert.Equal(t, "value1", array[0])
	assert.Equal(t, "replaced", array[1])
	assert.Equal(t, "value3", array[2])
}

func TestResolver(t *testing.T) {
	t.Run("successful resolution", func(t *testing.T) {
		r := NewResolver(
			map[string]any{
				"spec": map[string]any{
					"field": "${value}-${suffix}",
				},
			},
			map[string]any{
				"\"resolved-\" + \"done\"": "resolved-done",
			},
		)
		summary := r.Resolve([]variable.FieldDescriptor{
			{
				Path:       "spec.field",
				Expression: krocel.NewUncompiled("\"resolved-\" + \"done\""),
			},
		})
		assert.Equal(t, 1, summary.TotalExpressions)
		assert.Equal(t, 1, summary.ResolvedExpressions)
		assert.Equal(t, "resolved-done", summary.Results[0].Replaced)
		assert.Empty(t, summary.Errors)
	})

	t.Run("error aggregation", func(t *testing.T) {
		r := NewResolver(
			map[string]any{
				"spec": map[string]any{
					"field1": "${value1}",
					"field2": "${value2}",
				},
			},
			map[string]any{
				"value1": "resolved",
				// value2 is missing - will cause error
			},
		)
		summary := r.Resolve([]variable.FieldDescriptor{
			{
				Path:       "spec.field1",
				Expression: krocel.NewUncompiled("value1"),
			},
			{
				Path:       "spec.field2",
				Expression: krocel.NewUncompiled("value2"),
			},
		})
		assert.Equal(t, 2, summary.TotalExpressions)
		assert.Equal(t, 1, summary.ResolvedExpressions)
		assert.Len(t, summary.Errors, 1)
		assert.Contains(t, summary.Errors[0].Error(), "no data provided for expression: value2")
	})
}

func TestUpsertValueAtPath(t *testing.T) {
	t.Run("creates nested structure", func(t *testing.T) {
		resource := map[string]any{}
		r := NewResolver(resource, nil)

		err := r.UpsertValueAtPath("status.conditions[0].type", "Ready")

		assert.NoError(t, err)
		assert.Equal(t, map[string]any{
			"status": map[string]any{
				"conditions": []any{
					map[string]any{
						"type": "Ready",
					},
				},
			},
		}, resource)
	})

	t.Run("updates existing value", func(t *testing.T) {
		resource := map[string]any{
			"status": map[string]any{
				"phase": "Pending",
			},
		}
		r := NewResolver(resource, nil)

		err := r.UpsertValueAtPath("status.phase", "Running")

		assert.NoError(t, err)
		assert.Equal(t, "Running", resource["status"].(map[string]any)["phase"])
	})
}

// TestResolveFieldWithEmptyBraces tests the regression where strings.Trim() was
// incorrectly stripping {} from expressions. This affected ternary CEL expressions
// that end with empty maps like: condition ? value : {}
func TestResolveFieldWithEmptyBraces(t *testing.T) {
	tests := []struct {
		name     string
		resource map[string]any
		data     map[string]any
		field    variable.FieldDescriptor
		want     ResolutionResult
	}{
		{
			name: "standalone expression ending with empty braces",
			resource: map[string]any{
				"metadata": map[string]any{
					"annotations": "${includeAnnotations ? annotations : {}}",
				},
			},
			data: map[string]any{
				"includeAnnotations ? annotations : {}": map[string]any{},
			},
			field: variable.FieldDescriptor{
				Path:       "metadata.annotations",
				Expression: krocel.NewUncompiled("includeAnnotations ? annotations : {}"),
			},
			want: ResolutionResult{
				Path:     "metadata.annotations",
				Resolved: true,
				Replaced: map[string]any{},
			},
		},
		{
			name: "complex expression with has() and empty braces",
			resource: map[string]any{
				"spec": map[string]any{
					"config": "${has(schema.config) && includeConfig ? schema.config : {}}",
				},
			},
			data: map[string]any{
				"has(schema.config) && includeConfig ? schema.config : {}": map[string]any{
					"key": "value",
				},
			},
			field: variable.FieldDescriptor{
				Path:       "spec.config",
				Expression: krocel.NewUncompiled("has(schema.config) && includeConfig ? schema.config : {}"),
			},
			want: ResolutionResult{
				Path:     "spec.config",
				Resolved: true,
				Replaced: map[string]any{
					"key": "value",
				},
			},
		},
		{
			name: "expression with empty braces on both sides",
			resource: map[string]any{
				"data": map[string]any{
					"field": "${condition ? {} : {}}",
				},
			},
			data: map[string]any{
				"condition ? {} : {}": map[string]any{},
			},
			field: variable.FieldDescriptor{
				Path:       "data.field",
				Expression: krocel.NewUncompiled("condition ? {} : {}"),
			},
			want: ResolutionResult{
				Path:     "data.field",
				Resolved: true,
				Replaced: map[string]any{},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewResolver(tt.resource, tt.data)
			got := r.resolveField(tt.field)

			assert.Equal(t, tt.want.Path, got.Path)
			assert.Equal(t, tt.want.Resolved, got.Resolved)
			assert.Equal(t, tt.want.Replaced, got.Replaced)

			if tt.want.Error != nil {
				assert.EqualError(t, got.Error, tt.want.Error.Error())
			} else {
				assert.NoError(t, got.Error)
			}

			if tt.want.Resolved {
				value, err := r.getValueFromPath(tt.field.Path)
				assert.NoError(t, err)
				assert.Equal(t, tt.want.Replaced, value)
			}
		})
	}
}

func TestResolveFieldOmit(t *testing.T) {
	tests := []struct {
		name         string
		resource     map[string]any
		data         map[string]any
		field        variable.FieldDescriptor
		wantResolved bool
		wantSentinel bool
	}{
		{
			name: "omit sentinel is placed in map field",
			resource: map[string]any{
				"spec": map[string]any{
					"name":   "test",
					"policy": "${expr}",
				},
			},
			data: map[string]any{
				"expr": sentinels.Omit{},
			},
			field: variable.FieldDescriptor{
				Path:       "spec.policy",
				Expression: krocel.NewUncompiled("expr"),
			},
			wantResolved: true,
			wantSentinel: true,
		},
		{
			name: "omit sentinel is placed in array element",
			resource: map[string]any{
				"spec": map[string]any{
					"args": []any{"${expr}", "keep"},
				},
			},
			data: map[string]any{
				"expr": sentinels.Omit{},
			},
			field: variable.FieldDescriptor{
				Path:       "spec.args[0]",
				Expression: krocel.NewUncompiled("expr"),
			},
			wantResolved: true,
			wantSentinel: true,
		},
		{
			name: "non-sentinel value writes normally",
			resource: map[string]any{
				"spec": map[string]any{
					"policy": "${expr}",
				},
			},
			data: map[string]any{
				"expr": "my-policy",
			},
			field: variable.FieldDescriptor{
				Path:       "spec.policy",
				Expression: krocel.NewUncompiled("expr"),
			},
			wantResolved: true,
			wantSentinel: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewResolver(tt.resource, tt.data)
			result := r.resolveField(tt.field)

			assert.Equal(t, tt.wantResolved, result.Resolved)
			assert.NoError(t, result.Error)

			value, err := r.getValueFromPath(tt.field.Path)
			assert.NoError(t, err)

			if tt.wantSentinel {
				assert.True(t, sentinels.IsOmit(value))
				assert.True(t, sentinels.IsOmit(result.Replaced))
			} else {
				assert.False(t, sentinels.IsOmit(value))
				assert.Equal(t, tt.data[tt.field.Expression.Original], value)
			}
		})
	}
}

func TestCleanOmitSentinels(t *testing.T) {
	tests := []struct {
		name string
		in   map[string]any
		want map[string]any
	}{
		{
			name: "removes top-level map key",
			in: map[string]any{
				"keep": "value",
				"drop": sentinels.Omit{},
			},
			want: map[string]any{
				"keep": "value",
			},
		},
		{
			name: "removes nested map key",
			in: map[string]any{
				"spec": map[string]any{
					"name":   "test",
					"policy": sentinels.Omit{},
				},
			},
			want: map[string]any{
				"spec": map[string]any{
					"name": "test",
				},
			},
		},
		{
			name: "filters array elements",
			in: map[string]any{
				"spec": map[string]any{
					"args": []any{sentinels.Omit{}, "keep1", sentinels.Omit{}, "keep2"},
				},
			},
			want: map[string]any{
				"spec": map[string]any{
					"args": []any{"keep1", "keep2"},
				},
			},
		},
		{
			name: "filters single-element array to empty",
			in: map[string]any{
				"items": []any{sentinels.Omit{}},
			},
			want: map[string]any{
				"items": []any{},
			},
		},
		{
			name: "cleans deeply nested array inside array",
			in: map[string]any{
				"spec": map[string]any{
					"containers": []any{
						map[string]any{
							"args": []any{"keep", sentinels.Omit{}, "also-keep"},
						},
					},
				},
			},
			want: map[string]any{
				"spec": map[string]any{
					"containers": []any{
						map[string]any{
							"args": []any{"keep", "also-keep"},
						},
					},
				},
			},
		},
		{
			name: "no sentinels leaves resource unchanged",
			in: map[string]any{
				"spec": map[string]any{
					"name": "test",
					"args": []any{"a", "b"},
				},
			},
			want: map[string]any{
				"spec": map[string]any{
					"name": "test",
					"args": []any{"a", "b"},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cleanOmitSentinels(tt.in)
			assert.Equal(t, tt.want, tt.in)
		})
	}
}
