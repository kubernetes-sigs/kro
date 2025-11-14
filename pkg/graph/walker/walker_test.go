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
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/kubernetes-sigs/kro/pkg/graph/dag"
)

func TestWalk_EmptyGraph(t *testing.T) {
	d := dag.NewDirectedAcyclicGraph[string]()

	executed := []string{}
	vertexFunc := func(ctx context.Context, vertexID string) error {
		executed = append(executed, vertexID)
		return nil
	}

	errs := Walk(context.Background(), d, vertexFunc, Options{})

	assert.Empty(t, errs)
	assert.Empty(t, executed)
}

func TestWalk_SingleVertex(t *testing.T) {
	d := dag.NewDirectedAcyclicGraph[string]()
	_ = d.AddVertex("A", 0)

	executed := []string{}
	vertexFunc := func(ctx context.Context, vertexID string) error {
		executed = append(executed, vertexID)
		return nil
	}

	errs := Walk(context.Background(), d, vertexFunc, Options{})

	assert.Empty(t, errs)
	assert.Equal(t, []string{"A"}, executed)
}

func TestWalk_TwoVerticesNoDependency(t *testing.T) {
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

	errs := Walk(context.Background(), d, vertexFunc, Options{})

	assert.Empty(t, errs)
	assert.Len(t, executed, 2)
	assert.Contains(t, executed, "A")
	assert.Contains(t, executed, "B")
}

func TestWalk_TwoVerticesWithDependency(t *testing.T) {
	d := dag.NewDirectedAcyclicGraph[string]()
	_ = d.AddVertex("A", 0)
	_ = d.AddVertex("B", 1)
	_ = d.AddDependencies("B", []string{"A"})

	executed := []string{}
	vertexFunc := func(ctx context.Context, vertexID string) error {
		executed = append(executed, vertexID)
		return nil
	}

	errs := Walk(context.Background(), d, vertexFunc, Options{})

	assert.Empty(t, errs)
	assert.Equal(t, []string{"A", "B"}, executed)
}

func TestWalk_ChainOfThree(t *testing.T) {
	// A -> B -> C
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

	errs := Walk(context.Background(), d, vertexFunc, Options{})

	assert.Empty(t, errs)
	assert.Equal(t, []string{"A", "B", "C"}, executed)
}

func TestWalk_DiamondDependency(t *testing.T) {
	// A and B have no deps, C depends on both
	//   A \
	//      -> C
	//   B /
	d := dag.NewDirectedAcyclicGraph[string]()
	_ = d.AddVertex("A", 0)
	_ = d.AddVertex("B", 1)
	_ = d.AddVertex("C", 2)
	_ = d.AddDependencies("C", []string{"A", "B"})

	var mu sync.Mutex
	executed := []string{}
	vertexFunc := func(ctx context.Context, vertexID string) error {
		mu.Lock()
		executed = append(executed, vertexID)
		mu.Unlock()
		return nil
	}

	errs := Walk(context.Background(), d, vertexFunc, Options{})

	assert.Empty(t, errs)
	assert.Len(t, executed, 3)
	assert.Equal(t, "C", executed[2], "C should be executed last")
}

func TestWalk_ParallelExecutionDetected(t *testing.T) {
	d := dag.NewDirectedAcyclicGraph[string]()
	_ = d.AddVertex("A", 0)
	_ = d.AddVertex("B", 1)

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

	errs := Walk(context.Background(), d, vertexFunc, Options{})

	assert.Empty(t, errs)
	assert.Greater(t, maxConcurrent, 1, "should execute A and B in parallel")
}

func TestWalk_ErrorInVertex(t *testing.T) {
	d := dag.NewDirectedAcyclicGraph[string]()
	_ = d.AddVertex("A", 0)

	expectedErr := errors.New("vertex A failed")
	vertexFunc := func(ctx context.Context, vertexID string) error {
		return expectedErr
	}

	errs := Walk(context.Background(), d, vertexFunc, Options{})

	assert.Len(t, errs, 1)
	assert.Equal(t, expectedErr, errs["A"])
}

