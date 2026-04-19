// fieldpath.go extracts referenced field paths from compiled CEL expressions.
//
// The design (005-reconciliation.md) commits to field-path-level hashing:
// "At graph compilation, the controller walks each compiled expression's AST to
// extract reference chains — sequences of select operations rooted at a scope
// variable." This file implements that extraction.
//
// A FieldPath is the deepest static prefix of a dot-select chain in a CEL
// expression. Static means no function calls, index operations, or
// comprehensions intervene. Examples:
//
//	deploy.status.availableReplicas      → ["status", "availableReplicas"]
//	deploy.metadata.labels["app"]        → ["metadata", "labels"]
//	deploy.status.conditions.filter(...) → ["status", "conditions"]
//	has(deploy.status.conditions)        → ["status", "conditions"]
package graphcontroller

import (
	celast "github.com/google/cel-go/common/ast"
)

// FieldPath is a sequence of field names representing a path into an object.
// For example, ["status", "availableReplicas"] represents .status.availableReplicas.
type FieldPath []string

// Equal returns true if two FieldPaths have the same segments.
func (fp FieldPath) Equal(other FieldPath) bool {
	if len(fp) != len(other) {
		return false
	}
	for i := range fp {
		if fp[i] != other[i] {
			return false
		}
	}
	return true
}

// extractFieldPathsFromAST walks a CEL AST and collects all (scopeVariable, FieldPath)
// pairs. comprehensionVars tracks iteration variables introduced by comprehension
// expressions — these are not scope variables and should not produce paths.
func extractFieldPathsFromAST(expr celast.Expr, scopeVars map[string]bool, comprehensionVars map[string]bool) map[string][]FieldPath {
	result := map[string][]FieldPath{}

	var walk func(e celast.Expr)
	walk = func(e celast.Expr) {
		if e == nil {
			return
		}

		switch e.Kind() {
		case celast.SelectKind:
			// Try to build a full select chain. If it resolves to a scope
			// variable, record the path.
			if root, path := resolveSelectChain(e, comprehensionVars); root != "" && scopeVars[root] {
				addPath(result, root, path)
				return // don't recurse into the chain — we've consumed it
			}
			// The chain didn't resolve to a scope variable. Recurse into
			// the operand to find any nested references.
			walk(e.AsSelect().Operand())

		case celast.CallKind:
			call := e.AsCall()
			// Skip the target of ready() calls — readiness is a runtime
			// property tracked separately (ReadinessDeps), not a field path.
			if call.IsMemberFunction() && call.FunctionName() == "ready" {
				return
			}
			if call.Target() != nil {
				walk(call.Target())
			}
			for _, arg := range call.Args() {
				walk(arg)
			}

		case celast.ComprehensionKind:
			comp := e.AsComprehension()
			// Walk the iteration range — this is where the scope variable
			// reference lives (e.g., deploy.status.conditions in .filter()).
			walk(comp.IterRange())
			// Walk the comprehension body, but with iteration variables excluded.
			innerVars := copyVarSet(comprehensionVars)
			innerVars[comp.IterVar()] = true
			if comp.HasIterVar2() {
				innerVars[comp.IterVar2()] = true
			}
			innerVars[comp.AccuVar()] = true
			innerResult := extractFieldPathsFromAST(comp.LoopCondition(), scopeVars, innerVars)
			mergeResults(result, innerResult)
			innerResult = extractFieldPathsFromAST(comp.LoopStep(), scopeVars, innerVars)
			mergeResults(result, innerResult)
			innerResult = extractFieldPathsFromAST(comp.AccuInit(), scopeVars, innerVars)
			mergeResults(result, innerResult)
			innerResult = extractFieldPathsFromAST(comp.Result(), scopeVars, innerVars)
			mergeResults(result, innerResult)

		case celast.ListKind:
			for _, elem := range e.AsList().Elements() {
				walk(elem)
			}

		case celast.MapKind:
			for _, entry := range e.AsMap().Entries() {
				me := entry.AsMapEntry()
				walk(me.Key())
				walk(me.Value())
			}

		case celast.StructKind:
			for _, field := range e.AsStruct().Fields() {
				sf := field.AsStructField()
				walk(sf.Value())
			}

		case celast.IdentKind:
			ident := e.AsIdent()
			if scopeVars[ident] && (comprehensionVars == nil || !comprehensionVars[ident]) {
				// Bare identifier reference with no field selection — the
				// entire object is referenced. Record an empty path.
				addPath(result, ident, nil)
			}

		case celast.LiteralKind:
			// No references in literals.

		default:
			// Unknown expression kind — skip.
		}
	}

	walk(expr)
	return result
}

// resolveSelectChain walks a Select chain backward (operand → operand → ...)
// to find the root identifier and collect field names. Returns ("", nil) if
// the chain doesn't resolve to a simple Ident root, or if the root is a
// comprehension variable.
//
// For deploy.status.availableReplicas (Select(Select(Ident("deploy"), "status"), "availableReplicas")):
//
//	root = "deploy", path = ["status", "availableReplicas"]
func resolveSelectChain(e celast.Expr, comprehensionVars map[string]bool) (string, FieldPath) {
	var segments []string
	current := e

	for current.Kind() == celast.SelectKind {
		sel := current.AsSelect()
		segments = append(segments, sel.FieldName())
		current = sel.Operand()
	}

	if current.Kind() != celast.IdentKind {
		return "", nil
	}

	root := current.AsIdent()
	if comprehensionVars != nil && comprehensionVars[root] {
		return "", nil
	}

	// Segments were collected in reverse order (leaf first).
	reverse(segments)
	return root, segments
}

// addFieldPath appends a FieldPath to a slice, deduplicating exact matches.
func addFieldPath(paths *[]FieldPath, path FieldPath) {
	for _, existing := range *paths {
		if existing.Equal(path) {
			return
		}
	}
	*paths = append(*paths, path)
}

// addPath adds a FieldPath to the result map, deduplicating exact matches.
func addPath(result map[string][]FieldPath, root string, path FieldPath) {
	for _, existing := range result[root] {
		if existing.Equal(path) {
			return
		}
	}
	result[root] = append(result[root], path)
}

// mergeResults merges src into dst, deduplicating paths.
func mergeResults(dst, src map[string][]FieldPath) {
	for root, paths := range src {
		for _, p := range paths {
			addPath(dst, root, p)
		}
	}
}

// copyVarSet creates a copy of a variable set, or a new empty set if nil.
func copyVarSet(vars map[string]bool) map[string]bool {
	result := make(map[string]bool, len(vars)+2)
	for k, v := range vars {
		result[k] = v
	}
	return result
}

// reverse reverses a slice in place.
func reverse(s []string) {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
}
