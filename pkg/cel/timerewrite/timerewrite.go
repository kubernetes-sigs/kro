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
// The rewrite runs between Parse and Check. It performs a taint analysis on
// the parsed AST — an expression is time-tainted if it contains a
// `time.now()` call, or references a comprehension variable (cel.bind,
// map/filter iterators) whose binding is tainted — and RENAMES the six
// affected operator calls (`_<_`, `_<=_`, `_>_`, `_>=_`, `_+_`, `_-_`) to
// the kro.time.* functions when either operand is tainted. It is a pure
// rename: no nodes are added or removed, all expression IDs and source
// positions are preserved, so error messages still point at the user's
// source.
//
// Taint is deliberately conservative (any containing expression is tainted,
// e.g. `size([time.now()])` taints the enclosing comparison). That is safe
// because the kro.time.* bindings fall back to exact standard-library
// semantics when neither evaluated operand is actually a kro time value.
package timerewrite

import (
	"github.com/google/cel-go/cel"
	exprpb "google.golang.org/genproto/googleapis/api/expr/v1alpha1"
)

// timeOperatorRenames maps CEL operator internal names to the kro.time.*
// functions declared by the time library (pkg/cel/library/time.go).
var timeOperatorRenames = map[string]string{
	"_<_":  "kro.time.lt",
	"_<=_": "kro.time.le",
	"_>_":  "kro.time.gt",
	"_>=_": "kro.time.ge",
	"_+_":  "kro.time.add",
	"_-_":  "kro.time.sub",
}

// comparisonResultFunctions are the renamed functions whose result is a bool
// (taint does not propagate through a comparison's result).
var comparisonResultFunctions = map[string]bool{
	"kro.time.lt": true, "kro.time.le": true,
	"kro.time.gt": true, "kro.time.ge": true,
	"_<_": true, "_<=_": true, "_>_": true, "_>=_": true,
}

// RewriteTimeOperators rewrites operators whose operands may carry
// time.now()-derived values into kro.time.* function calls. The input must
// be a parsed (not necessarily checked) AST; the result preserves source
// info and should be passed to env.Check.
func RewriteTimeOperators(a *cel.Ast) (*cel.Ast, error) {
	pe, err := cel.AstToParsedExpr(a)
	if err != nil {
		return nil, err
	}
	rw := &timeRewriter{taintedVars: map[string]int{}}
	rw.walk(pe.GetExpr())
	return cel.ParsedExprToAstWithSource(pe, a.Source()), nil
}

// timeRewriter tracks comprehension variables currently bound to tainted
// expressions. Values are depth counts so shadowing nests correctly.
type timeRewriter struct {
	taintedVars map[string]int
}

func (rw *timeRewriter) bindVar(name string, tainted bool) {
	if name == "" || !tainted {
		return
	}
	rw.taintedVars[name]++
}

func (rw *timeRewriter) unbindVar(name string, tainted bool) {
	if name == "" || !tainted {
		return
	}
	rw.taintedVars[name]--
	if rw.taintedVars[name] <= 0 {
		delete(rw.taintedVars, name)
	}
}

// walk computes taint bottom-up and renames tainted operator calls in place.
// Returns whether the subtree's VALUE may carry a kro time value.
func (rw *timeRewriter) walk(e *exprpb.Expr) bool {
	if e == nil {
		return false
	}
	switch node := e.GetExprKind().(type) {
	case *exprpb.Expr_ConstExpr:
		return false

	case *exprpb.Expr_IdentExpr:
		return rw.taintedVars[node.IdentExpr.GetName()] > 0

	case *exprpb.Expr_SelectExpr:
		return rw.walk(node.SelectExpr.GetOperand())

	case *exprpb.Expr_ListExpr:
		tainted := false
		for _, el := range node.ListExpr.GetElements() {
			if rw.walk(el) {
				tainted = true
			}
		}
		return tainted

	case *exprpb.Expr_StructExpr:
		tainted := false
		for _, entry := range node.StructExpr.GetEntries() {
			if k := entry.GetMapKey(); k != nil && rw.walk(k) {
				tainted = true
			}
			if rw.walk(entry.GetValue()) {
				tainted = true
			}
		}
		return tainted

	case *exprpb.Expr_CallExpr:
		return rw.walkCall(node.CallExpr)

	case *exprpb.Expr_ComprehensionExpr:
		return rw.walkComprehension(node.ComprehensionExpr)
	}
	return false
}

func (rw *timeRewriter) walkCall(call *exprpb.Expr_Call) bool {
	tainted := false
	if t := call.GetTarget(); t != nil {
		if rw.walk(t) {
			tainted = true
		}
	}
	for _, arg := range call.GetArgs() {
		if rw.walk(arg) {
			tainted = true
		}
	}

	// time.now(): the taint source.
	if call.GetFunction() == "now" &&
		call.GetTarget().GetIdentExpr().GetName() == "time" &&
		len(call.GetArgs()) == 0 {
		return true
	}

	// Rename tainted operators to their kro.time.* equivalents.
	if newName, ok := timeOperatorRenames[call.GetFunction()]; ok &&
		call.GetTarget() == nil && len(call.GetArgs()) == 2 && tainted {
		call.Function = newName
	}

	// A comparison's result is a bool; it cannot carry a time value.
	// string()'s result is the sanctioned escape hatch, also not a carrier.
	if comparisonResultFunctions[call.GetFunction()] || call.GetFunction() == "string" {
		return false
	}
	return tainted
}

// walkComprehension threads taint through CEL's only variable-binding
// construct. cel.bind expands to a comprehension whose accuInit is the bound
// value; map/filter bind the iterVar per element of the range. The loop step
// may fold tainted elements into the accumulator (e.g. map over a tainted
// range), so accumulator taint is computed to a (two-iteration, monotone
// boolean) fixpoint before the result expression is walked.
func (rw *timeRewriter) walkComprehension(comp *exprpb.Expr_Comprehension) bool {
	rangeTainted := rw.walk(comp.GetIterRange())
	accuInitTainted := rw.walk(comp.GetAccuInit())

	rw.bindVar(comp.GetIterVar(), rangeTainted)
	rw.bindVar(comp.GetIterVar2(), rangeTainted)

	// First pass with the accumulator's initial taint.
	rw.bindVar(comp.GetAccuVar(), accuInitTainted)
	rw.walk(comp.GetLoopCondition())
	stepTainted := rw.walk(comp.GetLoopStep())
	rw.unbindVar(comp.GetAccuVar(), accuInitTainted)

	// Fixpoint: if the loop step folds tainted values into the accumulator,
	// re-walk the loop body with the accumulator tainted so renames inside
	// see the final taint state. Renaming is monotone (never undone), so
	// re-walking is safe, and the boolean lattice converges in one retry.
	accuTainted := accuInitTainted || stepTainted
	rw.bindVar(comp.GetAccuVar(), accuTainted)
	if accuTainted && !accuInitTainted {
		rw.walk(comp.GetLoopCondition())
		rw.walk(comp.GetLoopStep())
	}
	resultTainted := rw.walk(comp.GetResult())
	rw.unbindVar(comp.GetAccuVar(), accuTainted)

	rw.unbindVar(comp.GetIterVar(), rangeTainted)
	rw.unbindVar(comp.GetIterVar2(), rangeTainted)

	return resultTainted
}
