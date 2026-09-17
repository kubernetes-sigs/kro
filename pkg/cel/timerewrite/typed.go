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

// typed.go — POC of TypeMap-directed normalization ("typed twin" pipeline).
//
// The syntactic kind inference shared by the other walkers exists because
// the rewrite must run before checking (a kro-on-RHS expression cannot pass
// the strict, kro-on-LHS-only declarations until normalized), and an
// unchecked AST has no types. This POC dissolves the chicken-and-egg with a
// second environment:
//
//	parse ──► check against PERMISSIVE TWIN ──► TypeMap (kro bit intact!)
//	                │                                │
//	                └── twin declares BOTH operand orders, so everything
//	                    legal-after-normalization types successfully
//	                                                 │
//	      rewrite driven by twin TypeMap  ◄──────────┘
//	                │
//	                ▼
//	      check against STRICT env  ──► fail-closed verdict + program
//
// The twin's types stay HONEST (kro.Timestamp / kro.Duration) — erasing kro
// to plain timestamp would type everything but destroy the very bit the
// rewrite needs. With honest twin types, `twinAST.GetType(argID)` answers
// "is this operand kro-derived?" authoritatively: identifier scoping,
// cel.bind resolution, map-field types, ternary joins are all the CHECKER's
// job, deleting the syntactic inference and its comprehension scope
// machinery entirely. It also answers ts-vs-dur exactly (via overload result
// types), removing the subtraction normalization's kind guess.
//
// Fail-closed is preserved: the twin is only a typing oracle. An expression
// that fails the twin check (time.now() >= "oops" matches no twin
// declaration either) skips the rewrite and takes the strict check's error.
//
// The remaining gap is unchanged: operands typed `dyn` are invisible to any
// checker; kro values escaping through dyn stay LHS-only via traits.
package timerewrite

import (
	"github.com/google/cel-go/cel"
	celast "github.com/google/cel-go/common/ast"
)

// Kro time type names, matched by name to avoid importing the library
// package (whose tests import this one).
const (
	kroTimestampTypeName = "kro.Timestamp"
	kroDurationTypeName  = "kro.Duration"
)

// NormalizeAndCheckTyped is the typed-twin pipeline: twin check for the
// TypeMap, TypeMap-directed rewrite, strict check for the verdict.
// twinEnv must be env extended with the permissive twin declarations
// (library.TwinCheckDeclarations).
func NormalizeAndCheckTyped(env, twinEnv *cel.Env, expr string) (*cel.Ast, error) {
	parsed, iss := env.Parse(expr)
	if iss != nil && iss.Err() != nil {
		return nil, iss.Err()
	}

	twinChecked, iss := twinEnv.Check(parsed)
	if iss != nil && iss.Err() != nil {
		// Not typeable even permissively: no rewrite can save it. Let the
		// strict check produce the (equivalent) fail-closed error.
		checked, iss := env.Check(parsed)
		if iss != nil && iss.Err() != nil {
			return nil, iss.Err()
		}
		return checked, nil
	}

	// Checking preserves expression IDs, so the twin's TypeMap keys the
	// SAME parsed tree we now rewrite.
	twin := twinChecked.NativeRep()
	rewriteTyped(parsed.NativeRep().Expr(), twin, newFreshIDs(celast.MaxID(parsed.NativeRep())))

	checked, iss := env.Check(parsed)
	if iss != nil && iss.Err() != nil {
		return nil, iss.Err()
	}
	return checked, nil
}

// kroTypeOf classifies a node's twin-checked type.
func kroTypeOf(twin *celast.AST, e celast.Expr) kroKind {
	t := twin.GetType(e.ID())
	if t == nil {
		return kindNone
	}
	switch t.TypeName() {
	case kroTimestampTypeName:
		return kindTs
	case kroDurationTypeName:
		return kindDur
	}
	return kindNone
}

// freshIDs mints expression IDs above the parse-time maximum for spliced
// nodes (the subtraction normalization's unary minus).
type freshIDs struct{ next int64 }

func newFreshIDs(maxID int64) *freshIDs { return &freshIDs{next: maxID + 1} }

func (f *freshIDs) id() int64 {
	f.next++
	return f.next - 1
}

// rewriteTyped normalizes operators bottom-up, with every "is this operand a
// kro time value?" decision answered by the twin TypeMap. No variable
// scoping, no kind synthesis: children are recursed only to rewrite nested
// operators.
func rewriteTyped(e celast.Expr, twin *celast.AST, ids *freshIDs) {
	if e == nil {
		return
	}
	for _, ch := range childrenOf(e) {
		rewriteTyped(ch, twin, ids)
	}
	if e.Kind() != celast.CallKind {
		return
	}
	call := e.AsCall()
	if call.IsMemberFunction() || len(call.Args()) != 2 {
		return
	}
	args := call.Args()
	fn := call.FunctionName()
	l, r := kroTypeOf(twin, args[0]), kroTypeOf(twin, args[1])
	if l.isTime() || !r.isTime() {
		return // kro already on the left (trait dispatch), or no kro at all
	}

	fac := celast.NewExprFactory()
	newCall := func(function string, callArgs ...celast.Expr) celast.Expr {
		return fac.NewCall(ids.id(), function, callArgs...)
	}

	if mirror, ok := mirroredComparisons[fn]; ok {
		e.SetKindCase(newCall(mirror, args[1], args[0]))
		return
	}
	switch fn {
	case "_+_":
		e.SetKindCase(newCall("_+_", args[1], args[0]))
	case "_-_":
		// The twin's ts-vs-dur answer is exact (overload result types), so
		// no kind guessing:
		if r == kindTs {
			e.SetKindCase(newCall("-_", newCall("_-_", args[1], args[0])))
		} else {
			e.SetKindCase(newCall("_+_", newCall("-_", args[1]), args[0]))
		}
	}
}
