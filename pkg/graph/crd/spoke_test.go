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

package crd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	featuregatetesting "k8s.io/component-base/featuregate/testing"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/features"
)

func objectSchema(properties map[string]extv1.JSONSchemaProps) extv1.JSONSchemaProps {
	return extv1.JSONSchemaProps{Type: "object", Properties: properties}
}

// hubCRD synthesizes a single-version instance CRD to append spokes to.
func hubCRD(t *testing.T, apiVersion string) *extv1.CustomResourceDefinition {
	t.Helper()
	crd, err := SynthesizeCRD(
		"kro.run", apiVersion, "Widget",
		objectSchema(map[string]extv1.JSONSchemaProps{"name": {Type: "string"}}),
		extv1.JSONSchemaProps{},
		false,
		extv1.NamespaceScoped,
		&v1alpha1.Schema{},
	)
	require.NoError(t, err)
	return crd
}

func TestAppendSpokeVersion(t *testing.T) {
	served := false
	tests := []struct {
		name     string
		previous v1alpha1.PreviousSchemaVersion
		verify   func(t *testing.T, spoke extv1.CustomResourceDefinitionVersion)
	}{
		{
			name:     "defaults to served",
			previous: v1alpha1.PreviousSchemaVersion{APIVersion: "v1alpha1"},
			verify: func(t *testing.T, spoke extv1.CustomResourceDefinitionVersion) {
				assert.True(t, spoke.Served)
				assert.False(t, spoke.Deprecated)
				assert.Nil(t, spoke.DeprecationWarning)
			},
		},
		{
			name:     "honours served false",
			previous: v1alpha1.PreviousSchemaVersion{APIVersion: "v1alpha1", Served: &served},
			verify: func(t *testing.T, spoke extv1.CustomResourceDefinitionVersion) {
				assert.False(t, spoke.Served)
			},
		},
		{
			name: "deprecation warning is carried over",
			previous: v1alpha1.PreviousSchemaVersion{
				APIVersion:         "v1alpha1",
				Deprecated:         true,
				DeprecationWarning: "use v1beta1",
			},
			verify: func(t *testing.T, spoke extv1.CustomResourceDefinitionVersion) {
				assert.True(t, spoke.Deprecated)
				require.NotNil(t, spoke.DeprecationWarning)
				assert.Equal(t, "use v1beta1", *spoke.DeprecationWarning)
			},
		},
		{
			name: "deprecated without a warning leaves the pointer nil",
			previous: v1alpha1.PreviousSchemaVersion{
				APIVersion: "v1alpha1",
				Deprecated: true,
			},
			verify: func(t *testing.T, spoke extv1.CustomResourceDefinitionVersion) {
				assert.True(t, spoke.Deprecated)
				assert.Nil(t, spoke.DeprecationWarning)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			crd := hubCRD(t, "v1beta1")
			spec := objectSchema(map[string]extv1.JSONSchemaProps{"name": {Type: "string"}})

			require.NoError(t, AppendSpokeVersion(crd, tt.previous, spec, nil, &v1alpha1.Schema{}))

			require.Len(t, crd.Spec.Versions, 2)
			hub := crd.Spec.Versions[0]
			assert.Equal(t, "v1beta1", hub.Name)
			assert.True(t, hub.Storage)
			assert.True(t, hub.Served)

			spoke := crd.Spec.Versions[1]
			assert.Equal(t, tt.previous.APIVersion, spoke.Name)
			assert.False(t, spoke.Storage)
			require.NotNil(t, spoke.Subresources)
			assert.NotNil(t, spoke.Subresources.Status)
			assert.Equal(t, hub.AdditionalPrinterColumns, spoke.AdditionalPrinterColumns)

			require.NotNil(t, crd.Spec.Conversion)
			assert.Equal(t, extv1.NoneConverter, crd.Spec.Conversion.Strategy)

			tt.verify(t, spoke)
		})
	}
}

