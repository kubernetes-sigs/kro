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
	"slices"

	celast "github.com/google/cel-go/common/ast"
	"github.com/google/cel-go/common/operators"

	"github.com/ellistarn/kro/experimental/controller/graph"
)

// ---------------------------------------------------------------------------
// astVisitor + walkAST — shared CEL AST traversal
// ---------------------------------------------------------------------------

// astVisitor defines callbacks for per-node handling during a CEL AST walk.
// walkAST owns the recursion structure (Select chain resolution, comprehension
// variable tracking, List/Map/Struct/Literal traversal). Visitors supply the
// per-use-case logic.
type astVisitor struct {
	// onSelectChain is called when a select chain resolves to a scope variable.
	// root is the variable name, path is the field chain.
	onSelectChain func(root string, path graph.FieldPath)

	// onCall is called for each CallKind node. Return true if the call was
	// fully consumed (no further recursion). walk is provided for custom
	// recursion into sub-expressions.
	onCall func(e celast.Expr, scopeVars, compVars map[string]bool, walk func(celast.Expr)) bool

	// onIdent is called for bare scope variable identifiers.
	onIdent func(name string)

	// onComprehension is called with the inner-scope results from a
	// comprehension body. The walker handles variable tracking; this
	// callback merges the inner walk's results into the outer state.
	onComprehension func(innerVars map[string]bool, sub celast.Expr)
}

// walkAST performs a depth-first walk of a CEL AST, calling visitor callbacks
// for each expression kind. scopeVars are the in-scope dependency variables;
// comprehensionVars tracks iteration variables to exclude from dependency analysis.
func walkAST(expr celast.Expr, scopeVars, comprehensionVars map[string]bool, v *astVisitor) {
	var walk func(e celast.Expr)
	walk = func(e celast.Expr) {
		if e == nil {
			return
		}

		switch e.Kind() {
		case celast.SelectKind:
			if root, path := resolveSelectChain(e, comprehensionVars); root != "" && scopeVars[root] {
				v.onSelectChain(root, path)
				return
			}
			walk(e.AsSelect().Operand())

		case celast.CallKind:
			if v.onCall != nil && v.onCall(e, scopeVars, comprehensionVars, walk) {
				return
			}
			call := e.AsCall()
			if call.Target() != nil {
				walk(call.Target())
			}
			for _, arg := range call.Args() {
				walk(arg)
			}

		case celast.ComprehensionKind:
			comp := e.AsComprehension()
			walk(comp.IterRange())
			innerVars := copyVarSet(comprehensionVars)
			innerVars[comp.IterVar()] = true
			if comp.HasIterVar2() {
				innerVars[comp.IterVar2()] = true
			}
			innerVars[comp.AccuVar()] = true
			for _, sub := range []celast.Expr{
				comp.LoopCondition(), comp.LoopStep(), comp.AccuInit(), comp.Result(),
			} {
				v.onComprehension(innerVars, sub)
			}

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
				v.onIdent(ident)
			}

		case celast.LiteralKind:
			// No references in literals.

		default:
			// Unknown expression kind — skip.
		}
	}

	walk(expr)
}

// ---------------------------------------------------------------------------
// extractFieldPathsFromAST
// ---------------------------------------------------------------------------

