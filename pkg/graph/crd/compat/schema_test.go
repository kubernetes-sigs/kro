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
	v1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

func TestCompareSchemas(t *testing.T) {
	tests := []struct {
		name               string
		oldSchema          *v1.JSONSchemaProps
		newSchema          *v1.JSONSchemaProps
		breakingCount      int
		nonBreakingCount   int
		expectedChangeType ChangeType
	}{
		{
			name:      "identical schemas",
			oldSchema: &v1.JSONSchemaProps{Type: "object"},
			newSchema: &v1.JSONSchemaProps{Type: "object"},
		},
		{
			name:               "changed type",
			oldSchema:          &v1.JSONSchemaProps{Type: "object"},
			newSchema:          &v1.JSONSchemaProps{Type: "array"},
			breakingCount:      1,
			expectedChangeType: TypeChanged,
		},
		{
			name: "changed description",
			oldSchema: &v1.JSONSchemaProps{
				Type:        "object",
				Description: "old description",
			},
			newSchema: &v1.JSONSchemaProps{
				Type:        "object",
				Description: "new description",
			},
			nonBreakingCount: 1,
		},
		{
			name: "changed pattern",
			oldSchema: &v1.JSONSchemaProps{
				Type:    "string",
				Pattern: "^[a-z]+$",
			},
			newSchema: &v1.JSONSchemaProps{
				Type:    "string",
				Pattern: "^[a-z0-9]+$",
			},
			breakingCount:      1,
			expectedChangeType: PatternChanged,
		},
		{
			name: "added pattern - breaking",
			oldSchema: &v1.JSONSchemaProps{
				Type: "string",
			},
			newSchema: &v1.JSONSchemaProps{
				Type:    "string",
				Pattern: "^[a-z]+$",
			},
			breakingCount:      1,
			expectedChangeType: PatternAdded,
		},
		{
			name: "removed pattern - non-breaking",
			oldSchema: &v1.JSONSchemaProps{
				Type:    "string",
				Pattern: "^[a-z]+$",
			},
			newSchema: &v1.JSONSchemaProps{
				Type: "string",
			},
			nonBreakingCount: 1,
		},
		{
			name: "multiple breaking changes",
			oldSchema: &v1.JSONSchemaProps{
				Type: "object",
				Properties: map[string]v1.JSONSchemaProps{
					"prop1": {Type: "string"},
					"prop2": {Type: "integer"},
					"prop3": {Type: "boolean"},
				},
			},
			newSchema: &v1.JSONSchemaProps{
				Type: "object",
				Properties: map[string]v1.JSONSchemaProps{
					"prop1": {Type: "integer"}, // type changed
					// prop2 removed
					"prop3": {Type: "boolean"}, // unchanged
				},
			},
			breakingCount: 2,
		},
		{
			name: "multiple non-breaking changes",
			oldSchema: &v1.JSONSchemaProps{
				Type:        "object",
				Description: "old description",
				Properties: map[string]v1.JSONSchemaProps{
					"prop1": {Type: "string"},
				},
			},
			newSchema: &v1.JSONSchemaProps{
				Type:        "object",
				Description: "new description",
				Properties: map[string]v1.JSONSchemaProps{
					"prop1": {Type: "string"},  // Unchanged
					"prop2": {Type: "integer"}, // Added optional
				},
			},
			nonBreakingCount: 2, // Description changed + new property
		},
		{
			name: "mixed breaking and non-breaking changes",
			oldSchema: &v1.JSONSchemaProps{
				Type:        "object",
				Description: "old description",
				Properties: map[string]v1.JSONSchemaProps{
					"prop1": {Type: "string"},
					"prop2": {Type: "integer"},
				},
			},
			newSchema: &v1.JSONSchemaProps{
				Type:        "object",
				Description: "new description", // Non-breaking
				Properties: map[string]v1.JSONSchemaProps{
					"prop1": {Type: "boolean"}, // Breaking - type changed
					// prop2 removed - breaking
					"prop3": {Type: "string"}, // Non-breaking - added
				},
			},
			breakingCount:    2,
			nonBreakingCount: 2,
		},
		{
			name:             "nil old schema - non-breaking",
			oldSchema:        nil,
			newSchema:        &v1.JSONSchemaProps{Type: "object"},
			nonBreakingCount: 1,
		},
		{
			name:               "nil new schema - breaking",
			oldSchema:          &v1.JSONSchemaProps{Type: "object"},
			newSchema:          nil,
			breakingCount:      1,
			expectedChangeType: PropertyRemoved,
		},
		{
			name:      "both schemas nil - no changes",
			oldSchema: nil,
			newSchema: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := Compare(tt.oldSchema, tt.newSchema)

			if tt.breakingCount > 0 {
				assert.False(t, result.IsCompatible(), "Expected incompatible")
				assert.Equal(t, tt.breakingCount, len(result.BreakingChanges), "Unexpected number of breaking changes")

				if tt.expectedChangeType != "" && len(result.BreakingChanges) > 0 {
					assert.Equal(t, tt.expectedChangeType, result.BreakingChanges[0].ChangeType, "Unexpected change type")
				}
			} else {
				assert.True(t, result.IsCompatible(), "Expected compatible")
			}

			if tt.nonBreakingCount > 0 {
				assert.Equal(t, tt.nonBreakingCount, len(result.NonBreakingChanges),
					"Unexpected number of non-breaking changes")
			}
		})
	}
}

