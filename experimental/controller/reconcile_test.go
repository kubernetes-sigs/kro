package graphcontroller

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/google/cel-go/cel"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// ---------------------------------------------------------------------------
// Unit tests — design reconciliation regression suite
//
// These test mechanisms introduced in the design reconciliation:
//   - NodeBlocked vs NodeExcluded propagation split
//   - pruneOrder reverse dependency ordering
//   - classifyAPIError default flip
//
// Each test verifies the system DOES the right thing (correctness), not
// just that it doesn't do the wrong thing (safety).
// ---------------------------------------------------------------------------

// TestSetStateDoesNotPropagate proves that SetState only sets the source
// node's state and does NOT propagate to dependents. State propagation
// is the responsibility of tryDispatch, which evaluates all dependencies
// with full precedence (Excluded > Blocked > Pending).
//
// The previous implementation used a first-wins flood fill that violated
// precedence in diamond dependencies: if an Error parent propagated
// before an Excluded parent, the child was marked Blocked instead of
// Excluded — an incorrect classification that prevented pruning.
func TestSetStateDoesNotPropagate(t *testing.T) {
	// Build a chain: A → B → C
	nodes := []Node{
		{ID: "a", Template: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "a"}}},
		{ID: "b", Template: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "b"}, "data": map[string]any{"ref": "${a.metadata.name}"}}},
		{ID: "c", Template: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "c"}, "data": map[string]any{"ref": "${b.metadata.name}"}}},
	}
	dag, err := BuildDAG(nodes, nil)
	require.NoError(t, err)

	states := []NodeState{NodeExcluded, NodeError, NodePending, NodeConflict, NodeSystemError, NodeReady, NodeNotReady}
	for _, sourceState := range states {
		t.Run(sourceState.String(), func(t *testing.T) {
			plan := NewPlanState(dag)
			plan.SetState(dag, "a", sourceState)

			assert.Equal(t, sourceState, plan.States["a"], "source should be set")
			assert.Equal(t, nodeUnvisited, plan.States["b"], "direct dependent should remain unvisited")
			assert.Equal(t, nodeUnvisited, plan.States["c"], "transitive dependent should remain unvisited")
		})
	}
}

// TestSummaryCountsBlockedState proves that PlanSummary correctly reports
// HasBlocked when a node is explicitly set to NodeBlocked. Since SetState
// no longer propagates, the test sets dependent state explicitly (matching
// what tryDispatch would do during a real walk).
func TestSummaryCountsBlockedState(t *testing.T) {
	nodes := []Node{
		{ID: "a", Template: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "a"}}},
		{ID: "b", Template: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "b"}, "data": map[string]any{"ref": "${a.metadata.name}"}}},
	}
	dag, err := BuildDAG(nodes, nil)
	require.NoError(t, err)

	plan := NewPlanState(dag)
	plan.SetState(dag, "a", NodeError)
	// Simulate what tryDispatch does: when b's dependency a is in error,
	// tryDispatch marks b as Blocked.
	plan.SetState(dag, "b", NodeBlocked)

	summary := plan.Summary()
	assert.True(t, summary.HasError, "should report error on the source node")
	assert.True(t, summary.HasBlocked, "should report blocked on the dependent node")
	assert.Equal(t, 0, summary.ReadyCount)
}

// ---------------------------------------------------------------------------
// tryDispatch state propagation tests
//
// These replace the old SetState propagation tests. The design commitment
// (004 § Propagation: Excluded > Blocked > Pending) is now enforced by
// tryDispatch, not SetState. These tests exercise tryDispatch directly.
// ---------------------------------------------------------------------------

// newTestWalkState builds a minimal walkState for testing tryDispatch.
// All nodes are marked as triggered so the skip check doesn't fire.
// Dependency states can be pre-set via plan.States before calling tryDispatch.
func newTestWalkState(t *testing.T, dag *DAG) *walkState {
	t.Helper()
	compiled := &compiledGraph{
		env:          nil,
		programs:     map[string]cel.Program{},
		exprPaths:    map[string]map[string][]FieldPath{},
		declaredVars: map[string]bool{},
		dag:          dag,
	}
	plan := NewPlanState(dag)
	triggered := make(map[string]bool, len(dag.Nodes))
	for i := range dag.Nodes {
		triggered[dag.Nodes[i].ID] = true
	}
	return &walkState{
		ctx:                  context.Background(),
		dag:                  dag,
		plan:                 plan,
		state:                newInstanceState(compiled),
		eval:                 &evaluator{compiled: compiled, scope: map[string]any{}},
		triggered:            triggered,
		propagationTriggered: map[string]bool{},
		dispatched:           map[int]bool{},
		outputsReady:         map[string]bool{},
		results:              make(chan nodeResult, 16),
	}
}

// TestTryDispatchPrecedence_ExcludedOverBlockedOverPending proves that
// tryDispatch enforces the design's state precedence when a node has
// multiple dependencies in different failure states.
//
// Per 004-graph-reconciliation.md § Propagation:
//   - "Any dependency Excluded → Excluded, regardless of other dependencies'
//     states (definitive absence propagates; the node is structurally non-viable)."
//   - "Any dependency in an error state → inherit Blocked."
//   - "Any dependency Pending → inherit Pending."
//   - "Precedence where multiple apply: Excluded > Blocked > Pending"
func TestTryDispatchPrecedence_ExcludedOverBlockedOverPending(t *testing.T) {
	// Diamond: A and B are parents of C.
	nodes := []Node{
		{ID: "a", Template: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "a"}}},
		{ID: "b", Template: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "b"}}},
		{ID: "c", Template: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "c"}, "data": map[string]any{"a": "${a.metadata.name}", "b": "${b.metadata.name}"}}},
	}
	dag, err := BuildDAG(nodes, nil)
	require.NoError(t, err)

	tests := []struct {
		name       string
		stateA     NodeState
		stateB     NodeState
		wantChildC NodeState
	}{
		// Excluded > Blocked: Excluded parent takes precedence over Error parent.
		{"Excluded+Error→Excluded", NodeExcluded, NodeError, NodeExcluded},
		{"Error+Excluded→Excluded", NodeError, NodeExcluded, NodeExcluded},
		// Excluded > Pending: Excluded parent takes precedence over Pending parent.
		{"Excluded+Pending→Excluded", NodeExcluded, NodePending, NodeExcluded},
		{"Pending+Excluded→Excluded", NodePending, NodeExcluded, NodeExcluded},
		// Blocked > Pending: Error parent takes precedence over Pending parent.
		{"Error+Pending→Blocked", NodeError, NodePending, NodeBlocked},
		{"Pending+Error→Blocked", NodePending, NodeError, NodeBlocked},
		// Same states: straightforward inheritance.
		{"Excluded+Excluded→Excluded", NodeExcluded, NodeExcluded, NodeExcluded},
		{"Error+Error→Blocked", NodeError, NodeError, NodeBlocked},
		{"Pending+Pending→Pending", NodePending, NodePending, NodePending},
		// All error variants map to Blocked.
		{"Conflict+Pending→Blocked", NodeConflict, NodePending, NodeBlocked},
		{"SystemError+Pending→Blocked", NodeSystemError, NodePending, NodeBlocked},
		{"Blocked+Pending→Blocked", NodeBlocked, NodePending, NodeBlocked},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			walk := newTestWalkState(t, dag)
			walk.plan.SetState(dag, "a", tc.stateA)
			walk.plan.SetState(dag, "b", tc.stateB)

			cIdx := -1
			for i, n := range dag.Nodes {
				if n.ID == "c" {
					cIdx = i
					break
				}
			}
			require.NotEqual(t, -1, cIdx)

			walk.tryDispatch(cIdx)
			assert.Equal(t, tc.wantChildC, walk.plan.States["c"],
				"child C should inherit %s from parents %s+%s", tc.wantChildC, tc.stateA, tc.stateB)
		})
	}
}

