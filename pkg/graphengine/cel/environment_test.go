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

package cel

import (
	"reflect"
	"sort"
	"testing"

	"github.com/google/cel-go/cel"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWithResourceIDs(t *testing.T) {
	tests := []struct {
		name string
		ids  []string
		want []string
	}{
		{
			name: "empty ids",
			ids:  []string{},
			want: []string(nil),
		},
		{
			name: "single id",
			ids:  []string{"resource1"},
			want: []string{"resource1"},
		},
		{
			name: "multiple ids",
			ids:  []string{"resource1", "resource2", "resource3"},
			want: []string{"resource1", "resource2", "resource3"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := &envOptions{}
			WithResourceIDs(tt.ids)(opts)
			assert.Equal(t, tt.want, opts.resourceIDs)
		})
	}
}

func TestWithCustomDeclarations(t *testing.T) {
	tests := []struct {
		name         string
		declarations []cel.EnvOption
		wantLen      int
	}{
		{
			name:         "empty declarations",
			declarations: []cel.EnvOption{},
			wantLen:      0,
		},
		{
			name:         "single declaration",
			declarations: []cel.EnvOption{cel.Variable("test", cel.StringType)},
			wantLen:      1,
		},
		{
			name: "multiple declarations",
			declarations: []cel.EnvOption{
				cel.Variable("test1", cel.AnyType),
				cel.Variable("test2", cel.StringType),
			},
			wantLen: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := &envOptions{}
			WithCustomDeclarations(tt.declarations)(opts)
			assert.Len(t, opts.customDeclarations, tt.wantLen)
		})
	}
}

func TestDefaultEnvironment(t *testing.T) {
	tests := []struct {
		name    string
		options []EnvOption
		wantErr bool
	}{
		{
			name:    "no options",
			options: nil,
			wantErr: false,
		},
		{
			name: "with resource IDs",
			options: []EnvOption{
				WithResourceIDs([]string{"resource1", "resource2"}),
			},
			wantErr: false,
		},
		{
			name: "with custom declarations",
			options: []EnvOption{
				WithCustomDeclarations([]cel.EnvOption{
					cel.Variable("custom", cel.StringType),
				}),
			},
			wantErr: false,
		},
		{
			name: "with both resource IDs and custom declarations",
			options: []EnvOption{
				WithResourceIDs([]string{"resource1"}),
				WithCustomDeclarations([]cel.EnvOption{
					cel.Variable("custom", cel.StringType),
				}),
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env, err := DefaultEnvironment(tt.options...)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
			assert.NotNil(t, env)
		})
	}
}

// TestDefaultEnvironment_KubernetesLibraries verifies that the default
// environment includes the Kubernetes CEL libraries we rely on, such as
// the URL library used by expressions like:
//   ${url(test.testList[0].uriTemplate).getHost()}

func TestDefaultEnvironment_KubernetesLibraries(t *testing.T) {
	env, err := DefaultEnvironment()
	require.NoError(t, err, "failed to create CEL env")

	tests := []struct {
		name string
		expr string
	}{
		{
			name: "url library",
			expr: `url("https://example.com/foo").getHost()`,
		},
		{
			name: "quantity library",
			expr: `quantity("500Mi").isLessThan(quantity("1Gi"))`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, issues := env.Compile(tc.expr)
			require.NoError(t, issues.Err(), "expected expression to compile without errors")
		})
	}
}

