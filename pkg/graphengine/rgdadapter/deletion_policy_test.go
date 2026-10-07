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
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	apimachineryruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/restmapper"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/features"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/compiler"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
	testk8s "github.com/kubernetes-sigs/kro/pkg/testutil/k8s"
)

func TestMain(m *testing.M) {
	// Enable DeletionPolicy by default for all rgdadapter tests.
	// Tests that verify gate-off behavior disable it locally.
	_ = features.FeatureGate.Set("DeletionPolicy=true")
	os.Exit(m.Run())
}

// With the gate off the field is rejected outright and a template that does
// not use it is passed through byte for byte. Not parallel: it flips the gate.
func TestResourceGraphDefinitionToGraph_DeletionPolicyGateDisabled(t *testing.T) {
	require.NoError(t, features.FeatureGate.Set("DeletionPolicy=false"))
	t.Cleanup(func() {
		require.NoError(t, features.FeatureGate.Set("DeletionPolicy=true"))
	})

	template := `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"cm","annotations":"${schema.spec.anns}"}}`

	for _, policy := range []v1alpha1.DeletionPolicy{v1alpha1.DeletionPolicyOrphaned, v1alpha1.DeletionPolicyDelete} {
		_, err := ResourceGraphDefinitionToGraph(rgdWithResource(&v1alpha1.Resource{
			ID:             "cm",
			DeletionPolicy: policy,
			Template:       apimachineryruntime.RawExtension{Raw: []byte(template)},
		}))
		require.ErrorIs(t, err, ErrUnsupported)
		assert.Contains(t, err.Error(), "DeletionPolicy feature gate")
	}

	g, err := ResourceGraphDefinitionToGraph(rgdWithResource(&v1alpha1.Resource{
		ID:       "cm",
		Template: apimachineryruntime.RawExtension{Raw: []byte(template)},
	}))
	require.NoError(t, err)
	assert.Equal(t, template, string(g.Spec.Nodes[0].Template.Raw))
}

