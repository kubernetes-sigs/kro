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

func runtimeEnv(t *testing.T) *cel.Env {
	t.Helper()
	env, err := cel.NewEnv(
		cel.Variable("schema", cel.AnyType),
		Runtime(),
	)
	require.NoError(t, err)
	env, err = env.Extend(cel.CustomTypeProvider(ConditionTypeProvider(env.CELTypeProvider())))
	require.NoError(t, err)
	return env
}

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

func TestRuntimeNewConditionUnknownFieldRejected(t *testing.T) {
	env := runtimeEnv(t)

	expr := `runtime.newCondition({type: 'X', status: 'True', reason: '', message: ''}).bogus`

	_, iss := env.Compile(expr)
	require.NotNil(t, iss.Err(), "expected compile error for unknown field access")
	assert.Contains(t, iss.Err().Error(), "undefined field 'bogus'")
}

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
			_, err := evalRuntime(t, env, tt.expr, nil)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.errSubstr)
		})
	}
}

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
			assert.Equal(t, types.String(""), got)
		})
	}
}

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

func TestRuntimeNewConditionRejectsInvalidComputedStatus(t *testing.T) {
	env := runtimeEnv(t)

	expr := `runtime.newCondition({type: 'X', status: schema.spec.s, reason: '', message: ''})`
	schemaVal := map[string]any{"spec": map[string]any{"s": "YES"}}

	_, err := evalRuntime(t, env, expr, map[string]any{"schema": schemaVal})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "status must be one of")
}

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

func TestRuntimeNewConditionRejectsEmptyComputedType(t *testing.T) {
	env := runtimeEnv(t)

	schemaVal := map[string]any{"spec": map[string]any{"name": ""}}
	_, err := evalRuntime(t, env,
		`runtime.newCondition({type: schema.spec.name, status: 'True', reason: '', message: ''})`,
		map[string]any{"schema": schemaVal},
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "type must not be empty")
}
