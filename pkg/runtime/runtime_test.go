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

package runtime

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	krocel "github.com/kubernetes-sigs/kro/pkg/cel"
	"github.com/kubernetes-sigs/kro/pkg/graph"
	"github.com/kubernetes-sigs/kro/pkg/graph/variable"
)

func TestFromGraph(t *testing.T) {
	tests := []struct {
		name     string
		graph    *graph.Graph
		instance *unstructured.Unstructured
		validate func(t *testing.T, rt *Runtime)
	}{
		{
			name: "nodes returned in topological order",
			graph: &graph.Graph{
				TopologicalOrder: []string{"a", "b", "c"},
				Nodes: map[string]*graph.Node{
					"a": {Meta: graph.NodeMeta{ID: "a", Type: graph.NodeTypeResource}},
					"b": {Meta: graph.NodeMeta{ID: "b", Type: graph.NodeTypeResource}},
					"c": {Meta: graph.NodeMeta{ID: "c", Type: graph.NodeTypeCollection}},
				},
				Instance: &graph.Node{Meta: graph.NodeMeta{ID: graph.InstanceNodeID, Type: graph.NodeTypeInstance}},
			},
			instance: testInstance("test"),
			validate: func(t *testing.T, rt *Runtime) {
				nodes := rt.Nodes()
				require.Len(t, nodes, 3)
				assert.Equal(t, "a", nodes[0].Spec.Meta.ID)
				assert.Equal(t, "b", nodes[1].Spec.Meta.ID)
				assert.Equal(t, "c", nodes[2].Spec.Meta.ID)
			},
		},
		{
			name: "dependencies wired correctly",
			graph: &graph.Graph{
				TopologicalOrder: []string{"vpc", "subnet"},
				Nodes: map[string]*graph.Node{
					"vpc":    {Meta: graph.NodeMeta{ID: "vpc", Type: graph.NodeTypeResource}},
					"subnet": {Meta: graph.NodeMeta{ID: "subnet", Type: graph.NodeTypeResource, Dependencies: []string{"vpc"}}},
				},
				Instance: &graph.Node{Meta: graph.NodeMeta{ID: graph.InstanceNodeID, Type: graph.NodeTypeInstance}},
			},
			instance: testInstance("test"),
			validate: func(t *testing.T, rt *Runtime) {
				nodes := rt.Nodes()
				subnet := nodes[1]

				_, hasVPC := subnet.deps["vpc"]
				_, hasSchema := subnet.deps[graph.InstanceNodeID]
				assert.True(t, hasVPC)
				assert.True(t, hasSchema)
				assert.Same(t, nodes[0], subnet.deps["vpc"])
				assert.Same(t, rt.Instance(), subnet.deps[graph.InstanceNodeID])
			},
		},
		{
			name: "instance node has observed set",
			graph: &graph.Graph{
				TopologicalOrder: []string{},
				Nodes:            map[string]*graph.Node{},
				Instance:         &graph.Node{Meta: graph.NodeMeta{ID: graph.InstanceNodeID, Type: graph.NodeTypeInstance}},
			},
			instance: testInstance("my-instance"),
			validate: func(t *testing.T, rt *Runtime) {
				inst := rt.Instance()
				require.Len(t, inst.observed, 1)
				assert.Equal(t, "my-instance", inst.observed[0].GetName())
			},
		},
		{
			name: "expressions deduplicated across nodes",
			graph: &graph.Graph{
				TopologicalOrder: []string{"a", "b"},
				Nodes: map[string]*graph.Node{
					"a": {
						Meta: graph.NodeMeta{ID: "a", Type: graph.NodeTypeResource},
						Variables: []*variable.ResourceField{
							{
								Kind: variable.ResourceVariableKindStatic,
								FieldDescriptor: variable.FieldDescriptor{
									Expression: krocel.NewUncompiled("schema.spec.name"),
								},
							},
						},
					},
					"b": {
						Meta: graph.NodeMeta{ID: "b", Type: graph.NodeTypeResource},
						Variables: []*variable.ResourceField{
							{
								Kind: variable.ResourceVariableKindStatic,
								FieldDescriptor: variable.FieldDescriptor{
									Expression: krocel.NewUncompiled("schema.spec.name"),
								},
							},
						},
					},
				},
				Instance: &graph.Node{Meta: graph.NodeMeta{ID: graph.InstanceNodeID, Type: graph.NodeTypeInstance}},
			},
			instance: testInstance("test"),
			validate: func(t *testing.T, rt *Runtime) {
				nodes := rt.Nodes()
				require.Len(t, nodes[0].templateExprs, 1)
				require.Len(t, nodes[1].templateExprs, 1)
				// Same pointer = deduplicated
				assert.Same(t, nodes[0].templateExprs[0], nodes[1].templateExprs[0])
			},
		},
		{
			name: "iteration expressions stay isolated while static expressions are shared",
			graph: &graph.Graph{
				TopologicalOrder: []string{"configs", "subnets", "deployment"},
				Nodes: map[string]*graph.Node{
					"configs": {
						Meta: graph.NodeMeta{ID: "configs", Type: graph.NodeTypeCollection},
						ForEach: []graph.ForEachDimension{
							{Name: "region", Expression: krocel.NewUncompiled("schema.spec.regions")},
						},
						Variables: []*variable.ResourceField{
							{
								Kind: variable.ResourceVariableKindIteration,
								FieldDescriptor: variable.FieldDescriptor{
									Path:       "metadata.name",
									Expression: krocel.NewUncompiled("region"),
								},
							},
						},
					},
					"subnets": {
						Meta: graph.NodeMeta{ID: "subnets", Type: graph.NodeTypeCollection},
						ForEach: []graph.ForEachDimension{
							{Name: "region", Expression: krocel.NewUncompiled("schema.spec.regions")},
						},
						Variables: []*variable.ResourceField{
							{
								Kind: variable.ResourceVariableKindIteration,
								FieldDescriptor: variable.FieldDescriptor{
									Path:       "metadata.name",
									Expression: krocel.NewUncompiled("region"),
								},
							},
						},
					},
					"deployment": {
						Meta:        graph.NodeMeta{ID: "deployment", Type: graph.NodeTypeResource},
						IncludeWhen: krocel.NewUncompiledSlice("schema.spec.enabled"),
						ReadyWhen:   krocel.NewUncompiledSlice("deployment.status.ready"),
						Variables: []*variable.ResourceField{
							{
								Kind: variable.ResourceVariableKindStatic,
								FieldDescriptor: variable.FieldDescriptor{
									Path:       "metadata.name",
									Expression: krocel.NewUncompiled("schema.spec.name"),
								},
							},
						},
					},
				},
				Instance: &graph.Node{
					Meta: graph.NodeMeta{ID: graph.InstanceNodeID, Type: graph.NodeTypeInstance},
					Variables: []*variable.ResourceField{
						{
							Kind: variable.ResourceVariableKindStatic,
							FieldDescriptor: variable.FieldDescriptor{
								Path:       "status.name",
								Expression: krocel.NewUncompiled("schema.spec.name"),
							},
						},
					},
				},
			},
			instance: testInstance("test"),
			validate: func(t *testing.T, rt *Runtime) {
				nodes := rt.Nodes()
				require.Len(t, nodes, 3)
				assert.NotSame(t, nodes[0].forEachExprs[0], nodes[1].forEachExprs[0])
				assert.NotSame(t, nodes[0].templateExprs[0], nodes[1].templateExprs[0])
				assert.Same(t, nodes[2].templateExprs[0], rt.Instance().templateExprs[0])
				assert.Len(t, nodes[2].includeWhenExprs, 1)
				assert.Len(t, nodes[2].readyWhenExprs, 1)
			},
		},
		{
			name: "original graph not mutated",
			graph: &graph.Graph{
				TopologicalOrder: []string{"node"},
				Nodes: map[string]*graph.Node{
					"node": {
						Meta:        graph.NodeMeta{ID: "node", Type: graph.NodeTypeResource, Dependencies: []string{"dep1"}},
						IncludeWhen: krocel.NewUncompiledSlice("schema.spec.enabled"),
					},
				},
				Instance: &graph.Node{Meta: graph.NodeMeta{ID: graph.InstanceNodeID, Type: graph.NodeTypeInstance}},
			},
			instance: testInstance("test"),
			validate: func(t *testing.T, rt *Runtime) {
				// Mutate runtime node
				nodes := rt.Nodes()
				nodes[0].Spec.Meta.Dependencies = append(nodes[0].Spec.Meta.Dependencies, "new")
				nodes[0].Spec.IncludeWhen = append(nodes[0].Spec.IncludeWhen, krocel.NewUncompiled("new"))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Capture original state for mutation test
			var origDeps []string
			var origIncludeLen int
			if node, ok := tt.graph.Nodes["node"]; ok {
				origDeps = append([]string{}, node.Meta.Dependencies...)
				origIncludeLen = len(node.IncludeWhen)
			}

			rt, err := FromGraph(tt.graph, tt.instance, graph.RGDConfig{MaxCollectionSize: 1000})
			require.NoError(t, err)

			tt.validate(t, rt)

			// Verify original graph unchanged (for mutation test)
			if node, ok := tt.graph.Nodes["node"]; ok {
				assert.Equal(t, origDeps, node.Meta.Dependencies, "original graph was mutated")
				assert.Equal(t, origIncludeLen, len(node.IncludeWhen), "original graph was mutated")
			}
		})
	}
}