func TestWalk_ErrorStopsDownstream(t *testing.T) {
	// A -> B -> C, B fails
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

	errs := Walk(context.Background(), d, vertexFunc, Options{})

	assert.Len(t, errs, 1)
	assert.Error(t, errs["B"])
	assert.Contains(t, executed, "A")
	assert.Contains(t, executed, "B")
	assert.NotContains(t, executed, "C", "C should not execute after B fails")
}

func TestWalk_MultipleErrors(t *testing.T) {
	// A and B independent, both fail
	d := dag.NewDirectedAcyclicGraph[string]()
	_ = d.AddVertex("A", 0)
	_ = d.AddVertex("B", 1)

	vertexFunc := func(ctx context.Context, vertexID string) error {
		return errors.New(vertexID + " failed")
	}

	errs := Walk(context.Background(), d, vertexFunc, Options{})

	assert.Len(t, errs, 2)
	assert.Error(t, errs["A"])
	assert.Error(t, errs["B"])
}

func TestWalk_ContextCancellation(t *testing.T) {
	d := dag.NewDirectedAcyclicGraph[string]()
	_ = d.AddVertex("A", 0)
	_ = d.AddVertex("B", 1)

	ctx, cancel := context.WithCancel(context.Background())
	var executedCount atomic.Int32

	vertexFunc := func(ctx context.Context, vertexID string) error {
		executedCount.Add(1)
		cancel() // Cancel on first execution
		time.Sleep(10 * time.Millisecond)
		return ctx.Err()
	}

	errs := Walk(ctx, d, vertexFunc, Options{})

	assert.NotEmpty(t, errs)
}

func TestWalk_ParallelismLimit(t *testing.T) {
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

	errs := Walk(context.Background(), d, vertexFunc, Options{Parallelism: 2})

	assert.Empty(t, errs)
	assert.LessOrEqual(t, maxConcurrent, 2, "should not exceed parallelism limit")
}

func TestWalk_DefaultParallelism(t *testing.T) {
	d := dag.NewDirectedAcyclicGraph[string]()
	for i := 0; i < 10; i++ {
		_ = d.AddVertex(string(rune('A'+i)), i)
	}

	var mu sync.Mutex
	maxConcurrent := 0
	currentConcurrent := 0

	vertexFunc := func(ctx context.Context, vertexID string) error {
		mu.Lock()
		currentConcurrent++
		if currentConcurrent > maxConcurrent {
			maxConcurrent = currentConcurrent
		}
		mu.Unlock()

		time.Sleep(10 * time.Millisecond)

		mu.Lock()
		currentConcurrent--
		mu.Unlock()
		return nil
	}

	errs := Walk(context.Background(), d, vertexFunc, Options{})

	assert.Empty(t, errs)
	assert.Greater(t, maxConcurrent, 1, "should execute in parallel by default")
}

func TestWalk_ComplexDependencies(t *testing.T) {
	//     A   B
	//      \ / \
	//       C   D
	//        \ /
	//         E
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

	errs := Walk(context.Background(), d, vertexFunc, Options{})

	assert.Empty(t, errs)
	assert.Len(t, executed, 5)

	// Build execution order index
	index := make(map[string]int)
	for i, v := range executed {
		index[v] = i
	}

	// Verify dependencies
	assert.Less(t, index["A"], index["C"], "C should come after A")
	assert.Less(t, index["B"], index["C"], "C should come after B")
	assert.Less(t, index["B"], index["D"], "D should come after B")
	assert.Less(t, index["C"], index["E"], "E should come after C")
	assert.Less(t, index["D"], index["E"], "E should come after D")
}

func TestWalk_AllVerticesExecuted(t *testing.T) {
	d := dag.NewDirectedAcyclicGraph[string]()
	vertices := []string{"A", "B", "C", "D", "E"}
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

	errs := Walk(context.Background(), d, vertexFunc, Options{})

	assert.Empty(t, errs)
	for _, v := range vertices {
		assert.True(t, executed[v], "vertex %s should be executed", v)
	}
}
