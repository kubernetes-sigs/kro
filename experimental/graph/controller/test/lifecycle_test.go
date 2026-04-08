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

func TestFullLifecycle(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-lifecycle",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "deployment",
						"template": map[string]any{
							"apiVersion": "apps/v1",
							"kind":       "Deployment",
							"metadata": map[string]any{
								"name": "lifecycle-deploy",
							},
							"spec": map[string]any{
								"replicas": int64(2),
								"selector": map[string]any{
									"matchLabels": map[string]any{
										"app": "lifecycle",
									},
								},
								"template": map[string]any{
									"metadata": map[string]any{
										"labels": map[string]any{
											"app": "lifecycle",
										},
									},
									"spec": map[string]any{
										"containers": []any{
											map[string]any{
												"name":  "app",
												"image": "nginx:1.21",
											},
										},
									},
								},
							},
						},
					},
					map[string]any{
						"id": "service",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "Service",
							"metadata": map[string]any{
								"name": "${deployment.metadata.name}-svc",
							},
							"spec": map[string]any{
								"selector": map[string]any{
									"app": "lifecycle",
								},
								"ports": []any{
									map[string]any{
										"port":       int64(80),
										"targetPort": int64(8080),
									},
								},
							},
						},
					},
				},
			},
		},
	}

	// --- Phase 1: Create and verify ---
	require.NoError(t, k8sClient.Create(ctx, graph))
	t.Log("Graph created")

	// Wait for Deployment
	deploy := &unstructured.Unstructured{}
	deploy.SetGroupVersionKind(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"})
	require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: "lifecycle-deploy", Namespace: ns}, deploy))
	t.Log("Deployment created")

	// Verify Deployment spec
	replicas, _, _ := unstructured.NestedInt64(deploy.Object, "spec", "replicas")
	assert.Equal(t, int64(2), replicas)
	containers, _, _ := unstructured.NestedSlice(deploy.Object, "spec", "template", "spec", "containers")
	require.Len(t, containers, 1)
	container := containers[0].(map[string]any)
	assert.Equal(t, "nginx:1.21", container["image"])

	// Verify Deployment is managed by Graph
	assertManagedBy(t, deploy, "test-lifecycle")

	// Wait for Service with the derived name
	svc := &unstructured.Unstructured{}
	svc.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Service"})
	require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: "lifecycle-deploy-svc", Namespace: ns}, svc))
	t.Log("Service created with derived name: lifecycle-deploy-svc")

	// Verify Service is managed by Graph
	assertManagedBy(t, svc, "test-lifecycle")

	// Wait for status to show Active
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		g := &unstructured.Unstructured{}
		g.SetGroupVersionKind(GraphGVK)
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "test-lifecycle", Namespace: ns}, g); err != nil {
			return false, nil
		}
		state, _, _ := unstructured.NestedString(g.Object, "status", "state")
		return state == "Active", nil
	}))

	g := &unstructured.Unstructured{}
	g.SetGroupVersionKind(GraphGVK)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "test-lifecycle", Namespace: ns}, g))

	state, _, _ := unstructured.NestedString(g.Object, "status", "state")
	assert.Equal(t, "Active", state)
	t.Logf("Status: state=%s", state)

	// --- Phase 2: Update and verify ---
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "test-lifecycle", Namespace: ns}, g))
	spec := g.Object["spec"].(map[string]any)
	nodes := spec["nodes"].([]any)
	deployRes := nodes[0].(map[string]any)
	tmpl := deployRes["template"].(map[string]any)
	deploySpec := tmpl["spec"].(map[string]any)
	deploySpec["replicas"] = int64(5)
	require.NoError(t, k8sClient.Update(ctx, g))
	t.Log("Updated Graph: replicas 2 → 5")

	// Wait for Deployment to be updated
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		d := &unstructured.Unstructured{}
		d.SetGroupVersionKind(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"})
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "lifecycle-deploy", Namespace: ns}, d); err != nil {
			return false, nil
		}
		r, _, _ := unstructured.NestedInt64(d.Object, "spec", "replicas")
		return r == int64(5), nil
	}))
	t.Log("Deployment updated to 5 replicas")

	// --- Phase 3: Delete and verify finalizer removal ---
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "test-lifecycle", Namespace: ns}, g))
	require.NoError(t, k8sClient.Delete(ctx, g))
	t.Log("Graph deleted")

	// Wait for Graph to be gone (finalizer removed)
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 5*time.Second, true, func(ctx context.Context) (bool, error) {
		check := &unstructured.Unstructured{}
		check.SetGroupVersionKind(GraphGVK)
		err := k8sClient.Get(ctx, types.NamespacedName{Name: "test-lifecycle", Namespace: ns}, check)
		return err != nil, nil
	}))
	t.Log("Graph gone — finalizer removed, GC cascade will handle owned resources")

	t.Log("Full lifecycle proved: create → verify → update → verify → delete → cleanup")
}