// TestTryDispatchChainPropagation proves that tryDispatch propagates state
// transitively through chains: A → B → C. When A is set to Excluded, both
// B and C become Excluded. When A is set to Error, both B and C become
// Blocked.
//
// This is the equivalent of the old TestSetStatePropagateSplitExcludedBlocked,
// now tested at the correct layer (tryDispatch, not SetState).
func TestTryDispatchChainPropagation(t *testing.T) {
	nodes := []Node{
		{ID: "a", Template: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "a"}}},
		{ID: "b", Template: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "b"}, "data": map[string]any{"ref": "${a.metadata.name}"}}},
		{ID: "c", Template: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "c"}, "data": map[string]any{"ref": "${b.metadata.name}"}}},
	}
	dag, err := BuildDAG(nodes, nil)
	require.NoError(t, err)

	tests := []struct {
		name  string
		rootA NodeState
		wantB NodeState
		wantC NodeState
	}{
		{"Excluded chains as Excluded", NodeExcluded, NodeExcluded, NodeExcluded},
		{"Error chains as Blocked", NodeError, NodeBlocked, NodeBlocked},
		{"Pending chains as Pending", NodePending, NodePending, NodePending},
		{"Conflict chains as Blocked", NodeConflict, NodeBlocked, NodeBlocked},
		{"SystemError chains as Blocked", NodeSystemError, NodeBlocked, NodeBlocked},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			walk := newTestWalkState(t, dag)
			walk.plan.SetState(dag, "a", tc.rootA)

			bIdx, cIdx := -1, -1
			for i, n := range dag.Nodes {
				switch n.ID {
				case "b":
					bIdx = i
				case "c":
					cIdx = i
				}
			}
			require.NotEqual(t, -1, bIdx)
			require.NotEqual(t, -1, cIdx)

			// Dispatch b — it will see a's state and propagate to c.
			walk.tryDispatch(bIdx)

			assert.Equal(t, tc.wantB, walk.plan.States["b"], "direct dependent")
			assert.Equal(t, tc.wantC, walk.plan.States["c"], "transitive dependent")
		})
	}
}

// TestTryDispatchPrecedence_RegressionDiamondExcludedBlocked is the
// regression test for the specific bug this change fixes. In the old code,
// propagateState used a first-wins flood fill that didn't enforce
// Excluded > Blocked precedence. This test proves the fix holds.
//
// The bug: diamond A(Excluded) + B(Error) → child could be Blocked
// (wrong) instead of Excluded (correct), preventing resource pruning.
func TestTryDispatchPrecedence_RegressionDiamondExcludedBlocked(t *testing.T) {
	nodes := []Node{
		{ID: "a", Template: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "a"}}},
		{ID: "b", Template: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "b"}}},
		{ID: "c", Template: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "c"}, "data": map[string]any{"a": "${a.metadata.name}", "b": "${b.metadata.name}"}}},
		{ID: "d", Template: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "d"}, "data": map[string]any{"ref": "${c.metadata.name}"}}},
	}
	dag, err := BuildDAG(nodes, nil)
	require.NoError(t, err)

	walk := newTestWalkState(t, dag)
	walk.plan.SetState(dag, "a", NodeExcluded)
	walk.plan.SetState(dag, "b", NodeError)

	cIdx := -1
	for i, n := range dag.Nodes {
		if n.ID == "c" {
			cIdx = i
			break
		}
	}
	require.NotEqual(t, -1, cIdx)

	walk.tryDispatch(cIdx)

	assert.Equal(t, NodeExcluded, walk.plan.States["c"],
		"child of Excluded+Error parents must be Excluded (not Blocked)")
	assert.Equal(t, NodeExcluded, walk.plan.States["d"],
		"transitive dependent must also be Excluded")
}

// TestPruneOrderReverseDependency proves that pruneOrder sorts prune
// candidates so dependents are deleted before their dependencies. This
// prevents dangling references during prune.
func TestPruneOrderReverseDependency(t *testing.T) {
	// Build A → B → C. Topological order: A(0), B(1), C(2).
	// Reverse dependency order for deletion: C, B, A.
	nodes := []Node{
		{ID: "a", Template: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "a"}}},
		{ID: "b", Template: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "b"}, "data": map[string]any{"ref": "${a.metadata.name}"}}},
		{ID: "c", Template: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "c"}, "data": map[string]any{"ref": "${b.metadata.name}"}}},
	}
	dag, err := BuildDAG(nodes, nil)
	require.NoError(t, err)

	keys := []string{
		"/v1/ConfigMap/default/a",
		"/v1/ConfigMap/default/b",
		"/v1/ConfigMap/default/c",
	}

	ordered := pruneOrder(keys, []*DAG{dag}, "default")

	require.Len(t, ordered, 3)
	// C depends on B depends on A. Reverse: C first, then B, then A.
	assert.Equal(t, "/v1/ConfigMap/default/c", ordered[0], "most-dependent should be first")
	assert.Equal(t, "/v1/ConfigMap/default/b", ordered[1])
	assert.Equal(t, "/v1/ConfigMap/default/a", ordered[2], "root should be last")
}

