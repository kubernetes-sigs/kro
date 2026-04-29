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
)

func TestPolicy(t *testing.T) {
	tests := []struct {
		name        string
		expr        string
		want        map[string]interface{}
		wantEvalErr bool
	}{
		{
			name: "policy() returns empty map",
			expr: "policy()",
			want: map[string]interface{}{},
		},
		{
			name: "policy().withRetain() returns map with deletePolicy retain",
			expr: "policy().withRetain()",
			want: map[string]interface{}{"deletePolicy": "retain"},
		},
		{
			name: "policy().withDelete() returns map with deletePolicy delete",
			expr: "policy().withDelete()",
			want: map[string]interface{}{"deletePolicy": "delete"},
		},
		{
			name:        "policy().withRetain().withDelete() errors",
			expr:        "policy().withRetain().withDelete()",
			wantEvalErr: true,
		},
		{
			name:        "policy().withDelete().withRetain() errors",
			expr:        "policy().withDelete().withRetain()",
			wantEvalErr: true,
		},
		{
			name: "policy().withRetain().withRetain() is idempotent",
			expr: "policy().withRetain().withRetain()",
			want: map[string]interface{}{"deletePolicy": "retain"},
		},
		{
			name: "policy().withDelete().withDelete() is idempotent",
			expr: "policy().withDelete().withDelete()",
			want: map[string]interface{}{"deletePolicy": "delete"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env, err := cel.NewEnv(Policy())
			require.NoError(t, err)

			ast, issues := env.Compile(tc.expr)
			require.NoError(t, issues.Err())

			prg, err := env.Program(ast)
			require.NoError(t, err)

			out, _, err := prg.Eval(map[string]interface{}{})
			if tc.wantEvalErr {
				if err != nil {
					return
				}
				assert.True(t, types.IsError(out), "expected error but got %v", out)
				return
			}
			require.NoError(t, err)

			native, err := conversion.GoNativeType(out)
			require.NoError(t, err)
			assert.Equal(t, tc.want, native)
		})
	}
}

func TestPolicyInConditionals(t *testing.T) {
	tests := []struct {
		name string
		expr string
		ctx  map[string]interface{}
		want map[string]interface{}
	}{
		{
			name: "conditional with retain",
			expr: `isProd ? policy().withRetain() : policy()`,
			ctx:  map[string]interface{}{"isProd": true},
			want: map[string]interface{}{"deletePolicy": "retain"},
		},
		{
			name: "conditional without retain",
			expr: `isProd ? policy().withRetain() : policy()`,
			ctx:  map[string]interface{}{"isProd": false},
			want: map[string]interface{}{},
		},
		{
			name: "conditional with delete policy",
			expr: `env == "prod" ? policy().withRetain() : policy().withDelete()`,
			ctx:  map[string]interface{}{"env": "dev"},
			want: map[string]interface{}{"deletePolicy": "delete"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			vars := make([]cel.EnvOption, 0, len(tc.ctx))
			for k := range tc.ctx {
				vars = append(vars, cel.Variable(k, cel.DynType))
			}

			env, err := cel.NewEnv(append(vars, Policy())...)
			require.NoError(t, err)

			ast, issues := env.Compile(tc.expr)
			require.NoError(t, issues.Err())

			prg, err := env.Program(ast)
			require.NoError(t, err)

			out, _, err := prg.Eval(tc.ctx)
			require.NoError(t, err)

			native, err := conversion.GoNativeType(out)
			require.NoError(t, err)
			assert.Equal(t, tc.want, native)
		})
	}
}

func TestPolicyLibraryName(t *testing.T) {
	lib := &policyLib{}
	assert.Equal(t, "kro.policy", lib.LibraryName())
}
