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

	"github.com/kubernetes-sigs/kro/pkg/graph/dag"
)

func TestWalker_ReverseSimpleChain(t *testing.T) {
	// Create a simple chain: A -> B -> C
	// In reverse order, should execute: C, B, A
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

	errors := Walk(context.Background(), d, vertexFunc, Options{Reverse: true})

	if len(errors) != 0 {
		t.Errorf("expected no errors, got %v", errors)
	}

	if len(executed) != 3 {
		t.Errorf("expected 3 vertices executed, got %d", len(executed))
	}

	// Verify execution order respects reverse dependencies
	// C must execute before B, B must execute before A
	indexC := indexOf(executed, "C")
	indexB := indexOf(executed, "B")
	indexA := indexOf(executed, "A")

	if indexC == -1 || indexB == -1 || indexA == -1 {
		t.Errorf("not all vertices were executed: %v", executed)
	}

	if indexC > indexB {
		t.Errorf("C should execute before B in reverse order, got order: %v", executed)
	}

	if indexB > indexA {
		t.Errorf("B should execute before A in reverse order, got order: %v", executed)
	}
}

func TestWalker_ReverseDiamond(t *testing.T) {
	// Create diamond pattern:
	//     A
	//    / \
	//   B   C
	//    \ /
	//     D
	// In reverse: D, then B and C in parallel, then A
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

	errors := Walk(context.Background(), d, vertexFunc, Options{Reverse: true})

	if len(errors) != 0 {
		t.Errorf("expected no errors, got %v", errors)
	}

	if len(executed) != 4 {
		t.Errorf("expected 4 vertices executed, got %d", len(executed))
	}

	// Verify D executes first
	if executed[0] != "D" {
		t.Errorf("D should execute first in reverse order, got: %v", executed)
	}

	// Verify A executes last
	if executed[3] != "A" {
		t.Errorf("A should execute last in reverse order, got: %v", executed)
	}

	// B and C should execute after D but before A
	indexB := indexOf(executed, "B")
	indexC := indexOf(executed, "C")

	if indexB < 1 || indexB > 2 {
		t.Errorf("B should execute in middle positions, got position %d in: %v", indexB, executed)
	}

	if indexC < 1 || indexC > 2 {
		t.Errorf("C should execute in middle positions, got position %d in: %v", indexC, executed)
	}
}

func TestWalker_ReverseWithFailure(t *testing.T) {
	// Create a chain: A -> B -> C
	// Fail B in reverse order, A should not execute
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
			return context.Canceled // Use any error
		}
		return nil
	}

	errors := Walk(context.Background(), d, vertexFunc, Options{Reverse: true})

	if len(errors) == 0 {
		t.Error("expected errors, got none")
	}

	if _, exists := errors["B"]; !exists {
		t.Error("expected B to have an error")
	}

	// C should execute (it has no reverse dependencies)
	if indexOf(executed, "C") == -1 {
		t.Error("C should have executed")
	}

	// B should execute (even though it failed)
	if indexOf(executed, "B") == -1 {
		t.Error("B should have executed (and failed)")
	}

	// A should NOT execute (B failed, and A depends on B in reverse)
	if indexOf(executed, "A") != -1 {
		t.Error("A should not have executed due to B's failure")
	}
}

// indexOf returns the index of item in slice, or -1 if not found
func indexOf(slice []string, item string) int {
	for i, v := range slice {
		if v == item {
			return i
		}
	}
	return -1
}