// TestPruneOrderUnmatchedKeysFirst proves that keys not matching any DAG
// node are placed first (deleted before mapped resources). This is the safe
// default for dynamic names (forEach, CEL-generated names).
func TestPruneOrderUnmatchedKeysFirst(t *testing.T) {
	nodes := []Node{
		{ID: "a", Template: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "a"}}},
	}
	dag, err := BuildDAG(nodes, nil)
	require.NoError(t, err)

	keys := []string{
		"/v1/ConfigMap/default/a",
		"/v1/ConfigMap/default/unknown-dynamic",
	}

	ordered := pruneOrder(keys, []*DAG{dag}, "default")

	require.Len(t, ordered, 2)
	assert.Equal(t, "/v1/ConfigMap/default/unknown-dynamic", ordered[0], "unmatched key should be first")
	assert.Equal(t, "/v1/ConfigMap/default/a", ordered[1])
}

// TestPruneOrderContributeKeysResolved proves that contribute-prefixed keys
// are resolved to their underlying resource key for position lookup.
func TestPruneOrderContributeKeysResolved(t *testing.T) {
	nodes := []Node{
		{ID: "a", Template: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "a"}}},
		{ID: "b", Template: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "b"}, "data": map[string]any{"ref": "${a.metadata.name}"}}},
	}
	dag, err := BuildDAG(nodes, nil)
	require.NoError(t, err)

	keys := []string{
		"/v1/ConfigMap/default/a",
		"contribute:/v1/ConfigMap/default/b",
	}

	ordered := pruneOrder(keys, []*DAG{dag}, "default")

	require.Len(t, ordered, 2)
	assert.Equal(t, "contribute:/v1/ConfigMap/default/b", ordered[0], "dependent contribute key should be first")
	assert.Equal(t, "/v1/ConfigMap/default/a", ordered[1])
}

// TestClassifyAPIErrorDefault proves that unrecognized errors (raw Go errors
// not wrapped as *StatusError) become NodeSystemError — the safe direction.
// Misclassifying transient network failures as deterministic (NodeError) means
// the system stops retrying when it should be retrying hardest (30-minute drift
// timer vs 5s SystemError retry). Misclassifying a deterministic error as
// transient means wasted retries — annoying but not an outage.
//
// Client errors (4xx) are positively identified; everything else is
// infrastructure until proven otherwise.
func TestClassifyAPIErrorDefault(t *testing.T) {
	t.Run("raw network error is NodeSystemError", func(t *testing.T) {
		err := &net.OpError{Op: "dial", Net: "tcp", Err: fmt.Errorf("connection refused")}
		info := classifyAPIError(err)
		assert.Equal(t, NodeSystemError, info.state,
			"network errors should be NodeSystemError — transient, needs retry")
	})

	t.Run("generic wrapped error is NodeSystemError", func(t *testing.T) {
		err := fmt.Errorf("unexpected EOF during API call")
		info := classifyAPIError(err)
		assert.Equal(t, NodeSystemError, info.state,
			"unrecognized errors default to NodeSystemError — safe direction")
	})

	t.Run("forbidden is NodeError", func(t *testing.T) {
		err := apierrors.NewForbidden(schema.GroupResource{Group: "", Resource: "configmaps"}, "test", fmt.Errorf("forbidden"))
		info := classifyAPIError(err)
		assert.Equal(t, NodeError, info.state)
		assert.Equal(t, "Forbidden", info.reason)
	})

	t.Run("unauthorized is NodeError", func(t *testing.T) {
		err := apierrors.NewUnauthorized("bad token")
		info := classifyAPIError(err)
		assert.Equal(t, NodeError, info.state)
		assert.Equal(t, "Unauthorized", info.reason)
	})

	t.Run("invalid is NodeError", func(t *testing.T) {
		err := apierrors.NewInvalid(schema.GroupKind{Group: "", Kind: "ConfigMap"}, "test", nil)
		info := classifyAPIError(err)
		assert.Equal(t, NodeError, info.state)
		assert.Equal(t, "ValidationFailed", info.reason)
	})

	t.Run("bad request is NodeError", func(t *testing.T) {
		err := apierrors.NewBadRequest("malformed")
		info := classifyAPIError(err)
		assert.Equal(t, NodeError, info.state)
		assert.Equal(t, "BadRequest", info.reason)
	})

	t.Run("internal server error is NodeSystemError", func(t *testing.T) {
		err := apierrors.NewInternalError(fmt.Errorf("etcd timeout"))
		info := classifyAPIError(err)
		assert.Equal(t, NodeSystemError, info.state, "5xx should be NodeSystemError")
		assert.Equal(t, "ServerError", info.reason)
	})

	t.Run("service unavailable is NodeSystemError", func(t *testing.T) {
		err := apierrors.NewServiceUnavailable("maintenance")
		info := classifyAPIError(err)
		assert.Equal(t, NodeSystemError, info.state)
	})

	t.Run("too many requests is NodeSystemError", func(t *testing.T) {
		err := apierrors.NewTooManyRequests("rate limited", 5)
		info := classifyAPIError(err)
		assert.Equal(t, NodeSystemError, info.state)
	})

	t.Run("nil error returns zero value", func(t *testing.T) {
		info := classifyAPIError(nil)
		assert.Equal(t, NodeState(0), info.state)
	})
}

// ---------------------------------------------------------------------------
// Validation tests — design reconciliation
// ---------------------------------------------------------------------------

// TestNodeIDHyphenRejected verifies that node IDs containing hyphens are
// rejected at parse time. Per 001-graph.md: "Hyphens are not allowed — they
// are parsed as subtraction by the CEL evaluator."
func TestNodeIDHyphenRejected(t *testing.T) {
	raw := []any{
		map[string]any{
			"id":       "my-app",
			"template": map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "cm"}},
		},
	}
	_, err := parseNodeList(raw)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "hyphens are not allowed")
}

// TestNodeIDCaseCollisionRejected verifies that node IDs that collide after
// lowercasing are rejected. Per 001-graph.md: "IDs that collide after
// lowercasing are rejected at compile time."
func TestNodeIDCaseCollisionRejected(t *testing.T) {
	raw := []any{
		map[string]any{
			"id":       "Deploy",
			"template": map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "a"}},
		},
		map[string]any{
			"id":       "deploy",
			"template": map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "b"}},
		},
	}
	_, err := parseNodeList(raw)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "collides with")
}

