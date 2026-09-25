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
	"slices"
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

func TestDAGAddDependenciesCycleRollback(t *testing.T) {
	d := NewDirectedAcyclicGraph[string]()
	for i, v := range []string{"A", "B", "C", "D"} {
		if err := d.AddVertex(v, i); err != nil {
			t.Fatalf("error from AddVertex(%s, %d): %v", v, i, err)
		}
	}
	if err := d.AddDependencies("B", []string{"A"}); err != nil {
		t.Fatalf("adding dependencies: %v", err)
	}
	if err := d.AddDependencies("C", []string{"B"}); err != nil {
		t.Fatalf("adding dependencies: %v", err)
	}
	if err := d.AddDependencies("A", []string{"D"}); err != nil {
		t.Fatalf("adding dependencies: %v", err)
	}

	cerr := AsCycleError[string](d.AddDependencies("A", []string{"C"}))
	if cerr == nil {
		t.Fatal("expected a CycleError")
	}
	if want := []string{"A", "C", "B", "A"}; !slices.Equal(cerr.Cycle, want) {
		t.Errorf("got cycle %v, want %v", cerr.Cycle, want)
	}

	// The rejected dependency must be rolled back.
	if _, exists := d.Vertices["A"].DependsOn["C"]; exists {
		t.Error("rejected dependency A->C was not rolled back")
	}

	// Pre-existing dependencies of A must survive the rollback.
	if _, exists := d.Vertices["A"].DependsOn["D"]; !exists {
		t.Error("rollback removed pre-existing dependency A->D")
	}

	// Valid additions must still be accepted afterwards.
	if err := d.AddDependencies("B", []string{"D"}); err != nil {
		t.Errorf("valid dependency rejected after rollback: %v", err)
	}
}

func TestDAGAddDependenciesCycleSeedOrderIndependent(t *testing.T) {
	// Some callers build the dependency list from a map (for example
	// simpleschema's Struct.Deps), so its order can change between calls.
	cycleFor := func(deps []string) []string {
		d := NewDirectedAcyclicGraph[string]()
		for i, v := range []string{"A", "B", "C"} {
			if err := d.AddVertex(v, i); err != nil {
				t.Fatalf("AddVertex(%s): %v", v, err)
			}
		}
		for _, v := range []string{"B", "C"} {
			if err := d.AddDependencies(v, []string{"A"}); err != nil {
				t.Fatalf("AddDependencies(%s): %v", v, err)
			}
		}
		cerr := AsCycleError[string](d.AddDependencies("A", deps))
		if cerr == nil {
			t.Fatal("expected a CycleError")
		}
		return cerr.Cycle
	}

	want := []string{"A", "C", "A"}
	for _, deps := range [][]string{{"B", "C"}, {"C", "B"}} {
		if got := cycleFor(deps); !slices.Equal(got, want) {
			t.Errorf("deps %v: got cycle %v, want %v", deps, got, want)
		}
	}
}

func TestDAGAddDependenciesCycleErrorDeterministic(t *testing.T) {
	// Map iteration order varies between iterations of the same loop. The
	// reported cycle must not: an unstable cycle string churns every surface
	// that repeats the error. Rebuild the same graph many times and require
	// one message.
	var first string
	for range 200 {
		d := NewDirectedAcyclicGraph[string]()
		for i, v := range []string{"A", "B", "C", "D"} {
			if err := d.AddVertex(v, i); err != nil {
				t.Fatalf("AddVertex(%s): %v", v, err)
			}
		}
		for from, deps := range map[string][]string{"B": {"A"}, "C": {"A"}, "D": {"B", "C"}} {
			if err := d.AddDependencies(from, deps); err != nil {
				t.Fatalf("AddDependencies(%s): %v", from, err)
			}
		}

		err := d.AddDependencies("A", []string{"D"})
		if err == nil {
			t.Fatal("expected a cycle error")
		}
		if first == "" {
			first = err.Error()
		} else if err.Error() != first {
			t.Fatalf("cycle message is not stable: got %q after %q", err.Error(), first)
		}
	}
}

