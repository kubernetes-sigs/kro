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

// TestContribution proves that a Graph can write fields to an object it doesn't own.
// This is partial SSA — the Graph writes specific fields (like status) without
// taking ownership. No ownerReference is set on the target.
//
// This is how child Graphs write status back to the WebApp instance in the RGD model.
func TestContribution(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Pre-create an "external" ConfigMap that the Graph will contribute to.
	// This simulates a WebApp instance created by a user — the Graph doesn't own it.
	external := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "webapp-instance",
				"namespace": ns,
			},
			"data": map[string]any{
				"image":    "nginx:latest",
				"replicas": "3",
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, external))

	// Graph: reads the external object, creates a Deployment, then contributes
	// status back to the external object.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-contribution",
				"namespace": ns,
			},
			"spec": map[string]any{
				"resources": []any{
					// Read the external object into scope
					map[string]any{
						"id": "schema",
						"externalRef": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "webapp-instance",
							},
						},
					},
					// Create an owned resource (Deployment-like ConfigMap)
					map[string]any{
						"id": "deployment",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "${schema.metadata.name}-deployment",
							},
							"data": map[string]any{
								"image":    "${schema.data.image}",
								"replicas": "${schema.data.replicas}",
							},
						},
					},
					// Contribution: write status fields back to the external object.
					// No owner reference — this is a partial SSA apply.
					map[string]any{
						"id":           "status",
						"contribution": true,
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "${schema.metadata.name}",
								"annotations": map[string]any{
									"kro.run/deployment-name": "${deployment.metadata.name}",
									"kro.run/deployment-uid":  "${deployment.metadata.uid}",
								},
							},
							// We write to data since ConfigMaps don't have status subresource.
							// In real usage this would be a status subresource write.
							"data": map[string]any{
								"image":          "${schema.data.image}",
								"replicas":       "${schema.data.replicas}",
								"deploymentName": "${deployment.metadata.name}",
							},
						},
					},
				},
			},
		},
	}

	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for the owned deployment ConfigMap
	deplCM := &unstructured.Unstructured{}
	deplCM.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
	require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: "webapp-instance-deployment", Namespace: ns}, deplCM))

	// The deployment CM should be managed by the Graph
	assertManagedBy(t, deplCM, "test-contribution")
	t.Logf("Owned resource created: %s (managed by Graph)", deplCM.GetName())

	// Wait for the contribution to be applied to the external object
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		updated := &unstructured.Unstructured{}
		updated.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "webapp-instance", Namespace: ns}, updated); err != nil {
			return false, nil
		}
		data, _, _ := unstructured.NestedStringMap(updated.Object, "data")
		return data["deploymentName"] == "webapp-instance-deployment", nil
	}))

	// Re-read the external object to verify
	updatedExternal := &unstructured.Unstructured{}
	updatedExternal.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "webapp-instance", Namespace: ns}, updatedExternal))

	// THE PROOF: The external object now has fields written by the Graph
	data, _, _ := unstructured.NestedStringMap(updatedExternal.Object, "data")
	assert.Equal(t, "webapp-instance-deployment", data["deploymentName"],
		"contribution should write deployment name to external object")
	assert.Equal(t, "nginx:latest", data["image"],
		"contribution should preserve existing fields")

	annotations := updatedExternal.GetAnnotations()
	assert.Equal(t, "webapp-instance-deployment", annotations["kro.run/deployment-name"],
		"contribution should write annotations to external object")
	assert.NotEmpty(t, annotations["kro.run/deployment-uid"],
		"contribution should write server-assigned UID")

	// THE KEY ASSERTION: The external object should NOT be managed by the Graph.
	// Contributions are partial — they don't set management labels.
	extLabels := updatedExternal.GetLabels()
	assert.NotEqual(t, "test-contribution", extLabels["internal.kro.run/graph-name"],
		"contribution should NOT set management labels on external object")

	t.Logf("Contribution applied: webapp-instance now has deploymentName=%s, deployment-uid=%s",
		data["deploymentName"], annotations["kro.run/deployment-uid"])
	t.Log("Partial SSA proved: Graph wrote fields to external object without taking ownership")
}

// TestResourcePruning proves that removing a resource from the Graph spec
// causes the previously-created resource to be deleted.
func TestResourcePruning(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-pruning",
				"namespace": ns,
			},
			"spec": map[string]any{
				"resources": []any{
					map[string]any{
						"id": "keep",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "keep-me"},
							"data":       map[string]any{"state": "permanent"},
						},
					},
					map[string]any{
						"id": "remove",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "remove-me"},
							"data":       map[string]any{"state": "temporary"},
						},
					},
				},
			},
		},
	}

	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for both ConfigMaps
	keepCM := &unstructured.Unstructured{}
	keepCM.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
	require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: "keep-me", Namespace: ns}, keepCM))

	removeCM := &unstructured.Unstructured{}
	removeCM.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
	require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: "remove-me", Namespace: ns}, removeCM))
	t.Log("Both ConfigMaps created")

	// Update the Graph: remove the second resource
	latest := &unstructured.Unstructured{}
	latest.SetGroupVersionKind(GraphGVK)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "test-pruning", Namespace: ns}, latest))

	unstructured.SetNestedSlice(latest.Object, []any{
		map[string]any{
			"id": "keep",
			"template": map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]any{"name": "keep-me"},
				"data":       map[string]any{"state": "permanent"},
			},
		},
	}, "spec", "resources")
	require.NoError(t, k8sClient.Update(ctx, latest))

	// Wait for the removed ConfigMap to be deleted
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		check := &unstructured.Unstructured{}
		check.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
		err := k8sClient.Get(ctx, types.NamespacedName{Name: "remove-me", Namespace: ns}, check)
		if err != nil {
			return true, nil // deleted
		}
		return false, nil
	}))
	t.Log("Removed ConfigMap was pruned")

	// The kept ConfigMap should still exist
	stillThere := &unstructured.Unstructured{}
	stillThere.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "keep-me", Namespace: ns}, stillThere))
	data, _, _ := unstructured.NestedStringMap(stillThere.Object, "data")
	assert.Equal(t, "permanent", data["state"])
	t.Log("Kept ConfigMap still exists with correct data")
}

// TestDynamicWatchExternalRefChange proves O(1) reactivity:
// changing an externalRef target triggers the Graph's reconciliation
// WITHOUT touching the Graph object itself.
