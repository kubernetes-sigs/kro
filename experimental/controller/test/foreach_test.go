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

// TestForEachBasic proves that forEach stamps one resource per item in a collection.
// A Graph with a base ConfigMap and a forEach that creates one ConfigMap per value.
func TestForEachBasic(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-foreach",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					// Base config: provides the list of values to iterate
					map[string]any{
						"id": "base",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "base-config",
							},
							"data": map[string]any{
								"prefix": "app",
							},
						},
					},
					// forEach: stamp one ConfigMap per value in a CEL list literal
					map[string]any{
						"id": "items",
						"forEach": map[string]any{
							"value": "${['alpha', 'beta', 'gamma']}",
						},
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "${base.data.prefix}-${value}",
							},
							"data": map[string]any{
								"item":   "${value}",
								"source": "${base.metadata.name}",
							},
						},
					},
				},
			},
		},
	}

	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for base config
	baseCM := &unstructured.Unstructured{}
	baseCM.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
	require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: "base-config", Namespace: ns}, baseCM))

	// Wait for all 3 stamped ConfigMaps
	for _, value := range []string{"alpha", "beta", "gamma"} {
		name := "app-" + value
		cm := &unstructured.Unstructured{}
		cm.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
		require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: name, Namespace: ns}, cm),
			"forEach should create ConfigMap %s", name)

		data, _, _ := unstructured.NestedStringMap(cm.Object, "data")
		assert.Equal(t, value, data["item"], "ConfigMap %s should have item=%s", name, value)
		assert.Equal(t, "base-config", data["source"], "ConfigMap %s should reference the base config", name)

		// Each stamped resource should be managed by the Graph
		assertManagedBy(t, cm, "test-foreach")

		t.Logf("forEach created: %s (item=%s source=%s)", name, data["item"], data["source"])
	}
}

// TestForEachWithExternalRefSelector proves the full pattern:
// collection watch reads a collection from the cluster,
// forEach iterates it and stamps one resource per item.
// This is the core mechanism for "one child Graph per instance" in the RGD model.
func TestForEachWithExternalRefSelector(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Pre-create 3 ConfigMaps with a common label — these are the "instances"
	for _, name := range []string{"instance-a", "instance-b", "instance-c"} {
		cm := &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name":      name,
					"namespace": ns,
					"labels": map[string]any{
						"app": "my-kind",
					},
				},
				"data": map[string]any{
					"color": name[len("instance-"):], // "a", "b", "c"
				},
			},
		}
		require.NoError(t, k8sClient.Create(ctx, cm))
	}

	// Graph: read collection via watch with selector, forEach stamps per item
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-selector-foreach",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					// watch with selector: reads all ConfigMaps with label app=my-kind
					map[string]any{
						"id": "instances",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"selector": map[string]any{
								"app": "my-kind",
							},
						},
					},
					// forEach: stamp one ConfigMap per instance
					map[string]any{
						"id": "copies",
						"forEach": map[string]any{
							"instance": "${instances}",
						},
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "${instance.metadata.name}-copy",
							},
							"data": map[string]any{
								"originalName": "${instance.metadata.name}",
								"color":        "${instance.data.color}",
								"originalUid":  "${instance.metadata.uid}",
							},
						},
					},
				},
			},
		},
	}

	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for all 3 copied ConfigMaps
	for _, name := range []string{"instance-a", "instance-b", "instance-c"} {
		copyName := name + "-copy"
		cm := &unstructured.Unstructured{}
		cm.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
		require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: copyName, Namespace: ns}, cm),
			"forEach should create ConfigMap %s", copyName)

		data, _, _ := unstructured.NestedStringMap(cm.Object, "data")
		assert.Equal(t, name, data["originalName"])
		expectedColor := name[len("instance-"):]
		assert.Equal(t, expectedColor, data["color"])

		// The copy should have the original's UID (server-assigned) in its data
		assert.NotEmpty(t, data["originalUid"], "copy should contain the original's server-assigned UID")

		t.Logf("forEach+selector created: %s (original=%s color=%s uid=%s)",
			copyName, data["originalName"], data["color"], data["originalUid"])
	}
}

