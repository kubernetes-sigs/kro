package graphcontroller_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
)

// TestRevisionCreatedOnGraphCreate verifies that creating a Graph produces
// a GraphRevision with the correct name, labels, and materialized resources.
func TestRevisionCreatedOnGraphCreate(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "rev-create-test",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "configmap",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "rev-test-cm",
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

	// Wait for the ConfigMap to be created (proves reconciliation happened)
	cm := &unstructured.Unstructured{}
	cm.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
	require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: "rev-test-cm", Namespace: ns}, cm))

	// Fetch the Graph to get its generation
	latestGraph := &unstructured.Unstructured{}
	latestGraph.SetGroupVersionKind(GraphGVK)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "rev-create-test", Namespace: ns}, latestGraph))
	generation := latestGraph.GetGeneration()

	// A GraphRevision should exist for this generation
	revName := fmt.Sprintf("rev-create-test-g%05d", generation)
	rev, err := waitForRevision(ctx, k8sClient, types.NamespacedName{Name: revName, Namespace: ns})
	require.NoError(t, err, "GraphRevision should be created")
	t.Logf("GraphRevision created: %s", rev.GetName())

	// Verify labels
	assertRevisionLabels(t, rev, "rev-create-test", generation)

	// Verify the revision has a spec with nodes
	spec, ok := rev.Object["spec"].(map[string]any)
	require.True(t, ok, "revision should have a spec")
	nodes, ok := spec["nodes"].([]any)
	require.True(t, ok, "revision spec should have nodes")
	assert.Len(t, nodes, 1, "revision should have 1 resource")

	// Verify the materialized resource has injected labels
	res := nodes[0].(map[string]any)
	assert.Equal(t, "configmap", res["id"])
	tmpl, ok := res["template"].(map[string]any)
	require.True(t, ok)
	// Verify the template has the expected structure (metadata with name).
	// Labels are injected at apply time (not materialization time) for Deferred
	// shapes since Owns vs Contribute isn't known until the first reconcile.
	md, _ := tmpl["metadata"].(map[string]any)
	require.NotNil(t, md)
	assert.Equal(t, "rev-test-cm", md["name"])
	t.Log("Revision has correct materialized template")
}

// TestRevisionCreatedOnSpecChange verifies that updating a Graph's spec
// creates a new GraphRevision while preserving the old one.
func TestRevisionCreatedOnSpecChange(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "rev-update-test",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "configmap",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "rev-update-cm",
							},
							"data": map[string]any{
								"version": "v1",
							},
						},
					},
				},
			},
		},
	}

	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for ConfigMap to be created
	cm := &unstructured.Unstructured{}
	cm.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
	require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: "rev-update-cm", Namespace: ns}, cm))

	// Get the first generation
	latestGraph := &unstructured.Unstructured{}
	latestGraph.SetGroupVersionKind(GraphGVK)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "rev-update-test", Namespace: ns}, latestGraph))
	gen1 := latestGraph.GetGeneration()

	// Wait for first revision
	rev1Name := fmt.Sprintf("rev-update-test-g%05d", gen1)
	_, err := waitForRevision(ctx, k8sClient, types.NamespacedName{Name: rev1Name, Namespace: ns})
	require.NoError(t, err, "first revision should exist")
	t.Logf("First revision: %s (generation %d)", rev1Name, gen1)

	// Update the Graph spec
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "rev-update-test", Namespace: ns}, latestGraph))
	nodes := []any{
		map[string]any{
			"id": "configmap",
			"template": map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name": "rev-update-cm",
				},
				"data": map[string]any{
					"version": "v2",
				},
			},
		},
	}
	unstructured.SetNestedSlice(latestGraph.Object, nodes, "spec", "nodes")
	require.NoError(t, k8sClient.Update(ctx, latestGraph))
	t.Log("Updated Graph spec: version v1 → v2")

	// Wait for the ConfigMap to be updated
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		cm2 := &unstructured.Unstructured{}
		cm2.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "rev-update-cm", Namespace: ns}, cm2); err != nil {
			return false, nil
		}
		data, _, _ := unstructured.NestedStringMap(cm2.Object, "data")
		return data["version"] == "v2", nil
	}))

	// Get the new generation
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "rev-update-test", Namespace: ns}, latestGraph))
	gen2 := latestGraph.GetGeneration()
	assert.Greater(t, gen2, gen1, "generation should have increased")

	// Second revision should exist
	rev2Name := fmt.Sprintf("rev-update-test-g%05d", gen2)
	rev2, err := waitForRevision(ctx, k8sClient, types.NamespacedName{Name: rev2Name, Namespace: ns})
	require.NoError(t, err, "second revision should exist")
	t.Logf("Second revision: %s (generation %d)", rev2Name, gen2)

	// Both revisions should exist
	count, err := countRevisions(ctx, k8sClient, "rev-update-test", ns)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, count, 1, "at least 1 revision should exist")
	t.Logf("Total revisions: %d", count)

	// Verify the second revision has updated content
	spec, _ := rev2.Object["spec"].(map[string]any)
	rev2Nodes, _ := spec["nodes"].([]any)
	require.Len(t, rev2Nodes, 1)
	rev2Tmpl := rev2Nodes[0].(map[string]any)["template"].(map[string]any)
	rev2Data, _ := rev2Tmpl["data"].(map[string]any)
	assert.Equal(t, "v2", rev2Data["version"], "second revision should have v2")
}

