package compiler

import (
	"testing"

	"github.com/google/cel-go/cel"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/kubernetes-sigs/kro/experimental/controller/graph"
)

// testFieldPathEnv creates a CEL environment with the given scope variable names
// declared as dyn types, plus the custom ready() member function, suitable
// for testing field path extraction.
func testFieldPathEnv(t *testing.T, vars ...string) *cel.Env {
	t.Helper()
	opts := []cel.EnvOption{
		cel.HomogeneousAggregateLiterals(),
		// Register the custom ready() member function so expressions like
		// deploy.ready() compile without errors.
		cel.Function("ready",
			cel.MemberOverload("dyn_ready",
				[]*cel.Type{cel.DynType},
				cel.BoolType,
			),
		),
	}
	for _, v := range vars {
		opts = append(opts, cel.Variable(v, cel.DynType))
	}
	env, err := cel.NewEnv(opts...)
	require.NoError(t, err)
	return env
}

func TestExtractFieldPaths(t *testing.T) {
	tests := []struct {
		name      string
		expr      string
		vars      []string
		scopeVars map[string]bool
		want      map[string][]graph.FieldPath
	}{
		{
			name:      "simple scalar path",
			expr:      "deploy.status.availableReplicas",
			vars:      []string{"deploy"},
			scopeVars: map[string]bool{"deploy": true},
			want: map[string][]graph.FieldPath{
				"deploy": {{"status", "availableReplicas"}},
			},
		},
		{
			name:      "single section",
			expr:      "deploy.spec",
			vars:      []string{"deploy"},
			scopeVars: map[string]bool{"deploy": true},
			want: map[string][]graph.FieldPath{
				"deploy": {{"spec"}},
			},
		},
		{
			name:      "compound expression — two different variables",
			expr:      "deploy.spec.replicas + svc.spec.ports",
			vars:      []string{"deploy", "svc"},
			scopeVars: map[string]bool{"deploy": true, "svc": true},
			want: map[string][]graph.FieldPath{
				"deploy": {{"spec", "replicas"}},
				"svc":    {{"spec", "ports"}},
			},
		},
		{
			name:      "ternary — same path in both branches deduplicates",
			expr:      "deploy.spec.replicas > 0 ? deploy.spec.replicas : 1",
			vars:      []string{"deploy"},
			scopeVars: map[string]bool{"deploy": true},
			want: map[string][]graph.FieldPath{
				"deploy": {{"spec", "replicas"}},
			},
		},
		{
			name:      "ternary — different paths in branches",
			expr:      "deploy.status.ready ? deploy.spec.replicas : deploy.spec.minReplicas",
			vars:      []string{"deploy"},
			scopeVars: map[string]bool{"deploy": true},
			want: map[string][]graph.FieldPath{
				"deploy": {
					{"status", "ready"},
					{"spec", "replicas"},
					{"spec", "minReplicas"},
				},
			},
		},
		{
			name:      "map index — terminates at index operator",
			expr:      `deploy.metadata.labels["app"]`,
			vars:      []string{"deploy"},
			scopeVars: map[string]bool{"deploy": true},
			want: map[string][]graph.FieldPath{
				"deploy": {{"metadata", "labels"}},
			},
		},
		{
			name:      "has macro — presence test",
			expr:      "has(deploy.status.conditions)",
			vars:      []string{"deploy"},
			scopeVars: map[string]bool{"deploy": true},
			want: map[string][]graph.FieldPath{
				"deploy": {{"status", "conditions"}},
			},
		},
		{
			name:      "filter comprehension — terminates at comprehension boundary",
			expr:      `deploy.status.conditions.filter(c, c.type == "Available")`,
			vars:      []string{"deploy"},
			scopeVars: map[string]bool{"deploy": true},
			want: map[string][]graph.FieldPath{
				// The path terminates at conditions because filter is a
				// comprehension. The iteration variable 'c' is not a scope var.
				"deploy": {{"status", "conditions"}},
			},
		},
		{
			name:      "exists comprehension",
			expr:      `deploy.status.conditions.exists(c, c.type == "Available" && c.status == "True")`,
			vars:      []string{"deploy"},
			scopeVars: map[string]bool{"deploy": true},
			want: map[string][]graph.FieldPath{
				"deploy": {{"status", "conditions"}},
			},
		},
		{
			name:      "bare identifier — entire object referenced",
			expr:      "deploy",
			vars:      []string{"deploy"},
			scopeVars: map[string]bool{"deploy": true},
			want: map[string][]graph.FieldPath{
				"deploy": {nil},
			},
		},
		{
			name:      "non-scope variable ignored",
			expr:      "someLocal.field",
			vars:      []string{"someLocal"},
			scopeVars: map[string]bool{"deploy": true}, // someLocal is not a scope var
			want:      map[string][]graph.FieldPath{},
		},
		{
			name:      "literal only — no paths",
			expr:      `"hello"`,
			vars:      []string{},
			scopeVars: map[string]bool{},
			want:      map[string][]graph.FieldPath{},
		},
		{
			name:      "size function on field",
			expr:      "size(deploy.spec.containers)",
			vars:      []string{"deploy"},
			scopeVars: map[string]bool{"deploy": true},
			want: map[string][]graph.FieldPath{
				"deploy": {{"spec", "containers"}},
			},
		},
		{
			name:      "nested function call — string concatenation",
			expr:      `deploy.metadata.name + "-svc"`,
			vars:      []string{"deploy"},
			scopeVars: map[string]bool{"deploy": true},
			want: map[string][]graph.FieldPath{
				"deploy": {{"metadata", "name"}},
			},
		},
		{
			name:      "comparison across two variables",
			expr:      "deploy.spec.replicas == config.data.replicas",
			vars:      []string{"deploy", "config"},
			scopeVars: map[string]bool{"deploy": true, "config": true},
			want: map[string][]graph.FieldPath{
				"deploy": {{"spec", "replicas"}},
				"config": {{"data", "replicas"}},
			},
		},
		{
			name:      "ready function call — not a select chain",
			expr:      "deploy.ready()",
			vars:      []string{"deploy"},
			scopeVars: map[string]bool{"deploy": true},
			// ready() is a CallExpr with target=Ident("deploy"). The
			// chain is Ident → Call, not Ident → Select, so no path
			// is extracted. This is correct — readiness is a runtime
			// property, not a field path.
			want: map[string][]graph.FieldPath{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := testFieldPathEnv(t, tt.vars...)
			ast, issues := env.Compile(tt.expr)
			require.NoError(t, issues.Err())
			got := ExtractFieldPathsFromAST(ast.NativeRep().Expr(), tt.scopeVars, nil)

			// Normalize empty maps for comparison.
			if len(got) == 0 {
				got = map[string][]graph.FieldPath{}
			}
			if len(tt.want) == 0 {
				tt.want = map[string][]graph.FieldPath{}
			}

			assert.Equal(t, tt.want, got, "ExtractFieldPathsFromAST(%q)", tt.expr)
		})
	}
}
