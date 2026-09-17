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

// time_dispatch.go — plan-time operator interception for kro time values on
// the RIGHT of standard operators.
//
// Runtime dispatch model (verified against cel-go v0.31 internals):
// standard operators are SINGLETON bindings that dispatch through the LEFT
// operand's trait interface (common/stdlib/standard.go, e.g.
// `lhs.(traits.Comparer).Compare(rhs)`), gated by evalBinary's
// `lVal.Type().HasTrait(...)`. Kro time values implement those traits, so a
// kro value on the LEFT already routes into kro's affine solver with no
// help. A kro value on the RIGHT of a plain operand, however, reaches the
// plain type's method (e.g. types.Timestamp.Compare), which type-asserts
// the RHS and returns a no-such-overload error — flip collection lost.
//
// This decorator closes exactly that residue. Following the shipped
// decRegexProgramSizeLimit precedent (interpreter/decorators.go), it wraps
// each planned binary time-capable operator call in a thin node whose Exec:
//
//  1. delegates to the underlying stdlib node (zero-cost fast path: all
//     plain-only and kro-on-LHS evaluations complete here);
//
//  2. only if that returned an error, re-evaluates the operands (CEL
//     evaluation is pure, so this is safe) and, when the failure shape is
//     (plain LHS, kro RHS), reroutes through the kro operand's traits using
//     the mirrored operation:
//
//     a <  b  ⇒ b.Compare(a) == 1        a +  b  ⇒ b.Add(a)
//     a <= b  ⇒ b.Compare(a) != -1       a − kroTs  ⇒ −(kroTs − a)
//     a >  b  ⇒ b.Compare(a) == -1       a − kroDur ⇒ (−kroDur) + a
//     a >= b  ⇒ b.Compare(a) != 1
//
//     Any other failure (including genuinely illegal pairs like
//     duration − kro.Timestamp) returns the ORIGINAL error unchanged, so
//     the decorator can only rescue KREP-legal operations, never widen the
//     language.
//
// This never re-binds a standard operator (no cel-go #990 conflict), never
// changes what type-checks (it runs strictly after a successful check), and
// costs one wrapper allocation per operator node at plan time.
package library

import (
	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/common/types/traits"
	"github.com/google/cel-go/interpreter"
)

// timeReroutableOps are the binary operators whose kro-on-RHS evaluations
// the decorator rescues. Unary minus needs no wrapping (single operand,
// trait dispatch already reaches kro values).
var timeReroutableOps = map[string]bool{
	"_<_": true, "_<=_": true, "_>_": true, "_>=_": true,
	"_+_": true, "_-_": true,
}

// kroTimeValue matches the solver-tracked time values via their marker
// method (same contract as the render guard in pkg/cel/conversion).
type kroTimeValue interface {
	ref.Val
	KroTimeSolverValue()
}

// TimeOperatorDecorator returns the ProgramOption installing the plan-time
// wrapper. It must be applied to every program that may evaluate time
// expressions; kro funnels all program construction through
// krocel.ProgramOptions, which installs it.
func TimeOperatorDecorator() cel.ProgramOption {
	return cel.CustomDecoratorV2(func(i interpreter.InterpretableV2) (interpreter.InterpretableV2, error) {
		call, ok := i.(interpreter.InterpretableCall)
		if !ok || !timeReroutableOps[call.Function()] || len(call.Args()) != 2 {
			return i, nil
		}
		return &timeOpCall{InterpretableCall: call}, nil
	})
}

// timeOpCall wraps a planned binary operator call, delegating on the fast
// path and rerouting the (plain LHS, kro RHS) failure shape.
type timeOpCall struct {
	interpreter.InterpretableCall
}

// Exec covers the ExecutionFrame evaluation path (top-level program eval).
func (c *timeOpCall) Exec(frame *interpreter.ExecutionFrame) ref.Val {
	out := c.InterpretableCall.Exec(frame)
	if !types.IsError(out) {
		return out
	}
	args := c.Args()
	return c.recover(out, args[0].Exec(frame), args[1].Exec(frame))
}

// Eval covers the Activation evaluation path. Some planned parents (e.g.
// conditional attributes) drive children through Interpretable.Eval; with Go
// embedding, an Exec-only override would be bypassed there, so both entry
// points reroute.
func (c *timeOpCall) Eval(a interpreter.Activation) ref.Val {
	out := c.InterpretableCall.Eval(a)
	if !types.IsError(out) {
		return out
	}
	args := c.Args()
	return c.recover(out, args[0].Eval(a), args[1].Eval(a))
}

// recover reroutes the (plain LHS, kro RHS) failure shape; any other
// failure keeps the original error.
func (c *timeOpCall) recover(out, l, r ref.Val) ref.Val {
	if _, lIsKro := l.(kroTimeValue); lIsKro || types.IsError(l) {
		return out // kro-on-LHS failures are genuine; keep the original error
	}
	rk, rIsKro := r.(kroTimeValue)
	if !rIsKro {
		return out // no kro operand involved; keep the original error
	}
	if res := rerouteMirrored(c.Function(), l, rk); res != nil {
		return res
	}
	return out
}

// rerouteMirrored evaluates `plain OP kro` through the kro operand's traits.
// Returns nil when the pairing is not a KREP-legal operation, in which case
// the caller surfaces the original error.
func rerouteMirrored(fn string, plain ref.Val, kro kroTimeValue) ref.Val {
	switch fn {
	case "_+_":
		// Addition is commutative across the KREP table.
		return kro.(traits.Adder).Add(plain)

	case "_-_":
		switch k := kro.(type) {
		case *KroTimestamp:
			// a − kroTs is legal only for timestamp a: −(kroTs − a).
			if _, ok := plain.(types.Timestamp); !ok {
				return nil
			}
			d := k.Subtract(plain)
			if types.IsError(d) {
				return d
			}
			return d.(traits.Negater).Negate()
		case *KroDuration:
			// a − kroDur ⇒ (−kroDur) + a; the Add lifts (and rejects
			// non-time a with a precise error).
			return k.Negate().(traits.Adder).Add(plain)
		}
		return nil

	case "_<_", "_<=_", "_>_", "_>=_":
		cmp := kro.(traits.Comparer).Compare(plain)
		if types.IsError(cmp) {
			return nil // unliftable RHS pairing (e.g. string vs kro time)
		}
		// cmp = sign(kro − plain); mirror each relation.
		switch fn {
		case "_<_": // plain < kro ⇔ kro − plain > 0
			return types.Bool(cmp == types.IntOne)
		case "_<=_":
			return types.Bool(cmp != types.IntNegOne)
		case "_>_":
			return types.Bool(cmp == types.IntNegOne)
		case "_>=_":
			return types.Bool(cmp != types.IntOne)
		}
	}
	return nil
}
