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

// timerewrite.go is the KREP-025 operator rewrite. kro time values are
// honest, intentionally-limited CEL types (kro.Timestamp / kro.Duration),
// and CEL does not allow adding overloads for standard operators across
// types (cel-go #252/#990). The KREP's "Overriding" section therefore calls
// for rewriting the AST so that operators on time values become ordinary
// function calls, which ARE declarable.
//
// The rewrite runs between Parse and Check and is TYPE-DIRECTED: it infers,
// bottom-up, which subexpressions produce a kro time VALUE — time.now()
// itself, arithmetic on it, values carried through cel.bind and ternaries —
// and renames the six operators (`_<_`, `_<=_`, `_>_`, `_>=_`, `_+_`,
// `_-_`) to kro.time.* calls ONLY when an operand is such a value. It is a
// pure rename: no nodes are added or removed, all expression IDs and source
// positions are preserved.
//
// Expressions that merely mention time.now() without their operands BEING a
// time value (e.g. `size([time.now()]) + 1`) are left untouched and checked
// under normal CEL rules. An ill-typed time operation (e.g.
// `time.now() >= "oops"`) is renamed and then rejected by the checker,
// because kro.time.* declares exactly the legal kro pairs — the same
// compile-time failure any unknown type combination gets.
package timerewrite

import (
	"github.com/google/cel-go/cel"
	exprpb "google.golang.org/genproto/googleapis/api/expr/v1alpha1"
)

// timeOperatorRenames maps CEL operator internal names to the kro.time.*
// functions declared by the time library (pkg/cel/library/time_functions.go).
var timeOperatorRenames = map[string]string{
	"_<_":  "kro.time.lt",
	"_<=_": "kro.time.le",
	"_>_":  "kro.time.gt",
	"_>=_": "kro.time.ge",
	"_+_":  "kro.time.add",
	"_-_":  "kro.time.sub",
}

// kroKind classifies what a subexpression's VALUE is, as far as the rewrite
// needs to know.
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
// Differing time kinds join conservatively to the branch that is a time
// value; a mixed ts/dur join cannot be represented and degrades to the
// first, which the checker will reject if actually misused.
func join(a, b kroKind) kroKind {
	if a == kindNone {
		return b
	}
	return a
}

// RewriteTimeOperators rewrites operators whose operands are
// time.now()-derived values into kro.time.* function calls. The input must
// be a parsed (not necessarily checked) AST; the result preserves source
// info and should be passed to env.Check.
func RewriteTimeOperators(a *cel.Ast) (*cel.Ast, error) {
	pe, err := cel.AstToParsedExpr(a)
	if err != nil {
		return nil, err
	}
	rw := &timeRewriter{vars: map[string][]kroKind{}}
	rw.walk(pe.GetExpr())
	return cel.ParsedExprToAstWithSource(pe, a.Source()), nil
}

// timeRewriter tracks comprehension variables (cel.bind values, map/filter
// iterators) currently bound to kro time values. Values are stacks so
// shadowing nests correctly.
type timeRewriter struct {
	vars map[string][]kroKind
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

// walk infers the kro kind of e's value bottom-up and renames operator calls
// with a kro time operand in place.
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
		// kro values have no fields; selecting from anything yields non-kro.
		// Still walk the operand for nested rewrites.
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
		// Values inside maps/structs are not tracked through lookups.
		return kindNone

	case *exprpb.Expr_CallExpr:
		return rw.walkCall(node.CallExpr)

	case *exprpb.Expr_ComprehensionExpr:
		return rw.walkComprehension(node.ComprehensionExpr)
	}
	return kindNone
}

func (rw *timeRewriter) walkCall(call *exprpb.Expr_Call) kroKind {
	targetKind := kindNone
	if t := call.GetTarget(); t != nil {
		targetKind = rw.walk(t)
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

	// Binary operators: rename when an operand IS a kro time value, and
	// infer the arithmetic result kind per the KREP definitions table.
	if newName, ok := timeOperatorRenames[fn]; ok &&
		call.GetTarget() == nil && len(call.GetArgs()) == 2 {
		l, r := argKinds[0], argKinds[1]
		if l.isTime() || r.isTime() {
			call.Function = newName
			switch fn {
			case "_+_", "_-_":
				return arithmeticKind(fn, l, r)
			default:
				return kindNone // comparisons yield bool
			}
		}
		return kindNone
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
	// functions, macros not otherwise handled — is treated as NOT producing
	// a kro value. If such a function actually returns one at runtime (only
	// possible through dyn), operators on it fail loudly rather than being
	// rewritten.
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
	_ = targetKind
	return kindNone
}

// arithmeticKind mirrors the KREP definitions table for the value kind of a
// renamed + / − whose operands include a kro time value. Plain counterparts
// (a timestamp() or duration() call, a schema field) have kind kindNone; the
// checker validates the actual pairing, so this only needs to be right for
// the LEGAL combinations.
func arithmeticKind(fn string, l, r kroKind) kroKind {
	switch fn {
	case "_+_":
		// ts+dur, dur+ts → ts; dur+dur → dur.
		if l == kindTs || r == kindTs {
			return kindTs
		}
		return kindDur
	case "_-_":
		// ts−ts → dur; ts−dur → ts; dur−dur → dur.
		if l == kindTs && r == kindTs {
			return kindDur
		}
		if l == kindTs {
			return kindTs
		}
		if l == kindNone && r == kindTs {
			// plain − kroTs: ts−ts → dur (any other pairing is rejected
			// by the checker).
			return kindDur
		}
		return kindDur
	}
	return kindNone
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
