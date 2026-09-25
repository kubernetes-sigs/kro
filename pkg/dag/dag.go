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
		fmt.Fprintf(&builder, "%v", s)
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
		fmt.Fprintf(&builder, "%v", s)
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

	added := make([]T, 0, len(dependencies))
	rollback := func() {
		for _, dependency := range added {
			delete(fromNode.DependsOn, dependency)
		}
	}
	for _, dependency := range dependencies {
		if _, toExists := d.Vertices[dependency]; !toExists {
			rollback()
			return fmt.Errorf("node %v does not exist", dependency)
		}
		if from == dependency {
			rollback()
			return fmt.Errorf("self references are not allowed")
		}
		if _, exists := fromNode.DependsOn[dependency]; !exists {
			fromNode.DependsOn[dependency] = struct{}{}
			added = append(added, dependency)
		}
	}

	// Every new dependency from->dep closes a cycle iff `from` is
	// already reachable from `dep` along DependsOn edges. Only entries added
	// by this call are rolled back on error, so pre-existing dependencies
	// are preserved.
	if path, closes := d.findPath(added, from); closes {
		rollback()
		cycle := make([]T, 0, len(path)+1)
		cycle = append(cycle, from)
		cycle = append(cycle, path...)
		return &CycleError[T]{
			Cycle: cycle,
		}
	}

	return nil
}

// findPath reports whether vertex `to` is reachable from any vertex in `from`
// along DependsOn edges and returns one such path.
func (d *DirectedAcyclicGraph[T]) findPath(from []T, to T) ([]T, bool) {
	visited := make(map[T]struct{}, len(from))
	stack := make([]T, 0, len(from))
	for _, start := range from {
		if _, seen := visited[start]; !seen {
			visited[start] = struct{}{}
			stack = append(stack, start)
		}
	}
	for len(stack) > 0 {
		node := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for neighbor := range d.Vertices[node].DependsOn {
			if neighbor == to {
				// A dependency closes a cycle: build the deterministic path.
				return d.findSortedPath(from, to)
			}
			if _, seen := visited[neighbor]; !seen {
				visited[neighbor] = struct{}{}
				stack = append(stack, neighbor)
			}
		}
	}
	return nil, false
}

// findSortedPath returns a deterministic path from a vertex in `from` to
// vertex `to`. It runs only on the cycle-closing call.
func (d *DirectedAcyclicGraph[T]) findSortedPath(from []T, to T) ([]T, bool) {
	visited := make(map[T]struct{}, len(from))
	parent := make(map[T]T, len(from))
	stack := make([]T, 0, len(from))
	for _, start := range slices.Sorted(slices.Values(from)) {
		if _, seen := visited[start]; !seen {
			visited[start] = struct{}{}
			// parent[start] == start marks the start of a path.
			parent[start] = start
			stack = append(stack, start)
		}
	}
	for len(stack) > 0 {
		node := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, neighbor := range slices.Sorted(maps.Keys(d.Vertices[node].DependsOn)) {
			if neighbor == to {
				path := []T{to}
				for cur := node; ; cur = parent[cur] {
					path = append(path, cur)
					if parent[cur] == cur {
						break
					}
				}
				slices.Reverse(path)
				return path, true
			}
			if _, seen := visited[neighbor]; !seen {
				visited[neighbor] = struct{}{}
				parent[neighbor] = node
				stack = append(stack, neighbor)
			}
		}
	}
	return nil, false
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

		// Visit dependencies in a deterministic (sorted) order. DependsOn is a
		// map, so ranging it directly would pick an arbitrary edge each call;
		// with more than one cycle present that non-determinism makes hasCycle
		// report a DIFFERENT cycle from run to run, which churns the compiled
		// Graph's condition message and resourceVersion on every reconcile of an
		// unchanged-but-invalid Graph (reviewer finding 3909713187). Sorting
		// makes the reported cycle a stable function of the graph alone.
		for _, dependency := range slices.Sorted(maps.Keys(d.Vertices[node].DependsOn)) {
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

	// Seed the outer walk in a deterministic (sorted) order too, for the same
	// reason: the starting vertex determines which cycle is discovered first.
	for _, node := range slices.Sorted(maps.Keys(d.Vertices)) {
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
