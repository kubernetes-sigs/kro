package graphcontroller

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// ---------------------------------------------------------------------------
// Correctness reconciliation — regression tests
// ---------------------------------------------------------------------------

// TestGVRKindFallback_RegressionIrregularPlurals proves that the
// Kind fallback in gvrKindFromInformer handles irregular plurals correctly.
// Before the fix, "networkpolicies" would produce "Networkpolicie" (garbage).
// After the fix, it produces "Networkpolicy" (correct singular, lowercase).
//
// CamelCase (NetworkPolicy vs Networkpolicy) cannot be reconstructed from
// lowercase resource names — there are no word boundaries in "networkpolicy".
// The primary path (PartialObjectMetadata.Kind) always provides the correct
// CamelCase Kind. This fallback only fires when Kind is empty, which is not
// expected in practice with metadata informers.
func TestGVRKindFallback_RegressionIrregularPlurals(t *testing.T) {
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

// TestWatchManagerKindFor proves that WatchManager.KindFor returns the
// canonical CamelCase Kind previously registered via ensureWatch's kind
// parameter, and "" for unknown GVRs.
func TestWatchManagerKindFor(t *testing.T) {
	wm := newWatchManager(nil, 0, func(watchEvent) {}, logr.Discard())

	gvr := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}

	// Before setting: should return "".
	assert.Equal(t, "", wm.KindFor(gvr), "unknown GVR should return empty string")

	// Set the kind directly.
	wm.mu.Lock()
	wm.gvrKinds[gvr] = "ConfigMap"
	wm.mu.Unlock()

	assert.Equal(t, "ConfigMap", wm.KindFor(gvr), "KindFor should return the registered canonical Kind")
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

// ---------------------------------------------------------------------------
// mergeCollectionChanges — input isolation
// ---------------------------------------------------------------------------

// TestMergeCollectionChanges_InputNotMutated proves that mergeCollectionChanges
// does not mutate its cached input slice. This defends the encapsulation: all
// mutation happens on an internally-owned copy. If someone changes the
// function to operate in-place, this test fails.
func TestMergeCollectionChanges_InputNotMutated(t *testing.T) {
	a := map[string]any{"metadata": map[string]any{"name": "a", "namespace": "ns"}, "data": map[string]any{"v": "1"}}
	b := map[string]any{"metadata": map[string]any{"name": "b", "namespace": "ns"}, "data": map[string]any{"v": "2"}}
	c := map[string]any{"metadata": map[string]any{"name": "c", "namespace": "ns"}, "data": map[string]any{"v": "3"}}
	cached := []any{a, b, c}

	// Delete the middle item. This is the path that previously used
	// items[:0] in-place filtering and would corrupt the input.
	result, err := mergeCollectionChanges(
		context.Background(), nil, cached,
		[]CollectionChange{{Namespace: "ns", Name: "b", EventType: WatchEventDelete}},
		schema.GroupVersionKind{}, nil,
	)
	require.NoError(t, err)

	// Result has 2 items, input still has 3 — unchanged.
	assert.Len(t, result, 2, "result should have item removed")
	require.Len(t, cached, 3, "input slice length must not change")
	assert.Equal(t, a, cached[0].(map[string]any), "input[0] must be original 'a'")
	assert.Equal(t, b, cached[1].(map[string]any), "input[1] must be original 'b'")
	assert.Equal(t, c, cached[2].(map[string]any), "input[2] must be original 'c'")
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
	spec, err := extractGraphSpec(graphObj)
	require.NoError(t, err)
	compiled, err := compileGraphSpec(spec, nil)
	require.NoError(t, err)
	resultsNode := compiled.dag.Nodes[compiled.dag.Index["results"]]

	graph := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "experimental.kro.run/v1alpha1", "kind": "Graph",
		"metadata": map[string]any{"name": "test", "namespace": "default"},
	}}
	r := &GraphReconciler{}
	names := []any{"alpha", "beta"}

	// Phase 1: config.version = "v1"
	state := newInstanceState(compiled)
	eval := newEvaluator(state)
	eval.effectiveGeneration = 1
	eval.scope["config"] = map[string]any{"version": "v1"}
	eval.scope["source"] = map[string]any{"names": names}
	eval.forEachNewScope = map[string]map[string]any{}
	eval.forEachNewKeys = map[string]map[string][]string{}
	eval.forEachNewItems = map[string][]any{}
	eval.forEachNewHashes = map[string]map[string]string{}

	_, err = r.reconcileForEach(context.Background(), graph, resultsNode, eval, nil, false)
	require.NoError(t, err)
	items1, ok := eval.scope["results"].([]any)
	require.True(t, ok)
	require.Len(t, items1, 2)
	for _, item := range items1 {
		assert.Contains(t, item.(map[string]any)["combined"].(string), "-v1")
	}

	for k, v := range eval.forEachNewScope {
		state.forEachItemScope[k] = v
	}
	for k, v := range eval.forEachNewKeys {
		state.forEachItemKeys[k] = v
	}
	for k, v := range eval.forEachNewHashes {
		state.forEachItemHashes[k] = v
	}
	for k, v := range eval.forEachNewItems {
		state.forEachItems[k] = v
	}

	// Phase 2: config.version = "v2", collection unchanged
	eval2 := newEvaluator(state)
	eval2.effectiveGeneration = 1
	eval2.scope["config"] = map[string]any{"version": "v2"}
	eval2.scope["source"] = map[string]any{"names": names}
	eval2.forEachNewScope = map[string]map[string]any{}
	eval2.forEachNewKeys = map[string]map[string][]string{}
	eval2.forEachNewItems = map[string][]any{}
	eval2.forEachNewHashes = map[string]map[string]string{}

	_, err = r.reconcileForEach(context.Background(), graph, resultsNode, eval2, nil, false)
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
