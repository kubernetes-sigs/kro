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

// timerewrite.go is the KREP-025 operand normalization. kro time values
// (kro.Timestamp / kro.Duration) participate in the standard operators via
// two mechanisms: overload DECLARATIONS merged onto the operators (the
// checker side — see pkg/cel/library/time_functions.go) and trait dispatch
// (the runtime side — CEL's standard operators dispatch through the LEFT
// operand's Comparer/Adder/Subtractor traits, which the kro types implement
// with requeue solving).
//
// Both mechanisms are left-biased, so the only expressions needing help are
// those with a kro value on the RIGHT of a plain operand. This pass runs
// between Parse and Check, infers bottom-up which subexpressions produce a
// kro time value, and normalizes:
//
//	plain <  kro   ⇒  kro >  plain      (mirrored comparison)
//	plain <= kro   ⇒  kro >= plain
//	plain +  kro   ⇒  kro +  plain      (commutative)
//	plainTs − kroTs  ⇒  −(kroTs − plainTs)
//	plain   − kroDur ⇒  (−kroDur) + plain
//
// Comparisons and addition are pure argument swaps (no nodes added, all
// expression IDs preserved). The two subtraction forms wrap one new unary
// minus node with a fresh ID. Expressions with the kro value already on
// the left — or with no kro value at all — are untouched: static overload
// resolution and trait dispatch handle them without any rewriting.
package timerewrite

import (
	"github.com/google/cel-go/cel"
	exprpb "google.golang.org/genproto/googleapis/api/expr/v1alpha1"
)

// mirroredComparisons maps a comparison to its operand-swapped equivalent.
var mirroredComparisons = map[string]string{
	"_<_":  "_>_",
	"_<=_": "_>=_",
	"_>_":  "_<_",
	"_>=_": "_<=_",
}

// kroKind classifies what a subexpression's VALUE is, as far as the
// normalization needs to know.
type kroKind int

const (
	kindNone    kroKind = iota // not a kro time value
	kindTs                     // kro.Timestamp
	kindDur                    // kro.Duration
	kindListTs                 // list whose elements are kro.Timestamp
	kindListDur                // list whose elements are kro.Duration
)

func (k kroKind) isTime() bool { return k == kindTs || k == kindDur }

func (k kroKind) element() kroKind {
	switch k {
	case kindListTs:
		return kindTs
	case kindListDur:
		return kindDur
	}
	return kindNone
}

func listOf(k kroKind) kroKind {
	switch k {
	case kindTs:
		return kindListTs
	case kindDur:
		return kindListDur
	}
	return kindNone
}

// join merges the kinds of alternative branches (ternary, list elements).
func join(a, b kroKind) kroKind {
	if a == kindNone {
		return b
	}
	return a
}

// RewriteTimeOperators normalizes operators with a time.now()-derived value
// on the right of a plain operand so that left-biased overload declarations
// and trait dispatch apply. The input must be a parsed (not necessarily
// checked) AST; the result preserves source info and should be passed to
// env.Check.
func RewriteTimeOperators(a *cel.Ast) (*cel.Ast, error) {
	pe, err := cel.AstToParsedExpr(a)
	if err != nil {
		return nil, err
	}
	rw := &timeRewriter{
		vars:   map[string][]kroKind{},
		nextID: maxExprID(pe.GetExpr()) + 1,
	}
	rw.walk(pe.GetExpr())
	return cel.ParsedExprToAstWithSource(pe, a.Source()), nil
}

// timeRewriter tracks comprehension variables (cel.bind values, map/filter
// iterators) bound to kro time values, and allocates fresh expression IDs
// for the unary-minus nodes the subtraction normalization introduces.
type timeRewriter struct {
	vars   map[string][]kroKind
	nextID int64
}

func (rw *timeRewriter) freshID() int64 {
	id := rw.nextID
	rw.nextID++
	return id
}

func (rw *timeRewriter) push(name string, k kroKind) {
	if name == "" {
		return
	}
	rw.vars[name] = append(rw.vars[name], k)
}

func (rw *timeRewriter) pop(name string) {
	if name == "" {
		return
	}
	s := rw.vars[name]
	if len(s) <= 1 {
		delete(rw.vars, name)
		return
	}
	rw.vars[name] = s[:len(s)-1]
}

