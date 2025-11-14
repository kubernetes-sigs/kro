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
	"sync"
	"testing"
	"time"

	"github.com/kubernetes-sigs/kro/pkg/graph/dag"
)

func TestWalker_SimpleChain(t *testing.T) {
	// Create a simple chain: A -> B -> C
	d := dag.NewDirectedAcyclicGraph[string]()
	_ = d.AddVertex("A", 0)
	_ = d.AddVertex("B", 1)
	_ = d.AddVertex("C", 2)
	_ = d.AddDependencies("B", []string{"A"})
	_ = d.AddDependencies("C", []string{"B"})

	var mu sync.Mutex
	executed := []string{}

	vertexFunc := func(ctx context.Context, vertexID string) error {
		mu.Lock()
		executed = append(executed, vertexID)
		mu.Unlock()
		return nil
	}

	errors := Walk(context.Background(), d, vertexFunc, Options{})

	if len(errors) != 0 {
		t.Errorf("expected no errors, got %v", errors)
	}

	if len(executed) != 3 {
		t.Errorf("expected 3 vertices to be executed, got %d", len(executed))
	}

	// Verify order: A must come before B, B must come before C
	aIdx, bIdx, cIdx := -1, -1, -1
	for i, v := range executed {
		switch v {
		case "A":
			aIdx = i
		case "B":
			bIdx = i
		case "C":
			cIdx = i
		}
	}

	if aIdx == -1 || bIdx == -1 || cIdx == -1 {
		t.Errorf("not all vertices were executed: A=%d, B=%d, C=%d", aIdx, bIdx, cIdx)
	}

	if aIdx > bIdx {
		t.Errorf("A should come before B: A=%d, B=%d", aIdx, bIdx)
	}

	if bIdx > cIdx {
		t.Errorf("B should come before C: B=%d, C=%d", bIdx, cIdx)
	}
}

func TestWalker_ParallelExecution(t *testing.T) {
	// Create a diamond: A and B can run in parallel, both depend on C
	//   A \
	//      -> C
	//   B /
	d := dag.NewDirectedAcyclicGraph[string]()
	_ = d.AddVertex("A", 0)
	_ = d.AddVertex("B", 1)
	_ = d.AddVertex("C", 2)
	_ = d.AddDependencies("C", []string{"A", "B"})

	var mu sync.Mutex
	executing := make(map[string]bool)
	executed := []string{}
	parallelExecutionDetected := false

	vertexFunc := func(ctx context.Context, vertexID string) error {
		mu.Lock()
		// Check if another vertex is currently executing
		if len(executing) > 0 {
			parallelExecutionDetected = true
		}
		executing[vertexID] = true
		mu.Unlock()

		// Simulate some work
		time.Sleep(10 * time.Millisecond)

		mu.Lock()
		delete(executing, vertexID)
		executed = append(executed, vertexID)
		mu.Unlock()

		return nil
	}

	errors := Walk(context.Background(), d, vertexFunc, Options{})

	if len(errors) != 0 {
		t.Errorf("expected no errors, got %v", errors)
	}

	if !parallelExecutionDetected {
		t.Error("expected parallel execution of A and B, but none was detected")
	}

	if len(executed) != 3 {
		t.Errorf("expected 3 vertices to be executed, got %d", len(executed))
	}

	// C should be last
	if executed[2] != "C" {
		t.Errorf("expected C to be executed last, got %v", executed)
	}
}

func TestWalker_ErrorHandling(t *testing.T) {
	// Create a graph where B fails: A -> B -> C
	d := dag.NewDirectedAcyclicGraph[string]()
	_ = d.AddVertex("A", 0)
	_ = d.AddVertex("B", 1)
	_ = d.AddVertex("C", 2)
	_ = d.AddDependencies("B", []string{"A"})
	_ = d.AddDependencies("C", []string{"B"})

	var mu sync.Mutex
	executed := []string{}

	vertexFunc := func(ctx context.Context, vertexID string) error {
		mu.Lock()
		executed = append(executed, vertexID)
		mu.Unlock()

		if vertexID == "B" {
			return context.DeadlineExceeded
		}
		return nil
	}

	errors := Walk(context.Background(), d, vertexFunc, Options{})

	if len(errors) != 1 {
		t.Errorf("expected 1 error, got %d: %v", len(errors), errors)
	}

	if errors["B"] != context.DeadlineExceeded {
		t.Errorf("expected error for B to be DeadlineExceeded, got %v", errors["B"])
	}

	// C should not be executed because B failed
	for _, v := range executed {
		if v == "C" {
			t.Error("C should not have been executed because B failed")
		}
	}
}

func TestWalker_ComplexDAG(t *testing.T) {
	// Create a more complex DAG:
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

	errors := Walk(context.Background(), d, vertexFunc, Options{})

	if len(errors) != 0 {
		t.Errorf("expected no errors, got %v", errors)
	}

	if len(executed) != 5 {
		t.Errorf("expected 5 vertices to be executed, got %d", len(executed))
	}

	// Build index map
	index := make(map[string]int)
	for i, v := range executed {
		index[v] = i
	}

	// Verify dependencies are respected
	if index["C"] < index["A"] || index["C"] < index["B"] {
		t.Error("C should come after both A and B")
	}

	if index["D"] < index["B"] {
		t.Error("D should come after B")
	}

	if index["E"] < index["C"] || index["E"] < index["D"] {
		t.Error("E should come after both C and D")
	}
}

func TestWalker_NoDependencies(t *testing.T) {
	// Create a graph with no dependencies
	d := dag.NewDirectedAcyclicGraph[string]()
	_ = d.AddVertex("A", 0)
	_ = d.AddVertex("B", 1)
	_ = d.AddVertex("C", 2)

	var mu sync.Mutex
	executed := []string{}

	vertexFunc := func(ctx context.Context, vertexID string) error {
		mu.Lock()
		executed = append(executed, vertexID)
		mu.Unlock()
		time.Sleep(5 * time.Millisecond) // Allow time for parallel execution
		return nil
	}

	errors := Walk(context.Background(), d, vertexFunc, Options{})

	if len(errors) != 0 {
		t.Errorf("expected no errors, got %v", errors)
	}

	if len(executed) != 3 {
		t.Errorf("expected 3 vertices to be executed, got %d", len(executed))
	}
}

func TestWalkerWithOptions_Parallelism(t *testing.T) {
	// Create a graph with no dependencies to test parallelism limit
	d := dag.NewDirectedAcyclicGraph[string]()
	_ = d.AddVertex("A", 0)
	_ = d.AddVertex("B", 1)
	_ = d.AddVertex("C", 2)
	_ = d.AddVertex("D", 3)

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

		time.Sleep(50 * time.Millisecond)

		mu.Lock()
		currentConcurrent--
		mu.Unlock()

		return nil
	}

	opts := Options{Parallelism: 2}
	errors := Walk(context.Background(), d, vertexFunc, opts)

	if len(errors) != 0 {
		t.Errorf("expected no errors, got %v", errors)
	}

	if maxConcurrent > 2 {
		t.Errorf("expected max 2 concurrent executions, got %d", maxConcurrent)
	}
}
