package graphcontroller_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var cmGVK = schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}

// applyConfigMapAs creates or updates a ConfigMap via SSA with the given field
// manager. This establishes field ownership for the specified manager.
func applyConfigMapAs(t *testing.T, ns, name, fieldManager string, data map[string]string) {
	t.Helper()
	dataAny := map[string]any{}
	for k, v := range data {
		dataAny[k] = v
	}
	payload := map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":      name,
			"namespace": ns,
		},
		"data": dataAny,
	}
	raw, err := json.Marshal(payload)
	require.NoError(t, err)

	cm := &unstructured.Unstructured{}
	cm.SetGroupVersionKind(cmGVK)
	cm.SetName(name)
	cm.SetNamespace(ns)
	require.NoError(t, k8sClient.Patch(ctx, cm, client.RawPatch(
		types.ApplyPatchType, raw),
		client.ForceOwnership,
		client.FieldOwner(fieldManager),
	))
}

// TestFieldConflictBlocksDependents verifies that a 409 Conflict from a
// competing SSA field manager surfaces as NodeConflict, blocks dependents,
// and allows independent branches to continue.
func TestFieldConflictBlocksDependents(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// 1. Create a ConfigMap via SSA with an external field manager.
	// This establishes "external-manager" as the owner of data.key.
	applyConfigMapAs(t, ns, "contested-cm", "external-manager", map[string]string{
		"key": "external-value",
	})

	// Confirm it exists before creating the Graph.
	cm := &unstructured.Unstructured{}
	cm.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "contested-cm", Namespace: ns}, cm))
	t.Log("external ConfigMap created with field manager 'external-manager'")

	// 2. Create a Graph with two resources:
	//    - "conflicted": targets the same ConfigMap, writes data.key (will 409)
	//    - "independent": creates a separate ConfigMap (no dep on conflicted)
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-conflict",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "conflicted",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "contested-cm"},
							"data": map[string]any{
								"key": "graph-value",
							},
						},
					},
					map[string]any{
						"id": "independent",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "independent-cm"},
							"data": map[string]any{
								"independent": "yes",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// 3. Wait for the independent ConfigMap to be created (proves independent
	//    branch continues despite conflict on first resource).
	indCM := &unstructured.Unstructured{}
	indCM.SetGroupVersionKind(cmGVK)
	require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{
		Name: "independent-cm", Namespace: ns,
	}, indCM))
	t.Log("independent ConfigMap created — independent branch continued")

	// 4. Verify the Graph reports FieldConflict.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 10*time.Second, true, func(ctx2 context.Context) (bool, error) {
		g := &unstructured.Unstructured{}
		g.SetGroupVersionKind(GraphGVK)
		if err := k8sClient.Get(ctx2, types.NamespacedName{Name: "test-conflict", Namespace: ns}, g); err != nil {
			return false, nil
		}
		conditions, _, _ := unstructured.NestedSlice(g.Object, "status", "conditions")
		for _, c := range conditions {
			cm, ok := c.(map[string]any)
			if !ok {
				continue
			}
			if cm["type"] == "Ready" && cm["reason"] == "FieldConflict" {
				return true, nil
			}
		}
		return false, nil
	}))
	t.Log("Graph status shows FieldConflict")

	// 5. Verify the contested ConfigMap retains the external value (no force takeover).
	contested := &unstructured.Unstructured{}
	contested.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "contested-cm", Namespace: ns}, contested))
	data, _, _ := unstructured.NestedStringMap(contested.Object, "data")
	assert.Equal(t, "external-value", data["key"], "external manager's value should be preserved")
	t.Log("contested ConfigMap retains external-value — no force takeover")
}

