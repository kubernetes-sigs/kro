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

// Behavioral corpus for the time operator surface: declarations
// (time_functions.go), trait dispatch (time.go), and the decorator
// (time_dispatch.go). Every case must produce the correct value and the
// correct solved flip.

import (
	"strings"
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

		// kro value carried through a dyn-typed binding, then used on the
		// RHS of +. expiry + (now − expiry) ≡ now, so the gate is
		// now >= expiry: false until expiry, flipping at +10m.
		{`cel.bind(age, time.now() - timestamp(schema.expiry), timestamp(schema.expiry) + age >= timestamp(schema.expiry))`, false, 10 * time.Minute},
		// kro value reached through a map-literal field, on the RHS.
		{`cel.bind(m, {"t": time.now()}, timestamp(schema.openAt) <= m.t)`, false, 5 * time.Minute},
		// ternary gate: conditionals drive children via Eval, covering the
		// wrapper's Eval entry point.
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

// TestDispatchRejectsIllTyped: anything outside the declared surface is a
// compile error. Grouped by the rule that rejects it.
func TestDispatchRejectsIllTyped(t *testing.T) {
	env := dispatchEnv(t)
	for _, expr := range []string{
		// wrong operand types, kro on the left
		`time.now() >= "oops"`,
		`time.now() + 1`,
		`time.now() < 5`,
		`time.now() * 2`,
		// wrong operand types, kro on the right
		`"oops" >= time.now()`,
		`1 + time.now()`,
		`2 * time.now()`,
		// operations between time values that are not defined
		`time.now() + time.now()`,                                       // ts + ts
		`duration("5m") - time.now()`,                                   // dur − ts
		`time.now() < duration("5m")`,                                   // cross-kind comparison
		`duration("5m") > time.now()`,                                   // cross-kind, reversed
		`(time.now() - time.now()) < timestamp("2026-01-01T00:00:00Z")`, // dur vs ts
		// unary minus is declared only on kro.Duration
		`-time.now()`,
		// conversions out of the solver (string() is the only exit)
		`int(time.now())`,
		`double(time.now())`,
		`int(time.now() - time.now())`,
		`duration(time.now())`,
		`timestamp(time.now() - time.now())`,
		// calendar accessors are not part of the surface
		`time.now().getSeconds()`,
		`time.now().getHours()`,
		`time.now().getDayOfWeek()`,
		`time.now().getFullYear()`,
		`(time.now() - time.now()).getHours()`,
		// unknown members on the time scope
		`time.later()`,
		// ternaries cannot join kro with plain or cross-kind values
		`true ? time.now() : timestamp("2026-01-01T00:00:00Z")`,
		`true ? time.now() : duration("5m")`,
		// typed equality and membership need type agreement
		`time.now() == timestamp("2026-01-01T00:00:00Z")`,
		`timestamp("2026-01-01T00:00:00Z") != time.now()`,
		`time.now() in [timestamp("2026-01-01T00:00:00Z")]`,
	} {
		if _, iss := env.Compile(expr); iss == nil || iss.Err() == nil {
			t.Errorf("%q: expected compile rejection", expr)
		}
	}

	// The errors name the real operator, so RGD authors see e.g. "_+_"
	// rather than an internal function.
	_, iss := env.Compile(`time.now() + 1`)
	if iss == nil || iss.Err() == nil {
		t.Fatal("expected compile rejection")
	}
	if !strings.Contains(iss.Err().Error(), "_+_") {
		t.Errorf("error should name the operator, got: %v", iss.Err())
	}
}

// TestDispatchIllegalDynPairsFailLoudly: illegal pairings reaching eval
// through dyn error loudly; the decorator only reroutes KREP operations.
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

// TestDispatchEqualityRejectedAtRuntime: equality shapes the checker cannot
// reject (dyn operands, kro==kro) error at eval instead of silently
// evaluating — an equality gate records no requeue.
func TestDispatchEqualityRejectedAtRuntime(t *testing.T) {
	env := dispatchEnv(t)
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	vars := map[string]any{"schema": map[string]any{"x": now.Format(time.RFC3339)}}

	for _, expr := range []string{
		`time.now() == schema.x`,
		`schema.x == time.now()`,
		`time.now() != schema.x`,
		`time.now() == time.now()`,
		`(time.now() - time.now()) == schema.x`,
	} {
		ast, iss := env.Compile(expr)
		if iss != nil && iss.Err() != nil {
			t.Errorf("%q: unexpected compile error: %v", expr, iss.Err())
			continue
		}
		prog, err := env.Program(ast, TimeOperatorDecorator())
		if err != nil {
			t.Fatalf("%q: program: %v", expr, err)
		}
		scope := map[string]any{TimeVarName: NewTimeValue(now)}
		for k, v := range vars {
			scope[k] = v
		}
		_, _, err = prog.Eval(scope)
		if err == nil {
			t.Errorf("%q: expected runtime rejection", expr)
		} else if !strings.Contains(err.Error(), "equality is not supported on time values") {
			t.Errorf("%q: wrong error: %v", expr, err)
		}
	}

	// Plain equality is untouched by the wrapper.
	for expr, want := range map[string]any{
		`schema.x == schema.x`:             true,
		`1 != 2`:                           true,
		`"a" == "b"`:                       false,
		`schema.x == "nope" ? "eq" : "ne"`: "ne",
	} {
		ast, iss := env.Compile(expr)
		if iss != nil && iss.Err() != nil {
			t.Fatalf("%q: compile: %v", expr, iss.Err())
		}
		prog, _ := env.Program(ast, TimeOperatorDecorator())
		scope := map[string]any{TimeVarName: NewTimeValue(now)}
		for k, v := range vars {
			scope[k] = v
		}
		out, _, err := prog.Eval(scope)
		if err != nil {
			t.Errorf("%q: eval: %v", expr, err)
		} else if out.Value() != want {
			t.Errorf("%q = %v, want %v", expr, out.Value(), want)
		}
	}
}
