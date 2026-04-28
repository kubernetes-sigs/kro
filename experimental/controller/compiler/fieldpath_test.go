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
		cel.OptionalTypes(),
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

// testAccessModeEnv creates a CEL environment for testing classifyAccessModes.
// It includes optional types support (for ?. syntax), plus ready() and updated()
// member functions declared on dyn types.
func testAccessModeEnv(t *testing.T, vars ...string) *cel.Env {
	t.Helper()
	opts := []cel.EnvOption{
		cel.HomogeneousAggregateLiterals(),
		cel.OptionalTypes(),
		cel.Function("ready",
			cel.MemberOverload("dyn_ready",
				[]*cel.Type{cel.DynType},
				cel.DynType,
			),
		),
		cel.Function("updated",
			cel.MemberOverload("dyn_updated",
				[]*cel.Type{cel.DynType},
				cel.DynType,
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
			name:      "ready function call — extracts __ready field path",
			expr:      "deploy.ready()",
			vars:      []string{"deploy"},
			scopeVars: map[string]bool{"deploy": true},
			// .ready() is a property of a node like any other — it
			// produces ["__ready"] as a field path through the same
			// mechanism as status.replicas or metadata.name.
			want: map[string][]graph.FieldPath{
				"deploy": {{"__ready"}},
			},
		},
		{
			name:      "optional select — extracts field path through ?.",
			expr:      "deploy.?status.?replicas.orValue(0)",
			vars:      []string{"deploy"},
			scopeVars: map[string]bool{"deploy": true},
			want: map[string][]graph.FieldPath{
				"deploy": {{"status", "replicas"}},
			},
		},
		{
			name:      "single optional select",
			expr:      "deploy.?status.orValue({})",
			vars:      []string{"deploy"},
			scopeVars: map[string]bool{"deploy": true},
			want: map[string][]graph.FieldPath{
				"deploy": {{"status"}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := testFieldPathEnv(t, tt.vars...)
			ast, issues := env.Compile(tt.expr)
			require.NoError(t, issues.Err())
			got := extractFieldPathsFromAST(ast.NativeRep().Expr(), tt.scopeVars, nil)

			// Normalize empty maps for comparison.
			if len(got) == 0 {
				got = map[string][]graph.FieldPath{}
			}
			if len(tt.want) == 0 {
				tt.want = map[string][]graph.FieldPath{}
			}

			assert.Equal(t, tt.want, got, "extractFieldPathsFromAST(%q)", tt.expr)
		})
	}
}

func TestClassifyAccessModes(t *testing.T) {
	tests := []struct {
		name      string
		expr      string
		vars      []string
		scopeVars map[string]bool
		want      map[string]bool
	}{
		{
			name:      "direct field select — hard",
			expr:      "deploy.status.replicas",
			vars:      []string{"deploy"},
			scopeVars: map[string]bool{"deploy": true},
			want:      map[string]bool{"deploy": false}, // false = direct (hard)
		},
		{
			name:      "optional field select — lazy",
			expr:      "deploy.?status.?replicas.orValue(0)",
			vars:      []string{"deploy"},
			scopeVars: map[string]bool{"deploy": true},
			want:      map[string]bool{"deploy": true}, // true = optional (lazy)
		},
		{
			name:      ".ready() — hard (no .orValue())",
			expr:      "deploy.ready()",
			vars:      []string{"deploy"},
			scopeVars: map[string]bool{"deploy": true},
			want:      map[string]bool{"deploy": false},
		},
		{
			name:      ".updated() — hard (no .orValue())",
			expr:      "deploy.updated()",
			vars:      []string{"deploy"},
			scopeVars: map[string]bool{"deploy": true},
			want:      map[string]bool{"deploy": false},
		},
		{
			name:      "direct + .ready() on same var — both hard",
			expr:      "deploy.ready() ? deploy.status.replicas : 0",
			vars:      []string{"deploy"},
			scopeVars: map[string]bool{"deploy": true},
			want:      map[string]bool{"deploy": false}, // both are direct access
		},
		{
			name:      "two vars — both direct",
			expr:      "deploy.ready() && svc.status.ready",
			vars:      []string{"deploy", "svc"},
			scopeVars: map[string]bool{"deploy": true, "svc": true},
			want:      map[string]bool{"deploy": false, "svc": false},
		},
		{
			name:      "bare identifier — direct",
			expr:      "deploy",
			vars:      []string{"deploy"},
			scopeVars: map[string]bool{"deploy": true},
			want:      map[string]bool{"deploy": false},
		},
		{
			name:      "chained optional select — lazy",
			expr:      "deploy.?metadata.?name.orValue('')",
			vars:      []string{"deploy"},
			scopeVars: map[string]bool{"deploy": true},
			want:      map[string]bool{"deploy": true},
		},
		{
			name:      "comprehension iter var not classified",
			expr:      "items.filter(i, i.ready())",
			vars:      []string{"items"},
			scopeVars: map[string]bool{"items": true},
			want:      map[string]bool{"items": false}, // items is accessed via .filter() which is direct (comprehension iter range)
		},
		{
			name:      "non-scope var ignored",
			expr:      "local.status.ready",
			vars:      []string{"local"},
			scopeVars: map[string]bool{"deploy": true}, // local is NOT a scope var
			want:      map[string]bool{},
		},
		{
			name:      "literal only — empty",
			expr:      `"hello"`,
			vars:      []string{},
			scopeVars: map[string]bool{},
			want:      map[string]bool{},
		},
		{
			name:      ".ready() in && chain — both hard (no .orValue())",
			expr:      "a.ready() && b.ready()",
			vars:      []string{"a", "b"},
			scopeVars: map[string]bool{"a": true, "b": true},
			want:      map[string]bool{"a": false, "b": false},
		},
		{
			name:      ".ready().orValue() — lazy",
			expr:      "deploy.ready().orValue(false)",
			vars:      []string{"deploy"},
			scopeVars: map[string]bool{"deploy": true},
			want:      map[string]bool{"deploy": true},
		},
		{
			name:      ".updated().orValue() — lazy",
			expr:      "deploy.updated().orValue(false)",
			vars:      []string{"deploy"},
			scopeVars: map[string]bool{"deploy": true},
			want:      map[string]bool{"deploy": true},
		},
		{
			name:      ".ready().orValue() and .ready() on different vars",
			expr:      "a.ready().orValue(false) && b.ready()",
			vars:      []string{"a", "b"},
			scopeVars: map[string]bool{"a": true, "b": true},
			want:      map[string]bool{"a": true, "b": false},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := testAccessModeEnv(t, tt.vars...)
			parsed, issues := env.Parse(tt.expr)
			require.NoError(t, issues.Err())
			got := classifyAccessModes(parsed.NativeRep().Expr(), tt.scopeVars, nil)

			// Normalize empty maps for comparison.
			if len(got) == 0 {
				got = map[string]bool{}
			}
			if len(tt.want) == 0 {
				tt.want = map[string]bool{}
			}

			assert.Equal(t, tt.want, got, "classifyAccessModes(%q)", tt.expr)
		})
	}
}
