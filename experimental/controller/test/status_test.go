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
						"template": map[string]any{
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

// TestRevisionConditionsHaveLastTransitionTime proves that GraphRevision
// conditions include lastTransitionTime, matching the Kubernetes condition
// convention (design 001-graph § Conditions, 002-revisions § Status).
//
// Bug: setRevisionCondition did not set lastTransitionTime at all, producing
// conditions without the field. This breaks kubectl wait --for=condition=...
// timeout reasoning and any monitoring that computes "time in state."
func TestRevisionConditionsHaveLastTransitionTime(t *testing.T) {
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

	// Wait a moment for at least one reconcile to fire (drift timer or watch event).
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
