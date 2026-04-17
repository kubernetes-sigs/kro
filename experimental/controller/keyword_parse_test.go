// keyword_parse_test.go — parser validation for the five-keyword schema.
//
// The parser is the enforcement point for the declared-keyword contract:
// exactly one of template / patch / ref / watch / def must be set per
// node. Each keyword has its own shape rules. The body-producing keywords
// (template / patch / def) accept either a static map or a CEL expression
// string that yields the body at runtime; Ref/Watch are identity-only and
// accept only maps. Errors are parse-time — misclassification cannot leak
// into reconcile.
package graphcontroller

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Mutual exclusivity
// ---------------------------------------------------------------------------

func TestParseKeyword_ExactlyOneKeywordRequired(t *testing.T) {
	tests := []struct {
		name    string
		raw     map[string]any
		wantMsg string
	}{
		{
			name:    "zero keywords",
			raw:     map[string]any{"id": "a"},
			wantMsg: "exactly one of",
		},
		{
			name: "template + patch",
			raw: map[string]any{
				"id":       "a",
				"template": map[string]any{"apiVersion": "v1", "kind": "ConfigMap"},
				"patch":    map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "x"}},
			},
			wantMsg: "template, patch",
		},
		{
			name: "template + ref",
			raw: map[string]any{
				"id":       "a",
				"template": map[string]any{"apiVersion": "v1", "kind": "ConfigMap"},
				"ref":      map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "x"}},
			},
			wantMsg: "template, ref",
		},
		{
			name: "ref + watch",
			raw: map[string]any{
				"id":    "a",
				"ref":   map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "x"}},
				"watch": map[string]any{"apiVersion": "v1", "kind": "ConfigMap"},
			},
			wantMsg: "ref, watch",
		},
		{
			name: "template + def",
			raw: map[string]any{
				"id":       "a",
				"template": map[string]any{"apiVersion": "v1", "kind": "ConfigMap"},
				"def":      map[string]any{"prefix": "app"},
			},
			wantMsg: "template, def",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseNodeList([]any{tc.raw})
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantMsg)
		})
	}
}

// ---------------------------------------------------------------------------
// Per-keyword classification — map form
// ---------------------------------------------------------------------------

func TestParseKeyword_ClassificationMap(t *testing.T) {
	cases := []struct {
		name string
		raw  map[string]any
		want NodeType
	}{
		{
			name: "template → Own",
			raw: map[string]any{
				"id":       "a",
				"template": map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "x"}, "data": map[string]any{"k": "v"}},
			},
			want: NodeTypeTemplate,
		},
		{
			name: "patch → Contribute",
			raw: map[string]any{
				"id":    "a",
				"patch": map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "x"}, "data": map[string]any{"k": "v"}},
			},
			want: NodeTypePatch,
		},
		{
			name: "ref → Ref",
			raw: map[string]any{
				"id":  "a",
				"ref": map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "x"}},
			},
			want: NodeTypeRef,
		},
		{
			name: "watch → Watch",
			raw: map[string]any{
				"id":    "a",
				"watch": map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "selector": map[string]any{}},
			},
			want: NodeTypeWatch,
		},
		{
			name: "def → Definition",
			raw: map[string]any{
				"id":  "a",
				"def": map[string]any{"prefix": "app"},
			},
			want: NodeTypeDef,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nodes, err := parseNodeList([]any{tc.raw})
			require.NoError(t, err)
			require.Len(t, nodes, 1)
			assert.Equal(t, tc.want, nodes[0].Type())
			assert.Empty(t, nodes[0].TemplateExpr, "map form should not populate TemplateExpr")
		})
	}
}

// ---------------------------------------------------------------------------
// Per-keyword classification — CEL-as-whole-body (string) form
// ---------------------------------------------------------------------------

// The body-producing keywords (template / patch / def) accept a string
// value as a CEL expression that evaluates to the body map at runtime.
// ExprKeyword records which classification the expression belongs to so
// downstream code can emit it back under the correct key.
func TestParseKeyword_ClassificationExpr(t *testing.T) {
	cases := []struct {
		name string
		raw  map[string]any
		want NodeType
	}{
		{
			name: "template string → Own",
			raw:  map[string]any{"id": "a", "template": "${schema.spec.template}"},
			want: NodeTypeTemplate,
		},
		{
			name: "patch string → Contribute",
			raw:  map[string]any{"id": "a", "patch": "${schema.spec.patch}"},
			want: NodeTypePatch,
		},
		{
			name: "def string → Definition",
			raw:  map[string]any{"id": "a", "def": "${computed}"},
			want: NodeTypeDef,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nodes, err := parseNodeList([]any{tc.raw})
			require.NoError(t, err)
			require.Len(t, nodes, 1)
			n := nodes[0]
			assert.Equal(t, tc.want, n.Type())
			assert.Equal(t, tc.want, n.ExprKeyword)
			assert.NotEmpty(t, n.TemplateExpr, "string form must populate TemplateExpr")
			assert.Nil(t, n.Template, "string form leaves map fields nil")
			assert.Nil(t, n.Patch)
			assert.Nil(t, n.Def)
		})
	}
}