// TestFieldConflictResolvesOnOwnershipRelease verifies that when the external
// actor releases field ownership, the Graph eventually becomes Active.
func TestFieldConflictResolvesOnOwnershipRelease(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// 1. Create ConfigMap with external field manager.
	applyConfigMapAs(t, ns, "release-cm", "external-manager", map[string]string{
		"key": "external-value",
	})
	cm := &unstructured.Unstructured{}
	cm.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "release-cm", Namespace: ns}, cm))

	// 2. Create Graph targeting that ConfigMap → conflict.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-conflict-resolve",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "target",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "release-cm"},
							"data": map[string]any{
								"key": "graph-value",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// 3. Wait for FieldConflict.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 10*time.Second, true, func(ctx2 context.Context) (bool, error) {
		g := &unstructured.Unstructured{}
		g.SetGroupVersionKind(GraphGVK)
		if err := k8sClient.Get(ctx2, types.NamespacedName{Name: "test-conflict-resolve", Namespace: ns}, g); err != nil {
			return false, nil
		}
		conditions, _, _ := unstructured.NestedSlice(g.Object, "status", "conditions")
		for _, c := range conditions {
			cm, ok := c.(map[string]any)
			if !ok {
				continue
			}
			if cm["type"] == "Ready" && cm["reason"] == "FieldConflict" {
				return true, nil
			}
		}
		return false, nil
	}))
	t.Log("Graph shows FieldConflict — now releasing external ownership")

	// 4. Release the external manager's ownership by deleting the ConfigMap.
	// The Graph controller will recreate it on the next reconcile.
	require.NoError(t, k8sClient.Delete(ctx, cm))
	t.Log("deleted external ConfigMap — Graph should recreate it")

	// 5. Wait for Graph to become Active.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 15*time.Second, true, func(ctx2 context.Context) (bool, error) {
		g := &unstructured.Unstructured{}
		g.SetGroupVersionKind(GraphGVK)
		if err := k8sClient.Get(ctx2, types.NamespacedName{Name: "test-conflict-resolve", Namespace: ns}, g); err != nil {
			return false, nil
		}
		state, _, _ := unstructured.NestedString(g.Object, "status", "state")
		return state == "Active", nil
	}))
	t.Log("Graph became Active after conflict resolution")

	// 6. Verify the ConfigMap now has the Graph's desired value.
	recreated := &unstructured.Unstructured{}
	recreated.SetGroupVersionKind(cmGVK)
	require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{
		Name: "release-cm", Namespace: ns,
	}, recreated))
	data, _, _ := unstructured.NestedStringMap(recreated.Object, "data")
	assert.Equal(t, "graph-value", data["key"], "Graph should own the ConfigMap after conflict resolution")
}

// TestHashSkipApplyOnUnchangedSpec verifies that a reconcile of an unchanged
// Graph doesn't re-apply resources — the template hash annotation matches,
// so the Patch is skipped and the resource's resourceVersion is stable.
func TestHashSkipApplyOnUnchangedSpec(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-hash-skip",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "cm",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "hash-target"},
							"data": map[string]any{
								"stable": "value",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for Active
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 10*time.Second, true, func(ctx2 context.Context) (bool, error) {
		g := &unstructured.Unstructured{}
		g.SetGroupVersionKind(GraphGVK)
		if err := k8sClient.Get(ctx2, types.NamespacedName{Name: "test-hash-skip", Namespace: ns}, g); err != nil {
			return false, nil
		}
		state, _, _ := unstructured.NestedString(g.Object, "status", "state")
		return state == "Active", nil
	}))

	// Read the ConfigMap and record its resourceVersion.
	cm := &unstructured.Unstructured{}
	cm.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "hash-target", Namespace: ns}, cm))
	rv := cm.GetResourceVersion()
	t.Logf("ConfigMap resourceVersion after first reconcile: %s", rv)

	// Verify template hash annotation is present.
	annotations := cm.GetAnnotations()
	require.NotEmpty(t, annotations["internal.kro.run/template-hash"], "template hash annotation should be present")
	t.Logf("template hash: %s", annotations["internal.kro.run/template-hash"])

	// Trigger a reconcile by touching the Graph's labels (doesn't change spec/generation).
	graphLatest := &unstructured.Unstructured{}
	graphLatest.SetGroupVersionKind(GraphGVK)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "test-hash-skip", Namespace: ns}, graphLatest))
	labels := graphLatest.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels["trigger"] = "reconcile"
	graphLatest.SetLabels(labels)
	require.NoError(t, k8sClient.Update(ctx, graphLatest))
	t.Log("triggered reconcile via label change")

	// Wait for the reconcile to process (give it a few cycles).
	time.Sleep(3 * time.Second)

	// Verify the ConfigMap's resourceVersion has NOT changed.
	cmAfter := &unstructured.Unstructured{}
	cmAfter.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "hash-target", Namespace: ns}, cmAfter))
	assert.Equal(t, rv, cmAfter.GetResourceVersion(),
		"ConfigMap resourceVersion should be unchanged — hash match should skip Patch")
	t.Log("ConfigMap resourceVersion stable — hash-gated apply skip confirmed")
}

