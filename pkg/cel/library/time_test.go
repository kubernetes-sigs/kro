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

import (
	"testing"
	"time"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/ext"

	kroast "github.com/kubernetes-sigs/kro/pkg/cel/timerewrite"
)

// timeEnv builds a CEL environment with the time library and a dyn `schema`
// variable for expressions that reference instance data.
func timeEnv(t *testing.T) *cel.Env {
	t.Helper()
	env, err := cel.NewEnv(
		Time(),
		ext.Bindings(),
		cel.Variable("schema", cel.DynType),
	)
	if err != nil {
		t.Fatalf("NewEnv: %v", err)
	}
	return env
}

// evalTime compiles (parse → time-operator rewrite → check → program) and
// evaluates expr with `time` fixed at now, returning the result and the
// TimeVal (for flip inspection). Mirrors krocel.ParseAndCheck, which cannot
// be imported here (pkg/cel imports this package).
func evalTime(t *testing.T, env *cel.Env, expr string, now time.Time, vars map[string]any) (any, *TimeVal) {
	t.Helper()
	parsed, iss := env.Parse(expr)
	if iss != nil && iss.Err() != nil {
		t.Fatalf("parse %q: %v", expr, iss.Err())
	}
	parsed, err := kroast.RewriteTimeOperators(parsed)
	if err != nil {
		t.Fatalf("rewrite %q: %v", expr, err)
	}
	ast, iss := env.Check(parsed)
	if iss != nil && iss.Err() != nil {
		t.Fatalf("check %q: %v", expr, iss.Err())
	}
	prog, err := env.Program(ast)
	if err != nil {
		t.Fatalf("program %q: %v", expr, err)
	}
	tv := NewTimeValue(now)
	scope := map[string]any{TimeVarName: tv}
	for k, v := range vars {
		scope[k] = v
	}
	out, _, err := prog.Eval(scope)
	if err != nil {
		t.Fatalf("eval %q: %v", expr, err)
	}
	return out.Value(), tv
}

func TestNowIsFixedPerReconcile(t *testing.T) {
	env := timeEnv(t)
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	// now() - now() must be the zero duration: every call observes the same now.
	got, tv := evalTime(t, env, `string(time.now() - time.now())`, now, nil)
	if got != "0s" {
		t.Fatalf("now() - now() = %v, want 0s", got)
	}
	if _, flip := tv.EarliestFlip(); flip {
		t.Fatalf("string() escape hatch must not record a requeue")
	}
}

func TestStringEscapeHatch(t *testing.T) {
	env := timeEnv(t)
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	got, tv := evalTime(t, env, `string(time.now())`, now, nil)
	if got != "2026-09-17T12:00:00Z" {
		t.Fatalf("string(time.now()) = %v, want 2026-09-17T12:00:00Z", got)
	}
	if _, flip := tv.EarliestFlip(); flip {
		t.Fatalf("string() escape hatch must not record a requeue")
	}
	// Arithmetic before the cast still renders the shifted instant.
	got, _ = evalTime(t, env, `string(time.now() + duration("1h"))`, now, nil)
	if got != "2026-09-17T13:00:00Z" {
		t.Fatalf("string(now+1h) = %v, want 2026-09-17T13:00:00Z", got)
	}
}

func TestComparisonSolvesFutureFlip(t *testing.T) {
	env := timeEnv(t)
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	created := now.Add(-2 * time.Minute).Format(time.RFC3339)

	// Startup grace period from the KREP: gate opens 5m after creation.
	// At now (2m after creation) the gate is closed and must requeue at +3m.
	got, tv := evalTime(t, env,
		`time.now() >= timestamp(schema.creationTimestamp) + duration("5m")`,
		now, map[string]any{"schema": map[string]any{"creationTimestamp": created}})
	if got != false {
		t.Fatalf("gate = %v, want false (2m into a 5m grace period)", got)
	}
	flip, ok := tv.EarliestFlip()
	if !ok {
		t.Fatalf("expected a recorded flip")
	}
	want := now.Add(3 * time.Minute)
	if !flip.Equal(want) {
		t.Fatalf("flip = %v, want %v", flip, want)
	}
}

func TestComparisonPastFlipNoRequeue(t *testing.T) {
	env := timeEnv(t)
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	created := now.Add(-10 * time.Minute).Format(time.RFC3339)

	// Gate already open: flip is in the past, no requeue.
	got, tv := evalTime(t, env,
		`time.now() >= timestamp(schema.creationTimestamp) + duration("5m")`,
		now, map[string]any{"schema": map[string]any{"creationTimestamp": created}})
	if got != true {
		t.Fatalf("gate = %v, want true (10m past creation)", got)
	}
	if _, ok := tv.EarliestFlip(); ok {
		t.Fatalf("no requeue expected for a flip in the past")
	}
}

