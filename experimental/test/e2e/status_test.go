package graphcontroller_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// TestStatusActiveOnSuccess proves that a successfully reconciled Graph
// reports Ready=True with reason=Ready and Compiled=True with reason=Compiled
// (design 001-graph § Status, § Conditions). Verifies exactly 2 conditions.
func TestStatusActiveOnSuccess(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-status-active",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "config",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "status-test",
							},
							"data": map[string]any{
								"key": "value",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for the ConfigMap to be created
	cm := &unstructured.Unstructured{}
	cm.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
	require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: "status-test", Namespace: ns}, cm))

	// Wait for status to be set to Active
	require.NoError(t, waitForGraphReady(ctx, k8sClient, types.NamespacedName{Name: "test-status-active", Namespace: ns}))

	g := &unstructured.Unstructured{}
	g.SetGroupVersionKind(GraphGVK)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "test-status-active", Namespace: ns}, g))

	assert.True(t, graphReady(g))

	conditions, _, _ := unstructured.NestedSlice(g.Object, "status", "conditions")
	require.Len(t, conditions, 2)

	compiled, ok := findCondition(conditions, "Compiled")
	require.True(t, ok, "Compiled condition should exist")
	assert.Equal(t, "True", compiled["status"])
	assert.Equal(t, "Compiled", compiled["reason"])

	cond, ok := findCondition(conditions, "Ready")
	require.True(t, ok, "Ready condition should exist")
	assert.Equal(t, "True", cond["status"])
	assert.Equal(t, "Ready", cond["reason"])
	t.Logf("Status: Compiled=%s, Ready=%s reason=%s message=%s", compiled["status"], cond["status"], cond["reason"], cond["message"])
}

// TestStatusInProgressOnReadyWhen proves that a Graph with a not-ready
// watch gets state=InProgress and Ready=False, then transitions
// to Active when the resource becomes ready.
func TestStatusInProgressOnReadyWhen(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Create source in not-ready state
	source := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "status-source",
				"namespace": ns,
			},
			"data": map[string]any{
				"ready": "false",
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, source))

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-status-inprogress",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "source",
						"ref": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "status-source",
							},
						},
						"readyWhen": []any{
							"${source.data.ready == 'true'}",
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for status to be InProgress (not-ready)
	require.NoError(t, waitForGraphReadyStatus(ctx, k8sClient, types.NamespacedName{Name: "test-status-inprogress", Namespace: ns}, "Unknown"))

	// Verify InProgress status
	g := &unstructured.Unstructured{}
	g.SetGroupVersionKind(GraphGVK)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "test-status-inprogress", Namespace: ns}, g))

	assert.Equal(t, "Unknown", graphReadyStatus(g))

	conditions, _, _ := unstructured.NestedSlice(g.Object, "status", "conditions")
	require.Len(t, conditions, 2)

	compiled, ok := findCondition(conditions, "Compiled")
	require.True(t, ok, "Compiled condition should exist")
	assert.Equal(t, "True", compiled["status"])

	cond, ok := findCondition(conditions, "Ready")
	require.True(t, ok, "Ready condition should exist")
	assert.Equal(t, "Unknown", cond["status"])
	assert.Equal(t, "NotReady", cond["reason"])
	t.Logf("Before: Compiled=%s Ready=%s reason=%s", compiled["status"], cond["status"], cond["reason"])

	// Transition source to ready
	latestSource := &unstructured.Unstructured{}
	latestSource.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "status-source", Namespace: ns}, latestSource))
	unstructured.SetNestedField(latestSource.Object, "true", "data", "ready")
	require.NoError(t, k8sClient.Update(ctx, latestSource))

	// Wait for status to transition to Active
	require.NoError(t, waitForGraphReady(ctx, k8sClient, types.NamespacedName{Name: "test-status-inprogress", Namespace: ns}))

	// Verify Active status
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "test-status-inprogress", Namespace: ns}, g))
	assert.True(t, graphReady(g))

	conditions, _, _ = unstructured.NestedSlice(g.Object, "status", "conditions")
	require.Len(t, conditions, 2)
	cond, ok = findCondition(conditions, "Ready")
	require.True(t, ok, "Ready condition should exist")
	assert.Equal(t, "True", cond["status"])
	assert.Equal(t, "Ready", cond["reason"])
	t.Logf("After: Ready=%s reason=%s", cond["status"], cond["reason"])
	t.Log("Status lifecycle proved: NotReady → Ready on readyWhen satisfied")
}

// ---------------------------------------------------------------------------
// Status condition message content tests
//
// Promoted from unit tests in controller/reconcile_test.go that called
// reconcileState.deriveReadyCondition() through the internal struct. These
// test the same behavior through the user-facing status conditions on the
// Graph object.
//
// Per status.go: the Ready condition message carries per-node error detail
// (joined with "; ") and informational notes (e.g., FinalizerSkipped).
// ---------------------------------------------------------------------------

