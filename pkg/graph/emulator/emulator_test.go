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

package emulator

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/kube-openapi/pkg/validation/spec"
)

func TestGenerateDummyCR(t *testing.T) {
	tests := []struct {
		name           string
		gvk            schema.GroupVersionKind
		schema         *spec.Schema
		validateOutput func(*testing.T, map[string]interface{})
	}{
		{
			name: "simple schema with basic types",
			gvk: schema.GroupVersionKind{
				Group:   "kro.run",
				Version: "v1alpha1",
				Kind:    "SimpleTest",
			},
			schema: &spec.Schema{
				SchemaProps: spec.SchemaProps{
					Properties: map[string]spec.Schema{
						"spec": {
							SchemaProps: spec.SchemaProps{
								Properties: map[string]spec.Schema{
									"stringField": {
										SchemaProps: spec.SchemaProps{
											Type: spec.StringOrArray{"string"},
										},
									},
									"intField": {
										SchemaProps: spec.SchemaProps{
											Type: spec.StringOrArray{"integer"},
										},
									},
									"boolField": {
										SchemaProps: spec.SchemaProps{
											Type: spec.StringOrArray{"boolean"},
										},
									},
								},
							},
						},
					},
				},
			},
			validateOutput: func(t *testing.T, obj map[string]interface{}) {
				spec, ok := obj["spec"].(map[string]interface{})
				require.True(t, ok, "spec should be an object")

				// Since those fields are generated randomly, we can only check the type
				// of the fields.
				assert.IsType(t, "", spec["stringField"], "stringField should be string")
				assert.IsType(t, int64(0), spec["intField"], "intField should be int64")
				assert.IsType(t, false, spec["boolField"], "boolField should be bool")
			},
		},
		{
			name: "complex schema with nested objects and arrays",
			gvk: schema.GroupVersionKind{
				Group:   "kro.run",
				Version: "v1alpha1",
				Kind:    "ComplexTest",
			},
			schema: &spec.Schema{
				SchemaProps: spec.SchemaProps{
					Properties: map[string]spec.Schema{
						"spec": {
							SchemaProps: spec.SchemaProps{
								Properties: map[string]spec.Schema{
									"config": {
										SchemaProps: spec.SchemaProps{
											Type: spec.StringOrArray{"object"},
											Properties: map[string]spec.Schema{
												"name": {
													SchemaProps: spec.SchemaProps{
														Type: spec.StringOrArray{"string"},
													},
												},
												"enabled": {
													SchemaProps: spec.SchemaProps{
														Type: spec.StringOrArray{"boolean"},
													},
												},
											},
										},
									},
									"items": {
										SchemaProps: spec.SchemaProps{
											Type: spec.StringOrArray{"array"},
											Items: &spec.SchemaOrArray{
												Schema: &spec.Schema{
													SchemaProps: spec.SchemaProps{
														Properties: map[string]spec.Schema{
															"id": {
																SchemaProps: spec.SchemaProps{
																	Type: spec.StringOrArray{"string"},
																},
															},
															"value": {
																SchemaProps: spec.SchemaProps{
																	Type: spec.StringOrArray{"integer"},
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
						"status": {
							SchemaProps: spec.SchemaProps{
								Properties: map[string]spec.Schema{
									"phase": {
										SchemaProps: spec.SchemaProps{
											Type: spec.StringOrArray{"string"},
										},
									},
									"replicas": {
										SchemaProps: spec.SchemaProps{
											Type: spec.StringOrArray{"integer"},
										},
									},
								},
							},
						},
					},
				},
			},
			validateOutput: func(t *testing.T, obj map[string]interface{}) {
				// Validate spec
				spec, ok := obj["spec"].(map[string]interface{})
				require.True(t, ok, "spec should be an object")

				// Validate config
				config, ok := spec["config"].(map[string]interface{})
				require.True(t, ok, "config should be an object")
				assert.IsType(t, "", config["name"], "config.name should be  string")
				assert.IsType(t, false, config["enabled"], "config.enabled should be bool")

				// Validate items
				items, ok := spec["items"].([]interface{})
				require.True(t, ok, "items should be an array")
				require.NotEmpty(t, items, "items should not be empty")

				for _, item := range items {
					itemMap, ok := item.(map[string]interface{})
					require.True(t, ok, "item should be an object")
					assert.IsType(t, "", itemMap["id"], "item.id should be string")
					assert.IsType(t, int64(0), itemMap["value"], "item.value should be int64")
				}

				// Validate status
				status, ok := obj["status"].(map[string]interface{})
				require.True(t, ok, "status should be an object")
				assert.IsType(t, "", status["phase"], "status.phase should be string")
				assert.IsType(t, int64(0), status["replicas"], "status.replicas should be int64")
			},
		},
		{
			name: "schema with enum values",
			gvk: schema.GroupVersionKind{
				Group:   "kro.run",
				Version: "v1alpha1",
				Kind:    "EnumTest",
			},
			schema: &spec.Schema{
				SchemaProps: spec.SchemaProps{
					Properties: map[string]spec.Schema{
						"spec": {
							SchemaProps: spec.SchemaProps{
								Properties: map[string]spec.Schema{
									"mode": {
										SchemaProps: spec.SchemaProps{
											Type: spec.StringOrArray{"string"},
											Enum: []interface{}{"Auto", "Manual", "Disabled"},
										},
									},
								},
							},
						},
					},
				},
			},
			validateOutput: func(t *testing.T, obj map[string]interface{}) {
				spec, ok := obj["spec"].(map[string]interface{})
				require.True(t, ok, "spec should be an object")

				mode, ok := spec["mode"].(string)
				require.True(t, ok, "mode should be string")
				assert.Contains(t, []string{"Auto", "Manual", "Disabled"}, mode)
			},
		},
		{
			name: "complex schema with nested objects and arrays using AnyOf",
			gvk: schema.GroupVersionKind{
				Group:   "kro.run",
				Version: "v1alpha1",
				Kind:    "AnyOfTest",
			},
			schema: &spec.Schema{
				SchemaProps: spec.SchemaProps{
					Properties: map[string]spec.Schema{
						"spec": {
							SchemaProps: spec.SchemaProps{
								Properties: map[string]spec.Schema{
									"config": {
										SchemaProps: spec.SchemaProps{
											Type: spec.StringOrArray{"object"},
											Properties: map[string]spec.Schema{
												"name": {
													SchemaProps: spec.SchemaProps{
														Type: spec.StringOrArray{"string"},
													},
												},
												"enabled": {
													SchemaProps: spec.SchemaProps{
														Type: spec.StringOrArray{"boolean"},
													},
												},
											},
										},
									},
									"items": {
										SchemaProps: spec.SchemaProps{
											Type: spec.StringOrArray{"array"},
											Items: &spec.SchemaOrArray{
												Schema: &spec.Schema{
													SchemaProps: spec.SchemaProps{
														Properties: map[string]spec.Schema{
															"id": {
																SchemaProps: spec.SchemaProps{
																	Type: spec.StringOrArray{"string"},
																},
															},
															"value": {
																SchemaProps: spec.SchemaProps{
																	AnyOf: []spec.Schema{
																		{
																			SchemaProps: spec.SchemaProps{
																				Type: spec.StringOrArray{"integer"},
																			},
																		},
																		{
																			SchemaProps: spec.SchemaProps{
																				Type: spec.StringOrArray{"string"},
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
						"status": {
							SchemaProps: spec.SchemaProps{
								Type: spec.StringOrArray{"object"},
								Properties: map[string]spec.Schema{
									"phase": {
										SchemaProps: spec.SchemaProps{
											Type: spec.StringOrArray{"string"},
										},
									},
									"replicas": {
										SchemaProps: spec.SchemaProps{
											Type: spec.StringOrArray{"integer"},
										},
									},
								},
							},
						},
					},
				},
			},
			validateOutput: func(t *testing.T, obj map[string]interface{}) {
				// Validate spec
				spec, ok := obj["spec"].(map[string]interface{})
				require.True(t, ok, "spec should be an object")

				// Validate config
				config, ok := spec["config"].(map[string]interface{})
				require.True(t, ok, "config should be an object")
				assert.IsType(t, "", config["name"], "config.name should be string")
				assert.IsType(t, false, config["enabled"], "config.enabled should be bool")

				// Validate items
				items, ok := spec["items"].([]interface{})
				require.True(t, ok, "items should be an array")
				require.NotEmpty(t, items, "items should not be empty")

				for _, item := range items {
					itemMap, ok := item.(map[string]interface{})
					require.True(t, ok, "item should be an object")
					assert.IsType(t, "", itemMap["id"], "item.id should be string")

					// Check if value is either int64 or string
					value := itemMap["value"]
					assert.True(t, isInt64OrString(value), "item.value should be either int64 or string")
				}

				// Validate status
				status, ok := obj["status"].(map[string]interface{})
				require.True(t, ok, "status should be an object")
				assert.IsType(t, "", status["phase"], "status.phase should be string")
				assert.IsType(t, int64(0), status["replicas"], "status.replicas should be int64")
			},
		},
		{
			name: "schema with number constraints",
			gvk: schema.GroupVersionKind{
				Group:   "kro.run",
				Version: "v1alpha1",
				Kind:    "ConstrainedTest",
			},
			schema: &spec.Schema{
				SchemaProps: spec.SchemaProps{
					Properties: map[string]spec.Schema{
						"spec": {
							SchemaProps: spec.SchemaProps{
								Properties: map[string]spec.Schema{
									"value": {
										SchemaProps: spec.SchemaProps{
											Type:    spec.StringOrArray{"integer"},
											Minimum: ptr(0.0),
											Maximum: ptr(100.0),
										},
									},
									"ratio": {
										SchemaProps: spec.SchemaProps{
											Type:    spec.StringOrArray{"number"},
											Minimum: ptr(0.0),
											Maximum: ptr(1.0),
										},
									},
								},
							},
						},
					},
				},
			},
			validateOutput: func(t *testing.T, obj map[string]interface{}) {
				spec, ok := obj["spec"].(map[string]interface{})
				require.True(t, ok, "spec should be a object")

				value, ok := spec["value"].(int64)
				require.True(t, ok, "value should be int64")
				assert.GreaterOrEqual(t, value, int64(0))
				assert.LessOrEqual(t, value, int64(100))

				ratio, ok := spec["ratio"].(float64)
				require.True(t, ok, "ratio should be float64")
				assert.GreaterOrEqual(t, ratio, 0.0)
				assert.LessOrEqual(t, ratio, 1.0)
			},
		},
		{
			name: "schema with array constraints",
			schema: &spec.Schema{
				SchemaProps: spec.SchemaProps{
					Properties: map[string]spec.Schema{
						"spec": {
							SchemaProps: spec.SchemaProps{
								Properties: map[string]spec.Schema{
									"items": {
										SchemaProps: spec.SchemaProps{
											Type: spec.StringOrArray{"array"},
											Items: &spec.SchemaOrArray{
												Schema: &spec.Schema{
													SchemaProps: spec.SchemaProps{
														Type: spec.StringOrArray{"string"},
													},
												},
											},
											MinItems: ptr[int64](10),
											MaxItems: ptr[int64](20),
										},
									},
								},
							},
						},
					},
				},
			},
			gvk: schema.GroupVersionKind{
				Group:   "kro.run",
				Version: "v1alpha1",
				Kind:    "ArrayTest",
			},
			validateOutput: func(t *testing.T, obj map[string]interface{}) {
				spec, ok := obj["spec"].(map[string]interface{})
				require.True(t, ok, "spec should be a object")

				items, ok := spec["items"].([]interface{})
				require.True(t, ok, "items should be an array")
				assert.GreaterOrEqual(t, len(items), 10)
				assert.LessOrEqual(t, len(items), 20)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := NewEmulator()
			cr, err := e.GenerateDummyCR(tt.gvk, tt.schema)
			require.NoError(t, err)
			require.NotNil(t, cr)

			assert.Equal(t, tt.gvk.GroupVersion().String(), cr.GetAPIVersion())
			assert.Equal(t, tt.gvk.Kind, cr.GetKind())
			assert.Equal(t, "default", cr.GetNamespace())
			assert.Equal(t, strings.ToLower(tt.gvk.Kind)+"-sample", cr.GetName())

			tt.validateOutput(t, cr.Object)
		})
	}
}

func TestGenerateDummyCRErrors(t *testing.T) {
	e := NewEmulator()
	gvk := schema.GroupVersionKind{
		Group:   "kro.run",
		Version: "v1alpha1",
		Kind:    "ErrorTest",
	}

	t.Run("nil schema", func(t *testing.T) {
		_, err := e.GenerateDummyCR(gvk, nil)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "schema is nil")
	})

	t.Run("invalid type", func(t *testing.T) {
		schema := &spec.Schema{
			SchemaProps: spec.SchemaProps{
				Properties: map[string]spec.Schema{
					"spec": {
						SchemaProps: spec.SchemaProps{
							Properties: map[string]spec.Schema{
								"field": {
									SchemaProps: spec.SchemaProps{
										Type: spec.StringOrArray{"invalid"},
									},
								},
							},
						},
					},
				},
			},
		}
		_, err := e.GenerateDummyCR(gvk, schema)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "unsupported type")
	})
}

func TestGenerateValueWithIntOrString(t *testing.T) {
	e := NewEmulator()

	t.Run("x-kubernetes-int-or-string present, generates integer", func(t *testing.T) {
		schema := &spec.Schema{
			VendorExtensible: spec.VendorExtensible{
				Extensions: map[string]interface{}{
					"x-kubernetes-int-or-string": true,
				},
			},
		}

		value, err := e.generateValue(schema)
		require.NoError(t, err)
		assert.IsType(t, int64(0), value, "Expected integer as default value for x-kubernetes-int-or-string")
	})

}

func TestGenerateValueWithPreserveUnknownFields(t *testing.T) {
	e := NewEmulator()

	t.Run("x-kubernetes-preserve-unknown-fields present, does not produce error", func(t *testing.T) {
		schema := &spec.Schema{
			VendorExtensible: spec.VendorExtensible{
				Extensions: map[string]interface{}{
					"x-kubernetes-preserve-unknown-fields": true,
				},
			},
		}

		_, err := e.generateValue(schema)
		require.NoError(t, err)
	})

}

func ptr[T comparable](v T) *T {
	return &v
}

// Helper function to check if a value is either int64 or string
func isInt64OrString(v interface{}) bool {
	switch v.(type) {
	case int64, string:
		return true
	default:
		return false
	}
}
