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

// time.go implements the minimal KREP-025 time library: `time.now()`.
//
// now() returns a KroTimestamp — an affine value nowCount·now + offset that
// tracks how many times now() participates in it. Arithmetic (+, -) between
// Kro time values (and plain CEL timestamps/durations) stays affine, and the
// four comparison operators (<, <=, >, >=) both answer the comparison at the
// reconcile's fixed `now` AND solve for the future instant at which the
// comparison flips, recording it on the per-reconcile Clock so the controller
// can requeue exactly then.
//
// string(time.now()) is the explicit escape hatch (for lastTransitionTime and
// similar): it renders the value as RFC3339 and records no requeue.
//
// KroTimestamp and KroDuration are HONEST, intentionally-limited CEL types
// (kro.Timestamp / kro.Duration) per the KREP's "separate types" design.
// They match no standard overload, so any use that has not been explicitly
// whitelisted fails closed at type-check or dispatch time. Operators reach
// them through the kro.time.* functions installed by the AST rewrite
// (pkg/cel/ast/timerewrite.go); string() and identity timestamp()/duration()
// casts are whitelisted in time_functions.go.
package library

import (
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/common/types/traits"
)

// TimeVarName is the CEL variable through which the time library is reached
// (`time.now()`). It is seeded per reconcile with a *TimeVal.
const TimeVarName = "time"

// TimeType is the opaque type of the `time` scope variable.
var TimeType = types.NewOpaqueType("kro.Time")

var (
	kroTimeTraits = traits.AdderType | traits.SubtractorType | traits.ComparerType

	// KroTimestampType and KroDurationType are the honest static AND runtime
	// types of time.now()-derived values (KREP-025 "separate types"). They
	// are intentionally NOT the native CEL timestamp/duration types: every
	// standard function or conversion that has not been explicitly
	// whitelisted fails closed with a no-such-overload error, keeping the
	// values inside the requeue solver. Operators reach these values through
	// the kro.time.* functions installed by the AST rewrite
	// (pkg/cel/ast/timerewrite.go).
	KroTimestampType = types.NewObjectType("kro.Timestamp", kroTimeTraits)
	KroDurationType  = types.NewObjectType("kro.Duration", kroTimeTraits)
)

// Clock is the per-reconcile time state: the fixed `now` every call to
// time.now() observes, plus the earliest future instant at which any
// evaluated comparison flips its result. Subgraph runtimes share their
// parent's Clock (via seeded scope), so flips recorded anywhere in the
// graph funnel into one requeue decision.
type Clock struct {
	mu       sync.Mutex
	now      time.Time
	earliest int64 // epoch nanos of the earliest future flip; 0 = none
}

// NewClock returns a Clock fixed at now.
func NewClock(now time.Time) *Clock {
	return &Clock{now: now}
}

// Now returns the fixed reconcile time.
func (c *Clock) Now() time.Time { return c.now }

// recordFlip records a future flip instant (epoch nanos), keeping the earliest.
func (c *Clock) recordFlip(nanos int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if nanos <= c.now.UnixNano() {
		return
	}
	if c.earliest == 0 || nanos < c.earliest {
		c.earliest = nanos
	}
}

// EarliestFlip returns the earliest future flip instant recorded by any
// comparison this reconcile, if one exists.
func (c *Clock) EarliestFlip() (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.earliest == 0 {
		return time.Time{}, false
	}
	return time.Unix(0, c.earliest), true
}

// TimeVal is the CEL value bound to the `time` variable. It carries the
// reconcile's Clock; time.now() reads it.
type TimeVal struct {
	clock *Clock
}

// NewTimeValue returns the scope value for the `time` variable, with now()
// fixed at the supplied instant.
func NewTimeValue(now time.Time) *TimeVal {
	return &TimeVal{clock: NewClock(now)}
}

// EarliestFlip exposes the clock's earliest recorded future flip.
func (v *TimeVal) EarliestFlip() (time.Time, bool) { return v.clock.EarliestFlip() }

// Now exposes the fixed reconcile time.
func (v *TimeVal) Now() time.Time { return v.clock.Now() }

func (v *TimeVal) ConvertToNative(typeDesc reflect.Type) (any, error) {
	return nil, fmt.Errorf("kro time library handle cannot be converted to a native value")
}

func (v *TimeVal) ConvertToType(typeVal ref.Type) ref.Val {
	if typeVal == types.TypeType {
		return TimeType
	}
	return types.NewErr("unsupported conversion from kro.Time to %v", typeVal)
}

