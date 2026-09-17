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

package timerewrite

import (
	"testing"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/ext"
	exprpb "google.golang.org/genproto/googleapis/api/expr/v1alpha1"
)

func parseEnv(t *testing.T) *cel.Env {
	t.Helper()
	// Parse-only concerns: the rewrite runs before checking, so variables
	// and functions need not be declared. Bindings ext is needed so
	// cel.bind parses into its comprehension form.
	env, err := cel.NewEnv(ext.Bindings(), cel.Variable("schema", cel.DynType))
	if err != nil {
		t.Fatalf("env: %v", err)
	}
	return env
}

func rewrite(t *testing.T, expr string) *exprpb.ParsedExpr {
	t.Helper()
	env := parseEnv(t)
	parsed, iss := env.Parse(expr)
	if iss != nil && iss.Err() != nil {
		t.Fatalf("parse %q: %v", expr, iss.Err())
	}
	rewritten, err := RewriteTimeOperators(parsed)
	if err != nil {
		t.Fatalf("rewrite %q: %v", expr, err)
	}
	pe, err := cel.AstToParsedExpr(rewritten)
	if err != nil {
		t.Fatalf("to proto: %v", err)
	}
	return pe
}

// functionsIn collects all call function names in the AST.
func functionsIn(e *exprpb.Expr, out map[string]int) {
	if e == nil {
		return
	}
	switch node := e.GetExprKind().(type) {
	case *exprpb.Expr_CallExpr:
		out[node.CallExpr.GetFunction()]++
		functionsIn(node.CallExpr.GetTarget(), out)
		for _, a := range node.CallExpr.GetArgs() {
			functionsIn(a, out)
		}
	case *exprpb.Expr_SelectExpr:
		functionsIn(node.SelectExpr.GetOperand(), out)
	case *exprpb.Expr_ListExpr:
		for _, el := range node.ListExpr.GetElements() {
			functionsIn(el, out)
		}
	case *exprpb.Expr_StructExpr:
		for _, en := range node.StructExpr.GetEntries() {
			functionsIn(en.GetMapKey(), out)
			functionsIn(en.GetValue(), out)
		}
	case *exprpb.Expr_ComprehensionExpr:
		c := node.ComprehensionExpr
		functionsIn(c.GetIterRange(), out)
		functionsIn(c.GetAccuInit(), out)
		functionsIn(c.GetLoopCondition(), out)
		functionsIn(c.GetLoopStep(), out)
		functionsIn(c.GetResult(), out)
	}
}

func TestNormalizationSwapsKroToLeft(t *testing.T) {
	cases := []struct {
		expr string
		want map[string]int // function → count expected AFTER rewrite
	}{
		// kro already on the left: untouched.
		{`time.now() >= timestamp(schema.openAt)`, map[string]int{"_>=_": 1}},
		{`time.now() - timestamp(schema.t) > duration("5s")`, map[string]int{"_-_": 1, "_>_": 1}},
		// kro on the right of a plain operand: comparison mirrored.
		{`timestamp(schema.openAt) <= time.now()`, map[string]int{"_>=_": 1, "_<=_": 0}},
		{`timestamp(schema.openAt) < time.now()`, map[string]int{"_>_": 1, "_<_": 0}},
		// plain + kro: swapped, operator unchanged.
		{`duration("5m") + time.now() >= timestamp(schema.t)`, map[string]int{"_+_": 1, "_>=_": 1}},
		// plainTs − kroTs ⇒ −(kroTs − plainTs): one new unary minus. The
		// negated result is a kro value on the LEFT of <, so the comparison
		// itself correctly stays unmirrored.
		{`timestamp(schema.expiry) - time.now() < duration("5m")`,
			map[string]int{"-_": 1, "_-_": 1, "_<_": 1}},
		// no time involved: fully untouched.
		{`schema.n < 3 && 1 + 2 > 0`, map[string]int{"_<_": 1, "_+_": 1, "_>_": 1}},
		// value-directed, not contains-based: size() yields an int.
		{`size([time.now()]) + 1 > 0`, map[string]int{"_+_": 1, "_>_": 1}},
		// mixed: only the kro-on-right comparison is mirrored.
		{`schema.n < 3 && timestamp(schema.t) < time.now()`, map[string]int{"_<_": 1, "_>_": 1}},
		// bind-carried kro value on the right: mirrored.
		{`cel.bind(deadline, time.now() + duration("5m"), timestamp(schema.t) < deadline)`,
			map[string]int{"_>_": 1, "_+_": 1}},
		// iterator-carried kro on the right: mirrored inside the loop.
		{`[time.now()].map(x, timestamp(schema.t) <= x)`, map[string]int{"_>=_": 1}},
		// index extracts kro element; already LHS: untouched.
		{`[time.now()][0] >= timestamp(schema.t)`, map[string]int{"_>=_": 1}},
		// ternary carries kro; on the right: mirrored.
		{`timestamp(schema.t) < (schema.b ? time.now() : time.now() + duration("1m"))`,
			map[string]int{"_>_": 1, "_<_": 0}},
		// string() launders: not a kro value, comparison untouched.
		{`string(time.now()) < schema.s`, map[string]int{"_<_": 1}},
	}
	for _, tc := range cases {
		pe := rewrite(t, tc.expr)
		fns := map[string]int{}
		functionsIn(pe.GetExpr(), fns)
		for fn, want := range tc.want {
			if fns[fn] != want {
				t.Errorf("%q: function %s count = %d, want %d (all: %v)",
					tc.expr, fn, fns[fn], want, fns)
			}
		}
	}
}

