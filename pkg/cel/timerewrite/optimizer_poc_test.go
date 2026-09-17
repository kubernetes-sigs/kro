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

package timerewrite_test

// External test package: the behavioral corpus needs the time library
// (pkg/cel/library), which cannot be imported from an in-package test
// without a cycle through pkg/cel.

import (
	"testing"
	"time"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/ext"

	"github.com/kubernetes-sigs/kro/pkg/cel/library"
	"github.com/kubernetes-sigs/kro/pkg/cel/timerewrite"
)

func optimizerEnv(t *testing.T) *cel.Env {
	t.Helper()
	env, err := cel.NewEnv(
		library.Time(),
		ext.Bindings(),
		cel.Variable("schema", cel.DynType),
	)
	if err != nil {
		t.Fatalf("env: %v", err)
	}
	return env
}

// TestOptimizerPOCBehavioralParity runs the key behavioral corpus through
// the ASTOptimizer path (NormalizeAndCheck) and asserts identical results
// and solved flips to the proto-walk path.
func TestOptimizerPOCBehavioralParity(t *testing.T) {
	for name, normalize := range map[string]func(*cel.Env, string) (*cel.Ast, error){
		"recursive": timerewrite.NormalizeAndCheck,
		"visitor":   timerewrite.NormalizeAndCheckVisitor,
	} {
		t.Run(name, func(t *testing.T) { runBehavioralParity(t, normalize) })
	}
}

func runBehavioralParity(t *testing.T, normalize func(*cel.Env, string) (*cel.Ast, error)) {
	env := optimizerEnv(t)
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	vars := map[string]any{"schema": map[string]any{
		"openAt": now.Add(5 * time.Minute).Format(time.RFC3339),
		"expiry": now.Add(10 * time.Minute).Format(time.RFC3339),
		"future": "9999-12-31T23:59:59Z",
		"n":      int64(1),
		"s":      "x",
	}}

	cases := []struct {
		expr     string
		want     any
		wantFlip time.Duration // 0 = no flip expected
	}{
		// kro on LHS (no rewriting needed at all)
		{`time.now() >= timestamp(schema.openAt)`, false, 5 * time.Minute},
		{`time.now() - timestamp(schema.openAt) > duration("1m")`, false, 6 * time.Minute},
		// kro on RHS: mirrored comparison
		{`timestamp(schema.openAt) <= time.now()`, false, 5 * time.Minute},
		// kro on RHS of +: swapped
		{`duration("5m") + time.now() >= timestamp(schema.expiry)`, false, 5 * time.Minute},
		// plain − kro (remaining-time shape): −(kro − plain)
		{`timestamp(schema.expiry) - time.now() < duration("5m")`, false, 5 * time.Minute},
		// bind-carried kro on RHS (macro metadata must survive the optimizer)
		{`cel.bind(deadline, time.now() + duration("5m"), timestamp(schema.openAt) < deadline)`, false, 0},
		// plain expressions untouched
		{`schema.n < 3 && size([time.now()]) + 1 > 0`, true, 0},
		// escape hatch
		{`string(time.now())`, "2026-09-17T12:00:00Z", 0},
		// far-range saturation fix carries over
		{`time.now() < timestamp(schema.future)`, true, -1}, // -1: flip allowed only at saturation horizon
		// SCOPING EDGES for the flat-visitor parent-walk:
		// range ident shadowed by same-named iterVar: the range `x` is the
		// OUTER bind (a kro list); the loop `x` is the element (kro ts).
		{`cel.bind(x, [time.now()], x.map(x, timestamp(schema.openAt) < x)[0])`, false, 5 * time.Minute},
		// iterator-carried kro value on the comparison's RIGHT: mirrored.
		{`[time.now()].map(x, timestamp(schema.openAt) <= x)[0]`, false, 5 * time.Minute},
	}

	for _, tc := range cases {
		checked, err := normalize(env, tc.expr)
		if err != nil {
			t.Errorf("%q: NormalizeAndCheck: %v", tc.expr, err)
			continue
		}
		prog, err := env.Program(checked)
		if err != nil {
			t.Errorf("%q: program: %v", tc.expr, err)
			continue
		}
		tv := library.NewTimeValue(now)
		scope := map[string]any{library.TimeVarName: tv}
		for k, v := range vars {
			scope[k] = v
		}
		out, _, err := prog.Eval(scope)
		if err != nil {
			t.Errorf("%q: eval: %v", tc.expr, err)
			continue
		}
		if out.Value() != tc.want {
			t.Errorf("%q = %v, want %v", tc.expr, out.Value(), tc.want)
		}
		flip, ok := tv.EarliestFlip()
		if tc.wantFlip < 0 {
			// Saturation-horizon case: any flip must be >100y out.
			if ok && flip.Before(now.AddDate(100, 0, 0)) {
				t.Errorf("%q: near-term flip %v for saturated operand", tc.expr, flip)
			}
			continue
		}
		switch {
		case tc.wantFlip == 0 && ok:
			t.Errorf("%q: unexpected flip %v", tc.expr, flip)
		case tc.wantFlip != 0 && !ok:
			t.Errorf("%q: expected flip at +%v, got none", tc.expr, tc.wantFlip)
		case tc.wantFlip != 0 && !flip.Equal(now.Add(tc.wantFlip)):
			t.Errorf("%q: flip = %v, want %v", tc.expr, flip, now.Add(tc.wantFlip))
		}
	}
}

// TestOptimizerPOCRejectsIllTyped asserts fail-closed pairings still fail at
// the optimizer's built-in recheck.
func TestOptimizerPOCRejectsIllTyped(t *testing.T) {
	env := optimizerEnv(t)
	for _, expr := range []string{
		`time.now() >= "oops"`,
		`time.now() + 1`,
		`duration("5m") - time.now()`, // dur − ts remains illegal
		`int(time.now())`,
	} {
		if _, err := timerewrite.NormalizeAndCheck(env, expr); err == nil {
			t.Errorf("recursive %q: expected rejection, but it compiled", expr)
		}
		if _, err := timerewrite.NormalizeAndCheckVisitor(env, expr); err == nil {
			t.Errorf("visitor %q: expected rejection, but it compiled", expr)
		}
	}
}
