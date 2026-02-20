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
	krocel "github.com/kubernetes-sigs/kro/pkg/cel"
	"github.com/kubernetes-sigs/kro/pkg/graph/variable"
)

// expressionEvaluationState tracks per-expression evaluation state.
// Expressions are cached globally and shared via pointers - if the same
// expression appears in multiple variables, they share the same state.
//
// This design mirrors the old runtime's proven caching architecture.
type expressionEvaluationState struct {
	// Expression holds the CEL expression with its pre-compiled Program.
	// The Program was compiled at graph build time and is reused here.
	Expression *krocel.Expression

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
}
