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
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
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

func TestGraphRevisionFinalizer(t *testing.T) {
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
			operation:     SetGraphRevisionFinalizer,
			check:         HasGraphRevisionFinalizer,
			expected:      true,
		},
		{
			name:          "Add finalizer to object w/ existing finalizers",
			initialObject: &metav1.ObjectMeta{Finalizers: []string{"some-other-finalizer"}},
			operation:     SetGraphRevisionFinalizer,
			check:         HasGraphRevisionFinalizer,
			expected:      true,
		},
		{
			name:          "Remove finalizer from object w/ finalizer",
			initialObject: &metav1.ObjectMeta{Finalizers: []string{kroFinalizer}},
			operation:     RemoveGraphRevisionFinalizer,
			check:         HasGraphRevisionFinalizer,
			expected:      false,
		},
		{
			name:          "Remove finalizer from object without finazlier",
			initialObject: &metav1.ObjectMeta{Finalizers: []string{"some-other-finalizer"}},
			operation:     RemoveGraphRevisionFinalizer,
			check:         HasGraphRevisionFinalizer,
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
		initialObject client.Object
		operation     func(object client.Object)
		check         func(object client.Object) bool
		expected      bool
		expectError   bool
	}{
		{
			name: "Set instance finalizer on unstructured obj w/o finalizers",
			initialObject: &unstructured.Unstructured{
				Object: map[string]any{
					"metadata": map[string]any{},
				},
			},
			operation: SetInstanceFinalizer,
			check:     HasInstanceFinalizer,
			expected:  true,
		},
		{
			name: "Set instance finalizer on unstructured obj w/ existing finalizers",
			initialObject: &unstructured.Unstructured{
				Object: map[string]any{
					"metadata": map[string]any{
						"finalizers": []any{"some-other-finalizer"},
					},
				},
			},
			operation: SetInstanceFinalizer,
			check:     HasInstanceFinalizer,
			expected:  true,
		},
		{
			name: "Remov instance finalizer from unstructured object that has it",
			initialObject: &unstructured.Unstructured{
				Object: map[string]any{
					"metadata": map[string]any{
						"finalizers": []any{kroFinalizer},
					},
				},
			},
			operation: RemoveInstanceFinalizer,
			check:     HasInstanceFinalizer,
			expected:  false,
		},
		{
			name: "Try to remve instance finalizer when its not there)",
			initialObject: &unstructured.Unstructured{
				Object: map[string]any{
					"metadata": map[string]any{
						"finalizers": []any{"some-other-finalizer"},
					},
				},
			},
			operation: RemoveInstanceFinalizer,
			check:     HasInstanceFinalizer,
			expected:  false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.operation(tc.initialObject)
			hasF := tc.check(tc.initialObject)
			assert.Equal(t, tc.expected, hasF)
		})
	}
}

func TestGraphFinalizer(t *testing.T) {
	cases := []struct {
		name             string
		initial          []string
		op               func(obj metav1.Object)
		wantFinalizers   []string
		wantHasFinalizer bool
	}{
		{
			name:             "Has returns false on empty list",
			initial:          nil,
			op:               func(metav1.Object) {},
			wantFinalizers:   nil,
			wantHasFinalizer: false,
		},
		{
			name:             "Set adds when absent",
			initial:          nil,
			op:               SetGraphFinalizer,
			wantFinalizers:   []string{GraphFinalizer},
			wantHasFinalizer: true,
		},
		{
			name:             "Set is a no-op when already present",
			initial:          []string{GraphFinalizer},
			op:               SetGraphFinalizer,
			wantFinalizers:   []string{GraphFinalizer},
			wantHasFinalizer: true,
		},
		{
			name:             "Set preserves unrelated finalizers",
			initial:          []string{"other/finalizer"},
			op:               SetGraphFinalizer,
			wantFinalizers:   []string{"other/finalizer", GraphFinalizer},
			wantHasFinalizer: true,
		},
		{
			name:             "Remove drops the finalizer",
			initial:          []string{GraphFinalizer},
			op:               RemoveGraphFinalizer,
			wantFinalizers:   []string{},
			wantHasFinalizer: false,
		},
		{
			name:             "Remove preserves unrelated finalizers",
			initial:          []string{"first", GraphFinalizer, "third"},
			op:               RemoveGraphFinalizer,
			wantFinalizers:   []string{"first", "third"},
			wantHasFinalizer: false,
		},
		{
			name:             "Remove is a no-op when absent",
			initial:          []string{"other"},
			op:               RemoveGraphFinalizer,
			wantFinalizers:   []string{"other"},
			wantHasFinalizer: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			obj := &metav1.ObjectMeta{Finalizers: append([]string(nil), tc.initial...)}
			tc.op(obj)
			assert.Equal(t, tc.wantFinalizers, obj.Finalizers)
			assert.Equal(t, tc.wantHasFinalizer, HasGraphFinalizer(obj))
		})
	}
}
