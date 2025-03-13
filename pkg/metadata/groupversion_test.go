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

package metadata

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestExtractGVKFromUnstructured(t *testing.T) {
	cases := []struct {
		name         string
		unstructured map[string]interface{}
		expectedGVK  schema.GroupVersionKind
		expectedErr  string
	}{
		{
			name: "Valid GVK with group",
			unstructured: map[string]interface{}{
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
			unstructured: map[string]interface{}{
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
			unstructured: map[string]interface{}{
				"apiVersion": "v1",
			},
			expectedErr: "kind not found or not a string",
		},
		{
			name: "Missing apiVersion",
			unstructured: map[string]interface{}{
				"kind": "Pod",
			},
			expectedErr: "apiVersion not found or not a string",
		},
		{
			name: "Invalid apiVersion format",
			unstructured: map[string]interface{}{
				"apiVersion": "apps/v1/beta",
				"kind":       "Deployment",
			},
			expectedErr: "invalid apiVersion format: apps/v1/beta",
		},
		{
			name: "Non-string kind",
			unstructured: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       123,
			},
			expectedErr: "kind not found or not a string",
		},
		{
			name: "Non-string apiVersion",
			unstructured: map[string]interface{}{
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
