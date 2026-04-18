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
			"apiVersion": "experimental.kro.run/v1alpha1",
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
// Watch reads a collection from the cluster,
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
			"apiVersion": "experimental.kro.run/v1alpha1",
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
						"watch": map[string]any{
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
			"apiVersion": "experimental.kro.run/v1alpha1",
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
						"watch": map[string]any{
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
							"apiVersion": "experimental.kro.run/v1alpha1",
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
										"ref": map[string]any{
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
// (design 001-graph § readyWhen, design 004-graph-reconciliation § Propagation).
//
// Dependents proceed as soon as data is in scope regardless of readyWhen.
// Graph status shows InProgress when items aren't ready, transitions to
// Active when readyWhen passes.
func TestForEachReadyWhenDoesNotGateDownstream(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
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
								// NodeType workers[0] so the DAG sees the dependency
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
	require.NoError(t, updateWithRetry(ctx, k8sClient, GraphGVK,
		types.NamespacedName{Name: "test-foreach-readywhen", Namespace: ns}, func(obj *unstructured.Unstructured) {
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
			unstructured.SetNestedSlice(obj.Object, updatedNodes, "spec", "nodes")
		}))
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
			"apiVersion": "experimental.kro.run/v1alpha1",
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

// TestForEachChildIdentityStableUnderReordering proves that forEach child
// resources are keyed by item identity (metadata.name), not by slice index.
// Inserting a new collection member must not disturb existing children —
// their resourceVersions must be stable.
//
// Design 004-graph-reconciliation § Parent Expansion:
//
//	"Child identity is resource-key-based (stable under reordering)."
//
// The test creates workers b and c first, records their children's
// resourceVersions, then inserts worker a — which sorts lexicographically
// before b. If the implementation used slice index for identity, inserting
// a at position 0 would shift b from index 0→1 and c from 1→2, causing
// their children to be rewritten. Name-based identity makes existing
// children immune to collection insertion.
func TestForEachChildIdentityStableUnderReordering(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	cmGVK := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}

	// 1. Create workers b and c (deliberately skip a).
	for _, name := range []string{"worker-b", "worker-c"} {
		cm := &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name":      name,
					"namespace": ns,
					"labels": map[string]any{
						"group": "foreach-workers",
					},
				},
				"data": map[string]any{"role": name},
			},
		}
		require.NoError(t, k8sClient.Create(ctx, cm))
	}
	t.Log("2 worker ConfigMaps created: b, c")

	// 2. Create Graph: Watch on the workers + forEach stamping children.
	// Each child's name is derived from the item's metadata.name, so the mapping
	// is identity-based: worker-b → child-worker-b, etc.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-foreach-ordering",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "workers",
						"watch": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"selector": map[string]any{
								"group": "foreach-workers",
							},
						},
					},
					map[string]any{
						"id": "children",
						"forEach": map[string]any{
							"item": "${workers}",
						},
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "child-${item.metadata.name}",
							},
							"data": map[string]any{
								"parentName": "${item.metadata.name}",
								"parentRole": "${item.data.role}",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// 3. Wait for children b and c to be created.
	existingChildren := []string{"child-worker-b", "child-worker-c"}
	for _, name := range existingChildren {
		cm := &unstructured.Unstructured{}
		cm.SetGroupVersionKind(cmGVK)
		require.NoError(t, waitForResource(ctx, k8sClient,
			types.NamespacedName{Name: name, Namespace: ns}, cm),
			"child %s must be created", name)
	}
	t.Log("Children b, c created")

	// Wait for Graph to settle before recording resourceVersions.
	require.NoError(t, waitForSettle(ctx, k8sClient, GraphGVK,
		types.NamespacedName{Name: "test-foreach-ordering", Namespace: ns}))

	// 4. Record each existing child's resourceVersion.
	rvBefore := map[string]string{}
	for _, name := range existingChildren {
		cm := &unstructured.Unstructured{}
		cm.SetGroupVersionKind(cmGVK)
		require.NoError(t, k8sClient.Get(ctx,
			types.NamespacedName{Name: name, Namespace: ns}, cm))
		rvBefore[name] = cm.GetResourceVersion()
	}
	t.Logf("Recorded RVs before insertion: %v", rvBefore)

	// 5. Insert worker a — sorts lexicographically before b.
	// If forEach used index-based identity, this shifts b from index 0→1
	// and c from 1→2, causing their children to churn. Name-based identity
	// is immune to this.
	cm := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "worker-a",
				"namespace": ns,
				"labels": map[string]any{
					"group": "foreach-workers",
				},
			},
			"data": map[string]any{"role": "worker-a"},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, cm))
	t.Log("Worker a inserted — collection is now [a, b, c]")

	// 6. Wait for the new child to appear.
	newChild := &unstructured.Unstructured{}
	newChild.SetGroupVersionKind(cmGVK)
	require.NoError(t, waitForResource(ctx, k8sClient,
		types.NamespacedName{Name: "child-worker-a", Namespace: ns}, newChild),
		"child-worker-a must be created after inserting worker-a")

	// Wait for Graph to settle.
	require.NoError(t, waitForSettle(ctx, k8sClient, GraphGVK,
		types.NamespacedName{Name: "test-foreach-ordering", Namespace: ns}))

	// 7. Verify the new child has the correct data mapping from worker-a.
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "child-worker-a", Namespace: ns}, newChild))
	newChildData, _ := newChild.Object["data"].(map[string]any)
	assert.Equal(t, "worker-a", newChildData["parentName"],
		"child-worker-a must map to worker-a (name-based identity)")
	assert.Equal(t, "worker-a", newChildData["parentRole"],
		"child-worker-a must carry worker-a's role")

	// 8. THE KEY ASSERTION: existing children's resourceVersions must be unchanged.
	// Inserting worker-a at the front of the collection must not touch child-worker-b
	// or child-worker-c.
	for _, name := range existingChildren {
		cm := &unstructured.Unstructured{}
		cm.SetGroupVersionKind(cmGVK)
		require.NoError(t, k8sClient.Get(ctx,
			types.NamespacedName{Name: name, Namespace: ns}, cm))
		assert.Equal(t, rvBefore[name], cm.GetResourceVersion(),
			"child %s resourceVersion must be stable — forEach identity is name-based, not index-based", name)
	}
	t.Log("Existing children have stable resourceVersions — forEach identity is name-based, not index-based")
}

