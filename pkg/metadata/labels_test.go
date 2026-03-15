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

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
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
			name:     "owned by kro (both labels)",
			labels:   map[string]string{OwnedLabel: "true", InternalOwnedLabel: "true"},
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
		{
			name: "same RGD - both label sets (dual-write)",
			existingLabels: map[string]string{
				OwnedLabel:                               "true",
				ResourceGraphDefinitionNameLabel:         "test-rgd",
				ResourceGraphDefinitionIDLabel:           "test-id",
				InternalOwnedLabel:                       "true",
				InternalResourceGraphDefinitionNameLabel: "test-rgd",
				InternalResourceGraphDefinitionIDLabel:   "test-id",
			},
			desiredLabels: map[string]string{
				OwnedLabel:                               "true",
				ResourceGraphDefinitionNameLabel:         "test-rgd",
				ResourceGraphDefinitionIDLabel:           "test-id",
				InternalOwnedLabel:                       "true",
				InternalResourceGraphDefinitionNameLabel: "test-rgd",
				InternalResourceGraphDefinitionIDLabel:   "test-id",
			},
			expectedKROOwned:  true,
			expectedNameMatch: true,
			expectedIDMatch:   true,
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

	t.Run("merge non-overlapping", func(t *testing.T) {
		l1 := GenericLabeler{"key1": "value1", "key2": "value2"}
		l2 := GenericLabeler{"key3": "value3", "key4": "value4"}
		merged := l1.merge(l2)
		assert.Equal(t, GenericLabeler{"key1": "value1", "key2": "value2", "key3": "value3", "key4": "value4"}, merged)
	})

	t.Run("merge with duplicate keys panics", func(t *testing.T) {
		l1 := GenericLabeler{"key1": "value1", "key2": "value2"}
		l2 := GenericLabeler{"key2": "value3", "key3": "value4"}
		assert.Panics(t, func() { l1.merge(l2) })
	})
}

func TestInstanceLabels(t *testing.T) {
	name := "rgd-name"
	uid := types.UID("rgd-uid")
	rgd := &v1alpha1.ResourceGraphDefinition{ObjectMeta: metav1.ObjectMeta{Name: name, UID: uid}}
	labeler := InstanceLabeler(rgd)

	v := version.GetVersionInfo().GitVersion

	assert.Equal(t, GenericLabeler{
		OwnedLabel:              "true",
		KROVersionLabel:         v,
		InternalOwnedLabel:      "true",
		InternalKROVersionLabel: v,

		ResourceGraphDefinitionIDLabel:           string(uid),
		ResourceGraphDefinitionNameLabel:         name,
		InternalResourceGraphDefinitionIDLabel:   string(uid),
		InternalResourceGraphDefinitionNameLabel: name,
	}, labeler)
}

func TestNodeLabels(t *testing.T) {
	instName := "instance-name"
	instNamespace := "instance-namespace"
	instUID := types.UID("instance-uid")
	group := "apps.example.com"
	instVersion := "v1"
	kind := "MyApp"

	instance := &unstructured.Unstructured{}
	instance.SetName(instName)
	instance.SetNamespace(instNamespace)
	instance.SetUID(instUID)
	instance.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   group,
		Version: instVersion,
		Kind:    kind,
	})

	v := version.GetVersionInfo().GitVersion

	t.Run("without collection", func(t *testing.T) {
		labeler := NodeLabeler(instance, "my-node", nil)
		assert.Equal(t, GenericLabeler{
			OwnedLabel:              "true",
			KROVersionLabel:         v,
			InternalOwnedLabel:      "true",
			InternalKROVersionLabel: v,

			ManagedByLabelKey: ManagedByKROValue,

			InstanceIDLabel:                string(instUID),
			InstanceLabel:                  instName,
			InstanceNamespaceLabel:         instNamespace,
			InstanceGroupLabel:             group,
			InstanceVersionLabel:           instVersion,
			InstanceKindLabel:              kind,
			InternalInstanceIDLabel:        string(instUID),
			InternalInstanceLabel:          instName,
			InternalInstanceNamespaceLabel: instNamespace,
			InternalInstanceGroupLabel:     group,
			InternalInstanceVersionLabel:   instVersion,
			InternalInstanceKindLabel:      kind,

			NodeIDLabel:         "my-node",
			InternalNodeIDLabel: "my-node",
		}, labeler)
	})

	t.Run("with collection", func(t *testing.T) {
		labeler := NodeLabeler(instance, "my-node", &CollectionInfo{Index: 2, Size: 5})

		assert.Equal(t, "my-node", labeler[NodeIDLabel])
		assert.Equal(t, "my-node", labeler[InternalNodeIDLabel])
		assert.Equal(t, "2", labeler[CollectionIndexLabel])
		assert.Equal(t, "5", labeler[CollectionSizeLabel])
		assert.Equal(t, "2", labeler[InternalCollectionIndexLabel])
		assert.Equal(t, "5", labeler[InternalCollectionSizeLabel])
		assert.Equal(t, string(instUID), labeler[InstanceIDLabel])
		assert.Equal(t, "true", labeler[OwnedLabel])
	})
}
