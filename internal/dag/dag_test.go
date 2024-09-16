// Copyright Amazon.com Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//	http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package dag

import (
	"reflect"
	"testing"
)

func TestDAGAddNode(t *testing.T) {
	d := NewDirectedAcyclicGraph()

	if err := d.AddVertex("A"); err != nil {
		t.Errorf("Failed to add node: %v", err)
	}

	if err := d.AddVertex("A"); err == nil {
		t.Error("Expected error when adding duplicate node, but got nil")
	}

	if len(d.Vertices) != 1 {
		t.Errorf("Expected 1 node, but got %d", len(d.Vertices))
	}
}

func TestDAGAddEdge(t *testing.T) {
	d := NewDirectedAcyclicGraph()
	d.AddVertex("A")
	d.AddVertex("B")

	if err := d.AddEdge("A", "B"); err != nil {
		t.Errorf("Failed to add edge: %v", err)
	}

	if err := d.AddEdge("A", "C"); err == nil {
		t.Error("Expected error when adding edge to non-existent node, but got nil")
	}

	if err := d.AddEdge("A", "A"); err == nil {
		t.Error("Expected error when adding self refernce, but got nil")
	}
}

func TestDAGHasCycle(t *testing.T) {
	d := NewDirectedAcyclicGraph()
	d.AddVertex("A")
	d.AddVertex("B")
	d.AddVertex("C")

	d.AddEdge("A", "B")
	d.AddEdge("B", "C")

	if cyclic, _ := d.HasCycle(); cyclic {
		t.Error("DAG incorrectly reported a cycle")
	}

	if err := d.AddEdge("C", "A"); err == nil {
		t.Error("Expected error when creating a cycle, but got nil")
	}

	// pointless to test for the cycle here, so we need to emulate one
	// by artificially adding a cycle.
	d.Vertices["C"].Edges["A"] = struct{}{}
	if cyclic, _ := d.HasCycle(); !cyclic {
		t.Error("DAG failed to detect cycle")
	}
}

func TestDAGTopologicalSort(t *testing.T) {
	d := NewDirectedAcyclicGraph()
	d.AddVertex("A")
	d.AddVertex("B")
	d.AddVertex("C")
	d.AddVertex("D")
	d.AddVertex("E")
	d.AddVertex("F")

	d.AddEdge("A", "B")
	d.AddEdge("A", "C")
	d.AddEdge("B", "D")
	d.AddEdge("C", "D")
	d.AddEdge("E", "A")
	d.AddEdge("E", "F")
	// [D B C A F E]

	order, err := d.TopologicalSort()
	if err != nil {
		t.Errorf("topological sort failed: %v", err)
	}

	// slices.Reverse(order)

	if !isValidTopologicalOrder(d, order) {
		t.Errorf("invalid topological order: %v", order)
	}
}

func TestDAGGetNodes(t *testing.T) {
	d := NewDirectedAcyclicGraph()
	d.AddVertex("A")
	d.AddVertex("B")
	d.AddVertex("C")

	nodes := d.GetVertices()
	expected := []string{"A", "B", "C"}

	if !reflect.DeepEqual(nodes, expected) {
		t.Errorf("GetNodes() = %v, want %v", nodes, expected)
	}
}

func TestDAGGetEdges(t *testing.T) {
	d := NewDirectedAcyclicGraph()
	d.AddVertex("A")
	d.AddVertex("B")
	d.AddVertex("C")
	d.AddEdge("A", "B")
	d.AddEdge("B", "C")

	edges := d.GetEdges()
	expected := [][2]string{{"A", "B"}, {"B", "C"}}

	if !reflect.DeepEqual(edges, expected) {
		t.Errorf("GetEdges() = %v, want %v", edges, expected)
	}
}

func isValidTopologicalOrder(d *DirectedAcyclicGraph, order []string) bool {
	pos := make(map[string]int)
	for i, node := range order {
		pos[node] = i
	}
	for _, node := range order {
		for successor := range d.Vertices[node].Edges {
			if pos[node] < pos[successor] {
				return false
			}
		}
	}
	return true
}