// TestHashAppliesOnSpecChange verifies that changing the Graph spec produces a
// new template hash and triggers a Patch.
func TestHashAppliesOnSpecChange(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-hash-change",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "cm",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "hash-change-target"},
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

	// Wait for Active
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 10*time.Second, true, func(ctx2 context.Context) (bool, error) {
		g := &unstructured.Unstructured{}
		g.SetGroupVersionKind(GraphGVK)
		if err := k8sClient.Get(ctx2, types.NamespacedName{Name: "test-hash-change", Namespace: ns}, g); err != nil {
			return false, nil
		}
		state, _, _ := unstructured.NestedString(g.Object, "status", "state")
		return state == "Active", nil
	}))

	// Record original hash.
	cm := &unstructured.Unstructured{}
	cm.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "hash-change-target", Namespace: ns}, cm))
	originalHash := cm.GetAnnotations()["internal.kro.run/template-hash"]
	require.NotEmpty(t, originalHash)
	t.Logf("original template hash: %s", originalHash)

	// Update the Graph spec.
	graphLatest := &unstructured.Unstructured{}
	graphLatest.SetGroupVersionKind(GraphGVK)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "test-hash-change", Namespace: ns}, graphLatest))
	nodes := []any{
		map[string]any{
			"id": "cm",
			"template": map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]any{"name": "hash-change-target"},
				"data": map[string]any{
					"version": "v2",
				},
			},
		},
	}
	unstructured.SetNestedSlice(graphLatest.Object, nodes, "spec", "nodes")
	require.NoError(t, k8sClient.Update(ctx, graphLatest))
	t.Log("updated Graph spec: version=v2")

	// Wait for the new value and a new hash.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 10*time.Second, true, func(ctx2 context.Context) (bool, error) {
		check := &unstructured.Unstructured{}
		check.SetGroupVersionKind(cmGVK)
		if err := k8sClient.Get(ctx2, types.NamespacedName{Name: "hash-change-target", Namespace: ns}, check); err != nil {
			return false, nil
		}
		data, _, _ := unstructured.NestedStringMap(check.Object, "data")
		return data["version"] == "v2", nil
	}))

	// Verify hash changed.
	cmAfter := &unstructured.Unstructured{}
	cmAfter.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "hash-change-target", Namespace: ns}, cmAfter))
	newHash := cmAfter.GetAnnotations()["internal.kro.run/template-hash"]
	assert.NotEqual(t, originalHash, newHash, "template hash should change after spec update")
	t.Logf("new template hash: %s — hash invalidation confirmed", newHash)
}

// TestSteadyStateNoStatusWrite verifies that a fully converged Graph makes
// zero API calls — the Graph object's resourceVersion is stable across
// multiple potential reconcile cycles.
func TestSteadyStateNoStatusWrite(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-steady-state",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "cm",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "steady-state-cm"},
							"data":       map[string]any{"key": "value"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for Active.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 10*time.Second, true, func(ctx2 context.Context) (bool, error) {
		g := &unstructured.Unstructured{}
		g.SetGroupVersionKind(GraphGVK)
		if err := k8sClient.Get(ctx2, types.NamespacedName{Name: "test-steady-state", Namespace: ns}, g); err != nil {
			return false, nil
		}
		state, _, _ := unstructured.NestedString(g.Object, "status", "state")
		return state == "Active", nil
	}))

	// Let it settle — wait for any in-flight reconciles to complete.
	time.Sleep(2 * time.Second)

	// Record the Graph's resourceVersion.
	g := &unstructured.Unstructured{}
	g.SetGroupVersionKind(GraphGVK)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "test-steady-state", Namespace: ns}, g))
	graphRV := g.GetResourceVersion()
	t.Logf("Graph resourceVersion after settling: %s", graphRV)

	// Also record the ConfigMap's resourceVersion.
	cm := &unstructured.Unstructured{}
	cm.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "steady-state-cm", Namespace: ns}, cm))
	cmRV := cm.GetResourceVersion()
	t.Logf("ConfigMap resourceVersion after settling: %s", cmRV)

	// Wait several potential reconcile cycles.
	time.Sleep(5 * time.Second)

	// Verify neither object's resourceVersion changed.
	gAfter := &unstructured.Unstructured{}
	gAfter.SetGroupVersionKind(GraphGVK)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "test-steady-state", Namespace: ns}, gAfter))
	assert.Equal(t, graphRV, gAfter.GetResourceVersion(),
		"Graph resourceVersion should be stable — no annotation or status writes in steady state")

	cmAfter := &unstructured.Unstructured{}
	cmAfter.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "steady-state-cm", Namespace: ns}, cmAfter))
	assert.Equal(t, cmRV, cmAfter.GetResourceVersion(),
		"ConfigMap resourceVersion should be stable — hash match should skip Patch")

	t.Log("steady state confirmed — zero API writes for both Graph and ConfigMap")
}