func TestCompareProperties(t *testing.T) {
	tests := []struct {
		name               string
		oldProps           map[string]v1.JSONSchemaProps
		newProps           map[string]v1.JSONSchemaProps
		oldRequired        []string
		newRequired        []string
		breakingCount      int
		nonBreakingCount   int
		expectedChangeType ChangeType
	}{
		{
			name: "removed property",
			oldProps: map[string]v1.JSONSchemaProps{
				"prop1": {Type: "string"},
				"prop2": {Type: "integer"},
			},
			newProps: map[string]v1.JSONSchemaProps{
				"prop1": {Type: "string"},
			},
			breakingCount:      1,
			expectedChangeType: PropertyRemoved,
		},
		{
			name: "multiple removed properties",
			oldProps: map[string]v1.JSONSchemaProps{
				"prop1": {Type: "string"},
				"prop2": {Type: "integer"},
				"prop3": {Type: "boolean"},
			},
			newProps: map[string]v1.JSONSchemaProps{
				"prop1": {Type: "string"},
			},
			breakingCount: 2,
		},
		{
			name: "added optional property",
			oldProps: map[string]v1.JSONSchemaProps{
				"prop1": {Type: "string"},
			},
			newProps: map[string]v1.JSONSchemaProps{
				"prop1": {Type: "string"},
				"prop2": {Type: "integer"},
			},
			nonBreakingCount: 1,
		},
		{
			name: "added required property without default",
			oldProps: map[string]v1.JSONSchemaProps{
				"prop1": {Type: "string"},
			},
			newProps: map[string]v1.JSONSchemaProps{
				"prop1": {Type: "string"},
				"prop2": {Type: "integer"},
			},
			newRequired:        []string{"prop2"},
			breakingCount:      1,
			expectedChangeType: PropertyAdded,
		},
		{
			name: "multiple required properties added without defaults",
			oldProps: map[string]v1.JSONSchemaProps{
				"prop1": {Type: "string"},
			},
			newProps: map[string]v1.JSONSchemaProps{
				"prop1": {Type: "string"},
				"prop2": {Type: "integer"},
				"prop3": {Type: "boolean"},
			},
			newRequired:   []string{"prop2", "prop3"},
			breakingCount: 2,
		},
		{
			name: "added required property with default",
			oldProps: map[string]v1.JSONSchemaProps{
				"prop1": {Type: "string"},
			},
			newProps: map[string]v1.JSONSchemaProps{
				"prop1": {Type: "string"},
				"prop2": {
					Type:    "integer",
					Default: &v1.JSON{Raw: []byte("42")},
				},
			},
			newRequired:      []string{"prop2"},
			nonBreakingCount: 1,
		},
		{
			name: "property type change",
			oldProps: map[string]v1.JSONSchemaProps{
				"prop1": {Type: "string"},
			},
			newProps: map[string]v1.JSONSchemaProps{
				"prop1": {Type: "integer"},
			},
			breakingCount:      1,
			expectedChangeType: TypeChanged,
		},
		{
			name: "multiple property type changes",
			oldProps: map[string]v1.JSONSchemaProps{
				"prop1": {Type: "string"},
				"prop2": {Type: "boolean"},
			},
			newProps: map[string]v1.JSONSchemaProps{
				"prop1": {Type: "integer"},
				"prop2": {Type: "number"},
			},
			breakingCount: 2,
		},
		{
			name: "mixed changes - breaking and non-breaking",
			oldProps: map[string]v1.JSONSchemaProps{
				"prop1": {Type: "string"},
				"prop2": {Type: "boolean"}, // removed
				"prop3": {Type: "integer"},
			},
			newProps: map[string]v1.JSONSchemaProps{
				"prop1": {Type: "integer"}, // type changed
				"prop3": {Type: "integer"}, // unchanged
				"prop4": {Type: "string"},  // added
			},
			breakingCount:    2,
			nonBreakingCount: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldSchema := &v1.JSONSchemaProps{
				Type:       "object",
				Properties: tt.oldProps,
				Required:   tt.oldRequired,
			}
			newSchema := &v1.JSONSchemaProps{
				Type:       "object",
				Properties: tt.newProps,
				Required:   tt.newRequired,
			}

			result := &Report{}
			compareProperties("root", oldSchema, newSchema, result)

			if tt.breakingCount > 0 {
				assert.True(t, result.HasBreakingChanges(), "Expected breaking changes")
				assert.Equal(t, tt.breakingCount, len(result.BreakingChanges),
					"Unexpected number of breaking changes")

				if tt.expectedChangeType != "" && len(result.BreakingChanges) > 0 {
					assert.Equal(t, tt.expectedChangeType, result.BreakingChanges[0].ChangeType,
						"Unexpected change type")
				}
			} else {
				assert.False(t, result.HasBreakingChanges(), "Expected no breaking changes")
			}

			if tt.nonBreakingCount > 0 {
				assert.Equal(t, tt.nonBreakingCount, len(result.NonBreakingChanges),
					"Unexpected number of non-breaking changes")
			}
		})
	}
}

