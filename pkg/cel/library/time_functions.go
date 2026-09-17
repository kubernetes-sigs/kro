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

// time_functions.go declares the CEL surface for the honest kro.Timestamp /
// kro.Duration types:
//
//  1. DECLARATION-ONLY overload merges on the standard operators, in BOTH
//     operand orders. cel-go forbids attaching custom BINDINGS to standard
//     operators (#990), but overload DECLARATIONS merge cleanly: the
//     checker learns every KREP-legal kro pairing, and anything undeclared
//     (`time.now() + 1`, `>= "oops"`, dur − kroTs) fails closed at check
//     time with the real operator name. At runtime, kro-on-LEFT rides the
//     standard singleton's left-operand trait dispatch into the affine
//     solver (time.go); kro-on-RIGHT is rerouted by the plan-time
//     decorator (time_dispatch.go). No AST rewriting exists.
//
//  2. The explicit whitelist on standard FUNCTIONS: string() is the
//     sanctioned escape hatch (renders RFC3339 / duration string, records
//     no requeue), and timestamp()/duration() are identity casts that keep
//     the value inside the solver. Everything else fails closed: an honest
//     type matches no other standard overload.
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

// The coercion table: exactly the operations the affine solver defines
// (KREP-025 definitions table), declared in BOTH operand orders. Runtime
// dispatch: kro-on-LEFT rides the standard singleton's left-operand trait
// dispatch; kro-on-RIGHT is rerouted by the plan-time decorator
// (time_dispatch.go). No AST rewriting anywhere.
// An ill-typed pairing (`time.now() >= "oops"`, `time.now() + 1`) matches
// no declaration and is rejected by the checker like any other unknown
// type combination — with the REAL operator name in the error.
var (
	// comparisons: like kinds only.
	kroComparablePairs = []operandPair{
		{"krots_krots", KroTimestampType, KroTimestampType, cel.BoolType},
		{"krots_ts", KroTimestampType, cel.TimestampType, cel.BoolType},
		{"krodur_krodur", KroDurationType, KroDurationType, cel.BoolType},
		{"krodur_dur", KroDurationType, cel.DurationType, cel.BoolType},
		// reversed orders (runtime via decorator reroute)
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
		// reversed orders (runtime via decorator reroute)
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
		// reversed orders (runtime via decorator reroute); note dur−kroTs
		// stays UNDECLARED — it is not a KREP operation.
		{"ts_krots", cel.TimestampType, KroTimestampType, KroDurationType},
		{"ts_krodur", cel.TimestampType, KroDurationType, KroTimestampType},
		{"dur_krodur", cel.DurationType, KroDurationType, KroDurationType},
	}
)

// operatorDeclarations merges the kro pairs onto op as declaration-only
// overloads (no bindings — the standard singleton + traits carry runtime).
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
		// Unary minus on kro.Duration (Negater trait), used by the
		// decorator's plain−kro reroute: a − b ⇒ −(b − a) / (−b) + a.
		cel.Function(operators.Negate,
			cel.Overload("kro_time_neg_krodur", []*cel.Type{KroDurationType}, KroDurationType)),

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
