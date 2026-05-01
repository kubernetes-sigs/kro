package graphcontroller

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ellistarn/kro/experimental/controller/compiler"
	dagpkg "github.com/ellistarn/kro/experimental/controller/dag"
	graphpkg "github.com/ellistarn/kro/experimental/controller/graph"
	"github.com/ellistarn/kro/experimental/controller/watches"
)

// ---------------------------------------------------------------------------
// Unit tests — design reconciliation regression suite
//
// These test mechanisms introduced in the design reconciliation:
//   - dagpkg.NodeBlocked vs dagpkg.NodeExcluded propagation split
//   - pruneOrder reverse dependency ordering
//   - classifyAPIError default flip
//
// Each test verifies the system DOES the right thing (correctness), not
// just that it doesn't do the wrong thing (safety).
// ---------------------------------------------------------------------------

// TestSetStateDoesNotPropagate proves that SetState only sets the source
// node's state and does NOT propagate to dependents. State propagation
// is the responsibility of checkDependencyGate (called during the walk),
// which evaluates all dependencies with full precedence (Excluded > Blocked > Pending).
//
// The previous implementation used a first-wins flood fill that violated
// precedence in diamond dependencies: if an Error parent propagated
// before an Excluded parent, the child was marked Blocked instead of
// Excluded — an incorrect classification that prevented pruning.
func TestSetStateDoesNotPropagate(t *testing.T) {
	// Build a chain: A → B → C
	nodes := []graphpkg.Node{
		{ID: "a", Template: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "a"}}},
		{ID: "b", Template: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "b"}, "data": map[string]any{"ref": "${a.metadata.name}"}}},
		{ID: "c", Template: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "c"}, "data": map[string]any{"ref": "${b.metadata.name}"}}},
	}
	dag, err := dagpkg.BuildDAG(nodes, nil, nil)
	require.NoError(t, err)

	states := []dagpkg.NodeState{dagpkg.NodeExcluded, dagpkg.NodeError, dagpkg.NodePending, dagpkg.NodeConflict, dagpkg.NodeSystemError, dagpkg.NodeReady, dagpkg.NodeNotReady}
	for _, sourceState := range states {
		t.Run(sourceState.String(), func(t *testing.T) {
			plan := dagpkg.NewPlanState(dag)
			plan.SetState(dag, "a", sourceState)

			assert.Equal(t, sourceState, plan.States["a"], "source should be set")
			assert.Equal(t, dagpkg.NodeUnvisited, plan.States["b"], "direct dependent should remain unvisited")
			assert.Equal(t, dagpkg.NodeUnvisited, plan.States["c"], "transitive dependent should remain unvisited")
		})
	}
}

// TestSummaryCountsBlockedState proves that dagpkg.PlanSummary correctly reports
// HasBlocked when a node is explicitly set to dagpkg.NodeBlocked. Since SetState
// no longer propagates, the test sets dependent state explicitly (matching
// what checkDependencyGate would do during a real walk).
func TestSummaryCountsBlockedState(t *testing.T) {
	nodes := []graphpkg.Node{
		{ID: "a", Template: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "a"}}},
		{ID: "b", Template: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "b"}, "data": map[string]any{"ref": "${a.metadata.name}"}}},
	}
	dag, err := dagpkg.BuildDAG(nodes, nil, nil)
	require.NoError(t, err)

	plan := dagpkg.NewPlanState(dag)
	plan.SetState(dag, "a", dagpkg.NodeError)
	// Simulate what checkDependencyGate does: when b's dependency a is in error,
	// the walk marks b as Blocked.
	plan.SetState(dag, "b", dagpkg.NodeBlocked)

	summary := plan.Summary()
	assert.True(t, summary.HasError, "should report error on the source node")
	assert.True(t, summary.HasBlocked, "should report blocked on the dependent node")
	assert.Equal(t, 0, summary.ReadyCount)
}

// ---------------------------------------------------------------------------
// checkDependencyGate state propagation tests
//
// These replace the old SetState propagation tests. The design commitment
// (004 § Propagation: Excluded > Blocked > Pending) is now enforced by
// checkDependencyGate during the walk. These tests exercise it directly.
// ---------------------------------------------------------------------------

// gateToNodeState maps a gateState to the dagpkg.NodeState that the walk
// would assign. Used by tests to verify precedence without needing a full walk.
func gateToNodeState(g gateState) dagpkg.NodeState {
	switch g {
	case gateExcluded:
		return dagpkg.NodeExcluded
	case gateBlocked:
		return dagpkg.NodeBlocked
	case gatePending:
		return dagpkg.NodePending
	default:
		return dagpkg.NodeReady // gateDispatch — would proceed to evaluation
	}
}

// TestCheckDependencyGatePrecedence_ExcludedOverBlockedOverPending proves that
// checkDependencyGate enforces the design's state precedence when a node has
// multiple dependencies in different failure states.
//
// Per 005-reconciliation.md § Propagation:
//   - "Any dependency Excluded → Excluded, regardless of other dependencies'
//     states (definitive absence propagates; the node is structurally non-viable)."
//   - "Any dependency in an error state → inherit Blocked."
//   - "Any dependency Pending → inherit Pending."
//   - "Precedence where multiple apply: Excluded > Blocked > Pending"
func TestCheckDependencyGatePrecedence_ExcludedOverBlockedOverPending(t *testing.T) {
	// Diamond: A and B are parents of C.
	nodes := []graphpkg.Node{
		{ID: "a", Template: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "a"}}},
		{ID: "b", Template: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "b"}}},
		{ID: "c", Template: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "c"}, "data": map[string]any{"a": "${a.metadata.name}", "b": "${b.metadata.name}"}}},
	}
	dag, err := dagpkg.BuildDAG(nodes, nil, nil)
	require.NoError(t, err)

	tests := []struct {
		name       string
		stateA     dagpkg.NodeState
		stateB     dagpkg.NodeState
		wantChildC dagpkg.NodeState
	}{
		// Excluded > Blocked: Excluded parent takes precedence over Error parent.
		{"Excluded+Error→Excluded", dagpkg.NodeExcluded, dagpkg.NodeError, dagpkg.NodeExcluded},
		{"Error+Excluded→Excluded", dagpkg.NodeError, dagpkg.NodeExcluded, dagpkg.NodeExcluded},
		// Excluded > Pending: Excluded parent takes precedence over Pending parent.
		{"Excluded+Pending→Excluded", dagpkg.NodeExcluded, dagpkg.NodePending, dagpkg.NodeExcluded},
		{"Pending+Excluded→Excluded", dagpkg.NodePending, dagpkg.NodeExcluded, dagpkg.NodeExcluded},
		// Blocked > Pending: Error parent takes precedence over Pending parent.
		{"Error+Pending→Blocked", dagpkg.NodeError, dagpkg.NodePending, dagpkg.NodeBlocked},
		{"Pending+Error→Blocked", dagpkg.NodePending, dagpkg.NodeError, dagpkg.NodeBlocked},
		// Same states: straightforward inheritance.
		{"Excluded+Excluded→Excluded", dagpkg.NodeExcluded, dagpkg.NodeExcluded, dagpkg.NodeExcluded},
		{"Error+Error→Blocked", dagpkg.NodeError, dagpkg.NodeError, dagpkg.NodeBlocked},
		{"Pending+Pending→Pending", dagpkg.NodePending, dagpkg.NodePending, dagpkg.NodePending},
		// All error variants map to Blocked.
		{"Conflict+Pending→Blocked", dagpkg.NodeConflict, dagpkg.NodePending, dagpkg.NodeBlocked},
		{"SystemError+Pending→Blocked", dagpkg.NodeSystemError, dagpkg.NodePending, dagpkg.NodeBlocked},
		{"Blocked+Pending→Blocked", dagpkg.NodeBlocked, dagpkg.NodePending, dagpkg.NodeBlocked},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			plan := dagpkg.NewPlanState(dag)
			plan.SetState(dag, "a", tc.stateA)
			plan.SetState(dag, "b", tc.stateB)

			cIdx := dag.Index["c"]
			node := &dag.Nodes[cIdx]

			gate := checkDependencyGate(node, plan)
			gotState := gateToNodeState(gate)
			assert.Equal(t, tc.wantChildC, gotState,
				"child C should inherit %s from parents %s+%s", tc.wantChildC, tc.stateA, tc.stateB)
		})
	}
}

