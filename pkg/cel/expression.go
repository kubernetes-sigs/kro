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

package cel

import (
	"fmt"

	"github.com/google/cel-go/cel"
)

// Expression wraps a CEL expression with its compiled program and metadata.
// Programs are compiled once at graph build time and reused across reconciliations.
// The struct is immutable and thread-safe after construction. It is save to use
// by multiple runtimes and reconciliations in parallel - thanks to Program being
// thread-safe.
//
// Lifecycle:
//   - Parser: creates with Original set (References and Program nil)
//   - Builder: populates References during dependency extraction (via ast.Inspector)
//   - Builder: populates Program during compilation (after type validation)
//   - Runtime: calls Eval() with context containing values for References
type Expression struct {
	// Original is the raw CEL expression string, preserved for error messages
	// and debugging. Set by parser.
	Original string

	// References lists all identifiers this expression accesses (e.g., "schema", "vpc").
	// These are the keys that must be present in the context passed to Eval.
	// Set by builder during dependency extraction.
	//
	// Note: References includes "schema" if used, but schema is NOT a DAG dependency.
	// DAG dependencies are tracked separately at Node.Meta.Dependencies.
	References []string

	// Program is the compiled CEL program. Set by builder after type validation.
	// It is stateless and thread-safe, allowing concurrent evaluation.
	Program cel.Program
}

// NewUncompiled creates an uncompiled Expression with only Original set.
// Use this in parser/tests where References and Program are set later by builder.
func NewUncompiled(expr string) *Expression {
	return &Expression{Original: expr}
}

// NewUncompiledSlice creates a slice of uncompiled Expressions from strings.
// Use this in parser/tests for multi-expression fields like string templates.
func NewUncompiledSlice(exprs ...string) []*Expression {
	result := make([]*Expression, len(exprs))
	for i, expr := range exprs {
		result[i] = &Expression{Original: expr}
	}
	return result
}

// Eval evaluates the compiled expression and returns the result.
func (e *Expression) Eval(ctx map[string]any) (any, error) {
	out, _, err := e.Program.Eval(ctx)
	if err != nil {
		return nil, fmt.Errorf("eval %q: %w", e.Original, err)
	}

	native, err := GoNativeType(out)
	if err != nil {
		return nil, fmt.Errorf("convert %q: %w", e.Original, err)
	}

	return native, nil
}
