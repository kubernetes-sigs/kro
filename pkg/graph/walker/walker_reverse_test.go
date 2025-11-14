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
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/kubernetes-sigs/kro/pkg/graph/dag"
)

func TestWalkReverse_EmptyGraph(t *testing.T) {
	d := dag.NewDirectedAcyclicGraph[string]()

	executed := []string{}
	vertexFunc := func(ctx context.Context, vertexID string) error {
		executed = append(executed, vertexID)
		return nil
	}

	errs := Walk(context.Background(), d, vertexFunc, Options{Reverse: true})

	assert.Empty(t, errs)
	assert.Empty(t, executed)
}

func TestWalkReverse_SingleVertex(t *testing.T) {
	d := dag.NewDirectedAcyclicGraph[string]()
	_ = d.AddVertex("A", 0)

	executed := []string{}
	vertexFunc := func(ctx context.Context, vertexID string) error {
		executed = append(executed, vertexID)
		return nil
	}

	errs := Walk(context.Background(), d, vertexFunc, Options{Reverse: true})

	assert.Empty(t, errs)
	assert.Equal(t, []string{"A"}, executed)
}

func TestWalkReverse_TwoVerticesNoDependency(t *testing.T) {
	d := dag.NewDirectedAcyclicGraph[string]()
	_ = d.AddVertex("A", 0)
	_ = d.AddVertex("B", 1)

	var mu sync.Mutex
	executed := []string{}
	vertexFunc := func(ctx context.Context, vertexID string) error {
		mu.Lock()
		executed = append(executed, vertexID)
		mu.Unlock()
		return nil
	}

	errs := Walk(context.Background(), d, vertexFunc, Options{Reverse: true})

	assert.Empty(t, errs)
	assert.Len(t, executed, 2)
	assert.Contains(t, executed, "A")
	assert.Contains(t, executed, "B")
}

func TestWalkReverse_ChainOfTwo(t *testing.T) {
	// A -> B in forward
	// B then A in reverse
	d := dag.NewDirectedAcyclicGraph[string]()
	_ = d.AddVertex("A", 0)
	_ = d.AddVertex("B", 1)
	_ = d.AddDependencies("B", []string{"A"})

	executed := []string{}
	vertexFunc := func(ctx context.Context, vertexID string) error {
		executed = append(executed, vertexID)
		return nil
	}

	errs := Walk(context.Background(), d, vertexFunc, Options{Reverse: true})

	assert.Empty(t, errs)
	assert.Equal(t, []string{"B", "A"}, executed)
}

func TestWalkReverse_ChainOfThree(t *testing.T) {
	// A -> B -> C in forward
	// C -> B -> A in reverse
	d := dag.NewDirectedAcyclicGraph[string]()
	_ = d.AddVertex("A", 0)
	_ = d.AddVertex("B", 1)
	_ = d.AddVertex("C", 2)
	_ = d.AddDependencies("B", []string{"A"})
	_ = d.AddDependencies("C", []string{"B"})

	executed := []string{}
	vertexFunc := func(ctx context.Context, vertexID string) error {
		executed = append(executed, vertexID)
		return nil
	}

	errs := Walk(context.Background(), d, vertexFunc, Options{Reverse: true})

	assert.Empty(t, errs)
	assert.Equal(t, []string{"C", "B", "A"}, executed)
}

func TestWalkReverse_DiamondPattern(t *testing.T) {
	//     A
	//    / \
	//   B   C
	//    \ /
	//     D
	// Reverse: D first, then B and C, then A
	d := dag.NewDirectedAcyclicGraph[string]()
	_ = d.AddVertex("A", 0)
	_ = d.AddVertex("B", 1)
	_ = d.AddVertex("C", 2)
	_ = d.AddVertex("D", 3)
	_ = d.AddDependencies("B", []string{"A"})
	_ = d.AddDependencies("C", []string{"A"})
	_ = d.AddDependencies("D", []string{"B", "C"})

	var mu sync.Mutex
	executed := []string{}
	vertexFunc := func(ctx context.Context, vertexID string) error {
		mu.Lock()
		executed = append(executed, vertexID)
		mu.Unlock()
		return nil
	}

	errs := Walk(context.Background(), d, vertexFunc, Options{Reverse: true})

	assert.Empty(t, errs)
	assert.Len(t, executed, 4)
	assert.Equal(t, "D", executed[0], "D should execute first")
	assert.Equal(t, "A", executed[3], "A should execute last")
}