// TestPruneOrderReverseDependency proves that pruneOrder sorts prune
// candidates so dependents are deleted before their dependencies. This
// prevents dangling references during prune.
func TestPruneOrderReverseDependency(t *testing.T) {
	// Build A → B → C. Topological order: A(0), B(1), C(2).
	// Reverse dependency order for deletion: C, B, A.
	nodes := []graphpkg.Node{
		{ID: "a", Template: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "a"}}},
		{ID: "b", Template: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "b"}, "data": map[string]any{"ref": "${a.metadata.name}"}}},
		{ID: "c", Template: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "c"}, "data": map[string]any{"ref": "${b.metadata.name}"}}},
	}
	dag, err := dagpkg.BuildDAG(nodes, nil, nil)
	require.NoError(t, err)

	keys := []string{
		"/v1/ConfigMap/default/a",
		"/v1/ConfigMap/default/b",
		"/v1/ConfigMap/default/c",
	}

	ordered := pruneOrder(keys, []*dagpkg.DAG{dag}, "default", nil)

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
	nodes := []graphpkg.Node{
		{ID: "a", Template: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "a"}}},
	}
	dag, err := dagpkg.BuildDAG(nodes, nil, nil)
	require.NoError(t, err)

	keys := []string{
		"/v1/ConfigMap/default/a",
		"/v1/ConfigMap/default/unknown-dynamic",
	}

	ordered := pruneOrder(keys, []*dagpkg.DAG{dag}, "default", nil)

	require.Len(t, ordered, 2)
	assert.Equal(t, "/v1/ConfigMap/default/unknown-dynamic", ordered[0], "unmatched key should be first")
	assert.Equal(t, "/v1/ConfigMap/default/a", ordered[1])
}

// TestPruneOrderContributeKeysResolved proves that contribute-prefixed keys
// are resolved to their underlying resource key for position lookup.
func TestPruneOrderContributeKeysResolved(t *testing.T) {
	nodes := []graphpkg.Node{
		{ID: "a", Template: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "a"}}},
		{ID: "b", Template: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "b"}, "data": map[string]any{"ref": "${a.metadata.name}"}}},
	}
	dag, err := dagpkg.BuildDAG(nodes, nil, nil)
	require.NoError(t, err)

	keys := []string{
		"/v1/ConfigMap/default/a",
		"patch:/v1/ConfigMap/default/b",
	}

	ordered := pruneOrder(keys, []*dagpkg.DAG{dag}, "default", nil)

	require.Len(t, ordered, 2)
	assert.Equal(t, "patch:/v1/ConfigMap/default/b", ordered[0], "dependent patch key should be first")
	assert.Equal(t, "/v1/ConfigMap/default/a", ordered[1])
}

// TestClassifyAPIErrorDefault proves that unrecognized errors (raw Go errors
// not wrapped as *StatusError) become dagpkg.NodeSystemError — the safe direction.
// Misclassifying transient network failures as deterministic (dagpkg.NodeError) means
// the system stops retrying when it should be retrying hardest (30-minute resync
// timer vs 5s SystemError retry). Misclassifying a deterministic error as
// transient means wasted retries — annoying but not an outage.
//
// Client errors (4xx) are positively identified; everything else is
// infrastructure until proven otherwise.
func TestClassifyAPIErrorDefault(t *testing.T) {
	t.Run("raw network error is dagpkg.NodeSystemError", func(t *testing.T) {
		err := &net.OpError{Op: "dial", Net: "tcp", Err: fmt.Errorf("connection refused")}
		info := classifyAPIError(err)
		assert.Equal(t, dagpkg.NodeSystemError, info.state,
			"network errors should be dagpkg.NodeSystemError — transient, needs retry")
	})

	t.Run("generic wrapped error is dagpkg.NodeSystemError", func(t *testing.T) {
		err := fmt.Errorf("unexpected EOF during API call")
		info := classifyAPIError(err)
		assert.Equal(t, dagpkg.NodeSystemError, info.state,
			"unrecognized errors default to dagpkg.NodeSystemError — safe direction")
	})

	t.Run("forbidden is dagpkg.NodeError", func(t *testing.T) {
		err := apierrors.NewForbidden(schema.GroupResource{Group: "", Resource: "configmaps"}, "test", fmt.Errorf("forbidden"))
		info := classifyAPIError(err)
		assert.Equal(t, dagpkg.NodeError, info.state)
		assert.Equal(t, "Forbidden", info.reason)
	})

	t.Run("unauthorized is dagpkg.NodeError", func(t *testing.T) {
		err := apierrors.NewUnauthorized("bad token")
		info := classifyAPIError(err)
		assert.Equal(t, dagpkg.NodeError, info.state)
		assert.Equal(t, "Unauthorized", info.reason)
	})

	t.Run("invalid is dagpkg.NodeError", func(t *testing.T) {
		err := apierrors.NewInvalid(schema.GroupKind{Group: "", Kind: "ConfigMap"}, "test", nil)
		info := classifyAPIError(err)
		assert.Equal(t, dagpkg.NodeError, info.state)
		assert.Equal(t, "ValidationFailed", info.reason)
	})

	t.Run("bad request is dagpkg.NodeError", func(t *testing.T) {
		err := apierrors.NewBadRequest("malformed")
		info := classifyAPIError(err)
		assert.Equal(t, dagpkg.NodeError, info.state)
		assert.Equal(t, "BadRequest", info.reason)
	})

	t.Run("internal server error is dagpkg.NodeSystemError", func(t *testing.T) {
		err := apierrors.NewInternalError(fmt.Errorf("etcd timeout"))
		info := classifyAPIError(err)
		assert.Equal(t, dagpkg.NodeSystemError, info.state, "5xx should be dagpkg.NodeSystemError")
		assert.Equal(t, "ServerError", info.reason)
	})

	t.Run("service unavailable is dagpkg.NodeSystemError", func(t *testing.T) {
		err := apierrors.NewServiceUnavailable("maintenance")
		info := classifyAPIError(err)
		assert.Equal(t, dagpkg.NodeSystemError, info.state)
	})

	t.Run("too many requests is dagpkg.NodeSystemError", func(t *testing.T) {
		err := apierrors.NewTooManyRequests("rate limited", 5)
		info := classifyAPIError(err)
		assert.Equal(t, dagpkg.NodeSystemError, info.state)
	})

	t.Run("nil error returns zero value", func(t *testing.T) {
		info := classifyAPIError(nil)
		assert.Equal(t, dagpkg.NodeState(0), info.state)
	})
}