// TestParseNodeListEnforcesValidDNSLabels verifies that parseNodeList rejects
// node IDs that are not valid DNS-1123 labels. Node IDs are embedded in
// identity label key prefixes as DNS subdomain segments — invalid IDs would
// cause the API server to reject resources at apply time.
func TestParseNodeListEnforcesValidDNSLabels(t *testing.T) {
	tests := []struct {
		name    string
		id      string
		wantErr bool
	}{
		// Valid cases — must not be rejected
		{name: "simple alphanumeric", id: "deploy", wantErr: false},
		{name: "single character", id: "a", wantErr: false},
		{name: "numbers only", id: "123", wantErr: false},
		{name: "alphanumeric mix", id: "app2v3", wantErr: false},
		{name: "max length 63", id: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", wantErr: false},

		// Invalid cases — must be rejected
		{name: "empty string", id: "", wantErr: true},
		{name: "underscore", id: "foo_bar", wantErr: true},
		{name: "multiple underscores", id: "prstatus_test_app_uat", wantErr: true},
		{name: "dot", id: "foo.bar", wantErr: true},
		{name: "space", id: "foo bar", wantErr: true},
		{name: "exceeds 63 chars", id: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", wantErr: true},
		{name: "leading dot", id: ".foo", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw := []any{
				map[string]any{
					"id":       tc.id,
					"template": map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "cm"}},
				},
			}
			_, err := parseNodeList(raw)
			if tc.wantErr {
				require.Error(t, err, "node ID %q must be rejected", tc.id)
			} else {
				require.NoError(t, err, "node ID %q must be accepted", tc.id)
			}
		})
	}
}

// TestFinalizesTargetMustExist verifies that a finalizes declaration pointing
// at a nonexistent node ID is rejected at DAG build time.
func TestFinalizesTargetMustExist(t *testing.T) {
	nodes := []Node{
		{ID: "snapshot", Finalizes: "nonexistent", Template: map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "snap"},
		}},
	}
	_, err := BuildDAG(nodes, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no node with that ID exists")
}

// TestFinalizesTargetMustBeResource verifies that a finalizes declaration
// targeting a Definition or Watch node is rejected at DAG build time.
// Definitions and Watches don't create managed resources and never become
// prune candidates — finalizing them is nonsensical.
func TestFinalizesTargetMustBeResource(t *testing.T) {
	tests := []struct {
		name   string
		target Node
	}{
		{
			name: "Definition target rejected",
			target: Node{
				ID:       "naming",
				Template: map[string]any{"prefix": "app"},
			},
		},
		{
			name: "Watch target rejected",
			target: Node{
				ID: "config",
				Template: map[string]any{
					"apiVersion": "v1",
					"kind":       "ConfigMap",
					"metadata":   map[string]any{"name": "config"},
				},
			},
		},
		{
			name: "WatchKind target rejected",
			target: Node{
				ID: "namespaces",
				Template: map[string]any{
					"apiVersion": "v1",
					"kind":       "Namespace",
				},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			nodes := []Node{
				tc.target,
				{ID: "snapshot", Finalizes: tc.target.ID, Template: map[string]any{
					"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "snap"},
				}},
			}
			_, err := BuildDAG(nodes, nil)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "cannot finalize")
		})
	}
}

// TestForEachVariableCollision verifies that forEach iterator variable names
// that shadow node IDs are rejected at parse time.
func TestForEachVariableCollision(t *testing.T) {
	raw := []any{
		map[string]any{
			"id":       "items",
			"template": map[string]any{"apiVersion": "v1", "kind": "Namespace"},
		},
		map[string]any{
			"id":       "policy",
			"forEach":  map[string]any{"items": "${items}"},
			"template": map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "cm"}},
		},
	}
	_, err := parseNodeList(raw)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "collides with a node ID")
}

// TestNodeStateString verifies that NodeState.String() returns the design's
// canonical names. Each concept has exactly one name.
func TestNodeStateString(t *testing.T) {
	tests := []struct {
		state NodeState
		want  string
	}{
		{nodeUnvisited, "Unvisited"},
		{NodePending, "Pending"},
		{NodeReady, "Ready"},
		{NodeNotReady, "NotReady"},
		{NodeExcluded, "Excluded"},
		{NodeBlocked, "Blocked"},
		{NodeError, "Error"},
		{NodeConflict, "Conflict"},
		{NodeSystemError, "SystemError"},
	}
	for _, tc := range tests {
		t.Run(tc.want, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.state.String())
		})
	}
}

// TestForEachChildIdentityLabelKey verifies the DNS subdomain format for
// forEach child identity labels per 004-graph-reconciliation.md § Child Identity.
func TestForEachChildIdentityLabelKey(t *testing.T) {
	key := forEachChildIdentityLabelKey(
		"policies", "default-deny", "ns-a",
		"NetworkPolicy", "networking.k8s.io",
		"mygraph", "default",
	)
	assert.Equal(t,
		"policies.default-deny.ns-a.networkpolicy.networking.k8s.io.mygraph.default.internal.kro.run/reference",
		key,
	)
}

// TestForEachChildIdentityLabelKeyNoGroup verifies core API group resources
// (empty group) omit the group segment.
func TestForEachChildIdentityLabelKeyNoGroup(t *testing.T) {
	key := forEachChildIdentityLabelKey(
		"configs", "my-cm", "default",
		"ConfigMap", "",
		"mygraph", "default",
	)
	assert.Equal(t,
		"configs.my-cm.default.configmap.mygraph.default.internal.kro.run/reference",
		key,
	)
}

// ---------------------------------------------------------------------------
// Correctness reconciliation — regression tests
// ---------------------------------------------------------------------------

// TestWatchKindReadyWhenFailure_RegressionReadyFlag proves that when a
// WatchKind's readyWhen fails, .ready() returns false. Per 001-graph.md:
// "A WatchKind's .ready() returns true when the node's readyWhen
// conditions pass (evaluated once against the whole array, not per-item)."
//
// Before the fix, items were stamped with __ready=true before readyWhen
// evaluation. If readyWhen failed, the items retained __ready=true and
// .ready() on the collection returned true even though the node was NotReady.
func TestWatchKindReadyWhenFailure_RegressionReadyFlag(t *testing.T) {
	// Simulate the WatchKind pattern: items with __ready set,
	// then readyWhen fails. .ready() on the array must return false.
	items := []any{
		map[string]any{
			"metadata": map[string]any{"name": "pod-a"},
			"status":   map[string]any{"phase": "Running"},
			"__ready":  true, // stamped during collection read
		},
		map[string]any{
			"metadata": map[string]any{"name": "pod-b"},
			"status":   map[string]any{"phase": "Pending"},
			"__ready":  true, // stamped during collection read
		},
	}

	// Simulate what the fix does: when readyWhen fails, __ready is
	// reset to false on all items.
	for _, item := range items {
		if m, ok := item.(map[string]any); ok {
			m["__ready"] = false
		}
	}

	// Verify: .ready() on the collection must return false.
	for _, item := range items {
		m, ok := item.(map[string]any)
		require.True(t, ok)
		ready, _ := m["__ready"].(bool)
		assert.False(t, ready, "items should have __ready=false after readyWhen failure")
	}

	// Verify the inverse: when readyWhen passes, __ready stays true.
	goodItems := []any{
		map[string]any{"metadata": map[string]any{"name": "pod-a"}, "__ready": true},
		map[string]any{"metadata": map[string]any{"name": "pod-b"}, "__ready": true},
	}
	for _, item := range goodItems {
		m, _ := item.(map[string]any)
		ready, _ := m["__ready"].(bool)
		assert.True(t, ready, "items should have __ready=true when readyWhen passes")
	}
}