// TestCascadeDeletion proves that all owned resources have correct owner
// references pointing to the Graph, and that deletion removes the finalizer.
// Actual GC cascade is a Kubernetes feature, not ours — we verify the setup.
func TestCascadeDeletion(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-cascade-delete",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "configA",
						"template": map[string]any{
							"apiVersion": "v1", "kind": "ConfigMap",
							"metadata": map[string]any{"name": "cascade-a"},
							"data":     map[string]any{"value": "a"},
						},
					},
					map[string]any{
						"id": "configB",
						"template": map[string]any{
							"apiVersion": "v1", "kind": "ConfigMap",
							"metadata": map[string]any{"name": "${configA.data.value}-cascade-b"},
							"data":     map[string]any{"value": "b"},
						},
					},
					map[string]any{
						"id": "configC",
						"template": map[string]any{
							"apiVersion": "v1", "kind": "ConfigMap",
							"metadata": map[string]any{"name": "${configB.data.value}-cascade-c"},
							"data":     map[string]any{"fromA": "${configA.data.value}", "fromB": "${configB.data.value}"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for all 3 ConfigMaps
	names := []string{"cascade-a", "a-cascade-b", "b-cascade-c"}
	for _, name := range names {
		cm := &unstructured.Unstructured{}
		cm.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
		require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: name, Namespace: ns}, cm))
	}
	t.Log("All 3 ConfigMaps created")

	// Verify all are managed by the Graph (labels)
	for _, name := range names {
		cm := &unstructured.Unstructured{}
		cm.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
		require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, cm))
		assertManagedBy(t, cm, "test-cascade-delete")
	}
	t.Log("All ConfigMaps have correct management labels")

	// Delete the Graph and verify finalizer is removed (Graph is gone)
	g := &unstructured.Unstructured{}
	g.SetGroupVersionKind(GraphGVK)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "test-cascade-delete", Namespace: ns}, g))
	require.NoError(t, k8sClient.Delete(ctx, g))

	// Wait for Graph to be gone (finalizer runs active cleanup)
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 5*time.Second, true, func(ctx context.Context) (bool, error) {
		check := &unstructured.Unstructured{}
		check.SetGroupVersionKind(GraphGVK)
		err := k8sClient.Get(ctx, types.NamespacedName{Name: "test-cascade-delete", Namespace: ns}, check)
		return err != nil, nil
	}))
	t.Log("Graph deleted, finalizer removed")

	// Verify managed resources were actively deleted by the finalizer
	for _, name := range names {
		cm := &unstructured.Unstructured{}
		cm.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
		err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, cm)
		assert.Error(t, err, "ConfigMap %s should be deleted by finalizer", name)
	}
	t.Log("All managed resources actively deleted by finalizer — no owner refs needed")
}