// extractFieldPathsFromAST walks a CEL AST and collects all (scopeVariable, FieldPath)
// pairs. comprehensionVars tracks iteration variables introduced by comprehension
// expressions — these are not scope variables and should not produce paths.
func extractFieldPathsFromAST(expr celast.Expr, scopeVars map[string]bool, comprehensionVars map[string]bool) map[string][]graph.FieldPath {
	result := map[string][]graph.FieldPath{}

	walkAST(expr, scopeVars, comprehensionVars, &astVisitor{
		onSelectChain: func(root string, path graph.FieldPath) {
			graph.AddPath(result, root, path)
		},
		onCall: func(e celast.Expr, sv, cv map[string]bool, walk func(celast.Expr)) bool {
			call := e.AsCall()
			// .ready() → extract ["__ready"] as a dependency path.
			if call.IsMemberFunction() && call.FunctionName() == "ready" {
				if target := call.Target(); target != nil {
					if target.Kind() == celast.IdentKind {
						root := target.AsIdent()
						if sv[root] && (cv == nil || !cv[root]) {
							graph.AddPath(result, root, graph.FieldPath{"__ready"})
						}
					} else {
						walk(target)
					}
				}
				return true
			}
			// Optional select: _?._ — extract field paths like regular selects.
			if call.FunctionName() == operators.OptSelect && len(call.Args()) == 2 {
				if root, path := resolveOptionalSelectChain(e, cv); root != "" && sv[root] {
					graph.AddPath(result, root, path)
					return true
				}
			}
			return false
		},
		onIdent: func(name string) {
			graph.AddPath(result, name, nil)
		},
		onComprehension: func(innerVars map[string]bool, sub celast.Expr) {
			innerResult := extractFieldPathsFromAST(sub, scopeVars, innerVars)
			mergeResults(result, innerResult)
		},
	})

	return result
}

// ---------------------------------------------------------------------------
// classifyAccessModes
// ---------------------------------------------------------------------------

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
		if prev && !optional {
			result[name] = false
		}
	}

	walkAST(expr, scopeVars, comprehensionVars, &astVisitor{
		onSelectChain: func(root string, _ graph.FieldPath) {
			markAccess(root, false) // direct access
		},
		onCall: func(e celast.Expr, sv, cv map[string]bool, walk func(celast.Expr)) bool {
			call := e.AsCall()

			// _?._ (OptSelect): scope var as first arg → optional access.
			if call.FunctionName() == operators.OptSelect && len(call.Args()) == 2 {
				arg0 := call.Args()[0]
				if arg0.Kind() == celast.IdentKind {
					root := arg0.AsIdent()
					if sv[root] && (cv == nil || !cv[root]) {
						markAccess(root, true)
						return true
					}
				}
				walk(call.Args()[0])
				return true
			}

			// _[?_] (OptIndex): same pattern as OptSelect.
			if call.FunctionName() == operators.OptIndex && len(call.Args()) == 2 {
				arg0 := call.Args()[0]
				if arg0.Kind() == celast.IdentKind {
					root := arg0.AsIdent()
					if sv[root] && (cv == nil || !cv[root]) {
						markAccess(root, true)
						return true
					}
				}
				walk(call.Args()[0])
				return true
			}

			// .orValue() wrapping .ready()/.updated() → optional access.
			if call.IsMemberFunction() && call.FunctionName() == "orValue" {
				if target := call.Target(); target != nil && target.Kind() == celast.CallKind {
					inner := target.AsCall()
					if inner.IsMemberFunction() &&
						(inner.FunctionName() == "ready" || inner.FunctionName() == "updated") {
						if innerTarget := inner.Target(); innerTarget != nil &&
							innerTarget.Kind() == celast.IdentKind {
							root := innerTarget.AsIdent()
							if sv[root] && (cv == nil || !cv[root]) {
								markAccess(root, true)
								for _, arg := range call.Args() {
									walk(arg)
								}
								return true
							}
						}
					}
				}
			}

			return false // not consumed — let walkAST do generic recursion
		},
		onIdent: func(name string) {
			markAccess(name, false) // direct access — bare identifier
		},
		onComprehension: func(innerVars map[string]bool, sub celast.Expr) {
			innerResult := classifyAccessModes(sub, scopeVars, innerVars)
			for name, optional := range innerResult {
				markAccess(name, optional)
			}
		},
	})

	return result
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

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
	slices.Reverse(segments)
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
			slices.Reverse(segments)
			return root, segments

		default:
			return "", nil
		}
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