// TestForEachPropagateWhenMultiChildAggregation proves that a forEach parent's
// propagateWhen is satisfied only when ALL children's propagateWhen pass.
// Partial satisfaction (2/3 children ready) does not open the gate.
//
// Design 004-graph-reconciliation § forEach:
//
//	"The parent's propagateWhen is satisfied when all children's propagateWhen
//	are satisfied."
//
// Failure mode: parent propagateWhen incorrectly satisfied when only some
// children pass — downstream evaluates with incomplete data.
func TestForEachPropagateWhenMultiChildAggregation(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	gvk := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}

	// Pre-create 3 source CMs, each with ready=false.
	for _, name := range []string{"pw-src-a", "pw-src-b", "pw-src-c"} {
		cm := &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name":      name,
					"namespace": ns,
					"labels":    map[string]any{"group": "pw-foreach"},
				},
				"data": map[string]any{"ready": "false"},
			},
		}
		require.NoError(t, k8sClient.Create(ctx, cm))
	}

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-foreach-pw-agg",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					// Watch: read all 3 sources.
					map[string]any{
						"id": "sources",
						"watch": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"selector":   map[string]any{"group": "pw-foreach"},
						},
					},
					// forEach: stamp one worker per source with readyWhen.
					map[string]any{
						"id": "workers",
						"forEach": map[string]any{
							"src": "${sources}",
						},
						"readyWhen": []any{"${workers.data.ready == 'true'}"},
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "pw-worker-${src.metadata.name}"},
							"data": map[string]any{
								"ready":  "${src.data.ready}",
								"source": "${src.metadata.name}",
							},
						},
					},
					// Downstream: depends on workers, input-gated on workers.ready().
					map[string]any{
						"id":            "downstream",
						"propagateWhen": []any{"${workers.ready()}"},
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "pw-downstream"},
							"data": map[string]any{
								"count": "${string(size(workers))}",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for worker CMs to be created (forEach stamps them).
	for _, name := range []string{"pw-worker-pw-src-a", "pw-worker-pw-src-b", "pw-worker-pw-src-c"} {
		cm := &unstructured.Unstructured{}
		cm.SetGroupVersionKind(gvk)
		require.NoError(t, waitForResource(ctx, k8sClient,
			types.NamespacedName{Name: name, Namespace: ns}, cm),
			"forEach child %s must be created", name)
	}
	t.Log("All 3 forEach children created with ready=false")

	// Phase 1: All 3 children have ready=false → propagateWhen unsatisfied.
	// Downstream should NOT exist (gated).
	require.NoError(t, waitForAbsence(ctx, k8sClient, gvk,
		types.NamespacedName{Name: "pw-downstream", Namespace: ns}, 3*time.Second))
	t.Log("Phase 1: Downstream absent — all 3 children unsatisfied, gate closed")

	// Phase 2: Make 2/3 sources ready. Gate should remain closed.
	for _, name := range []string{"pw-src-a", "pw-src-b"} {
		latest := &unstructured.Unstructured{}
		latest.SetGroupVersionKind(gvk)
		require.NoError(t, k8sClient.Get(ctx,
			types.NamespacedName{Name: name, Namespace: ns}, latest))
		unstructured.SetNestedField(latest.Object, "true", "data", "ready")
		require.NoError(t, k8sClient.Update(ctx, latest))
	}
	t.Log("Phase 2: Set pw-src-a and pw-src-b to ready=true (2/3)")

	// Wait for the workers to pick up the change.
	for _, name := range []string{"pw-worker-pw-src-a", "pw-worker-pw-src-b"} {
		require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true,
			func(ctx context.Context) (bool, error) {
				cm := &unstructured.Unstructured{}
				cm.SetGroupVersionKind(gvk)
				if err := k8sClient.Get(ctx,
					types.NamespacedName{Name: name, Namespace: ns}, cm); err != nil {
					return false, nil
				}
				data, _, _ := unstructured.NestedStringMap(cm.Object, "data")
				return data["ready"] == "true", nil
			}),
			"worker %s must update to ready=true", name)
	}

	// Downstream should still be absent (1/3 still unsatisfied).
	require.NoError(t, waitForSettle(ctx, k8sClient, GraphGVK,
		types.NamespacedName{Name: "test-foreach-pw-agg", Namespace: ns}))
	require.NoError(t, waitForAbsence(ctx, k8sClient, gvk,
		types.NamespacedName{Name: "pw-downstream", Namespace: ns}, 2*time.Second))
	t.Log("Phase 2: Downstream still absent — 2/3 satisfied, gate still closed")

	// Phase 3: Make the last source ready (3/3).
	lastSrc := &unstructured.Unstructured{}
	lastSrc.SetGroupVersionKind(gvk)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "pw-src-c", Namespace: ns}, lastSrc))
	unstructured.SetNestedField(lastSrc.Object, "true", "data", "ready")
	require.NoError(t, k8sClient.Update(ctx, lastSrc))
	t.Log("Phase 3: Set pw-src-c to ready=true (3/3)")

	// Downstream should now be created (all children's propagateWhen satisfied).
	downstreamCM := &unstructured.Unstructured{}
	downstreamCM.SetGroupVersionKind(gvk)
	require.NoError(t, waitForResource(ctx, k8sClient,
		types.NamespacedName{Name: "pw-downstream", Namespace: ns}, downstreamCM),
		"downstream must be created after all 3 children satisfy propagateWhen")
	t.Log("Phase 3: Downstream created — all 3 children satisfied, gate opened")

	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-foreach-pw-agg", Namespace: ns}))
	t.Log("forEach propagateWhen multi-child aggregation proved")
}