func TestDAGAddDependenciesErrorRollback(t *testing.T) {
	d := NewDirectedAcyclicGraph[string]()
	for i, v := range []string{"A", "B", "C"} {
		if err := d.AddVertex(v, i); err != nil {
			t.Fatalf("AddVertex(%s): %v", v, err)
		}
	}
	if err := d.AddDependencies("A", []string{"B"}); err != nil {
		t.Fatalf("AddDependencies: %v", err)
	}

	for name, deps := range map[string][]string{
		"missing vertex": {"C", "X"},
		"self reference": {"C", "A"},
	} {
		if err := d.AddDependencies("A", deps); err == nil {
			t.Errorf("%s: expected an error", name)
		}
		if _, exists := d.Vertices["A"].DependsOn["C"]; exists {
			t.Errorf("%s: A->C was not rolled back", name)
		}
		if _, exists := d.Vertices["A"].DependsOn["B"]; !exists {
			t.Errorf("%s: rollback removed pre-existing A->B", name)
		}
	}
}

func TestDAGAddDependenciesDuplicates(t *testing.T) {
	d := NewDirectedAcyclicGraph[string]()
	for i, v := range []string{"A", "B"} {
		if err := d.AddVertex(v, i); err != nil {
			t.Fatalf("AddVertex(%s): %v", v, err)
		}
	}
	for range 2 {
		if err := d.AddDependencies("A", []string{"B", "B"}); err != nil {
			t.Fatalf("AddDependencies: %v", err)
		}
	}
	if got := len(d.Vertices["A"].DependsOn); got != 1 {
		t.Errorf("got %d dependencies, want 1", got)
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

	if _, err := d.ReverseTopologicalLayers(); err == nil {
		t.Errorf("ReverseTopologicalLayers failed to detect cycle")
	} else if AsCycleError[string](err) == nil {
		t.Errorf("ReverseTopologicalLayers returned unexpected error: %T %v", err, err)
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
		{Nodes: "A,B,C,D,E,F", Edges: "D->C", Want: "A,B,D,C,E,F"},
		{Nodes: "A,B,C,D,E,F", Edges: "F->A,F->B,B->A", Want: "C,D,E,F,B,A"},
		{Nodes: "A,B,C,D,E,F", Edges: "B->A,C->A,D->B,D->C,F->E,A->E", Want: "D,B,C,A,F,E"},
		// B depends on A and C; D depends on C. B should come before D since B has lower order.
		{Nodes: "A,B,C,D", Edges: "A->B,C->B,C->D", Want: "A,C,B,D"},
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
				for edge := range strings.SplitSeq(g.Edges, ",") {
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

func TestDAGReverseTopologicalLayers(t *testing.T) {
	tests := []struct {
		name  string
		nodes string
		edges string
		want  [][]string
	}{
		{
			name: "empty graph",
			want: [][]string{},
		},
		{
			name:  "independent vertices",
			nodes: "A,B,C",
			want:  [][]string{{"A", "B", "C"}},
		},
		{
			name:  "chain",
			nodes: "A,B,C",
			edges: "A->B,B->C",
			want:  [][]string{{"C"}, {"B"}, {"A"}},
		},
		{
			name:  "diamond",
			nodes: "A,B,C,D",
			edges: "A->B,A->C,B->D,C->D",
			want:  [][]string{{"D"}, {"B", "C"}, {"A"}},
		},
		{
			name:  "uneven independent branches",
			nodes: "A,B,C,D,E",
			edges: "A->B,A->C,B->D",
			want:  [][]string{{"C", "D", "E"}, {"B"}, {"A"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := NewDirectedAcyclicGraph[string]()
			if tt.nodes != "" {
				for i, node := range strings.Split(tt.nodes, ",") {
					if err := d.AddVertex(node, i); err != nil {
						t.Fatalf("adding vertex: %v", err)
					}
				}
			}

			if tt.edges != "" {
				for edge := range strings.SplitSeq(tt.edges, ",") {
					tokens := strings.SplitN(edge, "->", 2)
					if err := d.AddDependencies(tokens[1], []string{tokens[0]}); err != nil {
						t.Fatalf("adding edge %q: %v", edge, err)
					}
				}
			}

			got, err := d.ReverseTopologicalLayers()
			if err != nil {
				t.Fatalf("building reverse topological layers: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("unexpected reverse topological layers: got %v, want %v", got, tt.want)
			}
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
		for j := range i {
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
