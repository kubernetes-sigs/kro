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

package dag

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestDAGAddNode(t *testing.T) {
	d := NewDirectedAcyclicGraph[string]()

	if err := d.AddVertex("A", 1); err != nil {
		t.Errorf("Failed to add node: %v", err)
	}

	if err := d.AddVertex("A", 1); err == nil {
		t.Error("Expected error when adding duplicate node, but got nil")
	}

	if len(d.Vertices) != 1 {
		t.Errorf("Expected 1 node, but got %d", len(d.Vertices))
	}
}

func TestDAGAddEdge(t *testing.T) {
	d := NewDirectedAcyclicGraph[string]()
	if err := d.AddVertex("A", 1); err != nil {
		t.Fatalf("error from AddVertex(A, 1): %v", err)
	}
	if err := d.AddVertex("B", 2); err != nil {
		t.Fatalf("error from AddVertex(B, 2): %v", err)
	}

	if err := d.AddDependencies("A", []string{"B"}); err != nil {
		t.Errorf("Failed to add edge: %v", err)
	}

	if err := d.AddDependencies("A", []string{"C"}); err == nil {
		t.Error("Expected error when adding edge to non-existent node, but got nil")
	}

	if err := d.AddDependencies("A", []string{"A"}); err == nil {
		t.Error("Expected error when adding self reference, but got nil")
	}
}

func TestDAGHasCycle(t *testing.T) {
	d := NewDirectedAcyclicGraph[string]()
	if err := d.AddVertex("A", 1); err != nil {
		t.Fatalf("error from AddVertex(A, 1): %v", err)
	}
	if err := d.AddVertex("B", 2); err != nil {
		t.Fatalf("error from AddVertex(B, 2): %v", err)
	}
	if err := d.AddVertex("C", 3); err != nil {
		t.Fatalf("error from AddVertex(C, 3): %v", err)
	}

	if err := d.AddDependencies("A", []string{"B"}); err != nil {
		t.Fatalf("adding dependencies: %v", err)
	}
	if err := d.AddDependencies("B", []string{"C"}); err != nil {
		t.Fatalf("adding dependencies: %v", err)
	}

	if cyclic, _ := d.hasCycle(); cyclic {
		t.Error("DAG incorrectly reported a cycle")
	}

	if err := d.AddDependencies("C", []string{"A"}); err == nil {
		t.Error("Expected error when creating a cycle, but got nil")
	}

	// pointless to test for the cycle here, so we need to emulate one
	// by artificially adding a cycle.
	d.Vertices["C"].DependsOn["A"] = struct{}{}
	if cyclic, _ := d.hasCycle(); !cyclic {
		t.Error("DAG failed to detect cycle")
	}

	if _, err := d.TopologicalSort(); err == nil {
		t.Errorf("TopologicalSort failed to detect cycle")
	} else if AsCycleError[string](err) == nil {
		t.Errorf("TopologicalSort returned unexpected error: %T %v", err, err)
	}
}

func TestDAGTopologicalSort(t *testing.T) {
	grid := []struct {
		Nodes string
		Edges string
		Want  string
	}{
		{Nodes: "A,B", Want: "A,B"},
		{Nodes: "A,B", Edges: "A->B", Want: "A,B"},
		{Nodes: "A,B", Edges: "B->A", Want: "B,A"},
		{Nodes: "A,B,C,D,E,F", Want: "A,B,C,D,E,F"},
		{Nodes: "A,B,C,D,E,F", Edges: "C->D", Want: "A,B,C,D,E,F"},
		{Nodes: "A,B,C,D,E,F", Edges: "D->C", Want: "A,B,D,E,F,C"},
		{Nodes: "A,B,C,D,E,F", Edges: "F->A,F->B,B->A", Want: "C,D,E,F,B,A"},
		{Nodes: "A,B,C,D,E,F", Edges: "B->A,C->A,D->B,D->C,F->E,A->E", Want: "D,F,B,C,A,E"},
	}

	for i, g := range grid {
		t.Run(fmt.Sprintf("[%d] nodes=%s,edges=%s", i, g.Nodes, g.Edges), func(t *testing.T) {
			d := NewDirectedAcyclicGraph[string]()
			for i, node := range strings.Split(g.Nodes, ",") {
				if err := d.AddVertex(node, i); err != nil {
					t.Fatalf("adding vertex: %v", err)
				}
			}

			if g.Edges != "" {
				for _, edge := range strings.Split(g.Edges, ",") {
					tokens := strings.SplitN(edge, "->", 2)
					if err := d.AddDependencies(tokens[1], []string{tokens[0]}); err != nil {
						t.Fatalf("adding edge %q: %v", edge, err)
					}
				}
			}

			order, err := d.TopologicalSort()
			if err != nil {
				t.Errorf("topological sort failed: %v", err)
			}

			got := strings.Join(order, ",")
			want := g.Want
			if !reflect.DeepEqual(got, want) {
				t.Errorf("unexpected result from TopologicalSort for nodes=%q edges=%q, got %q, want %q", g.Nodes, g.Edges, got, want)
			}

			checkValidTopologicalOrder(t, d, order)
		})
	}
}

