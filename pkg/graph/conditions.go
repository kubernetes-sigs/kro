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
	"fmt"

	"github.com/google/cel-go/cel"
	celast "github.com/google/cel-go/common/ast"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/cel/library"
)

// validateConditionExpressions applies build-time rules to the author's
// condition expressions:
//
//   - runtime.condition(schema, 'X') with a literal X must name a kro
//     built-in type: an author-declared type is a self-reference, anything
//     else is unknown. Child-resource and non-literal lookups are
//     unrestricted.
//   - Literal newCondition values: status must be True/False/Unknown and
//     type must not be empty. Computed values are checked at evaluation.
//   - No two entries may declare the same literal type; a type may repeat
//     within one entry (ternary branches). This check is not branch-aware:
//     two entries that could only emit the shared type in mutually
//     exclusive branches are still rejected.
//
// newCondition's key rules are enforced by its parse-time macro. Unparsable
// expressions are skipped; the regular compile step reports those errors.
func validateConditionExpressions(env *cel.Env, expressions []string) error {
	asts := make([]celast.Expr, len(expressions))
	for i, expr := range expressions {
		parsed, iss := env.Parse(expr)
		if iss.Err() != nil {
			continue
		}
		asts[i] = parsed.NativeRep().Expr()
	}

	// Collect the literal types each entry declares via newCondition so the
	// second pass can detect self-references, rejecting types declared by
	// more than one entry along the way.
	declared := map[string]struct{}{}
	for _, root := range asts {
		if root == nil {
			continue
		}
		for typ := range literalConditionTypes(root) {
			if _, dup := declared[typ]; dup {
				return fmt.Errorf("condition type %q is declared by more than one conditions entry", typ)
			}
			declared[typ] = struct{}{}
		}
	}

	for i, root := range asts {
		if root == nil {
			continue
		}
		if err := validateConditionAST(root, declared, expressions[i]); err != nil {
			return err
		}
	}
	return nil
}

// literalConditionTypes returns the literal type values of every
// runtime.newCondition call under root.
func literalConditionTypes(root celast.Expr) map[string]struct{} {
	out := map[string]struct{}{}
	celast.PreOrderVisit(root, celast.NewExprVisitor(func(e celast.Expr) {
		if isRuntimeCall(e, "newCondition") && len(e.AsCall().Args()) == 1 {
			if typ, ok := mapLiteralEntryStringValue(e.AsCall().Args()[0], "type"); ok {
				out[typ] = struct{}{}
			}
		}
	}))
	return out
}

// validateConditionAST applies the lookup and literal-value rules to every
// runtime call under expr, returning the first error found.
func validateConditionAST(expr celast.Expr, authorTypes map[string]struct{}, exprText string) error {
	var firstErr error
	celast.PreOrderVisit(expr, celast.NewExprVisitor(func(e celast.Expr) {
		if firstErr != nil {
			return
		}
		switch {
		case isRuntimeCall(e, "condition"):
			firstErr = validateConditionLookup(e, authorTypes, exprText)
		case isRuntimeCall(e, "newCondition"):
			firstErr = validateNewConditionLiterals(e, exprText)
		}
	}))
	return firstErr
}

// validateNewConditionLiterals checks runtime.newCondition's literal status
// and type values. Computed values are checked at evaluation time.
func validateNewConditionLiterals(call celast.Expr, exprText string) error {
	args := call.AsCall().Args()
	if len(args) != 1 {
		return nil // Wrong arity is a CEL type-checker concern.
	}
	if status, ok := mapLiteralEntryStringValue(args[0], "status"); ok {
		if !library.IsValidConditionStatus(status) {
			return fmt.Errorf(
				"runtime.newCondition: status must be one of True, False, Unknown (got %q) in expression %q",
				status, exprText,
			)
		}
	}
	if typeVal, ok := mapLiteralEntryStringValue(args[0], "type"); ok && typeVal == "" {
		return fmt.Errorf("runtime.newCondition: type must not be empty in expression %q", exprText)
	}
	return nil
}

// validateConditionLookup enforces the lookup rules for
// runtime.condition(schema, 'X') calls: X must be a kro built-in type, not
// an author-declared type (self-reference) or an unknown name (typo).
// Lookups on other objects (child resources) and non-literal lookups are
// unrestricted.
func validateConditionLookup(call celast.Expr, authorTypes map[string]struct{}, exprText string) error {
	args := call.AsCall().Args()
	if len(args) != 2 {
		return nil // Wrong arity is a CEL type-checker concern.
	}
	if obj := args[0]; obj.Kind() != celast.IdentKind || obj.AsIdent() != SchemaVarName {
		return nil
	}
	typeName, ok := literalString(args[1])
	if !ok {
		return nil
	}

	if _, isBuiltin := v1alpha1.KROBuiltinConditionTypes[typeName]; isBuiltin {
		return nil
	}
	if _, isAuthor := authorTypes[typeName]; isAuthor {
		return fmt.Errorf(
			"runtime.condition(schema, %q): custom conditions cannot reference each other in expression %q",
			typeName, exprText,
		)
	}
	return fmt.Errorf(
		"runtime.condition(schema, %q): unknown condition type; only kro's built-in types "+
			"(InstanceManaged, GraphResolved, ResourcesReady, Ready) can be read from schema in expression %q",
		typeName, exprText,
	)
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
	return target != nil && target.Kind() == celast.IdentKind && target.AsIdent() == library.RuntimeVarName
}

// literalString returns the string value of a literal-string AST node.
func literalString(expr celast.Expr) (string, bool) {
	if expr.Kind() != celast.LiteralKind {
		return "", false
	}
	s, ok := expr.AsLiteral().Value().(string)
	return s, ok
}

// mapLiteralEntryStringValue looks up key in a map literal's entries and
// returns the entry's value if it's a literal string.
func mapLiteralEntryStringValue(expr celast.Expr, key string) (string, bool) {
	if expr.Kind() != celast.MapKind {
		return "", false
	}
	for _, entry := range expr.AsMap().Entries() {
		me := entry.AsMapEntry()
		if k, ok := literalString(me.Key()); ok && k == key {
			return literalString(me.Value())
		}
	}
	return "", false
}
