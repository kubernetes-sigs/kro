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
// reports Ready=True with reason=Ready and Accepted=True with reason=Accepted
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

	accepted, ok := findCondition(conditions, "Accepted")
	require.True(t, ok, "Accepted condition should exist")
	assert.Equal(t, "True", accepted["status"])
	assert.Equal(t, "Accepted", accepted["reason"])

	cond, ok := findCondition(conditions, "Ready")
	require.True(t, ok, "Ready condition should exist")
	assert.Equal(t, "True", cond["status"])
	assert.Equal(t, "Ready", cond["reason"])
	t.Logf("Status: Accepted=%s, Ready=%s reason=%s message=%s", accepted["status"], cond["status"], cond["reason"], cond["message"])
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

	accepted, ok := findCondition(conditions, "Accepted")
	require.True(t, ok, "Accepted condition should exist")
	assert.Equal(t, "True", accepted["status"])

	cond, ok := findCondition(conditions, "Ready")
	require.True(t, ok, "Ready condition should exist")
	assert.Equal(t, "Unknown", cond["status"])
	assert.Equal(t, "NotReady", cond["reason"])
	t.Logf("Before: Accepted=%s Ready=%s reason=%s", accepted["status"], cond["status"], cond["reason"])

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
