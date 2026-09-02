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
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	apimachineryruntime "k8s.io/apimachinery/pkg/runtime"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
)

// The RGD → Graph translation is the whole RGD compatibility surface: every
// accepted RGD has to become an equivalent Graph, and every shape with no
// equivalent has to be rejected by name rather than silently dropped. A
// resource that vanished in translation would be deleted from the cluster on
// the next prune.
func TestResourceGraphDefinitionToGraph_Rejections(t *testing.T) {
	t.Parallel()

	rgdWith := func(resources ...*v1alpha1.Resource) *v1alpha1.ResourceGraphDefinition {
		return &v1alpha1.ResourceGraphDefinition{
			ObjectMeta: metav1.ObjectMeta{Name: "webapp"},
			Spec:       v1alpha1.ResourceGraphDefinitionSpec{Resources: resources},
		}
	}
	template := func() apimachineryruntime.RawExtension {
		return apimachineryruntime.RawExtension{
			Raw: []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"cm"}}`),
		}
	}

	tests := []struct {
		name        string
		rgd         *v1alpha1.ResourceGraphDefinition
		wantErr     string
		unsupported bool
	}{
		{
			name:    "a nil RGD is rejected",
			rgd:     nil,
			wantErr: "resourcegraphdefinition is required",
		},
		{
			name:    "a nil resource entry is rejected with its index",
			rgd:     rgdWith(nil),
			wantErr: "resource[0]",
		},
		{
			name: "template and externalRef together are rejected",
			rgd: rgdWith(&v1alpha1.Resource{
				ID:       "both",
				Template: template(),
				ExternalRef: &v1alpha1.ExternalRef{
					APIVersion: "v1", Kind: "ConfigMap",
					Metadata: v1alpha1.ExternalRefMetadata{Name: "cm"},
				},
			}),
			wantErr:     "both",
			unsupported: true,
		},
		{
			name:        "neither template nor externalRef is rejected",
			rgd:         rgdWith(&v1alpha1.Resource{ID: "empty"}),
			wantErr:     "empty",
			unsupported: true,
		},
		{
			name: "a named externalRef without a name is rejected",
			rgd: rgdWith(&v1alpha1.Resource{
				ID: "noname",
				ExternalRef: &v1alpha1.ExternalRef{
					APIVersion: "v1", Kind: "ConfigMap",
				},
			}),
			wantErr:     "metadata.name",
			unsupported: true,
		},
		{
			name: "forEach on an externalRef is rejected rather than ignored",
			rgd: rgdWith(&v1alpha1.Resource{
				ID: "fanout",
				ExternalRef: &v1alpha1.ExternalRef{
					APIVersion: "v1", Kind: "ConfigMap",
					Metadata: v1alpha1.ExternalRefMetadata{Name: "cm"},
				},
				ForEach: []v1alpha1.ForEachDimension{{"n": "${schema.spec.names}"}},
			}),
			wantErr:     "forEach",
			unsupported: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := ResourceGraphDefinitionToGraph(tt.rgd)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
			if tt.unsupported {
				assert.True(t, errors.Is(err, ErrUnsupported),
					"an unmappable RGD shape must be reported as ErrUnsupported, got %v", err)
			}
		})
	}
}

// Everything the RGD declares per-resource has to arrive on the Graph node
// unchanged, and the translated Graph must not alias the RGD's memory — the
// RGD spec comes from the revision registry and is shared across every
// instance reconcile of that revision.
func TestResourceGraphDefinitionToGraph_CarriesFieldsThrough(t *testing.T) {
	t.Parallel()

	rgd := &v1alpha1.ResourceGraphDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "webapp"},
		Spec: v1alpha1.ResourceGraphDefinitionSpec{Resources: []*v1alpha1.Resource{
			{
				ID: "cm",
				Template: apimachineryruntime.RawExtension{
					Raw: []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"${schema.spec.name}"}}`),
				},
				ReadyWhen:   []string{"${cm.metadata.name != ''}"},
				IncludeWhen: []string{"${schema.spec.enabled}"},
				ForEach:     []v1alpha1.ForEachDimension{{"n": "${schema.spec.names}"}},
			},
			{
				ID: "vpc",
				ExternalRef: &v1alpha1.ExternalRef{
					APIVersion: "v1", Kind: "ConfigMap",
					Metadata: v1alpha1.ExternalRefMetadata{Name: "existing", Namespace: "infra"},
				},
				ReadyWhen: []string{"${vpc.metadata.name != ''}"},
			},
		}},
	}

	g, err := ResourceGraphDefinitionToGraph(rgd)
	require.NoError(t, err)

	assert.Equal(t, "webapp", g.Name)
	assert.Equal(t, "Graph", g.GroupVersionKind().Kind,
		"the translated object must identify as a Graph")
	require.Len(t, g.Spec.Nodes, 2)

	cm := g.Spec.Nodes[0]
	assert.Equal(t, "cm", cm.ID, "node order must follow the RGD's resource order")
	require.NotNil(t, cm.Template)
	assert.Contains(t, string(cm.Template.Raw), "${schema.spec.name}",
		"CEL must be carried through unevaluated")
	assert.Equal(t, []string{"${cm.metadata.name != ''}"}, cm.ReadyWhen)
	assert.Equal(t, []string{"${schema.spec.enabled}"}, cm.IncludeWhen)
	require.Len(t, cm.ForEach, 1)
	assert.Equal(t, "${schema.spec.names}", cm.ForEach[0]["n"])
	assert.Nil(t, cm.Ref, "a template resource must not produce a ref node")

	vpc := g.Spec.Nodes[1]
	assert.Equal(t, "vpc", vpc.ID)
	require.NotNil(t, vpc.Ref)
	assert.Equal(t, "existing", vpc.Ref.Metadata.Name)
	assert.Equal(t, "infra", vpc.Ref.Metadata.Namespace)
	assert.Nil(t, vpc.Template, "an externalRef resource must not produce a template node")

	t.Run("the translated Graph does not alias the RGD", func(t *testing.T) {
		rgd.Spec.Resources[0].Template.Raw[0] = 'X'
		rgd.Spec.Resources[0].ReadyWhen[0] = "mutated"
		rgd.Spec.Resources[0].ForEach[0]["n"] = "mutated"
		rgd.Spec.Resources[1].ExternalRef.Metadata.Name = "mutated"

		assert.NotEqual(t, byte('X'), cm.Template.Raw[0], "template bytes must be copied")
		assert.Equal(t, "${cm.metadata.name != ''}", cm.ReadyWhen[0], "readyWhen must be copied")
		assert.Equal(t, "${schema.spec.names}", cm.ForEach[0]["n"], "forEach must be deep-copied")
		assert.Equal(t, "existing", vpc.Ref.Metadata.Name, "externalRef must be deep-copied")
	})
}