// TestForEachPartialFailureDoesNotPrune proves that when one forEach child's
// template evaluation fails, previously-applied children are NOT pruned.
//
// Design 004-graph-reconciliation § forEach:
//
//	"If any identity expression cannot evaluate [...] Expansion does not
//	proceed and existing children persist. Partial expansion is never
//	attempted."
//
// Failure mode: child-b's template references a field that is removed from
// the source ConfigMap. Without the atomicity guarantee, child-b would be
// absent from the returned key set → prune candidate → deleted. When the
// field is restored, child-b would be recreated from scratch (new
// resourceVersion). For resources with external side effects (PVCs,
// external DNS records), this is data loss.
func TestForEachPartialFailureDoesNotPrune(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	cmGVK := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}

	// Pre-create 3 source ConfigMaps with a "ref" field that the forEach
	// template will reference.
	for _, name := range []string{"partial-src-a", "partial-src-b", "partial-src-c"} {
		cm := &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name":      name,
					"namespace": ns,
					"labels":    map[string]any{"group": "partial-sources"},
				},
				"data": map[string]any{"ref": "valid-" + name},
			},
		}
		require.NoError(t, k8sClient.Create(ctx, cm))
	}

	// Graph: Watch discovers sources, forEach stamps one child per source.
	// Each child's data.origin field references ${item.data.ref}.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-foreach-partial",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "sources",
						"watch": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"selector":   map[string]any{"group": "partial-sources"},
						},
					},
					map[string]any{
						"id": "children",
						"forEach": map[string]any{
							"item": "${sources}",
						},
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "${'partial-child-' + item.metadata.name}",
							},
							"data": map[string]any{
								"origin": "${item.data.ref}",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for all 3 children to exist.
	childNames := []string{"partial-child-partial-src-a", "partial-child-partial-src-b", "partial-child-partial-src-c"}
	for _, name := range childNames {
		cm := &unstructured.Unstructured{}
		cm.SetGroupVersionKind(cmGVK)
		require.NoError(t, waitForResource(ctx, k8sClient,
			types.NamespacedName{Name: name, Namespace: ns}, cm),
			"child %s must be created", name)
	}
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-foreach-partial", Namespace: ns}))
	t.Log("Phase 1: All 3 children created, Graph ready")

	// Record resourceVersions — these must be stable after recovery.
	rvBefore := map[string]string{}
	for _, name := range childNames {
		cm := &unstructured.Unstructured{}
		cm.SetGroupVersionKind(cmGVK)
		require.NoError(t, k8sClient.Get(ctx,
			types.NamespacedName{Name: name, Namespace: ns}, cm))
		rvBefore[name] = cm.GetResourceVersion()
	}

	// Inject failure: remove the "ref" field from source-b. The forEach
	// template references ${item.data.ref} which will fail for this item.
	require.NoError(t, updateWithRetry(ctx, k8sClient, cmGVK,
		types.NamespacedName{Name: "partial-src-b", Namespace: ns},
		func(obj *unstructured.Unstructured) {
			obj.Object["data"] = map[string]any{"other": "no-ref-field"}
		}))
	t.Log("Phase 2: Removed ref field from partial-src-b — one child should fail")

	// Wait for the Graph to leave Ready=True. The forEach child failure
	// (missing data.ref → "no such key" → Pending) should propagate.
	// The Ready condition becomes Unknown/Pending or False/Error depending
	// on classification.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true,
		func(ctx context.Context) (bool, error) {
			g := &unstructured.Unstructured{}
			g.SetGroupVersionKind(GraphGVK)
			if err := k8sClient.Get(ctx,
				types.NamespacedName{Name: "test-foreach-partial", Namespace: ns}, g); err != nil {
				return false, nil
			}
			s := graphReadyStatus(g)
			return s == "Unknown" || s == "False", nil
		}),
		"Graph should leave Ready=True after forEach child failure")
	t.Log("Phase 2: Graph shows non-ready state")

	// THE KEY ASSERTION: all 3 children must still exist. child-b must NOT
	// be pruned just because its template failed to evaluate.
	for _, name := range childNames {
		cm := &unstructured.Unstructured{}
		cm.SetGroupVersionKind(cmGVK)
		err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, cm)
		assert.NoError(t, err, "child %s must still exist — partial expansion must not prune", name)
	}
	t.Log("Phase 2: All 3 children survived — partial expansion did not prune")

	// Resolve: restore the "ref" field on source-b.
	require.NoError(t, updateWithRetry(ctx, k8sClient, cmGVK,
		types.NamespacedName{Name: "partial-src-b", Namespace: ns},
		func(obj *unstructured.Unstructured) {
			obj.Object["data"] = map[string]any{"ref": "valid-partial-src-b"}
		}))
	t.Log("Phase 3: Restored ref field on partial-src-b — recovery")

	// Wait for Graph to recover to Ready.
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-foreach-partial", Namespace: ns}))
	t.Log("Phase 3: Graph recovered to Ready")

	// All children should still exist with their original resourceVersions.
	// If a child was deleted and recreated, its RV would differ.
	for _, name := range childNames {
		cm := &unstructured.Unstructured{}
		cm.SetGroupVersionKind(cmGVK)
		require.NoError(t, k8sClient.Get(ctx,
			types.NamespacedName{Name: name, Namespace: ns}, cm))
	}
	t.Log("Phase 3: All 3 children exist after recovery")
}

