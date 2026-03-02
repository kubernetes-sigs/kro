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

	"github.com/kubernetes-sigs/kro/pkg/graph"
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
				Nodes: map[string]*graph.CompiledNode{
					"a": {Meta: graph.CompiledNodeMeta{ID: "a", Type: graph.NodeTypeScalar}},
					"b": {Meta: graph.CompiledNodeMeta{ID: "b", Type: graph.NodeTypeScalar}},
					"c": {Meta: graph.CompiledNodeMeta{ID: "c", Type: graph.NodeTypeCollection}},
				},
				Instance: &graph.CompiledNode{Meta: graph.CompiledNodeMeta{ID: graph.SchemaVarName, Type: graph.NodeTypeInstance}},
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
				Nodes: map[string]*graph.CompiledNode{
					"vpc":    {Meta: graph.CompiledNodeMeta{ID: "vpc", Type: graph.NodeTypeScalar}},
					"subnet": {Meta: graph.CompiledNodeMeta{ID: "subnet", Type: graph.NodeTypeScalar, Dependencies: []string{"vpc"}}},
				},
				Instance: &graph.CompiledNode{Meta: graph.CompiledNodeMeta{ID: graph.SchemaVarName, Type: graph.NodeTypeInstance}},
			},
			instance: testInstance("test"),
			validate: func(t *testing.T, rt *Runtime) {
				nodes := rt.Nodes()
				subnet := nodes[1]

				_, hasVPC := subnet.deps["vpc"]
				_, hasSchema := subnet.deps[graph.SchemaVarName]
				assert.True(t, hasVPC)
				assert.True(t, hasSchema)
				assert.Same(t, nodes[0], subnet.deps["vpc"])
				assert.Same(t, rt.Instance(), subnet.deps[graph.SchemaVarName])
			},
		},
		{
			name: "instance node has observed set",
			graph: &graph.Graph{
				TopologicalOrder: []string{},
				Nodes:            map[string]*graph.CompiledNode{},
				Instance:         &graph.CompiledNode{Meta: graph.CompiledNodeMeta{ID: graph.SchemaVarName, Type: graph.NodeTypeInstance}},
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
				Nodes: map[string]*graph.CompiledNode{
					"a": {
						Meta: graph.CompiledNodeMeta{ID: "a", Type: graph.NodeTypeScalar},
						Variables: []*graph.CompiledVariable{
							{
								Kind:  graph.FieldStatic,
								Exprs: []*graph.CompiledExpr{{Raw: "schema.spec.name"}},
							},
						},
					},
					"b": {
						Meta: graph.CompiledNodeMeta{ID: "b", Type: graph.NodeTypeScalar},
						Variables: []*graph.CompiledVariable{
							{
								Kind:  graph.FieldStatic,
								Exprs: []*graph.CompiledExpr{{Raw: "schema.spec.name"}},
							},
						},
					},
				},
				Instance: &graph.CompiledNode{Meta: graph.CompiledNodeMeta{ID: graph.SchemaVarName, Type: graph.NodeTypeInstance}},
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
			name: "original graph not mutated",
			graph: &graph.Graph{
				TopologicalOrder: []string{"node"},
				Nodes: map[string]*graph.CompiledNode{
					"node": {
						Meta:        graph.CompiledNodeMeta{ID: "node", Type: graph.NodeTypeScalar, Dependencies: []string{"dep1"}},
						IncludeWhen: []*graph.CompiledExpr{{Raw: "schema.spec.enabled"}},
					},
				},
				Instance: &graph.CompiledNode{Meta: graph.CompiledNodeMeta{ID: graph.SchemaVarName, Type: graph.NodeTypeInstance}},
			},
			instance: testInstance("test"),
			validate: func(t *testing.T, rt *Runtime) {
				// Mutate runtime node
				nodes := rt.Nodes()
				nodes[0].Spec.Meta.Dependencies = append(nodes[0].Spec.Meta.Dependencies, "new")
				nodes[0].Spec.IncludeWhen = append(nodes[0].Spec.IncludeWhen, &graph.CompiledExpr{Raw: "new"})
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
		Nodes: map[string]*graph.CompiledNode{
			"deployment": {
				Meta: graph.CompiledNodeMeta{ID: "deployment", Type: graph.NodeTypeScalar},
			},
		},
		Instance: &graph.CompiledNode{
			Meta: graph.CompiledNodeMeta{
				ID:           graph.SchemaVarName,
				Type:         graph.NodeTypeInstance,
				Dependencies: []string{"deployment"},
			},
			Variables: []*graph.CompiledVariable{
				{
					Kind:  graph.FieldDynamic,
					Path:  "status.deploymentReady",
					Exprs: []*graph.CompiledExpr{{Raw: "deployment.status.ready"}},
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
