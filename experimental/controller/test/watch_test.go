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

// TestDynamicWatchExternalRefChange proves O(1) reactivity:
// changing a watch target triggers the Graph's reconciliation
// WITHOUT touching the Graph object itself.
func TestDynamicWatchExternalRefChange(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Create the config that the Graph reads via watch
	config := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "watched-config",
				"namespace": ns,
			},
			"data": map[string]any{
				"version": "v1",
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, config))

	// Graph reads the config via watch and creates a resource referencing it
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-dynamic-watch",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "config",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "watched-config"},
						},
					},
					map[string]any{
						"id": "output",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "output"},
							"data": map[string]any{
								"version": "${config.data.version}",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for initial output
	output := &unstructured.Unstructured{}
	output.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
	require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: "output", Namespace: ns}, output))

	data, _, _ := unstructured.NestedStringMap(output.Object, "data")
	assert.Equal(t, "v1", data["version"])
	t.Log("Initial output created with version=v1")

	// Now update the watch target — do NOT touch the Graph object
	latestConfig := &unstructured.Unstructured{}
	latestConfig.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "watched-config", Namespace: ns}, latestConfig))
	unstructured.SetNestedField(latestConfig.Object, "v2", "data", "version")
	require.NoError(t, k8sClient.Update(ctx, latestConfig))
	t.Log("Updated watched-config version to v2 (Graph object NOT touched)")

	// THE PROOF: the output should update to v2, triggered by the dynamic watch
	// on the watch target — not by a Graph spec change.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 5*time.Second, true, func(ctx context.Context) (bool, error) {
		updated := &unstructured.Unstructured{}
		updated.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "output", Namespace: ns}, updated); err != nil {
			return false, nil
		}
		d, _, _ := unstructured.NestedStringMap(updated.Object, "data")
		return d["version"] == "v2", nil
	}))

	t.Log("Output updated to version=v2 — dynamic watch triggered reconciliation")
}

// TestDynamicWatchCollectionMembershipChange proves that adding a new object
// matching a collection selector triggers the parent Graph's reconciliation.
func TestDynamicWatchCollectionMembershipChange(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Start with 2 items
	for _, name := range []string{"item-a", "item-b"} {
		cm := &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name":      name,
					"namespace": ns,
					"labels":    map[string]any{"collection": "watched"},
				},
				"data": map[string]any{"value": name},
			},
		}
		require.NoError(t, k8sClient.Create(ctx, cm))
	}

	// Graph: reads collection, forEach stamps one copy per item
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-collection-watch",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "items",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"selector":   map[string]any{"collection": "watched"},
						},
					},
					map[string]any{
						"id": "copies",
						"forEach": map[string]any{
							"item": "${items}",
						},
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "${item.metadata.name}-copy"},
							"data":       map[string]any{"source": "${item.metadata.name}"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for initial 2 copies
	for _, name := range []string{"item-a-copy", "item-b-copy"} {
		cm := &unstructured.Unstructured{}
		cm.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
		require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: name, Namespace: ns}, cm))
	}
	t.Log("Initial 2 copies created")

	// Add a new item to the collection — do NOT touch the Graph
	newItem := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "item-c",
				"namespace": ns,
				"labels":    map[string]any{"collection": "watched"},
			},
			"data": map[string]any{"value": "item-c"},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, newItem))
	t.Log("Added item-c to collection (Graph object NOT touched)")

	// THE PROOF: item-c-copy should appear, triggered by the dynamic collection watch
	newCopy := &unstructured.Unstructured{}
	newCopy.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
	require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: "item-c-copy", Namespace: ns}, newCopy),
		"new collection member should trigger parent reconciliation via dynamic watch")

	data, _, _ := unstructured.NestedStringMap(newCopy.Object, "data")
	assert.Equal(t, "item-c", data["source"])
	t.Log("item-c-copy created — dynamic collection watch triggered reconciliation")
}
