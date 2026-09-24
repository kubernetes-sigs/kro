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

package rgdadapter

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apimachineryruntime "k8s.io/apimachinery/pkg/runtime"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
)

// The deletion policy has to reach the managed object as an annotation: the
// prune and teardown paths rediscover their candidates from live cluster state
// and never re-read the RGD, so an annotation that failed to land means the
// resource is deleted despite being declared Detach.
func TestResourceGraphDefinitionToGraph_DeletionPolicyAnnotation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		policy   v1alpha1.DeletionPolicy
		template string
		wantMeta map[string]any
	}{
		{
			name:     "detach annotates a template without metadata annotations",
			policy:   v1alpha1.DeletionPolicyDetach,
			template: `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"cm"}}`,
			wantMeta: map[string]any{
				"name": "cm",
				"annotations": map[string]any{
					metadata.DeletionPolicyAnnotation: "Detach",
				},
			},
		},
		{
			name:     "detach merges into the author's annotations",
			policy:   v1alpha1.DeletionPolicyDetach,
			template: `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"cm","annotations":{"team":"${schema.spec.team}"}}}`,
			wantMeta: map[string]any{
				"name": "cm",
				"annotations": map[string]any{
					"team":                            "${schema.spec.team}",
					metadata.DeletionPolicyAnnotation: "Detach",
				},
			},
		},
		{
			// Delete is the default, so it is the ABSENCE of the annotation:
			// existing objects stay byte-identical and flipping back to Delete
			// lets server-side apply prune the annotation.
			name:     "explicit delete leaves the template untouched",
			policy:   v1alpha1.DeletionPolicyDelete,
			template: `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"cm"}}`,
			wantMeta: map[string]any{"name": "cm"},
		},
		{
			name:     "unset policy leaves the template untouched",
			template: `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"cm"}}`,
			wantMeta: map[string]any{"name": "cm"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			g, err := ResourceGraphDefinitionToGraph(rgdWithResource(&v1alpha1.Resource{
				ID:             "cm",
				DeletionPolicy: tt.policy,
				Template:       apimachineryruntime.RawExtension{Raw: []byte(tt.template)},
			}))
			require.NoError(t, err)
			require.Len(t, g.Spec.Nodes, 1)

			var manifest map[string]any
			require.NoError(t, json.Unmarshal(g.Spec.Nodes[0].Template.Raw, &manifest))
			assert.Equal(t, tt.wantMeta, manifest["metadata"])
			assert.Equal(t, "ConfigMap", manifest["kind"])
		})
	}
}

// The annotation is injected into the template, so a metadata stanza that is a
// CEL expression rather than a literal map has nowhere to put it. Failing loudly
// beats overwriting the author's expression or silently dropping the policy,
// which would delete a resource the author asked to keep.
func TestResourceGraphDefinitionToGraph_DeletionPolicyUnannotatableTemplate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		template string
		wantErr  string
	}{
		{
			name:     "metadata is an expression",
			template: `{"apiVersion":"v1","kind":"ConfigMap","metadata":"${schema.spec.meta}"}`,
			wantErr:  "metadata is not a map",
		},
		{
			name:     "metadata.annotations is an expression",
			template: `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"cm","annotations":"${schema.spec.anns}"}}`,
			wantErr:  "metadata.annotations is not a map",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := ResourceGraphDefinitionToGraph(rgdWithResource(&v1alpha1.Resource{
				ID:             "cm",
				DeletionPolicy: v1alpha1.DeletionPolicyDetach,
				Template:       apimachineryruntime.RawExtension{Raw: []byte(tt.template)},
			}))
			require.Error(t, err)
			assert.True(t, errors.Is(err, ErrUnsupported), "want ErrUnsupported, got %v", err)
			assert.Contains(t, err.Error(), tt.wantErr)
			assert.Contains(t, err.Error(), `resource "cm"`)
		})
	}
}

// A template with no metadata at all still has to carry the policy, so the
// stanza is created rather than skipped. Such a manifest is rejected later by
// the compiler (a template needs a name), but the adapter must not be the
// place that quietly loses the policy.
func TestResourceGraphDefinitionToGraph_DeletionPolicyAddsMissingMetadata(t *testing.T) {
	t.Parallel()

	g, err := ResourceGraphDefinitionToGraph(rgdWithResource(&v1alpha1.Resource{
		ID:             "cm",
		DeletionPolicy: v1alpha1.DeletionPolicyDetach,
		Template:       apimachineryruntime.RawExtension{Raw: []byte(`{"apiVersion":"v1","kind":"ConfigMap"}`)},
	}))
	require.NoError(t, err)
	require.Len(t, g.Spec.Nodes, 1)

	var manifest map[string]any
	require.NoError(t, json.Unmarshal(g.Spec.Nodes[0].Template.Raw, &manifest))
	assert.Equal(t, map[string]any{
		"annotations": map[string]any{metadata.DeletionPolicyAnnotation: "Detach"},
	}, manifest["metadata"])
}

func rgdWithResource(res *v1alpha1.Resource) *v1alpha1.ResourceGraphDefinition {
	return &v1alpha1.ResourceGraphDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "webapp"},
		Spec:       v1alpha1.ResourceGraphDefinitionSpec{Resources: []*v1alpha1.Resource{res}},
	}
}