func (v *TimeVal) Equal(other ref.Val) ref.Val {
	o, ok := other.(*TimeVal)
	return types.Bool(ok && o.clock == v.clock)
}

func (v *TimeVal) Type() ref.Type { return TimeType }
func (v *TimeVal) Value() any     { return v.clock }

// affine is the shared representation: value(now) = nowCount·now + offset.
// offset is epoch-anchored nanoseconds for timestamps and plain nanoseconds
// for durations. A plain CEL timestamp T lifts to {0, T} and a plain CEL
// duration d lifts to {0, d}.
type affine struct {
	clock    *Clock
	nowCount int64
	offset   int64
}

// valueAt evaluates the affine function at the clock's fixed now.
func (a affine) valueAt() int64 {
	return a.nowCount*a.clock.now.UnixNano() + a.offset
}

// compareAndSolve compares two affine values at the fixed now and records the
// future instant at which the comparison flips (if any) on the LHS clock:
//
//	flipTime = (r.offset − l.offset) / (l.nowCount − r.nowCount)
//
// Equal nowCounts are parallel lines that never flip; a flip in the past
// needs no requeue.
func compareAndSolve(l, r affine) types.Int {
	nowN := l.clock.now.UnixNano()
	lv := l.nowCount*nowN + l.offset
	rv := r.nowCount*nowN + r.offset

	if denom := l.nowCount - r.nowCount; denom != 0 {
		flip := (r.offset - l.offset) / denom
		if flip > nowN {
			l.clock.recordFlip(flip)
		}
	}

	switch {
	case lv < rv:
		return types.IntNegOne
	case lv > rv:
		return types.IntOne
	default:
		return types.IntZero
	}
}

// liftTimestamp lifts a value to a timestamp affine: a *KroTimestamp as-is,
// a plain CEL timestamp to {0, T}. clock supplies the Clock for lifted plain
// values.
func liftTimestamp(v ref.Val, clock *Clock) (affine, bool) {
	switch t := v.(type) {
	case *KroTimestamp:
		return t.affine, true
	case types.Timestamp:
		return affine{clock: clock, nowCount: 0, offset: t.Time.UnixNano()}, true
	}
	if v.Type() == types.TimestampType {
		if tt, ok := v.Value().(time.Time); ok {
			return affine{clock: clock, nowCount: 0, offset: tt.UnixNano()}, true
		}
	}
	return affine{}, false
}

// liftDuration lifts a value to a duration affine: a *KroDuration as-is, a
// plain CEL duration to {0, d}.
func liftDuration(v ref.Val, clock *Clock) (affine, bool) {
	switch d := v.(type) {
	case *KroDuration:
		return d.affine, true
	case types.Duration:
		return affine{clock: clock, nowCount: 0, offset: int64(d.Duration)}, true
	}
	if v.Type() == types.DurationType {
		if dd, ok := v.Value().(time.Duration); ok {
			return affine{clock: clock, nowCount: 0, offset: int64(dd)}, true
		}
	}
	return affine{}, false
}

// KroTimestamp is the now()-tracking timestamp: value(now) = nowCount·now + offset.
//
// It reports the native CEL timestamp runtime type; operator dispatch reaches
// the methods below through the trait interfaces, keeping solving active.
type KroTimestamp struct {
	affine
}

var (
	_ ref.Val           = (*KroTimestamp)(nil)
	_ traits.Adder      = (*KroTimestamp)(nil)
	_ traits.Subtractor = (*KroTimestamp)(nil)
	_ traits.Comparer   = (*KroTimestamp)(nil)
	_ traits.Receiver   = (*KroTimestamp)(nil)
)

// Add implements ts + dur → ts. Adding two timestamps errors (as in CEL).
func (t *KroTimestamp) Add(other ref.Val) ref.Val {
	if d, ok := liftDuration(other, t.clock); ok {
		return &KroTimestamp{affine{clock: t.clock, nowCount: t.nowCount + d.nowCount, offset: t.offset + d.offset}}
	}
	if _, ok := liftTimestamp(other, t.clock); ok {
		return types.NewErr("adding two timestamps is not supported")
	}
	return types.MaybeNoSuchOverloadErr(other)
}

