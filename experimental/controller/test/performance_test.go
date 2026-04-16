package graphcontroller_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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
			"apiVersion": "experimental.kro.run/v1alpha1",
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
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx2 context.Context) (bool, error) {
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
			if cm["type"] == "Ready" && cm["reason"] == "Conflict" {
				return true, nil
			}
		}
		return false, nil
	}))
	t.Log("Graph status shows Conflict")

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
			"apiVersion": "experimental.kro.run/v1alpha1",
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
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx2 context.Context) (bool, error) {
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
			if cm["type"] == "Ready" && cm["reason"] == "Conflict" {
				return true, nil
			}
		}
		return false, nil
	}))
	t.Log("Graph shows Conflict — now releasing external ownership")

	// 4. Release the external manager's ownership by deleting the ConfigMap.
	// The Graph controller will recreate it on the next reconcile.
	require.NoError(t, k8sClient.Delete(ctx, cm))
	t.Log("deleted external ConfigMap — Graph should recreate it")

	// 5. Wait for Graph to become Active. The controller must detect the
	// deletion via dynamic watch, then reconcile and recreate the resource.
	// The controller may be in exponential backoff from the repeated 409s
	// (default backoff caps at 16s), so use a generous timeout.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 60*time.Second, true, func(ctx2 context.Context) (bool, error) {
		g := &unstructured.Unstructured{}
		g.SetGroupVersionKind(GraphGVK)
		if err := k8sClient.Get(ctx2, types.NamespacedName{Name: "test-conflict-resolve", Namespace: ns}, g); err != nil {
			return false, nil
		}
		return graphReady(g), nil
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
			"apiVersion": "experimental.kro.run/v1alpha1",
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
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx2 context.Context) (bool, error) {
		g := &unstructured.Unstructured{}
		g.SetGroupVersionKind(GraphGVK)
		if err := k8sClient.Get(ctx2, types.NamespacedName{Name: "test-hash-skip", Namespace: ns}, g); err != nil {
			return false, nil
		}
		return graphReady(g), nil
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
	require.NoError(t, updateWithRetry(ctx, k8sClient, GraphGVK,
		types.NamespacedName{Name: "test-hash-skip", Namespace: ns}, func(obj *unstructured.Unstructured) {
			labels := obj.GetLabels()
			if labels == nil {
				labels = map[string]string{}
			}
			labels["trigger"] = "reconcile"
			obj.SetLabels(labels)
		}))
	t.Log("triggered reconcile via label change")

	// Wait for the reconcile to settle (RV stability check, not a fixed sleep).
	require.NoError(t, waitForSettle(ctx, k8sClient, cmGVK, types.NamespacedName{Name: "hash-target", Namespace: ns}))

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
			"apiVersion": "experimental.kro.run/v1alpha1",
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
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx2 context.Context) (bool, error) {
		g := &unstructured.Unstructured{}
		g.SetGroupVersionKind(GraphGVK)
		if err := k8sClient.Get(ctx2, types.NamespacedName{Name: "test-hash-change", Namespace: ns}, g); err != nil {
			return false, nil
		}
		return graphReady(g), nil
	}))

	// Record original hash.
	cm := &unstructured.Unstructured{}
	cm.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "hash-change-target", Namespace: ns}, cm))
	originalHash := cm.GetAnnotations()["internal.kro.run/template-hash"]
	require.NotEmpty(t, originalHash)
	t.Logf("original template hash: %s", originalHash)

	// Update the Graph spec.
	require.NoError(t, updateWithRetry(ctx, k8sClient, GraphGVK,
		types.NamespacedName{Name: "test-hash-change", Namespace: ns}, func(obj *unstructured.Unstructured) {
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
			unstructured.SetNestedSlice(obj.Object, nodes, "spec", "nodes")
		}))
	t.Log("updated Graph spec: version=v2")

	// Wait for the new value and a new hash.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx2 context.Context) (bool, error) {
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
			"apiVersion": "experimental.kro.run/v1alpha1",
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
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx2 context.Context) (bool, error) {
		g := &unstructured.Unstructured{}
		g.SetGroupVersionKind(GraphGVK)
		if err := k8sClient.Get(ctx2, types.NamespacedName{Name: "test-steady-state", Namespace: ns}, g); err != nil {
			return false, nil
		}
		return graphReady(g), nil
	}))

	// Let it settle — poll for RV stability rather than fixed sleep.
	require.NoError(t, waitForSettle(ctx, k8sClient, GraphGVK, types.NamespacedName{Name: "test-steady-state", Namespace: ns}))

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

	// Wait for potential reconcile cycles — poll for stability, not fixed sleep.
	require.NoError(t, waitForSettle(ctx, k8sClient, GraphGVK, types.NamespacedName{Name: "test-steady-state", Namespace: ns}))

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
			"apiVersion": "experimental.kro.run/v1alpha1",
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
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx2 context.Context) (bool, error) {
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
			if cm["type"] == "Ready" && cm["reason"] == "Conflict" {
				return true, nil
			}
		}
		return false, nil
	}))
	t.Log("Graph shows Conflict")

	// 5. Delete the Graph.
	graphLatest := &unstructured.Unstructured{}
	graphLatest.SetGroupVersionKind(GraphGVK)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "test-delete-conflict", Namespace: ns}, graphLatest))
	require.NoError(t, k8sClient.Delete(ctx, graphLatest))
	t.Log("Graph deleted")

	// 6. Wait for Graph to be fully deleted.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx2 context.Context) (bool, error) {
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

