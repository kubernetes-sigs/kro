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

package runtime

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	krocel "github.com/kubernetes-sigs/kro/pkg/cel"
	"github.com/kubernetes-sigs/kro/pkg/graph"
	"github.com/kubernetes-sigs/kro/pkg/graph/variable"
)

func TestFilterDependencies(t *testing.T) {
	tests := []struct {
		name         string
		node         *graph.Node
		instanceSpec map[string]any
		declaredDeps []string
		expected     []string
		expectError  bool
	}{
		{
			name:         "ternary - true branch",
			node:         nodeWithExpression("schema.spec.useDB ? db.host : pvc.name"),
			instanceSpec: map[string]any{"spec": map[string]any{"useDB": true}},
			declaredDeps: []string{"db", "pvc"},
			expected:     []string{"db"},
		},
		{
			name:         "ternary - false branch",
			node:         nodeWithExpression("schema.spec.useDB ? db.host : pvc.name"),
			instanceSpec: map[string]any{"spec": map[string]any{"useDB": false}},
			declaredDeps: []string{"db", "pvc"},
			expected:     []string{"pvc"},
		},
		{
			name:         "nested ternary",
			node:         nodeWithExpression("schema.spec.useDB ? (schema.spec.isReplicated ? primary.host : single.host) : pvc.name"),
			instanceSpec: map[string]any{"spec": map[string]any{"useDB": true, "isReplicated": false}},
			declaredDeps: []string{"primary", "single", "pvc"},
			expected:     []string{"single"},
		},
		{
			name: "multiple expressions - both used",
			node: nodeWithExpressions([]string{
				"db.host",
				"cache.endpoint",
			}),
			instanceSpec: map[string]any{},
			declaredDeps: []string{"db", "cache", "pvc"},
			expected:     []string{"db", "cache"},
		},
		{
			name:         "no conditionals - all deps used",
			node:         nodeWithExpression("db.host + ':' + db.port"),
			instanceSpec: map[string]any{},
			declaredDeps: []string{"db"},
			expected:     []string{"db"},
		},
		{
			name:         "forEach iterator with conditional",
			node:         nodeWithForEach("schema.spec.useDB ? db.replicas : [1, 2, 3]"),
			instanceSpec: map[string]any{"spec": map[string]any{"useDB": true}},
			declaredDeps: []string{"db", "pvc"},
			expected:     []string{"db"},
		},
		{
			name:         "all dependencies filtered out",
			node:         nodeWithExpression("schema.spec.constant"),
			instanceSpec: map[string]any{"spec": map[string]any{"constant": "value"}},
			declaredDeps: []string{"db", "pvc"},
			expected:     []string{}, // no resource deps, only schema
		},
		{
			name:         "complex nested expression",
			node:         nodeWithExpression("schema.spec.tier == 'prod' ? (schema.spec.ha ? primary.host : single.host) : dev.host"),
			instanceSpec: map[string]any{"spec": map[string]any{"tier": "prod", "ha": true}},
			declaredDeps: []string{"primary", "single", "dev"},
			expected:     []string{"primary"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filtered, err := filterDependencies(tt.node, tt.instanceSpec, tt.declaredDeps)

			if tt.expectError {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.ElementsMatch(t, tt.expected, filtered, "filtered dependencies should match expected")
		})
	}
}

func TestShouldOptimizeDependencies(t *testing.T) {
	tests := []struct {
		name     string
		node     *graph.Node
		expected bool
	}{
		{
			name: "has non-schema deps - should optimize",
			node: &graph.Node{
				Meta: graph.NodeMeta{
					Dependencies: []string{"db", "pvc"},
				},
				Variables: []*variable.ResourceField{
					{
						FieldDescriptor: variable.FieldDescriptor{
							Expression: &krocel.Expression{
								Original:   "schema.useDB ? db.host : pvc.name",
								References: []string{"schema", "db", "pvc"},
							},
						},
					},
				},
			},
			expected: true,
		},
		{
			name: "only schema deps - skip optimization",
			node: &graph.Node{
				Meta: graph.NodeMeta{
					Dependencies: []string{},
				},
				Variables: []*variable.ResourceField{
					{
						FieldDescriptor: variable.FieldDescriptor{
							Expression: &krocel.Expression{
								Original:   "schema.appName",
								References: []string{"schema"},
							},
						},
					},
				},
			},
			expected: false,
		},
		{
			name: "has non-schema deps without schema refs - should optimize",
			node: &graph.Node{
				Meta: graph.NodeMeta{
					Dependencies: []string{"db"},
				},
				Variables: []*variable.ResourceField{
					{
						FieldDescriptor: variable.FieldDescriptor{
							Expression: &krocel.Expression{
								Original:   "db.host",
								References: []string{"db"},
							},
						},
					},
				},
			},
			expected: true,
		},
		{
			name: "has non-schema deps with forEach - should optimize",
			node: &graph.Node{
				Meta: graph.NodeMeta{
					Dependencies: []string{"db"},
				},
				ForEach: []graph.ForEachDimension{
					{
						Name: "item",
						Expression: &krocel.Expression{
							Original:   "schema.items",
							References: []string{"schema"},
						},
					},
				},
			},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := shouldOptimizeDependencies(tt.node)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestPartialEvaluatorSimplify(t *testing.T) {
	tests := []struct {
		name         string
		expression   string
		instanceSpec map[string]any
		resourceIDs  []string
		expected     string
	}{
		{
			name:         "simple ternary - true",
			expression:   "schema.spec.useDB ? db.host : pvc.name",
			instanceSpec: map[string]any{"spec": map[string]any{"useDB": true}},
			resourceIDs:  []string{"db", "pvc"},
			expected:     "db.host",
		},
		{
			name:         "simple ternary - false",
			expression:   "schema.spec.useDB ? db.host : pvc.name",
			instanceSpec: map[string]any{"spec": map[string]any{"useDB": false}},
			resourceIDs:  []string{"db", "pvc"},
			expected:     "pvc.name",
		},
		{
			name:         "nested ternary - requires iteration",
			expression:   "schema.spec.a ? (schema.spec.b ? x.val : y.val) : z.val",
			instanceSpec: map[string]any{"spec": map[string]any{"a": true, "b": false}},
			resourceIDs:  []string{"x", "y", "z"},
			expected:     "y.val",
		},
		{
			name:         "no schema references - unchanged",
			expression:   "db.host + ':' + db.port",
			instanceSpec: map[string]any{},
			resourceIDs:  []string{"db"},
			expected:     "db.host + \":\" + db.port", // CEL may normalize quotes
		},
		{
			name:         "comparison operator",
			expression:   "schema.spec.tier == 'prod' ? primary.host : dev.host",
			instanceSpec: map[string]any{"spec": map[string]any{"tier": "prod"}},
			resourceIDs:  []string{"primary", "dev"},
			expected:     "primary.host",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env, err := krocel.DefaultEnvironment(
				krocel.WithResourceIDs(append([]string{graph.SchemaVarName}, tt.resourceIDs...)),
			)
			require.NoError(t, err)

			pe, err := newPartialEvaluator(env, tt.instanceSpec, tt.resourceIDs)
			require.NoError(t, err)

			simplified, err := pe.simplifyExpression(tt.expression)
			require.NoError(t, err)

			assert.Equal(t, tt.expected, simplified)
		})
	}
}

// Helper functions to create test nodes

func nodeWithExpression(expr string) *graph.Node {
	return &graph.Node{
		Meta: graph.NodeMeta{
			ID:           "test-node",
			Dependencies: []string{"schema", "db", "pvc", "primary", "single", "cache", "dev"},
		},
		Variables: []*variable.ResourceField{
			{
				FieldDescriptor: variable.FieldDescriptor{
					Path: "test.field",
					Expression: &krocel.Expression{
						Original:   expr,
						References: []string{"schema", "db", "pvc", "primary", "single", "cache", "dev"},
					},
				},
			},
		},
	}
}

func nodeWithExpressions(exprs []string) *graph.Node {
	vars := make([]*variable.ResourceField, len(exprs))
	for i, expr := range exprs {
		vars[i] = &variable.ResourceField{
			FieldDescriptor: variable.FieldDescriptor{
				Path: "test.field",
				Expression: &krocel.Expression{
					Original:   expr,
					References: []string{"schema", "db", "cache", "pvc"},
				},
			},
		}
	}
	return &graph.Node{
		Meta: graph.NodeMeta{
			ID:           "test-node",
			Dependencies: []string{"schema", "db", "cache", "pvc"},
		},
		Variables: vars,
	}
}

func nodeWithForEach(iterExpr string) *graph.Node {
	return &graph.Node{
		Meta: graph.NodeMeta{
			ID:           "test-node",
			Dependencies: []string{"schema", "db", "pvc"},
		},
		ForEach: []graph.ForEachDimension{
			{
				Name: "item",
				Expression: &krocel.Expression{
					Original:   iterExpr,
					References: []string{"schema", "db", "pvc"},
				},
			},
		},
	}
}
