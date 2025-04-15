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

package graph

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestResource_Dependencies(t *testing.T) {
	tests := []struct {
		name         string
		dependencies []string
		checkDep     string
		hasDep       bool
		addDeps      []string
		finalDeps    []string
	}{
		{
			name:         "empty dependencies",
			dependencies: []string{},
			checkDep:     "test",
			hasDep:       false,
			addDeps:      []string{"test1", "test2"},
			finalDeps:    []string{"test1", "test2"},
		},
		{
			name:         "existing dependency",
			dependencies: []string{"test1", "test2"},
			checkDep:     "test1",
			hasDep:       true,
			addDeps:      []string{"test3", "test1"}, // test1 is duplicate
			finalDeps:    []string{"test1", "test2", "test3"},
		},
		{
			name:         "multiple additions",
			dependencies: []string{"test1"},
			checkDep:     "test3",
			hasDep:       false,
			addDeps:      []string{"test2", "test3", "test4"},
			finalDeps:    []string{"test1", "test2", "test3", "test4"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &Resource{
				dependencies: tt.dependencies,
			}

			// Test HasDependency
			assert.Equal(t, tt.hasDep, r.HasDependency(tt.checkDep))

			// Test AddDependencies
			r.addDependencies(tt.addDeps...)

			// Verify final dependencies
			assert.ElementsMatch(t, tt.finalDeps, r.GetDependencies())
		})
	}
}