// The deletion policy has to reach the managed object as an annotation: the
// prune and teardown paths rediscover their candidates from live cluster state
// and never re-read the RGD, so an annotation that failed to land means the
// resource is deleted despite being declared Orphaned.
func TestResourceGraphDefinitionToGraph_DeletionPolicyAnnotation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		policy   v1alpha1.DeletionPolicy
		template string
		wantMeta map[string]any
	}{
		{
			name:     "orphaned annotates a template without metadata annotations",
			policy:   v1alpha1.DeletionPolicyOrphaned,
			template: `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"cm"}}`,
			wantMeta: map[string]any{
				"name": "cm",
				"annotations": map[string]any{
					metadata.DeletionPolicyAnnotation: "Orphaned",
				},
			},
		},
		{
			name:     "orphaned merges into the author's annotations",
			policy:   v1alpha1.DeletionPolicyOrphaned,
			template: `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"cm","annotations":{"team":"${schema.spec.team}"}}}`,
			wantMeta: map[string]any{
				"name": "cm",
				"annotations": map[string]any{
					"team":                            "${schema.spec.team}",
					metadata.DeletionPolicyAnnotation: "Orphaned",
				},
			},
		},
		{
			// Builder validation rejects this key in templates; the adapter
			// still lets the declared policy win if one slips through.
			name:     "declared policy wins over a template value",
			policy:   v1alpha1.DeletionPolicyOrphaned,
			template: `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"cm","annotations":{"internal.kro.run/deletion-policy":"Delete"}}}`,
			wantMeta: map[string]any{
				"name": "cm",
				"annotations": map[string]any{
					metadata.DeletionPolicyAnnotation: "Orphaned",
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
			name:     "metadata.annotations is not a standalone expression",
			template: `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"cm","annotations":"x-${schema.spec.anns}"}}`,
			wantErr:  "single ${...} expression",
		},
		{
			name:     "metadata.annotations is neither a map nor a string",
			template: `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"cm","annotations":["a"]}}`,
			wantErr:  "metadata.annotations is not a map",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := ResourceGraphDefinitionToGraph(rgdWithResource(&v1alpha1.Resource{
				ID:             "cm",
				DeletionPolicy: v1alpha1.DeletionPolicyOrphaned,
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
		DeletionPolicy: v1alpha1.DeletionPolicyOrphaned,
		Template:       apimachineryruntime.RawExtension{Raw: []byte(`{"apiVersion":"v1","kind":"ConfigMap"}`)},
	}))
	require.NoError(t, err)
	require.Len(t, g.Spec.Nodes, 1)

	var manifest map[string]any
	require.NoError(t, json.Unmarshal(g.Spec.Nodes[0].Template.Raw, &manifest))
	assert.Equal(t, map[string]any{
		"annotations": map[string]any{metadata.DeletionPolicyAnnotation: "Orphaned"},
	}, manifest["metadata"])
}

// An annotations map built by a single CEL expression cannot be edited
// statically, so the declared policy is enforced at resolve time: merged in
// when Orphaned, filtered out otherwise so an instance cannot smuggle it in.
func TestBuildRuntimeForInstance_DeletionPolicyInAnnotationsExpression(t *testing.T) {
	tests := []struct {
		name   string
		policy v1alpha1.DeletionPolicy
		anns   map[string]any
		want   map[string]string
	}{
		{
			name:   "orphaned wins over the expression's value",
			policy: v1alpha1.DeletionPolicyOrphaned,
			anns: map[string]any{
				"team":                            "platform",
				metadata.DeletionPolicyAnnotation: "Delete",
			},
			want: map[string]string{
				"team":                            "platform",
				metadata.DeletionPolicyAnnotation: "Orphaned",
			},
		},
		{
			name:   "delete drops a policy supplied by the expression",
			policy: v1alpha1.DeletionPolicyDelete,
			anns: map[string]any{
				"team":                            "platform",
				metadata.DeletionPolicyAnnotation: "Orphaned",
			},
			want: map[string]string{"team": "platform"},
		},
		{
			name: "unset drops a policy supplied by the expression",
			anns: map[string]any{
				"team":                            "platform",
				metadata.DeletionPolicyAnnotation: "Orphaned",
			},
			want: map[string]string{"team": "platform"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rgd := &v1alpha1.ResourceGraphDefinition{
				ObjectMeta: metav1.ObjectMeta{Name: "webapp"},
				Spec: v1alpha1.ResourceGraphDefinitionSpec{
					Schema: &v1alpha1.Schema{
						APIVersion: "v1alpha1",
						Kind:       "WebApp",
						Spec:       apimachineryruntime.RawExtension{Raw: []byte(`{"anns":"map[string]string"}`)},
					},
					Resources: []*v1alpha1.Resource{{
						ID:             "cm",
						DeletionPolicy: tt.policy,
						Template: rawResource(map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name":        "cm",
								"namespace":   "default",
								"annotations": "${schema.spec.anns}",
							},
						}),
					}},
				},
			}
			instance := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "kro.run/v1alpha1",
				"kind":       "WebApp",
				"metadata":   map[string]any{"name": "demo", "namespace": "default"},
				"spec":       map[string]any{"anns": tt.anns},
			}}

			fakeResolver, disco := testk8s.NewFakeResolver()
			rm := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(disco))
			rt, _, err := BuildRuntimeForInstance(rgd, instance, compiler.NewCompilerWithDependencies(fakeResolver, rm))
			require.NoError(t, err)

			schemaObjs, err := rt.Node(SchemaNodeID).Resolve()
			require.NoError(t, err)
			rt.Set(SchemaNodeID, schemaObjs[0].Object)

			objs, err := rt.Node("cm").Resolve()
			require.NoError(t, err)
			require.Len(t, objs, 1)
			assert.Equal(t, tt.want, objs[0].GetAnnotations())
		})
	}
}

func rgdWithResource(res *v1alpha1.Resource) *v1alpha1.ResourceGraphDefinition {
	return &v1alpha1.ResourceGraphDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "webapp"},
		Spec:       v1alpha1.ResourceGraphDefinitionSpec{Resources: []*v1alpha1.Resource{res}},
	}
}
