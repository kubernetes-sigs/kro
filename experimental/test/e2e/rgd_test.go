package graphcontroller_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
)

func TestRGDPatternEndToEnd(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// SimpleApp CRD is pre-installed in TestMain.
	simpleAppGVK := schema.GroupVersionKind{Group: "test.kro.run", Version: "v1alpha1", Kind: "SimpleApp"}

	// Create the L1 controller Graph:
	// watches all SimpleApps, stamps one child Graph per instance
	controllerGraph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "simpleapp-controller",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					// Watch all SimpleApp instances in this namespace
					map[string]any{
						"id": "instances",
						"watch": map[string]any{
							"apiVersion": "test.kro.run/v1alpha1",
							"kind":       "SimpleApp",
							"metadata":   map[string]any{"namespace": ns},
							"selector":   map[string]any{},
						},
					},
					// Stamp one child Graph per instance
					map[string]any{
						"id": "childGraphs",
						"forEach": map[string]any{
							"app": "${instances}",
						},
						"template": map[string]any{
							"apiVersion": "experimental.kro.run/v1alpha1",
							"kind":       "Graph",
							"metadata": map[string]any{
								"name": "${app.metadata.name}-simpleapp",
							},
							"spec": map[string]any{
								"nodes": []any{
									// L2: Read the specific instance by name (baked in by L1)
									map[string]any{
										"id": "schema",
										"ref": map[string]any{
											"apiVersion": "test.kro.run/v1alpha1",
											"kind":       "SimpleApp",
											"metadata": map[string]any{
												"name":      "${app.metadata.name}",
												"namespace": "${app.metadata.namespace}",
											},
										},
									},
									// L2: Create a ConfigMap from the instance spec
									// $${} deferred to L2 evaluation
									map[string]any{
										"id": "config",
										"template": map[string]any{
											"apiVersion": "v1",
											"kind":       "ConfigMap",
											"metadata": map[string]any{
												"name": "$${schema.metadata.name}-config",
											},
											"data": map[string]any{
												"image":    "$${schema.spec.image}",
												"replicas": "$${string(schema.spec.replicas)}",
												"appName":  "$${schema.metadata.name}",
											},
										},
									},
									// L2: Patch status + annotations back to the instance.
									// Auto-detected as contribution: template has only
									// apiVersion, kind, metadata, and status (no spec).
									map[string]any{
										"id": "statusContrib",
										"patch": map[string]any{
											"apiVersion": "test.kro.run/v1alpha1",
											"kind":       "SimpleApp",
											"metadata": map[string]any{
												"name":      "$${schema.metadata.name}",
												"namespace": "$${schema.metadata.namespace}",
												"annotations": map[string]any{
													"kro.run/managed-by": "graph-controller",
												},
											},
											"status": map[string]any{
												"configName": "$${config.metadata.name}",
												"image":      "$${config.data.image}",
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
	require.NoError(t, k8sClient.Create(ctx, controllerGraph))
	t.Log("L1 controller Graph created")

	// Create a SimpleApp instance
	app1 := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "test.kro.run/v1alpha1",
			"kind":       "SimpleApp",
			"metadata": map[string]any{
				"name":      "my-app",
				"namespace": ns,
			},
			"spec": map[string]any{
				"image":    "nginx:latest",
				"replicas": int64(3),
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, app1))
	t.Log("SimpleApp instance my-app created")

	// --- L1 assertions: child Graph should be stamped ---

	childGraph := &unstructured.Unstructured{}
	childGraph.SetGroupVersionKind(GraphGVK)
	require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: "my-app-simpleapp", Namespace: ns}, childGraph))
	t.Log("L1: Child Graph my-app-simpleapp created")

	// Child Graph should be managed by the controller Graph
	assertManagedBy(t, childGraph, "simpleapp-controller")

	// --- L2 assertions: child Graph creates resources from instance spec ---

	// ConfigMap should be created with data from the SimpleApp spec
	configCM := &unstructured.Unstructured{}
	configCM.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
	require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: "my-app-config", Namespace: ns}, configCM))

	cmData, _, _ := unstructured.NestedStringMap(configCM.Object, "data")
	assert.Equal(t, "nginx:latest", cmData["image"], "ConfigMap should have image from SimpleApp spec")
	assert.Equal(t, "3", cmData["replicas"], "ConfigMap should have replicas from SimpleApp spec")
	assert.Equal(t, "my-app", cmData["appName"], "ConfigMap should have name from SimpleApp metadata")
	t.Logf("L2: ConfigMap created: image=%s replicas=%s appName=%s", cmData["image"], cmData["replicas"], cmData["appName"])

	// ConfigMap should be managed by the child Graph (not the controller Graph)
	assertManagedBy(t, configCM, "my-app-simpleapp")

	// --- Contribution: status written back to the SimpleApp instance via status subresource ---

	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		updated := &unstructured.Unstructured{}
		updated.SetGroupVersionKind(simpleAppGVK)
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "my-app", Namespace: ns}, updated); err != nil {
			return false, nil
		}
		configName, _, _ := unstructured.NestedString(updated.Object, "status", "configName")
		return configName == "my-app-config", nil
	}))

	// Verify both status fields
	updatedApp := &unstructured.Unstructured{}
	updatedApp.SetGroupVersionKind(simpleAppGVK)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "my-app", Namespace: ns}, updatedApp))
	statusConfigName, _, _ := unstructured.NestedString(updatedApp.Object, "status", "configName")
	statusImage, _, _ := unstructured.NestedString(updatedApp.Object, "status", "image")
	assert.Equal(t, "my-app-config", statusConfigName)
	assert.Equal(t, "nginx:latest", statusImage)
	t.Logf("L2: Status contribution applied — SimpleApp status: configName=%s image=%s", statusConfigName, statusImage)

	// Verify annotation was also written (metadata contribution in same declaration)
	annotations := updatedApp.GetAnnotations()
	assert.Equal(t, "graph-controller", annotations["kro.run/managed-by"],
		"contribution should write annotations via regular SSA alongside status")
	t.Log("L2: Annotation contribution also applied — auto-split proved")

	// --- Reactivity: update SimpleApp spec, verify L2 propagates ---

	require.NoError(t, updateWithRetry(ctx, k8sClient, simpleAppGVK,
		types.NamespacedName{Name: "my-app", Namespace: ns}, func(obj *unstructured.Unstructured) {
			unstructured.SetNestedField(obj.Object, "redis:7", "spec", "image")
		}))
	t.Log("Updated SimpleApp: image=redis:7")

	// ConfigMap should update to reflect the new image
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		cm := &unstructured.Unstructured{}
		cm.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "my-app-config", Namespace: ns}, cm); err != nil {
			return false, nil
		}
		data, _, _ := unstructured.NestedStringMap(cm.Object, "data")
		return data["image"] == "redis:7", nil
	}))
	t.Log("L2: ConfigMap updated to image=redis:7 — reactive propagation proved")

	// Verify status contribution also updated reactively
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		updated := &unstructured.Unstructured{}
		updated.SetGroupVersionKind(simpleAppGVK)
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "my-app", Namespace: ns}, updated); err != nil {
			return false, nil
		}
		img, _, _ := unstructured.NestedString(updated.Object, "status", "image")
		return img == "redis:7", nil
	}))
	t.Log("L2: Status contribution updated to image=redis:7 — reactive status writeback proved")

	// --- Scale: add a second instance, verify second child Graph appears ---

	app2 := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "test.kro.run/v1alpha1",
			"kind":       "SimpleApp",
			"metadata": map[string]any{
				"name":      "second-app",
				"namespace": ns,
			},
			"spec": map[string]any{
				"image":    "postgres:15",
				"replicas": int64(1),
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, app2))

	// Second child Graph should appear
	childGraph2 := &unstructured.Unstructured{}
	childGraph2.SetGroupVersionKind(GraphGVK)
	require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: "second-app-simpleapp", Namespace: ns}, childGraph2))
	t.Log("L1: Second child Graph second-app-simpleapp created")

	// Second ConfigMap should appear with correct data
	configCM2 := &unstructured.Unstructured{}
	configCM2.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
	require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: "second-app-config", Namespace: ns}, configCM2))

	cmData2, _, _ := unstructured.NestedStringMap(configCM2.Object, "data")
	assert.Equal(t, "postgres:15", cmData2["image"])
	assert.Equal(t, "1", cmData2["replicas"])
	t.Logf("L2: Second ConfigMap created: image=%s replicas=%s", cmData2["image"], cmData2["replicas"])

	t.Log("Full RGD L1→L2 pattern proved: watch instances → stamp child Graphs → create resources → contribute back → reactive updates → scale")
}

