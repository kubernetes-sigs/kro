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

package compat

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		name              string
		oldVersions       []v1.CustomResourceDefinitionVersion
		newVersions       []v1.CustomResourceDefinitionVersion
		expectError       bool
		errorMessage      string
		expectBreaking    bool
		breakingCount     int
		nonBreakingCount  int
		checkBreakingType ChangeType
	}{
		{
			name: "identical versions",
			oldVersions: []v1.CustomResourceDefinitionVersion{
				{
					Name: "v1",
					Schema: &v1.CustomResourceValidation{
						OpenAPIV3Schema: &v1.JSONSchemaProps{
							Type: "object",
							Properties: map[string]v1.JSONSchemaProps{
								"spec": {
									Type: "object",
									Properties: map[string]v1.JSONSchemaProps{
										"replicas": {Type: "integer"},
									},
								},
							},
						},
					},
				},
			},
			newVersions: []v1.CustomResourceDefinitionVersion{
				{
					Name: "v1",
					Schema: &v1.CustomResourceValidation{
						OpenAPIV3Schema: &v1.JSONSchemaProps{
							Type: "object",
							Properties: map[string]v1.JSONSchemaProps{
								"spec": {
									Type: "object",
									Properties: map[string]v1.JSONSchemaProps{
										"replicas": {Type: "integer"},
									},
								},
							},
						},
					},
				},
			},
			expectError:    false,
			expectBreaking: false,
		},
		{
			name: "too many versions",
			oldVersions: []v1.CustomResourceDefinitionVersion{
				{Name: "v1"},
				{Name: "v2"},
			},
			newVersions: []v1.CustomResourceDefinitionVersion{
				{Name: "v1"},
			},
			expectError:  true,
			errorMessage: "expected exactly one version",
		},
		{
			name: "missing schema in old version",
			oldVersions: []v1.CustomResourceDefinitionVersion{
				{
					Name: "v1",
				},
			},
			newVersions: []v1.CustomResourceDefinitionVersion{
				{
					Name: "v1",
					Schema: &v1.CustomResourceValidation{
						OpenAPIV3Schema: &v1.JSONSchemaProps{Type: "object"},
					},
				},
			},
			expectError:  true,
			errorMessage: "has no schema",
		},
		{
			name: "missing schema in new version",
			oldVersions: []v1.CustomResourceDefinitionVersion{
				{
					Name: "v1",
					Schema: &v1.CustomResourceValidation{
						OpenAPIV3Schema: &v1.JSONSchemaProps{Type: "object"},
					},
				},
			},
			newVersions: []v1.CustomResourceDefinitionVersion{
				{
					Name: "v1",
				},
			},
			expectError:  true,
			errorMessage: "has no schema",
		},
		{
			name: "breaking change - property removed",
			oldVersions: []v1.CustomResourceDefinitionVersion{
				{
					Name: "v1",
					Schema: &v1.CustomResourceValidation{
						OpenAPIV3Schema: &v1.JSONSchemaProps{
							Type: "object",
							Properties: map[string]v1.JSONSchemaProps{
								"spec": {
									Type: "object",
									Properties: map[string]v1.JSONSchemaProps{
										"replicas": {Type: "integer"},
										"selector": {Type: "object"},
									},
								},
							},
						},
					},
				},
			},
			newVersions: []v1.CustomResourceDefinitionVersion{
				{
					Name: "v1",
					Schema: &v1.CustomResourceValidation{
						OpenAPIV3Schema: &v1.JSONSchemaProps{
							Type: "object",
							Properties: map[string]v1.JSONSchemaProps{
								"spec": {
									Type: "object",
									Properties: map[string]v1.JSONSchemaProps{
										"replicas": {Type: "integer"},
									},
								},
							},
						},
					},
				},
			},
			expectError:       false,
			expectBreaking:    true,
			breakingCount:     1,
			checkBreakingType: PropertyRemoved,
		},
		{
			name: "non-breaking change - added optional property",
			oldVersions: []v1.CustomResourceDefinitionVersion{
				{
					Name: "v1",
					Schema: &v1.CustomResourceValidation{
						OpenAPIV3Schema: &v1.JSONSchemaProps{
							Type: "object",
							Properties: map[string]v1.JSONSchemaProps{
								"spec": {
									Type: "object",
									Properties: map[string]v1.JSONSchemaProps{
										"replicas": {Type: "integer"},
									},
								},
							},
						},
					},
				},
			},
			newVersions: []v1.CustomResourceDefinitionVersion{
				{
					Name: "v1",
					Schema: &v1.CustomResourceValidation{
						OpenAPIV3Schema: &v1.JSONSchemaProps{
							Type: "object",
							Properties: map[string]v1.JSONSchemaProps{
								"spec": {
									Type: "object",
									Properties: map[string]v1.JSONSchemaProps{
										"replicas": {Type: "integer"},
										"selector": {Type: "object"},
									},
								},
							},
						},
					},
				},
			},
			expectError:      false,
			expectBreaking:   false,
			nonBreakingCount: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := CompareVersions(tt.oldVersions, tt.newVersions)

			if tt.expectError {
				require.Error(t, err)
				if tt.errorMessage != "" {
					assert.Contains(t, err.Error(), tt.errorMessage)
				}
				return
			}

			require.NoError(t, err)

			if tt.expectBreaking {
				assert.False(t, result.IsCompatible(), "Expected incompatible (breaking changes)")
				assert.Equal(t, tt.breakingCount, len(result.BreakingChanges), "Unexpected number of breaking changes")

				if tt.checkBreakingType != "" && len(result.BreakingChanges) > 0 {
					assert.Equal(t, tt.checkBreakingType, result.BreakingChanges[0].ChangeType,
						"Unexpected breaking change type")
				}
			} else {
				assert.True(t, result.IsCompatible(), "Expected compatible (no breaking changes)")
			}

			if tt.nonBreakingCount > 0 {
				assert.Equal(t, tt.nonBreakingCount, len(result.NonBreakingChanges),
					"Unexpected number of non-breaking changes")
			}
		})
	}
}