// TestDedicatedFieldManagerName verifies that each Graph uses a dedicated SSA
// field manager in the format `<name>.<namespace>.internal.kro.run`.
//
// Design 003-ownership § Field Manager:
//
//	"Each Graph instance gets a dedicated SSA field manager:
//	<name>.<namespace>.internal.kro.run"
//
// The field manager name is the key that makes per-Graph field ownership
// scoped and independent. If the format is wrong, multi-graph coexistence
// breaks because field managers can't be distinguished by namespace/name.
func TestDedicatedFieldManagerName(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	graphName := "test-field-manager"
	expectedManager := graphName + "." + ns + ".internal.kro.run"

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      graphName,
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "cm",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "field-manager-cm"},
							"data":       map[string]any{"key": "value"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: graphName, Namespace: ns}))

	// Read the managed ConfigMap and inspect its managedFields.
	cm := &unstructured.Unstructured{}
	cm.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "field-manager-cm", Namespace: ns}, cm))

	managedFields := cm.GetManagedFields()
	require.NotEmpty(t, managedFields, "ConfigMap must have managedFields entries")

	// Find the kro-owned field manager entry.
	var foundManager string
	for _, mf := range managedFields {
		if mf.Manager == expectedManager {
			foundManager = mf.Manager
			break
		}
	}

	assert.Equal(t, expectedManager, foundManager,
		"managed resource must have a field manager in format <name>.<ns>.internal.kro.run")
	t.Logf("Field manager verified: %s", foundManager)
}

