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
	"cmp"
	"container/heap"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// Vertex represents a node/vertex in a directed acyclic graph.
type Vertex[T cmp.Ordered] struct {
	// ID is a unique identifier for the node
	ID T
	// Order records the original order, and is used to preserve the original user-provided ordering as far as possible.
	Order int
	// DependsOn stores the IDs of the nodes that this node depends on.
	// If we depend on another vertex, we must appear after that vertex in the topological sort.
	DependsOn map[T]struct{}
}

func (v Vertex[T]) String() string {
	var builder strings.Builder
	builder.Grow(len(v.DependsOn))
	for i, s := range slices.Collect(maps.Keys(v.DependsOn)) {
		builder.WriteString(fmt.Sprintf("%v", s))
		if i < len(v.DependsOn)-1 {
			builder.WriteString(",")
		}
	}
	return fmt.Sprintf("Vertex[ID: %v, Order: %d, DependsOn: %s]", v.ID, v.Order, builder.String())
}

// DirectedAcyclicGraph represents a directed acyclic graph
type DirectedAcyclicGraph[T cmp.Ordered] struct {
	// Vertices stores the nodes in the graph
	Vertices map[T]*Vertex[T]
}

type topoHeapItem[T cmp.Ordered] struct {
	ID    T
	Order int
}

type topoHeap[T cmp.Ordered] []topoHeapItem[T]

func (h topoHeap[T]) Len() int {
	return len(h)
}

func (h topoHeap[T]) Less(i, j int) bool {
	if h[i].Order != h[j].Order {
		return h[i].Order < h[j].Order
	}
	return h[i].ID < h[j].ID
}

func (h topoHeap[T]) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
}

func (h *topoHeap[T]) Push(x any) {
	*h = append(*h, x.(topoHeapItem[T]))
}

func (h *topoHeap[T]) Pop() any {
	old := *h
	n := len(old)
	item := old[n-1]
	*h = old[:n-1]
	return item
}

type traversalDirection int

const (
	dependenciesFirst traversalDirection = iota
	dependentsFirst
)

type topologicalTraversal[T cmp.Ordered] struct {
	graph     *DirectedAcyclicGraph[T]
	remaining map[T]int
	unlocks   map[T][]T
	ready     topoHeap[T]
}

func newTopologicalTraversal[T cmp.Ordered](
	graph *DirectedAcyclicGraph[T],
	direction traversalDirection,
) *topologicalTraversal[T] {
	traversal := &topologicalTraversal[T]{
		graph:     graph,
		remaining: make(map[T]int, len(graph.Vertices)),
		unlocks:   make(map[T][]T, len(graph.Vertices)),
		ready:     make(topoHeap[T], 0, len(graph.Vertices)),
	}

	for id := range graph.Vertices {
		traversal.remaining[id] = 0
	}
	for id, vertex := range graph.Vertices {
		for dependency := range vertex.DependsOn {
			switch direction {
			case dependenciesFirst:
				traversal.remaining[id]++
				traversal.unlocks[dependency] = append(traversal.unlocks[dependency], id)
			case dependentsFirst:
				traversal.remaining[dependency]++
				traversal.unlocks[id] = append(traversal.unlocks[id], dependency)
			}
		}
	}

	for id, remaining := range traversal.remaining {
		if remaining == 0 {
			traversal.ready = append(traversal.ready, topoHeapItem[T]{
				ID:    id,
				Order: graph.Vertices[id].Order,
			})
		}
	}
	heap.Init(&traversal.ready)

	return traversal
}

func (t *topologicalTraversal[T]) popReady() topoHeapItem[T] {
	return heap.Pop(&t.ready).(topoHeapItem[T])
}

func (t *topologicalTraversal[T]) advance(id T) {
	for _, unlocked := range t.unlocks[id] {
		t.remaining[unlocked]--
		if t.remaining[unlocked] == 0 {
			heap.Push(&t.ready, topoHeapItem[T]{
				ID:    unlocked,
				Order: t.graph.Vertices[unlocked].Order,
			})
		}
	}
}

// NewDirectedAcyclicGraph creates a new directed acyclic graph.
func NewDirectedAcyclicGraph[T cmp.Ordered]() *DirectedAcyclicGraph[T] {
	return &DirectedAcyclicGraph[T]{
		Vertices: make(map[T]*Vertex[T]),
	}
}

