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

package crd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	"github.com/kro-run/kro/api/v1alpha1"
)

const state string = "State"

func TestSynthesizeCRD(t *testing.T) {
	tests := []struct {
		name                 string
		group                string
		apiVersion           string
		kind                 string
		spec                 extv1.JSONSchemaProps
		status               extv1.JSONSchemaProps
		statusFieldsOverride bool
		expectedName         string
		expectedGroup        string
	}{
		{
			name:                 "standard group and kind",
			group:                "kro.com",
			apiVersion:           "v1",
			kind:                 "Widget",
			spec:                 extv1.JSONSchemaProps{Type: "object"},
			status:               extv1.JSONSchemaProps{Type: "object"},
			statusFieldsOverride: true,
			expectedName:         "widgets.kro.com",
			expectedGroup:        "kro.com",
		},
		{
			name:                 "empty group uses default domain",
			group:                "",
			apiVersion:           "v1alphav2",
			kind:                 "Service",
			spec:                 extv1.JSONSchemaProps{Type: "object"},
			status:               extv1.JSONSchemaProps{Type: "object"},
			statusFieldsOverride: false,
			expectedName:         "services." + v1alpha1.KRODomainName,
			expectedGroup:        v1alpha1.KRODomainName,
		},
		{
			name:                 "mixes case kind",
			group:                "kro.com",
			apiVersion:           "v2",
			kind:                 "DataBase",
			spec:                 extv1.JSONSchemaProps{Type: "object"},
			status:               extv1.JSONSchemaProps{Type: "object"},
			statusFieldsOverride: true,
			expectedName:         "databases.kro.com",
			expectedGroup:        "kro.com",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			crd := SynthesizeCRD(tt.group, tt.apiVersion, tt.kind, tt.spec, tt.status, tt.statusFieldsOverride, v1alpha1.AdditionalPrinterColumnPolicyReplace, nil)

			assert.Equal(t, tt.expectedName, crd.Name)
			assert.Equal(t, tt.expectedGroup, crd.Spec.Group)
			assert.Equal(t, tt.kind, crd.Spec.Names.Kind)
			assert.Equal(t, tt.kind+"List", crd.Spec.Names.ListKind)

			require.Len(t, crd.Spec.Versions, 1)
			version := crd.Spec.Versions[0]
			assert.Equal(t, tt.apiVersion, version.Name)
			assert.True(t, version.Served)
			assert.True(t, version.Storage)

			require.NotNil(t, version.Schema)
			require.NotNil(t, version.Schema.OpenAPIV3Schema)

			require.NotNil(t, version.Subresources)
			require.NotNil(t, version.Subresources.Status)

			assert.Equal(t, defaultAdditionalPrinterColumns, version.AdditionalPrinterColumns)
		})
	}
}

