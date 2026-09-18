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

// time_functions.go declares the CEL surface for kro.Timestamp and
// kro.Duration:
//
//  1. Declaration-only overloads on the standard operators, in both operand
//     orders. Undeclared pairings fail at check time with the real operator
//     name. At runtime, kro-on-left dispatches through the operand's traits
//     (time.go); kro-on-right is routed by the decorator
//     (time_dispatch.go).
//
//  2. A whitelist on standard functions: string() renders the value and
//     exits the solver; timestamp() and duration() are identity casts.
//     Everything else matches no overload and fails closed.
package library

import (
	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/operators"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
)

// operandPair is one legal (lhs, rhs) → result signature declared on a
// standard operator.
type operandPair struct {
	id   string
	l, r *cel.Type
	res  *cel.Type
}

// The coercion table: exactly the operations the affine solver defines,
// declared in both operand orders. Ill-typed pairings (`time.now() + 1`,
// `>= "oops"`) match no declaration and are rejected at check time.
var (
	// comparisons: like kinds only.
	kroComparablePairs = []operandPair{
		{"krots_krots", KroTimestampType, KroTimestampType, cel.BoolType},
		{"krots_ts", KroTimestampType, cel.TimestampType, cel.BoolType},
		{"krodur_krodur", KroDurationType, KroDurationType, cel.BoolType},
		{"krodur_dur", KroDurationType, cel.DurationType, cel.BoolType},
		// reversed orders
		{"ts_krots", cel.TimestampType, KroTimestampType, cel.BoolType},
		{"dur_krodur", cel.DurationType, KroDurationType, cel.BoolType},
	}

	// addition: ts+dur → ts, dur+ts → ts, dur+dur → dur.
	kroAddPairs = []operandPair{
		{"krots_dur", KroTimestampType, cel.DurationType, KroTimestampType},
		{"krots_krodur", KroTimestampType, KroDurationType, KroTimestampType},
		{"krodur_ts", KroDurationType, cel.TimestampType, KroTimestampType},
		{"krodur_krots", KroDurationType, KroTimestampType, KroTimestampType},
		{"krodur_dur", KroDurationType, cel.DurationType, KroDurationType},
		{"krodur_krodur", KroDurationType, KroDurationType, KroDurationType},
		// reversed orders
		{"ts_krodur", cel.TimestampType, KroDurationType, KroTimestampType},
		{"dur_krots", cel.DurationType, KroTimestampType, KroTimestampType},
		{"dur_krodur", cel.DurationType, KroDurationType, KroDurationType},
	}

	// subtraction: ts−ts → dur, ts−dur → ts, dur−dur → dur.
	kroSubPairs = []operandPair{
		{"krots_krots", KroTimestampType, KroTimestampType, KroDurationType},
		{"krots_ts", KroTimestampType, cel.TimestampType, KroDurationType},
		{"krots_dur", KroTimestampType, cel.DurationType, KroTimestampType},
		{"krots_krodur", KroTimestampType, KroDurationType, KroTimestampType},
		{"krodur_dur", KroDurationType, cel.DurationType, KroDurationType},
		{"krodur_krodur", KroDurationType, KroDurationType, KroDurationType},
		// reversed orders; dur−kroTs stays undeclared (not a KREP operation)
		{"ts_krots", cel.TimestampType, KroTimestampType, KroDurationType},
		{"ts_krodur", cel.TimestampType, KroDurationType, KroTimestampType},
		{"dur_krodur", cel.DurationType, KroDurationType, KroDurationType},
	}
)

// operatorDeclarations merges the kro pairs onto op as declaration-only
// overloads; runtime dispatch is traits + the decorator.
func operatorDeclarations(op, prefix string, pairs []operandPair) cel.EnvOption {
	opts := make([]cel.FunctionOpt, 0, len(pairs))
	for _, p := range pairs {
		opts = append(opts, cel.Overload(
			"kro_time_"+prefix+"_"+p.id,
			[]*cel.Type{p.l, p.r}, p.res,
		))
	}
	return cel.Function(op, opts...)
}

// timeFunctionDeclarations returns the operator declaration merges and the
// whitelist overloads. Installed by the Time() library.
func timeFunctionDeclarations() []cel.EnvOption {
	identity := func(v ref.Val) ref.Val { return v }
	toString := func(v ref.Val) ref.Val { return v.ConvertToType(types.StringType) }

	return []cel.EnvOption{
		operatorDeclarations(operators.Less, "lt", kroComparablePairs),
		operatorDeclarations(operators.LessEquals, "le", kroComparablePairs),
		operatorDeclarations(operators.Greater, "gt", kroComparablePairs),
		operatorDeclarations(operators.GreaterEquals, "ge", kroComparablePairs),
		operatorDeclarations(operators.Add, "add", kroAddPairs),
		operatorDeclarations(operators.Subtract, "sub", kroSubPairs),
		// Unary minus on kro.Duration (Negater trait); also used by the
		// decorator's subtraction reroute.
		cel.Function(operators.Negate,
			cel.Overload("kro_time_neg_krodur", []*cel.Type{KroDurationType}, KroDurationType)),

		// string() is the escape hatch from requeue solving.
		cel.Function("string",
			cel.Overload("kro_timestamp_to_string", []*cel.Type{KroTimestampType}, cel.StringType,
				cel.UnaryBinding(toString))),
		cel.Function("string",
			cel.Overload("kro_duration_to_string", []*cel.Type{KroDurationType}, cel.StringType,
				cel.UnaryBinding(toString))),
		// Identity casts keep values inside the solver.
		cel.Function("timestamp",
			cel.Overload("kro_timestamp_identity", []*cel.Type{KroTimestampType}, KroTimestampType,
				cel.UnaryBinding(identity))),
		cel.Function("duration",
			cel.Overload("kro_duration_identity", []*cel.Type{KroDurationType}, KroDurationType,
				cel.UnaryBinding(identity))),
	}
}