// AddVertex adds a new node to the graph.
func (d *DirectedAcyclicGraph[T]) AddVertex(id T, order int) error {
	if _, exists := d.Vertices[id]; exists {
		return fmt.Errorf("node %v already exists", id)
	}
	d.Vertices[id] = &Vertex[T]{
		ID:        id,
		Order:     order,
		DependsOn: make(map[T]struct{}),
	}
	return nil
}

type CycleError[T cmp.Ordered] struct {
	Cycle []T
}

func (e *CycleError[T]) Error() string {
	return fmt.Sprintf("graph contains a cycle: %s", formatCycle(e.Cycle))
}

func formatCycle[T cmp.Ordered](cycle []T) string {
	var builder strings.Builder
	builder.Grow(len(cycle))
	for i, s := range cycle {
		builder.WriteString(fmt.Sprintf("%v", s))
		if i < len(cycle)-1 {
			builder.WriteString(" -> ")
		}
	}
	return builder.String()
}

// AsCycleError returns the (potentially wrapped) CycleError, or nil if it is not a CycleError.
func AsCycleError[T cmp.Ordered](err error) *CycleError[T] {
	if cycleError, ok := errors.AsType[*CycleError[T]](err); ok {
		return cycleError
	}
	return nil
}

// AddDependencies adds a set of dependencies to the "from" vertex.
// This indicates that all the vertexes in "dependencies" must occur before "from".
func (d *DirectedAcyclicGraph[T]) AddDependencies(from T, dependencies []T) error {
	fromNode, fromExists := d.Vertices[from]
	if !fromExists {
		return fmt.Errorf("node %v does not exist", from)
	}

	for _, dependency := range dependencies {
		_, toExists := d.Vertices[dependency]
		if !toExists {
			return fmt.Errorf("node %v does not exist", dependency)
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
		return &CycleError[T]{
			Cycle: cycle,
		}
	}

	return nil
}

// TopologicalSort returns the vertexes of the graph, respecting topological ordering first,
// and preserving order of nodes within each "depth" of the topological ordering.
func (d *DirectedAcyclicGraph[T]) TopologicalSort() ([]T, error) {
	traversal := newTopologicalTraversal(d, dependenciesFirst)

	order := make([]T, 0, len(d.Vertices))
	for traversal.ready.Len() > 0 {
		current := traversal.popReady()
		order = append(order, current.ID)
		traversal.advance(current.ID)
	}

	if len(order) == len(d.Vertices) {
		return order, nil
	}

	hasCycle, cycle := d.hasCycle()
	if !hasCycle {
		// Unexpected!
		return nil, &CycleError[T]{}
	}
	return nil, &CycleError[T]{
		Cycle: cycle,
	}
}

// ReverseTopologicalLayers returns vertices grouped into layers with dependents
// before their dependencies. Vertices in the same layer have no dependency
// ordering between them and can be processed concurrently. Original vertex
// order is preserved within each layer where possible.
func (d *DirectedAcyclicGraph[T]) ReverseTopologicalLayers() ([][]T, error) {
	traversal := newTopologicalTraversal(d, dependentsFirst)
	layers := make([][]T, 0)
	processed := 0

	for traversal.ready.Len() > 0 {
		layerSize := traversal.ready.Len()
		items := make([]topoHeapItem[T], 0, layerSize)
		layer := make([]T, 0, layerSize)
		for range layerSize {
			item := traversal.popReady()
			items = append(items, item)
			layer = append(layer, item.ID)
		}

		for _, item := range items {
			traversal.advance(item.ID)
		}
		layers = append(layers, layer)
		processed += len(layer)
	}

	if processed == len(d.Vertices) {
		return layers, nil
	}

	hasCycle, cycle := d.hasCycle()
	if !hasCycle {
		return nil, &CycleError[T]{}
	}
	return nil, &CycleError[T]{
		Cycle: cycle,
	}
}

func (d *DirectedAcyclicGraph[T]) hasCycle() (bool, []T) {
	visited := make(map[T]bool)
	recStack := make(map[T]bool)
	var cyclePath []T

	var dfs func(T) bool
	dfs = func(node T) bool {
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
			cyclePath = []T{}
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
