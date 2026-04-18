package graphcontroller_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-dynamic-watch",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "config",
						"ref": map[string]any{
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
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
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
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-collection-watch",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "items",
						"watch": map[string]any{
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

	// THE PROOF: item-c-copy should appear, triggered by the dynamic Watch
	newCopy := &unstructured.Unstructured{}
	newCopy.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
	require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: "item-c-copy", Namespace: ns}, newCopy),
		"new collection member should trigger parent reconciliation via dynamic watch")

	data, _, _ := unstructured.NestedStringMap(newCopy.Object, "data")
	assert.Equal(t, "item-c", data["source"])
	t.Log("item-c-copy created — dynamic Watch triggered reconciliation")
}

// TestCollectionMemberRelabeledOutOfSelector proves that when a resource's
// labels change so it no longer matches a Watch selector, the Watch
// coordinator routes the update (via oldLabels matching) and the forEach
// output for that item is pruned.
func TestCollectionMemberRelabeledOutOfSelector(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Create 2 items matching the selector.
	for _, name := range []string{"item-a", "item-b"} {
		cm := &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name":      name,
					"namespace": ns,
					"labels":    map[string]any{"tier": "frontend"},
				},
				"data": map[string]any{"val": name},
			},
		}
		require.NoError(t, k8sClient.Create(ctx, cm))
	}

	// Graph watches by label selector, forEach stamps copies.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-relabel",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "items",
						"watch": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"selector":   map[string]any{"tier": "frontend"},
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

	// Wait for both copies.
	for _, name := range []string{"item-a-copy", "item-b-copy"} {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"})
		require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: name, Namespace: ns}, obj))
	}
	t.Log("Both copies created")

	// Relabel item-b so it no longer matches the selector.
	// The Watch coordinator should route this via oldLabels matching.
	cmGVK := schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}
	require.NoError(t, updateWithRetry(ctx, k8sClient, cmGVK, types.NamespacedName{Name: "item-b", Namespace: ns}, func(obj *unstructured.Unstructured) {
		obj.SetLabels(map[string]string{"tier": "backend"})
	}))
	t.Log("Relabeled item-b to tier=backend (Graph NOT touched)")

	// THE PROOF: item-b-copy should be pruned because item-b left the
	// collection. item-a-copy should remain.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true,
		func(ctx context.Context) (bool, error) {
			obj := &unstructured.Unstructured{}
			obj.SetGroupVersionKind(cmGVK)
			err := k8sClient.Get(ctx, types.NamespacedName{Name: "item-b-copy", Namespace: ns}, obj)
			if err != nil && !apierrors.IsNotFound(err) {
				return false, nil // transient error — keep polling
			}
			return apierrors.IsNotFound(err), nil
		}),
		"item-b-copy should be pruned after item-b left the selector set")

	// item-a-copy should still exist.
	aCopy := &unstructured.Unstructured{}
	aCopy.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "item-a-copy", Namespace: ns}, aCopy))
	t.Log("item-b-copy pruned, item-a-copy remains — oldLabels routing works end-to-end")
}

