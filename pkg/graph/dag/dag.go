// Copyright 2025 The Kube Resource Orchestrator Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package dag

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"golang.org/x/exp/maps"
)

// Vertex represents a node/vertex in a directed acyclic graph.
type Vertex struct {
	// ID is a unique identifier for the node
	ID string
	// Order records the original order, and is used to preserve the original user-provided ordering as far as possible.
	Order int
	// DependsOn stores the IDs of the nodes that this node depends on.
	// If we depend on another vertex, we must appear after that vertex in the topological sort.
	DependsOn map[string]struct{}
}

func (v Vertex) String() string {
	dependsOn := strings.Join(maps.Keys(v.DependsOn), ",")
	return fmt.Sprintf("Vertex[ID: %s, Order: %d, DependsOn: %s]", v.ID, v.Order, dependsOn)
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
func (d *DirectedAcyclicGraph) AddVertex(id string, order int) error {
	if _, exists := d.Vertices[id]; exists {
		return fmt.Errorf("node %s already exists", id)
	}
	d.Vertices[id] = &Vertex{
		ID:        id,
		Order:     order,
		DependsOn: make(map[string]struct{}),
	}
	return nil
}

type CycleError struct {
	Cycle []string
}

func (e *CycleError) Error() string {
	return fmt.Sprintf("graph contains a cycle: %s", formatCycle(e.Cycle))
}

func formatCycle(cycle []string) string {
	return strings.Join(cycle, " -> ")
}

// AsCycleError returns the (potentially wrapped) CycleError, or nil if it is not a CycleError.
func AsCycleError(err error) *CycleError {
	cycleError := &CycleError{}
	if errors.As(err, &cycleError) {
		return cycleError
	}
	return nil
}

// AddDependencies adds a set of dependencies to the "from" vertex.
// This indicates that all the vertexes in "dependencies" must occur before "from".
func (d *DirectedAcyclicGraph) AddDependencies(from string, dependencies []string) error {
	fromNode, fromExists := d.Vertices[from]
	if !fromExists {
		return fmt.Errorf("node %s does not exist", from)
	}

	for _, dependency := range dependencies {
		_, toExists := d.Vertices[dependency]
		if !toExists {
			return fmt.Errorf("node %s does not exist", dependency)
		}
		if from == dependency {
			return fmt.Errorf("self references are not allowed")
		}
		fromNode.DependsOn[dependency] = struct{}{}
	}

	// Check if the graph is still a DAG
	hasCycle, cycle := d.hasCycle()
	if hasCycle {
		// Ehmmm, we have a cycle, let's remove the edge we just added
		for _, dependency := range dependencies {
			delete(fromNode.DependsOn, dependency)
		}
		return &CycleError{
			Cycle: cycle,
		}
	}

	return nil
}

// TopologicalSort returns the vertexes of the graph, respecting topological ordering first,
// and preserving order of nodes within each "depth" of the topological ordering.
func (d *DirectedAcyclicGraph) TopologicalSort() ([]string, error) {
	visited := make(map[string]bool)
	var order []string

	// Make a list of vertices, sorted by Order
	vertices := make([]*Vertex, 0, len(d.Vertices))
	for _, vertex := range d.Vertices {
		vertices = append(vertices, vertex)
	}
	sort.Slice(vertices, func(i, j int) bool {
		return vertices[i].Order < vertices[j].Order
	})

	for len(visited) < len(vertices) {
		progress := false

		for _, vertex := range vertices {
			if visited[vertex.ID] {
				continue
			}

			allDependenciesReady := true
			for dep := range vertex.DependsOn {
				if !visited[dep] {
					allDependenciesReady = false
					break
				}
			}
			if !allDependenciesReady {
				continue
			}

			order = append(order, vertex.ID)
			visited[vertex.ID] = true
			progress = true
		}

		if !progress {
			hasCycle, cycle := d.hasCycle()
			if !hasCycle {
				// Unexpected!
				return nil, &CycleError{}
			}
			return nil, &CycleError{
				Cycle: cycle,
			}
		}
	}

	return order, nil
}

func (d *DirectedAcyclicGraph) hasCycle() (bool, []string) {
	visited := make(map[string]bool)
	recStack := make(map[string]bool)
	var cyclePath []string

	var dfs func(string) bool
	dfs = func(node string) bool {
		visited[node] = true
		recStack[node] = true
		cyclePath = append(cyclePath, node)

		for dependency := range d.Vertices[node].DependsOn {
			if !visited[dependency] {
				if dfs(dependency) {
					return true
				}
			} else if recStack[dependency] {
				// Found a cycle, add the closing node to complete the cycle
				cyclePath = append(cyclePath, dependency)
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
