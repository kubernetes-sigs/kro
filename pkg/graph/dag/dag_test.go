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
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestAddVertex_Success(t *testing.T) {
	d := NewDirectedAcyclicGraph[string]()

	err := d.AddVertex("A", 1)

	assert.NoError(t, err)
	assert.Len(t, d.Vertices, 1)
	assert.Contains(t, d.Vertices, "A")
}

func TestAddVertex_DuplicateReturnsError(t *testing.T) {
	d := NewDirectedAcyclicGraph[string]()
	_ = d.AddVertex("A", 1)

	err := d.AddVertex("A", 1)

	assert.Error(t, err)
	assert.Len(t, d.Vertices, 1)
}

func TestAddVertex_MultipleVertices(t *testing.T) {
	d := NewDirectedAcyclicGraph[string]()

	_ = d.AddVertex("A", 1)
	_ = d.AddVertex("B", 2)
	_ = d.AddVertex("C", 3)

	assert.Len(t, d.Vertices, 3)
	assert.Contains(t, d.Vertices, "A")
	assert.Contains(t, d.Vertices, "B")
	assert.Contains(t, d.Vertices, "C")
}

func TestAddDependencies_Success(t *testing.T) {
	d := NewDirectedAcyclicGraph[string]()
	_ = d.AddVertex("A", 1)
	_ = d.AddVertex("B", 2)

	err := d.AddDependencies("A", []string{"B"})

	assert.NoError(t, err)
	assert.Contains(t, d.Vertices["A"].DependsOn, "B")
}

func TestAddDependencies_NonExistentNodeReturnsError(t *testing.T) {
	d := NewDirectedAcyclicGraph[string]()
	_ = d.AddVertex("A", 1)

	err := d.AddDependencies("A", []string{"C"})

	assert.Error(t, err)
}

func TestAddDependencies_SelfReferenceReturnsError(t *testing.T) {
	d := NewDirectedAcyclicGraph[string]()
	_ = d.AddVertex("A", 1)

	err := d.AddDependencies("A", []string{"A"})

	assert.Error(t, err)
}

func TestAddDependencies_MultipleDependencies(t *testing.T) {
	d := NewDirectedAcyclicGraph[string]()
	_ = d.AddVertex("A", 1)
	_ = d.AddVertex("B", 2)
	_ = d.AddVertex("C", 3)

	err := d.AddDependencies("A", []string{"B", "C"})

	assert.NoError(t, err)
	assert.Contains(t, d.Vertices["A"].DependsOn, "B")
	assert.Contains(t, d.Vertices["A"].DependsOn, "C")
}

func TestHasCycle_NoCycle(t *testing.T) {
	d := NewDirectedAcyclicGraph[string]()
	_ = d.AddVertex("A", 1)
	_ = d.AddVertex("B", 2)
	_ = d.AddVertex("C", 3)
	_ = d.AddDependencies("A", []string{"B"})
	_ = d.AddDependencies("B", []string{"C"})

	cyclic, _ := d.hasCycle()

	assert.False(t, cyclic)
}

func TestHasCycle_DetectsCycle(t *testing.T) {
	d := NewDirectedAcyclicGraph[string]()
	_ = d.AddVertex("A", 1)
	_ = d.AddVertex("B", 2)
	_ = d.AddVertex("C", 3)
	_ = d.AddDependencies("A", []string{"B"})
	_ = d.AddDependencies("B", []string{"C"})
	// Artificially create cycle
	d.Vertices["C"].DependsOn["A"] = struct{}{}

	cyclic, _ := d.hasCycle()

	assert.True(t, cyclic)
}

func TestAddDependencies_PreventsCycle(t *testing.T) {
	d := NewDirectedAcyclicGraph[string]()
	_ = d.AddVertex("A", 1)
	_ = d.AddVertex("B", 2)
	_ = d.AddVertex("C", 3)
	_ = d.AddDependencies("A", []string{"B"})
	_ = d.AddDependencies("B", []string{"C"})

	err := d.AddDependencies("C", []string{"A"})

	assert.Error(t, err)
}

func TestTopologicalSortLevels_DetectsCycle(t *testing.T) {
	d := NewDirectedAcyclicGraph[string]()
	_ = d.AddVertex("A", 1)
	_ = d.AddVertex("B", 2)
	_ = d.AddVertex("C", 3)
	_ = d.AddDependencies("A", []string{"B"})
	_ = d.AddDependencies("B", []string{"C"})
	d.Vertices["C"].DependsOn["A"] = struct{}{}

	_, err := d.TopologicalSortLevels()

	assert.Error(t, err)
	assert.NotNil(t, AsCycleError[string](err))
}

func TestTopologicalSortLevels_SimpleChain(t *testing.T) {
	d := NewDirectedAcyclicGraph[string]()
	_ = d.AddVertex("A", 0)
	_ = d.AddVertex("B", 1)
	_ = d.AddVertex("C", 2)
	_ = d.AddDependencies("B", []string{"A"})
	_ = d.AddDependencies("C", []string{"B"})

	levels, err := d.TopologicalSortLevels()

	assert.NoError(t, err)
	assert.Len(t, levels, 3)
	assert.Equal(t, [][]string{{"A"}, {"B"}, {"C"}}, levels)
}