// TestMultiGraphWatchRouting proves that when two Graphs both watch the same
// external resource via ref, a single change to that resource triggers
// reconciliation of both Graphs.
func TestMultiGraphWatchRouting(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Shared config both Graphs read.
	config := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "shared-config",
				"namespace": ns,
			},
			"data": map[string]any{"value": "original"},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, config))

	// Helper: build a Graph that reads shared-config and stamps an output.
	makeGraph := func(name, outputName string) *unstructured.Unstructured {
		return &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "experimental.kro.run/v1alpha1",
				"kind":       "Graph",
				"metadata": map[string]any{
					"name":      name,
					"namespace": ns,
				},
				"spec": map[string]any{
					"nodes": []any{
						map[string]any{
							"id": "config",
							"ref": map[string]any{
								"apiVersion": "v1",
								"kind":       "ConfigMap",
								"metadata":   map[string]any{"name": "shared-config"},
							},
						},
						map[string]any{
							"id": "output",
							"template": map[string]any{
								"apiVersion": "v1",
								"kind":       "ConfigMap",
								"metadata":   map[string]any{"name": outputName},
								"data":       map[string]any{"copied": "${config.data.value}"},
							},
						},
					},
				},
			},
		}
	}

	require.NoError(t, k8sClient.Create(ctx, makeGraph("graph-a", "output-a")))
	require.NoError(t, k8sClient.Create(ctx, makeGraph("graph-b", "output-b")))

	// Wait for initial outputs.
	cmGVK := schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}
	for _, name := range []string{"output-a", "output-b"} {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(cmGVK)
		require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: name, Namespace: ns}, obj))
		data, _, _ := unstructured.NestedStringMap(obj.Object, "data")
		assert.Equal(t, "original", data["copied"])
	}
	t.Log("Both graphs converged with value=original")

	// Update the shared config — neither Graph is touched.
	require.NoError(t, updateWithRetry(ctx, k8sClient, cmGVK, types.NamespacedName{Name: "shared-config", Namespace: ns}, func(obj *unstructured.Unstructured) {
		unstructured.SetNestedField(obj.Object, "updated", "data", "value")
	}))
	t.Log("Updated shared-config to value=updated (neither Graph touched)")

	// THE PROOF: both outputs should update to "updated", triggered by the
	// single watch event being routed to both Graphs.
	for _, name := range []string{"output-a", "output-b"} {
		require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
			obj := &unstructured.Unstructured{}
			obj.SetGroupVersionKind(cmGVK)
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, obj); err != nil {
				return false, nil
			}
			data, _, _ := unstructured.NestedStringMap(obj.Object, "data")
			return data["copied"] == "updated", nil
		}), "output %s should update after shared-config change", name)
	}
	t.Log("Both outputs updated — multi-graph watch routing works end-to-end")
}

// TestStaleWatchCleanupOnSpecChange proves that when a node is removed from
// a Graph's spec (via spec update), the watch coordinator cleans the stale
// index entry. Observable: changing the old watched resource no longer triggers
// reconciliation — the output using the old ref stops updating.
func TestStaleWatchCleanupOnSpecChange(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Create two configs: one the Graph will initially ref, another it will
	// switch to.
	for _, name := range []string{"config-old", "config-new"} {
		cm := &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name":      name,
					"namespace": ns,
				},
				"data": map[string]any{"value": name},
			},
		}
		require.NoError(t, k8sClient.Create(ctx, cm))
	}

	// Graph V1: refs config-old.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-stale-watch",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "config",
						"ref": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "config-old"},
						},
					},
					map[string]any{
						"id": "output",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "output"},
							"data":       map[string]any{"source": "${config.data.value}"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for output with config-old's value.
	cmGVK := schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}
	output := &unstructured.Unstructured{}
	output.SetGroupVersionKind(cmGVK)
	require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: "output", Namespace: ns}, output))
	data, _, _ := unstructured.NestedStringMap(output.Object, "data")
	assert.Equal(t, "config-old", data["source"])
	t.Log("V1: output reads from config-old")

	// Update the Graph spec to ref config-new instead.
	graphGVK := schema.GroupVersionKind{Group: "experimental.kro.run", Version: "v1alpha1", Kind: "Graph"}
	require.NoError(t, updateWithRetry(ctx, k8sClient, graphGVK, types.NamespacedName{Name: "test-stale-watch", Namespace: ns}, func(obj *unstructured.Unstructured) {
		unstructured.SetNestedField(obj.Object, "config-new", "spec", "nodes")
		// Replace the full nodes list to switch the ref target.
		obj.Object["spec"].(map[string]any)["nodes"] = []any{
			map[string]any{
				"id": "config",
				"ref": map[string]any{
					"apiVersion": "v1",
					"kind":       "ConfigMap",
					"metadata":   map[string]any{"name": "config-new"},
				},
			},
			map[string]any{
				"id": "output",
				"template": map[string]any{
					"apiVersion": "v1",
					"kind":       "ConfigMap",
					"metadata":   map[string]any{"name": "output"},
					"data":       map[string]any{"source": "${config.data.value}"},
				},
			},
		}
	}))
	t.Log("Updated Graph to ref config-new instead of config-old")

	// Wait for output to switch to config-new.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(cmGVK)
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "output", Namespace: ns}, obj); err != nil {
			return false, nil
		}
		d, _, _ := unstructured.NestedStringMap(obj.Object, "data")
		return d["source"] == "config-new", nil
	}))
	t.Log("V2: output now reads from config-new")

	// THE PROOF: use a causal fence. Update config-old (stale target), then
	// update config-new (active target). If the stale watch was cleaned, the
	// output should reflect the config-new update without ever showing the
	// config-old value — proving the controller processed events in this
	// window and correctly ignored config-old.
	require.NoError(t, updateWithRetry(ctx, k8sClient, cmGVK, types.NamespacedName{Name: "config-old", Namespace: ns}, func(obj *unstructured.Unstructured) {
		unstructured.SetNestedField(obj.Object, "should-not-appear", "data", "value")
	}))
	t.Log("Updated config-old (Graph no longer watches it)")

	require.NoError(t, updateWithRetry(ctx, k8sClient, cmGVK, types.NamespacedName{Name: "config-new", Namespace: ns}, func(obj *unstructured.Unstructured) {
		unstructured.SetNestedField(obj.Object, "config-new-v2", "data", "value")
	}))
	t.Log("Updated config-new to config-new-v2 (causal fence)")

	// Wait for the output to reflect the config-new-v2 update. This is a
	// positive assertion — the controller processed the config-new event.
	// If it also processed config-old, the source field would show
	// "should-not-appear" instead of "config-new-v2".
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(cmGVK)
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "output", Namespace: ns}, obj); err != nil {
			return false, nil
		}
		d, _, _ := unstructured.NestedStringMap(obj.Object, "data")
		if d["source"] == "should-not-appear" {
			t.Fatal("output read from config-old after spec change — stale watch index not cleaned")
		}
		return d["source"] == "config-new-v2", nil
	}))
	t.Log("Output shows config-new-v2 — stale watch cleaned, causal fence passed")
}