func TestParallelLinesNeverFlip(t *testing.T) {
	env := timeEnv(t)
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)

	// Both sides carry nowCount 1: parallel lines, never flips.
	got, tv := evalTime(t, env, `time.now() < time.now() + duration("1h")`, now, nil)
	if got != true {
		t.Fatalf("now < now+1h = %v, want true", got)
	}
	if _, ok := tv.EarliestFlip(); ok {
		t.Fatalf("parallel comparison must not record a flip")
	}
}

func TestEarliestOfMultipleComparisons(t *testing.T) {
	env := timeEnv(t)
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	t1 := now.Add(10 * time.Minute).Format(time.RFC3339)
	t2 := now.Add(4 * time.Minute).Format(time.RFC3339)

	got, tv := evalTime(t, env,
		`time.now() >= timestamp(schema.a) && time.now() >= timestamp(schema.b)`,
		now, map[string]any{"schema": map[string]any{"a": t1, "b": t2}})
	if got != false {
		t.Fatalf("gate = %v, want false", got)
	}
	flip, ok := tv.EarliestFlip()
	if !ok {
		t.Fatalf("expected a recorded flip")
	}
	// && short-circuits: only the first comparison runs, so the earliest
	// EVALUATED flip is at +10m. (Solving only sees executed comparisons.)
	want := now.Add(10 * time.Minute)
	if !flip.Equal(want) {
		t.Fatalf("flip = %v, want %v", flip, want)
	}
}

func TestDurationComparisonSolves(t *testing.T) {
	env := timeEnv(t)
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	updated := now.Add(-2 * time.Second).Format(time.RFC3339)

	// Age gate: now - lastUpdated > 5s. At age 2s it's false; flips at +3s.
	got, tv := evalTime(t, env,
		`time.now() - timestamp(schema.lastUpdated) > duration("5s")`,
		now, map[string]any{"schema": map[string]any{"lastUpdated": updated}})
	if got != false {
		t.Fatalf("age gate = %v, want false", got)
	}
	flip, ok := tv.EarliestFlip()
	if !ok {
		t.Fatalf("expected a recorded flip")
	}
	want := now.Add(3 * time.Second)
	if !flip.Equal(want) {
		t.Fatalf("flip = %v, want %v", flip, want)
	}
}

func TestNowCountTwoSolvesUniformly(t *testing.T) {
	env := timeEnv(t)
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	target := now.Add(-1 * time.Hour).Format(time.RFC3339)

	// b = now + (now - T) has nowCount 2 (KREP example). Compared against a
	// constant C: flip = (C - (-T)) / 2. With T = now-1h and C = now+1h:
	// flip = (now+1h + now-1h)/2 = now → not in the future → no requeue,
	// and b(now) = 2·now − (now−1h) = now+1h which is NOT < now+1h → false.
	got, tv := evalTime(t, env,
		`time.now() + (time.now() - timestamp(schema.t)) < timestamp(schema.c)`,
		now, map[string]any{"schema": map[string]any{
			"t": target,
			"c": now.Add(1 * time.Hour).Format(time.RFC3339),
		}})
	if got != false {
		t.Fatalf("nowCount-2 comparison = %v, want false", got)
	}
	if _, ok := tv.EarliestFlip(); ok {
		t.Fatalf("flip at exactly now must not requeue")
	}
}

func TestAddingTwoTimestampsErrors(t *testing.T) {
	env := timeEnv(t)
	defer func() { _ = recover() }()
	got, _ := evalTimeErr(t, env, `time.now() + timestamp(schema.t)`,
		map[string]any{"schema": map[string]any{"t": "2026-01-01T00:00:00Z"}})
	if got == nil {
		t.Fatalf("expected error adding two timestamps")
	}
}

// evalTimeErr is evalTime for expressions expected to fail at check or eval.
func evalTimeErr(t *testing.T, env *cel.Env, expr string, vars map[string]any) (error, *TimeVal) {
	t.Helper()
	parsed, iss := env.Parse(expr)
	if iss != nil && iss.Err() != nil {
		return iss.Err(), nil
	}
	parsed, err := kroast.RewriteTimeOperators(parsed)
	if err != nil {
		return err, nil
	}
	ast, iss := env.Check(parsed)
	if iss != nil && iss.Err() != nil {
		return iss.Err(), nil
	}
	prog, err := env.Program(ast)
	if err != nil {
		return err, nil
	}
	tv := NewTimeValue(time.Now())
	scope := map[string]any{TimeVarName: tv}
	for k, v := range vars {
		scope[k] = v
	}
	_, _, err = prog.Eval(scope)
	return err, tv
}