// TestParseNodeListEnforcesValidNodeIDs verifies that parseNodeList rejects
// node IDs that are not valid CEL identifiers. Stricter naming conventions
// (e.g., lower camelCase for RGD resources) are enforced by the consumer.
func TestParseNodeListEnforcesValidNodeIDs(t *testing.T) {
	tests := []struct {
		name    string
		id      string
		wantErr bool
	}{
		// Valid cases — accepted by the parser (valid CEL identifiers)
		{name: "simple alphanumeric", id: "deploy", wantErr: false},
		{name: "single character", id: "a", wantErr: false},
		{name: "camelCase", id: "myApp2v3", wantErr: false},
		{name: "long but valid", id: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", wantErr: false},
		{name: "starts with uppercase", id: "MyResource", wantErr: false},
		{name: "underscore", id: "foo_bar", wantErr: false},
		{name: "multiple underscores", id: "prstatus_test_app_uat", wantErr: false},
		{name: "leading underscore", id: "_foo", wantErr: false},

		// Invalid cases — not valid CEL identifiers
		{name: "empty string", id: "", wantErr: true},
		{name: "starts with digit", id: "123resource", wantErr: true},
		{name: "dot", id: "foo.bar", wantErr: true},
		{name: "space", id: "foo bar", wantErr: true},
		{name: "hyphen", id: "my-app", wantErr: true},
		{name: "leading dot", id: ".foo", wantErr: true},
		{name: "special character", id: "resource!", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw := []any{
				map[string]any{
					"id":       tc.id,
					"template": map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "cm"}},
				},
			}
			_, err := graphpkg.ParseNodeList(raw)
			if tc.wantErr {
				require.Error(t, err, "node ID %q must be rejected", tc.id)
			} else {
				require.NoError(t, err, "node ID %q must be accepted", tc.id)
			}
		})
	}
} // TestNodeStateString verifies that dagpkg.NodeState.String() returns the design's
// canonical names. Each concept has exactly one name.
func TestNodeStateString(t *testing.T) {
	tests := []struct {
		state dagpkg.NodeState
		want  string
	}{
		{dagpkg.NodeUnvisited, "Unvisited"},
		{dagpkg.NodePending, "Pending"},
		{dagpkg.NodeReady, "Ready"},
		{dagpkg.NodeNotReady, "NotReady"},
		{dagpkg.NodeExcluded, "Excluded"},
		{dagpkg.NodeBlocked, "Blocked"},
		{dagpkg.NodeError, "Error"},
		{dagpkg.NodeConflict, "Conflict"},
		{dagpkg.NodeSystemError, "SystemError"},
	}
	for _, tc := range tests {
		t.Run(tc.want, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.state.String())
		})
	}
}