// TestDynamicResourceListViaCEL proves that a parent Graph can construct a child
// Graph's spec.nodes via a CEL expression — the key mechanism for example 3.
//
// The parent has a list of resource definitions in a ConfigMap. Its forEach stamps
// a child Graph whose spec.nodes is ${...} — a CEL expression that concatenates
// a base watch with the dynamic resource list. The parent evaluates this
// expression against its own scope, so the child Graph arrives at the API server
// with spec.nodes as a concrete []any. The child reconciles normally.
//
// This proves the evaluation boundary handles structural fields: the parent evaluates
// spec.nodes, the child sees a literal array. No special "Phase 1" needed.
func TestDynamicResourceListViaCEL(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Source ConfigMap that the child Graph will read
	source := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "dynamic-source",
				"namespace": ns,
			},
			"data": map[string]any{
				"message": "hello-from-dynamic",
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, source))

	// Parent Graph: forEach over a literal list, stamps child Graphs with
	// spec.nodes constructed from a CEL expression.
	// The child's nodes are:
	//   [watch for the source CM] + [template CM that uses the source data]
	// This is the example 3 pattern — spec.nodes is a ${} expression.
	parent := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "dynamic-resources-parent",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "children",
						"forEach": map[string]any{
							"item": "${['child-a']}",
						},
						"template": map[string]any{
							"apiVersion": "experimental.kro.run/v1alpha1",
							"kind":       "Graph",
							"metadata": map[string]any{
								"name": "${item}-graph",
							},
							"spec": map[string]any{
								// This is the key: spec.nodes is a CEL expression that
								// the PARENT evaluates. It constructs the child's node
								// list dynamically. CEL output is opaque data — strings
								// inside (like ${source.data.message}) survive to the child
								// without needing $${} escaping.
								"nodes": `${[
								{"id": "source", "template": {"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "dynamic-source"}}},
								{"id": "result", "template": {"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": item + "-result"}, "data": {"value": "${source.data.message}"}}}
							]}`,
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, parent))
	t.Log("Parent Graph created with CEL-constructed spec.nodes")

	// Child Graph should be created
	childGraph := &unstructured.Unstructured{}
	childGraph.SetGroupVersionKind(GraphGVK)
	require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: "child-a-graph", Namespace: ns}, childGraph))

	// Verify the child Graph's spec.nodes is a concrete []any (not a string)
	childSpec, _, _ := unstructured.NestedFieldNoCopy(childGraph.Object, "spec", "nodes")
	childNodes, ok := childSpec.([]any)
	require.True(t, ok, "child spec.nodes should be []any, got %T", childSpec)
	assert.Len(t, childNodes, 2, "child should have 2 nodes (watch + template)")
	t.Logf("Child Graph spec.nodes is concrete []any with %d nodes", len(childNodes))

	// The child Graph should reconcile and create the result ConfigMap
	resultCM := &unstructured.Unstructured{}
	resultCM.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
	require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: "child-a-result", Namespace: ns}, resultCM))

	data, _, _ := unstructured.NestedStringMap(resultCM.Object, "data")
	assert.Equal(t, "hello-from-dynamic", data["value"],
		"child should resolve watch and use its data in the template")
	t.Logf("Child result ConfigMap: value=%s", data["value"])

	t.Log("Dynamic resource list proved: parent CEL expression → child concrete []any → child reconciles normally")
}