// TestForEachStampsChildGraphs proves the RGD-like pattern:
// watch with selector reads instances, forEach stamps one child Graph per instance,
// each child Graph independently reconciles with its own scope via watch.
// This is the three-level nesting that makes the RGD system work.
func TestForEachStampsChildGraphs(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Pre-create 2 "WebApp" instances (as ConfigMaps, since we don't have a real CRD)
	for _, app := range []struct{ name, image string }{
		{"webapp-foo", "nginx:latest"},
		{"webapp-bar", "redis:7"},
	} {
		cm := &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name":      app.name,
					"namespace": ns,
					"labels": map[string]any{
						"kind": "WebApp",
					},
				},
				"data": map[string]any{
					"image":    app.image,
					"replicas": "3",
				},
			},
		}
		require.NoError(t, k8sClient.Create(ctx, cm))
	}

	// Parent Graph: reads all WebApp instances, stamps one child Graph per instance.
	// Each child Graph reads its specific instance via watch and creates resources.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "webapp-controller",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					// Read all WebApp instances
					map[string]any{
						"id": "webapps",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"selector": map[string]any{
								"kind": "WebApp",
							},
						},
					},
					// Stamp one child Graph per instance
					map[string]any{
						"id": "childGraphs",
						"forEach": map[string]any{
							"webapp": "${webapps}",
						},
						"template": map[string]any{
							"apiVersion": "kro.run/v1alpha1",
							"kind":       "Graph",
							"metadata": map[string]any{
								// Evaluated at L0: each webapp's name
								"name": "${webapp.metadata.name}-graph",
							},
							"spec": map[string]any{
								"nodes": []any{
									// Child reads its specific instance by name
									map[string]any{
										"id": "schema",
										"template": map[string]any{
											"apiVersion": "v1",
											"kind":       "ConfigMap",
											"metadata": map[string]any{
												// Evaluated at L0 → concrete name
												"name": "${webapp.metadata.name}",
											},
										},
									},
									// Child creates a "deployment summary" ConfigMap
									map[string]any{
										"id": "summary",
										"template": map[string]any{
											"apiVersion": "v1",
											"kind":       "ConfigMap",
											"metadata": map[string]any{
												// $${...} stripped at L0 → ${...} evaluated at L1
												"name": "$${schema.metadata.name}-summary",
											},
											"data": map[string]any{
												"image":    "$${schema.data.image}",
												"replicas": "$${schema.data.replicas}",
												"appName":  "$${schema.metadata.name}",
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	require.NoError(t, k8sClient.Create(ctx, graph))

	// Verify child Graphs were created
	for _, appName := range []string{"webapp-foo", "webapp-bar"} {
		childGraphName := appName + "-graph"
		childGraph := &unstructured.Unstructured{}
		childGraph.SetGroupVersionKind(GraphGVK)
		require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: childGraphName, Namespace: ns}, childGraph),
			"forEach should create child Graph %s", childGraphName)
		t.Logf("L0: Child Graph %s created", childGraphName)
	}

	// Verify each child Graph independently created its summary ConfigMap
	for _, app := range []struct{ name, image string }{
		{"webapp-foo", "nginx:latest"},
		{"webapp-bar", "redis:7"},
	} {
		summaryName := app.name + "-summary"
		summary := &unstructured.Unstructured{}
		summary.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
		require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: summaryName, Namespace: ns}, summary),
			"child Graph should create summary ConfigMap %s", summaryName)

		data, _, _ := unstructured.NestedStringMap(summary.Object, "data")
		assert.Equal(t, app.image, data["image"],
			"%s should have image from the WebApp instance", summaryName)
		assert.Equal(t, "3", data["replicas"],
			"%s should have replicas from the WebApp instance", summaryName)
		assert.Equal(t, app.name, data["appName"],
			"%s should have appName from the WebApp instance", summaryName)

		// Summary should be managed by its child Graph, not the parent
		assertManagedBy(t, summary, app.name+"-graph")

		t.Logf("L1: %s created (image=%s replicas=%s appName=%s) managed by %s-graph",
			summaryName, data["image"], data["replicas"], data["appName"], app.name)
	}

	t.Log("Full RGD-like pattern proved: parent reads collection → forEach stamps child Graphs → each child reads its instance → creates resources")
}

