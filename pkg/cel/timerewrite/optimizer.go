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

// optimizer.go — POC of the KREP-025 operand normalization as a cel-go
// ASTOptimizer (the library's sanctioned AST-rewriting facility), instead of
// the hand-rolled protobuf walk in timerewrite.go.
//
// What the StaticOptimizer machinery buys us over the proto walk:
//
//   - Node construction through OptimizerContext allocates fresh, collision-
//     free expression IDs (replacing our maxExprID()+1 bookkeeping).
//   - After the pass runs, the optimizer renumbers ALL IDs stably and
//     reconciles macro-call source metadata (normalizeIDs/cleanupMacroRefs)
//     — bookkeeping the proto walk simply doesn't do.
//   - It re-checks the rewritten expression itself, so the entry point
//     returns a CHECKED AST: Parse → Optimize replaces Parse → Rewrite →
//     Check.
//
// The traversal logic (kro-kind inference, comprehension variable scoping,
// swap/mirror normalization) is identical in shape to timerewrite.go, ported
// from exprpb structs to the native ast.Expr interface: kind dispatch via
// Kind()/As*(), in-place node replacement via SetKindCase.
package timerewrite

import (
	"fmt"

	"github.com/google/cel-go/cel"
	celast "github.com/google/cel-go/common/ast"
)

// Optimizer returns the normalization as a cel.ASTOptimizer for use with
// cel.NewStaticOptimizer.
func Optimizer() cel.ASTOptimizer {
	return timeOperatorOptimizer{}
}

// NormalizeAndCheck parses expr, applies the time-operator normalization
// through the StaticOptimizer machinery, and returns the CHECKED AST.
// POC counterpart of krocel.ParseAndCheck.
func NormalizeAndCheck(env *cel.Env, expr string) (*cel.Ast, error) {
	parsed, iss := env.Parse(expr)
	if iss != nil && iss.Err() != nil {
		return nil, iss.Err()
	}
	so, err := cel.NewStaticOptimizer(Optimizer())
	if err != nil {
		return nil, fmt.Errorf("time optimizer construction: %w", err)
	}
	checked, iss := so.Optimize(env, parsed)
	if iss != nil && iss.Err() != nil {
		return nil, iss.Err()
	}
	return checked, nil
}

type timeOperatorOptimizer struct{}

// Optimize implements cel.ASTOptimizer. The input may be parsed-only; the
// StaticOptimizer re-checks after this returns.
func (timeOperatorOptimizer) Optimize(ctx *cel.OptimizerContext, a *celast.AST) *celast.AST {
	w := &nativeWalker{ctx: ctx, vars: map[string][]kroKind{}}
	w.walk(a.Expr())
	return a
}

// nativeWalker mirrors timeRewriter (timerewrite.go) on the native AST.
type nativeWalker struct {
	ctx  *cel.OptimizerContext
	vars map[string][]kroKind
}

func (w *nativeWalker) push(name string, k kroKind) {
	if name == "" {
		return
	}
	w.vars[name] = append(w.vars[name], k)
}

func (w *nativeWalker) pop(name string) {
	if name == "" {
		return
	}
	s := w.vars[name]
	if len(s) <= 1 {
		delete(w.vars, name)
		return
	}
	w.vars[name] = s[:len(s)-1]
}

func (w *nativeWalker) lookup(name string) kroKind {
	s := w.vars[name]
	if len(s) == 0 {
		return kindNone
	}
	return s[len(s)-1]
}

func (w *nativeWalker) walk(e celast.Expr) kroKind {
	if e == nil {
		return kindNone
	}
	switch e.Kind() {
	case celast.LiteralKind, celast.UnspecifiedExprKind:
		return kindNone

	case celast.IdentKind:
		return w.lookup(e.AsIdent())

	case celast.SelectKind:
		w.walk(e.AsSelect().Operand())
		return kindNone

	case celast.ListKind:
		elem := kindNone
		for _, el := range e.AsList().Elements() {
			elem = join(elem, w.walk(el))
		}
		return listOf(elem)

	case celast.MapKind:
		for _, entry := range e.AsMap().Entries() {
			me := entry.AsMapEntry()
			w.walk(me.Key())
			w.walk(me.Value())
		}
		return kindNone

	case celast.StructKind:
		for _, field := range e.AsStruct().Fields() {
			w.walk(field.AsStructField().Value())
		}
		return kindNone

	case celast.CallKind:
		return w.walkCall(e)

	case celast.ComprehensionKind:
		return w.walkComprehension(e.AsComprehension())
	}
	return kindNone
}

