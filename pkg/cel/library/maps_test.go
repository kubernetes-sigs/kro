package library

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/cel-go/cel"
)

func TestMaps(t *testing.T) {
	mapsTests := []struct {
		expr string
		err  string
	}{
		{expr: `{}.merge({}) == {}`},
		{expr: `{}.merge({'a': 1}) == {'a': 1}`},
		{expr: `{}.merge({'a': 2.1}) == {'a': 2.1}`},
		{expr: `{}.merge({'a': 'foo'}) == {'a': 'foo'}`},
		{expr: `{'a': 1}.merge({}) == {'a': 1}`},
		{expr: `{'a': 1}.merge({'b': 2}) == {'a': 1, 'b': 2}`},
		{expr: `{'a': 1}.merge({'a': 2, 'b': 2}) == {'a': 2, 'b': 2}`},

		// {expr: `{}.merge([])`, err: "ERROR: <input>:1:9: found no matching overload for 'merge' applied to 'map(dyn, dyn).(list(dyn))'"},
	}

	env := testMapsEnv(t)
	for i, tc := range mapsTests {
		t.Run(fmt.Sprintf("%d", i), func(t *testing.T) {
			var asts []*cel.Ast
			pAst, iss := env.Parse(tc.expr)
			if iss.Err() != nil {
				t.Fatalf("env.Parse(%v) failed: %v", tc.expr, iss.Err())
			}
			asts = append(asts, pAst)
			cAst, iss := env.Check(pAst)
			if iss.Err() != nil {
				t.Fatalf("env.Check(%v) failed: %v", tc.expr, iss.Err())
			}
			asts = append(asts, cAst)

			for _, ast := range asts {
				prg, err := env.Program(ast)
				if err != nil {
					t.Fatalf("env.Program() failed: %v", err)
				}
				out, _, err := prg.Eval(cel.NoVars())
				if tc.err != "" {
					if err == nil {
						t.Fatalf("got value %v, wanted error %s for expr: %s",
							out.Value(), tc.err, tc.expr)
					}
					if !strings.Contains(err.Error(), tc.err) {
						t.Errorf("got error %v, wanted error %s for expr: %s", err, tc.err, tc.expr)
					}
				} else if err != nil {
					t.Fatal(err)
				} else if out.Value() != true {
					t.Errorf("got %v, wanted true for expr: %s", out.Value(), tc.expr)
				}
			}
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
