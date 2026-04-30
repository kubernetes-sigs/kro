// keyword_parse_controller_test.go — keyword parse tests that depend on
// controller-internal symbols (compileGraphSpec, evaluator).
package graphcontroller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ellistarn/kro/experimental/controller/compiler"
	"github.com/ellistarn/kro/experimental/controller/graph"
)

func TestParseKeyword_ExprEvaluatesToNonMap(t *testing.T) {
	cases := []struct {
		name    string
		keyword string
		want    graph.NodeType
	}{
		{"template expr → non-map", "template", graph.NodeTypeTemplate},
		{"patch expr → non-map", "patch", graph.NodeTypePatch},
		{"def expr → non-map", "def", graph.NodeTypeDef},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// An expression that evaluates to a string, not a map.
			spec := &graph.GraphSpec{Nodes: []graph.Node{
				node(graph.Node{ID: "a", TemplateExpr: "${'not a map'}", ExprKeyword: tc.want}, tc.want),
			}}
			compiled, err := compiler.CompileGraphSpec(spec, nil)
			require.NoError(t, err, "compile should succeed; evaluation failure is runtime")
			e := &evaluator{compiled: compiled, scope: map[string]any{}}
			_, evalErr := e.toMapNode(spec.Nodes[0])
			require.Error(t, evalErr)
			// Error names the keyword in use (via ExprKeyword.String()).
			assert.Contains(t, evalErr.Error(), tc.want.String(),
				"eval error should name the keyword in use (got: %s)", evalErr.Error())
			assert.Contains(t, evalErr.Error(), "want map",
				"eval error should explain the type mismatch")
		})
	}
}

func TestParseKeyword_ExprReferencesUndefined(t *testing.T) {
	// Compiling an expression that references an identifier not declared
	// in the graph spec fails at compile time. The parser accepts the
	// string; compileGraphSpec surfaces the error.
	spec := &graph.GraphSpec{Nodes: []graph.Node{
		node(graph.Node{ID: "a", TemplateExpr: "${undeclaredThing.field}", ExprKeyword: graph.NodeTypeTemplate}, graph.NodeTypeTemplate),
	}}
	_, err := compiler.CompileGraphSpec(spec, nil)
	require.Error(t, err, "compile should reject expression referencing undeclared identifier")
	assert.Contains(t, err.Error(), "undeclaredThing",
		"compile error should name the unknown identifier")
}
