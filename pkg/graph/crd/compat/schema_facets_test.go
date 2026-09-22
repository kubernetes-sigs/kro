// Copyright 2026 The Kubernetes Authors.
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
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/component-base/featuregate"

	"github.com/kubernetes-sigs/kro/pkg/features"
)

func setFeatureGate(t *testing.T, feature featuregate.Feature, enabled bool) {
	t.Helper()

	previous := features.FeatureGate.Enabled(feature)
	require.NoError(t, features.FeatureGate.Set(fmt.Sprintf("%s=%t", feature, enabled)))
	t.Cleanup(func() {
		require.NoError(t, features.FeatureGate.Set(fmt.Sprintf("%s=%t", feature, previous)))
	})
}

func TestCompareConservativeCRDComparisonFeatureGate(t *testing.T) {
	trueValue := true
	tests := []struct {
		name                   string
		oldSchema              *v1.JSONSchemaProps
		newSchema              *v1.JSONSchemaProps
		expectedChangeType     ChangeType
		breakingByDefault      bool
		breakingConservatively bool
	}{
		{
			name:      "CEL validation",
			oldSchema: &v1.JSONSchemaProps{Type: "object"},
			newSchema: &v1.JSONSchemaProps{
				Type:         "object",
				XValidations: v1.ValidationRules{{Rule: "self.size() < 2"}},
			},
			expectedChangeType:     ValidationRulesChanged,
			breakingConservatively: true,
		},
		{
			name:                   "format",
			oldSchema:              &v1.JSONSchemaProps{Type: "integer", Format: "int32"},
			newSchema:              &v1.JSONSchemaProps{Type: "integer", Format: "int64"},
			expectedChangeType:     FormatChanged,
			breakingConservatively: true,
		},
		{
			name: "map value property",
			oldSchema: &v1.JSONSchemaProps{
				Type: "object",
				AdditionalProperties: &v1.JSONSchemaPropsOrBool{
					Schema: &v1.JSONSchemaProps{
						Type: "object",
						Properties: map[string]v1.JSONSchemaProps{
							"removed": {Type: "string"},
						},
					},
				},
			},
			newSchema: &v1.JSONSchemaProps{
				Type: "object",
				AdditionalProperties: &v1.JSONSchemaPropsOrBool{
					Schema: &v1.JSONSchemaProps{Type: "object"},
				},
			},
			expectedChangeType:     PropertyRemoved,
			breakingConservatively: true,
		},
		{
			name:      "enum constraint added",
			oldSchema: &v1.JSONSchemaProps{Type: "string"},
			newSchema: &v1.JSONSchemaProps{
				Type: "string",
				Enum: []v1.JSON{{Raw: []byte(`"value"`)}},
			},
			expectedChangeType:     EnumConstraintAdded,
			breakingByDefault:      true,
			breakingConservatively: true,
		},
		{
			name:                   "nullable removed",
			oldSchema:              &v1.JSONSchemaProps{Type: "string", Nullable: true},
			newSchema:              &v1.JSONSchemaProps{Type: "string"},
			expectedChangeType:     NullableRemoved,
			breakingByDefault:      true,
			breakingConservatively: true,
		},
		{
			name: "unknown-field preservation removed",
			oldSchema: &v1.JSONSchemaProps{
				Type:                   "object",
				XPreserveUnknownFields: &trueValue,
			},
			newSchema:              &v1.JSONSchemaProps{Type: "object"},
			expectedChangeType:     PreserveUnknownFieldsRemoved,
			breakingByDefault:      true,
			breakingConservatively: true,
		},
		{
			name:                   "unclassified constraint",
			oldSchema:              &v1.JSONSchemaProps{Type: "array"},
			newSchema:              &v1.JSONSchemaProps{Type: "array", UniqueItems: true},
			expectedChangeType:     UnclassifiedSchemaChange,
			breakingConservatively: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			states := []struct {
				name             string
				conservative     bool
				expectedBreaking bool
			}{
				{
					name:             "default",
					expectedBreaking: tt.breakingByDefault,
				},
				{
					name:             "conservative",
					conservative:     true,
					expectedBreaking: tt.breakingConservatively,
				},
			}

			for _, state := range states {
				t.Run(state.name, func(t *testing.T) {
					setFeatureGate(t, features.ConservativeCRDComparison, state.conservative)
					report := Compare(tt.oldSchema, tt.newSchema)

					setFeatureGate(t, features.ConservativeCRDComparison, !state.conservative)
					explicitReport := Compare(
						tt.oldSchema,
						tt.newSchema,
						WithConservativeComparison(state.conservative),
					)

					assert.Equal(t, explicitReport, report)
					if !state.expectedBreaking {
						assert.False(t, report.HasBreakingChanges())
						return
					}
					require.Len(t, report.BreakingChanges, 1)
					assert.Equal(t, tt.expectedChangeType, report.BreakingChanges[0].ChangeType)
				})
			}
		})
	}
}