// TestIdempotentReReconcileZeroWrites proves that re-reconciling a converged
// Graph with no spec change produces zero API writes to ALL managed resources
// (design 004-graph-reconciliation § Propagation: change check hash match → skip).
//
// This extends TestSteadyStateNoStatusWrite by checking every managed resource's
// resourceVersion, not just the Graph object. The most common production
// reconcile is a no-op triggered by requeue or informer resync — any
// unnecessary update shows up as excess API load at scale.
func TestIdempotentReReconcileZeroWrites(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Create a multi-resource Graph to verify across all managed objects.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-idempotent",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "configA",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "idempotent-a"},
							"data":       map[string]any{"key": "a"},
						},
					},
					map[string]any{
						"id": "configB",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "${configA.data.key}-idempotent-b"},
							"data":       map[string]any{"from": "${configA.data.key}"},
						},
					},
					map[string]any{
						"id": "configC",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "${configB.data.from}-idempotent-c"},
							"data": map[string]any{
								"fromA": "${configA.data.key}",
								"fromB": "${configB.data.from}",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for convergence.
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-idempotent", Namespace: ns}))
	require.NoError(t, waitForSettle(ctx, k8sClient, GraphGVK,
		types.NamespacedName{Name: "test-idempotent", Namespace: ns}))

	// Record resourceVersions for all managed resources.
	resourceNames := []string{"idempotent-a", "a-idempotent-b", "a-idempotent-c"}
	rvBefore := map[string]string{}
	for _, name := range resourceNames {
		cm := &unstructured.Unstructured{}
		cm.SetGroupVersionKind(cmGVK)
		require.NoError(t, k8sClient.Get(ctx,
			types.NamespacedName{Name: name, Namespace: ns}, cm))
		rvBefore[name] = cm.GetResourceVersion()
	}

	// Record Graph and revision RVs too.
	g := &unstructured.Unstructured{}
	g.SetGroupVersionKind(GraphGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "test-idempotent", Namespace: ns}, g))
	graphRV := g.GetResourceVersion()
	t.Logf("Recorded RVs: graph=%s resources=%v", graphRV, rvBefore)

	// Trigger a reconcile by touching the Graph's labels (doesn't change spec).
	require.NoError(t, updateWithRetry(ctx, k8sClient, GraphGVK,
		types.NamespacedName{Name: "test-idempotent", Namespace: ns}, func(obj *unstructured.Unstructured) {
			labels := obj.GetLabels()
			if labels == nil {
				labels = map[string]string{}
			}
			labels["trigger"] = "idempotency-check"
			obj.SetLabels(labels)
		}))
	t.Log("Triggered reconcile via label change")

	// Wait for the reconcile to settle.
	require.NoError(t, waitForSettle(ctx, k8sClient, GraphGVK,
		types.NamespacedName{Name: "test-idempotent", Namespace: ns}))

	// THE KEY ASSERTIONS: verify all managed resources have unchanged RVs.
	for _, name := range resourceNames {
		cm := &unstructured.Unstructured{}
		cm.SetGroupVersionKind(cmGVK)
		require.NoError(t, k8sClient.Get(ctx,
			types.NamespacedName{Name: name, Namespace: ns}, cm))
		assert.Equal(t, rvBefore[name], cm.GetResourceVersion(),
			"ConfigMap %s resourceVersion should be unchanged — idempotent reconcile", name)
	}
	t.Log("All managed resources have stable resourceVersions — idempotent re-reconcile proved")
}

