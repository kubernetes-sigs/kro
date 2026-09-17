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

// visitor.go — the operand normalization implemented purely with cel-go's
// REUSED traversal (ast.PreOrderVisit / ast.PostOrderVisit), no hand-rolled
// recursion.
//
// The recursive walker (optimizer.go) gets two things implicitly from its
// call stack: synthesized values (each child's kro-kind returned upward) and
// scoped variable bindings (comprehension variables pushed/popped around the
// body). Both are reconstructible over a flat visitor stream:
//
//   - Synthesis: PostOrderVisit guarantees children are visited before their
//     parent (see ast/navigable.go visit()), so a map[exprID]kroKind
//     side-table is always populated for a node's children when the node's
//     own callback runs.
//
//   - Scoping: one PreOrderVisit builds a parent map; an identifier resolves
//     its binding by walking UP the parent chain to the nearest enclosing
//     comprehension that (a) binds the name and (b) was entered from a body
//     field (loopCondition/loopStep/result). Ascending from iterRange or
//     accuInit means the identifier is OUTSIDE that comprehension's scope
//     (e.g. the range `x` in `x.map(x, ...)`), so the search continues
//     upward — which also handles shadowing, nearest binding first.
//
//     The binding's kind is read from the side-table (kinds[iterRange.ID()]
//     / kinds[accuInit.ID()]), which post-order guarantees is already
//     computed: navigable.go descends comprehensions in the order iterRange,
//     accuInit, loopCondition, loopStep, result.
//
// Node mutation is the same as optimizer.go: SetKindCase splices, and the
// OptimizerContext factory mints collision-free IDs for the subtraction
// normalization's new unary-minus node.
package timerewrite

import (
	"fmt"

	"github.com/google/cel-go/cel"
	celast "github.com/google/cel-go/common/ast"
)

// VisitorOptimizer returns the normalization as a cel.ASTOptimizer whose
// traversal reuses ast.PreOrderVisit/PostOrderVisit.
func VisitorOptimizer() cel.ASTOptimizer {
	return visitorOptimizer{}
}

// NormalizeAndCheckVisitor is NormalizeAndCheck backed by the visitor-based
// traversal.
func NormalizeAndCheckVisitor(env *cel.Env, expr string) (*cel.Ast, error) {
	parsed, iss := env.Parse(expr)
	if iss != nil && iss.Err() != nil {
		return nil, iss.Err()
	}
	so, err := cel.NewStaticOptimizer(VisitorOptimizer())
	if err != nil {
		return nil, fmt.Errorf("time optimizer construction: %w", err)
	}
	checked, iss := so.Optimize(env, parsed)
	if iss != nil && iss.Err() != nil {
		return nil, iss.Err()
	}
	return checked, nil
}

type visitorOptimizer struct{}

func (visitorOptimizer) Optimize(ctx *cel.OptimizerContext, a *celast.AST) *celast.AST {
	root := a.Expr()

	// Pass 1 (PreOrderVisit): parent links + node registry, for scope
	// resolution by upward walk.
	parent := map[int64]int64{}
	nodes := map[int64]celast.Expr{}
	celast.PreOrderVisit(root, celast.NewExprVisitor(func(e celast.Expr) {
		nodes[e.ID()] = e
		for _, ch := range childrenOf(e) {
			parent[ch.ID()] = e.ID()
		}
	}))

	// Pass 2 (PostOrderVisit): children-first kind synthesis + rewrites.
	v := &flatVisitor{ctx: ctx, kinds: map[int64]kroKind{}, parent: parent, nodes: nodes}
	celast.PostOrderVisit(root, celast.NewExprVisitor(v.visit))
	return a
}

// childrenOf enumerates an expression's direct children.
func childrenOf(e celast.Expr) []celast.Expr {
	switch e.Kind() {
	case celast.CallKind:
		call := e.AsCall()
		var out []celast.Expr
		if call.IsMemberFunction() {
			out = append(out, call.Target())
		}
		return append(out, call.Args()...)
	case celast.SelectKind:
		return []celast.Expr{e.AsSelect().Operand()}
	case celast.ListKind:
		return e.AsList().Elements()
	case celast.MapKind:
		var out []celast.Expr
		for _, entry := range e.AsMap().Entries() {
			me := entry.AsMapEntry()
			out = append(out, me.Key(), me.Value())
		}
		return out
	case celast.StructKind:
		var out []celast.Expr
		for _, field := range e.AsStruct().Fields() {
			out = append(out, field.AsStructField().Value())
		}
		return out
	case celast.ComprehensionKind:
		c := e.AsComprehension()
		return []celast.Expr{c.IterRange(), c.AccuInit(), c.LoopCondition(), c.LoopStep(), c.Result()}
	}
	return nil
}

