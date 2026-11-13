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

package compat

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	"github.com/kubernetes-sigs/kro/pkg/features"
)

func TestCompareVersionsConservativeComparisonOption(t *testing.T) {
	oldVersions := []v1.CustomResourceDefinitionVersion{{
		Name: "v1",
		Schema: &v1.CustomResourceValidation{OpenAPIV3Schema: &v1.JSONSchemaProps{
			Type: "integer", Format: "int32",
		}},
	}}
	newVersions := []v1.CustomResourceDefinitionVersion{{
		Name: "v1",
		Schema: &v1.CustomResourceValidation{OpenAPIV3Schema: &v1.JSONSchemaProps{
			Type: "integer", Format: "int64",
		}},
	}}

	for _, tc := range []struct {
		name         string
		gateEnabled  bool
		conservative bool
	}{
		{name: "enable with gate off", gateEnabled: false, conservative: true},
		{name: "disable with gate on", gateEnabled: true, conservative: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setFeatureGate(t, features.ConservativeCRDComparison, tc.gateEnabled)

			got, err := CompareVersions(oldVersions, newVersions, WithConservativeComparison(tc.conservative))
			require.NoError(t, err)
			want := Compare(oldVersions[0].Schema.OpenAPIV3Schema, newVersions[0].Schema.OpenAPIV3Schema,
				WithConservativeComparison(tc.conservative))
			assert.Equal(t, want, got.Versions["v1"])
			assert.Equal(t, tc.conservative, got.Versions["v1"].HasBreakingChanges())
		})
	}
}

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
		expectAdded       []string
		expectRemoved     []string
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
			name: "version added and removed",
			oldVersions: []v1.CustomResourceDefinitionVersion{
				{Name: "v1", Schema: &v1.CustomResourceValidation{OpenAPIV3Schema: &v1.JSONSchemaProps{Type: "object"}}},
				{Name: "v1alpha1", Schema: &v1.CustomResourceValidation{OpenAPIV3Schema: &v1.JSONSchemaProps{Type: "object"}}},
			},
			newVersions: []v1.CustomResourceDefinitionVersion{
				{Name: "v1", Schema: &v1.CustomResourceValidation{OpenAPIV3Schema: &v1.JSONSchemaProps{Type: "object"}}},
				{Name: "v2", Schema: &v1.CustomResourceValidation{OpenAPIV3Schema: &v1.JSONSchemaProps{Type: "object"}}},
			},
			expectAdded:   []string{"v2"},
			expectRemoved: []string{"v1alpha1"},
		},
		{
			name: "no longer served is a metadata change, not a breaking one",
			oldVersions: []v1.CustomResourceDefinitionVersion{
				{Name: "v1", Served: true, Schema: &v1.CustomResourceValidation{OpenAPIV3Schema: &v1.JSONSchemaProps{Type: "object"}}},
			},
			newVersions: []v1.CustomResourceDefinitionVersion{
				{Name: "v1", Served: false, Schema: &v1.CustomResourceValidation{OpenAPIV3Schema: &v1.JSONSchemaProps{Type: "object"}}},
			},
			nonBreakingCount: 1,
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
		{
			name: "non-breaking change - additional printer columns changed",
			oldVersions: []v1.CustomResourceDefinitionVersion{
				{
					Name: "v1",
					Schema: &v1.CustomResourceValidation{
						OpenAPIV3Schema: &v1.JSONSchemaProps{Type: "object"},
					},
					AdditionalPrinterColumns: []v1.CustomResourceColumnDefinition{
						{Name: "Replicas", Type: "integer", JSONPath: ".spec.replicas"},
					},
				},
			},
			newVersions: []v1.CustomResourceDefinitionVersion{
				{
					Name: "v1",
					Schema: &v1.CustomResourceValidation{
						OpenAPIV3Schema: &v1.JSONSchemaProps{Type: "object"},
					},
					AdditionalPrinterColumns: []v1.CustomResourceColumnDefinition{
						{Name: "Replicas", Type: "integer", JSONPath: ".spec.replicas"},
						{Name: "Age", Type: "date", JSONPath: ".metadata.creationTimestamp"},
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
			versionsReport, err := CompareVersions(tt.oldVersions, tt.newVersions)

			if tt.expectError {
				require.Error(t, err)
				if tt.errorMessage != "" {
					assert.Contains(t, err.Error(), tt.errorMessage)
				}
				return
			}

			require.NoError(t, err)
			assert.ElementsMatch(t, tt.expectAdded, versionsReport.Added)
			assert.ElementsMatch(t, tt.expectRemoved, versionsReport.Removed)

			result, ok := versionsReport.Versions["v1"]
			require.True(t, ok, "expected a report for version v1")

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

func TestCompareAcrossVersions(t *testing.T) {
	hub := v1.CustomResourceDefinitionVersion{
		Name: "v1beta1",
		Schema: &v1.CustomResourceValidation{OpenAPIV3Schema: &v1.JSONSchemaProps{
			Type: "object",
			Properties: map[string]v1.JSONSchemaProps{
				"spec": {Type: "object", Properties: map[string]v1.JSONSchemaProps{
					"replicas": {Type: "integer"},
				}},
			},
		}},
	}
	spoke := v1.CustomResourceDefinitionVersion{
		Name: "v1alpha1",
		Schema: &v1.CustomResourceValidation{OpenAPIV3Schema: &v1.JSONSchemaProps{
			Type: "object",
			Properties: map[string]v1.JSONSchemaProps{
				"spec": {Type: "object", Properties: map[string]v1.JSONSchemaProps{
					"replicas": {Type: "integer"},
					"retired":  {Type: "string"},
				}},
			},
		}},
	}

	// The spoke declares a field the hub cannot serve: an object stored under
	// the hub loses it when read as the spoke.
	report, err := CompareAcrossVersions(spoke, hub)
	require.NoError(t, err)
	assert.False(t, report.IsCompatible())
	assert.Equal(t, PropertyRemoved, report.BreakingChanges[0].ChangeType)

	// The other direction only adds a property, which is compatible.
	report, err = CompareAcrossVersions(hub, spoke)
	require.NoError(t, err)
	assert.True(t, report.IsCompatible())

	_, err = CompareAcrossVersions(v1.CustomResourceDefinitionVersion{Name: "v1"}, hub)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "has no schema")
}