func TestSwapReversesArguments(t *testing.T) {
	// timestamp(schema.openAt) <= time.now()  ⇒  time.now() >= timestamp(...)
	pe := rewrite(t, `timestamp(schema.openAt) <= time.now()`)
	root := pe.GetExpr().GetCallExpr()
	if root.GetFunction() != "_>=_" {
		t.Fatalf("root function = %s, want _>=_", root.GetFunction())
	}
	lhs := root.GetArgs()[0].GetCallExpr()
	if lhs.GetFunction() != "now" {
		t.Fatalf("lhs after swap = %v, want the now() call", root.GetArgs()[0])
	}
}

func TestPureSwapPreservesIDs(t *testing.T) {
	env := parseEnv(t)
	expr := `timestamp(schema.openAt) <= time.now()`
	parsed, iss := env.Parse(expr)
	if iss != nil && iss.Err() != nil {
		t.Fatalf("parse: %v", iss.Err())
	}
	before, _ := cel.AstToParsedExpr(parsed)
	beforeIDs := map[int64]bool{}
	collectIDs(before.GetExpr(), beforeIDs)

	rewritten, err := RewriteTimeOperators(parsed)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	after, _ := cel.AstToParsedExpr(rewritten)
	afterIDs := map[int64]bool{}
	collectIDs(after.GetExpr(), afterIDs)

	if len(beforeIDs) != len(afterIDs) {
		t.Fatalf("node count changed on a pure swap: %d -> %d", len(beforeIDs), len(afterIDs))
	}
	for id := range beforeIDs {
		if !afterIDs[id] {
			t.Fatalf("expression ID %d lost in rewrite", id)
		}
	}
}

func TestSubtractionNormalizationAddsOneFreshID(t *testing.T) {
	env := parseEnv(t)
	expr := `timestamp(schema.expiry) - time.now()`
	parsed, iss := env.Parse(expr)
	if iss != nil && iss.Err() != nil {
		t.Fatalf("parse: %v", iss.Err())
	}
	before, _ := cel.AstToParsedExpr(parsed)
	beforeIDs := map[int64]bool{}
	collectIDs(before.GetExpr(), beforeIDs)

	rewritten, err := RewriteTimeOperators(parsed)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	after, _ := cel.AstToParsedExpr(rewritten)
	afterIDs := map[int64]bool{}
	collectIDs(after.GetExpr(), afterIDs)

	if len(afterIDs) != len(beforeIDs)+1 {
		t.Fatalf("node count = %d, want %d (exactly one new unary minus)", len(afterIDs), len(beforeIDs)+1)
	}
	for id := range beforeIDs {
		if !afterIDs[id] {
			t.Fatalf("pre-existing expression ID %d lost", id)
		}
	}
	// Shape: -_( _-_( now(), plain ) )
	root := after.GetExpr().GetCallExpr()
	if root.GetFunction() != "-_" || len(root.GetArgs()) != 1 {
		t.Fatalf("root = %s/%d args, want unary -_", root.GetFunction(), len(root.GetArgs()))
	}
	inner := root.GetArgs()[0].GetCallExpr()
	if inner.GetFunction() != "_-_" {
		t.Fatalf("inner = %s, want _-_", inner.GetFunction())
	}
	if inner.GetArgs()[0].GetCallExpr().GetFunction() != "now" {
		t.Fatalf("inner lhs should be the now() call after normalization")
	}
}

func collectIDs(e *exprpb.Expr, out map[int64]bool) {
	if e == nil {
		return
	}
	out[e.GetId()] = true
	switch node := e.GetExprKind().(type) {
	case *exprpb.Expr_CallExpr:
		collectIDs(node.CallExpr.GetTarget(), out)
		for _, a := range node.CallExpr.GetArgs() {
			collectIDs(a, out)
		}
	case *exprpb.Expr_SelectExpr:
		collectIDs(node.SelectExpr.GetOperand(), out)
	case *exprpb.Expr_ListExpr:
		for _, el := range node.ListExpr.GetElements() {
			collectIDs(el, out)
		}
	case *exprpb.Expr_StructExpr:
		for _, en := range node.StructExpr.GetEntries() {
			collectIDs(en.GetMapKey(), out)
			collectIDs(en.GetValue(), out)
		}
	case *exprpb.Expr_ComprehensionExpr:
		c := node.ComprehensionExpr
		collectIDs(c.GetIterRange(), out)
		collectIDs(c.GetAccuInit(), out)
		collectIDs(c.GetLoopCondition(), out)
		collectIDs(c.GetLoopStep(), out)
		collectIDs(c.GetResult(), out)
	}
}
