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

// TestStatusUserDefinedFields proves that spec.status template expressions
// are evaluated against the resource scope and merged into the Graph's status.
// This mirrors the RGD pattern where instance.status contains computed fields
// from child resources (e.g., deploymentReady, address).
func TestStatusUserDefinedFields(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-status-fields",
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
								"name": "status-fields-config",
							},
							"data": map[string]any{
								"appName": "my-app",
								"version": "1.2.3",
							},
						},
					},
				},
				"status": map[string]any{
					"appName":    "${config.data.appName}",
					"appVersion": "${config.data.version}",
					"configName": "${config.metadata.name}",
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for status to include user-defined fields
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		g := &unstructured.Unstructured{}
		g.SetGroupVersionKind(GraphGVK)
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "test-status-fields", Namespace: ns}, g); err != nil {
			return false, nil
		}
		appName, _, _ := unstructured.NestedString(g.Object, "status", "appName")
		return appName == "my-app", nil
	}))

	g := &unstructured.Unstructured{}
	g.SetGroupVersionKind(GraphGVK)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "test-status-fields", Namespace: ns}, g))

	// Verify controller-managed fields
	state, _, _ := unstructured.NestedString(g.Object, "status", "state")
	assert.Equal(t, "Active", state)

	// Verify user-defined fields
	appName, _, _ := unstructured.NestedString(g.Object, "status", "appName")
	assert.Equal(t, "my-app", appName)
	appVersion, _, _ := unstructured.NestedString(g.Object, "status", "appVersion")
	assert.Equal(t, "1.2.3", appVersion)
	configName, _, _ := unstructured.NestedString(g.Object, "status", "configName")
	assert.Equal(t, "status-fields-config", configName)

	// Verify conditions are still present alongside user fields
	conditions, _, _ := unstructured.NestedSlice(g.Object, "status", "conditions")
	require.Len(t, conditions, 2)

	accepted, ok := findCondition(conditions, "Accepted")
	require.True(t, ok, "Accepted condition should exist")
	assert.Equal(t, "True", accepted["status"])

	cond, ok := findCondition(conditions, "Ready")
	require.True(t, ok, "Ready condition should exist")
	assert.Equal(t, "True", cond["status"])

	t.Logf("User-defined status: appName=%s appVersion=%s configName=%s (alongside state=%s Ready=%s)",
		appName, appVersion, configName, state, cond["status"])
}

// TestFullLifecycle mirrors the core RGD lifecycle integration test:
//   - Create a Graph with Deployment → Service dependency chain
//   - Verify both resources are created with correct cross-references
//   - Verify status shows Active with user-defined status fields
//   - Update the Graph spec (change replicas)
//   - Verify the child resources are updated
//   - Delete the Graph
//   - Verify all child resources are cleaned up

func TestPartialStatusResolution(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Control ConfigMap: drives which resources are included
	control := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "status-control",
				"namespace": ns,
			},
			"data": map[string]any{
				"enable1": "true",
				"enable2": "false",
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, control))

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-partial-status",
				"namespace": ns,
			},
			"spec": map[string]any{
				"resources": []any{
					map[string]any{
						"id": "control",
						"externalRef": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "status-control"},
						},
					},
					map[string]any{
						"id": "cm1",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "status-cm1"},
							"data":       map[string]any{"value": "from-cm1"},
						},
						"includeWhen": []any{"${control.data.enable1 == 'true'}"},
					},
					map[string]any{
						"id": "cm2",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "status-cm2"},
							"data":       map[string]any{"value": "from-cm2"},
						},
						"includeWhen": []any{"${control.data.enable2 == 'true'}"},
					},
				},
				"status": map[string]any{
					"field1": "${cm1.data.value}",
					"field2": "${cm2.data.value}",
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Phase 1: enable1=true, enable2=false → field1 present, field2 absent
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		g := &unstructured.Unstructured{}
		g.SetGroupVersionKind(GraphGVK)
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "test-partial-status", Namespace: ns}, g); err != nil {
			return false, nil
		}
		f1, ok1, _ := unstructured.NestedString(g.Object, "status", "field1")
		_, ok2, _ := unstructured.NestedString(g.Object, "status", "field2")
		return ok1 && f1 == "from-cm1" && !ok2, nil
	}))
	t.Log("Phase 1: field1=from-cm1 present, field2 absent (cm2 excluded)")

	// Phase 2: flip enable1=false, enable2=true → field1 disappears, field2 appears
	latestCtl := &unstructured.Unstructured{}
	latestCtl.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "status-control", Namespace: ns}, latestCtl))
	unstructured.SetNestedField(latestCtl.Object, "false", "data", "enable1")
	unstructured.SetNestedField(latestCtl.Object, "true", "data", "enable2")
	require.NoError(t, k8sClient.Update(ctx, latestCtl))
	t.Log("Updated control: enable1=false, enable2=true")

	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		g := &unstructured.Unstructured{}
		g.SetGroupVersionKind(GraphGVK)
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "test-partial-status", Namespace: ns}, g); err != nil {
			return false, nil
		}
		_, ok1, _ := unstructured.NestedString(g.Object, "status", "field1")
		f2, ok2, _ := unstructured.NestedString(g.Object, "status", "field2")
		return !ok1 && ok2 && f2 == "from-cm2", nil
	}))
	t.Log("Phase 2: field1 disappeared (cm1 excluded), field2=from-cm2 appeared")
	t.Log("Partial status resolution proved: fields toggle with resource inclusion")
}

// TestForEachCollectionScaleUpDown proves that changing the size of a forEach
// input collection adds/removes stamped resources accordingly.
// Scale up: add a source item → new stamped resource appears (via dynamic watch).
// Scale down: remove a source item → stale stamped resource is pruned.
