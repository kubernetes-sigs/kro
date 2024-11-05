// Copyright Amazon.com Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//	http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package metadata

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

func TestResourceGroupFinalizer(t *testing.T) {
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
			operation:     SetResourceGroupFinalizer,
			check:         HasResourceGroupFinalizer,
			expected:      true,
		},
		{
			name:          "Add finalizer to object w/ existing finalizers",
			initialObject: &metav1.ObjectMeta{Finalizers: []string{"some-other-finalizer"}},
			operation:     SetResourceGroupFinalizer,
			check:         HasResourceGroupFinalizer,
			expected:      true,
		},
		{
			name:          "Remove finalizer from object w/ finalizer",
			initialObject: &metav1.ObjectMeta{Finalizers: []string{symphonyFinalizer}},
			operation:     RemoveResourceGroupFinalizer,
			check:         HasResourceGroupFinalizer,
			expected:      false,
		},
		{
			name:          "Remove finalizer from object without finazlier",
			initialObject: &metav1.ObjectMeta{Finalizers: []string{"some-other-finalizer"}},
			operation:     RemoveResourceGroupFinalizer,
			check:         HasResourceGroupFinalizer,
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

func TestInstanceFinalizer(t *testing.T) {
	uid := types.UID("test-uid")
	cases := []struct {
		name          string
		initialObject metav1.Object
		operation     func(metav1.Object, types.UID)
		check         func(metav1.Object, types.UID) bool
		expected      bool
	}{
		{
			name:          "Set instance finalizer on objet w/o finalizers",
			initialObject: &metav1.ObjectMeta{},
			operation:     SetInstanceFinalizer,
			check:         HasInstanceFinalizer,
			expected:      true,
		},
		{
			name:          "Set instanc finalizer on object w/ existing finalizers",
			initialObject: &metav1.ObjectMeta{Finalizers: []string{"some-other-finalizer"}},
			operation:     SetInstanceFinalizer,
			check:         HasInstanceFinalizer,
			expected:      true,
		},
		{
			name:          "Remove instance finalizer from object that has it",
			initialObject: &metav1.ObjectMeta{Finalizers: []string{getInstanceFinalizerName(uid)}},
			operation:     RemoveInstanceFinalizer,
			check:         HasInstanceFinalizer,
			expected:      false,
		},
		{
			name:          "Try to remove instance finalizer when its not present",
			initialObject: &metav1.ObjectMeta{Finalizers: []string{"some-other-finalizer"}},
			operation:     RemoveInstanceFinalizer,
			check:         HasInstanceFinalizer,
			expected:      false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.operation(tc.initialObject, uid)
			assert.Equal(t, tc.expected, tc.check(tc.initialObject, uid))
		})
	}
}

func TestInstanceFinalizerUnstructured(t *testing.T) {
	uid := types.UID("test-uid")
	cases := []struct {
		name          string
		initialObject *unstructured.Unstructured
		operation     func(*unstructured.Unstructured, types.UID) error
		check         func(*unstructured.Unstructured, types.UID) (bool, error)
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
						"finalizers": []interface{}{getInstanceFinalizerName(uid)},
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
			err := tc.operation(tc.initialObject, uid)
			if tc.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				hasF, err := tc.check(tc.initialObject, uid)
				assert.NoError(t, err)
				assert.Equal(t, tc.expected, hasF)
			}
		})
	}
}
