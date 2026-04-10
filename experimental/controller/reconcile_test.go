package graphcontroller

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

// TestSetStatePropagateSplitExcludedBlocked proves that SetState propagates
// NodeExcluded for definitive absence (includeWhen=false) and NodeBlocked
// for uncertain absence (dependency errored). This is the structural split
// that makes prune safety possible — without it, the prune loop can't
// distinguish "definitely doesn't exist" from "might exist after recovery."
func TestSetStatePropagateSplitExcludedBlocked(t *testing.T) {
	// Build a chain: A → B → C
	nodes := []Node{
		{ID: "a", Template: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "a"}}},
		{ID: "b", Template: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "b"}, "data": map[string]any{"ref": "${a.metadata.name}"}}},
		{ID: "c", Template: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "c"}, "data": map[string]any{"ref": "${b.metadata.name}"}}},
	}
	dag, err := BuildDAG(nodes)
	require.NoError(t, err)

	t.Run("NodeExcluded propagates as NodeExcluded", func(t *testing.T) {
		plan := NewPlanState(dag)
		plan.SetState(dag, "a", NodeExcluded)

		assert.Equal(t, NodeExcluded, plan.States["a"], "source should be Excluded")
		assert.Equal(t, NodeExcluded, plan.States["b"], "direct dependent should be Excluded")
		assert.Equal(t, NodeExcluded, plan.States["c"], "transitive dependent should be Excluded")
	})

	t.Run("NodeError propagates as NodeBlocked", func(t *testing.T) {
		plan := NewPlanState(dag)
		plan.SetState(dag, "a", NodeError)

		assert.Equal(t, NodeError, plan.States["a"], "source should be Error")
		assert.Equal(t, NodeBlocked, plan.States["b"], "direct dependent should be Blocked")
		assert.Equal(t, NodeBlocked, plan.States["c"], "transitive dependent should be Blocked")
	})

	t.Run("NodeDataPending propagates as NodeBlocked", func(t *testing.T) {
		plan := NewPlanState(dag)
		plan.SetState(dag, "a", NodeDataPending)

		assert.Equal(t, NodeDataPending, plan.States["a"])
		assert.Equal(t, NodeBlocked, plan.States["b"])
		assert.Equal(t, NodeBlocked, plan.States["c"])
	})

	t.Run("NodeConflict propagates as NodeBlocked", func(t *testing.T) {
		plan := NewPlanState(dag)
		plan.SetState(dag, "a", NodeConflict)

		assert.Equal(t, NodeConflict, plan.States["a"])
		assert.Equal(t, NodeBlocked, plan.States["b"])
		assert.Equal(t, NodeBlocked, plan.States["c"])
	})

	t.Run("NodeSystemError propagates as NodeBlocked", func(t *testing.T) {
		plan := NewPlanState(dag)
		plan.SetState(dag, "a", NodeSystemError)

		assert.Equal(t, NodeSystemError, plan.States["a"])
		assert.Equal(t, NodeBlocked, plan.States["b"])
		assert.Equal(t, NodeBlocked, plan.States["c"])
	})

	t.Run("NodeReady does not propagate", func(t *testing.T) {
		plan := NewPlanState(dag)
		plan.SetState(dag, "a", NodeReady)

		assert.Equal(t, NodeReady, plan.States["a"])
		assert.Equal(t, NodePending, plan.States["b"], "Ready should not propagate")
		assert.Equal(t, NodePending, plan.States["c"])
	})

	t.Run("NodeNotReady does not propagate", func(t *testing.T) {
		plan := NewPlanState(dag)
		plan.SetState(dag, "a", NodeNotReady)

		assert.Equal(t, NodeNotReady, plan.States["a"])
		assert.Equal(t, NodePending, plan.States["b"], "NotReady should not propagate")
		assert.Equal(t, NodePending, plan.States["c"])
	})
}

// TestSummaryCountsBlockedState proves that PlanSummary correctly reports
// HasBlocked when any node is in NodeBlocked state. This feeds the allReady
// check and the status condition.
func TestSummaryCountsBlockedState(t *testing.T) {
	nodes := []Node{
		{ID: "a", Template: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "a"}}},
		{ID: "b", Template: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "b"}, "data": map[string]any{"ref": "${a.metadata.name}"}}},
	}
	dag, err := BuildDAG(nodes)
	require.NoError(t, err)

	plan := NewPlanState(dag)
	plan.SetState(dag, "a", NodeError)
	// b should now be NodeBlocked via propagation

	summary := plan.Summary()
	assert.True(t, summary.HasError, "should report error on the source node")
	assert.True(t, summary.HasBlocked, "should report blocked on the dependent node")
	assert.Equal(t, 0, summary.ReadyCount)
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
	dag, err := BuildDAG(nodes)
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
	dag, err := BuildDAG(nodes)
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
	dag, err := BuildDAG(nodes)
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

// TestClassifyAPIErrorDefaultIsNodeError proves that unrecognized errors
// become NodeError (user-fixable), not NodeSystemError. Template evaluation
// failures, CEL bugs, and marshaling errors are deterministic — they require
// user action, not infrastructure recovery.
func TestClassifyAPIErrorDefaultIsNodeError(t *testing.T) {
	t.Run("non-API error is NodeError", func(t *testing.T) {
		info := classifyAPIError(fmt.Errorf("template evaluation failed: cannot divide by zero"))
		assert.Equal(t, NodeError, info.state, "non-API error should be NodeError, not NodeSystemError")
	})

	t.Run("forbidden is NodeError", func(t *testing.T) {
		err := apierrors.NewForbidden(schema.GroupResource{Group: "", Resource: "configmaps"}, "test", fmt.Errorf("forbidden"))
		info := classifyAPIError(err)
		assert.Equal(t, NodeError, info.state)
		assert.Equal(t, "Forbidden", info.reason)
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