// TestFullRGDSystemL0L1L2 proves the complete 3-level Graph architecture from example 3.
//
//	L0 (rgd-controller): watches all RGDs, stamps one L1 Graph per RGD
//	L1 (per-RGD):        reads its RGD, creates the kind's CRD, watches instances, stamps L2 Graphs
//	L2 (per-instance):   reads its instance, creates resources, contributes status back
//
// This test pre-installs the RGD CRD (that would normally be created by L0) and
// focuses on proving the full reactive chain: RGD → CRD → instance → resources → status.
func TestFullRGDSystemL0L1L2(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)
	group := uniqueGroup()

	// RGD CRD is pre-installed in TestMain.

	// ── L0 Graph: the RGD controller ──
	l0Graph := buildRGDControllerGraph(ns)
	require.NoError(t, k8sClient.Create(ctx, l0Graph))
	t.Log("L0 Graph created: rgd-controller")

	// ── Create an RGD ──
	rgd := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "test.kro.run/v1alpha1",
			"kind":       "ResourceGraphDefinition",
			"metadata": map[string]any{
				"name":      "myapp",
				"namespace": ns,
			},
			"spec": map[string]any{
				"schema": map[string]any{
					"kind":       "MyApp",
					"apiVersion": "v1alpha1",
					"group":      group,
					"spec": map[string]any{
						"image":    "string | default=nginx",
						"replicas": "integer | default=1",
					},
				},
				"nodes": []any{
					map[string]any{
						"id": "config",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "${schema.metadata.name}-config",
							},
							"data": map[string]any{
								"image":    "${schema.spec.image}",
								"replicas": "${string(schema.spec.replicas)}",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, rgd))
	t.Log("RGD created: myapp")

	// ── L0→L1: Verify the L1 controller Graph is stamped ──
	l1Graph := &unstructured.Unstructured{}
	l1Graph.SetGroupVersionKind(GraphGVK)
	require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: "myapp-controller", Namespace: ns}, l1Graph))
	t.Log("L1: Controller Graph myapp-controller created")

	// ── L1: Verify the MyApp CRD is created ──
	crdName := "myapps." + group
	require.NoError(t, waitForCRD(ctx, k8sClient, crdName))
	t.Logf("L1: CRD %s created and established", crdName)
	t.Cleanup(func() {
		crd := &apiextensionsv1.CustomResourceDefinition{}
		crd.Name = crdName
		_ = k8sClient.Delete(context.Background(), crd)
	})

	// ── Create a MyApp instance ──
	myApp := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": group + "/v1alpha1",
			"kind":       "MyApp",
			"metadata": map[string]any{
				"name":      "test-instance",
				"namespace": ns,
			},
			"spec": map[string]any{
				"image":    "redis:7",
				"replicas": int64(3),
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, myApp))
	t.Log("MyApp instance created: test-instance")

	// ── L1→L2: Verify the instance Graph is stamped ──
	l2Graph := &unstructured.Unstructured{}
	l2Graph.SetGroupVersionKind(GraphGVK)
	require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: "test-instance-myapp", Namespace: ns}, l2Graph))
	t.Log("L2: Instance Graph test-instance-myapp created")

	// ── L2: Verify resources are created from RGD spec ──
	configCM := &unstructured.Unstructured{}
	configCM.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
	require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: "test-instance-config", Namespace: ns}, configCM))

	data, _, _ := unstructured.NestedStringMap(configCM.Object, "data")
	assert.Equal(t, "redis:7", data["image"])
	assert.Equal(t, "3", data["replicas"])
	t.Logf("L2: ConfigMap created: image=%s replicas=%s", data["image"], data["replicas"])

	t.Log("MILESTONE: Full L0→L1→L2 proved: RGD → CRD → instance → resources")

	// ── Unwind deletion: delete L0, verify full chain cleans up ──
	l0Latest := &unstructured.Unstructured{}
	l0Latest.SetGroupVersionKind(GraphGVK)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "rgd-controller", Namespace: ns}, l0Latest))
	require.NoError(t, k8sClient.Delete(ctx, l0Latest))
	t.Log("Deleting L0 Graph: rgd-controller")

	// L0 should be gone (its finalizer deletes L1 Graphs and waits for them to unwind)
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 15*time.Second, true, func(ctx context.Context) (bool, error) {
		check := &unstructured.Unstructured{}
		check.SetGroupVersionKind(GraphGVK)
		err := k8sClient.Get(ctx, types.NamespacedName{Name: "rgd-controller", Namespace: ns}, check)
		return err != nil, nil
	}))
	t.Log("L0 Graph deleted")

	// L1 should be gone (deleted by L0's finalizer, L1's finalizer cleaned up L2 and CRD)
	l1Check := &unstructured.Unstructured{}
	l1Check.SetGroupVersionKind(GraphGVK)
	err := k8sClient.Get(ctx, types.NamespacedName{Name: "myapp-controller", Namespace: ns}, l1Check)
	assert.Error(t, err, "L1 Graph should be deleted")
	t.Log("L1 Graph deleted")

	// L2 should be gone (deleted by L1's finalizer, L2's finalizer cleaned up resources)
	l2Check := &unstructured.Unstructured{}
	l2Check.SetGroupVersionKind(GraphGVK)
	err = k8sClient.Get(ctx, types.NamespacedName{Name: "test-instance-myapp", Namespace: ns}, l2Check)
	assert.Error(t, err, "L2 Graph should be deleted")
	t.Log("L2 Graph deleted")

	// L2's managed resources should be gone
	cmCheck := &unstructured.Unstructured{}
	cmCheck.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
	err = k8sClient.Get(ctx, types.NamespacedName{Name: "test-instance-config", Namespace: ns}, cmCheck)
	assert.Error(t, err, "L2 ConfigMap should be deleted")
	t.Log("L2 resources deleted")

	t.Log("UNWIND DELETION PROVED: L0 delete → L1 delete → L2 delete → resources deleted")
}

