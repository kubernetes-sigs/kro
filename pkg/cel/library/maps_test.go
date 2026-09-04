// Copyright 2025 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package library

import (
	"fmt"
	"testing"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types/ref"
	"github.com/stretchr/testify/require"
)

func TestMaps(t *testing.T) {
	mapsTests := []struct {
		expr string
	}{
		{expr: `{}.merge({}) == {}`},
		{expr: `{}.merge({'a': 1}) == {'a': 1}`},
		{expr: `{}.merge({'a': 2.1}) == {'a': 2.1}`},
		{expr: `{}.merge({'a': 'foo'}) == {'a': 'foo'}`},
		{expr: `{'a': 1}.merge({}) == {'a': 1}`},
		{expr: `{'a': 1}.merge({'b': 2}) == {'a': 1, 'b': 2}`},
		{expr: `{'a': 1}.merge({'a': 2, 'b': 2}) == {'a': 2, 'b': 2}`},
	}

	env := testMapsEnv(t)
	for i, tc := range mapsTests {
		t.Run(fmt.Sprintf("%d", i), func(t *testing.T) {
			evalTrue(t, env, tc.expr)
		})
	}
}

func TestDeepMerge(t *testing.T) {
	// expr is the call under test; want is the expected CEL value.
	// Comparison uses CEL equality so nested map/list shapes stay consistent.
	// notEqual=true asserts expr != want (negative expectation).
	deepMergeTests := []struct {
		name     string
		expr     string
		want     string
		notEqual bool
	}{
		// empty maps (and empty-side fast paths)
		{name: "both empty", expr: `{}.deepMerge({})`, want: `{}`},
		{name: "empty rhs returns lhs", expr: `{'a': 1}.deepMerge({})`, want: `{'a': 1}`},
		{name: "empty lhs returns rhs", expr: `{}.deepMerge({'a': 1})`, want: `{'a': 1}`},

		// top-level shape
		{name: "disjoint keys union", expr: `{'a': 1}.deepMerge({'b': 2})`, want: `{'a': 1, 'b': 2}`},
		{name: "shallow leaf: rhs wins", expr: `{'a': 1}.deepMerge({'a': 2})`, want: `{'a': 2}`},
		{name: "shallow leaf: lhs does not win", expr: `{'a': 1}.deepMerge({'a': 2})`, want: `{'a': 1}`, notEqual: true},
		{name: "lhs-only key preserved", expr: `{'a': {'x': 1}, 'keep': 7}.deepMerge({'a': {'y': 2}})`, want: `{'a': {'x': 1, 'y': 2}, 'keep': 7}`},
		{name: "rhs-only key added", expr: `{'a': 1}.deepMerge({'a': 1, 'b': 2})`, want: `{'a': 1, 'b': 2}`},

		// nested maps — the core deepMerge behavior vs shallow merge
		{
			name: "nested merge keeps lhs-only and rhs-only leaves; rhs wins shared leaf",
			expr: `{'a': {'x': 1, 'y': 2}}.deepMerge({'a': {'y': 3, 'z': 4}})`,
			want: `{'a': {'x': 1, 'y': 3, 'z': 4}}`,
		},
		{
			name: "nested merge is not shallow replace",
			// shallow merge would yield this and drop x
			expr:     `{'a': {'x': 1, 'y': 2}}.deepMerge({'a': {'y': 3, 'z': 4}})`,
			want:     `{'a': {'y': 3, 'z': 4}}`,
			notEqual: true,
		},
		{
			name: "shallow merge still replaces nested maps wholesale",
			expr: `{'a': {'x': 1, 'y': 2}}.merge({'a': {'y': 3, 'z': 4}})`,
			want: `{'a': {'y': 3, 'z': 4}}`,
		},
		{
			name: "3-level nesting rhs wins deepest shared leaf",
			expr: `{'a': {'b': {'c': 1, 'd': 2}}}.deepMerge({'a': {'b': {'d': 9, 'e': 5}}})`,
			want: `{'a': {'b': {'c': 1, 'd': 9, 'e': 5}}}`,
		},

		// type-crossing at a key: rhs always wins (no attempt to coerce)
		{name: "scalar rhs replaces map lhs", expr: `{'a': {'x': 1}}.deepMerge({'a': 5})`, want: `{'a': 5}`},
		{name: "map rhs replaces scalar lhs", expr: `{'a': 5}.deepMerge({'a': {'x': 1}})`, want: `{'a': {'x': 1}}`},
		{name: "list rhs replaces list lhs wholesale", expr: `{'a': [1, 2]}.deepMerge({'a': [3]})`, want: `{'a': [3]}`},
		{name: "list rhs does not concatenate", expr: `{'a': [1, 2]}.deepMerge({'a': [3]})`, want: `{'a': [1, 2, 3]}`, notEqual: true},
		{
			name: "list rhs does not deep-merge list elements",
			expr: `{'a': [{'n': 'x', 'v': 1}]}.deepMerge({'a': [{'n': 'x', 'v': 2}]})`,
			want: `{'a': [{'n': 'x', 'v': 2}]}`,
		},

		// key types
		{name: "int keys merge with rhs leaf win", expr: `{1: 'a', 2: 'b'}.deepMerge({2: 'c', 3: 'd'})`, want: `{1: 'a', 2: 'c', 3: 'd'}`},

		// inverse priority is just swapped operands (no deepMergeLeft)
		{
			name: "swapped operands invert leaf priority",
			expr: `{'a': {'y': 3, 'z': 4}}.deepMerge({'a': {'x': 1, 'y': 2}})`,
			want: `{'a': {'x': 1, 'y': 2, 'z': 4}}`,
		},

		// issue #1343 shape
		{
			name: "securedpod defaults under user podSpec",
			expr: `{'securityContext': {'runAsNonRoot': true}}.deepMerge({'containers': [{'name': 'app'}], 'securityContext': {'fsGroup': 2000}})`,
			want: `{'containers': [{'name': 'app'}], 'securityContext': {'runAsNonRoot': true, 'fsGroup': 2000}}`,
		},
	}

	env := testMapsEnv(t)
	for _, tc := range deepMergeTests {
		t.Run(tc.name, func(t *testing.T) {
			got := evalCEL(t, env, tc.expr)
			want := evalCEL(t, env, tc.want)
			eq := got.Equal(want)
			if tc.notEqual {
				require.Equal(t, false, eq.Value(), "expr=%s\nwant(not)=%s\ngot=%v", tc.expr, tc.want, got)
				return
			}
			require.Equal(t, true, eq.Value(), "expr=%s\nwant=%s\ngot=%v", tc.expr, tc.want, got)
		})
	}

	// Immutability: deepMerge must not mutate the receiver literal's observable value.
	t.Run("inputs are not mutated", func(t *testing.T) {
		base := `{'a': {'x': 1}}`
		merged := evalCEL(t, env, base+`.deepMerge({'a': {'y': 2}})`)
		wantMerged := evalCEL(t, env, `{'a': {'x': 1, 'y': 2}}`)
		require.Equal(t, true, merged.Equal(wantMerged).Value())

		// Re-evaluate the original base expression; it must be unchanged.
		baseVal := evalCEL(t, env, base)
		wantBase := evalCEL(t, env, `{'a': {'x': 1}}`)
		require.Equal(t, true, baseVal.Equal(wantBase).Value())
	})
}