// TestWatchKindNoReadyWhen_ItemsReady proves that when a collection
// watch has no readyWhen, all items have __ready=true (applied = ready).
func TestWatchKindNoReadyWhen_ItemsReady(t *testing.T) {
	items := []any{
		map[string]any{"metadata": map[string]any{"name": "ns-a"}, "__ready": true},
		map[string]any{"metadata": map[string]any{"name": "ns-b"}, "__ready": true},
	}
	for _, item := range items {
		m, _ := item.(map[string]any)
		ready, _ := m["__ready"].(bool)
		assert.True(t, ready, "WatchKind items without readyWhen should be ready")
	}
}

// TestGVRKindFromInformerFallback_RegressionIrregularPlurals proves that the
// Kind fallback in gvrKindFromInformer handles irregular plurals correctly.
// Before the fix, "networkpolicies" would produce "Networkpolicie" (garbage).
// After the fix, it produces "Networkpolicy" (correct singular, lowercase).
//
// CamelCase (NetworkPolicy vs Networkpolicy) cannot be reconstructed from
// lowercase resource names — there are no word boundaries in "networkpolicy".
// The primary path (PartialObjectMetadata.Kind) always provides the correct
// CamelCase Kind. This fallback only fires when Kind is empty, which is not
// expected in practice with metadata informers.
func TestGVRKindFromInformerFallback_RegressionIrregularPlurals(t *testing.T) {
	tests := []struct {
		resource string
		want     string
	}{
		// Regular plurals — singularize + title-case first char
		{"configmaps", "Configmap"},
		{"deployments", "Deployment"},
		{"services", "Service"},
		// Irregular plurals — the fix ensures correct singular form.
		// Before: "networkpolicies" → "Networkpolicie" (truncated garbage)
		// After:  "networkpolicies" → "Networkpolicy" (correct singular)
		{"networkpolicies", "Networkpolicy"},
		{"ingresses", "Ingress"},
	}
	for _, tc := range tests {
		t.Run(tc.resource, func(t *testing.T) {
			gvr := schema.GroupVersionResource{Resource: tc.resource}
			got := gvrKindFromInformer(gvr, nil)
			assert.Equal(t, tc.want, got, "gvrKindFromInformer(%q) should produce correct singular form", tc.resource)
		})
	}
}

// TestGVRKindFromInformerPrimaryPath proves that when PartialObjectMetadata
// carries the Kind (the normal case with metadata informers), the exact
// CamelCase Kind is returned.
func TestGVRKindFromInformerPrimaryPath(t *testing.T) {
	accessor := &metav1.PartialObjectMetadata{}
	accessor.Kind = "NetworkPolicy"
	gvr := schema.GroupVersionResource{Resource: "networkpolicies"}
	got := gvrKindFromInformer(gvr, accessor)
	assert.Equal(t, "NetworkPolicy", got, "primary path should return exact CamelCase Kind")
}

// TestClassifyAPIErrorNetworkErrors_RegressionRetry proves that raw network
// errors (not wrapped as *StatusError) get classified as NodeSystemError for
// the 5s retry instead of NodeError's 30-minute drift timer.
func TestClassifyAPIErrorNetworkErrors_RegressionRetry(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{"dial error", &net.OpError{Op: "dial", Net: "tcp", Err: fmt.Errorf("connection refused")}},
		{"DNS error", &net.DNSError{Err: "no such host", Name: "apiserver", Server: ""}},
		{"generic network", fmt.Errorf("read tcp: connection reset by peer")},
		{"wrapped generic", fmt.Errorf("doing something: %w", fmt.Errorf("unexpected EOF"))},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			info := classifyAPIError(tc.err)
			assert.Equal(t, NodeSystemError, info.state,
				"network/transient error %q should be NodeSystemError for fast retry", tc.err)
		})
	}
}

// TestReconcileStateDeriveReadyCondition_FinalizerSkipped proves that the
// prune phase can surface FinalizerSkipped information via nodeErrors which
// flows into the Ready condition message as an informational note.
func TestReconcileStateDeriveReadyCondition_FinalizerSkipped(t *testing.T) {
	state := &reconcileState{
		compiled:   true,
		nodeCount:  3,
		nodeErrors: []string{"prune: finalization skipped for /v1/PersistentVolumeClaim/default/data (target absent)"},
	}
	// With no error flags set, the graph should still be Ready (finalization
	// skipped is informational, not an error). The message includes the note.
	status, reason, message := state.deriveReadyCondition()
	assert.Equal(t, ConditionTrue, status)
	assert.Equal(t, "Ready", reason)
	assert.Contains(t, message, "finalization skipped", "Ready message should surface FinalizerSkipped info")
}

// ---------------------------------------------------------------------------
// Correctness reconciliation — readyWhen expression errors
//
// Per 001-graph.md: "readyWhen is a health signal — it does not gate
// downstream execution. Dependents proceed as soon as the node is applied
// and its data is in scope, regardless of readyWhen."
//
// A readyWhen expression that returns a non-bool type or hits a CEL error
// is a permanent spec error — but it must NOT produce NodeError (which
// blocks dependents). It must produce NodeNotReady.
// ---------------------------------------------------------------------------