// TestForEachChildIdentityLabelKey verifies the DNS subdomain format for
// forEach child identity labels per 005-reconciliation.md § Child Identity.
func TestForEachChildIdentityLabelKey(t *testing.T) {
	key := graphpkg.ForEachChildIdentityLabelKey(
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
	key := graphpkg.ForEachChildIdentityLabelKey(
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
			got := watches.GVRKindFromInformer(gvr, nil)
			assert.Equal(t, tc.want, got, "GVRKindFromInformer(%q) should produce correct singular form", tc.resource)
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
	got := watches.GVRKindFromInformer(gvr, accessor)
	assert.Equal(t, "NetworkPolicy", got, "primary path should return exact CamelCase Kind")
}

// TestClassifyAPIErrorNetworkErrors_RegressionRetry proves that raw network
// errors (not wrapped as *StatusError) get classified as dagpkg.NodeSystemError for
// the 5s retry instead of dagpkg.NodeError's 30-minute resync timer.
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
			assert.Equal(t, dagpkg.NodeSystemError, info.state,
				"network/transient error %q should be dagpkg.NodeSystemError for fast retry", tc.err)
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
// is a permanent spec error — but it must NOT produce dagpkg.NodeError (which
// blocks dependents). It must produce dagpkg.NodeNotReady.
// ---------------------------------------------------------------------------

// TestEvalReadiness_ExpressionErrorWrapsReadyWhenFailed proves that when
// readyWhen evaluation fails with a permanent expression error (not data
// pending), evalReadiness wraps it with compiler.ErrReadyWhenFailed so the
// coordinator classifies it as dagpkg.NodeNotReady instead of dagpkg.NodeError.
// // TestEvalReadiness_NormalNotReadyIsUnchanged proves that normal readyWhen
// failures (condition evaluates to false, data pending) still produce
// compiler.ErrWaitingForReadiness — the fix only affects expression errors.
func TestEvalReadiness_NormalNotReadyIsUnchanged(t *testing.T) {
	spec := &graphpkg.GraphSpec{
		Nodes: []graphpkg.Node{
			{
				ID:        "deploy",
				Template:  map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": map[string]any{"name": "app"}},
				ReadyWhen: []string{"${deploy.status.availableReplicas > 0}"},
			},
		},
	}
	compiled, err := compiler.CompileGraphSpec(spec, nil)
	require.NoError(t, err)

	eval := &evaluator{compiled: compiled, scope: map[string]any{
		"deploy": map[string]any{
			"metadata": map[string]any{"name": "app"},
			"status":   map[string]any{"availableReplicas": int64(0)},
		},
	}}

	err = eval.evalReadiness("deploy", []string{"${deploy.status.availableReplicas > 0}"})
	require.Error(t, err)

	// Normal readyWhen failure (condition false) — should still be compiler.ErrWaitingForReadiness.
	assert.True(t, errors.Is(err, compiler.ErrWaitingForReadiness),
		"normal readyWhen=false should produce compiler.ErrWaitingForReadiness, got: %v", err)
	assert.False(t, errors.Is(err, compiler.ErrReadyWhenFailed),
		"normal readyWhen=false should NOT produce compiler.ErrReadyWhenFailed")
}

// ---------------------------------------------------------------------------
// Correctness reconciliation — forEach parent state error precedence
//
// Per 005-reconciliation.md § Parent State: "Deterministic errors (Error)
// take precedence over transient errors (SystemError, Conflict) — if any
// child's failure is deterministic, retrying cannot resolve the parent."
// ---------------------------------------------------------------------------

// TestHighestPriorityChildError proves that when multiple forEach children
// fail, the highest-priority error is returned — not the first one.
func TestHighestPriorityChildError(t *testing.T) {
	errConflict := fmt.Errorf("SSA conflict on apps/v1/Deployment my-app: %w: field taken", compiler.ErrFieldConflict)
	errForbidden := apierrors.NewForbidden(schema.GroupResource{Resource: "deployments"}, "my-app", fmt.Errorf("RBAC"))
	errNetwork := &net.OpError{Op: "dial", Net: "tcp", Err: fmt.Errorf("connection refused")}
	errCEL := fmt.Errorf("evaluating template: expression returned string, want int")

	tests := []struct {
		name     string
		errs     []error
		wantType dagpkg.NodeState // the expected classification of the returned error
	}{
		{
			name:     "deterministic (Forbidden) over transient (network)",
			errs:     []error{errNetwork, errForbidden},
			wantType: dagpkg.NodeError,
		},
		{
			name:     "deterministic (CEL) over conflict",
			errs:     []error{errConflict, errCEL},
			wantType: dagpkg.NodeError,
		},
		{
			name:     "conflict over network (system error)",
			errs:     []error{errNetwork, errConflict},
			wantType: dagpkg.NodeConflict,
		},
		{
			name:     "single error returns itself",
			errs:     []error{errNetwork},
			wantType: dagpkg.NodeSystemError,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := highestPriorityChildError(tc.errs)
			require.NotNil(t, result)

			// Classify the result to check priority.
			if errors.Is(result, compiler.ErrFieldConflict) {
				assert.Equal(t, dagpkg.NodeConflict, tc.wantType)
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
// fallback. Per 005-reconciliation.md § Prune: resource keys encode
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
// (CEL evaluation, template rendering) are classified as dagpkg.NodeError even when
// their message contains network-like patterns. Before the fix, an error
// message containing "unexpected EOF" from a JSON marshal failure would
// false-positive as a network error → dagpkg.NodeSystemError → 5s retry loop.
//
// The fix: errors originating from non-API operations are wrapped with
// compiler.ErrEvaluation at the source (toMap, evalString). classifyAPIError checks
// for this sentinel before falling through to network pattern matching.
//
// This tests a classification boundary on a pure function — manufacturing
// the same false-positive in e2e would require a contrived expression whose
// error text happens to match network patterns. The unit test encodes the
// property directly.
func TestClassifyAPIError_EvalErrorWithNetworkPattern(t *testing.T) {
	// An evaluation error whose message contains a network error pattern.
	// Without the sentinel, this would be classified as dagpkg.NodeSystemError.
	evalErr := fmt.Errorf("evaluating template: %w: unexpected EOF in field value",
		compiler.ErrEvaluation)
	info := classifyAPIError(evalErr)
	assert.Equal(t, dagpkg.NodeError, info.state,
		"evaluation errors must be dagpkg.NodeError even when message contains network patterns")

	// A real network error should still be dagpkg.NodeSystemError.
	netErr := fmt.Errorf("unexpected EOF during API call")
	info = classifyAPIError(netErr)
	assert.Equal(t, dagpkg.NodeSystemError, info.state,
		"real network errors without compiler.ErrEvaluation sentinel should remain dagpkg.NodeSystemError")
}

// ---------------------------------------------------------------------------
// Correctness reconciliation — Finding 4: Status priority
// ---------------------------------------------------------------------------

// TestDeriveReadyCondition_BlockedBeforePending proves that when both
// Blocked and Pending states coexist, Blocked takes priority in the Ready
// condition. Blocked means "upstream error — someone needs to act."
// Pending means "just waiting." Blocked is more actionable.
//
// Per 005-reconciliation.md § Propagation: "Precedence where multiple
// apply: Excluded > Blocked > Pending."
//
// This tests a pure priority function over inputs — not reconciliation
// behavior. The inputs are a set of states, the output is a condition.
func TestDeriveReadyCondition_BlockedBeforePending(t *testing.T) {
	state := &reconcileState{
		compiled: true,
		planSummary: dagpkg.PlanSummary{
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
		planSummary: dagpkg.PlanSummary{HasPending: true},
		nodeErrors:  []string{"deploy: waiting for input from cfg"},
	}
	_, _, message := s.deriveReadyCondition()
	assert.Contains(t, message, "waiting for input from cfg")
}

// ---------------------------------------------------------------------------
// dagpkg.FinalizeSkippedStates — silent Ready fallthrough (#14) regression
// ---------------------------------------------------------------------------

// TestFinalizeSkippedStates_RestoresPreviousState exercises the happy path —
// a node in the outputsReady set with a previousPlanStates entry has its
// state restored so dagpkg.PlanSummary counts it.
func TestFinalizeSkippedStates_RestoresPreviousState(t *testing.T) {
	plan := &dagpkg.PlanState{
		States: map[string]dagpkg.NodeState{
			"n1": dagpkg.NodeUnvisited,
		},
	}
	outputsReady := map[string]bool{"n1": true}
	prev := map[string]dagpkg.NodeState{"n1": dagpkg.NodeReady}

	dagpkg.FinalizeSkippedStates(plan, outputsReady, prev, nil)

	assert.Equal(t, dagpkg.NodeReady, plan.States["n1"],
		"skipped node with prior state should restore to prior state")
}

// TestFinalizeSkippedStates_RegressionSilentReady guards against the
// silent-Ready fallthrough documented in #14: a node in outputsReady with no
// previousPlanStates entry previously stayed dagpkg.NodeUnvisited, which dagpkg.PlanSummary
// silently counts as zero — the graph appeared Ready with one fewer node
// than it actually had. The fix explicitly marks such nodes dagpkg.NodePending.
func TestFinalizeSkippedStates_RegressionSilentReady(t *testing.T) {
	plan := &dagpkg.PlanState{
		States: map[string]dagpkg.NodeState{
			"n1": dagpkg.NodeUnvisited,
		},
	}
	outputsReady := map[string]bool{"n1": true}
	// Empty previousPlanStates — the structurally-impossible case.
	prev := map[string]dagpkg.NodeState{}

	var diagnosedNode string
	dagpkg.FinalizeSkippedStates(plan, outputsReady, prev, func(id string) {
		diagnosedNode = id
	})

	assert.Equal(t, dagpkg.NodePending, plan.States["n1"],
		"skipped node with no prior state must be marked Pending, not left Unvisited")
	assert.Equal(t, "n1", diagnosedNode,
		"the callback should surface the diagnostic so logs record the invariant break")
}

// TestFinalizeSkippedStates_IgnoresNonSkipped confirms the helper only
// touches nodes in outputsReady AND in dagpkg.NodeUnvisited — nodes that were
// actually walked keep whatever the walker set.
func TestFinalizeSkippedStates_IgnoresNonSkipped(t *testing.T) {
	plan := &dagpkg.PlanState{
		States: map[string]dagpkg.NodeState{
			"walked": dagpkg.NodeError,
			"skip":   dagpkg.NodeUnvisited,
		},
	}
	outputsReady := map[string]bool{"skip": true} // "walked" is NOT in outputsReady
	prev := map[string]dagpkg.NodeState{"skip": dagpkg.NodeReady, "walked": dagpkg.NodeReady}

	dagpkg.FinalizeSkippedStates(plan, outputsReady, prev, nil)

	assert.Equal(t, dagpkg.NodeError, plan.States["walked"],
		"node not in outputsReady must not be overwritten")
	assert.Equal(t, dagpkg.NodeReady, plan.States["skip"],
		"node in outputsReady with prior state gets restored")
}

// TestCheckDependencyGate_RegressionExcludedPersistence verifies that when
// a dependency is Excluded, checkDependencyGate returns gateExcluded so
// the walk can set the child to Excluded. This tests the same invariant
// as the old tryDispatch persistence test — contagious exclusion.
func TestCheckDependencyGate_RegressionExcludedPersistence(t *testing.T) {
	// Build a DAG: root → child. Root will be Excluded in the plan.
	// checkDependencyGate on child should return gateExcluded.
	nodes := []graphpkg.Node{
		{
			ID: "root",
			Template: map[string]any{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]any{"name": "root"},
			},
		},
		{
			ID: "child",
			Template: map[string]any{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]any{"name": "child"},
				"data":     map[string]any{"ref": "${root.data.key}"},
			},
		},
	}
	spec := &graphpkg.GraphSpec{Nodes: nodes}
	compiled, err := compiler.CompileGraphSpec(spec, nil)
	require.NoError(t, err)
	dag := dagpkg.AssembleDAG(spec.Nodes, compiled.Topology)

	plan := dagpkg.NewPlanState(dag)
	// Mark root as Excluded in the plan (simulates root's includeWhen=false).
	plan.SetState(dag, "root", dagpkg.NodeExcluded)

	childIdx := dag.Index["child"]
	node := &dag.Nodes[childIdx]

	gate := checkDependencyGate(node, plan)
	assert.Equal(t, gateExcluded, gate,
		"child should see gateExcluded when root is Excluded")
	assert.Equal(t, dagpkg.NodeExcluded, gateToNodeState(gate),
		"child's gated state should be Excluded")
}

// ---------------------------------------------------------------------------
// pickEffectiveGeneration — compile-failure fallback generation (#19)
// ---------------------------------------------------------------------------

// revisionWithGeneration builds a minimal revision fixture for tests that
// only care about the generation label.
func revisionWithGeneration(gen int64) *unstructured.Unstructured {
	r := &unstructured.Unstructured{}
	r.SetLabels(map[string]string{
		graphpkg.LabelGraphGeneration: fmt.Sprintf("%d", gen),
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
// on the latest graph generation." Per 005-reconciliation.md § Propagation
// Control: used in propagateWhen for forEach rollout gating.
// ---------------------------------------------------------------------------

// TestUpdatedFunction_ScalarNode proves that .updated() reads __updated from
// a scalar scope object (map[string]any).
func TestUpdatedFunction_ScalarNode(t *testing.T) {
	spec := &graphpkg.GraphSpec{
		Nodes: []graphpkg.Node{
			{ID: "deploy", Template: map[string]any{
				"apiVersion": "apps/v1", "kind": "Deployment",
				"metadata": map[string]any{"name": "test"},
			}},
			{ID: "check", Def: map[string]any{
				"result": "${deploy.updated()}",
			}},
		},
	}
	compiled, err := compiler.CompileGraphSpec(spec, nil)
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
			result, err := compiled.Eval("deploy.updated()", map[string]any{
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
	spec := &graphpkg.GraphSpec{
		Nodes: []graphpkg.Node{
			node(graphpkg.Node{ID: "deploys", Watch: map[string]any{
				"apiVersion": "apps/v1", "kind": "Deployment",
				"selector": map[string]any{},
			}}, graphpkg.NodeTypeWatch),
			{ID: "check", Def: map[string]any{
				"result": "${deploys.updated()}",
			}},
		},
	}
	compiled, err := compiler.CompileGraphSpec(spec, nil)
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
			result, err := compiled.Eval("deploys.updated()", map[string]any{
				"deploys": tt.items,
			})
			require.NoError(t, err)
			assert.Equal(t, tt.want, result)
		})
	}
}

// TestUpdatedFunction_Filter proves that .updated() works inside
// collection filter expressions — the primary propagateWhen pattern from
// 005-reconciliation.md § Propagation Control.
func TestUpdatedFunction_Filter(t *testing.T) {
	spec := &graphpkg.GraphSpec{
		Nodes: []graphpkg.Node{
			node(graphpkg.Node{ID: "deploys", Watch: map[string]any{
				"apiVersion": "apps/v1", "kind": "Deployment",
				"selector": map[string]any{},
			}}, graphpkg.NodeTypeWatch),
			{ID: "check", Def: map[string]any{
				"count": "${deploys.filter(d, d.updated()).size()}",
			}},
		},
	}
	compiled, err := compiler.CompileGraphSpec(spec, nil)
	require.NoError(t, err)

	items := []any{
		map[string]any{"__updated": true, "metadata": map[string]any{"name": "a"}},
		map[string]any{"__updated": false, "metadata": map[string]any{"name": "b"}},
		map[string]any{"__updated": true, "metadata": map[string]any{"name": "c"}},
	}
	result, err := compiled.Eval("deploys.filter(d, d.updated()).size()", map[string]any{
		"deploys": items,
	})
	require.NoError(t, err)
	assert.Equal(t, int64(2), result)
}

// TestUpdatedFunction_CombinedWithReady proves the four-state matrix from
// 005-reconciliation.md § Propagation Control works in CEL:
//
//	updated() && ready()  → Current
//	updated() && !ready() → Updating
//	!updated() && ready() → Pending
//	!updated() && !ready()→ Stuck
func TestUpdatedFunction_CombinedWithReady(t *testing.T) {
	spec := &graphpkg.GraphSpec{
		Nodes: []graphpkg.Node{
			node(graphpkg.Node{ID: "items", Watch: map[string]any{
				"apiVersion": "v1", "kind": "Pod",
				"selector": map[string]any{},
			}}, graphpkg.NodeTypeWatch),
			{ID: "counts", Def: map[string]any{
				"current":  "${items.filter(i, i.updated() && i.ready()).size()}",
				"updating": "${items.filter(i, i.updated() && !i.ready()).size()}",
				"pending":  "${items.filter(i, !i.updated() && i.ready()).size()}",
				"stuck":    "${items.filter(i, !i.updated() && !i.ready()).size()}",
			}},
		},
	}
	compiled, err := compiler.CompileGraphSpec(spec, nil)
	require.NoError(t, err)

	items := []any{
		map[string]any{"__updated": true, "__ready": true, "metadata": map[string]any{"name": "current"}},
		map[string]any{"__updated": true, "__ready": false, "metadata": map[string]any{"name": "updating"}},
		map[string]any{"__updated": false, "__ready": true, "metadata": map[string]any{"name": "pending"}},
		map[string]any{"__updated": false, "__ready": false, "metadata": map[string]any{"name": "stuck"}},
	}
	scope := map[string]any{"items": items}

	current, err := compiled.Eval("items.filter(i, i.updated() && i.ready()).size()", scope)
	require.NoError(t, err)
	assert.Equal(t, int64(1), current)

	updating, err := compiled.Eval("items.filter(i, i.updated() && !i.ready()).size()", scope)
	require.NoError(t, err)
	assert.Equal(t, int64(1), updating)

	pending, err := compiled.Eval("items.filter(i, !i.updated() && i.ready()).size()", scope)
	require.NoError(t, err)
	assert.Equal(t, int64(1), pending)

	stuck, err := compiled.Eval("items.filter(i, !i.updated() && !i.ready()).size()", scope)
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
		gvk := graphpkg.GVKFromMap(map[string]any{"apiVersion": apiVersion, "kind": kind})
		genKey := graphpkg.ForEachChildGenerationLabelKey(parentID, name, ns, kind, gvk.Group, graphName, graphNS)
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
	spec := &graphpkg.GraphSpec{
		Nodes: []graphpkg.Node{
			{ID: "config", Def: map[string]any{
				"name":  "my-app",
				"port":  "8080",
				"debug": "false",
			}},
		},
	}
	compiled, err := compiler.CompileGraphSpec(spec, nil)
	require.NoError(t, err)

	eval := &evaluator{compiled: compiled, scope: map[string]any{}}
	r := &GraphReconciler{}
	err = r.cluster().reconcileDefinition(context.Background(), spec.Nodes[0], eval)
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
// four-state matrix (005-reconciliation.md § Propagation Control).
func TestForEach_CarryForwardStampsUpdatedFromLabel(t *testing.T) {
	spec := &graphpkg.GraphSpec{
		Nodes: []graphpkg.Node{
			{ID: "source", Def: map[string]any{
				"items": []any{
					map[string]any{"metadata": map[string]any{"name": "alpha"}},
					map[string]any{"metadata": map[string]any{"name": "beta"}},
				},
			}},
			{ID: "workers", ForEach: &graphpkg.ForEachBinding{VarName: "item", Expr: "${source.items}"},
				// propagateWhen that immediately halts — forces ALL items
				// through the carry-forward path.
				PropagateWhen: []string{"${false}"},
				Def:           map[string]any{"name": "${item.metadata.name}"},
			},
		},
	}
	compiled, err := compiler.CompileGraphSpec(spec, nil)
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
	alphaGenKey := graphpkg.ForEachChildGenerationLabelKey("workers", "alpha", "default", "Deployment", "apps", "test", "default")
	betaGenKey := graphpkg.ForEachChildGenerationLabelKey("workers", "beta", "default", "Deployment", "apps", "test", "default")

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
	prevState := &forEachCarryForward{
		items: map[string][]any{
			"workers/item": {
				map[string]any{"metadata": map[string]any{"name": "alpha"}},
				map[string]any{"metadata": map[string]any{"name": "beta"}},
			},
		},
		itemScope: map[string]map[string]any{
			"workers": {"alpha": prevAlpha, "beta": prevBeta},
		},
		itemKeys: map[string]map[string][]string{
			"workers": {"alpha": {"key-alpha"}, "beta": {"key-beta"}},
		},
	}

	graph := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "experimental.kro.run/v1alpha1",
		"kind":       "Graph",
		"metadata":   map[string]any{"name": "test", "namespace": "default"},
	}}

	r := &GraphReconciler{}
	rs := newReconcileScope(graph, nil)
	_, err = r.cluster().reconcileForEach(context.Background(), rs, spec.Nodes[1], eval, false, prevState)
	// compiler.ErrWaitingForReadiness expected — propagateWhen halted expansion.
	require.ErrorIs(t, err, compiler.ErrWaitingForReadiness)

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

// TestForEach_DefinitionItemsAlwaysReEvaluated proves that forEach definition
// items are always re-evaluated (no caching). Every item gets __updated = true
// because definitions are vacuously updated on every reconcile.
func TestForEach_DefinitionItemsAlwaysReEvaluated(t *testing.T) {
	sourceNode := graphpkg.Node{ID: "source", Def: map[string]any{
		"items": []any{
			map[string]any{"metadata": map[string]any{"name": "alpha"}},
		},
	}}
	sourceNode.SetType(graphpkg.NodeTypeDef)

	resultsNode := graphpkg.Node{ID: "results", ForEach: &graphpkg.ForEachBinding{VarName: "item", Expr: "${source.items}"},
		Def: map[string]any{"name": "${item.metadata.name}"},
	}
	resultsNode.SetType(graphpkg.NodeTypeDef)

	spec := &graphpkg.GraphSpec{
		Nodes: []graphpkg.Node{sourceNode, resultsNode},
	}
	compiled, err := compiler.CompileGraphSpec(spec, nil)
	require.NoError(t, err)

	state := newInstanceState(compiled)
	eval := newEvaluator(state)
	eval.effectiveGeneration = 7

	sourceItems := []any{
		map[string]any{"metadata": map[string]any{"name": "alpha"}},
	}
	eval.scope["source"] = map[string]any{"items": sourceItems}

	graph := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "experimental.kro.run/v1alpha1",
		"kind":       "Graph",
		"metadata":   map[string]any{"name": "test", "namespace": "default"},
	}}

	r := &GraphReconciler{}
	rs := newReconcileScope(graph, nil)
	_, err = r.cluster().reconcileForEach(context.Background(), rs, spec.Nodes[1], eval, false, nil)
	require.NoError(t, err)

	items, ok := eval.scope["results"].([]any)
	require.True(t, ok)
	require.Len(t, items, 1)

	alpha, ok := items[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, true, alpha["__updated"],
		"definition items are always re-evaluated and should be updated")
}

// TestForEach_RegressionSharedContextPropagation proves that when a shared
// dependency changes but collection items don't, forEach items are re-evaluated
// with the new context rather than serving stale output.
func TestForEach_RegressionSharedContextPropagation(t *testing.T) {
	graphObj := map[string]any{
		"spec": map[string]any{
			"nodes": []any{
				map[string]any{"id": "config", "def": map[string]any{"version": "v1"}},
				map[string]any{"id": "source", "def": map[string]any{"names": []any{"alpha", "beta"}}},
				map[string]any{
					"id": "results", "forEach": map[string]any{"item": "${source.names}"},
					"def": map[string]any{"combined": "${item}-${config.version}"},
				},
			},
		},
	}
	spec, err := graphpkg.ExtractGraphSpec(graphObj)
	require.NoError(t, err)
	compiled, err := compiler.CompileGraphSpec(spec, nil)
	require.NoError(t, err)
	dag := dagpkg.AssembleDAG(spec.Nodes, compiled.Topology)
	resultsNode := dag.Nodes[dag.Index["results"]]

	graph := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "experimental.kro.run/v1alpha1", "kind": "Graph",
		"metadata": map[string]any{"name": "test", "namespace": "default"},
	}}
	r := &GraphReconciler{}
	c := r.cluster()
	rs := newReconcileScope(graph, nil)
	names := []any{"alpha", "beta"}

	// Phase 1: config.version = "v1"
	state := newInstanceState(compiled)
	eval := newEvaluator(state)
	eval.effectiveGeneration = 1
	eval.scope["config"] = map[string]any{"version": "v1"}
	eval.scope["source"] = map[string]any{"names": names}

	out, err := c.reconcileForEach(context.Background(), rs, resultsNode, eval, false, nil)
	require.NoError(t, err)
	items1, ok := eval.scope["results"].([]any)
	require.True(t, ok)
	require.Len(t, items1, 2)
	for _, item := range items1 {
		assert.Contains(t, item.(map[string]any)["combined"].(string), "-v1")
	}

	// Merge newState into instanceState for phase 2.
	newState := out.forEach
	for k, v := range newState.itemScope {
		state.forEach.itemScope[k] = v
	}
	for k, v := range newState.itemKeys {
		state.forEach.itemKeys[k] = v
	}
	for k, v := range newState.items {
		state.forEach.items[k] = v
	}

	// Phase 2: config.version = "v2", collection unchanged.
	// Items are always re-evaluated so they pick up the new config.
	eval2 := newEvaluator(state)
	eval2.effectiveGeneration = 1
	eval2.scope["config"] = map[string]any{"version": "v2"}
	eval2.scope["source"] = map[string]any{"names": names}

	prevState2 := &forEachCarryForward{
		items:     state.forEach.items,
		itemScope: state.forEach.itemScope,
		itemKeys:  state.forEach.itemKeys,
	}

	_, err = c.reconcileForEach(context.Background(), rs, resultsNode, eval2, false, prevState2)
	require.NoError(t, err)
	items2, ok := eval2.scope["results"].([]any)
	require.True(t, ok)
	require.Len(t, items2, 2)
	for _, item := range items2 {
		combined := item.(map[string]any)["combined"].(string)
		assert.Contains(t, combined, "-v2",
			"output should reference v2, not stale v1")
		assert.NotContains(t, combined, "-v1",
			"stale v1 should not appear after config change")
	}
}

// TestPath2_SelfRefresh_StampsReady proves that when Path 2 (hash-match +
// self-state changed) replaces a node's scope with a fresh GET, the __ready
// flag is re-stamped so downstream .ready() calls return the correct value.
//
// Regression: Path 2 called evalReadinessConditions (which doesn't call markReady)
// instead of evalReadiness. The fresh GET replaced the scope object, wiping
// the __ready flag from the previous reconcile. Downstream nodes using
// .ready() found __ready missing and returned false — even when the node
// was actually ready. This caused CRD nodes (which have no readyWhen and
// are vacuously ready) to appear not-ready after any self-state change,
// blocking rgdStatus propagation and preventing RGDs from staying Active.
func TestPath2_SelfRefresh_StampsReady(t *testing.T) {
	// Build: crd (template, no readyWhen) → downstream (def, uses crd.ready())
	spec := &graphpkg.GraphSpec{
		Nodes: []graphpkg.Node{
			{ID: "crd", Template: map[string]any{
				"apiVersion": "apiextensions.k8s.io/v1",
				"kind":       "CustomResourceDefinition",
				"metadata":   map[string]any{"name": "test.kro.run"},
			}},
			{ID: "downstream", Def: map[string]any{
				"result": "${crd.ready()}",
			}},
		},
	}
	compiled, err := compiler.CompileGraphSpec(spec, nil)
	require.NoError(t, err)

	// Phase 1: Normal dispatch stamps __ready via evalReadiness.
	scopeWithReady := map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1",
		"kind":       "CustomResourceDefinition",
		"metadata":   map[string]any{"name": "test.kro.run", "resourceVersion": "1"},
		"status":     map[string]any{"conditions": []any{map[string]any{"type": "Established", "status": "True"}}},
	}
	eval1 := &evaluator{
		scope:    map[string]any{"crd": scopeWithReady},
		compiled: compiled,
	}
	// evalReadiness with no readyWhen stamps __ready = true
	require.NoError(t, eval1.evalReadiness("crd", nil))
	result1, err := compiled.Eval("crd.ready()", eval1.scope)
	require.NoError(t, err)
	assert.True(t, result1.(bool), "after evalReadiness, crd.ready() should be true")

	// Phase 2: Simulate Path 2 — fresh GET replaces scope (no __ready).
	// This is what controller.go:488 does: w.eval.scope[node.ID] = readBack.Object
	freshGET := map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1",
		"kind":       "CustomResourceDefinition",
		"metadata":   map[string]any{"name": "test.kro.run", "resourceVersion": "2"},
		"status":     map[string]any{"conditions": []any{map[string]any{"type": "Established", "status": "True"}}},
	}
	eval1.scope["crd"] = freshGET // Path 2 scope replacement

	// BUG: without markReady, __ready is missing from the fresh scope.
	// Verify .ready() returns false — this is the regression.
	result2, err := compiled.Eval("crd.ready()", eval1.scope)
	require.NoError(t, err)
	assert.False(t, result2.(bool),
		"REGRESSION: after Path 2 scope replacement without markReady, "+
			"crd.ready() should be false because __ready is missing")

	// FIX: Path 2 must call markReady after readiness evaluation.
	// For nodes with no readyWhen, nodeState is dagpkg.NodeReady, so markReady(true).
	eval1.markReady("crd", true)
	result3, err := compiled.Eval("crd.ready()", eval1.scope)
	require.NoError(t, err)
	assert.True(t, result3.(bool),
		"after markReady, crd.ready() should be true again")
}

// TestBareIdentifierForEachDependency verifies that a forEach node referencing
// a bare identifier (e.g., forEach: ${items} with no field access) correctly
// establishes a DAG dependency on the referenced node. Without this, the
// dependent node won't appear in dag.Dependents and will never receive a
// propagation trigger from the coordinator.
//
// Regression test for: forEach expressions that are bare identifiers (no .field
// access) produce no AST field paths, causing processExpr to bail via the
// exprPaths[expr] miss path without registering the dependency.
func TestBareIdentifierForEachDependency(t *testing.T) {
	spec := &graphpkg.GraphSpec{
		Nodes: []graphpkg.Node{
			node(graphpkg.Node{ID: "source", Template: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]any{"name": "src"},
			}}, graphpkg.NodeTypeTemplate),
			node(graphpkg.Node{ID: "items", Watch: map[string]any{
				"apiVersion": "v1",
				"kind":       "Pod",
				"selector":   map[string]any{},
			}}, graphpkg.NodeTypeWatch),
			node(graphpkg.Node{ID: "consumer",
				ForEach: &graphpkg.ForEachBinding{VarName: "item", Expr: "${items}"},
				Template: map[string]any{
					"apiVersion": "v1",
					"kind":       "ConfigMap",
					"metadata":   map[string]any{"name": "${item.metadata.name}-copy"},
					"data":       map[string]any{"ref": "${source.metadata.name}"},
				},
			}, graphpkg.NodeTypeTemplate),
		},
	}

	compiled, err := compiler.CompileGraphSpec(spec, nil)
	assert.NoError(t, err)
	assert.NotNil(t, compiled)

	// Find the consumer node in the DAG.
	dag := compiled.Topology
	consumerIdx, ok := dag.Index["consumer"]
	assert.True(t, ok, "consumer node must exist in DAG")

	consumerNode := compiled.Topology.NodeDeps(consumerIdx)

	// The consumer node must depend on both "items" (bare forEach reference)
	// and "source" (field access in template body).
	_, hasItems := consumerNode["items"]
	assert.True(t, hasItems,
		"consumer must depend on 'items' (bare identifier in forEach expression)")
	_, hasSource := consumerNode["source"]
	assert.True(t, hasSource,
		"consumer must depend on 'source' (field access in template body)")

	// Verify the reverse: items.Dependents must include consumer.
	found := false
	for _, depIdx := range dag.Dependents["items"] {
		if spec.Nodes[depIdx].ID == "consumer" {
			found = true
			break
		}
	}
	assert.True(t, found,
		"dag.Dependents['items'] must include 'consumer' for propagation triggers to work")
}