func (w *nativeWalker) walkCall(e celast.Expr) kroKind {
	call := e.AsCall()
	if t := call.Target(); t != nil {
		w.walk(t)
	}
	args := call.Args()
	argKinds := make([]kroKind, len(args))
	for i, arg := range args {
		argKinds[i] = w.walk(arg)
	}

	fn := call.FunctionName()

	// time.now(): the source of kro.Timestamp values.
	if fn == "now" && call.IsMemberFunction() &&
		call.Target().Kind() == celast.IdentKind &&
		call.Target().AsIdent() == "time" && len(args) == 0 {
		return kindTs
	}

	if !call.IsMemberFunction() && len(args) == 2 {
		l, r := argKinds[0], argKinds[1]

		// plain ⋛ kro  ⇒  kro (mirrored ⋛) plain
		if mirror, ok := mirroredComparisons[fn]; ok {
			if !l.isTime() && r.isTime() {
				e.SetKindCase(w.ctx.NewCall(mirror, args[1], args[0]))
			}
			return kindNone
		}

		switch fn {
		case "_+_":
			if !l.isTime() && r.isTime() {
				e.SetKindCase(w.ctx.NewCall("_+_", args[1], args[0]))
				l, r = r, l
			}
			if l.isTime() || r.isTime() {
				return addKind(l, r)
			}
			return kindNone
		case "_-_":
			if !l.isTime() && r.isTime() {
				w.normalizeSubtraction(e, args[0], args[1], r)
				if r == kindTs {
					return kindDur
				}
				return kindNone
			}
			if l.isTime() {
				return subKind(l, r)
			}
			return kindNone
		}
	}

	// Ternary carries either branch's value.
	if fn == "_?_:_" && len(args) == 3 {
		return join(argKinds[1], argKinds[2])
	}

	// Index into a list of kro values extracts the element.
	if fn == "_[_]" && len(args) == 2 {
		return argKinds[0].element()
	}

	switch fn {
	case "timestamp":
		if len(argKinds) == 1 && argKinds[0] == kindTs {
			return kindTs
		}
	case "duration":
		if len(argKinds) == 1 && argKinds[0] == kindDur {
			return kindDur
		}
	}
	return kindNone
}

// normalizeSubtraction rewrites `plain − kro` in place, mirroring
// timerewrite.go, but with node construction (and fresh ID allocation)
// delegated to the OptimizerContext factory:
//
//	plainTs − kroTs   ⇒  −(kroTs − plainTs)
//	plain   − kroDur  ⇒  (−kroDur) + plain
func (w *nativeWalker) normalizeSubtraction(e celast.Expr, plain, kro celast.Expr, rhsKind kroKind) {
	if rhsKind == kindTs {
		e.SetKindCase(w.ctx.NewCall("-_", w.ctx.NewCall("_-_", kro, plain)))
		return
	}
	e.SetKindCase(w.ctx.NewCall("_+_", w.ctx.NewCall("-_", kro), plain))
}

func (w *nativeWalker) walkComprehension(comp celast.ComprehensionExpr) kroKind {
	rangeKind := w.walk(comp.IterRange())
	accuKind := w.walk(comp.AccuInit())

	w.push(comp.IterVar(), rangeKind.element())
	w.push(comp.IterVar2(), kindNone)
	w.push(comp.AccuVar(), accuKind)

	w.walk(comp.LoopCondition())
	w.walk(comp.LoopStep())
	resultKind := w.walk(comp.Result())

	w.pop(comp.AccuVar())
	w.pop(comp.IterVar2())
	w.pop(comp.IterVar())

	return resultKind
}
