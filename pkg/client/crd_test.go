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

package client

import (
	"testing"

	"github.com/stretchr/testify/assert"
	v1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

func TestMergeVersions(t *testing.T) {
	tests := []struct {
		name     string
		newCRD   *v1.CustomResourceDefinition
		oldCRD   *v1.CustomResourceDefinition
		expected []v1.CustomResourceDefinitionVersion
	}{
		{
			name: "Preserve old version not in new CRD",
			newCRD: &v1.CustomResourceDefinition{
				Spec: v1.CustomResourceDefinitionSpec{
					Versions: []v1.CustomResourceDefinitionVersion{
						{Name: "v1beta1", Storage: true, Served: true},
					},
				},
			},
			oldCRD: &v1.CustomResourceDefinition{
				Spec: v1.CustomResourceDefinitionSpec{
					Versions: []v1.CustomResourceDefinitionVersion{
						{Name: "v1alpha1", Storage: true, Served: true},
					},
				},
			},
			expected: []v1.CustomResourceDefinitionVersion{
				{Name: "v1beta1", Storage: true, Served: true},
				{Name: "v1alpha1", Storage: false, Served: true},
			},
		},
		{
			name: "Do not duplicate existing version",
			newCRD: &v1.CustomResourceDefinition{
				Spec: v1.CustomResourceDefinitionSpec{
					Versions: []v1.CustomResourceDefinitionVersion{
						{Name: "v1alpha1", Storage: true, Served: true},
					},
				},
			},
			oldCRD: &v1.CustomResourceDefinition{
				Spec: v1.CustomResourceDefinitionSpec{
					Versions: []v1.CustomResourceDefinitionVersion{
						{Name: "v1alpha1", Storage: true, Served: true},
					},
				},
			},
			expected: []v1.CustomResourceDefinitionVersion{
				{Name: "v1alpha1", Storage: true, Served: true},
			},
		},
		{
			name: "Handle multiple old versions",
			newCRD: &v1.CustomResourceDefinition{
				Spec: v1.CustomResourceDefinitionSpec{
					Versions: []v1.CustomResourceDefinitionVersion{
						{Name: "v1", Storage: true, Served: true},
					},
				},
			},
			oldCRD: &v1.CustomResourceDefinition{
				Spec: v1.CustomResourceDefinitionSpec{
					Versions: []v1.CustomResourceDefinitionVersion{
						{Name: "v1beta1", Storage: false, Served: true},
						{Name: "v1alpha1", Storage: true, Served: true},
					},
				},
			},
			expected: []v1.CustomResourceDefinitionVersion{
				{Name: "v1", Storage: true, Served: true},
				{Name: "v1beta1", Storage: false, Served: true},
				{Name: "v1alpha1", Storage: false, Served: true},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wrapper := &CRDWrapper{}
			wrapper.mergeVersions(tt.newCRD, tt.oldCRD)

			assert.Equal(t, len(tt.expected), len(tt.newCRD.Spec.Versions))

			for _, expectedVer := range tt.expected {
				found := false
				for _, actualVer := range tt.newCRD.Spec.Versions {
					if actualVer.Name == expectedVer.Name {
						found = true
						assert.Equal(t, expectedVer.Storage, actualVer.Storage, "Storage flag mismatch for version %s", expectedVer.Name)
						assert.Equal(t, expectedVer.Served, actualVer.Served, "Served flag mismatch for version %s", expectedVer.Name)
						break
					}
				}
				assert.True(t, found, "Expected version %s not found in result", expectedVer.Name)
			}
		})
	}
}