func TestCompareRequiredFields(t *testing.T) {
	tests := []struct {
		name              string
		oldProps          map[string]v1.JSONSchemaProps
		newProps          map[string]v1.JSONSchemaProps
		oldRequired       []string
		newRequired       []string
		breakingCount     int
		nonBreakingCount  int
		checkBreakingType ChangeType
	}{
		{
			name: "no required fields",
			oldProps: map[string]v1.JSONSchemaProps{
				"prop1": {Type: "string"},
			},
			newProps: map[string]v1.JSONSchemaProps{
				"prop1": {Type: "string"},
			},
		},
		{
			name: "added required field for existing property",
			oldProps: map[string]v1.JSONSchemaProps{
				"prop1": {Type: "string"},
			},
			newProps: map[string]v1.JSONSchemaProps{
				"prop1": {Type: "string"},
			},
			newRequired:       []string{"prop1"},
			breakingCount:     1,
			checkBreakingType: RequiredAdded,
		},
		{
			name: "removed required field",
			oldProps: map[string]v1.JSONSchemaProps{
				"prop1": {Type: "string"},
			},
			newProps: map[string]v1.JSONSchemaProps{
				"prop1": {Type: "string"},
			},
			oldRequired:      []string{"prop1"},
			nonBreakingCount: 1,
		},
		{
			name: "ignore newly added property in required check",
			oldProps: map[string]v1.JSONSchemaProps{
				"prop1": {Type: "string"},
			},
			newProps: map[string]v1.JSONSchemaProps{
				"prop1": {Type: "string"},
				"prop2": {Type: "integer"},
			},
			newRequired: []string{"prop2"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldSchema := &v1.JSONSchemaProps{
				Type:       "object",
				Properties: tt.oldProps,
				Required:   tt.oldRequired,
			}
			newSchema := &v1.JSONSchemaProps{
				Type:       "object",
				Properties: tt.newProps,
				Required:   tt.newRequired,
			}

			result := &Report{}
			compareRequiredFields("root", oldSchema, newSchema, result)

			if tt.breakingCount > 0 {
				assert.True(t, result.HasBreakingChanges(), "Expected breaking changes")
				assert.Equal(t, tt.breakingCount, len(result.BreakingChanges), "Unexpected number of breaking changes")

				if tt.checkBreakingType != "" && len(result.BreakingChanges) > 0 {
					assert.Equal(t, tt.checkBreakingType, result.BreakingChanges[0].ChangeType,
						"Unexpected breaking change type")
				}
			} else {
				assert.False(t, result.HasBreakingChanges(), "Expected no breaking changes")
			}

			if tt.nonBreakingCount > 0 {
				assert.Equal(t, tt.nonBreakingCount, len(result.NonBreakingChanges),
					"Unexpected number of non-breaking changes")
			}
		})
	}
}

