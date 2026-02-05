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

import "github.com/kubernetes-sigs/kro/pkg/graph/variable"

// expressionEvaluationState tracks per-expression evaluation state.
// Expressions are cached globally and shared via pointers - if the same
// expression appears in multiple variables, they share the same state.
//
// This design mirrors the old runtime's proven caching architecture.
type expressionEvaluationState struct {
	// Expression is the CEL expression string.
	Expression string

	// Dependencies is the list of resource IDs this expression depends on.
	// All dependencies must be resolved/ready before evaluation.
	Dependencies []string

	// Kind indicates when this expression should be evaluated:
	//   - Static: at init time (only depends on schema.*)
	//   - Dynamic: when dependencies are ready
	//   - Iteration: during collection expansion
	Kind variable.ResourceVariableKind

	// Resolved indicates whether the expression has been evaluated.
	Resolved bool

	// ResolvedValue holds the cached result. Nil until Resolved=true.
	ResolvedValue any

	// EvaluationCost holds the CEL cost for evaluating this expression.
	// Cost is measured in cost units where 1,000,000 equals roughly 0.1 seconds.
	//
	// NOTE: This represents the cost of the most recent evaluation, not the
	// cumulative cost across all evaluations. When an expression is evaluated
	// multiple times (e.g., readyWhen during reconciliation loops), only the
	// last evaluation's cost is retained.
	EvaluationCost uint64
}