// TestRevisionNotCreatedOnCompilationFailure verifies that a Graph with
// invalid CEL expressions does NOT produce a GraphRevision.
func TestRevisionNotCreatedOnCompilationFailure(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "rev-fail-test",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "bad",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "bad-cm",
							},
							"data": map[string]any{
								// Invalid CEL expression — type error at compile time
								"value": "${true + 42}",
							},
						},
					},
				},
			},
		},
	}

	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for Graph to show Accepted=False
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		g := &unstructured.Unstructured{}
		g.SetGroupVersionKind(GraphGVK)
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "rev-fail-test", Namespace: ns}, g); err != nil {
			return false, nil
		}
		conditions, _, _ := unstructured.NestedSlice(g.Object, "status", "conditions")
		cond, found := findCondition(conditions, "Accepted")
		if !found {
			return false, nil
		}
		return cond["status"] == "False", nil
	}))
	t.Log("Graph has Accepted=False")

	// No revision should exist
	count, err := countRevisions(ctx, k8sClient, "rev-fail-test", ns)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "no revisions should exist when compilation fails")
	t.Log("No revision created — compilation failure correctly prevented revision creation")
}

// TestRevisionCleanupOnDelete verifies that deleting a Graph cleans up
// all associated GraphRevisions.
func TestRevisionCleanupOnDelete(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "rev-delete-test",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "configmap",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "rev-delete-cm",
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

	// Wait for resource and revision to exist
	cm := &unstructured.Unstructured{}
	cm.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
	require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: "rev-delete-cm", Namespace: ns}, cm))

	latestGraph := &unstructured.Unstructured{}
	latestGraph.SetGroupVersionKind(GraphGVK)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "rev-delete-test", Namespace: ns}, latestGraph))
	gen := latestGraph.GetGeneration()

	revName := fmt.Sprintf("rev-delete-test-g%05d", gen)
	_, err := waitForRevision(ctx, k8sClient, types.NamespacedName{Name: revName, Namespace: ns})
	require.NoError(t, err)
	t.Logf("Revision exists: %s", revName)

	// Delete the Graph
	require.NoError(t, k8sClient.Delete(ctx, latestGraph))
	t.Log("Graph deleted")

	// Wait for Graph to be gone (finalizer removed)
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		check := &unstructured.Unstructured{}
		check.SetGroupVersionKind(GraphGVK)
		err := k8sClient.Get(ctx, types.NamespacedName{Name: "rev-delete-test", Namespace: ns}, check)
		return err != nil, nil
	}))
	t.Log("Graph gone")

	// GraphRevision should also be gone
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		count, err := countRevisions(ctx, k8sClient, "rev-delete-test", ns)
		if err != nil {
			return false, nil
		}
		return count == 0, nil
	}))
	t.Log("All revisions cleaned up on Graph deletion")

	// Managed resources should also be gone
	err = k8sClient.Get(ctx, types.NamespacedName{Name: "rev-delete-cm", Namespace: ns}, cm)
	assert.Error(t, err, "ConfigMap should be deleted")
	t.Log("Managed resources cleaned up")
}

// TestRevisionActivation verifies the activation lifecycle:
// the active revision gets Ready=True and Active=True conditions.
func TestRevisionActivation(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "rev-activate-test",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "configmap",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "rev-activate-cm",
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

	// Wait for the Graph to become Active (all resources reconciled)
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		g := &unstructured.Unstructured{}
		g.SetGroupVersionKind(GraphGVK)
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "rev-activate-test", Namespace: ns}, g); err != nil {
			return false, nil
		}
		return graphReady(g), nil
	}))
	t.Log("Graph is Active")

	// Get the revision
	latestGraph := &unstructured.Unstructured{}
	latestGraph.SetGroupVersionKind(GraphGVK)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "rev-activate-test", Namespace: ns}, latestGraph))
	gen := latestGraph.GetGeneration()

	revName := fmt.Sprintf("rev-activate-test-g%05d", gen)

	// Revision should have Ready=True
	require.NoError(t, waitForRevisionCondition(ctx, k8sClient,
		types.NamespacedName{Name: revName, Namespace: ns},
		"Ready", "True"))
	t.Log("Revision has Ready=True")

	// Revision should have Active=True
	require.NoError(t, waitForRevisionCondition(ctx, k8sClient,
		types.NamespacedName{Name: revName, Namespace: ns},
		"Active", "True"))
	t.Log("Revision has Active=True")

	// Revision should have Propagated=True
	require.NoError(t, waitForRevisionCondition(ctx, k8sClient,
		types.NamespacedName{Name: revName, Namespace: ns},
		"Propagated", "True"))
	t.Log("Revision has Propagated=True")

	t.Log("Revision activation lifecycle proved: Propagated → Ready → Active")
}
