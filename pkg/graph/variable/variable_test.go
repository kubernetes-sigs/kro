// Copyright 2025 The Kube Resource Orchestrator Authors
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

package variable

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestResourceFieldAddDependencies(t *testing.T) {
	tests := []struct {
		name              string
		initialDeps       []string
		depsToAdd         []string
		expectedFinalDeps []string
	}{
		{
			name:              "add new dependencies",
			initialDeps:       []string{"resource1", "resource2"},
			depsToAdd:         []string{"resource3", "resource4"},
			expectedFinalDeps: []string{"resource1", "resource2", "resource3", "resource4"},
		},
		{
			name:              "add duplicate dependencies",
			initialDeps:       []string{"resource1", "resource2"},
			depsToAdd:         []string{"resource2", "resource3"},
			expectedFinalDeps: []string{"resource1", "resource2", "resource3"},
		},
		{
			name:              "add to empty dependencies",
			initialDeps:       []string{},
			depsToAdd:         []string{"resource1", "resource2"},
			expectedFinalDeps: []string{"resource1", "resource2"},
		},
		{
			name:              "add empty dependencies",
			initialDeps:       []string{"resource1", "resource2"},
			depsToAdd:         []string{},
			expectedFinalDeps: []string{"resource1", "resource2"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rf := ResourceField{
				Dependencies: tc.initialDeps,
			}

			rf.AddDependencies(tc.depsToAdd...)

			assert.ElementsMatch(t, tc.expectedFinalDeps, rf.Dependencies)

			seen := make(map[string]bool)
			for _, dep := range rf.Dependencies {
				assert.False(t, seen[dep], "Duplicate dependency found: %s", dep)
				seen[dep] = true
			}
		})
	}
}
