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
	"testing"

	"github.com/google/cel-go/cel"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// validatorEnv builds a CEL env that knows the runtime library and the
// `schema` variable, suitable for parsing condition expressions.
func validatorEnv(t *testing.T) *cel.Env {
	t.Helper()
	env, err := cel.NewEnv(
		cel.Variable("schema", cel.AnyType),
		Runtime(),
	)
	require.NoError(t, err)
	return env
}

func TestValidateConditionExpressions_AcceptsValid(t *testing.T) {
	env := validatorEnv(t)

	tests := []struct {
		name string
		exprs []string
	}{
		{
			name: "single well-formed condition",
			exprs: []string{
				`runtime.newCondition({type: 'PrimaryReady', status: 'True', reason: 'OK', message: 'all good'})`,
			},
		},
		{
			name: "multiple well-formed conditions",
			exprs: []string{
				`runtime.newCondition({type: 'A', status: 'True', reason: '', message: ''})`,
				`runtime.newCondition({type: 'B', status: 'False', reason: '', message: ''})`,
				`runtime.newCondition({type: 'C', status: 'Unknown', reason: '', message: ''})`,
			},
		},
		{
			name: "condition reading kro built-in (not a self-reference)",
			exprs: []string{
				`runtime.newCondition({type: 'Ready',
					status: runtime.condition(schema, 'ResourcesReady').status,
					reason: '', message: ''})`,
			},
		},
		{
			name: "dynamic status value (macro skips literal validation; runtime checks)",
			exprs: []string{
				`runtime.newCondition({type: 'X',
					status: schema.spec.someStatus,
					reason: '', message: ''})`,
			},
		},
		{
			name: "dynamic type lookup in runtime.condition (self-reference rule skips dynamic types)",
			exprs: []string{
				`runtime.newCondition({type: 'A', status: 'True', reason: '', message: ''})`,
				`runtime.condition(schema, schema.spec.someType).status`,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateConditionExpressions(env, tt.exprs)
			assert.NoError(t, err)
		})
	}
}

// Structural validation of runtime.newCondition (key names, status
// literals, required fields) is enforced by the parse-time macro in
// runtime.go and tested in runtime_test.go's TestRuntimeNewConditionRejections.
// This file's tests focus on the self-reference rule, which is the only
// validation responsibility remaining in ValidateConditionExpressions.

func TestValidateConditionExpressions_RejectsSelfReference(t *testing.T) {
	env := validatorEnv(t)

	exprs := []string{
		// Defines PrimaryReady as a custom condition.
		`runtime.newCondition({type: 'PrimaryReady', status: 'True', reason: '', message: ''})`,
		// Tries to read PrimaryReady from schema — illegal self-reference.
		`runtime.newCondition({type: 'Ready',
			status: runtime.condition(schema, 'PrimaryReady').status,
			reason: '', message: ''})`,
	}

	err := ValidateConditionExpressions(env, exprs)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "custom conditions cannot reference each other")
	assert.Contains(t, err.Error(), `"PrimaryReady"`)
}

func TestValidateConditionExpressions_AllowsBuiltInLookup(t *testing.T) {
	env := validatorEnv(t)

	// runtime.condition(schema, 'ResourcesReady') is allowed because
	// ResourcesReady is a kro built-in, not defined in this expression set.
	exprs := []string{
		`runtime.newCondition({type: 'PrimaryReady', status: 'True', reason: '', message: ''})`,
		`runtime.newCondition({type: 'Ready',
			status: runtime.condition(schema, 'ResourcesReady').status,
			reason: '', message: ''})`,
	}

	err := ValidateConditionExpressions(env, exprs)
	assert.NoError(t, err)
}

func TestValidateConditionExpressions_EmptyList(t *testing.T) {
	env := validatorEnv(t)

	err := ValidateConditionExpressions(env, nil)
	assert.NoError(t, err)

	err = ValidateConditionExpressions(env, []string{})
	assert.NoError(t, err)
}

func TestValidateConditionExpressions_ParseErrorsAreSilent(t *testing.T) {
	env := validatorEnv(t)

	// Garbage expression — parse will fail, but the validator shouldn't
	// surface that. Parse errors are the regular CEL pipeline's job.
	exprs := []string{`this is not (((valid CEL`}

	err := ValidateConditionExpressions(env, exprs)
	assert.NoError(t, err, "validator should silently skip parse errors; CEL handles those")
}

func TestValidateConditionExpressions_NonRuntimeCallsIgnored(t *testing.T) {
	// Calls that look superficially similar but aren't runtime.* should be
	// ignored by the validator AND the parse-time macro. These will parse
	// fine but type-check would fail (because the receivers don't expose
	// these methods); the validator only checks AST shape and the macro
	// only fires for receivers named "runtime".
	envForNonRuntime, err := cel.NewEnv(
		cel.Variable("schema", cel.AnyType),
		Runtime(),
	)
	require.NoError(t, err)

	exprs := []string{
		// schema.spec.condition(...) — not a runtime call.
		`schema.spec.condition(schema, 'X')`,
		// schema.newCondition(...) — receiver is "schema", not "runtime".
		// The macro must skip this; if it didn't, parse would fail because
		// of the unknown bare-identifier key "extra". Quoted keys also
		// confirm the macro stays out of non-runtime receivers.
		`schema.newCondition({"extra": 'allowed-here'})`,
	}

	err = ValidateConditionExpressions(envForNonRuntime, exprs)
	assert.NoError(t, err)
}
