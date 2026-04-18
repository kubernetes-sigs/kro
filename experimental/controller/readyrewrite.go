// readyrewrite.go implements CEL AST rewrites that substitute member-function
// calls on scope variables with map lookups on reserved scope variables.
//
// Two rewrites use the same mechanism:
//
//   - `<wk_id>.ready()` → `__kroNodeReady["<wk_id>"]` for Watch node IDs.
//     This lets `.ready()` on a Watch reflect the node's own readyWhen verdict
//     — including when the collection is empty. Per 001-graph.md § readyWhen.
//
//   - `<ident>.dependencies()` → `__kroDeps["<ident>"]` for all node IDs.
//     Per 001-graph.md § propagateWhen.
//
// Both use rewriteMemberCallToMapLookup — a generic AST walker parameterized
// by the function name to match, the ID set to match against, and the reserved
// variable to redirect to.
package graphcontroller

import (
	celast "github.com/google/cel-go/common/ast"
	"github.com/google/cel-go/common/operators"
	"github.com/google/cel-go/common/types"
)

// reservedNodeReadyVar is the CEL scope variable carrying per-Watch
// readiness verdicts. Declared as map(string, bool) in the env; populated
// before each prg.Eval from instanceState. The leading underscore
// distinguishes it from user-visible identifiers.
const reservedNodeReadyVar = "__kroNodeReady"

// reservedDepsMapVar is the CEL scope variable carrying per-node
// dependency lists for the .dependencies() member function. Maps
// node ID → list of dependency scope values. Populated before
// propagateWhen evaluation. Per 001-graph.md § propagateWhen.
const reservedDepsMapVar = "__kroDeps"

// rewriteCollectionReady rewrites `<wk_id>.ready()` → `__kroNodeReady["<wk_id>"]`
// for each Watch node ID in collectionIDs.
//
// Scalar and forEach nodes are NOT rewritten. Their `.ready()` continues
// to work via per-item `__ready` stamping, which is correct for those
// topologies.
func rewriteCollectionReady(expr celast.Expr, collectionIDs map[string]bool, factory celast.ExprFactory, nextID func() int64) bool {
	return rewriteMemberCallToMapLookup(expr, "ready", collectionIDs, reservedNodeReadyVar, factory, nextID)
}

// rewriteDependencies rewrites `<ident>.dependencies()` → `__kroDeps["<ident>"]`
// for each node ID in scopeVars.
func rewriteDependencies(expr celast.Expr, scopeVars map[string]bool, factory celast.ExprFactory, nextID func() int64) bool {
	return rewriteMemberCallToMapLookup(expr, "dependencies", scopeVars, reservedDepsMapVar, factory, nextID)
}

// rewriteMemberCallToMapLookup walks a CEL AST in place and rewrites each
// `<ident>.<funcName>()` call whose target identifier is in idSet into
// `<targetVar>["<ident>"]`. Returns true if any rewrite occurred.
//
// The rewrite is structural: the original node's id is preserved on the
// outer expression, and SetKindCase replaces the expression kind. New
// sub-expressions are assigned fresh IDs via nextID — reusing IDs across
// different kinds confuses CEL's type checker ("incompatible type already
// exists for expression").
func rewriteMemberCallToMapLookup(expr celast.Expr, funcName string, idSet map[string]bool, targetVar string, factory celast.ExprFactory, nextID func() int64) bool {
	if expr == nil || len(idSet) == 0 {
		return false
	}
	changed := false

	var walk func(e celast.Expr)
	walk = func(e celast.Expr) {
		if e == nil {
			return
		}
		switch e.Kind() {
		case celast.CallKind:
			call := e.AsCall()
			// Match the `<ident>.<funcName>()` pattern at this node.
			if call.IsMemberFunction() && call.FunctionName() == funcName &&
				len(call.Args()) == 0 && call.Target() != nil &&
				call.Target().Kind() == celast.IdentKind {
				ident := call.Target().AsIdent()
				if idSet[ident] {
					// Rewrite in place to: <targetVar>["<ident>"]
					// The CEL operator for index access is "_[_]",
					// with the map as arg0 and the key as arg1. It is
					// NOT a member function. Fresh IDs are required
					// for the new sub-expressions to avoid type-check
					// collisions.
					idxOp := factory.NewCall(
						e.ID(),
						operators.Index,
						factory.NewIdent(nextID(), targetVar),
						factory.NewLiteral(nextID(), types.String(ident)),
					)
					e.SetKindCase(idxOp)
					changed = true
					return
				}
			}
			// Recurse into target and args.
			if call.Target() != nil {
				walk(call.Target())
			}
			for _, arg := range call.Args() {
				walk(arg)
			}

		case celast.SelectKind:
			walk(e.AsSelect().Operand())

		case celast.ComprehensionKind:
			comp := e.AsComprehension()
			walk(comp.IterRange())
			walk(comp.LoopCondition())
			walk(comp.LoopStep())
			walk(comp.AccuInit())
			walk(comp.Result())

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
				walk(field.AsStructField().Value())
			}
		}
	}
	walk(expr)
	return changed
}