// ---------------------------------------------------------------------------
// Ref and Watch reject string form — they are identity-only
// ---------------------------------------------------------------------------

func TestParseKeyword_RefAndWatchRejectStrings(t *testing.T) {
	tests := []struct {
		name string
		raw  map[string]any
	}{
		{"ref string rejected", map[string]any{"id": "a", "ref": "${foo}"}},
		{"watch string rejected", map[string]any{"id": "a", "watch": "${foo}"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseNodeList([]any{tc.raw})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "identity-only")
		})
	}
}

// ---------------------------------------------------------------------------
// Per-keyword shape validation (map form)
// ---------------------------------------------------------------------------

func TestParseKeyword_ShapeValidation(t *testing.T) {
	tests := []struct {
		name    string
		raw     map[string]any
		wantMsg string
	}{
		// template
		{
			name:    "template missing apiVersion",
			raw:     map[string]any{"id": "a", "template": map[string]any{"kind": "ConfigMap"}},
			wantMsg: "template: missing apiVersion",
		},
		{
			name:    "template missing kind",
			raw:     map[string]any{"id": "a", "template": map[string]any{"apiVersion": "v1"}},
			wantMsg: "template: missing kind",
		},

		// patch — requires apiVersion, kind, metadata.name
		{
			name: "patch missing metadata.name",
			raw: map[string]any{"id": "a", "patch": map[string]any{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]any{},
				"data":     map[string]any{"k": "v"},
			}},
			wantMsg: "patch: missing metadata.name",
		},
		{
			name: "patch with Force rejected",
			raw: map[string]any{"id": "a", "patch": map[string]any{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]any{
					"name":        "x",
					"annotations": map[string]any{"kro.run/apply": "Force"},
				},
				"data": map[string]any{"k": "v"},
			}},
			wantMsg: "patch: kro.run/apply: Force",
		},

		// ref — identity-only, requires metadata.name
		{
			name: "ref missing name",
			raw: map[string]any{"id": "a", "ref": map[string]any{
				"apiVersion": "v1", "kind": "ConfigMap",
			}},
			wantMsg: "ref: missing metadata.name",
		},
		{
			name: "ref with body field",
			raw: map[string]any{"id": "a", "ref": map[string]any{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]any{"name": "x"},
				"data":     map[string]any{"k": "v"},
			}},
			wantMsg: `ref: unexpected field "data"`,
		},
		{
			name: "ref with extra metadata field",
			raw: map[string]any{"id": "a", "ref": map[string]any{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]any{"name": "x", "labels": map[string]any{}},
			}},
			wantMsg: `ref: unexpected metadata field "labels"`,
		},

		// watch — must NOT have metadata.name
		{
			name: "watch with metadata.name",
			raw: map[string]any{"id": "a", "watch": map[string]any{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]any{"name": "x"},
			}},
			wantMsg: "watch: metadata.name is not valid",
		},

		// def — must NOT have apiVersion or kind
		{
			name: "def with apiVersion",
			raw: map[string]any{"id": "a", "def": map[string]any{
				"apiVersion": "v1",
			}},
			wantMsg: "def: apiVersion is not valid",
		},
		{
			name: "def with kind",
			raw: map[string]any{"id": "a", "def": map[string]any{
				"kind": "ConfigMap",
			}},
			wantMsg: "def: kind is not valid",
		},

		// String-form variants — empty string rejected
		{
			name:    "template empty string",
			raw:     map[string]any{"id": "a", "template": ""},
			wantMsg: "template: empty string",
		},
		{
			name:    "patch empty string",
			raw:     map[string]any{"id": "a", "patch": ""},
			wantMsg: "patch: empty string",
		},
		{
			name:    "def empty string",
			raw:     map[string]any{"id": "a", "def": ""},
			wantMsg: "def: empty string",
		},

		// Wrong types
		{
			name:    "template integer",
			raw:     map[string]any{"id": "a", "template": 42},
			wantMsg: "template: expected map or string",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseNodeList([]any{tc.raw})
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantMsg)
		})
	}
}

