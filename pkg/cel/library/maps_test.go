// Copyright 2025 The Kube Resource Orchestrator Authors
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
	"fmt"
	"testing"

	"github.com/google/cel-go/cel"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMaps(t *testing.T) {
	mapsTests := []struct {
		expr string
		err  require.ErrorAssertionFunc
	}{
		{expr: `{}.merge({}) == {}`},
		{expr: `{}.merge({'a': 1}) == {'a': 1}`},
		{expr: `{}.merge({'a': 2.1}) == {'a': 2.1}`},
		{expr: `{}.merge({'a': 'foo'}) == {'a': 'foo'}`},
		{expr: `{'a': 1}.merge({}) == {'a': 1}`},
		{expr: `{'a': 1}.merge({'b': 2}) == {'a': 1, 'b': 2}`},
		{expr: `{'a': 1}.merge({'a': 2, 'b': 2}) == {'a': 2, 'b': 2}`},

		{expr: `{}.merge([])`, err: func(t require.TestingT, err error, i ...interface{}) {
			require.ErrorContains(t, err, "no matching overload for 'merge'")
		}},
	}

	env := testMapsEnv(t)
	for i, tc := range mapsTests {
		t.Run(fmt.Sprintf("%d", i), func(t *testing.T) {
			r := require.New(t)

			ast, iss := env.Compile(tc.expr)
			if tc.err != nil {
				tc.err(t, iss.Err())
				return
			}
			r.NoError(iss.Err(), "compile failed for expr: %s", tc.expr)

			prg, err := env.Program(ast)
			require.NoError(t, err)

			out, _, err := prg.Eval(cel.NoVars())
			require.NoError(t, err)
			assert.True(t, out.Value().(bool))
		})
	}
}

func testMapsEnv(t *testing.T, opts ...cel.EnvOption) *cel.Env {
	t.Helper()
	baseOpts := []cel.EnvOption{
		Maps(),
	}
	env, err := cel.NewEnv(append(baseOpts, opts...)...)
	if err != nil {
		t.Fatalf("cel.NewEnv(Maps()) failed: %v", err)
	}
	return env
}
