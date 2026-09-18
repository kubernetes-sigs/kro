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

// time_dispatch.go routes kro time values appearing on the RIGHT of a
// standard operator.
//
// Standard operators dispatch through the LEFT operand's traits, so a kro
// value on the left reaches the solver directly. A kro value on the right
// reaches the plain operand's method, which errors on the foreign type.
// The decorator wraps the six time-capable binary operators: it delegates
// first, and only when that errored with a (plain LHS, kro RHS) operand
// shape does it evaluate the mirrored operation through the kro operand's
// traits. Any other failure keeps the original error, so the decorator
// never widens the language.
package library

import (
	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/common/types/traits"
	"github.com/google/cel-go/interpreter"
)

// timeReroutableOps are the binary operators the decorator wraps. Unary
// minus dispatches on its only operand and needs no wrapping.
var timeReroutableOps = map[string]bool{
	"_<_": true, "_<=_": true, "_>_": true, "_>=_": true,
	"_+_": true, "_-_": true,
}

// kroTimeValue matches solver-tracked time values via their marker method.
type kroTimeValue interface {
	ref.Val
	KroTimeSolverValue()
}

// TimeOperatorDecorator returns the ProgramOption installing the operator
// wrapper. Every program that may evaluate time expressions needs it;
// krocel.ProgramOptions installs it for all kro programs.
func TimeOperatorDecorator() cel.ProgramOption {
	return cel.CustomDecoratorV2(func(i interpreter.InterpretableV2) (interpreter.InterpretableV2, error) {
		call, ok := i.(interpreter.InterpretableCall)
		if !ok || !timeReroutableOps[call.Function()] || len(call.Args()) != 2 {
			return i, nil
		}
		return &timeOpCall{InterpretableCall: call}, nil
	})
}

// timeOpCall wraps a planned binary operator call.
type timeOpCall struct {
	interpreter.InterpretableCall
}

// Exec covers the ExecutionFrame evaluation path.
func (c *timeOpCall) Exec(frame *interpreter.ExecutionFrame) ref.Val {
	out := c.InterpretableCall.Exec(frame)
	if !types.IsError(out) {
		return out
	}
	args := c.Args()
	return c.recover(out, args[0].Exec(frame), args[1].Exec(frame))
}

// Eval covers the Activation evaluation path. Conditionals drive children
// through Eval, so both entry points must reroute (Go embedding would
// otherwise bypass an Exec-only override).
func (c *timeOpCall) Eval(a interpreter.Activation) ref.Val {
	out := c.InterpretableCall.Eval(a)
	if !types.IsError(out) {
		return out
	}
	args := c.Args()
	return c.recover(out, args[0].Eval(a), args[1].Eval(a))
}

// recover reroutes the (plain LHS, kro RHS) failure shape; anything else
// keeps the original error.
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

// rerouteMirrored evaluates `plain OP kro` through the kro operand's
// traits. Returns nil for pairings that are not KREP operations.
func rerouteMirrored(fn string, plain ref.Val, kro kroTimeValue) ref.Val {
	switch fn {
	case "_+_":
		// Addition is commutative.
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
			// a − kroDur ⇒ (−kroDur) + a; Add rejects non-time a.
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
