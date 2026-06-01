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

package library

import (
	"fmt"

	"github.com/google/cel-go/cel"
	celast "github.com/google/cel-go/common/ast"
)

// ValidateConditionExpressions performs build-time AST inspection on a
// list of author-defined condition expressions. It enforces the
// self-reference rule: runtime.condition(_, 'X') where X is a literal
// that matches a custom-defined type from runtime.newCondition is
// rejected — custom conditions cannot reference each other.
//
// Structural rules for runtime.newCondition (allowed keys, valid status
// literals, required fields) are enforced earlier by a parse-time macro
// in runtime.go. By the time expressions reach this validator the macro
// has already rewritten identifier-keyed map literals to string-keyed
// ones, so the helpers below find condition types via literal-string
// keys.
//
// Returns the first error encountered, or nil if all expressions pass.
func ValidateConditionExpressions(env *cel.Env, expressions []string) error {
	// First pass: collect literal type values from all newCondition calls
	// so the second pass can detect self-references.
	customTypes := map[string]struct{}{}
	for _, expr := range expressions {
		ast, iss := env.Parse(expr)
		if iss.Err() != nil {
			// Parse errors aren't this validator's responsibility; let the
			// regular CEL parse step surface them.
			continue
		}
		collectCustomTypes(ast.NativeRep().Expr(), customTypes)
	}

	for _, expr := range expressions {
		ast, iss := env.Parse(expr)
		if iss.Err() != nil {
			continue
		}
		if err := validateExpressionAST(ast.NativeRep().Expr(), customTypes, expr); err != nil {
			return err
		}
	}

	return nil
}

// collectCustomTypes walks the AST looking for runtime.newCondition({type: 'X'})
// calls with literal-string type values, and adds the type names to out.
func collectCustomTypes(expr celast.Expr, out map[string]struct{}) {
	walk(expr, nil, func(e celast.Expr) {
		if !isRuntimeCall(e, "newCondition") {
			return
		}
		args := e.AsCall().Args()
		if len(args) != 1 {
			return
		}
		typeVal, ok := mapLiteralEntryStringValue(args[0], "type")
		if !ok {
			return
		}
		out[typeVal] = struct{}{}
	})
}

// validateExpressionAST walks expr applying the self-reference rule.
// Returns the first error found.
func validateExpressionAST(expr celast.Expr, customTypes map[string]struct{}, exprText string) error {
	var firstErr error
	walk(expr, &firstErr, func(e celast.Expr) {
		if isRuntimeCall(e, "condition") {
			if err := validateCondition(e, customTypes, exprText); err != nil {
				firstErr = err
			}
		}
	})
	return firstErr
}

// validateCondition enforces the self-reference rule: runtime.condition(_, 'X')
// where X is a literal that matches a custom-defined type is rejected.
func validateCondition(call celast.Expr, customTypes map[string]struct{}, exprText string) error {
	args := call.AsCall().Args()
	if len(args) != 2 {
		return nil // Wrong arity is a CEL type-checker concern.
	}

	typeArg := args[1]
	typeName, ok := literalString(typeArg)
	if !ok {
		return nil // Dynamic type; runtime check.
	}

	if _, isCustom := customTypes[typeName]; isCustom {
		return fmt.Errorf(
			"runtime.condition(_, %q): custom conditions cannot reference each other in expression %q",
			typeName, exprText,
		)
	}

	return nil
}

// -------------------------------------------------------------------------
// AST helpers
// -------------------------------------------------------------------------

// walk recursively visits expr and every sub-expression, calling visit for
// each. Order is pre-order: parent visited before children.
//
// If errPtr is non-nil, traversal aborts as soon as *errPtr becomes non-nil.
// Pass nil for exhaustive traversal (e.g., when the visitor cannot fail).
func walk(expr celast.Expr, errPtr *error, visit func(celast.Expr)) {
	if expr == nil || (errPtr != nil && *errPtr != nil) {
		return
	}
	visit(expr)

	switch expr.Kind() {
	case celast.CallKind:
		call := expr.AsCall()
		if call.Target() != nil {
			walk(call.Target(), errPtr, visit)
		}
		for _, arg := range call.Args() {
			walk(arg, errPtr, visit)
		}
	case celast.SelectKind:
		walk(expr.AsSelect().Operand(), errPtr, visit)
	case celast.ListKind:
		for _, elem := range expr.AsList().Elements() {
			walk(elem, errPtr, visit)
		}
	case celast.MapKind:
		for _, entry := range expr.AsMap().Entries() {
			me := entry.AsMapEntry()
			walk(me.Key(), errPtr, visit)
			walk(me.Value(), errPtr, visit)
		}
	case celast.StructKind:
		for _, field := range expr.AsStruct().Fields() {
			sf := field.AsStructField()
			walk(sf.Value(), errPtr, visit)
		}
	case celast.ComprehensionKind:
		comp := expr.AsComprehension()
		walk(comp.IterRange(), errPtr, visit)
		walk(comp.AccuInit(), errPtr, visit)
		walk(comp.LoopCondition(), errPtr, visit)
		walk(comp.LoopStep(), errPtr, visit)
		walk(comp.Result(), errPtr, visit)
	}
}

// isRuntimeCall reports whether expr is a call of the form
// runtime.<methodName>(...).
func isRuntimeCall(expr celast.Expr, methodName string) bool {
	if expr.Kind() != celast.CallKind {
		return false
	}
	call := expr.AsCall()
	if !call.IsMemberFunction() || call.FunctionName() != methodName {
		return false
	}
	target := call.Target()
	if target == nil || target.Kind() != celast.IdentKind {
		return false
	}
	return target.AsIdent() == RuntimeVarName
}

// literalString returns the string value of a literal-string AST node, or
// ("", false) if the node isn't a literal string.
func literalString(expr celast.Expr) (string, bool) {
	if expr.Kind() != celast.LiteralKind {
		return "", false
	}
	val := expr.AsLiteral().Value()
	s, ok := val.(string)
	return s, ok
}

// mapLiteralEntryStringValue looks up key in a map literal's entries and
// returns the entry's value if it's a literal string. Returns ("", false)
// if expr isn't a map literal, the key isn't present, or the value isn't a
// literal string.
func mapLiteralEntryStringValue(expr celast.Expr, key string) (string, bool) {
	if expr.Kind() != celast.MapKind {
		return "", false
	}
	for _, entry := range expr.AsMap().Entries() {
		me := entry.AsMapEntry()
		k, ok := literalString(me.Key())
		if !ok || k != key {
			continue
		}
		return literalString(me.Value())
	}
	return "", false
}
