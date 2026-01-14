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

package runtime

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestValidateUniqueIdentities(t *testing.T) {
	tests := []struct {
		name       string
		objs       []*unstructured.Unstructured
		namespaced bool
		wantErr    bool
	}{
		{
			name:       "empty slice",
			objs:       nil,
			namespaced: true,
			wantErr:    false,
		},
		{
			name: "unique identities",
			objs: []*unstructured.Unstructured{
				newUnstructured("apps/v1", "Deployment", "ns", "deploy-a"),
				newUnstructured("apps/v1", "Deployment", "ns", "deploy-b"),
			},
			namespaced: true,
			wantErr:    false,
		},
		{
			name: "duplicate identities",
			objs: []*unstructured.Unstructured{
				newUnstructured("apps/v1", "Deployment", "ns", "deploy-a"),
				newUnstructured("apps/v1", "Deployment", "ns", "deploy-a"),
			},
			namespaced: true,
			wantErr:    true,
		},
		{
			name: "same name different namespace for namespaced resource",
			objs: []*unstructured.Unstructured{
				newUnstructured("apps/v1", "Deployment", "ns1", "deploy"),
				newUnstructured("apps/v1", "Deployment", "ns2", "deploy"),
			},
			namespaced: true,
			wantErr:    false, // different namespaces = different resources
		},
		{
			name: "same name different kind",
			objs: []*unstructured.Unstructured{
				newUnstructured("apps/v1", "Deployment", "ns", "foo"),
				newUnstructured("v1", "Service", "ns", "foo"),
			},
			namespaced: true,
			wantErr:    false,
		},
		{
			name: "nil objects are skipped",
			objs: []*unstructured.Unstructured{
				newUnstructured("apps/v1", "Deployment", "ns", "deploy"),
				nil,
				newUnstructured("apps/v1", "Deployment", "ns", "deploy2"),
			},
			namespaced: true,
			wantErr:    false,
		},
		{
			name: "cluster-scoped resources with different namespaces should collide",
			objs: []*unstructured.Unstructured{
				newUnstructured("rbac.authorization.k8s.io/v1", "ClusterRole", "", "admin"),
				newUnstructured("rbac.authorization.k8s.io/v1", "ClusterRole", "ignored", "admin"),
			},
			namespaced: false, // cluster-scoped
			wantErr:    true,  // same name = collision
		},
		{
			name: "cluster-scoped resources with same name collide",
			objs: []*unstructured.Unstructured{
				newUnstructured("rbac.authorization.k8s.io/v1", "ClusterRole", "", "admin"),
				newUnstructured("rbac.authorization.k8s.io/v1", "ClusterRole", "", "admin"),
			},
			namespaced: false,
			wantErr:    true,
		},
		{
			name: "cluster-scoped resources with different names are unique",
			objs: []*unstructured.Unstructured{
				newUnstructured("rbac.authorization.k8s.io/v1", "ClusterRole", "", "admin"),
				newUnstructured("rbac.authorization.k8s.io/v1", "ClusterRole", "", "viewer"),
			},
			namespaced: false,
			wantErr:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateUniqueIdentities(tt.objs, tt.namespaced)
			if tt.wantErr {
				require.Error(t, err, "expected error for duplicate identity")
				assert.Contains(t, err.Error(), "duplicate identity")
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestOrderedCollectionObserved(t *testing.T) {
	tests := []struct {
		name       string
		observed   []*unstructured.Unstructured
		desired    []*unstructured.Unstructured
		namespaced bool
		want       []string // expected names in order
	}{
		{
			name:       "empty observed",
			observed:   nil,
			desired:    []*unstructured.Unstructured{newUnstructured("v1", "Pod", "ns", "a")},
			namespaced: true,
			want:       nil,
		},
		{
			name:       "empty desired",
			observed:   []*unstructured.Unstructured{newUnstructured("v1", "Pod", "ns", "a")},
			desired:    nil,
			namespaced: true,
			want:       []string{"a"},
		},
		{
			name: "reorders to match desired",
			observed: []*unstructured.Unstructured{
				newUnstructured("v1", "Pod", "ns", "c"),
				newUnstructured("v1", "Pod", "ns", "a"),
				newUnstructured("v1", "Pod", "ns", "b"),
			},
			desired: []*unstructured.Unstructured{
				newUnstructured("v1", "Pod", "ns", "a"),
				newUnstructured("v1", "Pod", "ns", "b"),
				newUnstructured("v1", "Pod", "ns", "c"),
			},
			namespaced: true,
			want:       []string{"a", "b", "c"},
		},
		{
			name: "orphans excluded",
			observed: []*unstructured.Unstructured{
				newUnstructured("v1", "Pod", "ns", "a"),
				newUnstructured("v1", "Pod", "ns", "orphan"),
				newUnstructured("v1", "Pod", "ns", "b"),
			},
			desired: []*unstructured.Unstructured{
				newUnstructured("v1", "Pod", "ns", "a"),
				newUnstructured("v1", "Pod", "ns", "b"),
			},
			namespaced: true,
			want:       []string{"a", "b"},
		},
		{
			name: "missing observed items create gaps",
			observed: []*unstructured.Unstructured{
				newUnstructured("v1", "Pod", "ns", "c"),
			},
			desired: []*unstructured.Unstructured{
				newUnstructured("v1", "Pod", "ns", "a"),
				newUnstructured("v1", "Pod", "ns", "b"),
				newUnstructured("v1", "Pod", "ns", "c"),
			},
			namespaced: true,
			want:       []string{"c"}, // only c is present
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := orderedCollectionObserved(tt.observed, tt.desired, tt.namespaced)

			if tt.want == nil {
				// For empty observed, result should equal observed
				if tt.observed == nil {
					assert.Nil(t, result)
				}
				return
			}

			require.Len(t, result, len(tt.want))
			for i, name := range tt.want {
				assert.Equal(t, name, result[i].GetName())
			}
		})
	}
}

func TestCartesianProduct(t *testing.T) {
	tests := []struct {
		name       string
		dimensions []evaluatedDimension
		want       []map[string]any
	}{
		{
			name:       "empty dimensions",
			dimensions: nil,
			want:       nil,
		},
		{
			name: "single dimension",
			dimensions: []evaluatedDimension{
				{name: "region", values: []any{"us-east-1", "us-west-2"}},
			},
			want: []map[string]any{
				{"region": "us-east-1"},
				{"region": "us-west-2"},
			},
		},
		{
			name: "two dimensions",
			dimensions: []evaluatedDimension{
				{name: "region", values: []any{"us-east", "us-west"}},
				{name: "az", values: []any{"a", "b"}},
			},
			want: []map[string]any{
				{"region": "us-east", "az": "a"},
				{"region": "us-east", "az": "b"},
				{"region": "us-west", "az": "a"},
				{"region": "us-west", "az": "b"},
			},
		},
		{
			name: "three dimensions with scalar values",
			dimensions: []evaluatedDimension{
				{name: "x", values: []any{1, 2}},
				{name: "y", values: []any{3}},
				{name: "z", values: []any{4, 5}},
			},
			want: []map[string]any{
				{"x": 1, "y": 3, "z": 4},
				{"x": 1, "y": 3, "z": 5},
				{"x": 2, "y": 3, "z": 4},
				{"x": 2, "y": 3, "z": 5},
			},
		},
		{
			name: "list values are preserved not flattened",
			dimensions: []evaluatedDimension{
				{name: "config", values: []any{[]int{1, 2}, []int{3, 4}}},
			},
			want: []map[string]any{
				{"config": []int{1, 2}},
				{"config": []int{3, 4}},
			},
		},
		{
			name: "empty dimension values returns nil",
			dimensions: []evaluatedDimension{
				{name: "x", values: []any{"a"}},
				{name: "y", values: []any{}}, // empty
			},
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := cartesianProduct(tt.dimensions)
			assert.Equal(t, tt.want, result)
		})
	}
}

func newUnstructured(apiVersion, kind, namespace, name string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion(apiVersion)
	obj.SetKind(kind)
	obj.SetNamespace(namespace)
	obj.SetName(name)
	return obj
}