func (rw *timeRewriter) lookup(name string) kroKind {
	s := rw.vars[name]
	if len(s) == 0 {
		return kindNone
	}
	return s[len(s)-1]
}

// walk infers the kro kind of e's value bottom-up and normalizes operator
// calls in place.
func (rw *timeRewriter) walk(e *exprpb.Expr) kroKind {
	if e == nil {
		return kindNone
	}
	switch node := e.GetExprKind().(type) {
	case *exprpb.Expr_ConstExpr:
		return kindNone

	case *exprpb.Expr_IdentExpr:
		return rw.lookup(node.IdentExpr.GetName())

	case *exprpb.Expr_SelectExpr:
		// A select from a map/struct is not tracked (its static type still
		// reaches the checker, and kro-on-LHS needs no rewriting anyway).
		rw.walk(node.SelectExpr.GetOperand())
		return kindNone

	case *exprpb.Expr_ListExpr:
		elem := kindNone
		for _, el := range node.ListExpr.GetElements() {
			elem = join(elem, rw.walk(el))
		}
		return listOf(elem)

	case *exprpb.Expr_StructExpr:
		for _, entry := range node.StructExpr.GetEntries() {
			rw.walk(entry.GetMapKey())
			rw.walk(entry.GetValue())
		}
		return kindNone

	case *exprpb.Expr_CallExpr:
		return rw.walkCall(node.CallExpr)

	case *exprpb.Expr_ComprehensionExpr:
		return rw.walkComprehension(node.ComprehensionExpr)
	}
	return kindNone
}