func TestWalkReverse_ParallelExecutionDetected(t *testing.T) {
	// A -> B, A -> C (B and C both depend on A)
	// Reverse: B and C in parallel, then A
	d := dag.NewDirectedAcyclicGraph[string]()
	_ = d.AddVertex("A", 0)
	_ = d.AddVertex("B", 1)
	_ = d.AddVertex("C", 2)
	_ = d.AddDependencies("B", []string{"A"})
	_ = d.AddDependencies("C", []string{"A"})

	var mu sync.Mutex
	concurrentExecutions := 0
	maxConcurrent := 0

	vertexFunc := func(ctx context.Context, vertexID string) error {
		mu.Lock()
		concurrentExecutions++
		if concurrentExecutions > maxConcurrent {
			maxConcurrent = concurrentExecutions
		}
		mu.Unlock()

		time.Sleep(20 * time.Millisecond)

		mu.Lock()
		concurrentExecutions--
		mu.Unlock()
		return nil
	}

	errs := Walk(context.Background(), d, vertexFunc, Options{Reverse: true})

	assert.Empty(t, errs)
	assert.Greater(t, maxConcurrent, 1, "B and C should execute in parallel")
}

func TestWalkReverse_ErrorInVertex(t *testing.T) {
	d := dag.NewDirectedAcyclicGraph[string]()
	_ = d.AddVertex("A", 0)

	expectedErr := errors.New("vertex A failed")
	vertexFunc := func(ctx context.Context, vertexID string) error {
		return expectedErr
	}

	errs := Walk(context.Background(), d, vertexFunc, Options{Reverse: true})

	assert.Len(t, errs, 1)
	assert.Equal(t, expectedErr, errs["A"])
}

func TestWalkReverse_ErrorStopsUpstream(t *testing.T) {
	// A -> B -> C in forward
	// C -> B -> A in reverse
	// B fails, so A should not execute
	d := dag.NewDirectedAcyclicGraph[string]()
	_ = d.AddVertex("A", 0)
	_ = d.AddVertex("B", 1)
	_ = d.AddVertex("C", 2)
	_ = d.AddDependencies("B", []string{"A"})
	_ = d.AddDependencies("C", []string{"B"})

	executed := []string{}
	vertexFunc := func(ctx context.Context, vertexID string) error {
		executed = append(executed, vertexID)
		if vertexID == "B" {
			return errors.New("B failed")
		}
		return nil
	}

	errs := Walk(context.Background(), d, vertexFunc, Options{Reverse: true})

	assert.Len(t, errs, 1)
	assert.Error(t, errs["B"])
	assert.Contains(t, executed, "C", "C should execute")
	assert.Contains(t, executed, "B", "B should execute (and fail)")
	assert.NotContains(t, executed, "A", "A should not execute after B fails")
}

func TestWalkReverse_MultipleIndependentErrors(t *testing.T) {
	// A -> B, A -> C
	// Reverse: B and C fail independently
	d := dag.NewDirectedAcyclicGraph[string]()
	_ = d.AddVertex("A", 0)
	_ = d.AddVertex("B", 1)
	_ = d.AddVertex("C", 2)
	_ = d.AddDependencies("B", []string{"A"})
	_ = d.AddDependencies("C", []string{"A"})

	vertexFunc := func(ctx context.Context, vertexID string) error {
		if vertexID == "B" || vertexID == "C" {
			return errors.New(vertexID + " failed")
		}
		return nil
	}

	errs := Walk(context.Background(), d, vertexFunc, Options{Reverse: true})

	assert.Len(t, errs, 2)
	assert.Error(t, errs["B"])
	assert.Error(t, errs["C"])
}