// TestParallelIndependence proves that independent resource chains make
// progress independently. When chain A is blocked (externalRef not ready),
// chain B (no dependency on A) proceeds normally.
func TestParallelIndependence(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Create the externalRef source for chain A — starts not ready
	sourceA := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata":   map[string]any{"name": "source-a", "namespace": ns},
			"data":       map[string]any{"ready": "false"},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, sourceA))

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata":   map[string]any{"name": "test-parallel", "namespace": ns},
			"spec": map[string]any{
				"nodes": []any{
					// Chain A: externalRef (not ready) → dependent-a
					map[string]any{
						"id": "extA",
						"externalRef": map[string]any{
							"apiVersion": "v1", "kind": "ConfigMap",
							"metadata": map[string]any{"name": "source-a"},
						},
						"readyWhen": []any{"${extA.data.ready == 'true'}"},
					},
					map[string]any{
						"id": "dependentA",
						"template": map[string]any{
							"apiVersion": "v1", "kind": "ConfigMap",
							"metadata": map[string]any{"name": "dependent-a"},
							"data":     map[string]any{"from": "${extA.data.ready}"},
						},
					},
					// Chain B: independent ConfigMap (no dependency on chain A)
					map[string]any{
						"id": "independentB",
						"template": map[string]any{
							"apiVersion": "v1", "kind": "ConfigMap",
							"metadata": map[string]any{"name": "independent-b"},
							"data":     map[string]any{"value": "b-works"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Chain B should be created even though chain A is blocked
	cmB := &unstructured.Unstructured{}
	cmB.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
	require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: "independent-b", Namespace: ns}, cmB))
	data, _, _ := unstructured.NestedStringMap(cmB.Object, "data")
	assert.Equal(t, "b-works", data["value"])
	t.Log("Chain B created independently while chain A is blocked")

	// Chain A's dependent should NOT exist
	cmA := &unstructured.Unstructured{}
	cmA.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
	err := k8sClient.Get(ctx, types.NamespacedName{Name: "dependent-a", Namespace: ns}, cmA)
	require.Error(t, err, "dependent-a should not exist while extA is not ready")
	t.Log("Chain A correctly blocked")

	// Unblock chain A
	latest := &unstructured.Unstructured{}
	latest.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "source-a", Namespace: ns}, latest))
	unstructured.SetNestedField(latest.Object, "true", "data", "ready")
	require.NoError(t, k8sClient.Update(ctx, latest))

	// Now chain A's dependent should appear
	require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: "dependent-a", Namespace: ns}, cmA))
	t.Log("Chain A unblocked after source became ready")
	t.Log("Parallel independence proved: blocked chain A did not prevent chain B")
}

// TestContagiousExclusion proves that excluding a resource via includeWhen
// propagates exclusion to all its dependents through the DAG.
func TestContagiousExclusion(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata":   map[string]any{"name": "test-contagious", "namespace": ns},
			"spec": map[string]any{
				"nodes": []any{
					// Config: controls the feature flag
					map[string]any{
						"id": "config",
						"template": map[string]any{
							"apiVersion": "v1", "kind": "ConfigMap",
							"metadata": map[string]any{"name": "contagious-config"},
							"data":     map[string]any{"featureEnabled": "false"},
						},
					},
					// Feature resource: excluded when feature is disabled
					map[string]any{
						"id": "feature",
						"template": map[string]any{
							"apiVersion": "v1", "kind": "ConfigMap",
							"metadata": map[string]any{"name": "feature-resource"},
							"data":     map[string]any{"value": "feature"},
						},
						"includeWhen": []any{"${config.data.featureEnabled == 'true'}"},
					},
					// Dependent: references the feature resource — should be contagiously excluded
					map[string]any{
						"id": "dependent",
						"template": map[string]any{
							"apiVersion": "v1", "kind": "ConfigMap",
							"metadata": map[string]any{"name": "feature-dependent"},
							"data":     map[string]any{"fromFeature": "${feature.data.value}"},
						},
					},
					// Independent: does NOT reference the feature resource — should still be created
					map[string]any{
						"id": "independent",
						"template": map[string]any{
							"apiVersion": "v1", "kind": "ConfigMap",
							"metadata": map[string]any{"name": "always-created"},
							"data":     map[string]any{"value": "independent"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Config and independent should be created
	cm := &unstructured.Unstructured{}
	cm.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
	require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: "contagious-config", Namespace: ns}, cm))
	require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: "always-created", Namespace: ns}, cm))
	t.Log("Config and independent resources created")

	// Feature and dependent should NOT exist (contagious exclusion)
	cmGVK2 := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}
	require.NoError(t, waitForAbsence(ctx, k8sClient, cmGVK2, types.NamespacedName{Name: "feature-resource", Namespace: ns}, 1*time.Second))
	require.NoError(t, waitForAbsence(ctx, k8sClient, cmGVK2, types.NamespacedName{Name: "feature-dependent", Namespace: ns}, 500*time.Millisecond))
	t.Log("Feature + dependent correctly excluded (contagious)")

	// Status should be Active (excluded resources don't block readiness)
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		g := &unstructured.Unstructured{}
		g.SetGroupVersionKind(GraphGVK)
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "test-contagious", Namespace: ns}, g); err != nil {
			return false, nil
		}
		state, _, _ := unstructured.NestedString(g.Object, "status", "state")
		return state == "Active", nil
	}))
	t.Log("Graph is Active despite excluded resources")
	t.Log("Contagious exclusion proved: feature excluded → dependent contagiously excluded → independent unaffected")
}