// TestDeletionSkipsConflictedResources verifies that resources in NodeConflict
// (never successfully applied by the controller) are not deleted when the Graph
// is deleted, while resources the controller did create are cleaned up.
func TestDeletionSkipsConflictedResources(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// 1. Create a ConfigMap owned by an external manager.
	applyConfigMapAs(t, ns, "external-owned-cm", "external-manager", map[string]string{
		"owner": "external",
	})
	cm := &unstructured.Unstructured{}
	cm.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "external-owned-cm", Namespace: ns}, cm))

	// 2. Create a Graph that targets the external CM (conflict) and creates its own CM.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-delete-conflict",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "conflicted",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "external-owned-cm"},
							"data":       map[string]any{"owner": "graph"},
						},
					},
					map[string]any{
						"id": "owned",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "graph-owned-cm"},
							"data":       map[string]any{"owner": "graph"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// 3. Wait for the owned ConfigMap to be created.
	ownedCM := &unstructured.Unstructured{}
	ownedCM.SetGroupVersionKind(cmGVK)
	require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{
		Name: "graph-owned-cm", Namespace: ns,
	}, ownedCM))
	t.Log("graph-owned-cm created")

	// 4. Wait for FieldConflict status.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 10*time.Second, true, func(ctx2 context.Context) (bool, error) {
		g := &unstructured.Unstructured{}
		g.SetGroupVersionKind(GraphGVK)
		if err := k8sClient.Get(ctx2, types.NamespacedName{Name: "test-delete-conflict", Namespace: ns}, g); err != nil {
			return false, nil
		}
		conditions, _, _ := unstructured.NestedSlice(g.Object, "status", "conditions")
		for _, c := range conditions {
			cm, ok := c.(map[string]any)
			if !ok {
				continue
			}
			if cm["type"] == "Ready" && cm["reason"] == "FieldConflict" {
				return true, nil
			}
		}
		return false, nil
	}))
	t.Log("Graph shows FieldConflict")

	// 5. Delete the Graph.
	graphLatest := &unstructured.Unstructured{}
	graphLatest.SetGroupVersionKind(GraphGVK)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "test-delete-conflict", Namespace: ns}, graphLatest))
	require.NoError(t, k8sClient.Delete(ctx, graphLatest))
	t.Log("Graph deleted")

	// 6. Wait for Graph to be fully deleted.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 10*time.Second, true, func(ctx2 context.Context) (bool, error) {
		g := &unstructured.Unstructured{}
		g.SetGroupVersionKind(GraphGVK)
		err := k8sClient.Get(ctx2, types.NamespacedName{Name: "test-delete-conflict", Namespace: ns}, g)
		return err != nil, nil // gone = success
	}))
	t.Log("Graph fully deleted")

	// 7. The external ConfigMap should still exist (never successfully applied).
	externalCM := &unstructured.Unstructured{}
	externalCM.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "external-owned-cm", Namespace: ns}, externalCM),
		"external-owned-cm should survive Graph deletion — was never successfully applied")
	data, _, _ := unstructured.NestedStringMap(externalCM.Object, "data")
	assert.Equal(t, "external", data["owner"], "external-owned-cm should retain external value")
	t.Log("external-owned-cm survived Graph deletion")

	// 8. The graph-owned ConfigMap should be deleted.
	deletedCM := &unstructured.Unstructured{}
	deletedCM.SetGroupVersionKind(cmGVK)
	err := k8sClient.Get(ctx, types.NamespacedName{Name: "graph-owned-cm", Namespace: ns}, deletedCM)
	assert.Error(t, err, "graph-owned-cm should be deleted during Graph cleanup")
	t.Log("graph-owned-cm deleted during cleanup — deletion semantics confirmed")
}