// TestPropagationStopsOnIrrelevantChange verifies the design's core performance
// claim: "If [propagation-hash matches], propagation stops."
// (004-graph-reconciliation.md § Propagation, step 8)
//
// The test creates a 3-node chain: source (Watch) → middle (Own) → leaf (Own).
// middle references source.data.version. leaf references middle.data.fromSource.
// After convergence, the test changes source.data.irrelevant — a field middle
// does NOT reference. The propagation-hash for source covers only the paths
// middle references (data.version), which didn't change. Therefore:
//
//   - middle is NOT propagation-triggered → retains previous state, skip
//   - leaf is NOT propagation-triggered → retains previous state, skip
//
// Both managed resources' resourceVersions must be stable — proving that
// the walk used the propagation-hash to skip downstream evaluation.
//
// The test uses two levels of assertion: (1) resourceVersion stability on
// middle and leaf, and (2) controller log scraping to verify middle was
// applied exactly once (initial creation only, not re-applied after the
// irrelevant field change). The log assertion distinguishes "propagation
// stopped" from "evaluated but same output" — the latter would produce a
// second "applied resource" log entry even if the resourceVersion is stable
// (because apply-hash idempotency catches duplicates before the API write).
func TestPropagationStopsOnIrrelevantChange(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// 1. Create the watch target with both a relevant and irrelevant field.
	source := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "prop-source",
				"namespace": ns,
			},
			"data": map[string]any{
				"version":    "v1",
				"irrelevant": "aaa",
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, source))

	// 2. Create a Graph with 3 nodes: source → middle → leaf.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-prop-stop",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "source",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "prop-source"},
						},
					},
					map[string]any{
						"id": "middle",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "prop-middle"},
							"data": map[string]any{
								"fromSource": "${source.data.version}",
							},
						},
					},
					map[string]any{
						"id": "leaf",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "prop-leaf"},
							"data": map[string]any{
								"fromMiddle": "${middle.data.fromSource}",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// 3. Wait for full convergence.
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-prop-stop", Namespace: ns}))

	// Let it settle.
	require.NoError(t, waitForSettle(ctx, k8sClient, GraphGVK,
		types.NamespacedName{Name: "test-prop-stop", Namespace: ns}))

	// 4. Record resourceVersions for middle and leaf.
	middle := &unstructured.Unstructured{}
	middle.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "prop-middle", Namespace: ns}, middle))
	middleRV := middle.GetResourceVersion()

	leaf := &unstructured.Unstructured{}
	leaf.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "prop-leaf", Namespace: ns}, leaf))
	leafRV := leaf.GetResourceVersion()
	t.Logf("Before: middle RV=%s, leaf RV=%s", middleRV, leafRV)

	// 4b. Capture the baseline apply count for middle BEFORE the irrelevant
	// update. Convergence may legitimately apply middle more than once
	// (e.g., previousSelfHashes starts empty), so we only assert that no
	// NEW applies happen after the irrelevant field change.
	middleApplyCountBefore := countApplyLines(t, "prop-middle")
	t.Logf("Middle apply count before irrelevant update: %d", middleApplyCountBefore)

	// 5. Update the watch target — change ONLY the irrelevant field.
	// middle references source.data.version, NOT source.data.irrelevant.
	latestSource := &unstructured.Unstructured{}
	latestSource.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "prop-source", Namespace: ns}, latestSource))
	unstructured.SetNestedField(latestSource.Object, "bbb", "data", "irrelevant")
	require.NoError(t, k8sClient.Update(ctx, latestSource))
	t.Log("Updated source.data.irrelevant=bbb (source.data.version unchanged)")

	// 6. Wait for the reconcile triggered by the watch event to settle.
	// The source node evaluates (watch trigger), but propagation stops because
	// the propagation-hash (scoped to data.version) didn't change.
	require.NoError(t, waitForSettle(ctx, k8sClient, GraphGVK,
		types.NamespacedName{Name: "test-prop-stop", Namespace: ns}))

	// 7. THE KEY ASSERTIONS: middle and leaf must NOT have been re-applied.
	middleAfter := &unstructured.Unstructured{}
	middleAfter.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "prop-middle", Namespace: ns}, middleAfter))
	assert.Equal(t, middleRV, middleAfter.GetResourceVersion(),
		"middle resourceVersion should be unchanged — propagation stopped at source")

	leafAfter := &unstructured.Unstructured{}
	leafAfter.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "prop-leaf", Namespace: ns}, leafAfter))
	assert.Equal(t, leafRV, leafAfter.GetResourceVersion(),
		"leaf resourceVersion should be unchanged — propagation stopped at source")

	// 8. Stronger assertion: verify middle was not re-applied by scanning
	// the controller log for apply events. Compare against the baseline
	// captured before the irrelevant update.
	middleApplyCountAfter := countApplyLines(t, "prop-middle")
	t.Logf("Middle apply count after irrelevant update: %d", middleApplyCountAfter)
	assert.Equal(t, middleApplyCountBefore, middleApplyCountAfter,
		"middle should not have been re-applied after the irrelevant field change — "+
			"additional applies indicate propagation did not stop")

	t.Log("Propagation stopped — irrelevant field change did not re-apply downstream resources")
}

// countApplyLines counts "applied resource" log entries for a given resource
// name in the controller log. Used by propagation-stop tests to verify that
// downstream nodes were not re-applied.
func countApplyLines(t *testing.T, resourceName string) int {
	t.Helper()
	wd, err := os.Getwd()
	require.NoError(t, err)
	moduleRoot := wd
	for {
		if _, err := os.Stat(filepath.Join(moduleRoot, "go.mod")); err == nil {
			break
		}
		moduleRoot = filepath.Dir(moduleRoot)
	}
	logPath := filepath.Join(moduleRoot, "build", "controller.log")
	logData, err := os.ReadFile(logPath)
	require.NoError(t, err, "reading controller log")
	var count int
	for _, line := range strings.Split(string(logData), "\n") {
		if strings.Contains(line, "applied resource") && strings.Contains(line, resourceName) {
			count++
		}
	}
	return count
}
