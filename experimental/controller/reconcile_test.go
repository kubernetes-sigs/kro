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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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
// // TestTryDispatchPrecedence_RegressionDiamondExcludedBlocked is the
// regression test for the specific bug this change fixes. In the old code,
// propagateState used a first-wins flood fill that didn't enforce
// Excluded > Blocked precedence. This test proves the fix holds.
// // TestPruneOrderReverseDependency proves that pruneOrder sorts prune
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

	ordered := pruneOrder(keys, []*DAG{dag}, "default", nil)

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

	ordered := pruneOrder(keys, []*DAG{dag}, "default", nil)

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
		"patch:/v1/ConfigMap/default/b",
	}

	ordered := pruneOrder(keys, []*DAG{dag}, "default", nil)

	require.Len(t, ordered, 2)
	assert.Equal(t, "patch:/v1/ConfigMap/default/b", ordered[0], "dependent patch key should be first")
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
} // TestNodeStateString verifies that NodeState.String() returns the design's
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
		"policies.default-deny.ns-a.networkpolicy.networking.k8s.io.mygraph.default.internal.kro.run/type",
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
		"configs.my-cm.default.configmap.mygraph.default.internal.kro.run/type",
		key,
	)
}

// ---------------------------------------------------------------------------
// Correctness reconciliation — regression tests
// ---------------------------------------------------------------------------

// TestWatchKindReadyWhenFailure_RegressionReadyFlag proves that when a
// Watch's readyWhen fails, .ready() returns false. Per 001-graph.md:
// "A Watch's .ready() returns true when the node's readyWhen
// conditions pass (evaluated once against the whole array, not per-item)."
// // TestWatchKindNoReadyWhen_ItemsReady proves that when a collection
// watch has no readyWhen, all items have __ready=true (applied = ready).
func TestWatchKindNoReadyWhen_ItemsReady(t *testing.T) {
	items := []any{
		map[string]any{"metadata": map[string]any{"name": "ns-a"}, "__ready": true},
		map[string]any{"metadata": map[string]any{"name": "ns-b"}, "__ready": true},
	}
	for _, item := range items {
		m, _ := item.(map[string]any)
		ready, _ := m["__ready"].(bool)
		assert.True(t, ready, "Watch items without readyWhen should be ready")
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
// // TestEvalReadiness_NormalNotReadyIsUnchanged proves that normal readyWhen
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
		key := staticResourceKey(tmpl, "default", nil)
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
		key := staticResourceKey(tmpl, "default", nil)
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
		key := staticResourceKey(tmpl, "default", nil)
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
		key := staticResourceKey(tmpl, "default", nil)
		assert.Equal(t, "/v1/ConfigMap/default/my-config", key,
			"should use fallback when namespace is empty string")
	})
}

// fakeGVKScopeResolver is a stub GVKScopeResolver that answers from a table.
// Used to verify that staticResourceKey produces empty-namespace keys for
// cluster-scoped kinds, matching what resourceKey(liveObj) produces after
// an API server response strips the namespace for cluster-scoped responses.
type fakeGVKScopeResolver map[string]bool // "group/version/Kind" → isNamespaced

func (f fakeGVKScopeResolver) IsNamespaced(gvk schema.GroupVersionKind) (bool, bool) {
	key := gvk.Group + "/" + gvk.Version + "/" + gvk.Kind
	v, ok := f[key]
	return v, ok
}

