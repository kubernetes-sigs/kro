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
	"strings"
	"testing"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// NOTE on map literal syntax: runtime.newCondition accepts a struct-style
// map literal with bare identifier keys:
//
//   runtime.newCondition({type: 'X', status: 'True', reason: 'R', message: 'M'})
//
// A parse-time macro registered in CompileOptions rewrites identifier
// keys into string literals before type-checking, so the function's
// declared signature can stay map(string, string). The macro also
// rejects quoted keys, computed keys, unknown keys, duplicate keys,
// invalid literal status values, and missing required keys (type,
// status). See newConditionMacro in runtime.go.

// runtimeEnv builds a CEL environment with the runtime library and a
// `schema` variable, matching how kro will use these expressions.
func runtimeEnv(t *testing.T) *cel.Env {
	t.Helper()
	env, err := cel.NewEnv(
		cel.Variable("schema", cel.AnyType),
		Runtime(),
	)
	require.NoError(t, err)
	return env
}

// evalRuntime compiles and evaluates expr with `runtime` bound to the
// singleton and any extra context the caller provides. It returns the
// result value or an error.
func evalRuntime(t *testing.T, env *cel.Env, expr string, extra map[string]any) (ref.Val, error) {
	t.Helper()

	ast, iss := env.Compile(expr)
	if iss.Err() != nil {
		return nil, iss.Err()
	}

	prog, err := env.Program(ast)
	require.NoError(t, err)

	ctx := map[string]any{RuntimeVarName: RuntimeSingleton}
	for k, v := range extra {
		ctx[k] = v
	}

	val, _, err := prog.Eval(ctx)
	return val, err
}

func TestRuntimeNewConditionWellFormed(t *testing.T) {
	env := runtimeEnv(t)

	val, err := evalRuntime(t, env,
		`runtime.newCondition({type: 'PrimaryReady', status: 'True', reason: 'Running', message: 'OK'})`,
		nil,
	)
	require.NoError(t, err)

	cond, ok := val.(*Condition)
	require.True(t, ok, "expected *Condition, got %T", val)
	assert.Equal(t, "PrimaryReady", cond.ConditionType)
	assert.Equal(t, "True", cond.Status)
	assert.Equal(t, "Running", cond.Reason)
	assert.Equal(t, "OK", cond.Message)
}

