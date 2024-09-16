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

package k8smetadata

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// mockObject is a simple implementation of metav1.Object for testing
type mockObject struct {
	metav1.ObjectMeta
}

// GetObjectMeta returns the object interface for the mockObject..
func (m *mockObject) GetObjectMeta() metav1.Object {
	return m
}

func TestIsSymphonyOwned(t *testing.T) {
	cases := []struct {
		name     string
		labels   map[string]string
		expected bool
	}{
		{
			name:     "owned by Symphony",
			labels:   map[string]string{OwnedLabel: "true"},
			expected: true,
		},
		{
			name:     "not owned by Symphony",
			labels:   map[string]string{OwnedLabel: "false"},
			expected: false,
		},
		{
			name:     "no ownership label",
			labels:   map[string]string{},
			expected: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			meta := metav1.ObjectMeta{Labels: tc.labels}
			result := IsSymphonyOwned(meta)
			assert.Equal(t, tc.expected, result)
		})
	}
}

func TestSetSymphonyOwned(t *testing.T) {
	cases := []struct {
		name          string
		initialLabels map[string]string
		expected      map[string]string
	}{
		{
			name:          "set owned on empty label",
			initialLabels: map[string]string{},
			expected:      map[string]string{OwnedLabel: "true"},
		},
		{
			name:          "override existing owned label",
			initialLabels: map[string]string{OwnedLabel: "false"},
			expected:      map[string]string{OwnedLabel: "true"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			meta := metav1.ObjectMeta{Labels: tc.initialLabels}
			SetSymphonyOwned(meta)
			assert.Equal(t, tc.expected, meta.Labels)
		})
	}
}

func TestSetSymphonyUnowned(t *testing.T) {
	cases := []struct {
		name          string
		initialLabels map[string]string
		expected      map[string]string
	}{
		{
			name:          "set unowned on empty label",
			initialLabels: map[string]string{},
			expected:      map[string]string{OwnedLabel: "false"},
		},
		{
			name:          "override existing owned label",
			initialLabels: map[string]string{OwnedLabel: "true"},
			expected:      map[string]string{OwnedLabel: "false"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			meta := metav1.ObjectMeta{Labels: tc.initialLabels}
			SetSymphonyUnowned(meta)
			assert.Equal(t, tc.expected, meta.Labels)
		})
	}
}

func TestGenericLabeler(t *testing.T) {
	t.Run("ApplyLabels", func(t *testing.T) {
		cases := []struct {
			name     string
			labeler  GenericLabeler
			expected map[string]string
		}{
			{
				name:     "Apply labels to empty object",
				labeler:  GenericLabeler{"key1": "value1", "key2": "value2"},
				expected: map[string]string{"key1": "value1", "key2": "value2"},
			},
			{
				name:     "Apply labels to object with existing labels",
				labeler:  GenericLabeler{"key2": "newvalue2", "key3": "value3"},
				expected: map[string]string{"key1": "value1", "key2": "newvalue2", "key3": "value3"},
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				obj := &mockObject{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"key1": "value1"}}}
				tc.labeler.ApplyLabels(obj)
				assert.Equal(t, tc.expected, obj.Labels)
			})
		}
	})

	t.Run("Merge", func(t *testing.T) {
		cases := []struct {
			name           string
			labeler1       GenericLabeler
			labeler2       GenericLabeler
			expectedMerged GenericLabeler
			expectError    bool
		}{
			{
				name:           "Merge non-overlapping labelers",
				labeler1:       GenericLabeler{"key1": "value1", "key2": "value2"},
				labeler2:       GenericLabeler{"key3": "value3", "key4": "value4"},
				expectedMerged: GenericLabeler{"key1": "value1", "key2": "value2", "key3": "value3", "key4": "value4"},
				expectError:    false,
			},
			{
				name:        "Merge with duplicate keys",
				labeler1:    GenericLabeler{"key1": "value1", "key2": "value2"},
				labeler2:    GenericLabeler{"key2": "value3", "key3": "value4"},
				expectError: true,
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				merged, err := tc.labeler1.Merge(tc.labeler2)
				if tc.expectError {
					assert.Error(t, err)
					assert.Contains(t, err.Error(), "duplicate labels")
				} else {
					assert.NoError(t, err)
					assert.Equal(t, tc.expectedMerged, merged)
				}
			})
		}
	})
}