func checkValidTopologicalOrder(t *testing.T, d *DirectedAcyclicGraph[string], order []string) {
	pos := make(map[string]int)
	for i, node := range order {
		pos[node] = i
	}

	// Verify that we obey the dependencies
	for _, node := range order {
		for successor := range d.Vertices[node].DependsOn {
			if pos[node] < pos[successor] {
				t.Errorf("invalid topological order: %v", order)
			}
		}
	}

	// Verify that we also obey the ordering, unless we cannot
	for i, nodeKey := range order {
		if i == 0 {
			continue
		}
		node := d.Vertices[nodeKey]
		previousNode := d.Vertices[order[i-1]]
		if previousNode.Order <= node.Order {
			continue // these two nodes are in order
		}

		// These two nodes are out of order, there should be a dependency on one of the previous nodes
		hasDep := false
		for j := 0; j < i; j++ {
			if _, found := node.DependsOn[order[j]]; found {
				hasDep = true
				break
			}
		}
		if !hasDep {
			t.Errorf("invalid topological order %q; node %v appears before %v", order, previousNode, node)
		}
	}
}

func TestDAGTopologicalSortLevels(t *testing.T) {
	grid := []struct {
		Name   string
		Nodes  string
		Edges  string
		Levels [][]string
	}{
		{
			Name:   "simple chain",
			Nodes:  "A,B,C",
			Edges:  "A->B,B->C",
			Levels: [][]string{{"A"}, {"B"}, {"C"}},
		},
		{
			Name:   "parallel resources",
			Nodes:  "A,B,C",
			Edges:  "A->C,B->C",
			Levels: [][]string{{"A", "B"}, {"C"}},
		},
		{
			Name:   "diamond pattern",
			Nodes:  "A,B,C,D",
			Edges:  "A->B,A->C,B->D,C->D",
			Levels: [][]string{{"A"}, {"B", "C"}, {"D"}},
		},
		{
			Name:   "no dependencies",
			Nodes:  "A,B,C",
			Edges:  "",
			Levels: [][]string{{"A", "B", "C"}},
		},
		{
			Name:   "complex DAG",
			Nodes:  "A,B,C,D,E,F",
			Edges:  "A->C,B->C,C->D,C->E,D->F,E->F",
			Levels: [][]string{{"A", "B"}, {"C"}, {"D", "E"}, {"F"}},
		},
		{
			Name:   "original order preserved within level",
			Nodes:  "Z,Y,X,W,V,U",
			Edges:  "Z->U,Y->U,X->U",
			Levels: [][]string{{"Z", "Y", "X", "W", "V"}, {"U"}},
		},
	}

	for _, g := range grid {
		t.Run(g.Name, func(t *testing.T) {
			d := NewDirectedAcyclicGraph[string]()
			for i, node := range strings.Split(g.Nodes, ",") {
				if err := d.AddVertex(node, i); err != nil {
					t.Fatalf("adding vertex: %v", err)
				}
			}

			if g.Edges != "" {
				for _, edge := range strings.Split(g.Edges, ",") {
					tokens := strings.SplitN(edge, "->", 2)
					if err := d.AddDependencies(tokens[1], []string{tokens[0]}); err != nil {
						t.Fatalf("adding edge %q: %v", edge, err)
					}
				}
			}

			levels, err := d.TopologicalSortLevels()
			if err != nil {
				t.Errorf("topological sort levels failed: %v", err)
			}

			if len(levels) != len(g.Levels) {
				t.Errorf("expected %d levels, got %d", len(g.Levels), len(levels))
				t.Logf("Expected levels: %v", g.Levels)
				t.Logf("Got levels: %v", levels)
				return
			}

			for i, expectedLevel := range g.Levels {
				if len(levels[i]) != len(expectedLevel) {
					t.Errorf("level %d: expected %d nodes, got %d", i, len(expectedLevel), len(levels[i]))
					continue
				}

				// Check exact order within level (not just membership)
				for j, expectedNode := range expectedLevel {
					if levels[i][j] != expectedNode {
						t.Errorf("level %d position %d: expected %s, got %s", i, j, expectedNode, levels[i][j])
					}
				}
			}

			// Verify that nodes in the same level have no dependencies on each other
			for levelIdx, level := range levels {
				for _, node := range level {
					for _, otherNode := range level {
						if node == otherNode {
							continue
						}
						if _, hasDep := d.Vertices[node].DependsOn[otherNode]; hasDep {
							t.Errorf("level %d: node %s depends on %s in the same level", levelIdx, node, otherNode)
						}
					}
				}
			}

			// Verify that all dependencies of a node appear in earlier levels
			nodeLevel := make(map[string]int)
			for i, level := range levels {
				for _, node := range level {
					nodeLevel[node] = i
				}
			}

			for _, level := range levels {
				for _, node := range level {
					for dep := range d.Vertices[node].DependsOn {
						if nodeLevel[dep] >= nodeLevel[node] {
							t.Errorf("node %s depends on %s, but %s is not in an earlier level", node, dep, dep)
						}
					}
				}
			}
		})
	}
}