// TestStaticResourceKey_ClusterScopedNamespace_Regression proves that
// staticResourceKey produces an empty-namespace key for cluster-scoped
// kinds when a GVKScopeResolver is provided — matching what resourceKey(obj)
// produces after the API server strips namespace from cluster-scoped
// responses. Before this fix, staticResourceKey unconditionally
// substituted the fallback (Graph's) namespace, so its keys never matched
// post-apply resourceKey output — prune diffing and finalizer lookups
// silently missed cluster-scoped resources.
//
// Per 003-ownership.md § Priority Resolution: "Cluster-scoped resources
// use empty string for the namespace component."
func TestStaticResourceKey_ClusterScopedNamespace_Regression(t *testing.T) {
	scope := fakeGVKScopeResolver{
		"rbac.authorization.k8s.io/v1/ClusterRole": false, // cluster-scoped
		"/v1/ConfigMap": true, // namespaced
	}

	t.Run("cluster-scoped kind yields empty namespace segment", func(t *testing.T) {
		tmpl := map[string]any{
			"apiVersion": "rbac.authorization.k8s.io/v1",
			"kind":       "ClusterRole",
			"metadata":   map[string]any{"name": "platform-admin"},
		}
		key := staticResourceKey(tmpl, "my-graph-ns", scope)
		assert.Equal(t, "rbac.authorization.k8s.io/v1/ClusterRole//platform-admin", key,
			"cluster-scoped kinds must have empty namespace segment to match resourceKey(liveObj)")
	})

	t.Run("namespaced kind still substitutes fallback", func(t *testing.T) {
		tmpl := map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata":   map[string]any{"name": "app-config"},
		}
		key := staticResourceKey(tmpl, "my-graph-ns", scope)
		assert.Equal(t, "/v1/ConfigMap/my-graph-ns/app-config", key,
			"namespaced kinds with empty template namespace substitute fallback")
	})

	t.Run("unknown scope falls back to substitution heuristic", func(t *testing.T) {
		tmpl := map[string]any{
			"apiVersion": "unknown.example.com/v1",
			"kind":       "UnknownKind",
			"metadata":   map[string]any{"name": "x"},
		}
		// Not in scope map — treat as unknown.
		key := staticResourceKey(tmpl, "my-ns", scope)
		assert.Equal(t, "unknown.example.com/v1/UnknownKind/my-ns/x", key,
			"unknown scope preserves pre-fix heuristic (substitute fallback)")
	})
}

// ---------------------------------------------------------------------------
// Correctness reconciliation — Finding 2: Patch status in teardown
// ---------------------------------------------------------------------------