type flatVisitor struct {
	ctx    *cel.OptimizerContext
	kinds  map[int64]kroKind
	parent map[int64]int64
	nodes  map[int64]celast.Expr
}

func (v *flatVisitor) visit(e celast.Expr) {
	v.kinds[e.ID()] = v.kindOf(e)
}

func (v *flatVisitor) kindOf(e celast.Expr) kroKind {
	switch e.Kind() {
	case celast.IdentKind:
		return v.resolveIdent(e.AsIdent(), e.ID())

	case celast.ListKind:
		elem := kindNone
		for _, el := range e.AsList().Elements() {
			elem = join(elem, v.kinds[el.ID()])
		}
		return listOf(elem)

	case celast.CallKind:
		return v.visitCall(e)

	case celast.ComprehensionKind:
		return v.kinds[e.AsComprehension().Result().ID()]
	}
	// Literals, selects, maps, structs: not kro-value carriers.
	return kindNone
}

// resolveIdent walks the parent chain to the nearest enclosing comprehension
// that binds name AND was entered through a body field. Ascending from
// iterRange/accuInit means the name refers to an outer scope at that level.
func (v *flatVisitor) resolveIdent(name string, id int64) kroKind {
	cur := id
	for {
		pid, ok := v.parent[cur]
		if !ok {
			return kindNone
		}
		p := v.nodes[pid]
		if p != nil && p.Kind() == celast.ComprehensionKind {
			c := p.AsComprehension()
			inBody := cur != c.IterRange().ID() && cur != c.AccuInit().ID()
			if inBody {
				switch name {
				case c.IterVar():
					return v.kinds[c.IterRange().ID()].element()
				case c.IterVar2():
					return kindNone
				case c.AccuVar():
					return v.kinds[c.AccuInit().ID()]
				}
			}
		}
		cur = pid
	}
}

func (v *flatVisitor) visitCall(e celast.Expr) kroKind {
	call := e.AsCall()
	args := call.Args()
	fn := call.FunctionName()

	// time.now(): the source of kro.Timestamp values.
	if fn == "now" && call.IsMemberFunction() &&
		call.Target().Kind() == celast.IdentKind &&
		call.Target().AsIdent() == "time" && len(args) == 0 {
		return kindTs
	}

	if !call.IsMemberFunction() && len(args) == 2 {
		l, r := v.kinds[args[0].ID()], v.kinds[args[1].ID()]

		if mirror, ok := mirroredComparisons[fn]; ok {
			if !l.isTime() && r.isTime() {
				e.SetKindCase(v.ctx.NewCall(mirror, args[1], args[0]))
			}
			return kindNone
		}

		switch fn {
		case "_+_":
			if !l.isTime() && r.isTime() {
				e.SetKindCase(v.ctx.NewCall("_+_", args[1], args[0]))
				l, r = r, l
			}
			if l.isTime() || r.isTime() {
				return addKind(l, r)
			}
			return kindNone
		case "_-_":
			if !l.isTime() && r.isTime() {
				if r == kindTs {
					e.SetKindCase(v.ctx.NewCall("-_", v.ctx.NewCall("_-_", args[1], args[0])))
					return kindDur
				}
				e.SetKindCase(v.ctx.NewCall("_+_", v.ctx.NewCall("-_", args[1]), args[0]))
				return kindNone
			}
			if l.isTime() {
				return subKind(l, r)
			}
			return kindNone
		}
	}

	if fn == "_?_:_" && len(args) == 3 {
		return join(v.kinds[args[1].ID()], v.kinds[args[2].ID()])
	}
	if fn == "_[_]" && len(args) == 2 {
		return v.kinds[args[0].ID()].element()
	}

	switch fn {
	case "timestamp":
		if len(args) == 1 && v.kinds[args[0].ID()] == kindTs {
			return kindTs
		}
	case "duration":
		if len(args) == 1 && v.kinds[args[0].ID()] == kindDur {
			return kindDur
		}
	}
	return kindNone
}