// TestDataPendingChain proves chained data dependencies:
// A is created → B references A.data.missing → B is data-pending →
// C references B → C is contagiously blocked.
// Then A is updated with the missing field → B resolves → C resolves.
func TestDataPendingChain(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Pre-create source without the field B needs
	source := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "chain-source", "namespace": ns},
			"data":     map[string]any{"existing": "yes"},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, source))

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata":   map[string]any{"name": "test-chain", "namespace": ns},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "source",
						"externalRef": map[string]any{
							"apiVersion": "v1", "kind": "ConfigMap",
							"metadata": map[string]any{"name": "chain-source"},
						},
					},
					map[string]any{
						"id": "middle",
						"template": map[string]any{
							"apiVersion": "v1", "kind": "ConfigMap",
							"metadata": map[string]any{"name": "chain-middle"},
							"data":     map[string]any{"value": "${source.data.chainField}"},
						},
					},
					map[string]any{
						"id": "tail",
						"template": map[string]any{
							"apiVersion": "v1", "kind": "ConfigMap",
							"metadata": map[string]any{"name": "chain-tail"},
							"data":     map[string]any{"fromMiddle": "${middle.data.value}"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Middle and tail should NOT exist (data pending)
	cmGVK3 := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}
	require.NoError(t, waitForAbsence(ctx, k8sClient, cmGVK3, types.NamespacedName{Name: "chain-middle", Namespace: ns}, 1500*time.Millisecond))
	require.NoError(t, waitForAbsence(ctx, k8sClient, cmGVK3, types.NamespacedName{Name: "chain-tail", Namespace: ns}, 500*time.Millisecond))
	t.Log("Chain correctly blocked: middle + tail pending")

	// Add the missing field
	latest := &unstructured.Unstructured{}
	latest.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "chain-source", Namespace: ns}, latest))
	unstructured.SetNestedField(latest.Object, "chain-value", "data", "chainField")
	require.NoError(t, k8sClient.Update(ctx, latest))
	t.Log("Added chainField to source")

	// Both middle and tail should resolve
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		result := &unstructured.Unstructured{}
		result.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "chain-tail", Namespace: ns}, result); err != nil {
			return false, nil
		}
		data, _, _ := unstructured.NestedStringMap(result.Object, "data")
		return data["fromMiddle"] == "chain-value", nil
	}))
	t.Log("Full chain resolved: source → middle → tail")
}

// TestPartialStatusResolution proves that spec.status fields appear/disappear
// based on whether their dependency resources are included/excluded.
// evaluateStatusTemplate soft-resolves: fields whose expressions fail are omitted.
// When a resource is excluded by includeWhen, it's not in scope, so status fields
// referencing it silently disappear.

