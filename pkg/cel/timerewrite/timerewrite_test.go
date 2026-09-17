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
	"strings"
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

func rewriteFunctions(t *testing.T, expr string) map[string]int {
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
	fns := map[string]int{}
	functionsIn(pe.GetExpr(), fns)
	return fns
}

func TestRewriteRenamesTaintedOperators(t *testing.T) {
	cases := []struct {
		expr        string
		wantRenamed []string
		wantKept    []string
	}{
		// now on LHS of comparison
		{`time.now() >= timestamp(schema.openAt)`, []string{"kro.time.ge"}, nil},
		// now on RHS
		{`timestamp(schema.openAt) <= time.now()`, []string{"kro.time.le"}, nil},
		// arithmetic chains taint into outer comparison
		{`time.now() - timestamp(schema.t) > duration("5s")`,
			[]string{"kro.time.sub", "kro.time.gt"}, nil},
		// plain operators stay untouched
		{`1 + 2 < 4 && timestamp(schema.a) < timestamp(schema.b)`,
			nil, []string{"_+_", "_<_"}},
		// mixed: only the tainted comparison is renamed
		{`schema.n < 3 && time.now() < timestamp(schema.t)`,
			[]string{"kro.time.lt"}, []string{"_<_"}},
		// taint through cel.bind
		{`cel.bind(deadline, time.now() + duration("5m"), timestamp(schema.t) < deadline)`,
			[]string{"kro.time.add", "kro.time.lt"}, nil},
		// taint through a map comprehension's iterator
		{`[time.now()].map(x, x < timestamp(schema.t))`, []string{"kro.time.lt"}, nil},
		// string() launders taint: comparison on the string is NOT renamed
		{`string(time.now()) < schema.s`, nil, []string{"_<_"}},
	}
	for _, tc := range cases {
		fns := rewriteFunctions(t, tc.expr)
		for _, want := range tc.wantRenamed {
			if fns[want] == 0 {
				t.Errorf("%q: expected %s in rewritten AST, got %v", tc.expr, want, fns)
			}
		}
		for _, keep := range tc.wantKept {
			if fns[keep] == 0 {
				t.Errorf("%q: expected %s kept in rewritten AST, got %v", tc.expr, keep, fns)
			}
		}
	}
}

func TestRewritePreservesIDsAndSource(t *testing.T) {
	env := parseEnv(t)
	expr := `time.now() >= timestamp(schema.openAt)`
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
		t.Fatalf("node count changed: %d -> %d", len(beforeIDs), len(afterIDs))
	}
	for id := range beforeIDs {
		if !afterIDs[id] {
			t.Fatalf("expression ID %d lost in rewrite", id)
		}
	}
	if !strings.Contains(rewritten.Source().Content(), "time.now()") {
		t.Fatalf("source content lost")
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