// TestDeepMergeNonMapArgErrors verifies runtime errors for non-map operands.
// With a dyn overload, non-map args type-check but must fail at eval time.
func TestDeepMergeNonMapArgErrors(t *testing.T) {
	env := testMapsEnv(t)
	cases := []struct {
		name string
		expr string
	}{
		{name: "rhs list", expr: `{'a': 1}.deepMerge([1, 2, 3])`},
		{name: "lhs list", expr: `[1, 2].deepMerge({'a': 1})`},
		{name: "rhs string", expr: `{'a': 1}.deepMerge('nope')`},
		{name: "lhs string", expr: `'nope'.deepMerge({'a': 1})`},
		{name: "both non-map", expr: `1.deepMerge(2)`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ast, iss := env.Compile(tc.expr)
			require.NoError(t, iss.Err(), "compile %q", tc.expr)
			prg, err := env.Program(ast)
			require.NoError(t, err)
			_, _, err = prg.Eval(cel.NoVars())
			require.Error(t, err, "expected eval error for %q", tc.expr)
		})
	}
}

// TestDeepMergeObjectTypeCompiles verifies the #1343 type-check regression:
// deepMerge accepts an opaque object-typed RHS that plain merge rejects.
func TestDeepMergeObjectTypeCompiles(t *testing.T) {
	env := testMapsEnv(t, cel.Variable("podSpec", cel.ObjectType("podspec")))

	deepExpr := `{"securityContext": {"runAsNonRoot": true}}.deepMerge(podSpec)`
	pAst, iss := env.Parse(deepExpr)
	require.NoError(t, iss.Err())
	_, iss = env.Check(pAst)
	require.NoError(t, iss.Err(), "deepMerge must type-check against object-typed field")

	// Control: the same shape with merge must still fail type-checking.
	mergeExpr := `{"securityContext": {"runAsNonRoot": true}}.merge(podSpec)`
	mpAst, iss := env.Parse(mergeExpr)
	require.NoError(t, iss.Err())
	_, iss = env.Check(mpAst)
	require.Error(t, iss.Err(), "expected merge to reject object-typed RHS")
}

