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

// time_functions.go declares the CEL surface that makes the honest
// kro.Timestamp / kro.Duration types usable:
//
//  1. The kro.time.* operator functions (lt, le, gt, ge, add, sub) that the
//     AST rewrite (pkg/cel/ast/timerewrite.go) renames tainted operators
//     into. CEL forbids adding overloads to standard OPERATORS across types
//     (cel-go #252/#990) — but ordinary functions are freely declarable, so
//     the rewrite turns `a >= b` into `kro.time.ge(a, b)`. Because taint
//     analysis is conservative, the bindings fall back to the standard
//     library's exact semantics when neither evaluated operand is actually
//     a kro time value.
//
//  2. The explicit whitelist overloads on standard FUNCTIONS: string() is
//     the sanctioned escape hatch (renders RFC3339 / duration string,
//     records no requeue), and timestamp()/duration() are identity casts
//     that keep the value inside the solver. Everything else fails closed:
//     an honest type matches no other standard overload.
package library

import (
	"math"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/operators"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/common/types/traits"
)

// timeFunctionDeclarations returns the kro.time.* operator functions and the
// whitelist overloads. Installed by the Time() library.
func timeFunctionDeclarations() []cel.EnvOption {
	binaryOp := func(op string) cel.FunctionOpt {
		return cel.Overload("kro_time_"+opName(op)+"_dyn_dyn",
			[]*cel.Type{cel.DynType, cel.DynType},
			resultType(op),
			cel.BinaryBinding(func(lhs, rhs ref.Val) ref.Val {
				return evalTimeOp(op, lhs, rhs)
			}),
		)
	}
	identity := func(v ref.Val) ref.Val { return v }
	toString := func(v ref.Val) ref.Val { return v.ConvertToType(types.StringType) }

	return []cel.EnvOption{
		cel.Function("kro.time.lt", binaryOp(operators.Less)),
		cel.Function("kro.time.le", binaryOp(operators.LessEquals)),
		cel.Function("kro.time.gt", binaryOp(operators.Greater)),
		cel.Function("kro.time.ge", binaryOp(operators.GreaterEquals)),
		cel.Function("kro.time.add", binaryOp(operators.Add)),
		cel.Function("kro.time.sub", binaryOp(operators.Subtract)),

		// Whitelist: string() is THE escape hatch from requeue solving.
		cel.Function("string",
			cel.Overload("kro_timestamp_to_string", []*cel.Type{KroTimestampType}, cel.StringType,
				cel.UnaryBinding(toString))),
		cel.Function("string",
			cel.Overload("kro_duration_to_string", []*cel.Type{KroDurationType}, cel.StringType,
				cel.UnaryBinding(toString))),
		// Identity casts: timestamp(time.now()) stays a solver value.
		cel.Function("timestamp",
			cel.Overload("kro_timestamp_identity", []*cel.Type{KroTimestampType}, KroTimestampType,
				cel.UnaryBinding(identity))),
		cel.Function("duration",
			cel.Overload("kro_duration_identity", []*cel.Type{KroDurationType}, KroDurationType,
				cel.UnaryBinding(identity))),
	}
}

func opName(op string) string {
	switch op {
	case operators.Less:
		return "lt"
	case operators.LessEquals:
		return "le"
	case operators.Greater:
		return "gt"
	case operators.GreaterEquals:
		return "ge"
	case operators.Add:
		return "add"
	case operators.Subtract:
		return "sub"
	}
	return "unknown"
}

func resultType(op string) *cel.Type {
	if op == operators.Add || op == operators.Subtract {
		return cel.DynType
	}
	return cel.BoolType
}

// kroClockOf returns the reconcile clock if v is a Kro time value.
func kroClockOf(v ref.Val) (*Clock, bool) {
	switch t := v.(type) {
	case *KroTimestamp:
		return t.clock, true
	case *KroDuration:
		return t.clock, true
	}
	return nil, false
}

// evalTimeOp dispatches a binary operator over evaluated operands. Plain
// operands take the standard-library path; a Kro time value on either side
// takes the affine solve/arithmetic path.
func evalTimeOp(fn string, lhs, rhs ref.Val) ref.Val {
	clock, lk := kroClockOf(lhs)
	if !lk {
		var rk bool
		clock, rk = kroClockOf(rhs)
		if !rk {
			return evalStandardOp(fn, lhs, rhs)
		}
	}

	switch fn {
	case operators.Add:
		return kroAdd(clock, lhs, rhs)
	case operators.Subtract:
		return kroSubtract(clock, lhs, rhs)
	default:
		return kroCompare(clock, fn, lhs, rhs)
	}
}

