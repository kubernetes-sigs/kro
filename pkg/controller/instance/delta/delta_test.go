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

package delta

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestCompare_Simple(t *testing.T) {
	desired := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"spec": map[string]interface{}{
				"replicas": int64(3),
				"template": map[string]interface{}{
					"spec": map[string]interface{}{
						"containers": []interface{}{
							map[string]interface{}{
								"name":  "app",
								"image": "nginx:1.19",
							},
						},
					},
				},
			},
		},
	}

	observed := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"spec": map[string]interface{}{
				"replicas": int64(2),
				"template": map[string]interface{}{
					"spec": map[string]interface{}{
						"containers": []interface{}{
							map[string]interface{}{
								"name":  "app",
								"image": "nginx:1.18",
							},
						},
					},
				},
			},
		},
	}

	differences, err := Compare(desired, observed)
	assert.NoError(t, err)

	// Find differences by path
	replicasDiff := findDiffByPath(differences, "spec.replicas")
	assert.NotNil(t, replicasDiff)
	assert.Equal(t, int64(2), replicasDiff.Observed)
	assert.Equal(t, int64(3), replicasDiff.Desired)

	imageDiff := findDiffByPath(differences, "spec.template.spec.containers[0].image")
	assert.NotNil(t, imageDiff)
	assert.Equal(t, "nginx:1.18", imageDiff.Observed)
	assert.Equal(t, "nginx:1.19", imageDiff.Desired)
}

func TestCompare_Arrays(t *testing.T) {
	tests := []struct {
		name         string
		desired      []interface{}
		observed     []interface{}
		expectDiff   bool
		expectedPath string
		expectedOld  interface{}
		expectedNew  interface{}
	}{
		{
			name:         "different lengths",
			desired:      []interface{}{int64(1), int64(2), int64(3)},
			observed:     []interface{}{int64(1), int64(2)},
			expectDiff:   true,
			expectedPath: "spec.numbers",
			expectedOld:  []interface{}{int64(1), int64(2)},
			expectedNew:  []interface{}{int64(1), int64(2), int64(3)},
		},
		{
			name:       "same content",
			desired:    []interface{}{int64(1), int64(2), int64(3)},
			observed:   []interface{}{int64(1), int64(2), int64(3)},
			expectDiff: false,
		},
		{
			name:         "different content same length",
			desired:      []interface{}{int64(1), int64(2), int64(3)},
			observed:     []interface{}{int64(1), int64(2), int64(4)},
			expectDiff:   true,
			expectedPath: "spec.numbers[2]",
			expectedOld:  int64(4),
			expectedNew:  int64(3),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			desired := &unstructured.Unstructured{
				Object: map[string]interface{}{
					"spec": map[string]interface{}{
						"numbers": tt.desired,
					},
				},
			}
			observed := &unstructured.Unstructured{
				Object: map[string]interface{}{
					"spec": map[string]interface{}{
						"numbers": tt.observed,
					},
				},
			}

			differences, err := Compare(desired, observed)
			assert.NoError(t, err)

			if tt.expectDiff {
				diff := findDiffByPath(differences, tt.expectedPath)
				assert.NotNil(t, diff)
				assert.Equal(t, tt.expectedOld, diff.Observed)
				assert.Equal(t, tt.expectedNew, diff.Desired)
			} else {
				assert.Empty(t, differences)
			}
		})
	}
}

func TestCompare_NewField(t *testing.T) {
	desired := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"spec": map[string]interface{}{
				"newField": "value",
			},
		},
	}

	observed := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"spec": map[string]interface{}{},
		},
	}

	differences, err := Compare(desired, observed)
	assert.NoError(t, err)

	diff := findDiffByPath(differences, "spec.newField")
	assert.NotNil(t, diff)
	assert.Nil(t, diff.Observed)
	assert.Equal(t, "value", diff.Desired)
}

func findDiffByPath(diffs []Difference, path string) *Difference {
	for _, diff := range diffs {
		if diff.Path == path {
			return &diff
		}
	}
	return nil
}

