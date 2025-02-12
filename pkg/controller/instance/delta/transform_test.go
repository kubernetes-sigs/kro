// Copyright 2025 The Kube Resource Orchestrator Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package delta

import (
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestSecretTransformer_CanTransform(t *testing.T) {
	transformer := &SecretTransformer{}

	tests := []struct {
		name     string
		obj      *unstructured.Unstructured
		expected bool
	}{
		{
			name: "secret object",
			obj: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "Secret",
				},
			},
			expected: true,
		},
		{
			name: "non-secret object",
			obj: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "ConfigMap",
				},
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := transformer.CanTransform(tt.obj)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestSecretTransformer_Transform(t *testing.T) {
	transformer := &SecretTransformer{}

	tests := []struct {
		name        string
		input       *unstructured.Unstructured
		expected    *unstructured.Unstructured
		expectError bool
	}{
		{
			name: "stringData only",
			input: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "Secret",
					"stringData": map[string]interface{}{
						"key1": "value1",
						"key2": "value2",
					},
				},
			},
			expected: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "Secret",
					"data": map[string]interface{}{
						"key1": base64.StdEncoding.EncodeToString([]byte("value1")),
						"key2": base64.StdEncoding.EncodeToString([]byte("value2")),
					},
				},
			},
			expectError: false,
		},
		{
			name: "stringData and data",
			input: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "Secret",
					"data": map[string]interface{}{
						"key1": base64.StdEncoding.EncodeToString([]byte("existing1")),
						"key3": base64.StdEncoding.EncodeToString([]byte("value3")),
					},
					"stringData": map[string]interface{}{
						"key1": "value1", // Should override existing key1
						"key2": "value2", // New key
					},
				},
			},
			expected: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "Secret",
					"data": map[string]interface{}{
						"key1": base64.StdEncoding.EncodeToString([]byte("value1")),
						"key2": base64.StdEncoding.EncodeToString([]byte("value2")),
						"key3": base64.StdEncoding.EncodeToString([]byte("value3")),
					},
				},
			},
			expectError: false,
		},
		{
			name: "no stringData",
			input: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "Secret",
					"data": map[string]interface{}{
						"key1": base64.StdEncoding.EncodeToString([]byte("value1")),
					},
				},
			},
			expected: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "Secret",
					"data": map[string]interface{}{
						"key1": base64.StdEncoding.EncodeToString([]byte("value1")),
					},
				},
			},
			expectError: false,
		},
		{
			name: "invalid stringData value",
			input: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "Secret",
					"stringData": map[string]interface{}{
						"key1": int64(123), // Not a string
					},
				},
			},
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := transformer.Transform(tt.input)
			if tt.expectError {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, tt.expected.Object, tt.input.Object)
		})
	}
}

func TestCompare_SecretTransformation(t *testing.T) {
	desired := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Secret",
			"metadata": map[string]interface{}{
				"name": "test-secret",
			},
			"stringData": map[string]interface{}{
				"username": "admin",
				"password": "secret123",
			},
		},
	}

	observed := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Secret",
			"metadata": map[string]interface{}{
				"name": "test-secret",
			},
			"data": map[string]interface{}{
				"username": base64.StdEncoding.EncodeToString([]byte("admin")),
				"password": base64.StdEncoding.EncodeToString([]byte("secret123")),
			},
		},
	}

	differences, err := Compare(desired, observed)
	assert.NoError(t, err)
	assert.Empty(t, differences, "Expected no differences after transformation")
}