// TestDefaultEnvironment_TwoVarComprehensions verifies that the two-variable
// comprehension macros (transformMap, transformMapEntry, transformList) from
// ext.TwoVarComprehensions() work through kro's CEL environment.
func TestDefaultEnvironment_TwoVarComprehensions(t *testing.T) {
	env, err := DefaultEnvironment()
	require.NoError(t, err, "failed to create CEL env")

	tests := []struct {
		name string
		expr string
		want any
	}{
		{
			name: "transformMap on list doubles values",
			expr: `[10, 20, 30].transformMap(i, v, v * 2)`,
			want: map[int64]int64{0: 20, 1: 40, 2: 60},
		},
		{
			name: "transformMap on list with filter",
			expr: `[10, 20, 30].transformMap(i, v, i > 0, v * 2)`,
			want: map[int64]int64{1: 40, 2: 60},
		},
		{
			name: "transformMap on map adds 10 to values",
			expr: `{'a': 1, 'b': 2}.transformMap(k, v, v + 10)`,
			want: map[string]int64{"a": 11, "b": 12},
		},
		{
			name: "transformMapEntry on list swaps key/value",
			expr: `['x','y'].transformMapEntry(i, v, {v: i})`,
			want: map[string]int64{"x": 0, "y": 1},
		},
		{
			name: "transformMapEntry on map doubles value",
			expr: `{'a': 1}.transformMapEntry(k, v, {k: v * 2})`,
			want: map[string]int64{"a": 2},
		},
		{
			name: "transformList on map extracts keys",
			expr: `{'a': 1, 'b': 2}.transformList(k, v, k)`,
			want: []string{"a", "b"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ast, issues := env.Compile(tc.expr)
			require.NoError(t, issues.Err(), "compile failed")

			prog, err := env.Program(ast)
			require.NoError(t, err, "program creation failed")

			out, _, err := prog.Eval(cel.NoVars())
			require.NoError(t, err, "eval failed")

			// Convert CEL ref.Val result to a native Go value for comparison.
			got, err := out.ConvertToNative(reflect.TypeOf(tc.want))
			require.NoError(t, err, "native conversion failed")

			if s, ok := got.([]string); ok {
				sort.Strings(s)
			}
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestDefaultEnvironment_CelBind verifies that cel.bind() expressions compile
// and evaluate correctly through kro's default CEL environment. cel.bind()
// introduces a named local variable scoped to the body expression, avoiding
// repeated sub-expression evaluation.
func TestDefaultEnvironment_CelBind(t *testing.T) {
	env, err := DefaultEnvironment()
	require.NoError(t, err, "failed to create CEL env")

	tests := []struct {
		name string
		expr string
		want any
	}{
		{
			name: "basic bind",
			expr: `cel.bind(x, 6, x * x)`,
			want: int64(36),
		},
		{
			name: "bind reuses value in multiple places",
			expr: `cel.bind(n, 3, n + n + n)`,
			want: int64(9),
		},
		{
			name: "nested bind",
			expr: `cel.bind(a, 2, cel.bind(b, a + 3, a * b))`,
			want: int64(10),
		},
		{
			name: "bind with string",
			expr: `cel.bind(s, "hello", s + " world")`,
			want: "hello world",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ast, issues := env.Compile(tc.expr)
			require.NoError(t, issues.Err(), "compile failed")

			prog, err := env.Program(ast)
			require.NoError(t, err, "program creation failed")

			out, _, err := prog.Eval(cel.NoVars())
			require.NoError(t, err, "eval failed")

			assert.Equal(t, tc.want, out.Value())
		})
	}
}

func TestBaseDeclarations_ReturnsSameSlice(t *testing.T) {
	a := BaseDeclarations()
	b := BaseDeclarations()
	assert.Equal(t, len(a), len(b))
	// Both calls should return the same backing array (cached via sync.Once)
	if len(a) > 0 && len(b) > 0 {
		assert.Same(t, &a[0], &b[0], "expected same backing array from cached BaseDeclarations")
	}
}

func TestBaseEnv_Extend_PreservesLibraries(t *testing.T) {
	// Verify that extending the cached base env still has all libraries available
	env, err := DefaultEnvironment(
		WithResourceIDs([]string{"myResource"}),
	)
	require.NoError(t, err)

	// Libraries from BaseDeclarations should be available
	expr := `url("https://example.com").getHost()`
	_, issues := env.Compile(expr)
	assert.NoError(t, issues.Err(), "URL library should be available via base.Extend()")

	// Custom variable should also work
	expr2 := `myResource`
	_, issues2 := env.Compile(expr2)
	assert.NoError(t, issues2.Err(), "extended variable should be available")
}

func TestBaseEnv_Extend_MultipleEnvironments(t *testing.T) {
	// Create two environments with different options from the same base
	env1, err := DefaultEnvironment(WithResourceIDs([]string{"alpha"}))
	require.NoError(t, err)

	env2, err := DefaultEnvironment(WithResourceIDs([]string{"beta"}))
	require.NoError(t, err)

	// Each should have its own variable, not the other's
	_, issues := env1.Compile("alpha")
	assert.NoError(t, issues.Err())

	_, issues = env2.Compile("beta")
	assert.NoError(t, issues.Err())
}

func BenchmarkDefaultEnvironment(b *testing.B) {
	for b.Loop() {
		_, err := DefaultEnvironment(WithResourceIDs([]string{"a", "b", "c"}))
		if err != nil {
			b.Fatal(err)
		}
	}
}

func Test_CELEnvHasFunction(t *testing.T) {
	env, err := DefaultEnvironment()
	require.NoError(t, err, "failed to create CEL env")
	expectedFns := []string{
		"_+_", "_-_", "_*_", "_/_", "_%_",
		"_<_", "_<=_", "_>_", "_>=_", "_==_", "_!=_",
		"_&&_", "_||_", "_?_:_", "_[_]",
		"size", "in", "matches",
		// types
		"int", "uint", "double", "bool", "string", "bytes", "timestamp", "duration", "type",
		// Custom functions
		"random.seededString",
		"json.unmarshal",
		"json.marshal",
	}
	for _, fn := range expectedFns {
		t.Run(fn, func(t *testing.T) {
			assert.True(t, env.HasFunction(fn), "function %q not available in env", fn)
		})
	}
}