// TestDeepMergeRuntimeUnstructured is the end-to-end regression test for
// GitHub issue #1343. RHS is a dyn variable bound to native
// map[string]interface{} — the shape kro gets from unstructured object fields.
func TestDeepMergeRuntimeUnstructured(t *testing.T) {
	env := testMapsEnv(t, cel.Variable("podSpec", cel.DynType))

	const expr = `{"securityContext": {"runAsNonRoot": true}}.deepMerge(podSpec)`
	const want = `{
		"containers": [{"name": "app", "image": "nginx:latest"}],
		"securityContext": {"runAsNonRoot": true, "fsGroup": 2000}
	}`

	podSpecVal := map[string]any{
		"containers": []any{
			map[string]any{"name": "app", "image": "nginx:latest"},
		},
		"securityContext": map[string]any{
			"fsGroup": int64(2000),
		},
	}

	got := evalCELWithVars(t, env, expr, map[string]any{"podSpec": podSpecVal})
	wantVal := evalCEL(t, env, want)
	require.Equal(t, true, got.Equal(wantVal).Value(), "got=%v want=%v", got, wantVal)
}

// TestDeepMergeOperandOrder documents priority via operand order (no Left/Right API).
//
// deepMerge is always RHS-wins. The two common call patterns are:
//
//	defaults.deepMerge(user)  → user wins shared leaves; defaults fill gaps  (SecuredPod)
//	user.deepMerge(defaults)  → defaults win shared leaves; user fills gaps
//
// Swapping operands inverts who wins on conflict; it is not a left-biased merge
// of the original argument order.
func TestDeepMergeOperandOrder(t *testing.T) {
	env := testMapsEnv(t,
		cel.Variable("podUser", cel.DynType),
		cel.Variable("defaults", cel.DynType),
	)

	input := map[string]any{
		"podUser": map[string]any{
			"containers": []any{
				map[string]any{"name": "app"},
			},
			"securityContext": map[string]any{
				"runAsNonRoot": false,
				"fsGroup":      int64(2000),
			},
		},
		"defaults": map[string]any{
			"securityContext": map[string]any{
				"runAsNonRoot": true,
			},
		},
	}

	cases := []struct {
		name     string
		expr     string
		want     string
		notEqual bool
	}{
		{
			name: "defaults.deepMerge(user): user wins shared leaf",
			expr: `defaults.deepMerge(podUser)`,
			want: `{
				"containers": [{"name": "app"}],
				"securityContext": {"runAsNonRoot": false, "fsGroup": 2000}
			}`,
		},
		{
			name: "user.deepMerge(defaults): defaults win shared leaf",
			expr: `podUser.deepMerge(defaults)`,
			want: `{
				"containers": [{"name": "app"}],
				"securityContext": {"runAsNonRoot": true, "fsGroup": 2000}
			}`,
		},
		{
			name:     "the two call orders are not equal when a leaf conflicts",
			expr:     `defaults.deepMerge(podUser)`,
			want:     `podUser.deepMerge(defaults)`,
			notEqual: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := evalCELWithVars(t, env, tc.expr, input)
			want := evalCELWithVars(t, env, tc.want, input)
			eq := got.Equal(want)
			if tc.notEqual {
				require.Equal(t, false, eq.Value(), "expr=%s\nwant(not)=%s\ngot=%v", tc.expr, tc.want, got)
				return
			}
			require.Equal(t, true, eq.Value(), "expr=%s\nwant=%s\ngot=%v", tc.expr, tc.want, got)
		})
	}
}