func TestCompareEnumValues(t *testing.T) {
	tests := []struct {
		name              string
		oldEnum           []v1.JSON
		newEnum           []v1.JSON
		expectBreaking    bool
		breakingCount     int
		nonBreakingCount  int
		checkBreakingType ChangeType
	}{
		{
			name:           "identical enums",
			oldEnum:        []v1.JSON{{Raw: []byte("\"value1\"")}},
			newEnum:        []v1.JSON{{Raw: []byte("\"value1\"")}},
			expectBreaking: false,
		},
		{
			name:              "removed enum value",
			oldEnum:           []v1.JSON{{Raw: []byte("\"value1\"")}, {Raw: []byte("\"value2\"")}},
			newEnum:           []v1.JSON{{Raw: []byte("\"value1\"")}},
			expectBreaking:    true,
			breakingCount:     1,
			checkBreakingType: EnumRestricted,
		},
		{
			name:             "added enum value",
			oldEnum:          []v1.JSON{{Raw: []byte("\"value1\"")}},
			newEnum:          []v1.JSON{{Raw: []byte("\"value1\"")}, {Raw: []byte("\"value2\"")}},
			expectBreaking:   false,
			nonBreakingCount: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldSchema := &v1.JSONSchemaProps{
				Type: "string",
				Enum: tt.oldEnum,
			}
			newSchema := &v1.JSONSchemaProps{
				Type: "string",
				Enum: tt.newEnum,
			}

			result := &Report{}
			compareEnumValues("root", oldSchema, newSchema, result)

			if tt.expectBreaking {
				assert.True(t, result.HasBreakingChanges(), "Expected breaking changes")
				assert.Equal(t, tt.breakingCount, len(result.BreakingChanges), "Unexpected number of breaking changes")

				if tt.checkBreakingType != "" && len(result.BreakingChanges) > 0 {
					assert.Equal(t, tt.checkBreakingType, result.BreakingChanges[0].ChangeType,
						"Unexpected breaking change type")
				}
			} else {
				assert.False(t, result.HasBreakingChanges(), "Expected no breaking changes")
			}

			if tt.nonBreakingCount > 0 {
				assert.Equal(t, tt.nonBreakingCount, len(result.NonBreakingChanges),
					"Unexpected number of non-breaking changes")
			}
		})
	}
}

func TestCompareArrayItems(t *testing.T) {
	tests := []struct {
		name              string
		oldItems          *v1.JSONSchemaPropsOrArray
		newItems          *v1.JSONSchemaPropsOrArray
		expectBreaking    bool
		breakingCount     int
		nonBreakingCount  int
		checkBreakingType ChangeType
	}{
		{
			name: "identical array items",
			oldItems: &v1.JSONSchemaPropsOrArray{
				Schema: &v1.JSONSchemaProps{Type: "string"},
			},
			newItems: &v1.JSONSchemaPropsOrArray{
				Schema: &v1.JSONSchemaProps{Type: "string"},
			},
			expectBreaking: false,
		},
		{
			name: "changed item type",
			oldItems: &v1.JSONSchemaPropsOrArray{
				Schema: &v1.JSONSchemaProps{Type: "string"},
			},
			newItems: &v1.JSONSchemaPropsOrArray{
				Schema: &v1.JSONSchemaProps{Type: "integer"},
			},
			expectBreaking:    true,
			breakingCount:     1,
			checkBreakingType: TypeChanged,
		},
		{
			name: "removed items schema",
			oldItems: &v1.JSONSchemaPropsOrArray{
				Schema: &v1.JSONSchemaProps{Type: "string"},
			},
			newItems:          nil,
			expectBreaking:    true,
			breakingCount:     1,
			checkBreakingType: PropertyRemoved,
		},
		{
			name:     "added items schema",
			oldItems: nil,
			newItems: &v1.JSONSchemaPropsOrArray{
				Schema: &v1.JSONSchemaProps{Type: "string"},
			},
			expectBreaking:   false,
			nonBreakingCount: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldSchema := &v1.JSONSchemaProps{
				Type:  "array",
				Items: tt.oldItems,
			}
			newSchema := &v1.JSONSchemaProps{
				Type:  "array",
				Items: tt.newItems,
			}

			result := &Report{}
			compareArrayItems("root", oldSchema, newSchema, result)

			if tt.expectBreaking {
				assert.True(t, result.HasBreakingChanges(), "Expected breaking changes")
				assert.Equal(t, tt.breakingCount, len(result.BreakingChanges), "Unexpected number of breaking changes")

				if tt.checkBreakingType != "" && len(result.BreakingChanges) > 0 {
					assert.Equal(t, tt.checkBreakingType, result.BreakingChanges[0].ChangeType,
						"Unexpected breaking change type")
				}
			} else {
				assert.False(t, result.HasBreakingChanges(), "Expected no breaking changes")
			}

			if tt.nonBreakingCount > 0 {
				assert.Equal(t, tt.nonBreakingCount, len(result.NonBreakingChanges),
					"Unexpected number of non-breaking changes")
			}
		})
	}
}
