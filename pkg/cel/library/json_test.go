// Copyright 2025 The Kubernetes Authors.
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
	"github.com/google/cel-go/common/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestJSONUnmarshal(t *testing.T) {
	env, err := cel.NewEnv(JSON())
	require.NoError(t, err)

	testCases := []struct {
		name     string
		expr     string
		expected interface{}
	}{
		{
			name:     "parse object",
			expr:     `json.unmarshal('{"name": "test", "count": 42}')`,
			expected: map[string]interface{}{"name": "test", "count": float64(42)},
		},
		{
			name:     "parse array",
			expr:     `json.unmarshal('[1, 2, 3]')`,
			expected: []interface{}{float64(1), float64(2), float64(3)},
		},
		{
			name:     "parse string",
			expr:     `json.unmarshal('"hello"')`,
			expected: "hello",
		},
		{
			name:     "parse number",
			expr:     `json.unmarshal('123')`,
			expected: float64(123),
		},
		{
			name:     "parse boolean",
			expr:     `json.unmarshal('true')`,
			expected: true,
		},
		{
			name:     "parse null",
			expr:     `json.unmarshal('null') == null`,
			expected: true,
		},
		{
			name:     "access nested field",
			expr:     `json.unmarshal('{"nested": {"value": "deep"}}').nested.value`,
			expected: "deep",
		},
		{
			name:     "access array element",
			expr:     `json.unmarshal('[1, 2, 3]')[1]`,
			expected: float64(2),
		},
		{
			name:     "access object field",
			expr:     `json.unmarshal('{"name": "test"}').name`,
			expected: "test",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ast, issues := env.Compile(tc.expr)
			require.NoError(t, issues.Err())

			prg, err := env.Program(ast)
			require.NoError(t, err)

			out, _, err := prg.Eval(map[string]interface{}{})
			require.NoError(t, err)

			result := out.Value()
			assert.Equal(t, tc.expected, result)
		})
	}
}

func TestJSONUnmarshalErrors(t *testing.T) {
	env, err := cel.NewEnv(JSON())
	require.NoError(t, err)

	testCases := []struct {
		name    string
		expr    string
		wantErr string
	}{
		{
			name:    "invalid JSON",
			expr:    `json.unmarshal('{"invalid"}')`,
			wantErr: "failed to parse JSON",
		},
		{
			name:    "empty string",
			expr:    `json.unmarshal('')`,
			wantErr: "failed to parse JSON",
		},
		{
			name:    "non-string argument",
			expr:    `json.unmarshal(123)`,
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

func TestJSONMarshal(t *testing.T) {
	env, err := cel.NewEnv(JSON())
	require.NoError(t, err)

	testCases := []struct {
		name     string
		expr     string
		expected string
	}{
		{
			name:     "marshal object",
			expr:     `json.marshal({"name": "test", "count": 42})`,
			expected: `{"count":42,"name":"test"}`,
		},
		{
			name:     "marshal array",
			expr:     `json.marshal([1, 2, 3])`,
			expected: `[1,2,3]`,
		},
		{
			name:     "marshal string",
			expr:     `json.marshal("hello")`,
			expected: `"hello"`,
		},
		{
			name:     "marshal number",
			expr:     `json.marshal(42)`,
			expected: `42`,
		},
		{
			name:     "marshal boolean",
			expr:     `json.marshal(true)`,
			expected: `true`,
		},
		{
			name:     "marshal null",
			expr:     `json.marshal(null)`,
			expected: `null`,
		},
		{
			name:     "marshal nested object",
			expr:     `json.marshal({"outer": {"inner": "value"}})`,
			expected: `{"outer":{"inner":"value"}}`,
		},
		{
			name:     "roundtrip",
			expr:     `json.marshal(json.unmarshal('{"name":"test","count":42}'))`,
			expected: `{"count":42,"name":"test"}`,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ast, issues := env.Compile(tc.expr)
			require.NoError(t, issues.Err())

			prg, err := env.Program(ast)
			require.NoError(t, err)

			out, _, err := prg.Eval(map[string]interface{}{})
			require.NoError(t, err)

			result := out.Value()
			assert.Equal(t, tc.expected, result)
		})
	}
}

func TestJSONVersion(t *testing.T) {
	lib := &jsonLibrary{}
	JSONVersion(1)(lib)
	assert.Equal(t, uint32(1), lib.version)
}
