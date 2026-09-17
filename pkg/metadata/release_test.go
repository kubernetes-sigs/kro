// Copyright 2026 The Kubernetes Authors.
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

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	applysetspec "github.com/kubernetes-sigs/kro/pkg/applyset"
)

func TestDeletionPolicyOf(t *testing.T) {
	cases := []struct {
		name        string
		annotations map[string]string
		want        v1alpha1.DeletionPolicy
	}{
		{
			name:        "detach",
			annotations: map[string]string{DeletionPolicyAnnotation: "Detach"},
			want:        v1alpha1.DeletionPolicyDetach,
		},
		{
			name:        "explicit delete",
			annotations: map[string]string{DeletionPolicyAnnotation: "Delete"},
			want:        v1alpha1.DeletionPolicyDelete,
		},
		{
			name:        "absent annotation defaults to delete",
			annotations: map[string]string{"other": "value"},
			want:        v1alpha1.DeletionPolicyDelete,
		},
		{
			// A typo must not make a resource undeletable.
			name:        "unrecognised value defaults to delete",
			annotations: map[string]string{DeletionPolicyAnnotation: "detach"},
			want:        v1alpha1.DeletionPolicyDelete,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			obj := &mockObject{ObjectMeta: metav1.ObjectMeta{Annotations: tc.annotations}}
			assert.Equal(t, tc.want, DeletionPolicyOf(obj))
		})
	}
}

func TestDeletionPolicyOfNilObject(t *testing.T) {
	assert.Equal(t, v1alpha1.DeletionPolicyDelete, DeletionPolicyOf(nil))
}

func TestReleaseKROMetadata(t *testing.T) {
	obj := &mockObject{ObjectMeta: metav1.ObjectMeta{
		Labels: map[string]string{
			applysetspec.ApplysetPartOfLabel: "applyset-abc-v1",
			OwnedLabel:                       "true",
			KROVersionLabel:                  "v1.0.0",
			NodeIDLabel:                      "deployment",
			CollectionIndexLabel:             "0",
			InstanceIDLabel:                  "uid",
			InstanceNamespaceLabel:           "default",
			ManagedByLabelKey:                ManagedByKROValue,
			"app":                            "web",
		},
		Annotations: map[string]string{
			ApplyOrderAnnotation:     "2",
			NodePathAnnotation:       "deployment",
			DeletionPolicyAnnotation: "Detach",
			"team":                   "platform",
		},
	}}

	assert.True(t, ReleaseKROMetadata(obj))
	assert.Equal(t, map[string]string{"app": "web"}, obj.GetLabels())
	assert.Equal(t, map[string]string{"team": "platform"}, obj.GetAnnotations())
}

func TestReleaseKROMetadataKeepsForeignManagedBy(t *testing.T) {
	obj := &mockObject{ObjectMeta: metav1.ObjectMeta{
		Labels: map[string]string{
			ManagedByLabelKey: "helm",
			OwnedLabel:        "true",
		},
	}}

	assert.True(t, ReleaseKROMetadata(obj))
	assert.Equal(t, map[string]string{ManagedByLabelKey: "helm"}, obj.GetLabels())
}

func TestReleaseKROMetadataClearsEmptiedMaps(t *testing.T) {
	obj := &mockObject{ObjectMeta: metav1.ObjectMeta{
		Labels:      map[string]string{OwnedLabel: "true"},
		Annotations: map[string]string{NodePathAnnotation: "deployment"},
	}}

	assert.True(t, ReleaseKROMetadata(obj))
	assert.Nil(t, obj.GetLabels())
	assert.Nil(t, obj.GetAnnotations())
}

func TestReleaseKROMetadataNoKROKeys(t *testing.T) {
	obj := &mockObject{ObjectMeta: metav1.ObjectMeta{
		Labels: map[string]string{"app": "web"},
	}}

	assert.False(t, ReleaseKROMetadata(obj))
	assert.Equal(t, map[string]string{"app": "web"}, obj.GetLabels())
}

func TestReleaseKROMetadataNilObject(t *testing.T) {
	assert.False(t, ReleaseKROMetadata(nil))
}
