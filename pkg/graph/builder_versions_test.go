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

package graph

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	featuregatetesting "k8s.io/component-base/featuregate/testing"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/features"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
)

// versionedRGD builds a minimal, otherwise valid RGD with the given hub
// version and previous versions.
func versionedRGD(apiVersion string, status map[string]any, previous ...krov1alpha1.PreviousSchemaVersion) *krov1alpha1.ResourceGraphDefinition {
	rgd := generator.NewResourceGraphDefinition("test-versions",
		generator.WithSchema("Widget", apiVersion,
			map[string]any{"name": "string"},
			status,
		),
		generator.WithResource("config", map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name": "${schema.spec.name}",
			},
		}, nil, nil),
	)
	rgd.Spec.Schema.PreviousVersions = previous
	return rgd
}

func previousVersion(apiVersion string, spec string, status string) krov1alpha1.PreviousSchemaVersion {
	previous := krov1alpha1.PreviousSchemaVersion{
		APIVersion: apiVersion,
		Spec:       runtime.RawExtension{Raw: []byte(spec)},
	}
	if status != "" {
		previous.Status = runtime.RawExtension{Raw: []byte(status)}
	}
	return previous
}

func TestGraphBuilderPreviousVersions(t *testing.T) {
	const specRaw = `{"name":"string"}`

	tests := []struct {
		name     string
		gateOn   bool
		rgd      func() *krov1alpha1.ResourceGraphDefinition
		errMsg   string
		validate func(t *testing.T, g *Graph)
	}{
		{
			name: "previous versions require the feature gate",
			rgd: func() *krov1alpha1.ResourceGraphDefinition {
				return versionedRGD("v1beta1", nil, previousVersion("v1alpha1", specRaw, ""))
			},
			errMsg: "schema.previousVersions requires the MultiVersionSchema feature gate to be enabled",
		},
		{
			name:   "previous version equal to the hub is rejected",
			gateOn: true,
			rgd: func() *krov1alpha1.ResourceGraphDefinition {
				return versionedRGD("v1beta1", nil, previousVersion("v1beta1", specRaw, ""))
			},
			errMsg: "must differ from schema.apiVersion",
		},
		{
			name:   "previous version outranking the hub is rejected",
			gateOn: true,
			rgd: func() *krov1alpha1.ResourceGraphDefinition {
				return versionedRGD("v1alpha1", nil, previousVersion("v1beta1", specRaw, ""))
			},
			errMsg: "must have lower priority than schema.apiVersion v1alpha1",
		},
		{
			name:   "duplicate previous versions are rejected",
			gateOn: true,
			rgd: func() *krov1alpha1.ResourceGraphDefinition {
				return versionedRGD("v1beta1", nil,
					previousVersion("v1alpha1", specRaw, ""),
					previousVersion("v1alpha1", specRaw, ""),
				)
			},
			errMsg: "is declared more than once",
		},
		{
			name:   "previous version status with an expression is rejected",
			gateOn: true,
			rgd: func() *krov1alpha1.ResourceGraphDefinition {
				return versionedRGD("v1beta1", nil,
					previousVersion("v1alpha1", specRaw, `{"name":"${config.metadata.name}"}`))
			},
			errMsg: "must declare types, not expressions",
		},
		{
			name:   "valid previous version yields a two-version CRD",
			gateOn: true,
			rgd: func() *krov1alpha1.ResourceGraphDefinition {
				return versionedRGD("v1beta1", nil,
					previousVersion("v1alpha1", `{"name":"string"}`, `{"phase":"string"}`))
			},
			validate: func(t *testing.T, g *Graph) {
				require.NotNil(t, g.CRD)
				require.Len(t, g.CRD.Spec.Versions, 2)
				assert.Equal(t, "v1beta1", g.CRD.Spec.Versions[0].Name)
				assert.True(t, g.CRD.Spec.Versions[0].Storage)
				assert.Equal(t, "v1alpha1", g.CRD.Spec.Versions[1].Name)
				assert.False(t, g.CRD.Spec.Versions[1].Storage)
				assert.True(t, g.CRD.Spec.Versions[1].Served)

				spokeSpec := g.CRD.Spec.Versions[1].Schema.OpenAPIV3Schema.Properties["spec"]
				assert.Contains(t, spokeSpec.Properties, "name")
				spokeStatus := g.CRD.Spec.Versions[1].Schema.OpenAPIV3Schema.Properties["status"]
				assert.Contains(t, spokeStatus.Properties, "phase")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, features.FeatureGate, features.MultiVersionSchema, tt.gateOn)

			g, err := newUnitTestBuilder().NewResourceGraphDefinition(tt.rgd(), defaultRGDConfig)

			if tt.errMsg != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.errMsg)
				return
			}
			require.NoError(t, err)
			tt.validate(t, g)
		})
	}
}

func TestGraphBuilderReservedStatusFields(t *testing.T) {
	tests := []struct {
		name   string
		gateOn bool
		field  string
		errMsg string
	}{
		{
			name:   "graphRevision is reserved with the gate on",
			gateOn: true,
			field:  "graphRevision",
			errMsg: `schema.status field "graphRevision" is reserved by kro`,
		},
		{
			name:   "targetGraphRevision is reserved with the gate on",
			gateOn: true,
			field:  "targetGraphRevision",
			errMsg: `schema.status field "targetGraphRevision" is reserved by kro`,
		},
		{
			name:   "graphRevision is allowed with the gate off",
			gateOn: false,
			field:  "graphRevision",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, features.FeatureGate, features.GraphRevisionPinning, tt.gateOn)

			rgd := versionedRGD("v1alpha1", map[string]any{
				tt.field: "${config.metadata.name}",
			})

			g, err := newUnitTestBuilder().NewResourceGraphDefinition(rgd, defaultRGDConfig)

			if tt.errMsg != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.errMsg)
				return
			}
			require.NoError(t, err)
			status := g.CRD.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["status"]
			assert.Contains(t, status.Properties, tt.field)
		})
	}
}
