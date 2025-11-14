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

package walker

import (
	"cmp"
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kubernetes-sigs/kro/pkg/graph/dag"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/semaphore"
)

// VertexFunc is the callback function executed for each vertex in the graph.
// It receives the vertex ID and returns an error if processing fails.
type VertexFunc[T cmp.Ordered] func(ctx context.Context, vertexID T) error

// Options configures the walker's execution behavior.
type Options struct {
	// Parallelism sets the maximum number of concurrent workers.
	// If <= 0, defaults to runtime.NumCPU().
	Parallelism int

	// StopOnError determines whether to stop execution when any vertex fails.
	// If false, continues executing independent vertices even after failures.
	StopOnError bool

	// Reverse walks the DAG in reverse topological order (bottom-up).
	// Useful for deletion scenarios where dependents must be processed before dependencies.
	Reverse bool
}

// Walk executes the DAG in parallel using a fixed worker pool.
// This is a clean, modern implementation that:
// - Separates topology computation from execution
// - Uses bounded worker pools for predictable parallelism
// - Employs atomic operations for indegree tracking
// - Leverages errgroup for structured concurrency
// - Maintains an immutable DAG (no dynamic graph mutation)
func Walk[T cmp.Ordered](ctx context.Context, d *dag.DirectedAcyclicGraph[T], fn VertexFunc[T], opts Options) map[T]error {
	if opts.Parallelism <= 0 {
		opts.Parallelism = runtime.NumCPU()
	}

	// Build indegree map and dependency graph
	// For normal order: indegree = number of dependencies
	// For reverse order: indegree = number of dependents
	indegree := make(map[T]*int32)
	nextVertices := make(map[T][]T) // vertices to process next after completing this one

	if opts.Reverse {
		// Reverse order: process vertices from bottom to top
		// A vertex is ready when all its dependents have been processed
		for id := range d.Vertices {
			// Count how many vertices depend on this one
			count := int32(0)
			indegree[id] = &count
		}

		// Build dependency counts and forward edges
		for id := range d.Vertices {
			for dep := range d.Vertices[id].DependsOn {
				// Increment the indegree of the dependency
				atomic.AddInt32(indegree[dep], 1)
				// After processing id, we can process its dependencies
				nextVertices[id] = append(nextVertices[id], dep)
			}
		}
	} else {
		// Normal order: process vertices from top to bottom
		// A vertex is ready when all its dependencies have been processed
		for id := range d.Vertices {
			count := int32(len(d.Vertices[id].DependsOn))
			indegree[id] = &count

			// Build reverse edges: after processing a dependency, process its dependents
			for dep := range d.Vertices[id].DependsOn {
				nextVertices[dep] = append(nextVertices[dep], id)
			}
		}
	}

	// Channel for ready vertices
	ready := make(chan T, len(d.Vertices))

	// Queue initial vertices with no dependencies
	for id, count := range indegree {
		if *count == 0 {
			ready <- id
		}
	}

	// Error tracking and processed tracking
	var errorsMu sync.Mutex
	errors := make(map[T]error)
	processed := make(map[T]bool)

	// Use errgroup for structured concurrency
	g, ctx := errgroup.WithContext(ctx)

	// Semaphore for bounded parallelism
	sem := semaphore.NewWeighted(int64(opts.Parallelism))

	// Helper to mark vertices as skipped due to failed dependencies
	var markSkipped func(T)
	markSkipped = func(id T) {
		errorsMu.Lock()
		if processed[id] {
			errorsMu.Unlock()
			return
		}
		processed[id] = true
		errorsMu.Unlock()

		// Recursively mark next vertices as skipped
		for _, next := range nextVertices[id] {
			markSkipped(next)
		}
	}

	// Worker pool
	for i := 0; i < opts.Parallelism; i++ {
		g.Go(func() error {
			for {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case id, ok := <-ready:
					if !ok {
						return nil
					}

					// Acquire semaphore
					if err := sem.Acquire(ctx, 1); err != nil {
						return err
					}

					// Execute vertex
					err := fn(ctx, id)
					sem.Release(1)

					errorsMu.Lock()
					processed[id] = true
					if err != nil {
						errors[id] = err
					}
					errorsMu.Unlock()

					if err != nil {
						// Mark all next vertices as skipped
						for _, next := range nextVertices[id] {
							markSkipped(next)
						}

						if opts.StopOnError {
							return err
						}
						// Continue processing - next vertices won't be unblocked
						continue
					}

					// Decrement indegree of next vertices and queue ready ones
					for _, next := range nextVertices[id] {
						if atomic.AddInt32(indegree[next], -1) == 0 {
							// Check if already marked as skipped
							errorsMu.Lock()
							isSkipped := processed[next]
							errorsMu.Unlock()

							if !isSkipped {
								select {
								case ready <- next:
								case <-ctx.Done():
									return ctx.Err()
								}
							}
						}
					}
				}
			}
		})
	}

	// Close ready channel when all vertices are processed or context done
	go func() {
		for {
			errorsMu.Lock()
			allProcessed := len(processed) == len(d.Vertices)
			errorsMu.Unlock()

			if allProcessed {
				close(ready)
				return
			}

			select {
			case <-ctx.Done():
				close(ready)
				return
			case <-time.After(10 * time.Millisecond):
				// Check again
			}
		}
	}()

	// Wait for completion
	g.Wait()

	return errors
}