// Subtract implements ts − ts → dur and ts − dur → ts.
func (t *KroTimestamp) Subtract(other ref.Val) ref.Val {
	if o, ok := liftTimestamp(other, t.clock); ok {
		return &KroDuration{affine{clock: t.clock, nowCount: t.nowCount - o.nowCount, offset: t.offset - o.offset}}
	}
	if d, ok := liftDuration(other, t.clock); ok {
		return &KroTimestamp{affine{clock: t.clock, nowCount: t.nowCount - d.nowCount, offset: t.offset - d.offset}}
	}
	return types.MaybeNoSuchOverloadErr(other)
}

// Compare answers the comparison at the fixed now and records the future
// flip instant (if any) for requeue solving. Serves <, <=, >, >=.
func (t *KroTimestamp) Compare(other ref.Val) ref.Val {
	o, ok := liftTimestamp(other, t.clock)
	if !ok {
		return types.MaybeNoSuchOverloadErr(other)
	}
	return compareAndSolve(t.affine, o)
}

// Receive rejects timestamp accessor methods (getSeconds, getDayOfWeek, …):
// they would leave the affine model and silently break requeue solving.
func (t *KroTimestamp) Receive(function string, overload string, args []ref.Val) ref.Val {
	return types.NewErr(
		"%s() is not supported on a time.now()-derived timestamp; use string(...) to explicitly opt out of requeue solving",
		function)
}

// instant is the concrete time this value denotes at the fixed now.
func (t *KroTimestamp) instant() time.Time {
	return time.Unix(0, t.valueAt()).UTC()
}

// ConvertToNative is rejected entirely: string(...) is the ONLY exit from
// the solver (KREP-025). Allowing native conversion would let a bare
// ${time.now()} render into an object as an implicit escape hatch with no
// requeue recorded.
func (t *KroTimestamp) ConvertToNative(typeDesc reflect.Type) (any, error) {
	return nil, fmt.Errorf(
		"a time.now()-derived value cannot be converted to %v; wrap it in string(...) to explicitly opt out of requeue solving", typeDesc)
}

// ConvertToType supports string(ts) as the explicit, no-requeue escape hatch.
func (t *KroTimestamp) ConvertToType(typeVal ref.Type) ref.Val {
	switch typeVal {
	case types.StringType:
		return types.String(t.instant().Format(time.RFC3339))
	case types.TimestampType:
		return t
	case types.TypeType:
		return KroTimestampType
	}
	return types.NewErr("unsupported conversion from kro timestamp to %v", typeVal)
}

func (t *KroTimestamp) Equal(other ref.Val) ref.Val {
	o, ok := liftTimestamp(other, t.clock)
	if !ok {
		return types.False
	}
	return types.Bool(t.nowCount == o.nowCount && t.offset == o.offset)
}

// KroTimeSolverValue marks this type as a solver-tracked time value for the
// render guard in pkg/cel/conversion (matched by anonymous interface to
// avoid an import cycle).
func (t *KroTimestamp) KroTimeSolverValue() {}

// Type reports the honest kro.Timestamp type. Static and runtime types
// agree, so the checker-selected whitelist overloads (string, timestamp)
// pass their runtime type guards, and everything unwhitelisted fails closed.
func (t *KroTimestamp) Type() ref.Type { return KroTimestampType }

// Value returns the concrete time at the fixed now, which is what object
// rendering serializes if the value is written without string(...).
func (t *KroTimestamp) Value() any { return t.instant() }

// KroDuration is the now()-tracking duration: value(now) = nowCount·now + offset.
type KroDuration struct {
	affine
}

var (
	_ ref.Val           = (*KroDuration)(nil)
	_ traits.Adder      = (*KroDuration)(nil)
	_ traits.Subtractor = (*KroDuration)(nil)
	_ traits.Comparer   = (*KroDuration)(nil)
	_ traits.Negater    = (*KroDuration)(nil)
	_ traits.Receiver   = (*KroDuration)(nil)
)

// Add implements dur + dur → dur and dur + ts → ts.
func (d *KroDuration) Add(other ref.Val) ref.Val {
	if o, ok := liftDuration(other, d.clock); ok {
		return &KroDuration{affine{clock: d.clock, nowCount: d.nowCount + o.nowCount, offset: d.offset + o.offset}}
	}
	if o, ok := liftTimestamp(other, d.clock); ok {
		return &KroTimestamp{affine{clock: d.clock, nowCount: d.nowCount + o.nowCount, offset: d.offset + o.offset}}
	}
	return types.MaybeNoSuchOverloadErr(other)
}