func TestCompareAdditionalPropertiesSchema(t *testing.T) {
	setFeatureGate(t, features.ConservativeCRDComparison, true)

	mapSchema := func(properties map[string]v1.JSONSchemaProps) *v1.JSONSchemaProps {
		return &v1.JSONSchemaProps{
			Type: "object",
			AdditionalProperties: &v1.JSONSchemaPropsOrBool{
				Schema: &v1.JSONSchemaProps{
					Type:       "object",
					Properties: properties,
				},
			},
		}
	}

	t.Run("removed map value property is breaking", func(t *testing.T) {
		t.Parallel()

		oldSchema := mapSchema(map[string]v1.JSONSchemaProps{
			"keep":    {Type: "string"},
			"removed": {Type: "string"},
		})
		newSchema := mapSchema(map[string]v1.JSONSchemaProps{
			"keep": {Type: "string"},
		})

		report := Compare(oldSchema, newSchema)

		require.Len(t, report.BreakingChanges, 1)
		assert.Equal(t, PropertyRemoved, report.BreakingChanges[0].ChangeType)
		assert.Equal(t, ".additionalProperties.properties.removed", report.BreakingChanges[0].Path)
	})

	t.Run("added optional map value property is non-breaking", func(t *testing.T) {
		t.Parallel()

		oldSchema := mapSchema(map[string]v1.JSONSchemaProps{
			"keep": {Type: "string"},
		})
		newSchema := mapSchema(map[string]v1.JSONSchemaProps{
			"keep":  {Type: "string"},
			"added": {Type: "string"},
		})

		report := Compare(oldSchema, newSchema)

		assert.False(t, report.HasBreakingChanges())
		require.Len(t, report.NonBreakingChanges, 1)
		assert.Equal(t, PropertyAdded, report.NonBreakingChanges[0].ChangeType)
		assert.Equal(t, ".additionalProperties.properties.added", report.NonBreakingChanges[0].Path)
	})

	t.Run("adding a map value schema is non-breaking", func(t *testing.T) {
		t.Parallel()

		oldSchema := &v1.JSONSchemaProps{Type: "object"}
		newSchema := mapSchema(nil)

		report := Compare(oldSchema, newSchema)

		assert.False(t, report.HasBreakingChanges())
		require.Len(t, report.NonBreakingChanges, 1)
		assert.Equal(t, AdditionalPropertiesChanged, report.NonBreakingChanges[0].ChangeType)
		assert.Equal(t, ".additionalProperties", report.NonBreakingChanges[0].Path)
	})
}

func TestCompareValidationRules(t *testing.T) {
	setFeatureGate(t, features.ConservativeCRDComparison, true)

	tests := []struct {
		name                string
		old                 v1.ValidationRules
		new                 v1.ValidationRules
		expectedBreaking    int
		expectedNonBreaking int
	}{
		{
			name:             "added",
			new:              v1.ValidationRules{{Rule: "self.size() < 2"}},
			expectedBreaking: 1,
		},
		{
			name:                "removed",
			old:                 v1.ValidationRules{{Rule: "self.size() < 2"}},
			expectedNonBreaking: 1,
		},
		{
			name:             "changed",
			old:              v1.ValidationRules{{Rule: "self.size() < 2"}},
			new:              v1.ValidationRules{{Rule: "self.size() < 3"}},
			expectedBreaking: 1,
		},
		{
			name: "nil and empty are equivalent",
			new:  v1.ValidationRules{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			report := Compare(
				&v1.JSONSchemaProps{Type: "object", XValidations: tt.old},
				&v1.JSONSchemaProps{Type: "object", XValidations: tt.new},
			)

			require.Len(t, report.BreakingChanges, tt.expectedBreaking)
			require.Len(t, report.NonBreakingChanges, tt.expectedNonBreaking)
			if tt.expectedBreaking > 0 {
				assert.Equal(t, ValidationRulesChanged, report.BreakingChanges[0].ChangeType)
				assert.Equal(t, ".x-kubernetes-validations", report.BreakingChanges[0].Path)
			}
			if tt.expectedNonBreaking > 0 {
				assert.Equal(t, ValidationRulesChanged, report.NonBreakingChanges[0].ChangeType)
				assert.Equal(t, ".x-kubernetes-validations", report.NonBreakingChanges[0].Path)
			}
		})
	}
}

