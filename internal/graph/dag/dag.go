// Copyright Amazon.com Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//     http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package dag

import (
	"fmt"
	"sort"
	"strings"
)

// Vertex represents a node/vertex in a directed acyclic graph.
type Vertex struct {
	// ID is a unique identifier for the node
	ID string
	// Edges stores the IDs of the nodes that this node has an outgoing edge to.
	// In symphony, this would be the children of a resource.
	Edges map[string]struct{}
}

// DirectedAcyclicGraph represents a directed acyclic graph
type DirectedAcyclicGraph struct {
	// Vertices stores the nodes in the graph
	Vertices map[string]*Vertex
}

// NewDirectedAcyclicGraph creates a new directed acyclic graph.
func NewDirectedAcyclicGraph() *DirectedAcyclicGraph {
	return &DirectedAcyclicGraph{
		Vertices: make(map[string]*Vertex),
	}
}

// AddVertex adds a new node to the graph.
func (d *DirectedAcyclicGraph) AddVertex(id string) error {
	if _, exists := d.Vertices[id]; exists {
		return fmt.Errorf("node %s already exists", id)
	}
	d.Vertices[id] = &Vertex{
		ID:    id,
		Edges: make(map[string]struct{}),
	}
	return nil
}

type CycleError struct {
	From, to string
	Cycle    []string
}

func (e *CycleError) Error() string {
	return fmt.Sprintf("Cannot add edge from %s to %s. This would create a cycle: %s", e.From, e.to, formatCycle(e.Cycle))
}

func formatCycle(cycle []string) string {
	return strings.Join(cycle, " -> ")
}

// AddEdge adds a directed edge from one node to another.
func (d *DirectedAcyclicGraph) AddEdge(from, to string) error {
	fromNode, fromExists := d.Vertices[from]
	_, toExists := d.Vertices[to]
	if !fromExists {
		return fmt.Errorf("node %s does not exist", from)
	}
	if !toExists {
		return fmt.Errorf("node %s does not exist", to)
	}
	if from == to {
		return fmt.Errorf("self references are not allowed")
	}

	fromNode.Edges[to] = struct{}{}

	// Check if the graph is still a DAG
	hasCycle, cycle := d.HasCycle()
	if hasCycle {
		// Ehmmm, we have a cycle, let's remove the edge we just added
		delete(fromNode.Edges, to)
		return &CycleError{
			From:  from,
			to:    to,
			Cycle: cycle,
		}
	}

	return nil
}

func (d *DirectedAcyclicGraph) TopologicalSort() ([]string, error) {
	if cyclic, _ := d.HasCycle(); cyclic {
		return nil, fmt.Errorf("graph has a cycle")
	}

	visited := make(map[string]bool)
	var order []string

	// Get a sorted list of all vertices
	vertices := d.GetVertices()

	var dfs func(string)
	dfs = func(node string) {
		visited[node] = true

		// Sort the neighbors to ensure deterministic order
		neighbors := make([]string, 0, len(d.Vertices[node].Edges))
		for neighbor := range d.Vertices[node].Edges {
			neighbors = append(neighbors, neighbor)
		}
		sort.Strings(neighbors)

		for _, neighbor := range neighbors {
			if !visited[neighbor] {
				dfs(neighbor)
			}
		}
		order = append(order, node)
	}

	// Visit nodes in a deterministic order
	for _, node := range vertices {
		if !visited[node] {
			dfs(node)
		}
	}

	return order, nil
}

// GetVertices returns the nodes in the graph in sorted alphabetical
// order.
func (d *DirectedAcyclicGraph) GetVertices() []string {
	nodes := make([]string, 0, len(d.Vertices))
	for node := range d.Vertices {
		nodes = append(nodes, node)
	}

	// Ensure deterministic order. This is important for TopologicalSort
	// to return a deterministic result.
	sort.Strings(nodes)
	return nodes
}

// GetEdges returns the edges in the graph in sorted order...
func (d *DirectedAcyclicGraph) GetEdges() [][2]string {
	var edges [][2]string
	for from, node := range d.Vertices {
		for to := range node.Edges {
			edges = append(edges, [2]string{from, to})
		}
	}
	sort.Slice(edges, func(i, j int) bool {
		// Sort by from node first
		if edges[i][0] == edges[j][0] {
			return edges[i][1] < edges[j][1]
		}
		return edges[i][0] < edges[j][0]
	})
	return edges
}

func (d *DirectedAcyclicGraph) HasCycle() (bool, []string) {
	visited := make(map[string]bool)
	recStack := make(map[string]bool)
	var cyclePath []string

	var dfs func(string) bool
	dfs = func(node string) bool {
		visited[node] = true
		recStack[node] = true
		cyclePath = append(cyclePath, node)

		for neighbor := range d.Vertices[node].Edges {
			if !visited[neighbor] {
				if dfs(neighbor) {
					return true
				}
			} else if recStack[neighbor] {
				// Found a cycle, add the closing node to complete the cycle
				cyclePath = append(cyclePath, neighbor)
				return true
			}
		}

		recStack[node] = false
		cyclePath = cyclePath[:len(cyclePath)-1]
		return false
	}

	for node := range d.Vertices {
		if !visited[node] {
			cyclePath = []string{}
			if dfs(node) {
				// Trim the cycle path to start from the repeated node
				start := 0
				for i, v := range cyclePath[:len(cyclePath)-1] {
					if v == cyclePath[len(cyclePath)-1] {
						start = i
						break
					}
				}
				return true, cyclePath[start:]
			}
		}
	}

	return false, nil
}