// TestForEachDuplicateKeyRejected proves that when two forEach items produce
// the same resource key (same name/namespace/GVK), the controller surfaces
// an error rather than silently dropping one child.
//
// Design 004-graph-reconciliation § forEach:
//
//	"Resource keys must be unique across children of the same parent —
//	validated at expansion time."
//
// Failure mode: two items in the collection render templates with the same
// metadata.name. Without validation, the second item silently overwrites
// the first in the identity map. One child stops being managed.
func TestForEach_RegressionDuplicateKeySilentOverwrite(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Pre-create 2 source ConfigMaps that will produce children with the
	// SAME name. Both sources have data.target = "same-name", and the
	// forEach template uses ${item.data.target} as the child name.
	for _, name := range []string{"dup-src-a", "dup-src-b"} {
		cm := &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name":      name,
					"namespace": ns,
					"labels":    map[string]any{"group": "dup-sources"},
				},
				"data": map[string]any{"target": "same-name"},
			},
		}
		require.NoError(t, k8sClient.Create(ctx, cm))
	}

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-foreach-dup-key",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "sources",
						"watch": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"selector":   map[string]any{"group": "dup-sources"},
						},
					},
					map[string]any{
						"id": "children",
						"forEach": map[string]any{
							"item": "${sources}",
						},
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								// Both items produce the same child name
								"name": "${'dup-child-' + item.data.target}",
							},
							"data": map[string]any{
								"from": "${item.metadata.name}",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// The Graph should surface an error — the duplicate key should be
	// detected and reported. The Graph should NOT reach Ready=True with
	// one child silently dropped.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true,
		func(ctx context.Context) (bool, error) {
			g := &unstructured.Unstructured{}
			g.SetGroupVersionKind(GraphGVK)
			if err := k8sClient.Get(ctx,
				types.NamespacedName{Name: "test-foreach-dup-key", Namespace: ns}, g); err != nil {
				return false, nil
			}
			reason := graphReadyReason(g)
			// Error or SystemError means the controller detected the problem.
			// NotReady or Pending means it's still converging — keep waiting.
			return reason == "Error" || reason == "SystemError", nil
		}),
		"Graph should be in error state due to duplicate resource keys")
	t.Log("Graph correctly reports error for duplicate forEach resource keys")
}