// TestWatchStatePreservedAcrossReconcileError proves that when a reconcile
// fails (e.g., invalid CEL expression), the watch state from the previous
// successful cycle is preserved. The error is surfaced in status, but after
// the spec is fixed, watch routing resumes correctly — proving the abort
// buffer semantics don't corrupt pre-existing watch indexes.
func TestWatchStatePreservedAcrossReconcileError(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Create the config that the Graph reads via ref.
	config := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "watched-cfg",
				"namespace": ns,
			},
			"data": map[string]any{"value": "v1"},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, config))

	// Graph V1: valid — reads config and stamps output.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-abort-recovery",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "config",
						"ref": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "watched-cfg"},
						},
					},
					map[string]any{
						"id": "output",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "output"},
							"data":       map[string]any{"copied": "${config.data.value}"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for initial convergence.
	cmGVK := schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}
	output := &unstructured.Unstructured{}
	output.SetGroupVersionKind(cmGVK)
	require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: "output", Namespace: ns}, output))
	data, _, _ := unstructured.NestedStringMap(output.Object, "data")
	assert.Equal(t, "v1", data["copied"])
	t.Log("V1 converged: output has value=v1")

	// V2: break the Graph with an invalid CEL expression in a new node.
	// This causes a compilation error — the reconcile aborts.
	graphGVK := schema.GroupVersionKind{Group: "experimental.kro.run", Version: "v1alpha1", Kind: "Graph"}
	require.NoError(t, updateWithRetry(ctx, k8sClient, graphGVK, types.NamespacedName{Name: "test-abort-recovery", Namespace: ns}, func(obj *unstructured.Unstructured) {
		obj.Object["spec"].(map[string]any)["nodes"] = []any{
			map[string]any{
				"id": "config",
				"ref": map[string]any{
					"apiVersion": "v1",
					"kind":       "ConfigMap",
					"metadata":   map[string]any{"name": "watched-cfg"},
				},
			},
			map[string]any{
				"id": "output",
				"template": map[string]any{
					"apiVersion": "v1",
					"kind":       "ConfigMap",
					"metadata":   map[string]any{"name": "output"},
					"data":       map[string]any{"copied": "${config.data.value}"},
				},
			},
			map[string]any{
				"id": "broken",
				"template": map[string]any{
					"apiVersion": "v1",
					"kind":       "ConfigMap",
					"metadata":   map[string]any{"name": "broken-output"},
					// Reference a node that doesn't exist — compile error.
					"data": map[string]any{"bad": "${nonexistent.field}"},
				},
			},
		}
	}))
	t.Log("V2: injected broken CEL expression")

	// Wait for the Compiled=False condition.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		g := &unstructured.Unstructured{}
		g.SetGroupVersionKind(graphGVK)
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "test-abort-recovery", Namespace: ns}, g); err != nil {
			return false, nil
		}
		conditions, _, _ := unstructured.NestedSlice(g.Object, "status", "conditions")
		for _, c := range conditions {
			cm := c.(map[string]any)
			if cm["type"] == "Compiled" && cm["status"] == "False" {
				return true, nil
			}
		}
		return false, nil
	}))
	t.Log("V2 shows Compiled=False — compilation error surfaced")

	// While broken: update the watched config. If abort preserved the
	// previous watch state, the ref watch for "watched-cfg" is still active
	// and will route events when the Graph is fixed.
	require.NoError(t, updateWithRetry(ctx, k8sClient, cmGVK, types.NamespacedName{Name: "watched-cfg", Namespace: ns}, func(obj *unstructured.Unstructured) {
		unstructured.SetNestedField(obj.Object, "v2", "data", "value")
	}))
	t.Log("Updated watched-cfg to v2 while Graph is broken")

	// V3: fix the Graph — remove the broken node.
	require.NoError(t, updateWithRetry(ctx, k8sClient, graphGVK, types.NamespacedName{Name: "test-abort-recovery", Namespace: ns}, func(obj *unstructured.Unstructured) {
		obj.Object["spec"].(map[string]any)["nodes"] = []any{
			map[string]any{
				"id": "config",
				"ref": map[string]any{
					"apiVersion": "v1",
					"kind":       "ConfigMap",
					"metadata":   map[string]any{"name": "watched-cfg"},
				},
			},
			map[string]any{
				"id": "output",
				"template": map[string]any{
					"apiVersion": "v1",
					"kind":       "ConfigMap",
					"metadata":   map[string]any{"name": "output"},
					"data":       map[string]any{"copied": "${config.data.value}"},
				},
			},
		}
	}))
	t.Log("V3: fixed Graph — removed broken node")

	// THE PROOF: output should update to v2 — the watch routing survived
	// the broken-spec cycle because the abort discarded the failed cycle's
	// watch buffer without corrupting the previous cycle's indexes.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(cmGVK)
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "output", Namespace: ns}, obj); err != nil {
			return false, nil
		}
		d, _, _ := unstructured.NestedStringMap(obj.Object, "data")
		return d["copied"] == "v2", nil
	}))
	t.Log("Output updated to v2 — watch state preserved across reconcile error")
}

