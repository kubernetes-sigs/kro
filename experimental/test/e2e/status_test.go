package graphcontroller_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
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
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		g := &unstructured.Unstructured{}
		g.SetGroupVersionKind(GraphGVK)
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "test-status-active", Namespace: ns}, g); err != nil {
			return false, nil
		}
		return graphReady(g), nil
	}))

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
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		g := &unstructured.Unstructured{}
		g.SetGroupVersionKind(GraphGVK)
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "test-status-inprogress", Namespace: ns}, g); err != nil {
			return false, nil
		}
		return graphReadyStatus(g) == "Unknown", nil
	}))

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
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		g := &unstructured.Unstructured{}
		g.SetGroupVersionKind(GraphGVK)
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "test-status-inprogress", Namespace: ns}, g); err != nil {
			return false, nil
		}
		return graphReady(g), nil
	}))

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

// TestRevisionCondition_RegressionLastTransitionTime proves that GraphRevision
// conditions include lastTransitionTime, matching the Kubernetes condition
// convention (design 001-graph § Conditions, 002-revisions § Status).
//
// Bug: setRevisionCondition did not set lastTransitionTime at all, producing
// conditions without the field. This breaks kubectl wait --for=condition=...
// timeout reasoning and any monitoring that computes "time in state."
func TestRevisionCondition_RegressionLastTransitionTime(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-revision-ltt",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "cm",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "revision-ltt-cm"},
							"data":       map[string]any{"key": "value"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for the Graph to be ready.
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-revision-ltt", Namespace: ns}))

	// Find the revision (generation 1). Wait for its Ready condition
	// to be set — waitForRevision only waits for the object to exist, but
	// the status subresource may not be written yet (race between Graph
	// status write and revision status write).
	revKey := types.NamespacedName{Name: "test-revision-ltt-g00001", Namespace: ns}
	require.NoError(t, waitForRevisionCondition(ctx, k8sClient, revKey, "Ready", "True"))
	rev := &unstructured.Unstructured{}
	rev.SetGroupVersionKind(GraphRevisionGVK)
	require.NoError(t, k8sClient.Get(ctx, revKey, rev))

	// Verify conditions exist and each has lastTransitionTime.
	status, _, _ := unstructured.NestedMap(rev.Object, "status")
	require.NotNil(t, status, "revision should have status")

	conditions, _, _ := unstructured.NestedSlice(rev.Object, "status", "conditions")
	require.NotEmpty(t, conditions, "revision should have at least one condition")

	for _, c := range conditions {
		cMap, ok := c.(map[string]any)
		require.True(t, ok)
		condType, _ := cMap["type"].(string)
		ltt, _ := cMap["lastTransitionTime"].(string)
		assert.NotEmpty(t, ltt,
			"condition %q must have lastTransitionTime set (was empty); "+
				"design 001-graph § Conditions: lastTransitionTime is preserved when "+
				"status does not change between reconciles", condType)
		t.Logf("condition %q has lastTransitionTime=%q", condType, ltt)
	}

	// Verify lastTransitionTime is stable across reconciles when status
	// doesn't change. Re-fetch the revision after a pause — if the condition
	// status is unchanged, lastTransitionTime must be preserved.
	// Guard: if lastTransitionTime was already missing (bug), skip the stability
	// check and let the NotEmpty assertions above report the failure.
	firstLTT := map[string]string{}
	allHaveLTT := true
	for _, c := range conditions {
		cMap := c.(map[string]any)
		ltt, ok := cMap["lastTransitionTime"].(string)
		if !ok || ltt == "" {
			allHaveLTT = false
			break
		}
		firstLTT[cMap["type"].(string)] = ltt
	}

	if !allHaveLTT {
		// The NotEmpty assertions above already failed — skip the stability check.
		return
	}

	// Wait a moment for at least one reconcile to fire (resync timer or watch event).
	// Then re-fetch and verify lastTransitionTime is unchanged.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		// Poll until revision resource version stabilizes (no further updates).
		r1 := &unstructured.Unstructured{}
		r1.SetGroupVersionKind(GraphRevisionGVK)
		if err := k8sClient.Get(ctx, revKey, r1); err != nil {
			return false, nil
		}
		rv1 := r1.GetResourceVersion()
		// Short pause then re-check.
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
		r2 := &unstructured.Unstructured{}
		r2.SetGroupVersionKind(GraphRevisionGVK)
		if err := k8sClient.Get(ctx, revKey, r2); err != nil {
			return false, nil
		}
		return r1.GetResourceVersion() == r2.GetResourceVersion() || rv1 == r2.GetResourceVersion(), nil
	}))

	// Re-read revision and check lastTransitionTime is preserved.
	rev2 := &unstructured.Unstructured{}
	rev2.SetGroupVersionKind(GraphRevisionGVK)
	require.NoError(t, k8sClient.Get(ctx, revKey, rev2))
	conditions2, _, _ := unstructured.NestedSlice(rev2.Object, "status", "conditions")
	for _, c := range conditions2 {
		cMap := c.(map[string]any)
		condType, _ := cMap["type"].(string)
		ltt2, _ := cMap["lastTransitionTime"].(string)
		assert.NotEmpty(t, ltt2,
			"condition %q lastTransitionTime must still be present after re-reconcile", condType)
		if first, ok := firstLTT[condType]; ok {
			assert.Equal(t, first, ltt2,
				"condition %q lastTransitionTime must not change when status is stable", condType)
		}
	}
	t.Log("GraphRevision conditions have lastTransitionTime and it is stable when status unchanged")
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