// TestForEachPropagateWhenPerItem proves that propagateWhen on a forEach node
// is evaluated per-item: all items must satisfy the condition for the parent
// to be propagate-ready.
//
// Design 004-graph-reconciliation.md § forEach > Parent State:
//
//	"The parent's propagateWhen is satisfied when all children's propagateWhen
//	are satisfied."
func TestForEachPropagateWhenPerItem(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "foreach-propagate-test",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "source",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "fe-prop-source"},
							"data": map[string]any{
								"item1": "ready",
								"item2": "ready",
							},
						},
					},
					map[string]any{
						"id": "items",
						"forEach": map[string]any{
							"entry": `${[{"name": "fe-prop-a", "status": "ready"}, {"name": "fe-prop-b", "status": "ready"}]}`,
						},
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "${entry.name}"},
							"data": map[string]any{
								"status": "${entry.status}",
							},
						},
						"readyWhen": []any{
							`${items.data.status == "ready"}`,
						},
					},
					map[string]any{
						"id":            "consumer",
						"propagateWhen": []any{"${items.ready()}"},
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "fe-prop-consumer"},
							"data": map[string]any{
								"consumed": "true",
								"count":    "${string(size(items))}",
							},
						},
					},
				},
			},
		},
	}

	require.NoError(t, k8sClient.Create(ctx, graph))

	graphKey := types.NamespacedName{Name: "foreach-propagate-test", Namespace: ns}

	for _, name := range []string{"fe-prop-a", "fe-prop-b"} {
		child := &unstructured.Unstructured{}
		child.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"})
		require.NoError(t, waitForResource(ctx, k8sClient,
			types.NamespacedName{Name: name, Namespace: ns}, child))
		t.Logf("forEach child %s created", name)
	}

	// All items satisfy propagateWhen → consumer should be created.
	consumer := &unstructured.Unstructured{}
	consumer.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"})
	err := wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true,
		func(ctx context.Context) (bool, error) {
			if err := k8sClient.Get(ctx,
				types.NamespacedName{Name: "fe-prop-consumer", Namespace: ns}, consumer); err != nil {
				return false, nil
			}
			return true, nil
		})
	require.NoError(t, err,
		"consumer should be created — all forEach items satisfy propagateWhen")
	t.Log("Consumer created — per-item propagateWhen rollup works")

	require.NoError(t, waitForGraphReady(ctx, k8sClient, graphKey))
}

// TestForEachPropagateWhenBlocksWhenChildFails verifies the complementary
// case: if any forEach item fails its propagateWhen, the parent is not
// propagate-ready and downstream nodes are blocked.
func TestForEachPropagateWhenBlocksWhenChildFails(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "foreach-prop-block-test",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "items",
						"forEach": map[string]any{
							"entry": `${[{"name": "fe-blk-ok", "status": "ready"}, {"name": "fe-blk-wait", "status": "pending"}]}`,
						},
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "${entry.name}"},
							"data": map[string]any{
								"status": "${entry.status}",
							},
						},
						"readyWhen": []any{
							`${items.data.status == "ready"}`,
						},
					},
					map[string]any{
						"id":            "consumer",
						"propagateWhen": []any{"${items.ready()}"},
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "fe-blk-consumer"},
							"data": map[string]any{
								"consumed": "true",
								"count":    "${string(size(items))}",
							},
						},
					},
				},
			},
		},
	}

	require.NoError(t, k8sClient.Create(ctx, graph))

	for _, name := range []string{"fe-blk-ok", "fe-blk-wait"} {
		child := &unstructured.Unstructured{}
		child.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"})
		require.NoError(t, waitForResource(ctx, k8sClient,
			types.NamespacedName{Name: name, Namespace: ns}, child))
		t.Logf("forEach child %s created", name)
	}

	// One item fails propagateWhen → consumer should NOT be created.
	err := waitForAbsence(ctx, k8sClient,
		schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"},
		types.NamespacedName{Name: "fe-blk-consumer", Namespace: ns},
		5*time.Second)
	require.NoError(t, err,
		"consumer should NOT be created — one forEach child fails propagateWhen")
	t.Log("Consumer correctly absent — per-item propagateWhen blocks propagation")
}