func TestTopologicalSortLevels_ParallelResources(t *testing.T) {
	d := NewDirectedAcyclicGraph[string]()
	_ = d.AddVertex("A", 0)
	_ = d.AddVertex("B", 1)
	_ = d.AddVertex("C", 2)
	_ = d.AddDependencies("C", []string{"A", "B"})

	levels, err := d.TopologicalSortLevels()

	assert.NoError(t, err)
	assert.Len(t, levels, 2)
	assert.Equal(t, [][]string{{"A", "B"}, {"C"}}, levels)
}

func TestTopologicalSortLevels_DiamondPattern(t *testing.T) {
	d := NewDirectedAcyclicGraph[string]()
	_ = d.AddVertex("A", 0)
	_ = d.AddVertex("B", 1)
	_ = d.AddVertex("C", 2)
	_ = d.AddVertex("D", 3)
	_ = d.AddDependencies("B", []string{"A"})
	_ = d.AddDependencies("C", []string{"A"})
	_ = d.AddDependencies("D", []string{"B", "C"})

	levels, err := d.TopologicalSortLevels()

	assert.NoError(t, err)
	assert.Len(t, levels, 3)
	assert.Equal(t, [][]string{{"A"}, {"B", "C"}, {"D"}}, levels)
}

func TestTopologicalSortLevels_NoDependencies(t *testing.T) {
	d := NewDirectedAcyclicGraph[string]()
	_ = d.AddVertex("A", 0)
	_ = d.AddVertex("B", 1)
	_ = d.AddVertex("C", 2)

	levels, err := d.TopologicalSortLevels()

	assert.NoError(t, err)
	assert.Len(t, levels, 1)
	assert.Equal(t, [][]string{{"A", "B", "C"}}, levels)
}

func TestTopologicalSortLevels_ComplexDAG(t *testing.T) {
	d := NewDirectedAcyclicGraph[string]()
	_ = d.AddVertex("A", 0)
	_ = d.AddVertex("B", 1)
	_ = d.AddVertex("C", 2)
	_ = d.AddVertex("D", 3)
	_ = d.AddVertex("E", 4)
	_ = d.AddVertex("F", 5)
	_ = d.AddDependencies("C", []string{"A", "B"})
	_ = d.AddDependencies("D", []string{"C"})
	_ = d.AddDependencies("E", []string{"C"})
	_ = d.AddDependencies("F", []string{"D", "E"})

	levels, err := d.TopologicalSortLevels()

	assert.NoError(t, err)
	assert.Len(t, levels, 4)
	assert.Equal(t, [][]string{{"A", "B"}, {"C"}, {"D", "E"}, {"F"}}, levels)
}

func TestTopologicalSortLevels_PreservesOrder(t *testing.T) {
	d := NewDirectedAcyclicGraph[string]()
	_ = d.AddVertex("Z", 0)
	_ = d.AddVertex("Y", 1)
	_ = d.AddVertex("X", 2)
	_ = d.AddVertex("W", 3)
	_ = d.AddVertex("V", 4)
	_ = d.AddVertex("U", 5)
	_ = d.AddDependencies("U", []string{"Z", "Y", "X"})

	levels, err := d.TopologicalSortLevels()

	assert.NoError(t, err)
	assert.Len(t, levels, 2)
	assert.Equal(t, []string{"Z", "Y", "X", "W", "V"}, levels[0])
	assert.Equal(t, []string{"U"}, levels[1])
}

func TestTopologicalSortLevels_NoInterLevelDependencies(t *testing.T) {
	d := NewDirectedAcyclicGraph[string]()
	_ = d.AddVertex("A", 0)
	_ = d.AddVertex("B", 1)
	_ = d.AddVertex("C", 2)
	_ = d.AddVertex("D", 3)
	_ = d.AddDependencies("C", []string{"A"})
	_ = d.AddDependencies("D", []string{"B"})

	levels, err := d.TopologicalSortLevels()

	assert.NoError(t, err)
	assert.Len(t, levels, 2)

	// Verify nodes in same level have no dependencies on each other
	for _, level := range levels {
		for _, node := range level {
			for _, otherNode := range level {
				if node != otherNode {
					assert.NotContains(t, d.Vertices[node].DependsOn, otherNode)
				}
			}
		}
	}
}

func TestTopologicalSortLevels_DependenciesInEarlierLevels(t *testing.T) {
	d := NewDirectedAcyclicGraph[string]()
	_ = d.AddVertex("A", 0)
	_ = d.AddVertex("B", 1)
	_ = d.AddVertex("C", 2)
	_ = d.AddDependencies("B", []string{"A"})
	_ = d.AddDependencies("C", []string{"B"})

	levels, err := d.TopologicalSortLevels()

	assert.NoError(t, err)
	assert.Len(t, levels, 3)

	// Build level map
	nodeLevel := make(map[string]int)
	for i, level := range levels {
		for _, node := range level {
			nodeLevel[node] = i
		}
	}

	// Verify all dependencies appear in earlier levels
	for _, level := range levels {
		for _, node := range level {
			for dep := range d.Vertices[node].DependsOn {
				assert.Less(t, nodeLevel[dep], nodeLevel[node])
			}
		}
	}
}