func TestClockSharedAcrossValues(t *testing.T) {
	// Simulates a subgraph runtime inheriting the parent clock: flips
	// recorded through either value surface on the same clock.
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	tv := NewTimeValue(now)
	nowVal := &KroTimestamp{affine{clock: tv.clock, nowCount: 1, offset: 0}}
	future := affine{clock: tv.clock, nowCount: 0, offset: now.Add(30 * time.Second).UnixNano()}
	if c := compareAndSolve(nowVal.affine, future); c != -1 {
		t.Fatalf("compare = %d, want -1", c)
	}
	flip, ok := tv.EarliestFlip()
	if !ok || !flip.Equal(now.Add(30*time.Second)) {
		t.Fatalf("flip = %v ok=%v, want %v", flip, ok, now.Add(30*time.Second))
	}
}

// --- Order-independence via TimeOperatorDecorator ---

func TestReversedComparisonSolves(t *testing.T) {
	env := timeEnv(t)
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	openAt := now.Add(5 * time.Minute).Format(time.RFC3339)

	// Kro value on the RIGHT: plain timestamp <= now(). Closed now, flips at +5m.
	got, tv := evalTime(t, env,
		`timestamp(schema.openAt) <= time.now()`,
		now, map[string]any{"schema": map[string]any{"openAt": openAt}})
	if got != false {
		t.Fatalf("reversed gate = %v, want false", got)
	}
	flip, ok := tv.EarliestFlip()
	if !ok || !flip.Equal(now.Add(5*time.Minute)) {
		t.Fatalf("flip = %v ok=%v, want %v", flip, ok, now.Add(5*time.Minute))
	}
}

func TestReversedAddition(t *testing.T) {
	env := timeEnv(t)
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	target := now.Add(10 * time.Minute).Format(time.RFC3339)

	// Kro value on the RIGHT of +: duration + now() must stay affine and solve.
	got, tv := evalTime(t, env,
		`duration("5m") + time.now() >= timestamp(schema.t)`,
		now, map[string]any{"schema": map[string]any{"t": target}})
	if got != false {
		t.Fatalf("dur+now >= t = %v, want false", got)
	}
	flip, ok := tv.EarliestFlip()
	if !ok || !flip.Equal(now.Add(5*time.Minute)) {
		t.Fatalf("flip = %v ok=%v, want %v (t - 5m)", flip, ok, now.Add(5*time.Minute))
	}
}

func TestReversedSubtraction(t *testing.T) {
	env := timeEnv(t)
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	expiry := now.Add(10 * time.Minute).Format(time.RFC3339)

	// Kro value on the RIGHT of −: plainTs − now() is a remaining-time
	// duration; gate "less than 5m remaining" flips at expiry − 5m.
	got, tv := evalTime(t, env,
		`timestamp(schema.expiry) - time.now() < duration("5m")`,
		now, map[string]any{"schema": map[string]any{"expiry": expiry}})
	if got != false {
		t.Fatalf("remaining < 5m = %v, want false (10m remaining)", got)
	}
	flip, ok := tv.EarliestFlip()
	if !ok || !flip.Equal(now.Add(5*time.Minute)) {
		t.Fatalf("flip = %v ok=%v, want %v", flip, ok, now.Add(5*time.Minute))
	}

	// ts − dur with the Kro duration on the right: plainTs − (now()−now()) is
	// exercised via bind to prove taint through indirection is caught.
	got, tv = evalTime(t, env,
		`cel.bind(age, time.now() - timestamp(schema.expiry), timestamp(schema.expiry) + age >= timestamp(schema.expiry))`,
		now, map[string]any{"schema": map[string]any{"expiry": expiry}})
	if got != false {
		t.Fatalf("bind-carried comparison = %v, want false (age negative)", got)
	}
	if _, ok := tv.EarliestFlip(); !ok {
		t.Fatalf("bind-carried comparison should record a flip")
	}
}

func TestReversedDurationMinusKroTimestampErrors(t *testing.T) {
	env := timeEnv(t)
	err, _ := evalTimeErr(t, env, `duration("5m") - time.now()`, nil)
	if err == nil {
		t.Fatalf("expected error subtracting a timestamp from a duration")
	}
}