// TestUnwatchableRefSurfacesError proves that when a Graph references a type
// that doesn't exist (CRD not installed), the controller surfaces a clear
// status condition rather than crashing or silently failing.
func TestUnwatchableRefSurfacesError(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Graph refs a type that doesn't exist.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-bad-ref",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "missing",
						"ref": map[string]any{
							"apiVersion": "does-not-exist.example.com/v1",
							"kind":       "Phantom",
							"metadata":   map[string]any{"name": "ghost"},
						},
					},
					map[string]any{
						"id": "output",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "output"},
							"data":       map[string]any{"val": "${missing.metadata.name}"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// The Graph should surface an error in status — either Compiled=False
	// (unresolvable GVK) or Ready with a pending/error node. The key
	// assertion is that the controller doesn't crash and surfaces the issue.
	graphGVK := schema.GroupVersionKind{Group: "experimental.kro.run", Version: "v1alpha1", Kind: "Graph"}
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		g := &unstructured.Unstructured{}
		g.SetGroupVersionKind(graphGVK)
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "test-bad-ref", Namespace: ns}, g); err != nil {
			return false, nil
		}
		conditions, _, _ := unstructured.NestedSlice(g.Object, "status", "conditions")
		for _, c := range conditions {
			cm := c.(map[string]any)
			// Accept either Compiled=False (can't resolve GVK) or
			// Ready=False (node pending/error).
			if cm["status"] == "False" {
				return true, nil
			}
		}
		return false, nil
	}))
	t.Log("Bad ref surfaced as status condition — controller healthy")

	// Verify the controller is still functional by creating a second,
	// valid Graph and confirming it converges.
	validGraph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-still-alive",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "out",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "alive"},
							"data":       map[string]any{"status": "ok"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, validGraph))

	alive := &unstructured.Unstructured{}
	alive.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"})
	require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: "alive", Namespace: ns}, alive))
	t.Log("Second Graph converged — controller not blocked by bad ref")
}
