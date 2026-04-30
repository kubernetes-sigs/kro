// fieldpath.go extracts referenced field paths from compiled CEL expressions.
//
// The design (005-reconciliation.md) commits to field-path-level hashing:
// "At graph compilation, the controller walks each compiled expression's AST to
// extract reference chains — sequences of select operations rooted at a scope
// variable." This file implements that extraction.
//
// The FieldPath type itself lives in graph/ (it's a pure data type). This file
// contains the CEL AST walking that produces FieldPaths — compiler logic that
// depends on cel-go/common/ast.
package compiler

import (
	celast "github.com/google/cel-go/common/ast"
	"github.com/google/cel-go/common/operators"

	"github.com/ellistarn/kro/experimental/controller/graph"
)

// extractFieldPathsFromAST walks a CEL AST and collects all (scopeVariable, FieldPath)
// pairs. comprehensionVars tracks iteration variables introduced by comprehension
// expressions — these are not scope variables and should not produce paths.
func extractFieldPathsFromAST(expr celast.Expr, scopeVars map[string]bool, comprehensionVars map[string]bool) map[string][]graph.FieldPath {
	result := map[string][]graph.FieldPath{}

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
				graph.AddPath(result, root, path)
				return // don't recurse into the chain — we've consumed it
			}
			// The chain didn't resolve to a scope variable. Recurse into
			// the operand to find any nested references.
			walk(e.AsSelect().Operand())

		case celast.CallKind:
			call := e.AsCall()
			// .ready() is a property of a node like any other field —
			// extract ["__ready"] as a dependency path. Per
			// 001-graph.md § Dependencies, .ready() produces a field
			// path through the same mechanism as status.replicas or
			// metadata.name.
			if call.IsMemberFunction() && call.FunctionName() == "ready" {
				if target := call.Target(); target != nil {
					if target.Kind() == celast.IdentKind {
						root := target.AsIdent()
						if scopeVars[root] && (comprehensionVars == nil || !comprehensionVars[root]) {
							graph.AddPath(result, root, graph.FieldPath{"__ready"})
						}
					} else {
						// Non-identifier target (e.g., comprehension
						// result) — walk to capture dependencies.
						walk(target)
					}
				}
				return
			}

			// Optional select: _?._ — extract field paths like regular selects.
			if call.FunctionName() == operators.OptSelect && len(call.Args()) == 2 {
				if root, path := resolveOptionalSelectChain(e, comprehensionVars); root != "" && scopeVars[root] {
					graph.AddPath(result, root, path)
					return // consumed the chain
				}
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
				graph.AddPath(result, ident, nil)
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
func resolveSelectChain(e celast.Expr, comprehensionVars map[string]bool) (string, graph.FieldPath) {
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

// resolveOptionalSelectChain walks through chained optional select operations
// (_?._) and regular selects to find the root identifier and collect field names.
// Returns ("", nil) if the chain doesn't resolve to a scope variable.
func resolveOptionalSelectChain(e celast.Expr, comprehensionVars map[string]bool) (string, graph.FieldPath) {
	var segments []string
	current := e

	for {
		switch current.Kind() {
		case celast.CallKind:
			call := current.AsCall()
			if call.FunctionName() == operators.OptSelect && len(call.Args()) == 2 {
				// args[1] is the field name as a string literal.
				fieldName, ok := call.Args()[1].AsLiteral().Value().(string)
				if !ok {
					return "", nil
				}
				segments = append(segments, fieldName)
				current = call.Args()[0]
				continue
			}
			// Not an optional select — can't continue the chain.
			return "", nil

		case celast.SelectKind:
			// Regular select can appear below optional selects in a chain
			// (e.g., deploy.status?.conditions).
			sel := current.AsSelect()
			segments = append(segments, sel.FieldName())
			current = sel.Operand()
			continue

		case celast.IdentKind:
			root := current.AsIdent()
			if comprehensionVars != nil && comprehensionVars[root] {
				return "", nil
			}
			// Segments were collected in reverse order (leaf first).
			reverse(segments)
			return root, segments

		default:
			return "", nil
		}
	}
}

// classifyAccessModes walks a CEL AST and classifies how each scope variable is
// accessed. Returns a map where true means "all accesses in this expression are
// through optional patterns" and false means "at least one direct access."
//
// Optional access patterns:
//   - _?._ (OptSelect) with scope var as first arg
//   - _[?_] (OptIndex) with scope var as first arg
//   - .ready().orValue() / .updated().orValue() — explicit optional unwrap
//
// Direct access: regular Select, bare Ident, bare .ready()/.updated(), or
// any other use. Bare .ready() (without .orValue()) is direct — the user
// is not handling absence, so the dep must be present.
// This classification drives DepKind: optional-only across ALL expressions → DepLazy.
func classifyAccessModes(expr celast.Expr, scopeVars map[string]bool, comprehensionVars map[string]bool) map[string]bool {
	result := map[string]bool{}

	// markAccess records an access mode for a variable. If the var hasn't been
	// seen, set it. If already seen as true (optional), a false (direct)
	// overrides to false. Once false, never goes back to true.
	markAccess := func(name string, optional bool) {
		prev, seen := result[name]
		if !seen {
			result[name] = optional
			return
		}
		// Once direct (false), it stays direct.
		if prev && !optional {
			result[name] = false
		}
	}

	var walk func(e celast.Expr)
	walk = func(e celast.Expr) {
		if e == nil {
			return
		}

		switch e.Kind() {
		case celast.SelectKind:
			// Try to resolve the select chain to a scope variable.
			if root, _ := resolveSelectChain(e, comprehensionVars); root != "" && scopeVars[root] {
				markAccess(root, false) // direct access
				return
			}
			// Chain didn't resolve — recurse into operand.
			walk(e.AsSelect().Operand())

		case celast.CallKind:
			call := e.AsCall()

			// _?._ (OptSelect): scope var as first arg → optional access.
			if call.FunctionName() == operators.OptSelect && len(call.Args()) == 2 {
				arg0 := call.Args()[0]
				if arg0.Kind() == celast.IdentKind {
					root := arg0.AsIdent()
					if scopeVars[root] && (comprehensionVars == nil || !comprehensionVars[root]) {
						markAccess(root, true) // optional access
						return                 // don't recurse — consumed the operand
					}
				}
				// Not a scope var ident — recurse into args[0] only.
				walk(call.Args()[0])
				return
			}

			// _[?_] (OptIndex): same pattern as OptSelect.
			if call.FunctionName() == operators.OptIndex && len(call.Args()) == 2 {
				arg0 := call.Args()[0]
				if arg0.Kind() == celast.IdentKind {
					root := arg0.AsIdent()
					if scopeVars[root] && (comprehensionVars == nil || !comprehensionVars[root]) {
						markAccess(root, true) // optional access
						return
					}
				}
				walk(call.Args()[0])
				return
			}

			// .orValue() wrapping .ready()/.updated() → optional access.
			// Pattern: deployment.ready().orValue(false) — the user
			// explicitly handles absence. Bare .ready() without .orValue()
			// falls through to the generic handler, which recurses into
			// the target ident and marks DIRECT.
			if call.IsMemberFunction() && call.FunctionName() == "orValue" {
				if target := call.Target(); target != nil && target.Kind() == celast.CallKind {
					inner := target.AsCall()
					if inner.IsMemberFunction() &&
						(inner.FunctionName() == "ready" || inner.FunctionName() == "updated") {
						if innerTarget := inner.Target(); innerTarget != nil &&
							innerTarget.Kind() == celast.IdentKind {
							root := innerTarget.AsIdent()
							if scopeVars[root] && (comprehensionVars == nil || !comprehensionVars[root]) {
								markAccess(root, true) // optional: .ready().orValue()
								for _, arg := range call.Args() {
									walk(arg) // walk the default value for any refs
								}
								return
							}
						}
					}
				}
				// Not the .ready()/.updated().orValue() pattern — fall through.
			}

			// Generic call — recurse into target and all args.
			// Bare .ready()/.updated() without .orValue() fall here: the
			// walk recurses into the target, reaches IdentKind, and marks
			// DIRECT — making the dep hard.
			if call.Target() != nil {
				walk(call.Target())
			}
			for _, arg := range call.Args() {
				walk(arg)
			}

		case celast.ComprehensionKind:
			comp := e.AsComprehension()
			walk(comp.IterRange())
			// Recurse into body with iteration variables excluded.
			innerVars := copyVarSet(comprehensionVars)
			innerVars[comp.IterVar()] = true
			if comp.HasIterVar2() {
				innerVars[comp.IterVar2()] = true
			}
			innerVars[comp.AccuVar()] = true
			innerResult := classifyAccessModes(comp.LoopCondition(), scopeVars, innerVars)
			mergeAccessModes(result, innerResult, markAccess)
			innerResult = classifyAccessModes(comp.LoopStep(), scopeVars, innerVars)
			mergeAccessModes(result, innerResult, markAccess)
			innerResult = classifyAccessModes(comp.AccuInit(), scopeVars, innerVars)
			mergeAccessModes(result, innerResult, markAccess)
			innerResult = classifyAccessModes(comp.Result(), scopeVars, innerVars)
			mergeAccessModes(result, innerResult, markAccess)

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
				markAccess(ident, false) // direct access — bare identifier
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

// mergeAccessModes merges inner classification results into the outer result
// using the provided markAccess function.
func mergeAccessModes(dst, src map[string]bool, markAccess func(string, bool)) {
	for name, optional := range src {
		markAccess(name, optional)
	}
}

// mergeResults merges src into dst, deduplicating paths.
func mergeResults(dst, src map[string][]graph.FieldPath) {
	for root, paths := range src {
		for _, p := range paths {
			graph.AddPath(dst, root, p)
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
