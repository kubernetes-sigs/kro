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

	"github.com/kubernetes-sigs/kro/pkg/cel/conversion"
	"github.com/kubernetes-sigs/kro/pkg/cel/sentinels"
)

func TestOmit(t *testing.T) {
	env, err := cel.NewEnv(Omit())
	require.NoError(t, err)

	ast, issues := env.Compile("omit()")
	require.NoError(t, issues.Err())

	prg, err := env.Program(ast)
	require.NoError(t, err)

	out, _, err := prg.Eval(map[string]interface{}{})
	require.NoError(t, err)

	native, err := conversion.GoNativeType(out)
	require.NoError(t, err)
	assert.True(t, sentinels.IsOmit(native))
}

func TestOmitInTernary(t *testing.T) {
	env, err := cel.NewEnv(Omit(), cel.Variable("x", cel.StringType))
	require.NoError(t, err)

	tests := []struct {
		name        string
		expr        string
		ctx         map[string]interface{}
		wantOmit    bool
		wantEvalErr bool
		wantString  string
	}{
		{
			name:     "ternary selects omit branch",
			expr:     `x == "" ? omit() : x`,
			ctx:      map[string]interface{}{"x": ""},
			wantOmit: true,
		},
		{
			name:       "ternary selects value branch",
			expr:       `x == "" ? omit() : x`,
			ctx:        map[string]interface{}{"x": "hello"},
			wantOmit:   false,
			wantString: "hello",
		},
		{
			name:       "omit in else branch, condition true",
			expr:       `x != "" ? x : omit()`,
			ctx:        map[string]interface{}{"x": "hello"},
			wantOmit:   false,
			wantString: "hello",
		},
		{
			name:     "omit in else branch, condition false",
			expr:     `x != "" ? x : omit()`,
			ctx:      map[string]interface{}{"x": ""},
			wantOmit: true,
		},
		{
			name:        "invalid omit operation, arithmetic",
			expr:        `omit() + 1`,
			wantOmit:    false,
			wantEvalErr: true,
		},
		{
			name:        "invalid omit operation, same-type equality",
			expr:        `omit() == omit()`,
			wantOmit:    false,
			wantEvalErr: true,
		},
		{
			name:        "invalid omit in string template, prefix",
			expr:        `"prefix-" + omit()`,
			wantOmit:    false,
			wantEvalErr: true,
		},
		{
			name:        "invalid omit in string template, full",
			expr:        `"prefix-" + omit() + "-suffix"`,
			wantOmit:    false,
			wantEvalErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ast, issues := env.Compile(tc.expr)
			require.NoError(t, issues.Err())

			prg, err := env.Program(ast)
			require.NoError(t, err)

			out, _, err := prg.Eval(tc.ctx)
			if tc.wantEvalErr {
				if err != nil {
					return
				}
				assert.True(t, types.IsError(out), "expected error but got %v", out)
				return
			}
			assert.NoError(t, err)

			native, err := conversion.GoNativeType(out)
			require.NoError(t, err)

			if tc.wantOmit {
				assert.True(t, sentinels.IsOmit(native))
			} else {
				assert.False(t, sentinels.IsOmit(native))
				assert.Equal(t, tc.wantString, native)
			}
		})
	}
}

func TestOmitLibraryName(t *testing.T) {
	lib := &omitLib{}
	assert.Equal(t, "kro.omit", lib.LibraryName())
}