func (rw *timeRewriter) walkCall(call *exprpb.Expr_Call) kroKind {
	if t := call.GetTarget(); t != nil {
		rw.walk(t)
	}
	argKinds := make([]kroKind, len(call.GetArgs()))
	for i, arg := range call.GetArgs() {
		argKinds[i] = rw.walk(arg)
	}

	fn := call.GetFunction()

	// time.now(): the source of kro.Timestamp values.
	if fn == "now" &&
		call.GetTarget().GetIdentExpr().GetName() == "time" &&
		len(call.GetArgs()) == 0 {
		return kindTs
	}

	if call.GetTarget() == nil && len(call.GetArgs()) == 2 {
		l, r := argKinds[0], argKinds[1]

		// plain ⋛ kro  ⇒  kro (mirrored ⋛) plain
		if mirror, ok := mirroredComparisons[fn]; ok {
			if !l.isTime() && r.isTime() {
				call.Function = mirror
				call.Args[0], call.Args[1] = call.Args[1], call.Args[0]
			}
			return kindNone // comparisons yield bool
		}

		switch fn {
		case "_+_":
			// plain + kro  ⇒  kro + plain (commutative).
			if !l.isTime() && r.isTime() {
				call.Args[0], call.Args[1] = call.Args[1], call.Args[0]
				l, r = r, l
			}
			if l.isTime() || r.isTime() {
				return addKind(l, r)
			}
			return kindNone
		case "_-_":
			if !l.isTime() && r.isTime() {
				rw.normalizeSubtraction(call, r)
				// ts−kroTs → dur; plain−kroDur → same kind as the plain side
				// (unknowable here); either way the checker types the result
				// from the declared overloads — report the conservative kind.
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
	if fn == "_?_:_" && len(call.GetArgs()) == 3 {
		return join(argKinds[1], argKinds[2])
	}

	// Index into a list of kro values extracts the element.
	if fn == "_[_]" && len(call.GetArgs()) == 2 {
		return argKinds[0].element()
	}

	// Whitelisted identity casts keep the value a solver value; string() is
	// the escape hatch (a plain string); everything else — size(), helper
	// functions — is treated as NOT producing a kro value. A kro value that
	// escapes through dyn still works when it lands on an operator's LEFT
	// (trait dispatch); on the right of a plain operand it errors loudly.
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

// normalizeSubtraction rewrites `plain − kro` in place:
//
//	plainTs − kroTs   ⇒  −(kroTs − plainTs)     (result: kro.Duration)
//	plain   − kroDur  ⇒  (−kroDur) + plain      (result matches plain's kind)
//
// Each form introduces exactly one unary-minus node with a fresh ID. An
// illegal pairing (e.g. plainDur − kroTs, whose swapped form kroTs−plainDur
// types as kro.Timestamp and cannot be negated) fails closed at check time.
func (rw *timeRewriter) normalizeSubtraction(call *exprpb.Expr_Call, rhsKind kroKind) {
	plain, kro := call.Args[0], call.Args[1]
	if rhsKind == kindTs {
		// −( kro − plain )
		inner := &exprpb.Expr{
			Id: rw.freshID(),
			ExprKind: &exprpb.Expr_CallExpr{CallExpr: &exprpb.Expr_Call{
				Function: "_-_",
				Args:     []*exprpb.Expr{kro, plain},
			}},
		}
		call.Function = "-_"
		call.Args = []*exprpb.Expr{inner}
		return
	}
	// ( −kro ) + plain
	neg := &exprpb.Expr{
		Id: rw.freshID(),
		ExprKind: &exprpb.Expr_CallExpr{CallExpr: &exprpb.Expr_Call{
			Function: "-_",
			Args:     []*exprpb.Expr{kro},
		}},
	}
	call.Function = "_+_"
	call.Args = []*exprpb.Expr{neg, plain}
}

// addKind / subKind mirror the KREP definitions table for the value kind of
// arithmetic whose operands include a kro time value. These kinds only steer
// LATER normalization decisions; the checker's declared overload result
// types are the authority for typing.
func addKind(l, r kroKind) kroKind {
	if l == kindTs || r == kindTs {
		return kindTs
	}
	return kindDur
}

func subKind(l, r kroKind) kroKind {
	if l == kindTs && r == kindTs {
		return kindDur
	}
	if l == kindTs && r == kindNone {
		// now() − plain: could be ts−ts (→dur) or ts−dur (→ts); assume the
		// common "age" shape (→dur). A wrong guess only affects a LATER
		// plain−kro normalization choice, which then fails closed at check.
		return kindDur
	}
	if l == kindTs {
		return kindTs // ts − kroDur → ts
	}
	return kindDur
}

// walkComprehension threads value kinds through CEL's variable-binding
// constructs. cel.bind expands to a comprehension whose accuInit is the
// bound value; map/filter bind the iterVar per element of the range.
func (rw *timeRewriter) walkComprehension(comp *exprpb.Expr_Comprehension) kroKind {
	rangeKind := rw.walk(comp.GetIterRange())
	accuKind := rw.walk(comp.GetAccuInit())

	rw.push(comp.GetIterVar(), rangeKind.element())
	rw.push(comp.GetIterVar2(), kindNone)
	rw.push(comp.GetAccuVar(), accuKind)

	rw.walk(comp.GetLoopCondition())
	rw.walk(comp.GetLoopStep())
	resultKind := rw.walk(comp.GetResult())

	rw.pop(comp.GetAccuVar())
	rw.pop(comp.GetIterVar2())
	rw.pop(comp.GetIterVar())

	return resultKind
}

// maxExprID returns the largest expression ID in the tree, so normalization
// can mint fresh, non-colliding IDs.
func maxExprID(e *exprpb.Expr) int64 {
	if e == nil {
		return 0
	}
	maxID := e.GetId()
	visit := func(child *exprpb.Expr) {
		if m := maxExprID(child); m > maxID {
			maxID = m
		}
	}
	switch node := e.GetExprKind().(type) {
	case *exprpb.Expr_CallExpr:
		visit(node.CallExpr.GetTarget())
		for _, a := range node.CallExpr.GetArgs() {
			visit(a)
		}
	case *exprpb.Expr_SelectExpr:
		visit(node.SelectExpr.GetOperand())
	case *exprpb.Expr_ListExpr:
		for _, el := range node.ListExpr.GetElements() {
			visit(el)
		}
	case *exprpb.Expr_StructExpr:
		for _, en := range node.StructExpr.GetEntries() {
			visit(en.GetMapKey())
			visit(en.GetValue())
		}
	case *exprpb.Expr_ComprehensionExpr:
		c := node.ComprehensionExpr
		visit(c.GetIterRange())
		visit(c.GetAccuInit())
		visit(c.GetLoopCondition())
		visit(c.GetLoopStep())
		visit(c.GetResult())
	}
	return maxID
}
