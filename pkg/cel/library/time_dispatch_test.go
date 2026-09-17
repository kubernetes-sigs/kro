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

package library

// Behavioral corpus for the operator dispatch architecture: two-sided
// declarations + standard-singleton left-trait dispatch + the plan-time
// decorator (time_dispatch.go). No AST rewriting exists; every case here
// compiles directly and must produce the correct value AND the correct
// solved flip.

import (
	"testing"
	"time"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/ext"
)

func dispatchEnv(t *testing.T) *cel.Env {
	t.Helper()
	env, err := cel.NewEnv(
		Time(),
		ext.Bindings(),
		cel.Variable("schema", cel.DynType),
	)
	if err != nil {
		t.Fatalf("env: %v", err)
	}
	return env
}

func TestDispatchBehavioralCorpus(t *testing.T) {
	env := dispatchEnv(t)
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
		wantFlip time.Duration // 0 = no flip expected; -1 = saturation horizon
	}{
		// kro on LHS: standard singleton trait dispatch, decorator fast path.
		{`time.now() >= timestamp(schema.openAt)`, false, 5 * time.Minute},
		{`time.now() - timestamp(schema.openAt) > duration("1m")`, false, 6 * time.Minute},
		// kro on RHS: decorator reroute (mirrored comparison).
		{`timestamp(schema.openAt) <= time.now()`, false, 5 * time.Minute},
		// kro on RHS of +: decorator reroute (commutative swap).
		{`duration("5m") + time.now() >= timestamp(schema.expiry)`, false, 5 * time.Minute},
		// plain − kro (remaining-time shape): decorator −(kro − plain).
		{`timestamp(schema.expiry) - time.now() < duration("5m")`, false, 5 * time.Minute},
		// bind-carried kro on RHS.
		{`cel.bind(deadline, time.now() + duration("5m"), timestamp(schema.openAt) < deadline)`, false, 0},
		// plain expressions untouched by the decorator fast path.
		{`schema.n < 3 && size([time.now()]) + 1 > 0`, true, 0},
		// escape hatch.
		{`string(time.now())`, "2026-09-17T12:00:00Z", 0},
		// far-range saturation.
		{`time.now() < timestamp(schema.future)`, true, -1},
		// comprehension-carried kro values, both scoping shapes.
		{`cel.bind(x, [time.now()], x.map(x, timestamp(schema.openAt) < x)[0])`, false, 5 * time.Minute},
		{`[time.now()].map(x, timestamp(schema.openAt) <= x)[0]`, false, 5 * time.Minute},

		// ── the two cases that killed previous architectures ──

		// DYN-AGE (killed the TypeMap-directed rewrite): kro-ness laundered
		// through a dyn-infected subtraction, then used on the RHS of +.
		// Value-level dispatch does not care what the checker knew.
		// expiry + (now − expiry) ≡ now, so the gate is now >= expiry:
		// false until expiry, flipping at +10m.
		{`cel.bind(age, time.now() - timestamp(schema.expiry), timestamp(schema.expiry) + age >= timestamp(schema.expiry))`, false, 10 * time.Minute},
		// MAP-SELECT (killed the syntactic walkers): kro value reached
		// through a map-literal field, on the comparison's RHS.
		{`cel.bind(m, {"t": time.now()}, timestamp(schema.openAt) <= m.t)`, false, 5 * time.Minute},
		// ternary-guarded gate (exercises the Eval() entry point of the
		// decorator wrapper; conditionals drive children via Eval).
		{`timestamp(schema.openAt) <= time.now() ? "open" : "closed"`, "closed", 5 * time.Minute},
	}

	for _, tc := range cases {
		ast, iss := env.Compile(tc.expr)
		if iss != nil && iss.Err() != nil {
			t.Errorf("%q: compile: %v", tc.expr, iss.Err())
			continue
		}
		prog, err := env.Program(ast, TimeOperatorDecorator())
		if err != nil {
			t.Errorf("%q: program: %v", tc.expr, err)
			continue
		}
		tv := NewTimeValue(now)
		scope := map[string]any{TimeVarName: tv}
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

// TestDispatchRejectsIllTyped: fail-closed pairings are rejected at compile
// time with the real operator name — declarations cover only KREP-legal
// pairs, in both orders.
func TestDispatchRejectsIllTyped(t *testing.T) {
	env := dispatchEnv(t)
	for _, expr := range []string{
		`time.now() >= "oops"`,
		`time.now() + 1`,
		`duration("5m") - time.now()`, // dur − ts is not a KREP operation
		`int(time.now())`,
	} {
		if _, iss := env.Compile(expr); iss == nil || iss.Err() == nil {
			t.Errorf("%q: expected compile rejection", expr)
		}
	}
}

// TestDispatchIllegalDynPairsFailLoudly: illegal pairings that reach eval
// through dyn must error, not silently succeed — the decorator only rescues
// KREP-legal shapes.
func TestDispatchIllegalDynPairsFailLoudly(t *testing.T) {
	env := dispatchEnv(t)
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	for _, expr := range []string{
		`schema.s < time.now()`,                   // string vs kroTs via dyn
		`duration(string(schema.s)) - time.now()`, // would be dur − kroTs
		`schema.n + time.now()`,                   // int + kroTs via dyn
	} {
		ast, iss := env.Compile(expr)
		if iss != nil && iss.Err() != nil {
			continue // compile rejection is fine too
		}
		prog, err := env.Program(ast, TimeOperatorDecorator())
		if err != nil {
			t.Fatalf("%q: program: %v", expr, err)
		}
		_, _, err = prog.Eval(map[string]any{
			TimeVarName: NewTimeValue(now),
			"schema":    map[string]any{"s": "x", "n": int64(1)},
		})
		if err == nil {
			t.Errorf("%q: expected loud eval error for illegal pairing", expr)
		}
	}
}