func TestDecoratorLeavesPlainOperatorsUntouched(t *testing.T) {
	env := timeEnv(t)
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	for expr, want := range map[string]any{
		`1 < 2`:            true,
		`2.5 >= 3.0`:       false,
		`"a" + "b"`:        "ab",
		`size([1] + [2])`:  int64(2),
		`10 - 4`:           int64(6),
		`timestamp(schema.a) < timestamp(schema.b)`: true,
	} {
		got, tv := evalTime(t, env, expr, now, map[string]any{"schema": map[string]any{
			"a": "2026-01-01T00:00:00Z", "b": "2026-06-01T00:00:00Z",
		}})
		if _, flip := tv.EarliestFlip(); flip {
			t.Errorf("%q: plain expression must not record a flip", expr)
		}
		if got != want {
			t.Errorf("%q = %v (%T), want %v", expr, got, got, want)
		}
	}
}

// --- Static type safety of the coercion table ---

// checkTime runs parse → rewrite → check and returns the check error (nil if
// the expression compiles).
func checkTime(t *testing.T, env *cel.Env, expr string) error {
	t.Helper()
	parsed, iss := env.Parse(expr)
	if iss != nil && iss.Err() != nil {
		t.Fatalf("parse %q: %v", expr, iss.Err())
	}
	parsed, err := kroast.RewriteTimeOperators(parsed)
	if err != nil {
		t.Fatalf("rewrite %q: %v", expr, err)
	}
	_, iss = env.Check(parsed)
	if iss != nil && iss.Err() != nil {
		return iss.Err()
	}
	return nil
}

func TestTypeMismatchesRejectedAtCompileTime(t *testing.T) {
	env := timeEnv(t)
	rejected := []string{
		`time.now() >= "oops"`,                       // ts vs string
		`time.now() + 1`,                             // ts + int
		`time.now() - "5m"`,                          // ts - string (must cast duration)
		`time.now() < duration("5m")`,                // ts vs dur: different kinds
		`time.now() - time.now() > 5`,                // dur vs int
		`time.now() + time.now()`,                    // ts + ts
		`duration("5m") - time.now()`,                // dur - ts
		`time.now().getSeconds() > 0`,                // calendar accessors not whitelisted
		`int(time.now())`,                            // only string() escapes the solver
	}
	for _, expr := range rejected {
		if err := checkTime(t, env, expr); err == nil {
			t.Errorf("%q: expected compile-time rejection, but it type-checked", expr)
		}
	}

	accepted := []string{
		`time.now() >= timestamp(schema.t)`,                        // kro ts vs ts
		`timestamp(schema.t) <= time.now()`,                        // reversed
		`time.now() - timestamp(schema.t) > duration("5s")`,        // kro dur vs dur
		`time.now() + duration("1h") < timestamp(schema.t)`,        // arithmetic then compare
		`string(time.now())`,                                       // the escape hatch
		`string(time.now() + duration("1h"))`,                      // arithmetic then escape
		`timestamp(schema.a) < timestamp(schema.b)`,                // plain ts comparison
		`size([time.now()]) + 1 > 0`,                               // over-taint: int mirror pairs
		`cel.bind(d, time.now() + duration("5m"), timestamp(schema.t) < d)`, // bind-carried
	}
	for _, expr := range accepted {
		if err := checkTime(t, env, expr); err != nil {
			t.Errorf("%q: expected to compile, got: %v", expr, err)
		}
	}
}

// --- UnixNano range safety ---

// TestFarRangeTimestampsCompareCorrectly pins the saturating-clamp fix:
// time.Time.UnixNano() is undefined outside ~1678..2262 (it wraps), but CEL
// timestamps span 0001..9999. A certificate notAfter of 9999-12-31 (the
// common "never expires" sentinel) must not silently flip comparisons.
func TestFarRangeTimestampsCompareCorrectly(t *testing.T) {
	env := timeEnv(t)
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	vars := map[string]any{"schema": map[string]any{
		"future": "9999-12-31T23:59:59Z",
		"past":   "1500-01-01T00:00:00Z",
	}}

	for expr, want := range map[string]any{
		`time.now() < timestamp(schema.future)`:  true,
		`time.now() >= timestamp(schema.future)`: false,
		`timestamp(schema.future) > time.now()`:  true, // reversed operand
		`time.now() > timestamp(schema.past)`:    true,
		`time.now() <= timestamp(schema.past)`:   false,
		// remaining-time shape against the sentinel: effectively infinite
		`timestamp(schema.future) - time.now() < duration("5m")`: false,
	} {
		got, tv := evalTime(t, env, expr, now, vars)
		if got != want {
			t.Errorf("%q = %v, want %v", expr, got, want)
		}
		// No flip should be recorded within any realistic horizon: the only
		// permissible flip is at the saturation boundary (~2262) or none.
		if f, ok := tv.EarliestFlip(); ok && f.Before(now.AddDate(100, 0, 0)) {
			t.Errorf("%q recorded near-term flip %v for an out-of-range operand", expr, f)
		}
	}
}
