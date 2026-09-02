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

package compiler

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	expv1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/dag"
	"github.com/kubernetes-sigs/kro/pkg/graph"
)

func TestValidateNodeID(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		id      string
		wantErr string
	}{
		{name: "lowercase", id: "vpc"},
		{name: "camelCase", id: "myConfig"},
		{name: "upper", id: "Pod"},
		{name: "with digits", id: "vpc1"},
		{name: "empty", id: "", wantErr: "required"},
		{name: "starts with digit", id: "1vpc", wantErr: "must match"},
		{name: "hyphen rejected", id: "vpc-1", wantErr: "must match"},
		{name: "underscore rejected", id: "vpc_1", wantErr: "must match"},
		{name: "reserved metadata", id: "metadata", wantErr: "reserved"},
		{name: "reserved cel keyword true", id: "true", wantErr: "reserved"},
		{name: "reserved cel keyword as", id: "as", wantErr: "reserved"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateNodeID(tc.id)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestValidateNodeShape(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		node    expv1alpha1.Node
		wantErr string
	}{
		{name: "exactly one — template", node: expv1alpha1.Node{ID: "n", Template: rawExtensionFromString("{}")}},
		{name: "exactly one — def", node: expv1alpha1.Node{ID: "n", Def: rawExtensionFromString("{}")}},
		{name: "none set", node: expv1alpha1.Node{ID: "n"}, wantErr: "exactly one"},
		{
			name: "two set",
			node: expv1alpha1.Node{ID: "n",
				Template: rawExtensionFromString("{}"),
				Def:      rawExtensionFromString("{}"),
			},
			wantErr: "exactly one",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateNodeShape(&tc.node)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestValidateForEachShape(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		node    expv1alpha1.Node
		wantErr string
	}{
		{name: "no forEach", node: expv1alpha1.Node{ID: "n"}},
		{name: "single dim", node: expv1alpha1.Node{ID: "n",
			ForEach: []expv1alpha1.ForEachDimension{{"region": "${regions}"}},
		}},
		{name: "two independent dims", node: expv1alpha1.Node{ID: "n",
			ForEach: []expv1alpha1.ForEachDimension{
				{"region": "${regions}"}, {"tier": "${tiers}"},
			},
		}},
		{name: "dim with zero entries", node: expv1alpha1.Node{ID: "n",
			ForEach: []expv1alpha1.ForEachDimension{{}},
		}, wantErr: "exactly one entry"},
		{name: "iterator collides with node id", node: expv1alpha1.Node{ID: "region",
			ForEach: []expv1alpha1.ForEachDimension{{"region": "${regions}"}},
		}, wantErr: "collides with node id"},
		{name: "iterator reserved", node: expv1alpha1.Node{ID: "n",
			ForEach: []expv1alpha1.ForEachDimension{{"metadata": "${stuff}"}},
		}, wantErr: "reserved"},
		{name: "iterator reserved each", node: expv1alpha1.Node{ID: "n",
			ForEach: []expv1alpha1.ForEachDimension{{"each": "${stuff}"}},
		}, wantErr: `iterator name "each" is reserved for collection readyWhen expressions`},
		{name: "duplicate iterators", node: expv1alpha1.Node{ID: "n",
			ForEach: []expv1alpha1.ForEachDimension{{"r": "${a}"}, {"r": "${b}"}},
		}, wantErr: "duplicate iterator"},
		{name: "invalid iterator characters", node: expv1alpha1.Node{ID: "n",
			ForEach: []expv1alpha1.ForEachDimension{{"r-1": "${a}"}},
		}, wantErr: "must match"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateForEachShape(&tc.node)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestValidateGraph(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		graph   *expv1alpha1.Graph
		wantErr string
	}{
		{name: "nil graph", graph: nil, wantErr: "graph is nil"},
		{name: "no nodes", graph: &expv1alpha1.Graph{}, wantErr: "at least one node"},
		{
			name: "duplicate ids",
			graph: &expv1alpha1.Graph{Spec: expv1alpha1.GraphSpec{Nodes: []expv1alpha1.Node{
				{ID: "n", Template: rawExtensionFromString("{}")},
				{ID: "n", Template: rawExtensionFromString("{}")},
			}}},
			wantErr: "duplicate",
		},
		{
			name: "bad id surfaces with index",
			graph: &expv1alpha1.Graph{Spec: expv1alpha1.GraphSpec{Nodes: []expv1alpha1.Node{
				{ID: "ok", Template: rawExtensionFromString("{}")},
				{ID: "bad-id", Template: rawExtensionFromString("{}")},
			}}},
			wantErr: "node[1]",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateGraph(tc.graph)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestRequireResolvableRoot(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		build   func() *dag.DirectedAcyclicGraph[string]
		wantErr string
	}{
		{
			name: "acyclic graph with a root",
			build: func() *dag.DirectedAcyclicGraph[string] {
				g := dag.NewDirectedAcyclicGraph[string]()
				_ = g.AddVertex("a", 0)
				_ = g.AddVertex("b", 1)
				_ = g.AddDependencies("b", []string{"a"})
				return g
			},
		},
		{
			name: "all nodes carry CEL but graph is acyclic",
			// b depends on a, a depends on nothing: a is a resolvable root
			// even though every node might carry CEL expressions.
			build: func() *dag.DirectedAcyclicGraph[string] {
				g := dag.NewDirectedAcyclicGraph[string]()
				_ = g.AddVertex("a", 0)
				_ = g.AddVertex("b", 1)
				_ = g.AddVertex("c", 2)
				_ = g.AddDependencies("b", []string{"a"})
				_ = g.AddDependencies("c", []string{"b"})
				return g
			},
		},
		{
			name: "empty graph",
			build: func() *dag.DirectedAcyclicGraph[string] {
				return dag.NewDirectedAcyclicGraph[string]()
			},
			wantErr: "at least one node",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := requireResolvableRoot(tc.build())
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestReservedNodeIDsSynchronizedWithGraph(t *testing.T) {
	t.Parallel()
	for id := range reservedNodeIDs {
		assert.True(t, graph.IsKROReservedWord(id),
			"compiler reservedNodeID %q must be present in graph.reservedKeyWords so RGD validation rejects it early", id)
	}
}