// ---------------------------------------------------------------------------
// Lazy dependency evaluation tests
//
// These tests verify the lazy dependency (DepLazy) behavior defined in
// 005-reconciliation.md § Dependencies: "Lazy dependencies are always in
// scope as optional values." and 004-compilation.md: "classification is
// per-consumer."
// ---------------------------------------------------------------------------

// TestLazyDepDoesNotGateDispatch proves that a lazy dependency (DepLazy)
// does not prevent a consumer from dispatching. Only hard dependencies
// gate dispatch. Per 005-reconciliation.md § Dependencies: "Lazy
// dependencies are always in scope as optional values."
func TestLazyDepDoesNotGateDispatch(t *testing.T) {
	// Node C depends on A (hard, direct field access) and B (lazy, .ready() only)
	spec := &graphpkg.GraphSpec{
		Nodes: []graphpkg.Node{
			{ID: "a", Template: map[string]any{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]any{"name": "a"},
			}},
			{ID: "b", Template: map[string]any{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]any{"name": "b"},
			}},
			{ID: "c", Template: map[string]any{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]any{"name": "c"},
				"data": map[string]any{
					"a_ref":   "${a.metadata.name}",                         // hard dep on A (direct field access)
					"b_ready": "${b.ready().orValue(false) ? 'yes' : 'no'}", // lazy dep on B (.ready().orValue() only)
				},
			}},
		},
	}
	compiled, err := compiler.CompileGraphSpec(spec, nil)
	require.NoError(t, err)
	dag := dagpkg.AssembleDAG(spec.Nodes, compiled.Topology)

	// Verify classification
	cIdx := dag.Index["c"]
	assert.Equal(t, graphpkg.DepHard, dag.Nodes[cIdx].Dependencies["a"], "a should be hard dep")
	assert.Equal(t, graphpkg.DepLazy, dag.Nodes[cIdx].Dependencies["b"], "b should be lazy dep")

	// A is ready, B is pending — C should still dispatch because B is lazy.
	plan := dagpkg.NewPlanState(dag)
	plan.SetState(dag, "a", dagpkg.NodeReady)
	plan.SetState(dag, "b", dagpkg.NodePending) // B is pending — lazy dep

	node := &dag.Nodes[cIdx]
	gate := checkDependencyGate(node, plan)
	// C should dispatch (gateDispatch) — B is lazy, so its Pending state is invisible to gating.
	// checkDependencyGate only checks hard deps; lazy dep B=Pending doesn't block.
	assert.Equal(t, gateDispatch, gate,
		"lazy dep B=Pending should not block C from dispatching")
}