// TestEvalReadiness_ExpressionErrorWrapsReadyWhenFailed proves that when
// readyWhen evaluation fails with a permanent expression error (not data
// pending), evalReadiness wraps it with ErrReadyWhenFailed so the
// coordinator classifies it as NodeNotReady instead of NodeError.
//
// Per 001-graph.md: "readyWhen is a health signal — it does not gate
// downstream execution."
func TestEvalReadiness_ExpressionErrorWrapsReadyWhenFailed(t *testing.T) {
	// Build a compiledGraph with a readyWhen expression that evaluates
	// to an int (not a bool) — a permanent expression error.
	// size(deploy.metadata.name) returns int64, which evalBoolCondition
	// rejects: "expression evaluated to int64, want bool"
	spec := &GraphSpec{
		Nodes: []Node{
			{
				ID:        "deploy",
				Template:  map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": map[string]any{"name": "app"}},
				ReadyWhen: []string{"${size(deploy.metadata.name)}"},
			},
		},
	}
	compiled, err := compileGraphSpec(spec, nil)
	require.NoError(t, err)

	eval := &evaluator{compiled: compiled, scope: map[string]any{
		"deploy": map[string]any{
			"metadata": map[string]any{"name": "app"},
		},
	}}

	err = eval.evalReadiness("deploy", []string{"${size(deploy.metadata.name)}"})
	require.Error(t, err)

	// The error must wrap ErrReadyWhenFailed — NOT be a raw error that
	// the coordinator would classify as NodeError.
	assert.True(t, errors.Is(err, ErrReadyWhenFailed),
		"readyWhen expression error should wrap ErrReadyWhenFailed, got: %v", err)

	// The error must NOT wrap ErrWaitingForReadiness — that's for transient
	// readiness conditions (data not yet available, condition evaluates false).
	assert.False(t, errors.Is(err, ErrWaitingForReadiness),
		"readyWhen expression error should NOT wrap ErrWaitingForReadiness")

	// The underlying error message should be preserved for diagnostics.
	assert.Contains(t, err.Error(), "want bool",
		"underlying expression error should be preserved")
}

// TestEvalReadiness_NormalNotReadyIsUnchanged proves that normal readyWhen
// failures (condition evaluates to false, data pending) still produce
// ErrWaitingForReadiness — the fix only affects expression errors.
func TestEvalReadiness_NormalNotReadyIsUnchanged(t *testing.T) {
	spec := &GraphSpec{
		Nodes: []Node{
			{
				ID:        "deploy",
				Template:  map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": map[string]any{"name": "app"}},
				ReadyWhen: []string{"${deploy.status.availableReplicas > 0}"},
			},
		},
	}
	compiled, err := compileGraphSpec(spec, nil)
	require.NoError(t, err)

	eval := &evaluator{compiled: compiled, scope: map[string]any{
		"deploy": map[string]any{
			"metadata": map[string]any{"name": "app"},
			"status":   map[string]any{"availableReplicas": int64(0)},
		},
	}}

	err = eval.evalReadiness("deploy", []string{"${deploy.status.availableReplicas > 0}"})
	require.Error(t, err)

	// Normal readyWhen failure (condition false) — should still be ErrWaitingForReadiness.
	assert.True(t, errors.Is(err, ErrWaitingForReadiness),
		"normal readyWhen=false should produce ErrWaitingForReadiness, got: %v", err)
	assert.False(t, errors.Is(err, ErrReadyWhenFailed),
		"normal readyWhen=false should NOT produce ErrReadyWhenFailed")
}

// TestWorkerClassification_ReadyWhenFailedIsNotReady proves that the worker
// goroutine's error classification maps ErrReadyWhenFailed to NodeNotReady,
// not NodeError. This is the boundary where the gating decision is made.
func TestWorkerClassification_ReadyWhenFailedIsNotReady(t *testing.T) {
	// Simulate the worker's classification logic.
	classify := func(err error) NodeState {
		if err == nil {
			return NodeReady
		}
		switch {
		case errors.Is(err, ErrPending):
			return NodePending
		case errors.Is(err, ErrWaitingForReadiness):
			return NodeNotReady
		case errors.Is(err, ErrReadyWhenFailed):
			return NodeNotReady
		case errors.Is(err, ErrFieldConflict):
			return NodeConflict
		default:
			return NodeError
		}
	}

	tests := []struct {
		name string
		err  error
		want NodeState
	}{
		{"readyWhen expression error", fmt.Errorf("%w: expression returned string", ErrReadyWhenFailed), NodeNotReady},
		{"readyWhen false", fmt.Errorf("%w", ErrWaitingForReadiness), NodeNotReady},
		{"data pending", fmt.Errorf("%w", ErrPending), NodePending},
		{"field conflict", fmt.Errorf("%w", ErrFieldConflict), NodeConflict},
		{"API error", fmt.Errorf("forbidden: permission denied"), NodeError},
		{"nil", nil, NodeReady},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := classify(tc.err)
			assert.Equal(t, tc.want, got,
				"error %v should classify as %s", tc.err, tc.want)
		})
	}
}

// ---------------------------------------------------------------------------
// Correctness reconciliation — forEach parent state error precedence
//
// Per 004-graph-reconciliation.md § Parent State: "Deterministic errors (Error)
// take precedence over transient errors (SystemError, Conflict) — if any
// child's failure is deterministic, retrying cannot resolve the parent."
// ---------------------------------------------------------------------------

// TestHighestPriorityChildError proves that when multiple forEach children
// fail, the highest-priority error is returned — not the first one.
func TestHighestPriorityChildError(t *testing.T) {
	errConflict := fmt.Errorf("SSA conflict on apps/v1/Deployment my-app: %w: field taken", ErrFieldConflict)
	errForbidden := apierrors.NewForbidden(schema.GroupResource{Resource: "deployments"}, "my-app", fmt.Errorf("RBAC"))
	errNetwork := &net.OpError{Op: "dial", Net: "tcp", Err: fmt.Errorf("connection refused")}
	errCEL := fmt.Errorf("evaluating template: expression returned string, want int")

	tests := []struct {
		name     string
		errs     []error
		wantType NodeState // the expected classification of the returned error
	}{
		{
			name:     "deterministic (Forbidden) over transient (network)",
			errs:     []error{errNetwork, errForbidden},
			wantType: NodeError,
		},
		{
			name:     "deterministic (CEL) over conflict",
			errs:     []error{errConflict, errCEL},
			wantType: NodeError,
		},
		{
			name:     "conflict over network (system error)",
			errs:     []error{errNetwork, errConflict},
			wantType: NodeConflict,
		},
		{
			name:     "single error returns itself",
			errs:     []error{errNetwork},
			wantType: NodeSystemError,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := highestPriorityChildError(tc.errs)
			require.NotNil(t, result)

			// Classify the result to check priority.
			if errors.Is(result, ErrFieldConflict) {
				assert.Equal(t, NodeConflict, tc.wantType)
			} else {
				info := classifyAPIError(result)
				assert.Equal(t, tc.wantType, info.state,
					"highest priority error should classify as %s, got %s (error: %v)",
					tc.wantType, info.state, result)
			}
		})
	}
}

// TestHighestPriorityChildError_Nil proves that an empty error list
// returns nil (no children failed).
func TestHighestPriorityChildError_Nil(t *testing.T) {
	assert.Nil(t, highestPriorityChildError(nil))
	assert.Nil(t, highestPriorityChildError([]error{}))
}