// BenchmarkDeepMerge measures the cost of deepMerge on a ~3-level nested map
// with ~20 keys to provide a stable baseline for future optimisation work.
func BenchmarkDeepMerge(b *testing.B) {
	env, err := cel.NewEnv(Maps())
	require.NoError(b, err)
	const expr = `{
		'meta': {'name': 'foo', 'namespace': 'bar', 'labels': {'app': 'myapp', 'env': 'prod'}},
		'spec': {
			'replicas': 3,
			'selector': {'matchLabels': {'app': 'myapp'}},
			'template': {
				'spec': {
					'containers': [{'name': 'main', 'image': 'nginx:1'}],
					'security': {'runAsNonRoot': true, 'fsGroup': 2000}
				}
			}
		}
	}.deepMerge({
		'meta': {'name': 'foo', 'labels': {'tier': 'backend'}},
		'spec': {
			'replicas': 5,
			'template': {
				'spec': {
					'security': {'runAsNonRoot': false, 'supplementalGroups': [1000, 2000]}
				}
			}
		},
		'status': {'phase': 'Running'}
	})`
	ast, iss := env.Compile(expr)
	require.NoError(b, iss.Err())
	prg, err := env.Program(ast)
	require.NoError(b, err)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _, err := prg.Eval(cel.NoVars())
		require.NoError(b, err)
	}
}

// evalCEL compiles and evaluates a CEL expression with no variables.
func evalCEL(t *testing.T, env *cel.Env, expr string) ref.Val {
	t.Helper()
	return evalCELWithVars(t, env, expr, cel.NoVars())
}

// evalCELWithVars compiles and evaluates a CEL expression against vars.
func evalCELWithVars(t *testing.T, env *cel.Env, expr string, vars any) ref.Val {
	t.Helper()
	ast, iss := env.Compile(expr)
	require.NoError(t, iss.Err(), "compile %q", expr)
	prg, err := env.Program(ast)
	require.NoError(t, err, "program %q", expr)
	out, _, err := prg.Eval(vars)
	require.NoError(t, err, "eval %q", expr)
	return out
}

// evalTrue parses, type-checks, and evaluates expr, requiring the result to be true
// under both the parse-only and checked programs.
func evalTrue(t *testing.T, env *cel.Env, expr string) {
	t.Helper()
	evalTrueWithVars(t, env, expr, nil)
}

// evalTrueWithVars compiles expr and requires it to evaluate to true against vars.
// When vars is nil, both parse-only and type-checked programs are evaluated.
func evalTrueWithVars(t *testing.T, env *cel.Env, expr string, vars any) {
	t.Helper()

	if vars == nil {
		vars = cel.NoVars()
		pAst, iss := env.Parse(expr)
		require.NoError(t, iss.Err(), "parse %q", expr)
		cAst, iss := env.Check(pAst)
		require.NoError(t, iss.Err(), "check %q", expr)

		for _, tc := range []struct {
			label string
			ast   *cel.Ast
		}{
			{label: "parse", ast: pAst},
			{label: "check", ast: cAst},
		} {
			prg, err := env.Program(tc.ast)
			require.NoError(t, err, "program(%s) %q", tc.label, expr)
			out, _, err := prg.Eval(vars)
			require.NoError(t, err, "eval(%s) %q", tc.label, expr)
			require.Equal(t, true, out.Value(), "eval(%s) %q", tc.label, expr)
		}
		return
	}

	out := evalCELWithVars(t, env, expr, vars)
	require.Equal(t, true, out.Value(), "eval %q", expr)
}

func testMapsEnv(t *testing.T, opts ...cel.EnvOption) *cel.Env {
	t.Helper()
	baseOpts := []cel.EnvOption{
		Maps(),
	}
	env, err := cel.NewEnv(append(baseOpts, opts...)...)
	require.NoError(t, err)
	return env
}