func TestAppendSpokeVersionRejectsDuplicates(t *testing.T) {
	crd := hubCRD(t, "v1beta1")
	spec := objectSchema(map[string]extv1.JSONSchemaProps{"name": {Type: "string"}})

	require.Error(t, AppendSpokeVersion(crd,
		v1alpha1.PreviousSchemaVersion{APIVersion: "v1beta1"}, spec, nil, &v1alpha1.Schema{}))
	assert.Len(t, crd.Spec.Versions, 1)

	require.NoError(t, AppendSpokeVersion(crd,
		v1alpha1.PreviousSchemaVersion{APIVersion: "v1alpha1"}, spec, nil, &v1alpha1.Schema{}))
	err := AppendSpokeVersion(crd,
		v1alpha1.PreviousSchemaVersion{APIVersion: "v1alpha1"}, spec, nil, &v1alpha1.Schema{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already present")
	assert.Len(t, crd.Spec.Versions, 2)
}

func TestSetCRDStatusFillsSpokePlaceholder(t *testing.T) {
	crd := hubCRD(t, "v1beta1")
	spec := objectSchema(map[string]extv1.JSONSchemaProps{"name": {Type: "string"}})
	require.NoError(t, AppendSpokeVersion(crd,
		v1alpha1.PreviousSchemaVersion{APIVersion: "v1alpha1"}, spec, nil, &v1alpha1.Schema{}))

	SetCRDStatus(crd, objectSchema(map[string]extv1.JSONSchemaProps{
		"endpoint": {Type: "string"},
	}), true)

	hubStatus := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["status"]
	spokeStatus := crd.Spec.Versions[1].Schema.OpenAPIV3Schema.Properties["status"]
	assert.Equal(t, hubStatus, spokeStatus)
	assert.Contains(t, spokeStatus.Properties, "endpoint")
	assert.Contains(t, spokeStatus.Properties, "state")
	assert.Contains(t, spokeStatus.Properties, "conditions")

	// The spoke holds a deep copy, not an alias of the hub status.
	hubStatus.Properties["mutated"] = extv1.JSONSchemaProps{Type: "string"}
	assert.NotContains(t, crd.Spec.Versions[1].Schema.OpenAPIV3Schema.Properties["status"].Properties, "mutated")
}

func TestSetCRDStatusKeepsDeclaredSpokeStatus(t *testing.T) {
	crd := hubCRD(t, "v1beta1")
	spec := objectSchema(map[string]extv1.JSONSchemaProps{"name": {Type: "string"}})
	declared := objectSchema(map[string]extv1.JSONSchemaProps{"phase": {Type: "string"}})
	require.NoError(t, AppendSpokeVersion(crd,
		v1alpha1.PreviousSchemaVersion{APIVersion: "v1alpha1"}, spec, &declared, &v1alpha1.Schema{}))

	SetCRDStatus(crd, objectSchema(map[string]extv1.JSONSchemaProps{
		"endpoint": {Type: "string"},
	}), true)

	spokeStatus := crd.Spec.Versions[1].Schema.OpenAPIV3Schema.Properties["status"]
	assert.Contains(t, spokeStatus.Properties, "phase")
	assert.NotContains(t, spokeStatus.Properties, "endpoint")
}

func TestSetCRDStatusGraphRevisionFields(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		name := "gate off"
		if enabled {
			name = "gate on"
		}
		t.Run(name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, features.FeatureGate, features.GraphRevisionPinning, enabled)

			crd := hubCRD(t, "v1beta1")
			spec := objectSchema(map[string]extv1.JSONSchemaProps{"name": {Type: "string"}})
			require.NoError(t, AppendSpokeVersion(crd,
				v1alpha1.PreviousSchemaVersion{APIVersion: "v1alpha1"}, spec, nil, &v1alpha1.Schema{}))

			SetCRDStatus(crd, objectSchema(map[string]extv1.JSONSchemaProps{
				"endpoint": {Type: "string"},
			}), true)

			for _, version := range crd.Spec.Versions {
				status := version.Schema.OpenAPIV3Schema.Properties["status"]
				if !enabled {
					assert.NotContains(t, status.Properties, "graphRevision", version.Name)
					assert.NotContains(t, status.Properties, "targetGraphRevision", version.Name)
					continue
				}
				for _, field := range []string{"graphRevision", "targetGraphRevision"} {
					props, ok := status.Properties[field]
					require.True(t, ok, "%s is missing %s", version.Name, field)
					assert.Equal(t, "integer", props.Type)
					assert.Equal(t, "int64", props.Format)
				}
			}
		})
	}
}