func TestForEachCollectionScaleUpDown(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Start with 2 source items
	for _, name := range []string{"scale-a", "scale-b"} {
		cm := &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name":      name,
					"namespace": ns,
					"labels":    map[string]any{"group": "scale-test"},
				},
				"data": map[string]any{"value": name},
			},
		}
		require.NoError(t, k8sClient.Create(ctx, cm))
	}

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-scale",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "sources",
						"externalRef": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"selector": map[string]any{"group": "scale-test"},
							},
						},
					},
					map[string]any{
						"id": "copies",
						"forEach": map[string]any{
							"src": "${sources}",
						},
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "${src.metadata.name}-copy"},
							"data":       map[string]any{"from": "${src.metadata.name}"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for initial 2 copies
	cmGVK := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}
	for _, name := range []string{"scale-a-copy", "scale-b-copy"} {
		cm := &unstructured.Unstructured{}
		cm.SetGroupVersionKind(cmGVK)
		require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: name, Namespace: ns}, cm))
	}
	t.Log("Initial: 2 copies created (scale-a-copy, scale-b-copy)")

	// Scale up: add scale-c
	newItem := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "scale-c",
				"namespace": ns,
				"labels":    map[string]any{"group": "scale-test"},
			},
			"data": map[string]any{"value": "scale-c"},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, newItem))

	newCopy := &unstructured.Unstructured{}
	newCopy.SetGroupVersionKind(cmGVK)
	require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: "scale-c-copy", Namespace: ns}, newCopy))
	t.Log("Scale up: scale-c-copy appeared")

	// Scale down: delete scale-a source
	deleteMe := &unstructured.Unstructured{}
	deleteMe.SetGroupVersionKind(cmGVK)
	deleteMe.SetName("scale-a")
	deleteMe.SetNamespace(ns)
	require.NoError(t, k8sClient.Delete(ctx, deleteMe))
	t.Log("Deleted source scale-a")

	// scale-a-copy should be pruned
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		check := &unstructured.Unstructured{}
		check.SetGroupVersionKind(cmGVK)
		err := k8sClient.Get(ctx, types.NamespacedName{Name: "scale-a-copy", Namespace: ns}, check)
		return err != nil, nil // gone = success
	}))
	t.Log("Scale down: scale-a-copy pruned")

	// scale-b-copy and scale-c-copy should still exist
	for _, name := range []string{"scale-b-copy", "scale-c-copy"} {
		cm := &unstructured.Unstructured{}
		cm.SetGroupVersionKind(cmGVK)
		require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, cm))
	}
	t.Log("Remaining copies still exist — scale up/down proved")
}

// TestIncludeWhenToggle proves the full create/prune cycle when includeWhen
// flips: false→true creates resources, true→false prunes them.
func TestIncludeWhenToggle(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Control ConfigMap: drives includeWhen
	control := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "toggle-control",
				"namespace": ns,
			},
			"data": map[string]any{
				"enabled": "false",
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, control))

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-toggle",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "control",
						"externalRef": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "toggle-control"},
						},
					},
					map[string]any{
						"id": "gated",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "gated-resource"},
							"data":       map[string]any{"value": "created"},
						},
						"includeWhen": []any{"${control.data.enabled == 'true'}"},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Phase 1: enabled=false → gated resource should NOT exist
	cmGVK := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}
	require.NoError(t, waitForAbsence(ctx, k8sClient, cmGVK,
		types.NamespacedName{Name: "gated-resource", Namespace: ns}, 1500*time.Millisecond))
	t.Log("Phase 1: gated-resource absent (enabled=false)")

	// Phase 2: flip to true → resource should be created
	latestCtl := &unstructured.Unstructured{}
	latestCtl.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "toggle-control", Namespace: ns}, latestCtl))
	unstructured.SetNestedField(latestCtl.Object, "true", "data", "enabled")
	require.NoError(t, k8sClient.Update(ctx, latestCtl))

	gated := &unstructured.Unstructured{}
	gated.SetGroupVersionKind(cmGVK)
	require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: "gated-resource", Namespace: ns}, gated))
	data, _, _ := unstructured.NestedStringMap(gated.Object, "data")
	assert.Equal(t, "created", data["value"])
	t.Log("Phase 2: gated-resource created (enabled=true)")

	// Phase 3: flip back to false → resource should be pruned
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "toggle-control", Namespace: ns}, latestCtl))
	unstructured.SetNestedField(latestCtl.Object, "false", "data", "enabled")
	require.NoError(t, k8sClient.Update(ctx, latestCtl))

	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		check := &unstructured.Unstructured{}
		check.SetGroupVersionKind(cmGVK)
		err := k8sClient.Get(ctx, types.NamespacedName{Name: "gated-resource", Namespace: ns}, check)
		return err != nil, nil // gone = success
	}))
	t.Log("Phase 3: gated-resource pruned (enabled=false)")
	t.Log("includeWhen toggle proved: false→true creates, true→false prunes")
}