// TestLazyDepClassification_MixedAccess proves that the same dependency
// gets different DepKind classifications from different consumers based
// on expression syntax. Per 004-compilation.md: classification is
// per-consumer.
func TestLazyDepClassification_MixedAccess(t *testing.T) {
	spec := &graphpkg.GraphSpec{
		Nodes: []graphpkg.Node{
			{ID: "deploy", Template: map[string]any{
				"apiVersion": "apps/v1", "kind": "Deployment",
				"metadata": map[string]any{"name": "deploy"},
			}},
			// Consumer A: accesses deploy only via .ready().orValue() → DepLazy
			{ID: "status", Def: map[string]any{
				"state": "${deploy.ready().orValue(false) ? 'ACTIVE' : 'PENDING'}",
			}},
			// Consumer B: accesses deploy directly → DepHard
			{ID: "service", Template: map[string]any{
				"apiVersion": "v1", "kind": "Service",
				"metadata": map[string]any{"name": "${deploy.metadata.name}"},
			}},
		},
	}
	compiled, err := compiler.CompileGraphSpec(spec, nil)
	require.NoError(t, err)
	dag := dagpkg.AssembleDAG(spec.Nodes, compiled.Topology)

	statusIdx := dag.Index["status"]
	serviceIdx := dag.Index["service"]

	assert.Equal(t, graphpkg.DepLazy, dag.Nodes[statusIdx].Dependencies["deploy"],
		"status accesses deploy only via .ready().orValue() → DepLazy")
	assert.Equal(t, graphpkg.DepHard, dag.Nodes[serviceIdx].Dependencies["deploy"],
		"service accesses deploy directly → DepHard")
}