func TestNewCRD(t *testing.T) {
	tests := []struct {
		name                     string
		group                    string
		apiVersion               string
		kind                     string
		additionalPrinterColumns []extv1.CustomResourceColumnDefinition
		expectedName             string
		expectedKind             string
		expectedPlural           string
		expectedSingular         string
		expectedPrinterColumns   []extv1.CustomResourceColumnDefinition
	}{
		{
			name:                   "basic example",
			group:                  "kro.com",
			apiVersion:             "v1",
			kind:                   "Test",
			expectedName:           "tests.kro.com",
			expectedKind:           "Test",
			expectedPlural:         "tests",
			expectedSingular:       "test",
			expectedPrinterColumns: defaultAdditionalPrinterColumns,
		},
		{
			name:                   "uppercase kind",
			group:                  "kro.com",
			apiVersion:             "v2beta1",
			kind:                   "CONFIG",
			expectedName:           "configs.kro.com",
			expectedKind:           "CONFIG",
			expectedPlural:         "configs",
			expectedSingular:       "config",
			expectedPrinterColumns: defaultAdditionalPrinterColumns,
		},
		{
			name:                   "mixed case kind",
			group:                  "kro.com",
			apiVersion:             "v2beta1",
			kind:                   "WebHook",
			expectedName:           "webhooks.kro.com",
			expectedKind:           "WebHook",
			expectedPlural:         "webhooks",
			expectedSingular:       "webhook",
			expectedPrinterColumns: defaultAdditionalPrinterColumns,
		},
		{
			name:                     "non nil empty printer columns",
			group:                    "kro.com",
			apiVersion:               "v2beta1",
			kind:                     "WebHook",
			additionalPrinterColumns: []extv1.CustomResourceColumnDefinition{},
			expectedName:             "webhooks.kro.com",
			expectedKind:             "WebHook",
			expectedPlural:           "webhooks",
			expectedSingular:         "webhook",
			expectedPrinterColumns:   defaultAdditionalPrinterColumns,
		},
		{
			name:       "custom printer columns",
			group:      "kro.com",
			apiVersion: "v2beta1",
			kind:       "WebHook",
			additionalPrinterColumns: []extv1.CustomResourceColumnDefinition{
				{
					Name:     "Available replicas",
					Type:     "integer",
					JSONPath: ".status.availableReplicas",
				},
				{
					Name:     "Image",
					Type:     "string",
					JSONPath: ".spec.image",
				},
			},
			expectedName:     "webhooks.kro.com",
			expectedKind:     "WebHook",
			expectedPlural:   "webhooks",
			expectedSingular: "webhook",
			expectedPrinterColumns: []extv1.CustomResourceColumnDefinition{
				{
					Name:     "Available replicas",
					Type:     "integer",
					JSONPath: ".status.availableReplicas",
				},
				{
					Name:     "Image",
					Type:     "string",
					JSONPath: ".spec.image",
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			schema := &extv1.JSONSchemaProps{Type: "object"}
			crd := newCRD(tt.group, tt.apiVersion, tt.kind, schema, tt.additionalPrinterColumns)

			assert.Equal(t, tt.expectedName, crd.Name)
			assert.Equal(t, tt.group, crd.Spec.Group)
			assert.Equal(t, tt.expectedKind, crd.Spec.Names.Kind)
			assert.Equal(t, tt.expectedKind+"List", crd.Spec.Names.ListKind)
			assert.Equal(t, tt.expectedPlural, crd.Spec.Names.Plural)
			assert.Equal(t, tt.expectedSingular, crd.Spec.Names.Singular)

			assert.Equal(t, extv1.NamespaceScoped, crd.Spec.Scope)

			require.Len(t, crd.Spec.Versions, 1)
			assert.Equal(t, tt.apiVersion, crd.Spec.Versions[0].Name)
			assert.Equal(t, schema, crd.Spec.Versions[0].Schema.OpenAPIV3Schema)
			assert.Equal(t, tt.expectedPrinterColumns, crd.Spec.Versions[0].AdditionalPrinterColumns)

			assert.Nil(t, crd.OwnerReferences)
		})
	}
}

func TestNewCRDSchema(t *testing.T) {
	tests := []struct {
		name                    string
		spec                    extv1.JSONSchemaProps
		status                  extv1.JSONSchemaProps
		statusFieldsOverride    bool
		expectedStateField      bool
		expectedConditionsField bool
	}{
		{
			name:                    "with override enabled and empty status",
			spec:                    extv1.JSONSchemaProps{Type: "object"},
			status:                  extv1.JSONSchemaProps{Type: "object"},
			statusFieldsOverride:    true,
			expectedStateField:      true,
			expectedConditionsField: true,
		},
		{
			name:                    "with override disabled",
			spec:                    extv1.JSONSchemaProps{Type: "object"},
			status:                  extv1.JSONSchemaProps{Type: "object"},
			statusFieldsOverride:    false,
			expectedStateField:      false,
			expectedConditionsField: false,
		},
		{
			name: "with existing status properties and override enabled",
			spec: extv1.JSONSchemaProps{Type: "object"},
			status: extv1.JSONSchemaProps{Type: "object", Properties: map[string]extv1.JSONSchemaProps{
				state: {
					Type:        "string",
					Description: "Custom state filed",
				},
				"customField": {Type: "string"},
			}},
			statusFieldsOverride:    true,
			expectedStateField:      true,
			expectedConditionsField: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			schema := newCRDSchema(tt.spec, tt.status, tt.statusFieldsOverride)

			assert.Equal(t, "object", schema.Type)
			require.NotNil(t, schema.Properties)

			assert.Contains(t, schema.Properties, "apiVersion")
			assert.Contains(t, schema.Properties, "kind")
			assert.Contains(t, schema.Properties, "metadata")
			assert.Contains(t, schema.Properties, "spec")
			assert.Contains(t, schema.Properties, "status")

			assert.Equal(t, tt.spec, schema.Properties["spec"])

			statusProps := schema.Properties["status"]
			require.NotNil(t, statusProps.Properties)

			if tt.expectedStateField {
				assert.Contains(t, statusProps.Properties, state)
				assert.Equal(t, defaultConditionsType, statusProps.Properties["conditions"])
			}
			if tt.status.Properties != nil {
				if customField, exists := tt.status.Properties["customField"]; exists {
					assert.Contains(t, statusProps.Properties, "customField")
					assert.Equal(t, customField, statusProps.Properties["customField"])
				}
			}

		})
	}
}