func TestWalkReverse_ContextCancellation(t *testing.T) {
	d := dag.NewDirectedAcyclicGraph[string]()
	_ = d.AddVertex("A", 0)
	_ = d.AddVertex("B", 1)
	_ = d.AddVertex("C", 2)
	_ = d.AddDependencies("B", []string{"A"})
	_ = d.AddDependencies("C", []string{"B"})

	ctx, cancel := context.WithCancel(context.Background())

	vertexFunc := func(ctx context.Context, vertexID string) error {
		if vertexID == "C" {
			cancel()
		}
		time.Sleep(10 * time.Millisecond)
		return ctx.Err()
	}

	errs := Walk(ctx, d, vertexFunc, Options{Reverse: true})

	assert.NotEmpty(t, errs)
}

func TestWalkReverse_ParallelismLimit(t *testing.T) {
	// Create 4 independent vertices
	d := dag.NewDirectedAcyclicGraph[string]()
	_ = d.AddVertex("A", 0)
	_ = d.AddVertex("B", 1)
	_ = d.AddVertex("C", 2)
	_ = d.AddVertex("D", 3)

	var mu sync.Mutex
	currentConcurrent := 0
	maxConcurrent := 0

	vertexFunc := func(ctx context.Context, vertexID string) error {
		mu.Lock()
		currentConcurrent++
		if currentConcurrent > maxConcurrent {
			maxConcurrent = currentConcurrent
		}
		mu.Unlock()

		time.Sleep(30 * time.Millisecond)

		mu.Lock()
		currentConcurrent--
		mu.Unlock()
		return nil
	}

	errs := Walk(context.Background(), d, vertexFunc, Options{Reverse: true, Parallelism: 2})

	assert.Empty(t, errs)
	assert.LessOrEqual(t, maxConcurrent, 2, "should not exceed parallelism limit")
}

func TestWalkReverse_ComplexGraph(t *testing.T) {
	//     A   B
	//      \ / \
	//       C   D
	//        \ /
	//         E
	// Reverse: E first, then C and D, then A and B
	d := dag.NewDirectedAcyclicGraph[string]()
	_ = d.AddVertex("A", 0)
	_ = d.AddVertex("B", 1)
	_ = d.AddVertex("C", 2)
	_ = d.AddVertex("D", 3)
	_ = d.AddVertex("E", 4)
	_ = d.AddDependencies("C", []string{"A", "B"})
	_ = d.AddDependencies("D", []string{"B"})
	_ = d.AddDependencies("E", []string{"C", "D"})

	var mu sync.Mutex
	executed := []string{}
	vertexFunc := func(ctx context.Context, vertexID string) error {
		mu.Lock()
		executed = append(executed, vertexID)
		mu.Unlock()
		return nil
	}

	errs := Walk(context.Background(), d, vertexFunc, Options{Reverse: true})

	assert.Empty(t, errs)
	assert.Len(t, executed, 5)

	// Build execution order index
	index := make(map[string]int)
	for i, v := range executed {
		index[v] = i
	}

	// Verify reverse dependencies
	assert.Greater(t, index["C"], index["E"], "C should come after E in reverse")
	assert.Greater(t, index["D"], index["E"], "D should come after E in reverse")
	assert.Greater(t, index["A"], index["C"], "A should come after C in reverse")
	assert.Greater(t, index["B"], index["C"], "B should come after C in reverse")
	assert.Greater(t, index["B"], index["D"], "B should come after D in reverse")
}

func TestWalkReverse_AllVerticesExecuted(t *testing.T) {
	d := dag.NewDirectedAcyclicGraph[string]()
	vertices := []string{"A", "B", "C", "D"}
	for i, v := range vertices {
		_ = d.AddVertex(v, i)
	}

	var mu sync.Mutex
	executed := make(map[string]bool)
	vertexFunc := func(ctx context.Context, vertexID string) error {
		mu.Lock()
		executed[vertexID] = true
		mu.Unlock()
		return nil
	}

	errs := Walk(context.Background(), d, vertexFunc, Options{Reverse: true})

	assert.Empty(t, errs)
	for _, v := range vertices {
		assert.True(t, executed[v], "vertex %s should be executed", v)
	}
}