func TestCompareValidationFacets(t *testing.T) {
	setFeatureGate(t, features.ConservativeCRDComparison, true)

	trueValue := true
	atomic := "atomic"
	set := "set"
	granular := "granular"

	tests := []struct {
		name              string
		old               v1.JSONSchemaProps
		new               v1.JSONSchemaProps
		breakingType      ChangeType
		nonBreakingType   ChangeType
		expectedFieldPath string
		noChange          bool
	}{
		{
			name:              "nullable removed",
			old:               v1.JSONSchemaProps{Type: "string", Nullable: true},
			new:               v1.JSONSchemaProps{Type: "string"},
			breakingType:      NullableRemoved,
			expectedFieldPath: ".nullable",
		},
		{
			name:              "nullable added",
			old:               v1.JSONSchemaProps{Type: "string"},
			new:               v1.JSONSchemaProps{Type: "string", Nullable: true},
			nonBreakingType:   NullableAdded,
			expectedFieldPath: ".nullable",
		},
		{
			name:              "format added",
			old:               v1.JSONSchemaProps{Type: "string"},
			new:               v1.JSONSchemaProps{Type: "string", Format: "date-time"},
			breakingType:      FormatChanged,
			expectedFieldPath: ".format",
		},
		{
			name:              "format removed",
			old:               v1.JSONSchemaProps{Type: "string", Format: "date-time"},
			new:               v1.JSONSchemaProps{Type: "string"},
			nonBreakingType:   FormatRemoved,
			expectedFieldPath: ".format",
		},
		{
			name:              "preserve unknown fields removed",
			old:               v1.JSONSchemaProps{Type: "object", XPreserveUnknownFields: &trueValue},
			new:               v1.JSONSchemaProps{Type: "object"},
			breakingType:      PreserveUnknownFieldsRemoved,
			expectedFieldPath: ".x-kubernetes-preserve-unknown-fields",
		},
		{
			name:              "preserve unknown fields added",
			old:               v1.JSONSchemaProps{Type: "object"},
			new:               v1.JSONSchemaProps{Type: "object", XPreserveUnknownFields: &trueValue},
			nonBreakingType:   PreserveUnknownFieldsAdded,
			expectedFieldPath: ".x-kubernetes-preserve-unknown-fields",
		},
		{
			name:              "list type changed from atomic to set is breaking",
			old:               v1.JSONSchemaProps{Type: "array", XListType: &atomic},
			new:               v1.JSONSchemaProps{Type: "array", XListType: &set},
			breakingType:      TopologyChanged,
			expectedFieldPath: ".x-kubernetes-list-type",
		},
		{
			name:              "list map keys changed",
			old:               v1.JSONSchemaProps{Type: "array", XListMapKeys: []string{"name"}},
			new:               v1.JSONSchemaProps{Type: "array", XListMapKeys: []string{"id"}},
			breakingType:      TopologyChanged,
			expectedFieldPath: ".x-kubernetes-list-map-keys",
		},
		{
			name:     "default list type made explicit",
			old:      v1.JSONSchemaProps{Type: "array"},
			new:      v1.JSONSchemaProps{Type: "array", XListType: &atomic},
			noChange: true,
		},
		{
			name:     "list map keys reordered",
			old:      v1.JSONSchemaProps{Type: "array", XListMapKeys: []string{"namespace", "name"}},
			new:      v1.JSONSchemaProps{Type: "array", XListMapKeys: []string{"name", "namespace"}},
			noChange: true,
		},
		{
			name:              "map type added",
			old:               v1.JSONSchemaProps{Type: "object"},
			new:               v1.JSONSchemaProps{Type: "object", XMapType: &granular},
			breakingType:      TopologyChanged,
			expectedFieldPath: ".x-kubernetes-map-type",
		},
		{
			name:              "unclassified constraint fails closed",
			old:               v1.JSONSchemaProps{Type: "array"},
			new:               v1.JSONSchemaProps{Type: "array", UniqueItems: true},
			breakingType:      UnclassifiedSchemaChange,
			expectedFieldPath: ".uniqueItems",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			report := Compare(&tt.old, &tt.new)
			if tt.noChange {
				assert.False(t, report.HasChanges())
				return
			}

			if tt.breakingType != "" {
				require.Len(t, report.BreakingChanges, 1)
				assert.Equal(t, tt.breakingType, report.BreakingChanges[0].ChangeType)
				assert.Equal(t, tt.expectedFieldPath, report.BreakingChanges[0].Path)
				return
			}

			assert.False(t, report.HasBreakingChanges())
			require.Len(t, report.NonBreakingChanges, 1)
			assert.Equal(t, tt.nonBreakingType, report.NonBreakingChanges[0].ChangeType)
			assert.Equal(t, tt.expectedFieldPath, report.NonBreakingChanges[0].Path)
		})
	}
}
