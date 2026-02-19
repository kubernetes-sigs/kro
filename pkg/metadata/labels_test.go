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
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/release-utils/version"
)

// mockObject is a simple implementation of metav1.Object for testing
type mockObject struct {
	metav1.ObjectMeta
}

// GetObjectMeta returns the object interface for the mockObject..
func (m *mockObject) GetObjectMeta() metav1.Object {
	return m
}

func TestIsKROOwned(t *testing.T) {
	cases := []struct {
		name     string
		labels   map[string]string
		expected bool
	}{
		{
			name:     "owned by kro",
			labels:   map[string]string{OwnedLabel: "true"},
			expected: true,
		},
		{
			name:     "not owned by kro",
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
			result := IsKROOwned(&meta)
			assert.Equal(t, tc.expected, result)
		})
	}
}

func TestCompareRGDOwnership(t *testing.T) {
	cases := []struct {
		name              string
		existingLabels    map[string]string
		desiredLabels     map[string]string
		expectedKROOwned  bool
		expectedNameMatch bool
		expectedIDMatch   bool
	}{
		{
			name: "same RGD - normal update",
			existingLabels: map[string]string{
				OwnedLabel:                       "true",
				ResourceGraphDefinitionNameLabel: "test-rgd",
				ResourceGraphDefinitionIDLabel:   "test-id",
			},
			desiredLabels: map[string]string{
				OwnedLabel:                       "true",
				ResourceGraphDefinitionNameLabel: "test-rgd",
				ResourceGraphDefinitionIDLabel:   "test-id",
			},
			expectedKROOwned:  true,
			expectedNameMatch: true,
			expectedIDMatch:   true,
		},
		{
			name: "not owned by KRO",
			existingLabels: map[string]string{
				ResourceGraphDefinitionNameLabel: "test-rgd",
				ResourceGraphDefinitionIDLabel:   "test-id",
			},
			desiredLabels: map[string]string{
				OwnedLabel:                       "true",
				ResourceGraphDefinitionNameLabel: "test-rgd",
				ResourceGraphDefinitionIDLabel:   "test-id",
			},
			expectedKROOwned:  false,
			expectedNameMatch: false,
			expectedIDMatch:   false,
		},
		{
			name: "different RGD name - conflict",
			existingLabels: map[string]string{
				OwnedLabel:                       "true",
				ResourceGraphDefinitionNameLabel: "existing-rgd",
				ResourceGraphDefinitionIDLabel:   "test-id",
			},
			desiredLabels: map[string]string{
				OwnedLabel:                       "true",
				ResourceGraphDefinitionNameLabel: "new-rgd",
				ResourceGraphDefinitionIDLabel:   "test-id",
			},
			expectedKROOwned:  true,
			expectedNameMatch: false,
			expectedIDMatch:   true,
		},
		{
			name: "same RGD name, different ID - adoption",
			existingLabels: map[string]string{
				OwnedLabel:                       "true",
				ResourceGraphDefinitionNameLabel: "test-rgd",
				ResourceGraphDefinitionIDLabel:   "existing-id",
			},
			desiredLabels: map[string]string{
				OwnedLabel:                       "true",
				ResourceGraphDefinitionNameLabel: "test-rgd",
				ResourceGraphDefinitionIDLabel:   "new-id",
			},
			expectedKROOwned:  true,
			expectedNameMatch: true,
			expectedIDMatch:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			existing := metav1.ObjectMeta{Labels: tc.existingLabels}
			desired := metav1.ObjectMeta{Labels: tc.desiredLabels}

			kroOwned, nameMatch, idMatch := CompareRGDOwnership(existing, desired)

			assert.Equal(t, tc.expectedKROOwned, kroOwned)
			assert.Equal(t, tc.expectedNameMatch, nameMatch)
			assert.Equal(t, tc.expectedIDMatch, idMatch)
		})
	}
}

func TestGenericLabeler(t *testing.T) {
	t.Run("ApplyLabels", func(t *testing.T) {
		cases := []struct {
			name         string
			labeler      GenericLabeler
			objectLabels map[string]string
			expected     map[string]string
		}{
			{
				name:         "Apply labels to empty object",
				labeler:      GenericLabeler{"key1": "value1", "key2": "value2"},
				objectLabels: nil,
				expected:     map[string]string{"key1": "value1", "key2": "value2"},
			},
			{
				name:         "Apply labels to object with existing labels",
				labeler:      GenericLabeler{"key2": "newvalue2", "key3": "value3"},
				objectLabels: map[string]string{"key1": "value1", "key2": "value2"},
				expected:     map[string]string{"key1": "value1", "key2": "newvalue2", "key3": "value3"},
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				obj := &mockObject{ObjectMeta: metav1.ObjectMeta{Labels: tc.objectLabels}}
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

func TestNewResourceGraphDefinitionLabeler(t *testing.T) {
	t.Run("NewResourceGraphDefinitionLabeler", func(t *testing.T) {
		name := "rgd-name"
		uid := types.UID("rgd-uid")
		obj := &mockObject{ObjectMeta: metav1.ObjectMeta{Name: name, UID: uid}}
		labeler := NewResourceGraphDefinitionLabeler(obj)
		assert.Equal(t, GenericLabeler{
			ResourceGraphDefinitionNameLabel: name,
			ResourceGraphDefinitionIDLabel:   string(uid),
		}, labeler)
	})
}

func TestNewInstanceLabeler(t *testing.T) {
	t.Run("NewInstanceLabeler", func(t *testing.T) {
		name := "instance-name"
		namespace := "instance-namespace"
		uid := types.UID("instance-uid")
		group := "apps.example.com"
		version := "v1"
		kind := "MyApp"

		obj := &unstructured.Unstructured{}
		obj.SetName(name)
		obj.SetNamespace(namespace)
		obj.SetUID(uid)
		obj.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   group,
			Version: version,
			Kind:    kind,
		})

		labeler := NewInstanceLabeler(obj)
		assert.Equal(t, GenericLabeler{
			InstanceLabel:          name,
			InstanceNamespaceLabel: namespace,
			InstanceIDLabel:        string(uid),
			InstanceGroupLabel:     group,
			InstanceVersionLabel:   version,
			InstanceKindLabel:      kind,
		}, labeler)
	})
}

func TestNewKROMetaLabeler(t *testing.T) {
	t.Run("NewKROMetaLabeler", func(t *testing.T) {
		labeler := NewKROMetaLabeler()
		assert.Equal(t, GenericLabeler{
			OwnedLabel:      "true",
			KROVersionLabel: version.GetVersionInfo().GitVersion,
		}, labeler)
	})
}

func TestNewNodeLabeler(t *testing.T) {
	t.Run("NewNodeLabeler", func(t *testing.T) {
		labeler := NewNodeLabeler()
		assert.Equal(t, GenericLabeler{
			ManagedByLabelKey: ManagedByKROValue,
		}, labeler)
	})
}

func TestSafeNodeID(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "short ID",
			input:    "configMap",
			expected: "3ca56079224bf9f3",
		},
		{
			name:     "long ID exceeding 63 chars",
			input:    "thisIsAnExtremelyLongResourceIdentifierThatExceedsSixtyThreeCharactersLimit",
			expected: "f2e632b9df04d066",
		},
		{
			name:     "different short ID",
			input:    "secret",
			expected: "2bb80d537b1da3e3",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := SafeNodeID(tc.input)
			assert.Equal(t, tc.expected, result)
			assert.Len(t, result, 16)
		})
	}

	t.Run("deterministic", func(t *testing.T) {
		assert.Equal(t, SafeNodeID("configMap"), SafeNodeID("configMap"))
	})
}
