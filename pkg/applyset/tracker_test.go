// Copyright 2025 The Kube Resource Orchestrator Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package applyset

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestTracker_Add(t *testing.T) {
	tests := []struct {
		name        string
		objects     []ApplyableObject
		expectError bool
		expectedLen int
	}{
		{
			name: "add single object",
			objects: []ApplyableObject{
				{
					Unstructured: &unstructured.Unstructured{
						Object: map[string]interface{}{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]interface{}{
								"name":      "test-cm",
								"namespace": "default",
							},
						},
					},
					ID: "client-id-1",
				},
			},
			expectError: false,
			expectedLen: 1,
		},
		{
			name: "add two different objects",
			objects: []ApplyableObject{
				{
					Unstructured: &unstructured.Unstructured{
						Object: map[string]interface{}{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]interface{}{
								"name":      "test-cm-1",
								"namespace": "default",
							},
						},
					},
					ID: "client-id-1",
				},
				{
					Unstructured: &unstructured.Unstructured{
						Object: map[string]interface{}{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]interface{}{
								"name":      "test-cm-2",
								"namespace": "default",
							},
						},
					},
					ID: "client-id-2",
				},
			},
			expectError: false,
			expectedLen: 2,
		},
		{
			name: "add duplicate object by GVKNN",
			objects: []ApplyableObject{
				{
					Unstructured: &unstructured.Unstructured{
						Object: map[string]interface{}{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]interface{}{
								"name":      "test-cm",
								"namespace": "default",
							},
						},
					},
					ID: "client-id-1",
				},
				{
					Unstructured: &unstructured.Unstructured{
						Object: map[string]interface{}{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]interface{}{
								"name":      "test-cm",
								"namespace": "default",
							},
						},
					},
					ID: "client-id-2",
				},
			},
			expectError: true,
			expectedLen: 1,
		},
		{
			name: "add duplicate object by client ID",
			objects: []ApplyableObject{
				{
					Unstructured: &unstructured.Unstructured{
						Object: map[string]interface{}{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]interface{}{
								"name":      "test-cm-1",
								"namespace": "default",
							},
						},
					},
					ID: "client-id-1",
				},
				{
					Unstructured: &unstructured.Unstructured{
						Object: map[string]interface{}{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]interface{}{
								"name":      "test-cm-2",
								"namespace": "default",
							},
						},
					},
					ID: "client-id-1",
				},
			},
			expectError: true,
			expectedLen: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tracker := NewTracker()
			var err error
			for _, obj := range tt.objects {
				// GroupVersionKind is not set automatically on unstructured.Unstructured
				obj.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
				err = tracker.Add(obj)
				if err != nil {
					break
				}
			}

			if tt.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}

			assert.Len(t, tracker.objects, tt.expectedLen)
		})
	}
}
