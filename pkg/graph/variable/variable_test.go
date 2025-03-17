// Copyright 2025 The Kube Resource Orchestrator Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package variable

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/kube-openapi/pkg/validation/spec"
)

func TestResourceVariableKind(t *testing.T) {
	tests := []struct {
		name          string
		kind          ResourceVariableKind
		expectedStr   string
		isStatic      bool
		isDynamic     bool
		isIncludeWhen bool
	}{
		{
			name:          "static kind",
			kind:          ResourceVariableKindStatic,
			expectedStr:   "static",
			isStatic:      true,
			isDynamic:     false,
			isIncludeWhen: false,
		},
		{
			name:          "dynamic kind",
			kind:          ResourceVariableKindDynamic,
			expectedStr:   "dynamic",
			isStatic:      false,
			isDynamic:     true,
			isIncludeWhen: false,
		},
		{
			name:          "readyWhen kind",
			kind:          ResourceVariableKindReadyWhen,
			expectedStr:   "readyWhen",
			isStatic:      false,
			isDynamic:     false,
			isIncludeWhen: false,
		},
		{
			name:          "includeWhen kind",
			kind:          ResourceVariableKindIncludeWhen,
			expectedStr:   "includeWhen",
			isStatic:      false,
			isDynamic:     false,
			isIncludeWhen: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Test String method
			assert.Equal(t, tc.expectedStr, tc.kind.String())

			// Test IsStatic method
			assert.Equal(t, tc.isStatic, tc.kind.IsStatic())

			// Test IsDynamic method
			assert.Equal(t, tc.isDynamic, tc.kind.IsDynamic())

			// Test IsIncludeWhen method
			assert.Equal(t, tc.isIncludeWhen, tc.kind.IsIncludeWhen())
		})
	}
}

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
			// Create a ResourceField with initial dependencies
			rf := ResourceField{
				Dependencies: tc.initialDeps,
			}

			// Add dependencies
			rf.AddDependencies(tc.depsToAdd...)

			// Check that the dependencies are as expected
			assert.ElementsMatch(t, tc.expectedFinalDeps, rf.Dependencies)

			// Also verify no duplicates
			seen := make(map[string]bool)
			for _, dep := range rf.Dependencies {
				// Should not have seen this dependency before
				assert.False(t, seen[dep], "Duplicate dependency found: %s", dep)
				seen[dep] = true
			}
		})
	}
}

func TestFieldDescriptor(t *testing.T) {
	tests := []struct {
		name           string
		fieldDesc      FieldDescriptor
		expectedPath   string
		expectedExprs  []string
		expectedTypes  []string
		expectedSchema *spec.Schema
		standalone     bool
	}{
		{
			name: "basic field descriptor",
			fieldDesc: FieldDescriptor{
				Path:                 "spec.replicas",
				Expressions:          []string{"${schema.spec.replicas + 5}"},
				ExpectedTypes:        []string{"integer"},
				ExpectedSchema:       nil,
				StandaloneExpression: true,
			},
			expectedPath:   "spec.replicas",
			expectedExprs:  []string{"${schema.spec.replicas + 5}"},
			expectedTypes:  []string{"integer"},
			expectedSchema: nil,
			standalone:     true,
		},
		{
			name: "field descriptor with schema",
			fieldDesc: FieldDescriptor{
				Path:                 "spec.template",
				Expressions:          []string{"${schema.spec.template}"},
				ExpectedTypes:        []string{"object"},
				ExpectedSchema:       &spec.Schema{},
				StandaloneExpression: true,
			},
			expectedPath:   "spec.template",
			expectedExprs:  []string{"${schema.spec.template}"},
			expectedTypes:  []string{"object"},
			expectedSchema: &spec.Schema{},
			standalone:     true,
		},
		{
			name: "field descriptor with multiple expressions",
			fieldDesc: FieldDescriptor{
				Path:                 "spec.name",
				Expressions:          []string{"${prefix}", "${suffix}"},
				ExpectedTypes:        []string{"string"},
				ExpectedSchema:       nil,
				StandaloneExpression: false,
			},
			expectedPath:   "spec.name",
			expectedExprs:  []string{"${prefix}", "${suffix}"},
			expectedTypes:  []string{"string"},
			expectedSchema: nil,
			standalone:     false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expectedPath, tc.fieldDesc.Path)
			assert.Equal(t, tc.expectedExprs, tc.fieldDesc.Expressions)
			assert.Equal(t, tc.expectedTypes, tc.fieldDesc.ExpectedTypes)
			assert.Equal(t, tc.expectedSchema, tc.fieldDesc.ExpectedSchema)
			assert.Equal(t, tc.standalone, tc.fieldDesc.StandaloneExpression)
		})
	}
}

func TestResourceField(t *testing.T) {
	tests := []struct {
		name          string
		resourceField ResourceField
		expectedKind  ResourceVariableKind
		expectedDeps  []string
	}{
		{
			name: "static resource field",
			resourceField: ResourceField{
				FieldDescriptor: FieldDescriptor{
					Path:        "spec.replicas",
					Expressions: []string{"${schema.spec.replicas + 5}"},
				},
				Kind:         ResourceVariableKindStatic,
				Dependencies: []string{"instance"},
			},
			expectedKind: ResourceVariableKindStatic,
			expectedDeps: []string{"instance"},
		},
		{
			name: "dynamic resource field",
			resourceField: ResourceField{
				FieldDescriptor: FieldDescriptor{
					Path:        "spec.vpcId",
					Expressions: []string{"${vpc.status.vpcId}"},
				},
				Kind:         ResourceVariableKindDynamic,
				Dependencies: []string{"vpc"},
			},
			expectedKind: ResourceVariableKindDynamic,
			expectedDeps: []string{"vpc"},
		},
		{
			name: "readyWhen resource field",
			resourceField: ResourceField{
				FieldDescriptor: FieldDescriptor{
					Path:        "readyWhen",
					Expressions: []string{"${cluster.status.status == \"Active\"}"},
				},
				Kind:         ResourceVariableKindReadyWhen,
				Dependencies: []string{"cluster"},
			},
			expectedKind: ResourceVariableKindReadyWhen,
			expectedDeps: []string{"cluster"},
		},
		{
			name: "includeWhen resource field",
			resourceField: ResourceField{
				FieldDescriptor: FieldDescriptor{
					Path:        "includeWhen",
					Expressions: []string{"${schema.spec.replicas > 1}"},
				},
				Kind:         ResourceVariableKindIncludeWhen,
				Dependencies: []string{"instance"},
			},
			expectedKind: ResourceVariableKindIncludeWhen,
			expectedDeps: []string{"instance"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expectedKind, tc.resourceField.Kind)
			assert.Equal(t, tc.expectedDeps, tc.resourceField.Dependencies)
		})
	}
}