func TestCompare_Comprehensive(t *testing.T) {
	desired := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"metadata": map[string]interface{}{
				"name": "test",
				"labels": map[string]interface{}{
					"env": "prod",
					"new": "label",
				},
			},
			"spec": map[string]interface{}{
				"replicas": int64(3),
				"selector": map[string]interface{}{
					"app": "test",
				},
				"template": map[string]interface{}{
					"metadata": map[string]interface{}{
						"labels": map[string]interface{}{
							"app": "test",
						},
					},
					"spec": map[string]interface{}{
						"containers": []interface{}{
							map[string]interface{}{
								"name":  "app",
								"image": "nginx:1.19",
								"env": []interface{}{
									map[string]interface{}{
										"name":  "DEBUG",
										"value": "true",
									},
									map[string]interface{}{
										"name":  "PORT",
										"value": "8080",
									},
								},
							},
						},
						"volumes": []interface{}{
							map[string]interface{}{
								"name": "config",
								"configMap": map[string]interface{}{
									"name": "app-config",
								},
							},
						},
					},
				},
			},
		},
	}

	observed := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"metadata": map[string]interface{}{
				"name": "test",
				"labels": map[string]interface{}{
					"env": "dev",      // changed value
					"old": "obsolete", // removed label
				},
				"resourceVersion":   "12345", // should be ignored
				"creationTimestamp": "NOTNIL",
			},
			"spec": map[string]interface{}{
				"replicas": int64(2), // changed value
				"selector": map[string]interface{}{
					"app": "test", // unchanged
				},
				"template": map[string]interface{}{
					"metadata": map[string]interface{}{
						"labels": map[string]interface{}{
							"app": "test",
						},
					},
					"spec": map[string]interface{}{
						"containers": []interface{}{
							map[string]interface{}{
								"name":  "app",
								"image": "nginx:1.18", // changed value
								"env": []interface{}{
									map[string]interface{}{
										"name":  "DEBUG",
										"value": "false", // changed value
									},
									// removed env var PORT
								},
								"resources": map[string]interface{}{
									"limits": map[string]interface{}{
										"cpu": "100m",
									},
								},
							},
						},
					},
				},
			},
		},
	}

	differences, err := Compare(desired, observed)
	assert.NoError(t, err)

	expectedChanges := map[string]struct {
		old interface{}
		new interface{}
	}{
		"metadata.labels.env":                    {old: "dev", new: "prod"},
		"metadata.labels.new":                    {old: nil, new: "label"},
		"spec.replicas":                          {old: int64(2), new: int64(3)},
		"spec.template.spec.containers[0].image": {old: "nginx:1.18", new: "nginx:1.19"},
		"spec.template.spec.containers[0].env": {
			old: []interface{}{
				map[string]interface{}{
					"name":  "DEBUG",
					"value": "false",
				},
			},
			new: []interface{}{
				map[string]interface{}{
					"name":  "DEBUG",
					"value": "true",
				},
				map[string]interface{}{
					"name":  "PORT",
					"value": "8080",
				},
			},
		},
		"spec.template.spec.volumes": {
			old: nil,
			new: []interface{}{
				map[string]interface{}{
					"name": "config",
					"configMap": map[string]interface{}{
						"name": "app-config",
					},
				},
			},
		},
	}

	assert.Equal(t, len(expectedChanges), len(differences), "number of differences should match")

	for _, diff := range differences {
		expected, ok := expectedChanges[diff.Path]
		assert.True(t, ok, "unexpected difference at path: %s", diff.Path)
		assert.Equal(t, expected.old, diff.Observed, "old value mismatch at path: %s", diff.Path)
		assert.Equal(t, expected.new, diff.Desired, "new value mismatch at path: %s", diff.Path)
	}
}

func TestCompare_EmptyMaps(t *testing.T) {
	tests := []struct {
		name     string
		desired  *unstructured.Unstructured
		observed *unstructured.Unstructured
		wantDiff bool
	}{
		{
			name: "empty maps in spec should not diff",
			desired: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"spec": map[string]interface{}{
						"tags": map[string]interface{}{},
					},
				},
			},
			observed: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"spec": map[string]interface{}{
						"tags": map[string]interface{}{},
					},
				},
			},
			wantDiff: false,
		},
		{
			name: "empty map in desired, no field in observed should diff",
			desired: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"spec": map[string]interface{}{
						"tags": map[string]interface{}{},
					},
				},
			},
			observed: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"spec": map[string]interface{}{},
				},
			},
			wantDiff: true,
		},
		{
			name: "nil map in desired, no field in observed should not diff",
			desired: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"spec": map[string]interface{}{
						"tags": nil,
					},
				},
			},
			observed: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"spec": map[string]interface{}{},
				},
			},
			wantDiff: false,
		},
		{
			name: "non-empty map should diff when values differ",
			desired: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"spec": map[string]interface{}{
						"tags": map[string]interface{}{
							"environment": "prod",
						},
					},
				},
			},
			observed: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"spec": map[string]interface{}{
						"tags": map[string]interface{}{
							"environment": "dev",
						},
					},
				},
			},
			wantDiff: true,
		},
		{
			name: "nested empty maps should diff",
			desired: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"spec": map[string]interface{}{
						"config": map[string]interface{}{
							"settings": map[string]interface{}{},
						},
					},
				},
			},
			observed: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"spec": map[string]interface{}{
						"config": map[string]interface{}{},
					},
				},
			},
			wantDiff: true,
		},
		{
			name: "non-empty map vs nil map should diff",
			desired: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"spec": map[string]interface{}{
						"tags": map[string]interface{}{
							"environment": "prod",
						},
					},
				},
			},
			observed: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"spec": map[string]interface{}{
						"tags": nil,
					},
				},
			},
			wantDiff: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			differences, err := Compare(tt.desired, tt.observed)
			assert.NoError(t, err)
			if tt.wantDiff {
				assert.NotEmpty(t, differences)
			} else {
				assert.Empty(t, differences,
					"expected no differences but got: %+v", differences)
			}
		})
	}
}