// TestForEachReadyWhenDoesNotGateDownstream proves that per-item readyWhen on
// forEach collections is a health signal, not a gate for dependents
// (design 001-graph § readyWhen, design 004-graph-execution § Wind).
//
// Dependents proceed as soon as data is in scope regardless of readyWhen.
// Graph status shows InProgress when items aren't ready, transitions to
// Active when readyWhen passes.
func TestForEachReadyWhenDoesNotGateDownstream(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-foreach-readywhen",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					// forEach stamps 3 ConfigMaps, each with ready = "false"
					map[string]any{
						"id": "workers",
						"forEach": map[string]any{
							"value": "${['alpha', 'beta', 'gamma']}",
						},
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "worker-${value}",
							},
							"data": map[string]any{
								"item":  "${value}",
								"ready": "false",
							},
						},
						"readyWhen": []any{
							"${workers.data.ready == 'true'}",
						},
					},
					// Downstream: references the workers collection. Should be blocked
					// until all workers pass readyWhen.
					map[string]any{
						"id": "summary",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "worker-summary",
							},
							"data": map[string]any{
								// Reference workers[0] so the DAG sees the dependency
								// (extractFirstIdentifier needs the resource ID as the first token)
								"firstWorker": "${workers[0].data.item}",
							},
						},
					},
				},
			},
		},
	}

	require.NoError(t, k8sClient.Create(ctx, graph))

	// All 3 worker ConfigMaps should be created (apply-all-then-gate)
	for _, value := range []string{"alpha", "beta", "gamma"} {
		name := "worker-" + value
		cm := &unstructured.Unstructured{}
		cm.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
		require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: name, Namespace: ns}, cm),
			"forEach should create ConfigMap %s even before readyWhen passes", name)
		data, _, _ := unstructured.NestedStringMap(cm.Object, "data")
		assert.Equal(t, "false", data["ready"])
		t.Logf("Worker %s created with ready=false", name)
	}

	// readyWhen no longer gates dependents — worker-summary should be created
	// immediately because worker data is in scope. The Graph status shows InProgress.
	summaryCheck := &unstructured.Unstructured{}
	summaryCheck.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
	require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: "worker-summary", Namespace: ns}, summaryCheck))
	t.Log("Summary created while workers not ready (readyWhen no longer gates dependents)")

	// Verify Graph status is InProgress
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		g := &unstructured.Unstructured{}
		g.SetGroupVersionKind(GraphGVK)
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "test-foreach-readywhen", Namespace: ns}, g); err != nil {
			return false, nil
		}
		return graphReadyStatus(g) == "Unknown", nil
	}))
	t.Log("Graph status is InProgress (workers not ready)")

	// Update the Graph spec: change worker template to ready = "true"
	latest := &unstructured.Unstructured{}
	latest.SetGroupVersionKind(GraphGVK)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "test-foreach-readywhen", Namespace: ns}, latest))

	updatedNodes := []any{
		map[string]any{
			"id": "workers",
			"forEach": map[string]any{
				"value": "${['alpha', 'beta', 'gamma']}",
			},
			"template": map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name": "worker-${value}",
				},
				"data": map[string]any{
					"item":  "${value}",
					"ready": "true",
				},
			},
			"readyWhen": []any{
				"${workers.data.ready == 'true'}",
			},
		},
		map[string]any{
			"id": "summary",
			"template": map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name": "worker-summary",
				},
				"data": map[string]any{
					"firstWorker": "${workers[0].data.item}",
				},
			},
		},
	}
	unstructured.SetNestedSlice(latest.Object, updatedNodes, "spec", "nodes")
	require.NoError(t, k8sClient.Update(ctx, latest))
	t.Log("Updated Graph: workers now have ready=true")

	// Summary should now be created
	summary := &unstructured.Unstructured{}
	summary.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
	require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: "worker-summary", Namespace: ns}, summary))

	data, _, _ := unstructured.NestedStringMap(summary.Object, "data")
	assert.NotEmpty(t, data["firstWorker"],
		"summary should have data from the first worker")
	t.Logf("Summary created with firstWorker=%s after workers became ready", data["firstWorker"])

	// Verify Graph transitions to Active
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		g := &unstructured.Unstructured{}
		g.SetGroupVersionKind(GraphGVK)
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "test-foreach-readywhen", Namespace: ns}, g); err != nil {
			return false, nil
		}
		return graphReady(g), nil
	}))
	t.Log("Graph transitioned to Active — forEach readyWhen per-item proved")
}

// TestForEachReadyWhenPassesImmediately proves the happy path: when all forEach items
// satisfy readyWhen on first evaluation, downstream resources are created immediately
// and the Graph reaches Active state.
func TestForEachReadyWhenPassesImmediately(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-foreach-readywhen-pass",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "workers",
						"forEach": map[string]any{
							"value": "${['alpha', 'beta', 'gamma']}",
						},
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "ready-worker-${value}",
							},
							"data": map[string]any{
								"item":  "${value}",
								"ready": "true",
							},
						},
						"readyWhen": []any{
							"${workers.data.ready == 'true'}",
						},
					},
					map[string]any{
						"id": "summary",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "ready-worker-summary",
							},
							"data": map[string]any{
								"firstWorker": "${workers[0].data.item}",
							},
						},
					},
				},
			},
		},
	}

	require.NoError(t, k8sClient.Create(ctx, graph))

	// All workers should be created
	for _, value := range []string{"alpha", "beta", "gamma"} {
		name := "ready-worker-" + value
		cm := &unstructured.Unstructured{}
		cm.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
		require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: name, Namespace: ns}, cm))
	}

	// Summary should be created immediately (readyWhen passes on first evaluation)
	summary := &unstructured.Unstructured{}
	summary.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
	require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: "ready-worker-summary", Namespace: ns}, summary))

	data, _, _ := unstructured.NestedStringMap(summary.Object, "data")
	assert.NotEmpty(t, data["firstWorker"])
	t.Logf("Summary created immediately with firstWorker=%s — all workers ready on first pass", data["firstWorker"])

	// Graph should be Active
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		g := &unstructured.Unstructured{}
		g.SetGroupVersionKind(GraphGVK)
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "test-foreach-readywhen-pass", Namespace: ns}, g); err != nil {
			return false, nil
		}
		return graphReady(g), nil
	}))
	t.Log("Graph is Active — forEach readyWhen happy path proved")
}