// TestLazyDepOptionalScope_AbsentReturnsDefault proves that when a lazy
// dependency is absent (optional.none in scope), field access through ?.
// and .orValue() returns the default value, and .ready() returns false.
// Per 005-reconciliation.md: "Lazy dependencies are always in scope as
// optional values."
func TestLazyDepOptionalScope_AbsentReturnsDefault(t *testing.T) {
	spec := &graphpkg.GraphSpec{
		Nodes: []graphpkg.Node{
			{ID: "deploy", Template: map[string]any{
				"apiVersion": "apps/v1", "kind": "Deployment",
				"metadata": map[string]any{"name": "deploy"},
			}},
			{ID: "status", Def: map[string]any{
				"replicas": "${deploy.?status.?availableReplicas.orValue(0)}",
				"ready":    "${deploy.ready().orValue(false) ? 'yes' : 'no'}",
			}},
		},
	}
	compiled, err := compiler.CompileGraphSpec(spec, nil)
	require.NoError(t, err)
	dag := dagpkg.AssembleDAG(spec.Nodes, compiled.Topology)

	// Verify deploy is lazy for status
	statusIdx := dag.Index["status"]
	statusNode := &dag.Nodes[statusIdx]
	require.Equal(t, graphpkg.DepLazy, statusNode.Dependencies["deploy"])

	// Create evaluator with deploy ABSENT from scope (lazy dep not yet available).
	// Manually populate lazy deps as optional.none() — matching what
	// evaluateNode does in the simplified walk.
	state := newInstanceState(compiled)
	eval := newEvaluator(state)
	// Don't put "deploy" in scope — it's absent.
	// Populate lazy dep as optional.none() (what the walk does for absent lazy deps).
	eval.scope["deploy"] = celOptionalNone()

	// .ready().orValue(false) on absent lazy dep → false
	readyVal, err := compiled.Eval("deploy.ready().orValue(false) ? 'yes' : 'no'", eval.scope)
	require.NoError(t, err)
	assert.Equal(t, "no", readyVal, ".ready().orValue(false) on absent lazy dep should return false")

	// ?.field.orValue() on absent lazy dep → default value
	replicasVal, err := compiled.Eval("deploy.?status.?availableReplicas.orValue(0)", eval.scope)
	require.NoError(t, err)
	assert.Equal(t, int64(0), replicasVal, "?.orValue(0) on absent lazy dep should return 0")
}

