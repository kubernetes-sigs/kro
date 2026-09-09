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

package metadata

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
)

func TestExtractGVKFromUnstructured(t *testing.T) {
	cases := []struct {
		name         string
		unstructured map[string]any
		expectedGVK  schema.GroupVersionKind
		expectedErr  string
	}{
		{
			name: "Valid GVK with group",
			unstructured: map[string]any{
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
			},
			expectedGVK: schema.GroupVersionKind{
				Group:   "apps",
				Version: "v1",
				Kind:    "Deployment",
			},
		},
		{
			name: "Valid GVK without group",
			unstructured: map[string]any{
				"apiVersion": "v1",
				"kind":       "Pod",
			},
			expectedGVK: schema.GroupVersionKind{
				Group:   "",
				Version: "v1",
				Kind:    "Pod",
			},
		},
		{
			name: "Missing kind",
			unstructured: map[string]any{
				"apiVersion": "v1",
			},
			expectedErr: "kind not found or not a string",
		},
		{
			name: "Missing apiVersion",
			unstructured: map[string]any{
				"kind": "Pod",
			},
			expectedErr: "apiVersion not found or not a string",
		},
		{
			name: "Invalid apiVersion format - too many slashes",
			unstructured: map[string]any{
				"apiVersion": "apps/v1/beta",
				"kind":       "Deployment",
			},
			expectedErr: "invalid apiVersion format",
		},
		{
			name: "Invalid kind - not DNS-1035 label (contains underscore)",
			unstructured: map[string]any{
				"apiVersion": "v1",
				"kind":       "Invalid_Kind",
			},
			expectedErr: "invalid kind",
		},
		{
			name: "Invalid kind - not DNS-1035 label (starts with number)",
			unstructured: map[string]any{
				"apiVersion": "v1",
				"kind":       "123Kind",
			},
			expectedErr: "invalid kind",
		},
		{
			name: "Invalid kind - not DNS-1035 label (too long)",
			unstructured: map[string]any{
				"apiVersion": "v1",
				"kind":       strings.Repeat("a", 64), // DNS-1035 labels max length is 63
			},
			expectedErr: "invalid kind",
		},
		{
			name: "Non-string kind",
			unstructured: map[string]any{
				"apiVersion": "v1",
				"kind":       123,
			},
			expectedErr: "kind not found or not a string",
		},
		{
			name: "Non-string apiVersion",
			unstructured: map[string]any{
				"apiVersion": 123,
				"kind":       "Pod",
			},
			expectedErr: "apiVersion not found or not a string",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gvk, err := ExtractGVKFromUnstructured(tc.unstructured)

			if tc.expectedErr != "" {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tc.expectedErr)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tc.expectedGVK, gvk)
			}
		})
	}
}

func TestResolvePlural(t *testing.T) {
	cases := []struct {
		name   string
		kind   string
		plural string
		want   string
	}{
		{
			name: "derives from kind when no plural is declared",
			kind: "WebApplication",
			want: "webapplications",
		},
		{
			name: "english pluralization is wrong for -o kinds",
			kind: "PodInfo",
			want: "podinfoes",
		},
		{
			name:   "declared plural wins",
			kind:   "PodInfo",
			plural: "podinfos",
			want:   "podinfos",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ResolvePlural(tc.kind, tc.plural))
		})
	}
}

func TestGetResourceGraphDefinitionInstanceGVR(t *testing.T) {
	gvr := GetResourceGraphDefinitionInstanceGVR(&v1alpha1.Schema{
		Group:      "example.io",
		APIVersion: "v1alpha1",
		Kind:       "PodInfo",
		Plural:     "podinfos",
	})
	assert.Equal(t, schema.GroupVersionResource{
		Group:    "example.io",
		Version:  "v1alpha1",
		Resource: "podinfos",
	}, gvr)

	assert.Equal(t, schema.GroupVersionResource{}, GetResourceGraphDefinitionInstanceGVR(nil))
}
