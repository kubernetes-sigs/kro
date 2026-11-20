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

package generate

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// The graph builder returns the instance CRD without TypeMeta, so the instance
// has to take its group, version and kind from the CRD spec.
func TestDummyInstanceUsesTheCRDSpec(t *testing.T) {
	crd := &extv1.CustomResourceDefinition{
		Spec: extv1.CustomResourceDefinitionSpec{
			Group: "kro.run",
			Names: extv1.CustomResourceDefinitionNames{Kind: "TestApp", Plural: "testapps"},
			Versions: []extv1.CustomResourceDefinitionVersion{{
				Name: "v1alpha1",
				Schema: &extv1.CustomResourceValidation{OpenAPIV3Schema: &extv1.JSONSchemaProps{
					Type: "object",
					Properties: map[string]extv1.JSONSchemaProps{
						"spec": {Type: "object", Properties: map[string]extv1.JSONSchemaProps{
							"name": {Type: "string"},
						}},
					},
				}},
			}},
		},
	}

	obj, err := dummyInstance(crd)
	require.NoError(t, err)
	assert.Equal(t, "kro.run/v1alpha1", obj.GetAPIVersion())
	assert.Equal(t, "TestApp", obj.GetKind())
	assert.Equal(t, "testapp-sample", obj.GetName())
}