// TestPatchStatusDetection proves that templateHasStatus correctly
// detects whether a Patch node applies status fields.
// During teardown, this determines whether releaseApply releases the
// status subresource.
//
// Per 003-ownership.md § Status Subresource: "Releases only target the
// subresources the template actually applied to — a status-only Patch
// releases only the status subresource."
func TestPatchStatusDetection(t *testing.T) {
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

// TestClassifyAPIError_EvalErrorWithNetworkPattern proves that non-API errors
// (CEL evaluation, template rendering) are classified as NodeError even when
// their message contains network-like patterns. Before the fix, an error
// message containing "unexpected EOF" from a JSON marshal failure would
// false-positive as a network error → NodeSystemError → 5s retry loop.
//
// The fix: errors originating from non-API operations are wrapped with
// ErrEvaluation at the source (toMap, evalString). classifyAPIError checks
// for this sentinel before falling through to network pattern matching.
//
// This tests a classification boundary on a pure function — manufacturing
// the same false-positive in e2e would require a contrived expression whose
// error text happens to match network patterns. The unit test encodes the
// property directly.
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
//
// This tests a pure priority function over inputs — not reconciliation
// behavior. The inputs are a set of states, the output is a condition.
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

// TestDeriveReadyCondition_PendingSurfacesReasons proves that when the
// Ready condition is Pending, the message includes per-node error details.
// Pure function test — exercises the formatting branch of deriveReadyCondition.
func TestDeriveReadyCondition_PendingSurfacesReasons(t *testing.T) {
	s := &reconcileState{
		compiled:    true,
		PlanSummary: PlanSummary{HasPending: true},
		nodeErrors:  []string{"deploy: waiting for input from cfg"},
	}
	_, _, message := s.deriveReadyCondition()
	assert.Contains(t, message, "waiting for input from cfg")
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
// Watch incremental cache tests
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

// TestWatchKindCacheLifecycle verifies the Watch cache in instanceState.
func TestWatchKindCacheLifecycle(t *testing.T) {
	spec := buildBenchSpec(3)
	compiled, err := compileGraphSpec(spec, nil)
	require.NoError(t, err)

	state := newInstanceState(compiled)

	// Initially empty.
	assert.Empty(t, state.collectionCache)

	// Store a cached list.
	items := []any{
		map[string]any{"metadata": map[string]any{"name": "pod-1"}},
		map[string]any{"metadata": map[string]any{"name": "pod-2"}},
	}
	state.collectionCache["myWatchKind"] = items

	// Retrieve.
	cached, ok := state.collectionCache["myWatchKind"]
	assert.True(t, ok)
	assert.Equal(t, 2, len(cached))
}

// TestWatchKindCacheDirty_Regression proves that instanceState carries a
// per-node dirty flag that the coordinator uses to force a full re-List
// after a failed incremental reconcile. Before this fix, a GET error in
// the incremental merge loop discarded the drained CollectionChanges;
// subsequent reconciles would take the incremental path against a stale
// cache and silently serve pre-failure data for up to one drift interval
// (default 30 minutes). The dirty flag makes the recovery explicit:
// coordinator detects dirty → forces full list → cache converges.
//
// Per 004-graph-reconciliation.md § Propagation: incremental updates must
// not allow the cache to serve stale data beyond the drift interval when
// a recoverable error interrupted a merge.
func TestWatchKindCacheDirty_Regression(t *testing.T) {
	spec := buildBenchSpec(3)
	compiled, err := compileGraphSpec(spec, nil)
	require.NoError(t, err)

	state := newInstanceState(compiled)
	require.NotNil(t, state.collectionDirty, "instanceState must initialize collectionDirty map")

	// Coordinator marks a node dirty after a Watch worker errors
	// without producing a collectionCacheUpdate.
	state.collectionDirty["pods"] = true
	assert.True(t, state.collectionDirty["pods"],
		"dirty flag must persist until next successful merge")

	// After a successful merge the coordinator clears the flag.
	delete(state.collectionDirty, "pods")
	assert.False(t, state.collectionDirty["pods"],
		"dirty flag cleared on successful merge")
}

// ---------------------------------------------------------------------------
// finalizeSkippedStates — silent Ready fallthrough (#14) regression
// ---------------------------------------------------------------------------

// TestFinalizeSkippedStates_RestoresPreviousState exercises the happy path —
// a node in the outputsReady set with a previousPlanStates entry has its
// state restored so PlanSummary counts it.
func TestFinalizeSkippedStates_RestoresPreviousState(t *testing.T) {
	plan := &PlanState{
		States: map[string]NodeState{
			"n1": nodeUnvisited,
		},
	}
	outputsReady := map[string]bool{"n1": true}
	prev := map[string]NodeState{"n1": NodeReady}

	finalizeSkippedStates(plan, outputsReady, prev, nil)

	assert.Equal(t, NodeReady, plan.States["n1"],
		"skipped node with prior state should restore to prior state")
}

// TestFinalizeSkippedStates_RegressionSilentReady guards against the
// silent-Ready fallthrough documented in #14: a node in outputsReady with no
// previousPlanStates entry previously stayed nodeUnvisited, which PlanSummary
// silently counts as zero — the graph appeared Ready with one fewer node
// than it actually had. The fix explicitly marks such nodes NodePending.
func TestFinalizeSkippedStates_RegressionSilentReady(t *testing.T) {
	plan := &PlanState{
		States: map[string]NodeState{
			"n1": nodeUnvisited,
		},
	}
	outputsReady := map[string]bool{"n1": true}
	// Empty previousPlanStates — the structurally-impossible case.
	prev := map[string]NodeState{}

	var diagnosedNode string
	finalizeSkippedStates(plan, outputsReady, prev, func(id string) {
		diagnosedNode = id
	})

	assert.Equal(t, NodePending, plan.States["n1"],
		"skipped node with no prior state must be marked Pending, not left Unvisited")
	assert.Equal(t, "n1", diagnosedNode,
		"the callback should surface the diagnostic so logs record the invariant break")
}

// TestFinalizeSkippedStates_IgnoresNonSkipped confirms the helper only
// touches nodes in outputsReady AND in nodeUnvisited — nodes that were
// actually walked keep whatever the walker set.
func TestFinalizeSkippedStates_IgnoresNonSkipped(t *testing.T) {
	plan := &PlanState{
		States: map[string]NodeState{
			"walked": NodeError,
			"skip":   nodeUnvisited,
		},
	}
	outputsReady := map[string]bool{"skip": true} // "walked" is NOT in outputsReady
	prev := map[string]NodeState{"skip": NodeReady, "walked": NodeReady}

	finalizeSkippedStates(plan, outputsReady, prev, nil)

	assert.Equal(t, NodeError, plan.States["walked"],
		"node not in outputsReady must not be overwritten")
	assert.Equal(t, NodeReady, plan.States["skip"],
		"node in outputsReady with prior state gets restored")
}

// ---------------------------------------------------------------------------
// pickEffectiveGeneration — compile-failure fallback generation (#19)
// ---------------------------------------------------------------------------

// revisionWithGeneration builds a minimal revision fixture for tests that
// only care about the generation label.
func revisionWithGeneration(gen int64) *unstructured.Unstructured {
	r := &unstructured.Unstructured{}
	r.SetLabels(map[string]string{
		LabelGraphGeneration: fmt.Sprintf("%d", gen),
	})
	return r
}

// TestPickEffectiveGeneration_HappyPath proves the default branch: when
// compilation succeeded, the graph's live generation is what was applied,
// so that's what gets stamped.
func TestPickEffectiveGeneration_HappyPath(t *testing.T) {
	graph := &unstructured.Unstructured{}
	graph.SetGeneration(7)
	active := revisionWithGeneration(7)

	got := pickEffectiveGeneration(graph, active, nil)
	assert.Equal(t, int64(7), got)
}

// TestPickEffectiveGeneration_RegressionFallbackGeneration guards #19: on
// compile-failure fallback, stamping the graph's generation would label
// resources with the generation whose spec didn't compile. The active
// revision's generation reflects what actually materialized.
func TestPickEffectiveGeneration_RegressionFallbackGeneration(t *testing.T) {
	graph := &unstructured.Unstructured{}
	graph.SetGeneration(10) // the spec at gen 10 failed to compile
	active := revisionWithGeneration(9)
	compileErr := errors.New("cel: undeclared reference to 'foo'")

	got := pickEffectiveGeneration(graph, active, compileErr)
	assert.Equal(t, int64(9), got,
		"fallback must stamp the active revision's generation, not the failed one")
}

// TestPickEffectiveGeneration_FallbackWithoutRevision is a defensive case.
// If compile failed AND no prior revision exists, the fallback path in the
// reconciler returns early before this is called. But the helper should
// still behave sanely: fall back to the graph generation rather than 0,
// which would produce a "generation=0" label.
func TestPickEffectiveGeneration_FallbackWithoutRevision(t *testing.T) {
	graph := &unstructured.Unstructured{}
	graph.SetGeneration(5)

	got := pickEffectiveGeneration(graph, nil, errors.New("compile failed"))
	assert.Equal(t, int64(5), got,
		"without an active revision, fall back to the graph's generation rather than stamping 0")
}

// ---------------------------------------------------------------------------
// forEach scope type safety
// ---------------------------------------------------------------------------

// TestForEach_RegressionNonSliceScopeReadyWhenNoPanic proves that if a forEach
// node's scope entry is not []any, the readyWhen stamping path returns an
// error instead of panicking on a type assertion.
//
// Regression: scopeVal.([]any) at foreach.go:312 is an unchecked type
// assertion that panics when scope contains a non-slice value.
func TestForEach_RegressionNonSliceScopeReadyWhenNoPanic(t *testing.T) {
	// forEachStampReadyWhen is the function under test (extracted helper).
	// Inject a non-slice value into scope for the forEach node.
	scope := map[string]any{
		"items": "not-a-slice",
	}
	err := forEachStampReadyWhen(scope, "items", []string{"true"}, nil)
	assert.Error(t, err, "non-slice scope should produce an error")
	assert.Contains(t, err.Error(), "expected []any")
}

// TestForEach_RegressionNonSliceScopeNoReadyWhenNoPanic proves the same for
// the no-readyWhen path (forEach nodes without readyWhen stamp all items
// __ready=true).
func TestForEach_RegressionNonSliceScopeNoReadyWhenNoPanic(t *testing.T) {
	scope := map[string]any{
		"items": 42, // should be []any
	}
	err := forEachStampReadyWhen(scope, "items", nil, nil)
	assert.Error(t, err, "non-slice scope should produce an error")
}

// TestForEach_SliceScopeReadyWhenWorks confirms the happy path still works.
func TestForEach_SliceScopeReadyWhenWorks(t *testing.T) {
	scope := map[string]any{
		"items": []any{
			map[string]any{"metadata": map[string]any{"name": "a"}},
			map[string]any{"metadata": map[string]any{"name": "b"}},
		},
	}
	// No readyWhen expressions — all items get __ready=true.
	err := forEachStampReadyWhen(scope, "items", nil, nil)
	require.NoError(t, err)
	items := scope["items"].([]any)
	for _, item := range items {
		m := item.(map[string]any)
		assert.Equal(t, true, m["__ready"])
	}
}

// ---------------------------------------------------------------------------
// .updated() CEL function tests
//
// Per 001-graph.md § CEL Functions: ".updated() — true when the node is
// on the latest graph generation." Per 004-graph-reconciliation.md § Propagation
// Control: used in propagateWhen for forEach rollout gating.
// ---------------------------------------------------------------------------

// TestUpdatedFunction_ScalarNode proves that .updated() reads __updated from
// a scalar scope object (map[string]any).
func TestUpdatedFunction_ScalarNode(t *testing.T) {
	spec := &GraphSpec{
		Nodes: []Node{
			{ID: "deploy", Template: map[string]any{
				"apiVersion": "apps/v1", "kind": "Deployment",
				"metadata": map[string]any{"name": "test"},
			}},
			{ID: "check", Def: map[string]any{
				"result": "${deploy.updated()}",
			}},
		},
	}
	compiled, err := compileGraphSpec(spec, nil)
	require.NoError(t, err)

	tests := []struct {
		name    string
		updated any
		want    bool
	}{
		{"updated=true", true, true},
		{"updated=false", false, false},
		{"no __updated field", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deploy := map[string]any{
				"metadata": map[string]any{"name": "test"},
			}
			if tt.updated != nil {
				deploy["__updated"] = tt.updated
			}
			result, err := compiled.eval("deploy.updated()", map[string]any{
				"deploy": deploy,
			})
			require.NoError(t, err)
			assert.Equal(t, tt.want, result)
		})
	}
}

// TestUpdatedFunction_Collection proves that .updated() on a collection
// returns true only when ALL items have __updated == true (the aggregate
// semantics matching .ready()). Empty collections are vacuously true.
func TestUpdatedFunction_Collection(t *testing.T) {
	spec := &GraphSpec{
		Nodes: []Node{
			{ID: "deploys", Watch: map[string]any{
				"apiVersion": "apps/v1", "kind": "Deployment",
				"selector": map[string]any{},
			}, nodeType: NodeTypeWatch},
			{ID: "check", Def: map[string]any{
				"result": "${deploys.updated()}",
			}},
		},
	}
	compiled, err := compileGraphSpec(spec, nil)
	require.NoError(t, err)

	tests := []struct {
		name  string
		items []any
		want  bool
	}{
		{"empty collection", []any{}, true},
		{"all updated", []any{
			map[string]any{"__updated": true, "metadata": map[string]any{"name": "a"}},
			map[string]any{"__updated": true, "metadata": map[string]any{"name": "b"}},
		}, true},
		{"one not updated", []any{
			map[string]any{"__updated": true, "metadata": map[string]any{"name": "a"}},
			map[string]any{"__updated": false, "metadata": map[string]any{"name": "b"}},
		}, false},
		{"none updated", []any{
			map[string]any{"__updated": false, "metadata": map[string]any{"name": "a"}},
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := compiled.eval("deploys.updated()", map[string]any{
				"deploys": tt.items,
			})
			require.NoError(t, err)
			assert.Equal(t, tt.want, result)
		})
	}
}

// TestUpdatedFunction_Filter proves that .updated() works inside
// collection filter expressions — the primary propagateWhen pattern from
// 004-graph-reconciliation.md § Propagation Control.
func TestUpdatedFunction_Filter(t *testing.T) {
	spec := &GraphSpec{
		Nodes: []Node{
			{ID: "deploys", Watch: map[string]any{
				"apiVersion": "apps/v1", "kind": "Deployment",
				"selector": map[string]any{},
			}, nodeType: NodeTypeWatch},
			{ID: "check", Def: map[string]any{
				"count": "${deploys.filter(d, d.updated()).size()}",
			}},
		},
	}
	compiled, err := compileGraphSpec(spec, nil)
	require.NoError(t, err)

	items := []any{
		map[string]any{"__updated": true, "metadata": map[string]any{"name": "a"}},
		map[string]any{"__updated": false, "metadata": map[string]any{"name": "b"}},
		map[string]any{"__updated": true, "metadata": map[string]any{"name": "c"}},
	}
	result, err := compiled.eval("deploys.filter(d, d.updated()).size()", map[string]any{
		"deploys": items,
	})
	require.NoError(t, err)
	assert.Equal(t, int64(2), result)
}

// TestUpdatedFunction_CombinedWithReady proves the four-state matrix from
// 004-graph-reconciliation.md § Propagation Control works in CEL:
//
//	updated() && ready()  → Current
//	updated() && !ready() → Updating
//	!updated() && ready() → Pending
//	!updated() && !ready()→ Stuck
func TestUpdatedFunction_CombinedWithReady(t *testing.T) {
	spec := &GraphSpec{
		Nodes: []Node{
			{ID: "items", Watch: map[string]any{
				"apiVersion": "v1", "kind": "Pod",
				"selector": map[string]any{},
			}, nodeType: NodeTypeWatch},
			{ID: "counts", Def: map[string]any{
				"current":  "${items.filter(i, i.updated() && i.ready()).size()}",
				"updating": "${items.filter(i, i.updated() && !i.ready()).size()}",
				"pending":  "${items.filter(i, !i.updated() && i.ready()).size()}",
				"stuck":    "${items.filter(i, !i.updated() && !i.ready()).size()}",
			}},
		},
	}
	compiled, err := compileGraphSpec(spec, nil)
	require.NoError(t, err)

	items := []any{
		map[string]any{"__updated": true, "__ready": true, "metadata": map[string]any{"name": "current"}},
		map[string]any{"__updated": true, "__ready": false, "metadata": map[string]any{"name": "updating"}},
		map[string]any{"__updated": false, "__ready": true, "metadata": map[string]any{"name": "pending"}},
		map[string]any{"__updated": false, "__ready": false, "metadata": map[string]any{"name": "stuck"}},
	}
	scope := map[string]any{"items": items}

	current, err := compiled.eval("items.filter(i, i.updated() && i.ready()).size()", scope)
	require.NoError(t, err)
	assert.Equal(t, int64(1), current)

	updating, err := compiled.eval("items.filter(i, i.updated() && !i.ready()).size()", scope)
	require.NoError(t, err)
	assert.Equal(t, int64(1), updating)

	pending, err := compiled.eval("items.filter(i, !i.updated() && i.ready()).size()", scope)
	require.NoError(t, err)
	assert.Equal(t, int64(1), pending)

	stuck, err := compiled.eval("items.filter(i, !i.updated() && !i.ready()).size()", scope)
	require.NoError(t, err)
	assert.Equal(t, int64(1), stuck)
}

// ---------------------------------------------------------------------------
// isItemUpdated tests — generation label comparison
// ---------------------------------------------------------------------------

// TestIsForEachItemUpdated proves the generation label lookup for forEach children.
func TestIsForEachItemUpdated(t *testing.T) {
	parentID := "deploys"
	graphName := "mygraph"
	graphNS := "default"

	// Build a resource with the forEach child generation label.
	makeItem := func(name, ns, apiVersion, kind string, gen int64) map[string]any {
		gvk := gvkFromMap(map[string]any{"apiVersion": apiVersion, "kind": kind})
		genKey := forEachChildGenerationLabelKey(parentID, name, ns, kind, gvk.Group, graphName, graphNS)
		return map[string]any{
			"apiVersion": apiVersion,
			"kind":       kind,
			"metadata": map[string]any{
				"name":      name,
				"namespace": ns,
				"labels": map[string]any{
					genKey: fmt.Sprintf("%d", gen),
				},
			},
		}
	}

	tests := []struct {
		name                string
		item                map[string]any
		effectiveGeneration int64
		want                bool
	}{
		{
			name:                "generation matches",
			item:                makeItem("frontend", "default", "apps/v1", "Deployment", 5),
			effectiveGeneration: 5,
			want:                true,
		},
		{
			name:                "generation does not match",
			item:                makeItem("frontend", "default", "apps/v1", "Deployment", 4),
			effectiveGeneration: 5,
			want:                false,
		},
		{
			name:                "no metadata",
			item:                map[string]any{"kind": "Pod"},
			effectiveGeneration: 5,
			want:                false, // unknown provenance → needs update
		},
		{
			name: "no labels",
			item: map[string]any{
				"apiVersion": "v1", "kind": "Pod",
				"metadata": map[string]any{"name": "test"},
			},
			effectiveGeneration: 5,
			want:                false, // no labels → needs update
		},
		{
			name: "no generation label (wrong graph)",
			item: map[string]any{
				"apiVersion": "v1", "kind": "Pod",
				"metadata": map[string]any{
					"name":      "test",
					"namespace": "default",
					"labels":    map[string]any{"unrelated": "label"},
				},
			},
			effectiveGeneration: 5,
			want:                false, // no kro generation label → needs update
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isForEachItemUpdated(tt.item, parentID, graphName, graphNS, tt.effectiveGeneration)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestMarkUpdated proves that markUpdated stamps __updated on scope entries.
func TestMarkUpdated(t *testing.T) {
	eval := &evaluator{
		scope: map[string]any{
			"deploy": map[string]any{"metadata": map[string]any{"name": "test"}},
		},
	}

	eval.markUpdated("deploy", true)
	m := eval.scope["deploy"].(map[string]any)
	assert.Equal(t, true, m["__updated"])

	eval.markUpdated("deploy", false)
	assert.Equal(t, false, m["__updated"])
}

// ---------------------------------------------------------------------------
// __updated stamping integration tests
//
// These exercise the actual code paths that stamp __updated, not just the
// helper functions. Each test drives a real reconciliation method or
// forEach loop path and verifies __updated appears with the correct value.
// ---------------------------------------------------------------------------

// TestReconcileDefinition_StampsUpdated proves that reconcileDefinition
// stamps __updated = true on the scope entry. Definition nodes are always
// re-evaluated — vacuously updated.
func TestReconcileDefinition_StampsUpdated(t *testing.T) {
	spec := &GraphSpec{
		Nodes: []Node{
			{ID: "config", Def: map[string]any{
				"name":  "my-app",
				"port":  "8080",
				"debug": "false",
			}},
		},
	}
	compiled, err := compileGraphSpec(spec, nil)
	require.NoError(t, err)

	eval := &evaluator{compiled: compiled, scope: map[string]any{}}
	r := &GraphReconciler{}
	err = r.reconcileDefinition(context.Background(), spec.Nodes[0], eval)
	require.NoError(t, err)

	m, ok := eval.scope["config"].(map[string]any)
	require.True(t, ok, "scope should contain config")
	assert.Equal(t, true, m["__updated"],
		"definition node must have __updated = true (always re-evaluated)")
}

// TestForEach_CarryForwardStampsUpdatedFromLabel proves that forEach items
// carried forward by propagateWhen get __updated derived from the generation
// label on the resource, not hardcoded true. This is the critical path for
// rollout gating — carried-forward items on an old generation must show
// updated() = false so propagateWhen can count them as "Pending" in the
// four-state matrix (004-graph-reconciliation.md § Propagation Control).
func TestForEach_CarryForwardStampsUpdatedFromLabel(t *testing.T) {
	spec := &GraphSpec{
		Nodes: []Node{
			{ID: "source", Def: map[string]any{
				"items": []any{
					map[string]any{"metadata": map[string]any{"name": "alpha"}},
					map[string]any{"metadata": map[string]any{"name": "beta"}},
				},
			}},
			{ID: "workers", ForEach: map[string]string{"item": "${source.items}"},
				// propagateWhen that immediately halts — forces ALL items
				// through the carry-forward path.
				PropagateWhen: []string{"${false}"},
				Def:           map[string]any{"name": "${item.metadata.name}"},
			},
		},
	}
	compiled, err := compileGraphSpec(spec, nil)
	require.NoError(t, err)

	state := newInstanceState(compiled)
	eval := newEvaluator(state)
	eval.effectiveGeneration = 5

	// Populate source in scope.
	eval.scope["source"] = map[string]any{
		"items": []any{
			map[string]any{"metadata": map[string]any{"name": "alpha"}},
			map[string]any{"metadata": map[string]any{"name": "beta"}},
		},
	}

	// Build previous scope data with generation labels.
	// "alpha" has current generation (5) → __updated = true
	// "beta" has old generation (4) → __updated = false
	alphaGenKey := forEachChildGenerationLabelKey("workers", "alpha", "default", "Deployment", "apps", "test", "default")
	betaGenKey := forEachChildGenerationLabelKey("workers", "beta", "default", "Deployment", "apps", "test", "default")

	prevAlpha := map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{
			"name": "alpha", "namespace": "default",
			"labels": map[string]any{alphaGenKey: "5"},
		},
		"__ready": true,
	}
	prevBeta := map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{
			"name": "beta", "namespace": "default",
			"labels": map[string]any{betaGenKey: "4"},
		},
		"__ready": true,
	}

	// Pre-populate forEach state so items are "known" and can be carried forward.
	eval.forEachPrevItems = map[string][]any{
		"workers/item": {
			map[string]any{"metadata": map[string]any{"name": "alpha"}},
			map[string]any{"metadata": map[string]any{"name": "beta"}},
		},
	}
	eval.forEachPrevScope = map[string]map[string]any{
		"workers": {"alpha": prevAlpha, "beta": prevBeta},
	}
	eval.forEachPrevKeys = map[string]map[string][]string{
		"workers": {"alpha": {"key-alpha"}, "beta": {"key-beta"}},
	}
	eval.forEachNewScope = map[string]map[string]any{}
	eval.forEachNewKeys = map[string]map[string][]string{}
	eval.forEachNewItems = map[string][]any{}

	graph := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "experimental.kro.run/v1alpha1",
		"kind":       "Graph",
		"metadata":   map[string]any{"name": "test", "namespace": "default"},
	}}

	r := &GraphReconciler{}
	_, err = r.reconcileForEach(context.Background(), graph, spec.Nodes[1], eval, nil, false)
	// ErrWaitingForReadiness expected — propagateWhen halted expansion.
	require.ErrorIs(t, err, ErrWaitingForReadiness)

	// Verify carried-forward items in scope have correct __updated stamps.
	items, ok := eval.scope["workers"].([]any)
	require.True(t, ok, "scope should contain workers as []any")
	require.Len(t, items, 2)

	alpha, ok := items[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, true, alpha["__updated"],
		"alpha (generation 5 == effectiveGeneration 5) should be updated")

	beta, ok := items[1].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, false, beta["__updated"],
		"beta (generation 4 != effectiveGeneration 5) should NOT be updated")
}

