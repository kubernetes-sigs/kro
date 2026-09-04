// Copyright 2026 The Kubernetes Authors.
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

package graph

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/kubernetes-sigs/kro/pkg/dag"
)

func TestApplyOrdersForDAG(t *testing.T) {
	dependencyGraph := dag.NewDirectedAcyclicGraph[string]()
	for i, nodeID := range []string{"a", "b", "c", "d"} {
		require.NoError(t, dependencyGraph.AddVertex(nodeID, i))
	}
	require.NoError(t, dependencyGraph.AddDependencies("b", []string{"a"}))
	require.NoError(t, dependencyGraph.AddDependencies("c", []string{"a"}))
	require.NoError(t, dependencyGraph.AddDependencies("d", []string{"b", "c"}))

	orders, err := applyOrdersForDAG(dependencyGraph)
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"a": 1, "b": 2, "c": 2, "d": 3}, orders)
}