func TestFromGraph_InstanceWithDependencies(t *testing.T) {
	g := &graph.Graph{
		TopologicalOrder: []string{"deployment"},
		Nodes: map[string]*graph.Node{
			"deployment": {
				Meta: graph.NodeMeta{ID: "deployment", Type: graph.NodeTypeResource},
			},
		},
		Instance: &graph.Node{
			Meta: graph.NodeMeta{
				ID:           graph.InstanceNodeID,
				Type:         graph.NodeTypeInstance,
				Dependencies: []string{"deployment"}, // instance depends on deployment for status
			},
			Variables: []*variable.ResourceField{
				{
					Kind: variable.ResourceVariableKindDynamic,
					FieldDescriptor: variable.FieldDescriptor{
						Path:       "status.deploymentReady",
						Expression: krocel.NewUncompiled("deployment.status.ready"),
					},
				},
			},
		},
	}

	rt, err := FromGraph(g, testInstance("test"), graph.RGDConfig{MaxCollectionSize: 1000})
	require.NoError(t, err)

	inst := rt.Instance()
	assert.Contains(t, inst.deps, "deployment")
	assert.Same(t, rt.Nodes()[0], inst.deps["deployment"])
	assert.Len(t, inst.templateVars, 1)
	assert.Len(t, inst.templateExprs, 1)
}

func testInstance(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "example.com/v1",
			"kind":       "Test",
			"metadata":   map[string]any{"name": name, "namespace": "default"},
		},
	}
}