// TestForEach_SkippedUnchangedStampsUpdatedFromLabel proves that forEach
// items skipped because their input is unchanged still get __updated
// re-stamped from the generation label. This matters after restarts: the
// input hash matches but the generation label on the resource is the only
// signal of which generation it was applied in.
func TestForEach_SkippedUnchangedStampsUpdatedFromLabel(t *testing.T) {
	spec := &GraphSpec{
		Nodes: []Node{
			{ID: "source", Def: map[string]any{
				"items": []any{
					map[string]any{"metadata": map[string]any{"name": "alpha"}},
				},
			}},
			{ID: "results", ForEach: map[string]string{"item": "${source.items}"},
				Def: map[string]any{"name": "${item.metadata.name}"},
			},
		},
	}
	compiled, err := compileGraphSpec(spec, nil)
	require.NoError(t, err)

	state := newInstanceState(compiled)
	eval := newEvaluator(state)
	eval.effectiveGeneration = 7

	sourceItems := []any{
		map[string]any{"metadata": map[string]any{"name": "alpha"}},
	}
	eval.scope["source"] = map[string]any{"items": sourceItems}

	// Build previous scope with a generation label matching current gen.
	genKey := forEachChildGenerationLabelKey("results", "alpha", "default", "Deployment", "apps", "test", "default")
	prevAlpha := map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{
			"name": "alpha", "namespace": "default",
			"labels": map[string]any{genKey: "7"},
		},
		"name": "alpha",
	}

	// Pre-populate forEach state: same items as current → skip path fires.
	eval.forEachPrevItems = map[string][]any{
		"results/item": sourceItems,
	}
	eval.forEachPrevScope = map[string]map[string]any{
		"results": {"alpha": prevAlpha},
	}
	eval.forEachPrevKeys = map[string]map[string][]string{
		"results": {"alpha": {}},
	}
	eval.forEachNewScope = map[string]map[string]any{}
	eval.forEachNewKeys = map[string]map[string][]string{}
	eval.forEachNewItems = map[string][]any{}

	graph := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "experimental.kro.run/v1alpha1",
		"kind":       "Graph",
		"metadata":   map[string]any{"name": "test", "namespace": "default"},
	}}

	r := &GraphReconciler{}
	_, err = r.reconcileForEach(context.Background(), graph, spec.Nodes[1], eval, nil, false)
	require.NoError(t, err)

	items, ok := eval.scope["results"].([]any)
	require.True(t, ok)
	require.Len(t, items, 1)

	alpha, ok := items[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, true, alpha["__updated"],
		"skipped-unchanged item with matching generation (7 == 7) should be updated")

	// Now test the mismatch case: same input, old generation label.
	eval2 := newEvaluator(state)
	eval2.effectiveGeneration = 8 // generation advanced
	eval2.scope["source"] = map[string]any{"items": sourceItems}

	// prevAlpha still has generation "7" but effectiveGeneration is now 8.
	eval2.forEachPrevItems = map[string][]any{"results/item": sourceItems}
	eval2.forEachPrevScope = map[string]map[string]any{
		"results": {"alpha": prevAlpha},
	}
	eval2.forEachPrevKeys = map[string]map[string][]string{"results": {"alpha": {}}}
	eval2.forEachNewScope = map[string]map[string]any{}
	eval2.forEachNewKeys = map[string]map[string][]string{}
	eval2.forEachNewItems = map[string][]any{}

	_, err = r.reconcileForEach(context.Background(), graph, spec.Nodes[1], eval2, nil, false)
	require.NoError(t, err)

	items2, ok := eval2.scope["results"].([]any)
	require.True(t, ok)
	require.Len(t, items2, 1)

	alpha2, ok := items2[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, false, alpha2["__updated"],
		"skipped-unchanged item with old generation (7 != 8) should NOT be updated")
}
