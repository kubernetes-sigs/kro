// readyrewrite.go implements a CEL AST rewrite that substitutes
// `<wk_id>.ready()` with `__kroNodeReady["<wk_id>"]` for each Watch
// node ID. This lets `.ready()` on a Watch reflect the node's own
// readyWhen verdict — including when the collection is empty.
//
// The per-item `__ready` stamping that backs `.ready()` for scalar and
// forEach nodes cannot represent collection-level readiness when the
// collection is empty. A Watch's own readyWhen verdict is tracked in
// the instance state and injected into CEL scope under a reserved key;
// this rewrite redirects `<wk_id>.ready()` calls to that key so the
// verdict is surfaced consistently for all collection sizes.
//
// Per 001-graph.md § readyWhen: "A Watch's `.ready()` returns true
// when the node's readyWhen conditions pass (evaluated once against the
// whole array, not per-item)."
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

// rewriteCollectionReady walks a CEL AST in place and rewrites each
// `<wk_id>.ready()` call whose target identifier is in collectionIDs
// into `__kroNodeReady["<wk_id>"]`. Returns true if any rewrite occurred.
//
// The rewrite is structural: the original node's id is preserved on the
// outer expression, and SetKindCase replaces the expression kind. New
// sub-expressions are assigned fresh IDs via nextID — reusing IDs across
// different kinds confuses CEL's type checker ("incompatible type already
// exists for expression").
//
// Scalar and forEach nodes are NOT rewritten. Their `.ready()` continues
// to work via per-item `__ready` stamping, which is correct for those
// topologies.
func rewriteCollectionReady(expr celast.Expr, collectionIDs map[string]bool, factory celast.ExprFactory, nextID func() int64) bool {
	if expr == nil || len(collectionIDs) == 0 {
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
			// Match the `<ident>.ready()` pattern at this node.
			if call.IsMemberFunction() && call.FunctionName() == "ready" &&
				len(call.Args()) == 0 && call.Target() != nil &&
				call.Target().Kind() == celast.IdentKind {
				ident := call.Target().AsIdent()
				if collectionIDs[ident] {
					// Rewrite in place to: __kroNodeReady["<ident>"]
					// The CEL operator for index access is "_[_]",
					// with the map as arg0 and the key as arg1. It is
					// NOT a member function. Fresh IDs are required
					// for the new sub-expressions to avoid type-check
					// collisions.
					idxOp := factory.NewCall(
						e.ID(),
						operators.Index,
						factory.NewIdent(nextID(), reservedNodeReadyVar),
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

// rewriteDependencies walks a CEL AST in place and rewrites each
// `<ident>.dependencies()` call whose target identifier is in scopeVars
// into `__kroDeps["<ident>"]`. Returns true if any rewrite occurred.
//
// Structurally identical to rewriteCollectionReady but matches
// `.dependencies()` instead of `.ready()` and uses scopeVars (all
// node IDs) instead of collectionIDs. The rewrite replaces the member
// call with an index operation on the reserved __kroDeps map variable.
func rewriteDependencies(expr celast.Expr, scopeVars map[string]bool, factory celast.ExprFactory, nextID func() int64) bool {
	if expr == nil || len(scopeVars) == 0 {
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
			if call.IsMemberFunction() && call.FunctionName() == "dependencies" &&
				len(call.Args()) == 0 && call.Target() != nil &&
				call.Target().Kind() == celast.IdentKind {
				ident := call.Target().AsIdent()
				if scopeVars[ident] {
					idxOp := factory.NewCall(
						e.ID(),
						operators.Index,
						factory.NewIdent(nextID(), reservedDepsMapVar),
						factory.NewLiteral(nextID(), types.String(ident)),
					)
					e.SetKindCase(idxOp)
					changed = true
					return
				}
			}
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