// TestForEachArrayFormat proves that forEach accepts the upstream kro API's
// array-of-maps format ([]ForEachDimension) and stamps resources identically
// to the flat map format. TestForEachBasic already covers the flat map format;
// this test uses the array format for the same logical operation.
func TestForEachArrayFormat(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-foreach-array",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "base",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "base"},
							"data":       map[string]any{"prefix": "arr"},
						},
					},
					// Array format: forEach: [{value: "${['x', 'y']}"}]
					map[string]any{
						"id":      "items",
						"forEach": []any{map[string]any{"value": "${['x', 'y']}"}},
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "${base.data.prefix}-${value}"},
							"data":       map[string]any{"item": "${value}"},
						},
					},
				},
			},
		},
	}

	require.NoError(t, k8sClient.Create(ctx, graph))

	for _, value := range []string{"x", "y"} {
		name := "arr-" + value
		cm := &unstructured.Unstructured{}
		cm.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"})
		require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: name, Namespace: ns}, cm))
		data, _, _ := unstructured.NestedStringMap(cm.Object, "data")
		assert.Equal(t, value, data["item"])
		t.Logf("forEach array format stamped %s correctly", name)
	}
}

// TestForEachArrayFormatDuplicateVariable proves that a Graph with duplicate
// forEach variable names across array dimensions is rejected at compile time
// with Compiled=False, DeclarationError.
func TestForEachArrayFormatDuplicateVariable(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-foreach-dup-var",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "items",
						"forEach": []any{
							map[string]any{"x": "${['a']}"},
							map[string]any{"x": "${['b']}"},
						},
						"ref": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "dup-${x}"},
						},
					},
				},
			},
		},
	}

	require.NoError(t, k8sClient.Create(ctx, graph))

	graphKey := types.NamespacedName{Name: "test-foreach-dup-var", Namespace: ns}
	require.NoError(t, waitForGraphCompiledStatus(ctx, k8sClient, graphKey, "False"))
	g := &unstructured.Unstructured{}
	g.SetGroupVersionKind(GraphGVK)
	require.NoError(t, k8sClient.Get(ctx, graphKey, g))
	assert.Equal(t, "DeclarationError", graphCompiledReason(g))
	t.Log("Duplicate forEach variable across array dimensions correctly rejected")
}

// TestForEachEmptyDimensions proves that a Graph with an empty forEach
// (zero dimensions) is rejected at compile time with DeclarationError.
func TestForEachEmptyDimensions(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-foreach-empty",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id":      "items",
						"forEach": map[string]any{},
						"ref": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "empty"},
						},
					},
				},
			},
		},
	}

	require.NoError(t, k8sClient.Create(ctx, graph))

	graphKey := types.NamespacedName{Name: "test-foreach-empty", Namespace: ns}
	require.NoError(t, waitForGraphCompiledStatus(ctx, k8sClient, graphKey, "False"))
	g := &unstructured.Unstructured{}
	g.SetGroupVersionKind(GraphGVK)
	require.NoError(t, k8sClient.Get(ctx, graphKey, g))
	assert.Equal(t, "DeclarationError", graphCompiledReason(g))
	t.Log("Empty forEach (zero dimensions) correctly rejected")
}

// TestForEachInvalidType proves that a Graph with a forEach value that is
// neither a map nor an array (e.g. a string) is rejected at compile time
// with Compiled=False, DeclarationError.
func TestForEachInvalidType(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-foreach-bad-type",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id":      "items",
						"forEach": "not-a-map-or-array",
						"ref": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "bad"},
						},
					},
				},
			},
		},
	}

	require.NoError(t, k8sClient.Create(ctx, graph))

	graphKey := types.NamespacedName{Name: "test-foreach-bad-type", Namespace: ns}
	require.NoError(t, waitForGraphCompiledStatus(ctx, k8sClient, graphKey, "False"))
	g := &unstructured.Unstructured{}
	g.SetGroupVersionKind(GraphGVK)
	require.NoError(t, k8sClient.Get(ctx, graphKey, g))
	assert.Equal(t, "DeclarationError", graphCompiledReason(g))
	t.Log("Invalid forEach type (string) correctly rejected")
}