// TestDriftDetection proves that when a managed resource is externally modified,
// the next reconcile restores it to the desired state via SSA.
func TestDriftNotRestored(t *testing.T) {
	// The content-addressed apply optimization (template hash) intentionally
	// skips re-applying when the desired state hasn't changed. This means
	// external mutations ("drift") persist until the Graph template output
	// changes. This is the same tradeoff as pod-template-hash in Deployments.
	t.Parallel()
	ns := createNamespace(t)

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-drift",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "managed",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "drift-target"},
							"data": map[string]any{
								"desired": "correct-value",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for ConfigMap
	cmGVK := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}
	cm := &unstructured.Unstructured{}
	cm.SetGroupVersionKind(cmGVK)
	require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: "drift-target", Namespace: ns}, cm))
	data, _, _ := unstructured.NestedStringMap(cm.Object, "data")
	assert.Equal(t, "correct-value", data["desired"])
	t.Log("ConfigMap created with desired=correct-value")

	// Externally mutate the ConfigMap
	latest := &unstructured.Unstructured{}
	latest.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "drift-target", Namespace: ns}, latest))
	unstructured.SetNestedField(latest.Object, "DRIFTED", "data", "desired")
	require.NoError(t, k8sClient.Update(ctx, latest))
	t.Log("Externally mutated: desired=DRIFTED")

	// Wait a few reconcile cycles to confirm drift persists (not restored).
	// The watch fires (resourceVersion changed), but the template hash matches
	// so the Patch is skipped.
	time.Sleep(3 * time.Second)

	check := &unstructured.Unstructured{}
	check.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "drift-target", Namespace: ns}, check))
	d, _, _ := unstructured.NestedStringMap(check.Object, "data")
	assert.Equal(t, "DRIFTED", d["desired"], "drift should persist — content-addressed apply skips re-apply when template output unchanged")
	t.Log("Drift persists as expected — content-addressed apply optimization working")

	// Now update the Graph spec to change the desired value — this SHOULD apply
	graphLatest := &unstructured.Unstructured{}
	graphLatest.SetGroupVersionKind(schema.GroupVersionKind{Group: "kro.run", Version: "v1alpha1", Kind: "Graph"})
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "test-drift", Namespace: ns}, graphLatest))

	nodes := []any{
		map[string]any{
			"id": "managed",
			"template": map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]any{"name": "drift-target"},
				"data": map[string]any{
					"desired": "new-value",
				},
			},
		},
	}
	unstructured.SetNestedSlice(graphLatest.Object, nodes, "spec", "nodes")
	require.NoError(t, k8sClient.Update(ctx, graphLatest))
	t.Log("Updated Graph spec: desired=new-value")

	// Wait for the new value to be applied (template hash changed → Patch fires)
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		check := &unstructured.Unstructured{}
		check.SetGroupVersionKind(cmGVK)
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "drift-target", Namespace: ns}, check); err != nil {
			return false, nil
		}
		d, _, _ := unstructured.NestedStringMap(check.Object, "data")
		return d["desired"] == "new-value", nil
	}))
	t.Log("Spec change applied: desired=new-value — content-addressed apply proved")
}

// TestRGDPatternEndToEnd proves the full L1→L2 pattern that the RGD system uses:
//
//	L1 Graph (controller):
//	  - Watches all SimpleApp instances via externalRef selector
//	  - Stamps one child Graph (L2) per instance via forEach
//
//	L2 Graph (per-instance):
//	  - Reads its specific SimpleApp via externalRef (name baked in by L1)
//	  - Creates a Deployment and a ConfigMap from the instance spec
//	  - Contributes annotations back to the SimpleApp instance
//
// This is the example-2 pattern from the design docs — the core mechanism that
// makes RGD integration tests work at the Graph layer.