// TestRGDLifecyclePort is a port of the RGD integration test
// "should handle updates to instance resources correctly" (lifecycle_test.go).
//
// It proves that the Graph controller, implementing the RGD system as Graphs,
// can handle the same lifecycle: create RGD → create instance → verify Deployment →
// update instance spec → verify Deployment converges → delete instance → verify cleanup.
//
// This uses the same L0 Graph as TestFullRGDSystemL0L1L2 (the rgd-controller pattern)
// but with a Deployment resource instead of a ConfigMap.
func TestRGDLifecyclePort(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)
	group := uniqueGroup()

	// RGD CRD is pre-installed in TestMain.

	// Create the L0 Graph (RGD controller) — same pattern as TestFullRGDSystemL0L1L2
	l0Graph := buildRGDControllerGraph(ns)
	require.NoError(t, k8sClient.Create(ctx, l0Graph))
	t.Log("L0: rgd-controller Graph created")

	// Create the RGD with a Deployment resource
	rgd := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "test.kro.run/v1alpha1",
			"kind":       "ResourceGraphDefinition",
			"metadata": map[string]any{
				"name":      "test-update",
				"namespace": ns,
			},
			"spec": map[string]any{
				"schema": map[string]any{
					"kind":       "TestInstanceUpdate",
					"apiVersion": "v1alpha1",
					"group":      group,
					"spec": map[string]any{
						"replicas": "integer | default=1",
						"image":    "string | default=nginx:latest",
						"port":     "integer | default=80",
					},
				},
				"nodes": []any{
					map[string]any{
						"id": "deployment",
						"template": map[string]any{
							"apiVersion": "apps/v1",
							"kind":       "Deployment",
							"metadata": map[string]any{
								"name": "deployment-${schema.metadata.name}",
							},
							"spec": map[string]any{
								"replicas": "${schema.spec.replicas}",
								"selector": map[string]any{
									"matchLabels": map[string]any{"app": "test"},
								},
								"template": map[string]any{
									"metadata": map[string]any{
										"labels": map[string]any{"app": "test"},
									},
									"spec": map[string]any{
										"containers": []any{
											map[string]any{
												"name":  "app",
												"image": "${schema.spec.image}",
												"ports": []any{
													map[string]any{
														"containerPort": "${schema.spec.port}",
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
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, rgd))
	t.Log("RGD created: test-update")

	// Wait for the CRD to be created by the L1 Graph
	crdName := "testinstanceupdates." + group
	require.NoError(t, waitForCRD(ctx, k8sClient, crdName))
	t.Logf("L1: CRD %s established", crdName)
	t.Cleanup(func() {
		crd := &apiextensionsv1.CustomResourceDefinition{}
		crd.Name = crdName
		_ = k8sClient.Delete(context.Background(), crd)
	})

	// Create an instance
	instanceGVK := schema.GroupVersionKind{Group: group, Version: "v1alpha1", Kind: "TestInstanceUpdate"}
	instance := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": group + "/v1alpha1",
			"kind":       "TestInstanceUpdate",
			"metadata": map[string]any{
				"name":      "test-instance-for-updates",
				"namespace": ns,
			},
			"spec": map[string]any{
				"image":    "nginx:1.19",
				"port":     int64(80),
				"replicas": int64(1),
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, instance))
	t.Log("Instance created: test-instance-for-updates")

	// Verify Deployment is created with initial values
	deployGVK := schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}
	deploy := &unstructured.Unstructured{}
	deploy.SetGroupVersionKind(deployGVK)
	require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{
		Name: "deployment-test-instance-for-updates", Namespace: ns,
	}, deploy))

	replicas, _, _ := unstructured.NestedInt64(deploy.Object, "spec", "replicas")
	assert.Equal(t, int64(1), replicas)

	containers, _, _ := unstructured.NestedSlice(deploy.Object, "spec", "template", "spec", "containers")
	require.Len(t, containers, 1)
	c := containers[0].(map[string]any)
	assert.Equal(t, "nginx:1.19", c["image"])

	ports := c["ports"].([]any)
	require.Len(t, ports, 1)
	p := ports[0].(map[string]any)
	// containerPort may come back as int64 or float64
	assert.EqualValues(t, 80, p["containerPort"])
	t.Log("Deployment verified: replicas=1, image=nginx:1.19, port=80")

	// Update instance spec — same as lifecycle_test.go
	require.NoError(t, updateWithRetry(ctx, k8sClient, instanceGVK,
		types.NamespacedName{Name: "test-instance-for-updates", Namespace: ns}, func(obj *unstructured.Unstructured) {
			unstructured.SetNestedField(obj.Object, int64(3), "spec", "replicas")
			unstructured.SetNestedField(obj.Object, "nginx:1.20", "spec", "image")
			unstructured.SetNestedField(obj.Object, int64(443), "spec", "port")
		}))
	t.Log("Instance updated: replicas=3, image=nginx:1.20, port=443")

	// Verify Deployment converges to new values.
	// Multi-level convergence (instance → L1 Graph → L2 Graph → Deployment)
	// can be slow under parallel test load.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		d := &unstructured.Unstructured{}
		d.SetGroupVersionKind(deployGVK)
		if err := k8sClient.Get(ctx, types.NamespacedName{
			Name: "deployment-test-instance-for-updates", Namespace: ns,
		}, d); err != nil {
			return false, nil
		}
		r, _, _ := unstructured.NestedInt64(d.Object, "spec", "replicas")
		if r != 3 {
			return false, nil
		}
		cs, _, _ := unstructured.NestedSlice(d.Object, "spec", "template", "spec", "containers")
		if len(cs) == 0 {
			return false, nil
		}
		cm := cs[0].(map[string]any)
		if cm["image"] != "nginx:1.20" {
			return false, nil
		}
		ps := cm["ports"].([]any)
		if len(ps) == 0 {
			return false, nil
		}
		pm := ps[0].(map[string]any)
		portVal, _ := pm["containerPort"]
		// Handle both int64 and float64
		switch v := portVal.(type) {
		case int64:
			return v == 443, nil
		case float64:
			return v == 443, nil
		}
		return false, nil
	}))
	t.Log("Deployment converged: replicas=3, image=nginx:1.20, port=443")

	// Delete instance — verify cascade cleanup
	toDelete := &unstructured.Unstructured{}
	toDelete.SetGroupVersionKind(instanceGVK)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{
		Name: "test-instance-for-updates", Namespace: ns,
	}, toDelete))
	require.NoError(t, k8sClient.Delete(ctx, toDelete))

	// Verify Deployment is cleaned up (L2 finalizer deletes it).
	// Multi-level cascade cleanup can take longer under parallel test load.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		d := &unstructured.Unstructured{}
		d.SetGroupVersionKind(deployGVK)
		err := k8sClient.Get(ctx, types.NamespacedName{
			Name: "deployment-test-instance-for-updates", Namespace: ns,
		}, d)
		return err != nil, nil
	}))
	t.Log("Deployment deleted after instance deletion — cascade cleanup proved")

	t.Log("RGD LIFECYCLE PORT PASSED: create → verify → update → converge → delete → cleanup")
}

// TestRGDForEachNonListFieldCompilationError was removed.
//
// It asserted that the PARENT (L1) Graph's Compiled condition would be False
// when a child node used forEach over a non-list field. This was caught by
// precompileExpressionChildGraphs, which has been removed.
//
// Per design 004-compilation.md § Recursive Compilation: "When conditions
// aren't met, the child compiles independently at reconcile time." The child
// Graph's own compilation catches the forEach error. The parent compiles fine.