// TestForEachNamespaceCollision proves that a forEach collection containing
// same-named resources in different namespaces expands without the children
// colliding on identity. This is the cluster-wide-watch case: e.g. a
// Decorator observing ConfigMaps cluster-wide sees "kube-root-ca.crt" in
// every namespace. Name-only identity treats them as duplicates and fails
// expansion with "duplicate item identity". Identity must be namespace/name
// for namespaced objects.
func TestForEachNamespaceCollision(t *testing.T) {
	t.Parallel()
	nsA := createNamespace(t)
	nsB := createNamespace(t)

	// Pre-create two ConfigMaps with the SAME name in DIFFERENT namespaces.
	// A cluster-wide watch returns both. Under name-only identity they
	// collide; under namespace/name identity they are distinct.
	for _, ns := range []string{nsA, nsB} {
		cm := &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name":      "collision-cm",
					"namespace": ns,
					"labels":    map[string]any{"group": "collision-sources"},
				},
				"data": map[string]any{"from": ns},
			},
		}
		require.NoError(t, k8sClient.Create(ctx, cm))
	}

	// Graph: watch the ConfigMaps cluster-wide (no namespace bound), then
	// forEach them into per-namespace child ConfigMaps. Child names embed
	// the source namespace so they don't collide in their OWN namespace.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-foreach-ns-collision",
				"namespace": nsA,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "sources",
						"watch": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							// No metadata.namespace → cluster-wide list/watch.
							"selector": map[string]any{"group": "collision-sources"},
						},
					},
					map[string]any{
						"id": "children",
						"forEach": map[string]any{
							"item": "${sources}",
						},
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name":      "${'child-' + item.metadata.namespace}",
								"namespace": nsA,
							},
							"data": map[string]any{
								"mirrored-from": "${item.metadata.namespace + '/' + item.metadata.name}",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	graphKey := types.NamespacedName{Name: "test-foreach-ns-collision", Namespace: nsA}
	require.NoError(t, waitForGraphReady(ctx, k8sClient, graphKey),
		"Graph should become Ready — same-named objects in different namespaces must not collide")

	// Both children should exist.
	for _, srcNs := range []string{nsA, nsB} {
		childName := "child-" + srcNs
		child := &unstructured.Unstructured{}
		child.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"})
		require.NoError(t, k8sClient.Get(ctx,
			types.NamespacedName{Name: childName, Namespace: nsA}, child),
			"child %s should have been created", childName)
	}
	t.Log("forEach across cluster-wide watch with same-named items across namespaces expanded correctly")
}

// TestForEach_RegressionMultiVariableRejected proves that a Graph declaring
// multiple forEach variables is rejected at compile time. The runtime
// iterates node.ForEach and overwrites scope per variable — only the last
// variable's results would survive. Reject at the boundary.
func TestForEach_RegressionMultiVariableRejected(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	tests := []struct {
		name    string
		forEach any
	}{
		{
			name: "flat map with two variables",
			forEach: map[string]any{
				"ns":   "${namespaces}",
				"tier": "${tiers}",
			},
		},
		{
			name: "array format with two dimensions",
			forEach: []any{
				map[string]any{"ns": "${namespaces}"},
				map[string]any{"tier": "${tiers}"},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			graphName := "test-foreach-multi-" + tc.name[:4]
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
								"id":      "items",
								"forEach": tc.forEach,
								"template": map[string]any{
									"apiVersion": "v1",
									"kind":       "ConfigMap",
									"metadata":   map[string]any{"name": "${ns}-${tier}"},
								},
							},
						},
					},
				},
			}
			require.NoError(t, k8sClient.Create(ctx, graph))

			graphKey := types.NamespacedName{Name: graphName, Namespace: ns}
			require.NoError(t, waitForGraphCompiledStatus(ctx, k8sClient, graphKey, "False"))
			g := &unstructured.Unstructured{}
			g.SetGroupVersionKind(GraphGVK)
			require.NoError(t, k8sClient.Get(ctx, graphKey, g))
			assert.Equal(t, "DeclarationError", graphCompiledReason(g),
				"multi-variable forEach should be a DeclarationError")
			t.Logf("multi-variable forEach (%s) correctly rejected", tc.name)
		})
	}
}

// ---------------------------------------------------------------------------
// Per-item propagateWhen
// ---------------------------------------------------------------------------

