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

package graph

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	krocel "github.com/kubernetes-sigs/kro/pkg/cel"
	"github.com/kubernetes-sigs/kro/pkg/graph/variable"
)

func TestNodeTypeString(t *testing.T) {
	tests := []struct {
		name string
		typ  NodeType
		want string
	}{
		{name: "resource", typ: NodeTypeResource, want: "Resource"},
		{name: "collection", typ: NodeTypeCollection, want: "Collection"},
		{name: "external", typ: NodeTypeExternal, want: "External"},
		{name: "instance", typ: NodeTypeInstance, want: "Instance"},
		{name: "external collection", typ: NodeTypeExternalCollection, want: "ExternalCollection"},
		{name: "unknown", typ: NodeType(99), want: "Unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.typ.String())
		})
	}
}

func TestNodeDeepCopy(t *testing.T) {
	var nilNode *Node
	assert.Nil(t, nilNode.DeepCopy())

	original := &Node{
		Meta: NodeMeta{
			ID:           "vpc",
			Index:        3,
			Type:         NodeTypeCollection,
			GVR:          schema.GroupVersionResource{Group: "ec2.services.k8s.aws", Version: "v1alpha1", Resource: "vpcs"},
			Namespaced:   true,
			Dependencies: []string{"network"},
		},
		Template: &unstructured.Unstructured{
			Object: map[string]interface{}{
				"metadata": map[string]interface{}{
					"name": "test",
					"labels": map[string]interface{}{
						"app": "demo",
					},
				},
			},
		},
		Variables: []*variable.ResourceField{
			{
				Kind: variable.ResourceVariableKindStatic,
				FieldDescriptor: variable.FieldDescriptor{
					Path:       "spec.name",
					Expression: &krocel.Expression{Original: "schema.spec.name"},
				},
			},
		},
		IncludeWhen: []*krocel.Expression{{Original: "schema.spec.enabled"}},
		ReadyWhen:   []*krocel.Expression{{Original: "vpc.status.state == 'ready'"}},
		ForEach: []ForEachDimension{
			{Name: "region", Expression: &krocel.Expression{Original: "schema.spec.regions"}},
		},
	}

	copied := original.DeepCopy()
	require.NotNil(t, copied)
	require.NotSame(t, original, copied)
	require.NotSame(t, original.Template, copied.Template)
	require.NotSame(t, original.Variables[0], copied.Variables[0])

	original.Meta.Dependencies[0] = "changed"
	original.Template.Object["metadata"].(map[string]interface{})["name"] = "mutated"
	original.Variables[0].Path = "spec.other"
	original.Variables[0].Expression = &krocel.Expression{Original: "schema.spec.other"}
	original.IncludeWhen[0] = &krocel.Expression{Original: "false"}
	original.ReadyWhen[0] = &krocel.Expression{Original: "false"}
	original.ForEach[0].Name = "zone"

	assert.Equal(t, []string{"network"}, copied.Meta.Dependencies)
	assert.Equal(t, "test", copied.Template.Object["metadata"].(map[string]interface{})["name"])
	assert.Equal(t, "spec.name", copied.Variables[0].Path)
	assert.Equal(t, "schema.spec.name", copied.Variables[0].Expression.Original)
	assert.Equal(t, "schema.spec.enabled", copied.IncludeWhen[0].Original)
	assert.Equal(t, "vpc.status.state == 'ready'", copied.ReadyWhen[0].Original)
	assert.Equal(t, "region", copied.ForEach[0].Name)
}
