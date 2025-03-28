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

package crd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	"github.com/kro-run/kro/api/v1alpha1"
)

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
			crd := SynthesizeCRD(tt.group, tt.apiVersion, tt.kind, tt.spec, tt.status, tt.statusFieldsOverride)

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
		})
	}
}

func TestNewCRD(t *testing.T) {
	tests := []struct {
		name             string
		group            string
		apiVersion       string
		kind             string
		expectedName     string
		expectedKind     string
		expectedPlural   string
		expectedSingular string
	}{
		{
			name:             "basic example",
			group:            "kro.com",
			apiVersion:       "v1",
			kind:             "Test",
			expectedName:     "tests.kro.com",
			expectedKind:     "Test",
			expectedPlural:   "tests",
			expectedSingular: "test",
		},
		{
			name:             "uppercase kind",
			group:            "kro.com",
			apiVersion:       "v2beta1",
			kind:             "CONFIG",
			expectedName:     "configs.kro.com",
			expectedKind:     "CONFIG",
			expectedPlural:   "configs",
			expectedSingular: "config",
		},
		{
			name:             "mixed case kind",
			group:            "kro.com",
			apiVersion:       "v2beta1",
			kind:             "WebHook",
			expectedName:     "webhooks.kro.com",
			expectedKind:     "WebHook",
			expectedPlural:   "webhooks",
			expectedSingular: "webhook",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			schema := &extv1.JSONSchemaProps{Type: "object"}
			crd := newCRD(tt.group, tt.apiVersion, tt.kind, schema)

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
				"state": {
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
				assert.Contains(t, statusProps.Properties, "state")
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
