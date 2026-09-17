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
//  1. DECLARATION-ONLY overload merges on the standard operators. cel-go
//     forbids attaching custom BINDINGS to standard operators (#990), but
//     overload DECLARATIONS merge cleanly: the checker learns the kro
//     pairings, and at runtime the standard library's singleton binding
//     dispatches through the LEFT operand's trait interfaces — which the
//     kro types implement with requeue solving (time.go). Only kro-on-LHS
//     pairs are declared; the AST rewrite (pkg/cel/timerewrite) normalizes
//     a kro value on the right of a plain operand onto the left, so a
//     pairing the rewrite cannot normalize fails closed at check time.
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
// (KREP-025 definitions table), kro on the LEFT (runtime dispatch is
// through the left operand's traits; the rewrite normalizes kro-on-right).
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
	}

	// addition: ts+dur → ts, dur+ts → ts, dur+dur → dur.
	kroAddPairs = []operandPair{
		{"krots_dur", KroTimestampType, cel.DurationType, KroTimestampType},
		{"krots_krodur", KroTimestampType, KroDurationType, KroTimestampType},
		{"krodur_ts", KroDurationType, cel.TimestampType, KroTimestampType},
		{"krodur_krots", KroDurationType, KroTimestampType, KroTimestampType},
		{"krodur_dur", KroDurationType, cel.DurationType, KroDurationType},
		{"krodur_krodur", KroDurationType, KroDurationType, KroDurationType},
	}

	// subtraction: ts−ts → dur, ts−dur → ts, dur−dur → dur.
	kroSubPairs = []operandPair{
		{"krots_krots", KroTimestampType, KroTimestampType, KroDurationType},
		{"krots_ts", KroTimestampType, cel.TimestampType, KroDurationType},
		{"krots_dur", KroTimestampType, cel.DurationType, KroTimestampType},
		{"krots_krodur", KroTimestampType, KroDurationType, KroTimestampType},
		{"krodur_dur", KroDurationType, cel.DurationType, KroDurationType},
		{"krodur_krodur", KroDurationType, KroDurationType, KroDurationType},
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

// TwinCheckDeclarations returns the extra declaration-only overloads for the
// PERMISSIVE TWIN environment used by TypeMap-directed normalization
// (pkg/cel/timerewrite/typed.go): the operand orders the normalization
// exists to fix — a kro time value on the RIGHT of a plain operand. These
// are NOT installed in the real environment (fail-closed there); the twin
// is a typing oracle only. Result types mirror the KREP definitions table.
func TwinCheckDeclarations() []cel.EnvOption {
	pair := func(op, id string, l, r, res *cel.Type) cel.EnvOption {
		return cel.Function(op, cel.Overload("kro_twin_"+id, []*cel.Type{l, r}, res))
	}
	kts, kdur := KroTimestampType, KroDurationType
	ts, dur := cel.TimestampType, cel.DurationType
	var opts []cel.EnvOption
	for op, name := range map[string]string{
		"_<_": "lt", "_<=_": "le", "_>_": "gt", "_>=_": "ge",
	} {
		opts = append(opts,
			pair(op, name+"_ts_krots", ts, kts, cel.BoolType),
			pair(op, name+"_dur_krodur", dur, kdur, cel.BoolType),
		)
	}
	return append(opts,
		pair("_+_", "add_dur_krots", dur, kts, kts),
		pair("_+_", "add_ts_krodur", ts, kdur, kts),
		pair("_+_", "add_dur_krodur", dur, kdur, kdur),
		pair("_-_", "sub_ts_krots", ts, kts, kdur),
		pair("_-_", "sub_ts_krodur", ts, kdur, kts),
		pair("_-_", "sub_dur_krodur", dur, kdur, kdur),
	)
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
		// Unary minus on kro.Duration (Negater trait), used by the rewrite's
		// plain−kro normalization: a − b ⇒ −(b − a) / (−b) + a.
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
