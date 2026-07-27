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

package runtime

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	expv1alpha1 "sigs.k8s.io/krocodile/api/v1alpha1"
	"sigs.k8s.io/krocodile/pkg/compiler"
)

func minimalProgram(order ...string) *compiler.Program {
	nodes := make(map[string]*compiler.Node, len(order))
	for _, id := range order {
		nodes[id] = &compiler.Node{ID: id, Kind: compiler.NodeKindDef}
	}
	return &compiler.Program{Nodes: nodes, TopologicalOrder: order}
}

func minimalGraph() *expv1alpha1.Graph {
	return &expv1alpha1.Graph{ObjectMeta: metav1.ObjectMeta{Name: "g", Namespace: "default"}}
}

// TestNodeAccessors covers the trivial Node getters via a single row per
// accessor. Each row builds its own Node so the assertions stay focused.
func TestNodeAccessors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		spec *compiler.Node
		want func(t *testing.T, n *Node)
	}{
		{
			name: "ID returns spec.ID",
			spec: &compiler.Node{ID: "x"},
			want: func(t *testing.T, n *Node) { assert.Equal(t, "x", n.ID()) },
		},
		{
			name: "Kind returns spec.Kind",
			spec: &compiler.Node{Kind: compiler.NodeKindTemplate},
			want: func(t *testing.T, n *Node) { assert.Equal(t, compiler.NodeKindTemplate, n.Kind()) },
		},
		{
			name: "Spec returns the wrapped compiler.Node",
			spec: &compiler.Node{ID: "y"},
			want: func(t *testing.T, n *Node) { assert.Equal(t, "y", n.Spec().ID) },
		},
		{
			name: "Namespaced returns spec.Namespaced",
			spec: &compiler.Node{Namespaced: true},
			want: func(t *testing.T, n *Node) { assert.True(t, n.Namespaced()) },
		},
		{
			name: "GVR returns spec.GVR",
			spec: func() *compiler.Node {
				n := &compiler.Node{}
				n.GVR.Group = "apps"
				n.GVR.Version = "v1"
				n.GVR.Resource = "deployments"
				return n
			}(),
			want: func(t *testing.T, n *Node) { assert.Equal(t, "deployments", n.GVR().Resource) },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.want(t, &Node{spec: tc.spec})
		})
	}
}

func TestRuntime(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		run  func(t *testing.T, rt *Runtime)
	}{
		{
			name: "Nodes returns topological order",
			run: func(t *testing.T, rt *Runtime) {
				got := make([]string, 0, len(rt.Nodes()))
				for _, n := range rt.Nodes() {
					got = append(got, n.ID())
				}
				assert.Equal(t, []string{"a", "b", "c"}, got)
			},
		},
		{
			name: "Node lookup hits and misses",
			run: func(t *testing.T, rt *Runtime) {
				assert.Equal(t, "b", rt.Node("b").ID())
				assert.Nil(t, rt.Node("missing"))
			},
		},
		{
			name: "Set publishes into scope",
			run: func(t *testing.T, rt *Runtime) {
				rt.Set("a", map[string]any{"x": 1})
				assert.Equal(t, map[string]any{"x": 1}, rt.Scope()["a"])
			},
		},
		{
			name: "Graph and Program accessors return what was passed in",
			run: func(t *testing.T, rt *Runtime) {
				assert.Equal(t, "g", rt.Graph().Name)
				assert.NotNil(t, rt.Program())
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rt := New(minimalProgram("a", "b", "c"), minimalGraph())
			tc.run(t, rt)
		})
	}
}
