// Copyright 2025 The Kube Resource Orchestrator Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package library

import (
	"fmt"
	"testing"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRandomString(t *testing.T) {
	env, err := cel.NewEnv(
		cel.Variable("schema", cel.AnyType),
		Random(),
	)
	require.NoError(t, err)

	tests := []struct {
		name     string
		expr     string
		length   int
		seed     string
		wantErr  bool
		errMsg   string
		validate func(*testing.T, string)
	}{
		{
			name:   "generate 10-character string",
			expr:   "random.seededString(10, 'test-seed')",
			length: 10,
			seed:   "test-seed",
			validate: func(t *testing.T, result string) {
				assert.Len(t, result, 10)
				for _, c := range result {
					assert.Contains(t, alphanumericChars, string(c), "Invalid character in random string")
				}
			},
		},
		{
			name:   "generate 20-character string",
			expr:   "random.seededString(20, 'test-seed')",
			length: 20,
			seed:   "test-seed",
			validate: func(t *testing.T, result string) {
				assert.Len(t, result, 20)
				for _, c := range result {
					assert.Contains(t, alphanumericChars, string(c), "Invalid character in random string")
				}
			},
		},
		{
			name:    "negative length",
			expr:    "random.seededString(-1, 'test-seed')",
			length:  -1,
			seed:    "test-seed",
			wantErr: true,
			errMsg:  "random.seededString length must be positive",
		},
		{
			name:    "zero length",
			expr:    "random.seededString(0, 'test-seed')",
			length:  0,
			seed:    "test-seed",
			wantErr: true,
			errMsg:  "random.seededString length must be positive",
		},
		{
			name:    "string length",
			expr:    "random.seededString('10', 'test-seed')",
			length:  10,
			seed:    "test-seed",
			wantErr: true,
		},
		{
			name:    "numeric seed",
			expr:    "random.seededString(10, 123)",
			length:  10,
			seed:    "test-seed",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ast, issues := env.Compile(tt.expr)
			if tt.wantErr && issues != nil {
				assert.Contains(t, issues.String(), tt.errMsg)
				return
			}
			require.NoError(t, issues.Err())

			program, err := env.Program(ast)
			require.NoError(t, err)

			out, _, err := program.Eval(map[string]interface{}{})
			if tt.wantErr {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.errMsg)
				return
			}
			require.NoError(t, err)

			result, ok := out.Value().(string)
			require.True(t, ok)
			tt.validate(t, result)

			// Test determinism by running the same expression again
			out2, _, err := program.Eval(map[string]interface{}{})
			require.NoError(t, err)
			result2, ok := out2.Value().(string)
			require.True(t, ok)
			assert.Equal(t, result, result2, "Random string should be deterministic")

			// Test different seeds produce different strings
			if tt.seed != "" {
				ast2, _ := env.Compile(fmt.Sprintf("random.seededString(%d, 'different-seed')", tt.length))
				program2, _ := env.Program(ast2)
				out3, _, _ := program2.Eval(map[string]interface{}{})
				result3 := out3.Value().(string)
				assert.NotEqual(t, result, result3, "Different seeds should produce different strings")
			}
		})
	}
}

func TestRandomStringErrors(t *testing.T) {
	env, err := cel.NewEnv(Random())
	require.NoError(t, err)

	testCases := []struct {
		name    string
		expr    string
		wantErr string
	}{
		{
			name:    "negative length",
			expr:    "random.seededString(-1, 'test-seed')",
			wantErr: "random.seededString length must be positive",
		},
		{
			name:    "zero length",
			expr:    "random.seededString(0, 'test-seed')",
			wantErr: "random.seededString length must be positive",
		},
		{
			name:    "missing seed argument",
			expr:    "random.seededString(10)",
			wantErr: "found no matching overload",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ast, issues := env.Compile(tc.expr)
			if issues != nil && issues.Err() != nil {
				assert.Contains(t, issues.String(), tc.wantErr)
				return
			}

			prg, err := env.Program(ast)
			require.NoError(t, err)

			result, _, err := prg.Eval(map[string]interface{}{})
			if err == nil {
				t.Error("Expected error, got none")
			}
			if errVal, ok := result.(*types.Err); !ok || !assert.Contains(t, errVal.Error(), tc.wantErr) {
				t.Errorf("Expected error containing %q, got %v", tc.wantErr, result)
			}
		})
	}
}