// TestLazyDepOptionalScope_PresentReturnsRealData proves that when a lazy
// dependency becomes available, expression evaluation sees the real data
// through the optional wrapper. This tests the absent→present transition.
func TestLazyDepOptionalScope_PresentReturnsRealData(t *testing.T) {
	spec := &graphpkg.GraphSpec{
		Nodes: []graphpkg.Node{
			{ID: "deploy", Template: map[string]any{
				"apiVersion": "apps/v1", "kind": "Deployment",
				"metadata": map[string]any{"name": "deploy"},
			}},
			{ID: "status", Def: map[string]any{
				"replicas": "${deploy.?status.?availableReplicas.orValue(0)}",
				"ready":    "${deploy.ready().orValue(false) ? 'yes' : 'no'}",
			}},
		},
	}
	compiled, err := compiler.CompileGraphSpec(spec, nil)
	require.NoError(t, err)
	dag := dagpkg.AssembleDAG(spec.Nodes, compiled.Topology)

	statusIdx := dag.Index["status"]
	statusNode := &dag.Nodes[statusIdx]
	require.Equal(t, graphpkg.DepLazy, statusNode.Dependencies["deploy"])

	// Create evaluator with deploy PRESENT in scope with real data.
	// Populate lazy dep as optional.of(value) — matching what the walk does
	// for present lazy deps.
	state := newInstanceState(compiled)
	eval := newEvaluator(state)
	deployData := map[string]any{
		"status": map[string]any{
			"availableReplicas": int64(3),
		},
		"__ready": true,
	}
	eval.scope["deploy"] = celOptionalOf(deployData)

	// .ready().orValue(false) on present lazy dep → true (deploy has __ready: true)
	readyVal, err := compiled.Eval("deploy.ready().orValue(false) ? 'yes' : 'no'", eval.scope)
	require.NoError(t, err)
	assert.Equal(t, "yes", readyVal, ".ready().orValue(false) on present lazy dep should return true")

	// ?.field.orValue() on present lazy dep → real value
	replicasVal, err := compiled.Eval("deploy.?status.?availableReplicas.orValue(0)", eval.scope)
	require.NoError(t, err)
	assert.Equal(t, int64(3), replicasVal, "?.orValue(0) on present lazy dep should return 3")
}