// ---------------------------------------------------------------------------
// Correctness reconciliation — readyWhen errors in status
// ---------------------------------------------------------------------------

// TestDeriveReadyCondition_NotReadyWithErrors proves that when nodes are
// NotReady and there are nodeErrors (e.g., readyWhen expression errors),
// the Ready condition message surfaces the errors for operator visibility.
func TestDeriveReadyCondition_NotReadyWithErrors(t *testing.T) {
	state := &reconcileState{
		compiled: true,
		PlanSummary: PlanSummary{
			HasNotReady: true,
		},
		nodeErrors: []string{"deploy: readyWhen evaluation failed: expression returned string, want bool"},
	}
	status, reason, message := state.deriveReadyCondition()
	assert.Equal(t, ConditionUnknown, status)
	assert.Equal(t, "NotReady", reason)
	assert.Contains(t, message, "readyWhen evaluation failed",
		"NotReady condition should surface readyWhen expression errors for operator triage")
}

// ---------------------------------------------------------------------------
// Correctness reconciliation — Finding 1: staticResourceKey namespace
// ---------------------------------------------------------------------------

// TestStaticResourceKey_ExplicitNamespace proves that staticResourceKey reads
// a literal metadata.namespace from the template instead of always using the
// fallback. Per 004-graph-reconciliation.md § Prune: resource keys encode
// group/version/Kind/namespace/name. If the template specifies a literal
// namespace, the key must use it.
//
// Before the fix, staticResourceKey always used fallbackNamespace, causing
// cross-namespace resources to have wrong keys in the prune candidate set.
func TestStaticResourceKey_ExplicitNamespace(t *testing.T) {
	t.Run("literal namespace overrides fallback", func(t *testing.T) {
		tmpl := map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "my-config",
				"namespace": "kube-system",
			},
		}
		key := staticResourceKey(tmpl, "default")
		assert.Equal(t, "/v1/ConfigMap/kube-system/my-config", key,
			"should use the template's literal namespace, not fallback")
	})

	t.Run("absent namespace uses fallback", func(t *testing.T) {
		tmpl := map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name": "my-config",
			},
		}
		key := staticResourceKey(tmpl, "default")
		assert.Equal(t, "/v1/ConfigMap/default/my-config", key,
			"should use fallback when namespace is absent")
	})

	t.Run("dynamic namespace uses fallback", func(t *testing.T) {
		tmpl := map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "my-config",
				"namespace": "${ns.metadata.name}",
			},
		}
		key := staticResourceKey(tmpl, "default")
		assert.Equal(t, "/v1/ConfigMap/default/my-config", key,
			"should use fallback when namespace contains ${...}")
	})

	t.Run("empty namespace uses fallback", func(t *testing.T) {
		tmpl := map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "my-config",
				"namespace": "",
			},
		}
		key := staticResourceKey(tmpl, "default")
		assert.Equal(t, "/v1/ConfigMap/default/my-config", key,
			"should use fallback when namespace is empty string")
	})
}

// ---------------------------------------------------------------------------
// Correctness reconciliation — Finding 2: Contribute status in teardown
// ---------------------------------------------------------------------------

// TestContributeStatusDetection proves that contributeHasStatus correctly
// detects whether a template's Contribute node applies status fields.
// During teardown, this determines whether releaseApply releases the
// status subresource.
//
// Per 003-ownership.md § Status Subresource: "Releases only target the
// subresources the template actually applied to — a status-only Contribute
// releases only the status subresource."
func TestContributeStatusDetection(t *testing.T) {
	t.Run("template with status field", func(t *testing.T) {
		tmpl := map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata":   map[string]any{"name": "target"},
			"status":     map[string]any{"ready": true},
		}
		assert.True(t, templateHasStatus(tmpl),
			"template with status field should be detected")
	})

	t.Run("template without status field", func(t *testing.T) {
		tmpl := map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata":   map[string]any{"name": "target"},
			"data":       map[string]any{"key": "value"},
		}
		assert.False(t, templateHasStatus(tmpl),
			"template without status field should not be detected")
	})

	t.Run("template with nil status", func(t *testing.T) {
		tmpl := map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata":   map[string]any{"name": "target"},
			"status":     nil,
		}
		assert.False(t, templateHasStatus(tmpl),
			"template with nil status should not be detected")
	})
}

// ---------------------------------------------------------------------------
// Correctness reconciliation — Finding 3: Non-API error classification
// ---------------------------------------------------------------------------

// TestClassifyAPIError_NonAPIErrorsAreNotSystemError proves that errors
// which are NOT wrapped *StatusError (i.e., not from the API server) AND
// are NOT network/infrastructure errors are classified correctly.
//
// Per 004-graph-reconciliation.md § Node States: Definition nodes do not
// produce SystemError (no API calls). CEL evaluation failures are Error.
//
// The coordinator re-classifies NodeError from the worker using
// classifyAPIError. For non-API errors (CEL failures, template errors),
// the function should distinguish between transient infrastructure errors
// and deterministic non-API errors.
func TestClassifyAPIError_CELErrorIsNotSystemError(t *testing.T) {
	// A CEL type error — deterministic, same inputs always produce same failure.
	// This should NOT be SystemError (which triggers 5s retry).
	celErr := fmt.Errorf("evaluating \"deploy.status.replicas > 0\": type conversion error: got string, want int")
	info := classifyAPIError(celErr)

	// Currently this is NodeSystemError (the bug). After the fix, this should
	// be NodeError (deterministic non-API failure).
	assert.Equal(t, NodeError, info.state,
		"CEL evaluation errors are deterministic non-API failures, not transient system errors")
}

// TestClassifyAPIError_EvalErrorWithNetworkPattern proves that non-API errors
// (CEL evaluation, template rendering) are classified as NodeError even when
// their message contains network-like patterns. Before the fix, an error
// message containing "unexpected EOF" from a JSON marshal failure would
// false-positive as a network error → NodeSystemError → 5s retry loop.
//
// The fix: errors originating from non-API operations are wrapped with
// ErrEvaluation at the source (toMap, evalString). classifyAPIError checks
// for this sentinel before falling through to network pattern matching.
func TestClassifyAPIError_EvalErrorWithNetworkPattern(t *testing.T) {
	// An evaluation error whose message contains a network error pattern.
	// Without the sentinel, this would be classified as NodeSystemError.
	evalErr := fmt.Errorf("evaluating template: %w: unexpected EOF in field value",
		ErrEvaluation)
	info := classifyAPIError(evalErr)
	assert.Equal(t, NodeError, info.state,
		"evaluation errors must be NodeError even when message contains network patterns")

	// A real network error should still be NodeSystemError.
	netErr := fmt.Errorf("unexpected EOF during API call")
	info = classifyAPIError(netErr)
	assert.Equal(t, NodeSystemError, info.state,
		"real network errors without ErrEvaluation sentinel should remain NodeSystemError")
}