func TestNewCRDAdditionalPrinterColumns(t *testing.T) {
	tests := []struct {
		name     string
		policy   v1alpha1.AdditionalPrinterColumnPolicy
		userCols []extv1.CustomResourceColumnDefinition
		want     []extv1.CustomResourceColumnDefinition
	}{
		// Replace Policy Tests:
		{
			name:     "Replace policy with empty user columns returns defaults",
			policy:   v1alpha1.AdditionalPrinterColumnPolicyReplace,
			userCols: []extv1.CustomResourceColumnDefinition{},
			want:     defaultAdditionalPrinterColumns,
		},
		{
			name:   "Replace policy with user columns returns only user columns",
			policy: v1alpha1.AdditionalPrinterColumnPolicyReplace,
			userCols: []extv1.CustomResourceColumnDefinition{
				{Name: "Custom", JSONPath: ".spec.custom", Type: "string"},
			},
			want: []extv1.CustomResourceColumnDefinition{
				{Name: "Custom", JSONPath: ".spec.custom", Type: "string"},
			},
		},
		{
			name:     "Empty policy defaults to Replace behavior",
			policy:   "",
			userCols: []extv1.CustomResourceColumnDefinition{},
			want:     defaultAdditionalPrinterColumns,
		},
		// Add Policy Tests:
		{
			name:     "Add policy with empty user columns returns defaults",
			policy:   v1alpha1.AdditionalPrinterColumnPolicyAdd,
			userCols: []extv1.CustomResourceColumnDefinition{},
			want:     defaultAdditionalPrinterColumns,
		},
		{
			name:   "Add policy overrides default by name",
			policy: v1alpha1.AdditionalPrinterColumnPolicyAdd,
			userCols: []extv1.CustomResourceColumnDefinition{
				{Name: state, JSONPath: ".status.customState", Type: "string"}, // override default State
			},
			want: func() []extv1.CustomResourceColumnDefinition {
				// Create expected result with State overridden
				result := make([]extv1.CustomResourceColumnDefinition, len(defaultAdditionalPrinterColumns))
				copy(result, defaultAdditionalPrinterColumns)
				// Find and override State column
				for i, col := range result {
					if col.Name == state {
						result[i] = extv1.CustomResourceColumnDefinition{Name: state, JSONPath: ".status.customState", Type: "string"}
						break
					}
				}
				return result
			}(),
		},
		{
			name:   "Add policy appends new user column",
			policy: v1alpha1.AdditionalPrinterColumnPolicyAdd,
			userCols: []extv1.CustomResourceColumnDefinition{
				{Name: "CUSTOM", JSONPath: ".spec.custom", Type: "string"},
			},
			want: append(defaultAdditionalPrinterColumns,
				extv1.CustomResourceColumnDefinition{Name: "CUSTOM", JSONPath: ".spec.custom", Type: "string"}),
		},
		{
			name:   "Add policy with mixed override and append",
			policy: v1alpha1.AdditionalPrinterColumnPolicyAdd,
			userCols: []extv1.CustomResourceColumnDefinition{
				{Name: state, JSONPath: ".status.customState", Type: "string"}, // override
				{Name: "CUSTOM", JSONPath: ".spec.custom", Type: "string"},     // append
			},
			want: func() []extv1.CustomResourceColumnDefinition {
				result := make([]extv1.CustomResourceColumnDefinition, len(defaultAdditionalPrinterColumns))
				copy(result, defaultAdditionalPrinterColumns)
				// Find and override State column
				for i, col := range result {
					if col.Name == state {
						result[i] = extv1.CustomResourceColumnDefinition{Name: state, JSONPath: ".status.customState", Type: "string"}
						break
					}
				}
				// Append custom column
				result = append(result, extv1.CustomResourceColumnDefinition{Name: "CUSTOM", JSONPath: ".spec.custom", Type: "string"})
				return result
			}(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := newCRDAdditionalPrinterColumns(tt.policy, tt.userCols)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestSynthesizeCRDWithPolicy(t *testing.T) {
	spec := extv1.JSONSchemaProps{
		Type: "object",
		Properties: map[string]extv1.JSONSchemaProps{
			"field": {Type: "string"},
		},
	}
	status := extv1.JSONSchemaProps{
		Type: "object",
		Properties: map[string]extv1.JSONSchemaProps{
			state: {Type: "string"},
		},
	}

	tests := []struct {
		name                       string
		policy                     v1alpha1.AdditionalPrinterColumnPolicy
		additionalPrinterColumns   []extv1.CustomResourceColumnDefinition
		expectedColumnCount        int
		expectedCustomColumnExists bool
	}{
		{
			name:   "Replace policy with custom columns",
			policy: v1alpha1.AdditionalPrinterColumnPolicyReplace,
			additionalPrinterColumns: []extv1.CustomResourceColumnDefinition{
				{Name: "Custom", JSONPath: ".spec.field", Type: "string"},
			},
			expectedColumnCount:        1,
			expectedCustomColumnExists: true,
		},
		{
			name:                       "Replace policy with empty columns uses defaults",
			policy:                     v1alpha1.AdditionalPrinterColumnPolicyReplace,
			additionalPrinterColumns:   []extv1.CustomResourceColumnDefinition{},
			expectedColumnCount:        len(defaultAdditionalPrinterColumns),
			expectedCustomColumnExists: false,
		},
		{
			name:   "Add policy merges with defaults",
			policy: v1alpha1.AdditionalPrinterColumnPolicyAdd,
			additionalPrinterColumns: []extv1.CustomResourceColumnDefinition{
				{Name: "Custom", JSONPath: ".spec.field", Type: "string"},
			},
			expectedColumnCount:        len(defaultAdditionalPrinterColumns) + 1,
			expectedCustomColumnExists: true,
		},
		{
			name:   "Empty policy defaults to Replace",
			policy: "",
			additionalPrinterColumns: []extv1.CustomResourceColumnDefinition{
				{Name: "Custom", JSONPath: ".spec.field", Type: "string"},
			},
			expectedColumnCount:        1,
			expectedCustomColumnExists: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			crd := SynthesizeCRD("kro.com", "v1", "TestKind", spec, status, false, tt.policy, tt.additionalPrinterColumns)

			require.NotNil(t, crd)
			assert.Equal(t, "testkinds.kro.com", crd.Name)

			// Check printer columns
			version := crd.Spec.Versions[0]
			assert.Len(t, version.AdditionalPrinterColumns, tt.expectedColumnCount)

			if tt.expectedCustomColumnExists {
				found := false
				for _, col := range version.AdditionalPrinterColumns {
					if col.Name == "Custom" {
						found = true
						assert.Equal(t, ".spec.field", col.JSONPath)
						break
					}
				}
				assert.True(t, found, "Custom column should be present")
			}
		})
	}
}

func TestAdditionalPrinterColumnPolicyBackwardsCompatibility(t *testing.T) {
	spec := extv1.JSONSchemaProps{Type: "object"}
	status := extv1.JSONSchemaProps{Type: "object"}

	// Test that omitting policy behaves like Replace
	crd1 := SynthesizeCRD("kro.com", "v1", "TestKind", spec, status, false, "", nil)
	crd2 := SynthesizeCRD("kro.com", "v1", "TestKind", spec, status, false, v1alpha1.AdditionalPrinterColumnPolicyReplace, nil)

	version1 := crd1.Spec.Versions[0]
	version2 := crd2.Spec.Versions[0]

	assert.Equal(t, version1.AdditionalPrinterColumns, version2.AdditionalPrinterColumns, "Empty policy should behave like Replace")
	assert.Equal(t, defaultAdditionalPrinterColumns, version1.AdditionalPrinterColumns, "Should use default columns when policy is empty and no user columns provided")
}

func TestAdditionalPrinterColumnPolicyMergeOrder(t *testing.T) {
	userCols := []extv1.CustomResourceColumnDefinition{
		{Name: "First", JSONPath: ".spec.first", Type: "string"},
		{Name: state, JSONPath: ".status.overriddenState", Type: "string"}, // Override default
		{Name: "Second", JSONPath: ".spec.second", Type: "integer"},
	}

	result := newCRDAdditionalPrinterColumns(v1alpha1.AdditionalPrinterColumnPolicyAdd, userCols)

	// Check that State was overridden in place
	stateFound := false
	for _, col := range result {
		if col.Name == state {
			stateFound = true
			assert.Equal(t, ".status.overriddenState", col.JSONPath, "State column should be overridden")
			break
		}
	}
	assert.True(t, stateFound, "State column should be present")

	// Check that new columns are appended at the end
	lastCols := result[len(defaultAdditionalPrinterColumns):]
	expectedNewCols := []extv1.CustomResourceColumnDefinition{
		{Name: "First", JSONPath: ".spec.first", Type: "string"},
		{Name: "Second", JSONPath: ".spec.second", Type: "integer"},
	}
	assert.Equal(t, expectedNewCols, lastCols, "New columns should be appended in order")

	// Verify total length
	expectedLength := len(defaultAdditionalPrinterColumns) + 2 // 2 new columns (State was overridden, not added)
	assert.Len(t, result, expectedLength)
}