// A selector externalRef is a read-only collection, so it is exempt from the
// metadata.name requirement that applies to a single named reference.
func TestResourceGraphDefinitionToGraph_SelectorCollectionNeedsNoName(t *testing.T) {
	t.Parallel()

	g, err := ResourceGraphDefinitionToGraph(&v1alpha1.ResourceGraphDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "webapp"},
		Spec: v1alpha1.ResourceGraphDefinitionSpec{Resources: []*v1alpha1.Resource{{
			ID: "pods",
			ExternalRef: &v1alpha1.ExternalRef{
				APIVersion: "v1", Kind: "ConfigMap",
				Metadata: v1alpha1.ExternalRefMetadata{
					Selector: apimachineryruntime.RawExtension{
						Raw: []byte(`{"matchLabels":{"tier":"db"}}`),
					},
				},
			},
		}}},
	})
	require.NoError(t, err)
	require.Len(t, g.Spec.Nodes, 1)
	require.NotNil(t, g.Spec.Nodes[0].Ref)
	require.True(t, g.Spec.Nodes[0].Ref.Metadata.HasSelector())
	assert.JSONEq(t, `{"matchLabels":{"tier":"db"}}`, string(g.Spec.Nodes[0].Ref.Metadata.Selector.Raw))
}

// A zero-resource RGD is valid: it manages no children and only projects
// status from its schema. The prepended `schema` def node keeps the compiled
// Graph non-empty so the MinItems=1 node constraint still holds.
func TestResourceGraphDefinitionToGraph_ZeroResourcesIsValid(t *testing.T) {
	t.Parallel()

	g, err := ResourceGraphDefinitionToGraph(&v1alpha1.ResourceGraphDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "noop"},
	})
	require.NoError(t, err)
	assert.Empty(t, g.Spec.Nodes)
	assert.Equal(t, "noop", g.Name)
}

// InstanceSchemaNode materialises the instance as a `def` node named `schema`
// so RGD-style ${schema.spec.*} references resolve as ordinary node lookups.
// The full metadata subtree has to be exposed, not just name/namespace, or
// RGDs referencing ${schema.metadata.uid} break.
func TestInstanceSchemaNode(t *testing.T) {
	t.Parallel()

	t.Run("a nil instance is rejected", func(t *testing.T) {
		t.Parallel()
		_, err := InstanceSchemaNode(nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "instance is required")
	})

	t.Run("the node is named schema and carries spec, status and full metadata", func(t *testing.T) {
		t.Parallel()
		instance := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "kro.run/v1alpha1", "kind": "WebApp",
			"metadata": map[string]any{
				"name": "demo", "namespace": "default",
				"uid":        "abc-123",
				"generation": int64(4),
				"labels":     map[string]any{"env": "prod"},
			},
			"spec":   map[string]any{"replicas": int64(2)},
			"status": map[string]any{"state": "ACTIVE"},
		}}

		node, err := InstanceSchemaNode(instance)
		require.NoError(t, err)
		assert.Equal(t, SchemaNodeID, node.ID)
		assert.Equal(t, "schema", node.ID, "the node ID must match RGD's `schema` variable")
		require.NotNil(t, node.Def)
		assert.Nil(t, node.Template)
		assert.Nil(t, node.Ref)

		raw := string(node.Def.Raw)
		for _, want := range []string{"kro.run/v1alpha1", "WebApp", "abc-123", "generation", "prod", "replicas", "ACTIVE"} {
			assert.Contains(t, raw, want,
				"the schema def node must expose %q so ${schema...} references resolve", want)
		}
	})

	t.Run("an instance with no spec or status still produces a node", func(t *testing.T) {
		t.Parallel()
		instance := &unstructured.Unstructured{Object: map[string]any{
			"metadata": map[string]any{"name": "bare", "namespace": "default"},
		}}
		node, err := InstanceSchemaNode(instance)
		require.NoError(t, err)
		require.NotNil(t, node.Def)
		assert.Contains(t, string(node.Def.Raw), "bare")
	})

	t.Run("a non-map metadata falls back to name and namespace", func(t *testing.T) {
		t.Parallel()
		instance := &unstructured.Unstructured{Object: map[string]any{
			"metadata": "not-a-map",
		}}
		instance.SetName("fallback")

		node, err := InstanceSchemaNode(instance)
		require.NoError(t, err)
		require.NotNil(t, node.Def)
		assert.Contains(t, string(node.Def.Raw), "metadata")
	})
}
