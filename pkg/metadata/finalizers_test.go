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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestResourceGraphDefinitionFinalizer(t *testing.T) {
	cases := []struct {
		name          string
		initialObject metav1.Object
		operation     func(metav1.Object)
		check         func(metav1.Object) bool
		expected      bool
	}{
		{
			name:          "Set finaliser on empty object",
			initialObject: &metav1.ObjectMeta{},
			operation:     SetResourceGraphDefinitionFinalizer,
			check:         HasResourceGraphDefinitionFinalizer,
			expected:      true,
		},
		{
			name:          "Add finalizer to object w/ existing finalizers",
			initialObject: &metav1.ObjectMeta{Finalizers: []string{"some-other-finalizer"}},
			operation:     SetResourceGraphDefinitionFinalizer,
			check:         HasResourceGraphDefinitionFinalizer,
			expected:      true,
		},
		{
			name:          "Remove finalizer from object w/ finalizer",
			initialObject: &metav1.ObjectMeta{Finalizers: []string{kroFinalizer}},
			operation:     RemoveResourceGraphDefinitionFinalizer,
			check:         HasResourceGraphDefinitionFinalizer,
			expected:      false,
		},
		{
			name:          "Remove finalizer from object without finazlier",
			initialObject: &metav1.ObjectMeta{Finalizers: []string{"some-other-finalizer"}},
			operation:     RemoveResourceGraphDefinitionFinalizer,
			check:         HasResourceGraphDefinitionFinalizer,
			expected:      false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.operation(tc.initialObject)
			assert.Equal(t, tc.expected, tc.check(tc.initialObject))
		})
	}
}

func TestInstanceFinalizerUnstructured(t *testing.T) {
	cases := []struct {
		name          string
		initialObject *unstructured.Unstructured
		operation     func(*unstructured.Unstructured) error
		check         func(*unstructured.Unstructured) (bool, error)
		expected      bool
		expectError   bool
	}{
		{
			name: "Set instance finalizer on unstructred obj w/o finalizers",
			initialObject: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"metadata": map[string]interface{}{},
				},
			},
			operation: SetInstanceFinalizerUnstructured,
			check:     HasInstanceFinalizerUnstructured,
			expected:  true,
		},
		{
			name: "Set instance finalizer on unstructured obj w/ existing finalizers",
			initialObject: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"metadata": map[string]interface{}{
						"finalizers": []interface{}{"some-other-finalizer"},
					},
				},
			},
			operation: SetInstanceFinalizerUnstructured,
			check:     HasInstanceFinalizerUnstructured,
			expected:  true,
		},
		{
			name: "Remov instance finalizer from unstructured object that has it",
			initialObject: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"metadata": map[string]interface{}{
						"finalizers": []interface{}{kroFinalizer},
					},
				},
			},
			operation: RemoveInstanceFinalizerUnstructured,
			check:     HasInstanceFinalizerUnstructured,
			expected:  false,
		},
		{
			name: "Try to remve instance finalizer when its not there)",
			initialObject: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"metadata": map[string]interface{}{
						"finalizers": []interface{}{"some-other-finalizer"},
					},
				},
			},
			operation: RemoveInstanceFinalizerUnstructured,
			check:     HasInstanceFinalizerUnstructured,
			expected:  false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.operation(tc.initialObject)
			if tc.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				hasF, err := tc.check(tc.initialObject)
				assert.NoError(t, err)
				assert.Equal(t, tc.expected, hasF)
			}
		})
	}
}
