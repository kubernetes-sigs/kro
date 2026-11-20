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

package defaults

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
)

// The API server fills in these defaults when an RGD is created. The CLI reads
// RGD files directly, so without them generate crd named the CRD "<plural>."
// with an empty group.
func TestApplyRGDDefaults(t *testing.T) {
	tests := []struct {
		name   string
		schema *v1alpha1.Schema
		want   *v1alpha1.Schema
	}{
		{
			name:   "group and scope unset",
			schema: &v1alpha1.Schema{APIVersion: "v1alpha1", Kind: "TestApp"},
			want: &v1alpha1.Schema{
				APIVersion: "v1alpha1", Kind: "TestApp",
				Group: v1alpha1.KRODomainName, Scope: v1alpha1.ResourceScopeNamespaced,
			},
		},
		{
			name: "group and scope set",
			schema: &v1alpha1.Schema{
				APIVersion: "v1alpha1", Kind: "TestApp",
				Group: "example.com", Scope: v1alpha1.ResourceScopeCluster,
			},
			want: &v1alpha1.Schema{
				APIVersion: "v1alpha1", Kind: "TestApp",
				Group: "example.com", Scope: v1alpha1.ResourceScopeCluster,
			},
		},
		{
			name: "no schema",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rgd := &v1alpha1.ResourceGraphDefinition{
				Spec: v1alpha1.ResourceGraphDefinitionSpec{Schema: tt.schema},
			}
			ApplyRGDDefaults(rgd)
			assert.Equal(t, tt.want, rgd.Spec.Schema)
		})
	}
}