// ---------------------------------------------------------------------------
// Force annotation is valid on template:
// ---------------------------------------------------------------------------

func TestParseKeyword_ForceOnTemplate(t *testing.T) {
	nodes, err := parseNodeList([]any{
		map[string]any{
			"id": "a",
			"template": map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name":        "x",
					"annotations": map[string]any{"kro.run/apply": "Force"},
				},
				"data": map[string]any{"k": "v"},
			},
		},
	})
	require.NoError(t, err)
	require.Len(t, nodes, 1)
	assert.Equal(t, NodeTypeTemplate, nodes[0].Type())
}

// ---------------------------------------------------------------------------
// Error message lists all five keywords
// ---------------------------------------------------------------------------

func TestParseKeyword_ZeroKeywordsLists(t *testing.T) {
	_, err := parseNodeList([]any{map[string]any{"id": "a"}})
	require.Error(t, err)
	msg := err.Error()
	for _, kw := range []string{"template", "patch", "ref", "watch", "def"} {
		assert.True(t, strings.Contains(msg, kw),
			"zero-keyword error should mention %q (got: %q)", kw, msg)
	}
}

// ---------------------------------------------------------------------------
// patch body with status populates hasStatusSubresource
// ---------------------------------------------------------------------------

func TestParseKeyword_PatchWithStatus(t *testing.T) {
	nodes, err := parseNodeList([]any{
		map[string]any{
			"id": "a",
			"patch": map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]any{"name": "x"},
				"status":     map[string]any{"ready": true},
			},
		},
	})
	require.NoError(t, err)
	require.Len(t, nodes, 1)
	assert.True(t, nodes[0].hasStatusSubresource)
}

func TestParseKeyword_PatchWithoutStatus(t *testing.T) {
	nodes, err := parseNodeList([]any{
		map[string]any{
			"id": "a",
			"patch": map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]any{"name": "x"},
				"data":       map[string]any{"k": "v"},
			},
		},
	})
	require.NoError(t, err)
	require.Len(t, nodes, 1)
	assert.False(t, nodes[0].hasStatusSubresource)
}

// ---------------------------------------------------------------------------
// CEL-as-whole-body runtime error messages
// ---------------------------------------------------------------------------

// Body-producing keywords (template/patch/def) with a CEL string evaluate
// the string at reconcile time to produce the body map. The evaluator
// error messages must name the keyword in use — users trace errors by
// keyword, not by internal ExprKeyword value. Tests here exercise the
// three runtime error shapes:
//
//  1. CEL evaluates to a non-map value (wrong shape)
//  2. CEL references an undefined identifier (compile-time catch)
//
// Empty-string and wrong-type are parse-time and covered by
// TestParseKeyword_ShapeValidation.

func TestParseKeyword_ExprEvaluatesToNonMap(t *testing.T) {
	cases := []struct {
		name    string
		keyword string
		want    NodeType
	}{
		{"template expr → non-map", "template", NodeTypeTemplate},
		{"patch expr → non-map", "patch", NodeTypePatch},
		{"def expr → non-map", "def", NodeTypeDef},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// An expression that evaluates to a string, not a map.
			spec := &GraphSpec{Nodes: []Node{
				{ID: "a", TemplateExpr: "${'not a map'}", ExprKeyword: tc.want, nodeType: tc.want},
			}}
			compiled, err := compileGraphSpec(spec, nil)
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
	spec := &GraphSpec{Nodes: []Node{
		{ID: "a", TemplateExpr: "${undeclaredThing.field}", ExprKeyword: NodeTypeTemplate, nodeType: NodeTypeTemplate},
	}}
	_, err := compileGraphSpec(spec, nil)
	require.Error(t, err, "compile should reject expression referencing undeclared identifier")
	assert.Contains(t, err.Error(), "undeclaredThing",
		"compile error should name the unknown identifier")
}

// TestParseKeyword_MutualExclusionAllPairs exercises every 2-keyword
// combination so the parser's exactly-one guard is covered exhaustively
// rather than by hand-picked representatives. For each pair, the parser
// must reject with an error that names both offending keywords.
func TestParseKeyword_MutualExclusionAllPairs(t *testing.T) {
	// Smallest valid body per keyword so shape validation doesn't fire
	// before the exactly-one guard.
	bodies := map[string]any{
		"template": map[string]any{"apiVersion": "v1", "kind": "ConfigMap"},
		"patch":    map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "x"}},
		"ref":      map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "x"}},
		"watch":    map[string]any{"apiVersion": "v1", "kind": "ConfigMap"},
		"def":      map[string]any{"k": "v"},
	}
	keys := []string{"template", "patch", "ref", "watch", "def"}
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			a, b := keys[i], keys[j]
			t.Run(a+"+"+b, func(t *testing.T) {
				raw := map[string]any{
					"id": "n",
					a:    bodies[a],
					b:    bodies[b],
				}
				_, err := parseNodeList([]any{raw})
				require.Error(t, err)
				msg := err.Error()
				assert.Contains(t, msg, "exactly one of",
					"error should flag the mutual-exclusion guard: %s", msg)
				assert.Contains(t, msg, a, "error should name %q: %s", a, msg)
				assert.Contains(t, msg, b, "error should name %q: %s", b, msg)
			})
		}
	}
}

