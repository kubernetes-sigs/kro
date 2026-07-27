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
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TestVertexString checks the human-readable rendering of a vertex,
// including its dependency list. DependsOn iteration order is not
// deterministic, so for the multi-dependency case we assert on the
// structural pieces rather than an exact string.
func TestVertexString(t *testing.T) {
	tests := []struct {
		name      string
		vertex    Vertex[string]
		wantParts []string
	}{
		{
			name:      "no dependencies",
			vertex:    Vertex[string]{ID: "A", Order: 0, DependsOn: map[string]struct{}{}},
			wantParts: []string{"Vertex[ID: A, Order: 0, DependsOn: ]"},
		},
		{
			name:      "single dependency",
			vertex:    Vertex[string]{ID: "B", Order: 2, DependsOn: map[string]struct{}{"A": {}}},
			wantParts: []string{"Vertex[ID: B, Order: 2, DependsOn: A]"},
		},
		{
			name:      "multiple dependencies",
			vertex:    Vertex[string]{ID: "C", Order: 3, DependsOn: map[string]struct{}{"A": {}, "B": {}}},
			wantParts: []string{"Vertex[ID: C, Order: 3, DependsOn:", "A", "B", ","},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.vertex.String()
			for _, part := range tc.wantParts {
				if !strings.Contains(got, part) {
					t.Errorf("String() = %q, expected to contain %q", got, part)
				}
			}
		})
	}
}

// TestCycleErrorError checks the error string and that formatCycle joins
// the cycle path with arrows.
func TestCycleErrorError(t *testing.T) {
	tests := []struct {
		name  string
		cycle []string
		want  string
	}{
		{
			name:  "empty cycle",
			cycle: nil,
			want:  "graph contains a cycle: ",
		},
		{
			name:  "single node",
			cycle: []string{"A"},
			want:  "graph contains a cycle: A",
		},
		{
			name:  "multi node",
			cycle: []string{"A", "B", "C", "A"},
			want:  "graph contains a cycle: A -> B -> C -> A",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := &CycleError[string]{Cycle: tc.cycle}
			if got := err.Error(); got != tc.want {
				t.Errorf("Error() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestAsCycleError verifies extraction of a CycleError from a (possibly
// wrapped) error, and that non-cycle errors return nil.
func TestAsCycleError(t *testing.T) {
	cycleErr := &CycleError[string]{Cycle: []string{"A", "B", "A"}}

	tests := []struct {
		name    string
		err     error
		wantNil bool
	}{
		{name: "direct cycle error", err: cycleErr, wantNil: false},
		{name: "wrapped cycle error", err: fmt.Errorf("context: %w", cycleErr), wantNil: false},
		{name: "plain error", err: errors.New("not a cycle"), wantNil: true},
		{name: "nil error", err: nil, wantNil: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := AsCycleError[string](tc.err)
			if tc.wantNil && got != nil {
				t.Errorf("AsCycleError() = %v, want nil", got)
			}
			if !tc.wantNil && got == nil {
				t.Errorf("AsCycleError() = nil, want non-nil")
			}
		})
	}
}

// TestTopoHeapLess covers the heap ordering, including the ID tiebreak that
// applies when two items share the same Order.
func TestTopoHeapLess(t *testing.T) {
	tests := []struct {
		name string
		i    topoHeapItem[string]
		j    topoHeapItem[string]
		want bool
	}{
		{
			name: "lower order wins",
			i:    topoHeapItem[string]{ID: "B", Order: 1},
			j:    topoHeapItem[string]{ID: "A", Order: 2},
			want: true,
		},
		{
			name: "higher order loses",
			i:    topoHeapItem[string]{ID: "A", Order: 3},
			j:    topoHeapItem[string]{ID: "B", Order: 2},
			want: false,
		},
		{
			name: "equal order falls back to ID - lower ID wins",
			i:    topoHeapItem[string]{ID: "A", Order: 5},
			j:    topoHeapItem[string]{ID: "B", Order: 5},
			want: true,
		},
		{
			name: "equal order falls back to ID - higher ID loses",
			i:    topoHeapItem[string]{ID: "B", Order: 5},
			j:    topoHeapItem[string]{ID: "A", Order: 5},
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := topoHeap[string]{tc.i, tc.j}
			if got := h.Less(0, 1); got != tc.want {
				t.Errorf("Less() = %v, want %v", got, tc.want)
			}
		})
	}
}
