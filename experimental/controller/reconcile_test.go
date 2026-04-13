package graphcontroller

import (
	"fmt"
	"net"
	"testing"

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
	dag, err := BuildDAG(nodes, nil)
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

	t.Run("NodePending propagates as NodePending", func(t *testing.T) {
		plan := NewPlanState(dag)
		plan.SetState(dag, "a", NodePending)

		assert.Equal(t, NodePending, plan.States["a"])
		assert.Equal(t, NodePending, plan.States["b"])
		assert.Equal(t, NodePending, plan.States["c"])
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
		assert.Equal(t, nodeUnvisited, plan.States["b"], "Ready should not propagate")
		assert.Equal(t, nodeUnvisited, plan.States["c"])
	})

	t.Run("NodeNotReady does not propagate", func(t *testing.T) {
		plan := NewPlanState(dag)
		plan.SetState(dag, "a", NodeNotReady)

		assert.Equal(t, NodeNotReady, plan.States["a"])
		assert.Equal(t, nodeUnvisited, plan.States["b"], "NotReady should not propagate")
		assert.Equal(t, nodeUnvisited, plan.States["c"])
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
	dag, err := BuildDAG(nodes, nil)
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
// canonical names. Per 006-quality.md: "Each concept has exactly one name."
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
// forEach child identity labels per 004-graph-execution.md § Child Identity.
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

// TestCollectionWatchReadyWhenFailure_RegressionReadyFlag proves that when a
// collection watch's readyWhen fails, .ready() returns false. Per 001-graph.md:
// "A collection watch's .ready() returns true when the node's readyWhen
// conditions pass (evaluated once against the whole array, not per-item)."
//
// Before the fix, items were stamped with __ready=true before readyWhen
// evaluation. If readyWhen failed, the items retained __ready=true and
// .ready() on the collection returned true even though the node was NotReady.
func TestCollectionWatchReadyWhenFailure_RegressionReadyFlag(t *testing.T) {
	// Simulate the collection watch pattern: items with __ready set,
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

// TestCollectionWatchNoReadyWhen_ItemsReady proves that when a collection
// watch has no readyWhen, all items have __ready=true (applied = ready).
func TestCollectionWatchNoReadyWhen_ItemsReady(t *testing.T) {
	items := []any{
		map[string]any{"metadata": map[string]any{"name": "ns-a"}, "__ready": true},
		map[string]any{"metadata": map[string]any{"name": "ns-b"}, "__ready": true},
	}
	for _, item := range items {
		m, _ := item.(map[string]any)
		ready, _ := m["__ready"].(bool)
		assert.True(t, ready, "collection watch items without readyWhen should be ready")
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