// evalStandardOp replicates the CEL standard library's singleton bindings
// (trait dispatch on the left operand, NaN compares false) for operands the
// conservative taint analysis renamed but that carry no time value.
func evalStandardOp(fn string, lhs, rhs ref.Val) ref.Val {
	switch fn {
	case operators.Add:
		adder, ok := lhs.(traits.Adder)
		if !ok {
			return types.MaybeNoSuchOverloadErr(lhs)
		}
		return adder.Add(rhs)
	case operators.Subtract:
		sub, ok := lhs.(traits.Subtractor)
		if !ok {
			return types.MaybeNoSuchOverloadErr(lhs)
		}
		return sub.Subtract(rhs)
	}
	if isNaNVal(lhs) || isNaNVal(rhs) {
		return types.False
	}
	cmp, ok := lhs.(traits.Comparer)
	if !ok {
		return types.MaybeNoSuchOverloadErr(lhs)
	}
	return compareResult(fn, cmp.Compare(rhs))
}

func isNaNVal(v ref.Val) bool {
	d, ok := v.(types.Double)
	return ok && math.IsNaN(float64(d))
}

// compareResult maps a three-way Compare result onto the comparison operator.
func compareResult(fn string, cmp ref.Val) ref.Val {
	c, ok := cmp.(types.Int)
	if !ok {
		return cmp // error from Compare
	}
	switch fn {
	case operators.Less:
		return types.Bool(c < 0)
	case operators.LessEquals:
		return types.Bool(c <= 0)
	case operators.Greater:
		return types.Bool(c > 0)
	case operators.GreaterEquals:
		return types.Bool(c >= 0)
	}
	return types.NewErr("unexpected comparison operator %q", fn)
}

// kroCompare lifts both operands (both timestamps, or both durations),
// answers the comparison at the fixed now, and records the future flip.
func kroCompare(clock *Clock, fn string, lhs, rhs ref.Val) ref.Val {
	if l, ok := liftTimestamp(lhs, clock); ok {
		r, ok := liftTimestamp(rhs, clock)
		if !ok {
			return types.MaybeNoSuchOverloadErr(rhs)
		}
		return compareResult(fn, compareAndSolve(l, r))
	}
	if l, ok := liftDuration(lhs, clock); ok {
		r, ok := liftDuration(rhs, clock)
		if !ok {
			return types.MaybeNoSuchOverloadErr(rhs)
		}
		return compareResult(fn, compareAndSolve(l, r))
	}
	return types.MaybeNoSuchOverloadErr(lhs)
}

// kroAdd implements ts+dur → ts, dur+ts → ts, dur+dur → dur; ts+ts errors.
func kroAdd(clock *Clock, lhs, rhs ref.Val) ref.Val {
	lt, ltOK := liftTimestamp(lhs, clock)
	ld, ldOK := liftDuration(lhs, clock)
	rt, rtOK := liftTimestamp(rhs, clock)
	rd, rdOK := liftDuration(rhs, clock)
	switch {
	case ltOK && rdOK:
		return &KroTimestamp{affine{clock: clock, nowCount: lt.nowCount + rd.nowCount, offset: lt.offset + rd.offset}}
	case ldOK && rtOK:
		return &KroTimestamp{affine{clock: clock, nowCount: ld.nowCount + rt.nowCount, offset: ld.offset + rt.offset}}
	case ldOK && rdOK:
		return &KroDuration{affine{clock: clock, nowCount: ld.nowCount + rd.nowCount, offset: ld.offset + rd.offset}}
	case ltOK && rtOK:
		return types.NewErr("adding two timestamps is not supported")
	}
	return types.MaybeNoSuchOverloadErr(rhs)
}

// kroSubtract implements ts−ts → dur, ts−dur → ts, dur−dur → dur; dur−ts errors.
func kroSubtract(clock *Clock, lhs, rhs ref.Val) ref.Val {
	lt, ltOK := liftTimestamp(lhs, clock)
	ld, ldOK := liftDuration(lhs, clock)
	rt, rtOK := liftTimestamp(rhs, clock)
	rd, rdOK := liftDuration(rhs, clock)
	switch {
	case ltOK && rtOK:
		return &KroDuration{affine{clock: clock, nowCount: lt.nowCount - rt.nowCount, offset: lt.offset - rt.offset}}
	case ltOK && rdOK:
		return &KroTimestamp{affine{clock: clock, nowCount: lt.nowCount - rd.nowCount, offset: lt.offset - rd.offset}}
	case ldOK && rdOK:
		return &KroDuration{affine{clock: clock, nowCount: ld.nowCount - rd.nowCount, offset: ld.offset - rd.offset}}
	case ldOK && rtOK:
		return types.NewErr("subtracting a timestamp from a duration is not supported")
	}
	return types.MaybeNoSuchOverloadErr(rhs)
}