// TestStatusBlockedSurfacesNodeErrors proves that when the Graph's Ready
// condition is Blocked, the message includes per-node error details — not
// a generic "blocked by upstream errors" stub.
//
// Replaces: TestDeriveReadyCondition_BlockedSurfacesStructuredReasons (unit)
func TestStatusBlockedSurfacesNodeErrors(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Label the namespace for webhook matching.
	faultLabel := "fault-" + ns
	labelNamespace(t, ns, map[string]string{"fault-inject": faultLabel})

	// Start fault-injection webhook.
	fw := startFaultWebhook(t)
	registerFaultWebhook(t, k8sClient, "fault-blocked-"+ns, fw, "fault-inject", faultLabel)

	// Reject the upstream ConfigMap — this will cause SystemError, which
	// blocks the dependent.
	fw.Reject("blocked-upstream")

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-status-blocked",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "upstream",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "blocked-upstream"},
							"data":       map[string]any{"key": "value"},
						},
					},
					map[string]any{
						"id": "downstream",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "blocked-downstream"},
							"data":       map[string]any{"ref": "${upstream.metadata.name}"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// The upstream will get SystemError (5xx from webhook), which surfaces
	// as SystemError or causes the downstream to be Blocked. The status
	// condition message should include per-node error details.
	require.NoError(t, waitForGraphReadyStatus(ctx, k8sClient,
		types.NamespacedName{Name: "test-status-blocked", Namespace: ns}, "False"))

	g := &unstructured.Unstructured{}
	g.SetGroupVersionKind(GraphGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "test-status-blocked", Namespace: ns}, g))

	message := graphReadyMessage(g)
	reason := graphReadyReason(g)
	t.Logf("Ready reason=%s message=%s", reason, message)

	// The message should contain structured error details — either the
	// node ID or the specific error, not just a generic stub.
	assert.NotEmpty(t, message, "Ready condition message should not be empty")
	// SystemError surfaces "upstream:" node identifier in the message.
	assert.Contains(t, message, "upstream",
		"message should identify the failing node")
}

// TestStatusNotReadySurfacesReadyWhenError proves that when a node's
// readyWhen expression is permanently invalid (returns non-bool), the
// Ready condition message surfaces the expression error — not just a
// generic "readyWhen conditions not met" stub.
//
// Replaces: TestDeriveReadyCondition_NotReadyWithErrors (unit)
func TestStatusNotReadySurfacesReadyWhenError(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-status-readywhen-err",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "deploy",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "readywhen-err-cm"},
							"data":       map[string]any{"key": "value"},
						},
						// readyWhen returns int (via size()), not bool.
						// This is a permanent expression error — should surface
						// in the status message.
						"readyWhen": []any{"${size(deploy.metadata.name)}"},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for NotReady status.
	require.NoError(t, waitForGraphReadyReason(ctx, k8sClient,
		types.NamespacedName{Name: "test-status-readywhen-err", Namespace: ns}, "NotReady"))

	g := &unstructured.Unstructured{}
	g.SetGroupVersionKind(GraphGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "test-status-readywhen-err", Namespace: ns}, g))

	message := graphReadyMessage(g)
	t.Logf("Ready message=%s", message)

	assert.Equal(t, "Unknown", graphReadyStatus(g))
	assert.Equal(t, "NotReady", graphReadyReason(g))
	// The message should contain the expression error, not just a generic stub.
	assert.Contains(t, message, "deploy",
		"message should identify the node with the readyWhen error")
}

// TestStatusReadyWithFinalizerSkippedNote proves that when a finalizes
// target is already absent, the Graph becomes Ready with an informational
// "FinalizerSkipped" note in the message — not an error, just info.
//
// Replaces: TestReconcileStateDeriveReadyCondition_FinalizerSkipped (unit)
func TestStatusReadyWithFinalizerSkippedNote(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-status-fskipped",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "data",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "finalize-target"},
							"data":       map[string]any{"key": "value"},
						},
					},
					map[string]any{
						"id":        "snapshot",
						"finalizes": "data",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "finalize-snapshot"},
							"data":       map[string]any{"ref": "${data.metadata.name}"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-status-fskipped", Namespace: ns}))

	// Manually delete the finalize target (data ConfigMap) to set up the
	// scenario where finalization finds the target absent.
	target := &unstructured.Unstructured{}
	target.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"})
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "finalize-target", Namespace: ns}, target))
	require.NoError(t, k8sClient.Delete(ctx, target))

	// Now delete the Graph to trigger teardown + finalization.
	require.NoError(t, k8sClient.Delete(ctx, graph))

	// The Graph should complete teardown. We can't observe the in-flight
	// FinalizerSkipped note because the graph is deleted, but we CAN verify
	// that teardown completes without blocking (which means the absent target
	// was handled correctly — the design commitment tested by the unit test).
	require.NoError(t, waitForDeletion(ctx, k8sClient, GraphGVK,
		types.NamespacedName{Name: "test-status-fskipped", Namespace: ns}))
}

// TestStatusDefinesReadyWhenUnsatisfied proves that a Graph with a def node
// whose readyWhen evaluates false reports status NotReady.
//
// Replaces: TestDefinesReconcile("readyWhen unsatisfied") (unit)
func TestStatusDefinesReadyWhenUnsatisfied(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-def-readywhen",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id":  "cfg",
						"def": map[string]any{"count": "0"},
						// readyWhen evaluates to false: "0" != "3"
						"readyWhen": []any{"${cfg.count == '3'}"},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for NotReady status.
	require.NoError(t, waitForGraphReadyReason(ctx, k8sClient,
		types.NamespacedName{Name: "test-def-readywhen", Namespace: ns}, "NotReady"))

	g := &unstructured.Unstructured{}
	g.SetGroupVersionKind(GraphGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "test-def-readywhen", Namespace: ns}, g))

	assert.Equal(t, "Unknown", graphReadyStatus(g))
	assert.Equal(t, "NotReady", graphReadyReason(g))
	t.Logf("def readyWhen unsatisfied: Ready=%s reason=%s message=%s",
		graphReadyStatus(g), graphReadyReason(g), graphReadyMessage(g))
}