func TestRuntimeNewConditionFieldAccess(t *testing.T) {
	env := runtimeEnv(t)

	tests := []struct {
		name string
		expr string
		want ref.Val
	}{
		{
			name: "type field",
			expr: `runtime.newCondition({type: 'X', status: 'True', reason: '', message: ''}).type`,
			want: types.String("X"),
		},
		{
			name: "status field",
			expr: `runtime.newCondition({type: 'X', status: 'False', reason: '', message: ''}).status`,
			want: types.String("False"),
		},
		{
			name: "reason field",
			expr: `runtime.newCondition({type: 'X', status: 'True', reason: 'R', message: ''}).reason`,
			want: types.String("R"),
		},
		{
			name: "message field",
			expr: `runtime.newCondition({type: 'X', status: 'True', reason: '', message: 'M'}).message`,
			want: types.String("M"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := evalRuntime(t, env, tt.expr, nil)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestRuntimeNewConditionRejections verifies the parse-time macro
// (newConditionMacro) rejects malformed runtime.newCondition arguments
// with descriptive errors at the source position of the offending node.
func TestRuntimeNewConditionRejections(t *testing.T) {
	env := runtimeEnv(t)

	tests := []struct {
		name      string
		expr      string
		errSubstr string
	}{
		{
			name:      "unknown bare-identifier key",
			expr:      `runtime.newCondition({type: 'X', status: 'True', reason: 'R', message: 'M', extra: 'foo'})`,
			errSubstr: `unknown key "extra"`,
		},
		{
			name:      "quoted key rejected",
			expr:      `runtime.newCondition({"type": 'X', "status": 'True'})`,
			errSubstr: "keys must be bare identifiers",
		},
		{
			name:      "duplicate key rejected",
			expr:      `runtime.newCondition({type: 'X', status: 'True', type: 'Y'})`,
			errSubstr: `duplicate key "type"`,
		},
		{
			name:      "invalid status literal",
			expr:      `runtime.newCondition({type: 'X', status: 'YES', reason: 'R', message: 'M'})`,
			errSubstr: "status must be one of",
		},
		{
			name:      "missing type",
			expr:      `runtime.newCondition({status: 'True', reason: 'R', message: 'M'})`,
			errSubstr: "'type' is required",
		},
		{
			name:      "missing status",
			expr:      `runtime.newCondition({type: 'X', reason: 'R', message: 'M'})`,
			errSubstr: "'status' is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// All these errors come from the macro at parse time, so
			// evalRuntime returns the error from env.Compile.
			_, err := evalRuntime(t, env, tt.expr, nil)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.errSubstr)
		})
	}
}

// TestRuntimeNewConditionMacroFiresOnNestedCalls verifies the macro
// inspects every runtime.newCondition call site, including ones nested
// as sub-expressions inside another runtime.newCondition's argument.
//
// Nested constructions are legitimate (using inner.status as a string
// value), but a malformed inner — e.g. a literal status='BAD' — must be
// caught at parse time just like a top-level malformed call would be.
//
// Note: this exercises ONLY the case where the inner call's bad literal
// is a direct map-entry value. Bad literals reachable through ternary
// branches assigned to status (e.g. cond ? 'BAD' : ...) bypass the
// macro's literal-status check, because the macro inspects only
// direct LiteralKind values, not literals nested inside compound
// expressions. Those cases are caught at runtime by newConditionImpl.
func TestRuntimeNewConditionMacroFiresOnNestedCalls(t *testing.T) {
	env := runtimeEnv(t)

	// Outer call is well-formed. Inner call has status='BAD' literal,
	// which the macro must reject at parse time.
	expr := `runtime.newCondition({type: 'X',
		status: schema.spec.healthy
			? 'True'
			: runtime.newCondition({type: 'Y', status: 'BAD', reason: '', message: ''}).status,
		reason: '', message: ''})`

	_, err := evalRuntime(t, env, expr, nil)
	require.Error(t, err, "macro must walk nested newCondition calls and reject malformed inner")
	assert.Contains(t, err.Error(), "status must be one of")
	assert.Contains(t, err.Error(), `"BAD"`)
}

// TestRuntimeConditionLookup exercises runtime.condition(obj, type) against
// an object whose conditions list contains raw maps (the wire shape).
func TestRuntimeConditionLookup(t *testing.T) {
	env := runtimeEnv(t)

	tests := []struct {
		name string
		expr string
		want ref.Val
	}{
		{
			name: "found Ready True",
			expr: `runtime.condition(schema, 'Ready').status`,
			want: types.String("True"),
		},
		{
			name: "found Ready Type",
			expr: `runtime.condition(schema, 'Ready').type`,
			want: types.String("Ready"),
		},
		{
			name: "found ResourcesReady False",
			expr: `runtime.condition(schema, 'ResourcesReady').status`,
			want: types.String("False"),
		},
		{
			name: "not found returns empty status",
			expr: `runtime.condition(schema, 'Missing').status`,
			want: types.String(""),
		},
		{
			name: "not found returns empty type",
			expr: `runtime.condition(schema, 'Missing').type`,
			want: types.String(""),
		},
	}

	schemaVal := map[string]any{
		"status": map[string]any{
			"conditions": []any{
				map[string]any{
					"type":    "Ready",
					"status":  "True",
					"reason":  "AllOK",
					"message": "everything good",
				},
				map[string]any{
					"type":   "ResourcesReady",
					"status": "False",
					"reason": "NotReady",
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := evalRuntime(t, env, tt.expr, map[string]any{"schema": schemaVal})
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestRuntimeConditionMissingPaths verifies that runtime.condition handles
// objects that don't carry any conditions yet — common during the initial
// reconcile window before status has been populated.
func TestRuntimeConditionMissingPaths(t *testing.T) {
	env := runtimeEnv(t)

	tests := []struct {
		name string
		obj  map[string]any
	}{
		{
			name: "no status field at all",
			obj:  map[string]any{},
		},
		{
			name: "status without conditions",
			obj:  map[string]any{"status": map[string]any{}},
		},
		{
			name: "status with empty conditions list",
			obj:  map[string]any{"status": map[string]any{"conditions": []any{}}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := evalRuntime(t, env,
				`runtime.condition(schema, 'Whatever').status`,
				map[string]any{"schema": tt.obj},
			)
			require.NoError(t, err)
			// Missing paths produce a Condition with empty fields, so .status
			// resolves to the empty string.
			assert.Equal(t, types.String(""), got)
		})
	}
}

// TestRuntimeConditionComposition verifies authors can compose
// runtime.condition with their own checks to derive Ready.
func TestRuntimeConditionComposition(t *testing.T) {
	env := runtimeEnv(t)

	expr := `runtime.condition(schema, 'ResourcesReady').status == 'True'
		? runtime.newCondition({type: 'Ready', status: 'True', reason: 'AllReady', message: ''})
		: runtime.newCondition({type: 'Ready', status: 'False', reason: 'NotYet', message: ''})`

	tests := []struct {
		name       string
		resources  string
		wantStatus string
		wantReason string
	}{
		{
			name:       "ResourcesReady True yields Ready True",
			resources:  "True",
			wantStatus: "True",
			wantReason: "AllReady",
		},
		{
			name:       "ResourcesReady False yields Ready False",
			resources:  "False",
			wantStatus: "False",
			wantReason: "NotYet",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			schemaVal := map[string]any{
				"status": map[string]any{
					"conditions": []any{
						map[string]any{
							"type":   "ResourcesReady",
							"status": tt.resources,
						},
					},
				},
			}
			got, err := evalRuntime(t, env, expr, map[string]any{"schema": schemaVal})
			require.NoError(t, err)

			cond, ok := got.(*Condition)
			require.True(t, ok, "expected *Condition, got %T", got)
			assert.Equal(t, "Ready", cond.ConditionType)
			assert.Equal(t, tt.wantStatus, cond.Status)
			assert.Equal(t, tt.wantReason, cond.Reason)
		})
	}
}

// TestRuntimeNewConditionInListLiteral verifies that two
// runtime.newCondition calls can sit side-by-side as elements of a CEL
// list literal: the macro rewrites each call independently, and each
// resulting *Condition flows through CEL's list machinery as a typed
// element (not collapsed to a generic dyn).
func TestRuntimeNewConditionInListLiteral(t *testing.T) {
	env := runtimeEnv(t)

	listExpr := `[
		runtime.newCondition({type: 'A', status: 'True', reason: '', message: ''}),
		runtime.newCondition({type: 'B', status: 'False', reason: '', message: ''})
	]`

	val, err := evalRuntime(t, env, listExpr, nil)
	require.NoError(t, err)

	listVal, ok := val.Value().([]ref.Val)
	require.True(t, ok, "expected []ref.Val, got %T", val.Value())
	require.Len(t, listVal, 2)

	first, ok := listVal[0].(*Condition)
	require.True(t, ok)
	assert.Equal(t, "A", first.ConditionType)

	second, ok := listVal[1].(*Condition)
	require.True(t, ok)
	assert.Equal(t, "B", second.ConditionType)
}

// TestRuntimeNewConditionFieldsDerivedFromSchema verifies that status and
// reason can be computed from schema-driven CEL expressions
func TestRuntimeNewConditionFieldsDerivedFromSchema(t *testing.T) {
	env := runtimeEnv(t)

	expr := `runtime.newCondition({
		type: 'PrimaryReady',
		status: schema.spec.primaryHealthy ? 'True' : 'False',
		reason: schema.spec.primaryHealthy ? 'Healthy' : 'Unhealthy',
		message: 'primary health check'
	})`

	for _, healthy := range []bool{true, false} {
		t.Run(map[bool]string{true: "healthy", false: "unhealthy"}[healthy], func(t *testing.T) {
			schemaVal := map[string]any{
				"spec": map[string]any{
					"primaryHealthy": healthy,
				},
			}

			val, err := evalRuntime(t, env, expr, map[string]any{"schema": schemaVal})
			require.NoError(t, err)

			cond, ok := val.(*Condition)
			require.True(t, ok)
			assert.Equal(t, "PrimaryReady", cond.ConditionType)
			if healthy {
				assert.Equal(t, "True", cond.Status)
				assert.Equal(t, "Healthy", cond.Reason)
			} else {
				assert.Equal(t, "False", cond.Status)
				assert.Equal(t, "Unhealthy", cond.Reason)
			}
		})
	}
}

// TestRuntimeFunctionsRequireRuntimeReceiver verifies that newCondition and
// condition can only be called on the runtime variable; bare function calls
// must fail to compile.
func TestRuntimeFunctionsRequireRuntimeReceiver(t *testing.T) {
	env := runtimeEnv(t)

	tests := []struct {
		name string
		expr string
	}{
		{
			name: "newCondition without receiver",
			expr: `newCondition({type: 'X', status: 'True', reason: '', message: ''})`,
		},
		{
			name: "condition without receiver",
			expr: `condition(schema, 'X')`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, iss := env.Compile(tt.expr)
			require.NotNil(t, iss.Err(), "expected compile error for %q", tt.expr)
			require.True(t,
				strings.Contains(iss.Err().Error(), "undeclared") ||
					strings.Contains(iss.Err().Error(), "no matching"),
				"unexpected compile error: %v", iss.Err(),
			)
		})
	}
}