// ---------------------------------------------------------------------------
// forEach validation
// ---------------------------------------------------------------------------

// TestForEachSingleVariableAccepted confirms single-variable forEach parses.
func TestForEachSingleVariableAccepted(t *testing.T) {
	raw := []any{map[string]any{
		"id": "items",
		"template": map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata":   map[string]any{"name": "${ns.metadata.name}"},
		},
		"forEach": map[string]any{
			"ns": "${namespaces}",
		},
	}}
	nodes, err := parseNodeList(raw)
	require.NoError(t, err)
	require.Len(t, nodes, 1)
	assert.Len(t, nodes[0].ForEach, 1)
}

// ---------------------------------------------------------------------------
// Regression: forEach variable collision check is global
// ---------------------------------------------------------------------------

// TestForEachVarCollision_RegressionLaterNodeID verifies that a forEach
// variable name colliding with a node ID declared LATER in the list is
// caught. Before the fix, only collisions with already-parsed nodes were
// detected.
func TestForEachVarCollision_RegressionLaterNodeID(t *testing.T) {
	raw := []any{
		map[string]any{
			"id":      "deployer",
			"forEach": map[string]any{"target": "${items}"},
			"template": map[string]any{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]any{"name": "${target}"},
			},
		},
		// "target" is a node ID declared AFTER the forEach variable.
		map[string]any{
			"id": "target",
			"template": map[string]any{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]any{"name": "the-target"},
			},
		},
	}

	_, err := parseNodeList(raw)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "collides with node ID",
		"forEach variable matching a later node ID should be rejected")
	assert.Contains(t, err.Error(), "target",
		"error message should name the colliding variable/ID")
}

// TestForEachVarCollision_RegressionCaseInsensitive verifies case-insensitive
// collision detection between forEach variables and node IDs.
func TestForEachVarCollision_RegressionCaseInsensitive(t *testing.T) {
	raw := []any{
		map[string]any{
			"id":      "deployer",
			"forEach": map[string]any{"MyNode": "${items}"},
			"template": map[string]any{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]any{"name": "${MyNode}"},
			},
		},
		map[string]any{
			"id": "mynode",
			"template": map[string]any{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]any{"name": "x"},
			},
		},
	}

	_, err := parseNodeList(raw)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "collides with node ID",
		"case-insensitive forEach variable collision should be rejected")
}

// ---------------------------------------------------------------------------
// Regression: non-string expressions rejected
// ---------------------------------------------------------------------------

// TestNonStringExpression_RegressionReadyWhen verifies that non-string
// elements in readyWhen/includeWhen/propagateWhen are rejected with
// an error, not silently dropped.
func TestNonStringExpression_RegressionReadyWhen(t *testing.T) {
	tests := []struct {
		name  string
		field string
		value []any
	}{
		{
			name:  "readyWhen with boolean",
			field: "readyWhen",
			value: []any{true},
		},
		{
			name:  "readyWhen with integer",
			field: "readyWhen",
			value: []any{42},
		},
		{
			name:  "includeWhen with boolean",
			field: "includeWhen",
			value: []any{false},
		},
		{
			name:  "propagateWhen with map",
			field: "propagateWhen",
			value: []any{map[string]any{"key": "val"}},
		},
		{
			name:  "readyWhen mixed valid and invalid",
			field: "readyWhen",
			value: []any{"${valid.expr}", 123},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := []any{
				map[string]any{
					"id":     "node1",
					tt.field: tt.value,
					"template": map[string]any{
						"apiVersion": "v1", "kind": "ConfigMap",
						"metadata": map[string]any{"name": "x"},
					},
				},
			}

			_, err := parseNodeList(raw)
			require.Error(t, err, "non-string %s element should be rejected", tt.field)
			assert.Contains(t, err.Error(), "must be a string",
				"error should mention type requirement")
		})
	}
}