// Subtract implements dur − dur → dur. dur − ts errors (as in CEL).
func (d *KroDuration) Subtract(other ref.Val) ref.Val {
	if o, ok := liftDuration(other, d.clock); ok {
		return &KroDuration{affine{clock: d.clock, nowCount: d.nowCount - o.nowCount, offset: d.offset - o.offset}}
	}
	if _, ok := liftTimestamp(other, d.clock); ok {
		return types.NewErr("subtracting a timestamp from a duration is not supported")
	}
	return types.MaybeNoSuchOverloadErr(other)
}

// Compare answers the comparison at the fixed now and records the future
// flip instant (if any) for requeue solving. Serves <, <=, >, >=.
func (d *KroDuration) Compare(other ref.Val) ref.Val {
	o, ok := liftDuration(other, d.clock)
	if !ok {
		return types.MaybeNoSuchOverloadErr(other)
	}
	return compareAndSolve(d.affine, o)
}

// Negate implements the unary minus, flipping both affine coefficients.
func (d *KroDuration) Negate() ref.Val {
	return &KroDuration{affine{clock: d.clock, nowCount: -d.nowCount, offset: -d.offset}}
}

// Receive rejects duration accessor methods (getHours, getSeconds, …):
// they would leave the affine model and silently break requeue solving.
func (d *KroDuration) Receive(function string, overload string, args []ref.Val) ref.Val {
	return types.NewErr(
		"%s() is not supported on a time.now()-derived duration; use string(...) to explicitly opt out of requeue solving",
		function)
}

// current is the concrete duration this value denotes at the fixed now.
func (d *KroDuration) current() time.Duration {
	return time.Duration(d.valueAt())
}

// ConvertToNative is rejected entirely: string(...) is the ONLY exit from
// the solver (KREP-025); see KroTimestamp.ConvertToNative.
func (d *KroDuration) ConvertToNative(typeDesc reflect.Type) (any, error) {
	return nil, fmt.Errorf(
		"a time.now()-derived value cannot be converted to %v; wrap it in string(...) to explicitly opt out of requeue solving", typeDesc)
}

// ConvertToType supports string(dur) as the explicit, no-requeue escape hatch.
func (d *KroDuration) ConvertToType(typeVal ref.Type) ref.Val {
	switch typeVal {
	case types.StringType:
		return types.String(d.current().String())
	case types.DurationType:
		return d
	case types.TypeType:
		return KroDurationType
	}
	return types.NewErr("unsupported conversion from kro duration to %v", typeVal)
}

func (d *KroDuration) Equal(other ref.Val) ref.Val {
	o, ok := liftDuration(other, d.clock)
	if !ok {
		return types.False
	}
	return types.Bool(d.nowCount == o.nowCount && d.offset == o.offset)
}

// KroTimeSolverValue marks this type as a solver-tracked time value for the
// render guard in pkg/cel/conversion; see KroTimestamp.KroTimeSolverValue.
func (d *KroDuration) KroTimeSolverValue() {}

// Type reports the honest kro.Duration type; see KroTimestamp.Type.
func (d *KroDuration) Type() ref.Type { return KroDurationType }

// Value returns the concrete duration at the fixed now.
func (d *KroDuration) Value() any { return d.current() }

// Time returns the cel.EnvOption registering the KREP-025 time library:
// the `time` variable and its member function now().
//
// now() is statically declared to return a CEL timestamp so expressions
// type-check against the standard timestamp/duration operators; at runtime it
// returns a *KroTimestamp whose operators track now() usage and solve for
// requeue instants.
func Time() cel.EnvOption {
	return cel.Lib(&timeLib{})
}

type timeLib struct{}

func (l *timeLib) LibraryName() string {
	return "kro.time"
}

func (l *timeLib) CompileOptions() []cel.EnvOption {
	opts := []cel.EnvOption{
		cel.Variable(TimeVarName, TimeType),
		cel.Function("now",
			cel.MemberOverload("kro_time_now",
				[]*cel.Type{TimeType},
				KroTimestampType,
				cel.UnaryBinding(func(arg ref.Val) ref.Val {
					tv, ok := arg.(*TimeVal)
					if !ok {
						return types.NewErr("time.now(): the time variable is not bound to a reconcile clock")
					}
					// now() ≡ {nowCount: 1, offset: 0}.
					return &KroTimestamp{affine{clock: tv.clock, nowCount: 1, offset: 0}}
				}),
			),
		),
	}
	return append(opts, timeFunctionDeclarations()...)
}

func (l *timeLib) ProgramOptions() []cel.ProgramOption {
	return nil
}
