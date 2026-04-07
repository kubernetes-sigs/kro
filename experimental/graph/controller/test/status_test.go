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

func TestStatusActiveOnSuccess(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-status-active",
				"namespace": ns,
			},
			"spec": map[string]any{
				"resources": []any{
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
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		g := &unstructured.Unstructured{}
		g.SetGroupVersionKind(GraphGVK)
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "test-status-active", Namespace: ns}, g); err != nil {
			return false, nil
		}
		state, _, _ := unstructured.NestedString(g.Object, "status", "state")
		return state == "Active", nil
	}))

	// Verify status
	g := &unstructured.Unstructured{}
	g.SetGroupVersionKind(GraphGVK)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "test-status-active", Namespace: ns}, g))

	state, _, _ := unstructured.NestedString(g.Object, "status", "state")
	assert.Equal(t, "Active", state)

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
	t.Logf("Status: state=%s, Accepted=%s, Ready=%s reason=%s message=%s", state, accepted["status"], cond["status"], cond["reason"], cond["message"])
}

// TestStatusInProgressOnReadyWhen proves that a Graph with a not-ready
// externalRef gets state=InProgress and Ready=False, then transitions
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
			"apiVersion": "kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-status-inprogress",
				"namespace": ns,
			},
			"spec": map[string]any{
				"resources": []any{
					map[string]any{
						"id": "source",
						"externalRef": map[string]any{
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
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		g := &unstructured.Unstructured{}
		g.SetGroupVersionKind(GraphGVK)
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "test-status-inprogress", Namespace: ns}, g); err != nil {
			return false, nil
		}
		state, _, _ := unstructured.NestedString(g.Object, "status", "state")
		return state == "InProgress", nil
	}))

	// Verify InProgress status
	g := &unstructured.Unstructured{}
	g.SetGroupVersionKind(GraphGVK)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "test-status-inprogress", Namespace: ns}, g))

	state, _, _ := unstructured.NestedString(g.Object, "status", "state")
	assert.Equal(t, "InProgress", state)

	conditions, _, _ := unstructured.NestedSlice(g.Object, "status", "conditions")
	require.Len(t, conditions, 2)

	accepted, ok := findCondition(conditions, "Accepted")
	require.True(t, ok, "Accepted condition should exist")
	assert.Equal(t, "True", accepted["status"])

	cond, ok := findCondition(conditions, "Ready")
	require.True(t, ok, "Ready condition should exist")
	assert.Equal(t, "False", cond["status"])
	assert.Equal(t, "ResourcesNotReady", cond["reason"])
	t.Logf("Before: state=%s Accepted=%s Ready=%s reason=%s", state, accepted["status"], cond["status"], cond["reason"])

	// Transition source to ready
	latestSource := &unstructured.Unstructured{}
	latestSource.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "status-source", Namespace: ns}, latestSource))
	unstructured.SetNestedField(latestSource.Object, "true", "data", "ready")
	require.NoError(t, k8sClient.Update(ctx, latestSource))

	// Wait for status to transition to Active
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		g := &unstructured.Unstructured{}
		g.SetGroupVersionKind(GraphGVK)
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "test-status-inprogress", Namespace: ns}, g); err != nil {
			return false, nil
		}
		state, _, _ := unstructured.NestedString(g.Object, "status", "state")
		return state == "Active", nil
	}))

	// Verify Active status
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "test-status-inprogress", Namespace: ns}, g))
	state, _, _ = unstructured.NestedString(g.Object, "status", "state")
	assert.Equal(t, "Active", state)

	conditions, _, _ = unstructured.NestedSlice(g.Object, "status", "conditions")
	require.Len(t, conditions, 2)
	cond, ok = findCondition(conditions, "Ready")
	require.True(t, ok, "Ready condition should exist")
	assert.Equal(t, "True", cond["status"])
	assert.Equal(t, "Ready", cond["reason"])
	t.Logf("After: state=%s Ready=%s reason=%s", state, cond["status"], cond["reason"])
	t.Log("Status lifecycle proved: InProgress → Active on readyWhen satisfied")
}

// TestForEachCollectionScaleUpDown proves that changing the size of a forEach
// input collection adds/removes stamped resources accordingly.
// Scale up: add a source item → new stamped resource appears (via dynamic watch).
// Scale down: remove a source item → stale stamped resource is pruned.