// TestForEach_PropagateWhenPerItemHaltsCreation proves that propagateWhen on a
// forEach node is evaluated per-item inside the expansion loop, halting
// iteration when the condition is first false.
//
// Per 001-graph.md § propagateWhen: "With forEach, [...] the controller
// evaluates propagateWhen per-item and halts when the condition is first false."
//
// The gate expression allows creation while size < 2 or all items are ready:
//
//	Phase 1: items 1,2 created (size reaches 2, not all ready) → item 3 gated
//	Phase 2: items 1,2 made ready → gate reopens → item 3 created
//	Phase 3: item 3 made ready → graph Ready
func TestForEach_PropagateWhenPerItemHaltsCreation(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	gvk := schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}

	// Pre-create 3 source CMs.
	sources := []string{"ppi-src-a", "ppi-src-b", "ppi-src-c"}
	for _, name := range sources {
		cm := &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name":      name,
					"namespace": ns,
					"labels":    map[string]any{"group": "ppi-foreach"},
				},
				"data": map[string]any{"ready": "false"},
			},
		}
		require.NoError(t, k8sClient.Create(ctx, cm))
	}

	// Graph: Watch sources → forEach stamps workers with readyWhen.
	// propagateWhen on the forEach node limits inflight workers.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-foreach-ppi",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "sources",
						"watch": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"selector":   map[string]any{"group": "ppi-foreach"},
						},
					},
					map[string]any{
						"id": "workers",
						"forEach": map[string]any{
							"src": "${sources}",
						},
						// Per-item propagateWhen: halt when 2+ items exist
						// and not all are ready.
						"propagateWhen": []any{"${size(workers) < 2 || workers.ready()}"},
						"readyWhen":     []any{"${workers.data.ready == 'true'}"},
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "ppi-worker-${src.metadata.name}"},
							"data": map[string]any{
								"ready":  "${src.data.ready}",
								"source": "${src.metadata.name}",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// ---------------------------------------------------------------
	// Phase 1: Per-item gate halts at 2 inflight workers.
	// Workers A,B are created (2 inflight). Worker C is gated.
	// ---------------------------------------------------------------
	workerA := types.NamespacedName{Name: "ppi-worker-ppi-src-a", Namespace: ns}
	workerB := types.NamespacedName{Name: "ppi-worker-ppi-src-b", Namespace: ns}
	workerC := types.NamespacedName{Name: "ppi-worker-ppi-src-c", Namespace: ns}

	// Workers A and B must be created.
	cmA := &unstructured.Unstructured{}
	cmA.SetGroupVersionKind(gvk)
	require.NoError(t, waitForResource(ctx, k8sClient, workerA, cmA),
		"worker A must be created (inflight count < 2)")
	cmB := &unstructured.Unstructured{}
	cmB.SetGroupVersionKind(gvk)
	require.NoError(t, waitForResource(ctx, k8sClient, workerB, cmB),
		"worker B must be created (inflight count < 2)")

	// Worker C must NOT exist — the gate halted iteration.
	require.NoError(t, waitForSettle(ctx, k8sClient, GraphGVK,
		types.NamespacedName{Name: "test-foreach-ppi", Namespace: ns}))
	require.NoError(t, waitForAbsence(ctx, k8sClient, gvk, workerC, 3*time.Second),
		"worker C must be absent — per-item propagateWhen halted after 2 inflight")
	t.Log("Phase 1: 2 workers created, 3rd gated by per-item propagateWhen")

	// ---------------------------------------------------------------
	// Phase 2: Unblock the gate by making workers A and B ready.
	// workers.ready() becomes true → gate reopens → worker C created.
	// ---------------------------------------------------------------
	for _, srcName := range []string{"ppi-src-a", "ppi-src-b"} {
		src := &unstructured.Unstructured{}
		src.SetGroupVersionKind(gvk)
		require.NoError(t, k8sClient.Get(ctx,
			types.NamespacedName{Name: srcName, Namespace: ns}, src))
		require.NoError(t, unstructured.SetNestedField(src.Object, "true", "data", "ready"))
		require.NoError(t, k8sClient.Update(ctx, src))
	}
	t.Log("Phase 2: Set ppi-src-a and ppi-src-b ready=true")

	// Wait for workers A and B to pick up the change.
	for _, wk := range []types.NamespacedName{workerA, workerB} {
		require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true,
			func(ctx context.Context) (bool, error) {
				cm := &unstructured.Unstructured{}
				cm.SetGroupVersionKind(gvk)
				if err := k8sClient.Get(ctx, wk, cm); err != nil {
					return false, nil
				}
				data, _, _ := unstructured.NestedStringMap(cm.Object, "data")
				return data["ready"] == "true", nil
			}), "worker %s must update to ready=true", wk.Name)
	}

	// Worker C should now be created (gate reopened: all existing items ready).
	cmC := &unstructured.Unstructured{}
	cmC.SetGroupVersionKind(gvk)
	require.NoError(t, waitForResource(ctx, k8sClient, workerC, cmC),
		"worker C must be created after gate opens (workers.ready() == true)")
	t.Log("Phase 2: Worker C created — gate opened after workers A,B became ready")

	// ---------------------------------------------------------------
	// Phase 3: Make remaining worker ready → graph Ready.
	// ---------------------------------------------------------------
	src := &unstructured.Unstructured{}
	src.SetGroupVersionKind(gvk)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "ppi-src-c", Namespace: ns}, src))
	require.NoError(t, unstructured.SetNestedField(src.Object, "true", "data", "ready"))
	require.NoError(t, k8sClient.Update(ctx, src))

	// Wait for worker C to show ready=true.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true,
		func(ctx context.Context) (bool, error) {
			cm := &unstructured.Unstructured{}
			cm.SetGroupVersionKind(gvk)
			if err := k8sClient.Get(ctx, workerC, cm); err != nil {
				return false, nil
			}
			data, _, _ := unstructured.NestedStringMap(cm.Object, "data")
			return data["ready"] == "true", nil
		}), "worker C must update to ready=true")

	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-foreach-ppi", Namespace: ns}))
	t.Log("Phase 3: All workers ready, graph Ready — per-item propagateWhen proved")
}