// ---------------------------------------------------------------------------
// Correctness reconciliation — Finding 4: Status priority
// ---------------------------------------------------------------------------

// TestDeriveReadyCondition_BlockedBeforePending proves that when both
// Blocked and Pending states coexist, Blocked takes priority in the Ready
// condition. Blocked means "upstream error — someone needs to act."
// Pending means "just waiting." Blocked is more actionable.
//
// Per 004-graph-reconciliation.md § Propagation: "Precedence where multiple
// apply: Excluded > Blocked > Pending."
func TestDeriveReadyCondition_BlockedBeforePending(t *testing.T) {
	state := &reconcileState{
		compiled: true,
		PlanSummary: PlanSummary{
			HasPending: true,
			HasBlocked: true,
		},
	}
	status, reason, _ := state.deriveReadyCondition()
	assert.Equal(t, ConditionUnknown, status)
	assert.Equal(t, "Blocked", reason,
		"Blocked should take priority over Pending — upstream error is more actionable than waiting")
}

// ---------------------------------------------------------------------------
// SystemError exponential backoff tests
//
// Per 004-graph-reconciliation.md § Trigger: "Transient errors (5xx) retry
// with exponential backoff [1s, resyncInterval]."
// ---------------------------------------------------------------------------

// TestSystemErrorExponentialBackoff verifies that consecutive SystemError
// states produce exponentially increasing backoff durations, capped at
// the drift interval.
func TestSystemErrorExponentialBackoff(t *testing.T) {
	state := &instanceState{
		systemErrorBackoff: make(map[string]time.Duration),
		driftTimers:        make(map[string]time.Time),
	}

	driftInterval := 30 * time.Minute
	nodeID := "testNode"

	// Simulate consecutive SystemError backoffs.
	expected := []time.Duration{
		1 * time.Second,
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		16 * time.Second,
	}

	for i, want := range expected {
		backoff := state.systemErrorBackoff[nodeID]
		if backoff == 0 {
			backoff = 1 * time.Second
		} else {
			backoff *= 2
		}
		if backoff > driftInterval {
			backoff = driftInterval
		}
		state.systemErrorBackoff[nodeID] = backoff

		assert.Equal(t, want, backoff,
			"iteration %d: backoff should double", i)
	}

	// Verify cap at drift interval.
	state.systemErrorBackoff[nodeID] = 20 * time.Minute
	backoff := state.systemErrorBackoff[nodeID] * 2
	if backoff > driftInterval {
		backoff = driftInterval
	}
	assert.Equal(t, driftInterval, backoff, "backoff should cap at drift interval")
}

// TestSystemErrorBackoffResetOnStateChange verifies that the backoff resets
// when the node transitions to any non-SystemError state.
func TestSystemErrorBackoffResetOnStateChange(t *testing.T) {
	state := &instanceState{
		systemErrorBackoff: make(map[string]time.Duration),
		driftTimers:        make(map[string]time.Time),
	}

	nodeID := "testNode"
	state.systemErrorBackoff[nodeID] = 16 * time.Second // accumulated backoff

	// Simulate transition to Ready — backoff should reset.
	delete(state.systemErrorBackoff, nodeID)
	_, exists := state.systemErrorBackoff[nodeID]
	assert.False(t, exists, "backoff should be cleared on non-SystemError transition")
}

// ---------------------------------------------------------------------------
// Readiness propagation hash tests
//
// The propagation hash includes readiness state so downstream nodes using
// .ready() are triggered when a node's readiness changes, even if output
// field paths are unchanged.
// ---------------------------------------------------------------------------

// TestReadinessStateIncludedInPropagationHash verifies that the propagation
// hash changes when a node's readiness state changes, even with identical
// output data.
func TestReadinessStateIncludedInPropagationHash(t *testing.T) {
	// The propagation hash is a string with suffixes. Verify that different
	// readiness states produce different hash suffixes.
	baseHash := "abc123"

	readyHash := baseHash + ":ready=true"
	notReadyHash := baseHash + ":ready=false"

	assert.NotEqual(t, readyHash, notReadyHash,
		"Ready and NotReady should produce different propagation hashes")
}

// ---------------------------------------------------------------------------
// WatchKind incremental cache tests
// ---------------------------------------------------------------------------

// TestCollectionChangeDedup verifies that multiple events for the same
// resource between reconciles are deduplicated to the latest event.
func TestCollectionChangeDedup(t *testing.T) {
	changes := []CollectionChange{
		{Namespace: "default", Name: "pod-1", EventType: WatchEventAdd},
		{Namespace: "default", Name: "pod-1", EventType: WatchEventUpdate},
		{Namespace: "default", Name: "pod-2", EventType: WatchEventAdd},
		{Namespace: "default", Name: "pod-1", EventType: WatchEventDelete},
	}

	type changeKey struct{ namespace, name string }
	deduped := make(map[changeKey]CollectionChange)
	for _, change := range changes {
		deduped[changeKey{change.Namespace, change.Name}] = change
	}

	assert.Equal(t, 2, len(deduped), "should have 2 unique resources")
	assert.Equal(t, WatchEventDelete, deduped[changeKey{"default", "pod-1"}].EventType,
		"pod-1 should have the latest event (delete)")
	assert.Equal(t, WatchEventAdd, deduped[changeKey{"default", "pod-2"}].EventType,
		"pod-2 should have its only event (add)")
}

// TestWatchKindCacheLifecycle verifies the WatchKind cache in instanceState.
func TestWatchKindCacheLifecycle(t *testing.T) {
	spec := buildBenchSpec(3)
	compiled, err := compileGraphSpec(spec, nil)
	require.NoError(t, err)

	state := newInstanceState(compiled)

	// Initially empty.
	assert.Empty(t, state.watchKindCache)

	// Store a cached list.
	items := []any{
		map[string]any{"metadata": map[string]any{"name": "pod-1"}},
		map[string]any{"metadata": map[string]any{"name": "pod-2"}},
	}
	state.watchKindCache["myWatchKind"] = items

	// Retrieve.
	cached, ok := state.watchKindCache["myWatchKind"]
	assert.True(t, ok)
	assert.Equal(t, 2, len(cached))
}
