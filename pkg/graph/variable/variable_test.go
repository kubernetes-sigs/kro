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

func TestFieldDescriptor(t *testing.T) {
	tests := []struct {
		name           string
		path           string
		expressions    []string
		expectedTypes  []string
		schema         *spec.Schema
		standalone     bool
		expectedResult FieldDescriptor
	}{
		{
			name:          "basic field descriptor",
			path:          "spec.replicas",
			expressions:   []string{"${schema.spec.replicas + 5}"},
			expectedTypes: []string{"integer"},
			schema:        nil,
			standalone:    true,
			expectedResult: FieldDescriptor{
				Path:                 "spec.replicas",
				Expressions:          []string{"${schema.spec.replicas + 5}"},
				ExpectedTypes:        []string{"integer"},
				ExpectedSchema:       nil,
				StandaloneExpression: true,
			},
		},
		{
			name:          "field descriptor with schema",
			path:          "spec.template",
			expressions:   []string{"${schema.spec.template}"},
			expectedTypes: []string{"object"},
			schema:        &spec.Schema{},
			standalone:    true,
			expectedResult: FieldDescriptor{
				Path:                 "spec.template",
				Expressions:          []string{"${schema.spec.template}"},
				ExpectedTypes:        []string{"object"},
				ExpectedSchema:       &spec.Schema{},
				StandaloneExpression: true,
			},
		},
		{
			name:          "field descriptor with multiple expressions",
			path:          "spec.name",
			expressions:   []string{"${prefix}", "${suffix}"},
			expectedTypes: []string{"string"},
			schema:        nil,
			standalone:    false,
			expectedResult: FieldDescriptor{
				Path:                 "spec.name",
				Expressions:          []string{"${prefix}", "${suffix}"},
				ExpectedTypes:        []string{"string"},
				ExpectedSchema:       nil,
				StandaloneExpression: false,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fd := FieldDescriptor{
				Path:                 tc.path,
				Expressions:          tc.expressions,
				ExpectedTypes:        tc.expectedTypes,
				ExpectedSchema:       tc.schema,
				StandaloneExpression: tc.standalone,
			}

			assert.Equal(t, tc.expectedResult.Path, fd.Path)
			assert.Equal(t, tc.expectedResult.Expressions, fd.Expressions)
			assert.Equal(t, tc.expectedResult.ExpectedTypes, fd.ExpectedTypes)
			assert.Equal(t, tc.expectedResult.ExpectedSchema, fd.ExpectedSchema)
			assert.Equal(t, tc.expectedResult.StandaloneExpression, fd.StandaloneExpression)

			if fd.ExpectedSchema != nil {
				assert.NotNil(t, fd.ExpectedSchema, "Schema should not be nil when expected")
			}
		})
	}
}
func TestResourceField(t *testing.T) {
	tests := []struct {
		name              string
		fieldDescriptor   FieldDescriptor
		kind              ResourceVariableKind
		initialDeps       []string
		additionalDeps    []string
		expectedKind      ResourceVariableKind
		expectedFinalDeps []string
	}{
		{
			name: "static resource field",
			fieldDescriptor: FieldDescriptor{
				Path:        "spec.replicas",
				Expressions: []string{"${schema.spec.replicas + 5}"},
			},
			kind:              ResourceVariableKindStatic,
			initialDeps:       []string{"instance"},
			additionalDeps:    []string{"schema"},
			expectedKind:      ResourceVariableKindStatic,
			expectedFinalDeps: []string{"instance", "schema"},
		},
		{
			name: "dynamic resource field",
			fieldDescriptor: FieldDescriptor{
				Path:        "spec.vpcId",
				Expressions: []string{"${vpc.status.vpcId}"},
			},
			kind:              ResourceVariableKindDynamic,
			initialDeps:       []string{"vpc"},
			additionalDeps:    []string{"instance"},
			expectedKind:      ResourceVariableKindDynamic,
			expectedFinalDeps: []string{"vpc", "instance"},
		},
		{
			name: "readyWhen resource field",
			fieldDescriptor: FieldDescriptor{
				Path:        "readyWhen",
				Expressions: []string{"${cluster.status.status == \"Active\"}"},
			},
			kind:              ResourceVariableKindReadyWhen,
			initialDeps:       []string{"cluster"},
			additionalDeps:    []string{"cluster", "another-dep"}, // Test deduplication
			expectedKind:      ResourceVariableKindReadyWhen,
			expectedFinalDeps: []string{"cluster", "another-dep"},
		},
		{
			name: "includeWhen resource field",
			fieldDescriptor: FieldDescriptor{
				Path:        "includeWhen",
				Expressions: []string{"${schema.spec.replicas > 1}"},
			},
			kind:              ResourceVariableKindIncludeWhen,
			initialDeps:       []string{"instance"},
			additionalDeps:    []string{},
			expectedKind:      ResourceVariableKindIncludeWhen,
			expectedFinalDeps: []string{"instance"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			field := ResourceField{
				FieldDescriptor: tc.fieldDescriptor,
				Kind:            tc.kind,
				Dependencies:    tc.initialDeps,
			}

			// Verifying initial state
			assert.Equal(t, tc.kind, field.Kind)
			assert.ElementsMatch(t, tc.initialDeps, field.Dependencies)

			field.AddDependencies(tc.additionalDeps...)

			// Verifying final state
			assert.Equal(t, tc.expectedKind, field.Kind)
			assert.ElementsMatch(t, tc.expectedFinalDeps, field.Dependencies)

			// Verifying behavior methods
			assert.Equal(t, tc.kind.IsStatic(), field.Kind.IsStatic())
			assert.Equal(t, tc.kind.IsDynamic(), field.Kind.IsDynamic())
			assert.Equal(t, tc.kind.IsIncludeWhen(), field.Kind.IsIncludeWhen())
			assert.Equal(t, string(tc.kind), field.Kind.String())
		})
	}
}
