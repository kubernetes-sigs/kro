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

// TestGraphCreatesDeploymentAndService proves the core reconciliation feedback loop:
// A Graph with a Deployment and a Service where the Service references the
// Deployment's metadata.name and metadata.uid (server-assigned field).
func TestGraphCreatesDeploymentAndService(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-app",
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
								"name": "my-app",
							},
							"spec": map[string]any{
								"replicas": int64(3),
								"selector": map[string]any{
									"matchLabels": map[string]any{
										"app": "my-app",
									},
								},
								"template": map[string]any{
									"metadata": map[string]any{
										"labels": map[string]any{
											"app": "my-app",
										},
									},
									"spec": map[string]any{
										"containers": []any{
											map[string]any{
												"name":  "app",
												"image": "nginx",
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
								// This expression references the Deployment read back from the API server.
								// The name comes from the actual object, proving the reconciliation loop works.
								"name": "${deployment.metadata.name}-svc",
								"annotations": map[string]any{
									// This references a server-assigned field — the UID.
									// If this evaluates correctly, it proves the API round-trip works.
									"deployment-uid": "${deployment.metadata.uid}",
								},
							},
							"spec": map[string]any{
								"selector": "${deployment.spec.selector.matchLabels}",
								"ports": []any{
									map[string]any{
										"port":     int64(80),
										"protocol": "TCP",
									},
								},
							},
						},
					},
				},
			},
		},
	}

	// Create the Graph
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for the Deployment to be created
	deployment := &unstructured.Unstructured{}
	deployment.SetGroupVersionKind(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"})
	require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: "my-app", Namespace: ns}, deployment))

	t.Logf("Deployment created: name=%s uid=%s", deployment.GetName(), deployment.GetUID())

	// The Deployment should have an owner reference back to the Graph
	// Verify Deployment is managed by the Graph (labels, not owner refs)
	assertManagedBy(t, deployment, "test-app")

	// Wait for the Service to be created
	svc := &unstructured.Unstructured{}
	svc.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Service"})
	require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: "my-app-svc", Namespace: ns}, svc))

	t.Logf("Service created: name=%s", svc.GetName())

	// THE KEY ASSERTION: Service name is "${deployment.metadata.name}-svc" = "my-app-svc"
	assert.Equal(t, "my-app-svc", svc.GetName())

	// THE PROOF: Service annotation has the Deployment's UID — a server-assigned field.
	// This value came from the API server, not from mock data.
	annotations := svc.GetAnnotations()
	require.NotNil(t, annotations)
	deployUID := string(deployment.GetUID())
	assert.NotEmpty(t, deployUID, "deployment UID should be set by API server")
	assert.Equal(t, deployUID, annotations["deployment-uid"],
		"service annotation should contain the deployment's server-assigned UID")
	t.Logf("Service annotation deployment-uid=%s matches Deployment UID=%s", annotations["deployment-uid"], deployUID)

	// Verify selector was evaluated from the Deployment's spec
	svcSpec, _, _ := unstructured.NestedMap(svc.Object, "spec", "selector")
	assert.Equal(t, "my-app", svcSpec["app"])

	// Verify owner reference on service
	assertManagedBy(t, svc, "test-app")
}

// TestGraphIncludeWhen proves that conditional resource inclusion works.
func TestGraphIncludeWhen(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-conditional",
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
								"name": "my-config",
							},
							"data": map[string]any{
								"replicas": "3",
							},
						},
					},
					map[string]any{
						"id": "included",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "included-config",
							},
							"data": map[string]any{
								"source": "${configmap.metadata.name}",
							},
						},
						// This evaluates to true: string "3" > "1" is true in CEL
						"includeWhen": []any{
							"${configmap.data.replicas > '1'}",
						},
					},
					map[string]any{
						"id": "excluded",
						"ref": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "excluded-config",
							},
						},
						// This evaluates to false: "3" > "9" is false
						"includeWhen": []any{
							"${configmap.data.replicas > '9'}",
						},
					},
				},
			},
		},
	}

	require.NoError(t, k8sClient.Create(ctx, graph))

	// ConfigMap "my-config" should be created
	cm := &unstructured.Unstructured{}
	cm.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
	require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: "my-config", Namespace: ns}, cm))

	// ConfigMap "included-config" should be created (condition is true)
	included := &unstructured.Unstructured{}
	included.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
	require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: "included-config", Namespace: ns}, included))

	// Verify the included configmap has the evaluated expression
	data, _, _ := unstructured.NestedStringMap(included.Object, "data")
	assert.Equal(t, "my-config", data["source"])

	// ConfigMap "excluded-config" should NOT be created
	require.NoError(t, waitForAbsence(ctx, k8sClient,
		schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"},
		types.NamespacedName{Name: "excluded-config", Namespace: ns}, 1*time.Second))
}

// TestGraphReconcilesOnUpdate proves that updating a Graph re-evaluates and updates resources.
func TestGraphReconcilesOnUpdate(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-update",
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
								"name": "update-test",
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
	require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: "update-test", Namespace: ns}, cm))

	data, _, _ := unstructured.NestedStringMap(cm.Object, "data")
	assert.Equal(t, "v1", data["version"])

	// Update the Graph — change the configmap data
	require.NoError(t, updateWithRetry(ctx, k8sClient, GraphGVK,
		types.NamespacedName{Name: "test-update", Namespace: ns}, func(obj *unstructured.Unstructured) {
			nodes := []any{
				map[string]any{
					"id": "configmap",
					"template": map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata": map[string]any{
							"name": "update-test",
						},
						"data": map[string]any{
							"version": "v2",
						},
					},
				},
			}
			unstructured.SetNestedSlice(obj.Object, nodes, "spec", "nodes")
		}))

	// Wait for the ConfigMap to be updated
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		cm2 := &unstructured.Unstructured{}
		cm2.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "update-test", Namespace: ns}, cm2); err != nil {
			return false, nil
		}
		data, _, _ := unstructured.NestedStringMap(cm2.Object, "data")
		return data["version"] == "v2", nil
	}))

	t.Log("ConfigMap updated from v1 to v2")
}

// TestNestedGraphEvaluationBoundary proves the design's core architectural claim:
// Graphs nest through Kubernetes persistence. The API server is the evaluation boundary.
//
// Parent Graph (L0):
//   - Creates ConfigMap "shared-data" with data.message = "hello-from-parent"
//   - Creates child Graph where $${...} is stripped to ${...} before persistence
//
// Child Graph (L1) — reconciled independently by the same controller:
//   - watch reads ConfigMap "shared-data" from API server into scope
//   - Evaluates ${...} expressions (which were $${...} at L0) against its own scope
//   - Creates ConfigMap "shared-data-result" with data.output = "hello-from-parent"
//
// The proof: a value that traversed parent→API server→child→API server→result
// with $${} stripping at the evaluation boundary.
func TestNestedGraphEvaluationBoundary(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	parentGraph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "parent",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					// Resource 1: ConfigMap with data the child will read
					map[string]any{
						"id": "sharedData",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "shared-data",
							},
							"data": map[string]any{
								"message": "hello-from-parent",
							},
						},
					},
					// Resource 2: Child Graph
					// ${...} is evaluated at L0 (parent scope).
					// $${...} is stripped to ${...} and persisted — evaluated at L1 (child scope).
					map[string]any{
						"id": "childGraph",
						"template": map[string]any{
							"apiVersion": "experimental.kro.run/v1alpha1",
							"kind":       "Graph",
							"metadata": map[string]any{
								// Evaluated at L0: ${sharedData.metadata.name} → "shared-data"
								"name": "${sharedData.metadata.name}-child",
							},
							"spec": map[string]any{
								"nodes": []any{
									// Child resource 1: watch reads the ConfigMap into child scope
									map[string]any{
										"id": "input",
										"ref": map[string]any{
											"apiVersion": "v1",
											"kind":       "ConfigMap",
											"metadata": map[string]any{
												// Evaluated at L0: → "shared-data"
												"name": "${sharedData.metadata.name}",
											},
										},
									},
									// Child resource 2: template with $${...} → ${...} at L1
									map[string]any{
										"id": "result",
										"template": map[string]any{
											"apiVersion": "v1",
											"kind":       "ConfigMap",
											"metadata": map[string]any{
												// $${...} stripped at L0 → ${input.metadata.name}-result
												// Evaluated at L1 → "shared-data-result"
												"name": "$${input.metadata.name}-result",
											},
											"data": map[string]any{
												// $${...} stripped at L0 → ${input.data.message}
												// Evaluated at L1 → "hello-from-parent"
												"output": "$${input.data.message}",
												// Mix: L0-evaluated value alongside L1-deferred expression
												"parentName": "${sharedData.metadata.name}",
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

	require.NoError(t, k8sClient.Create(ctx, parentGraph))

	// --- L0 assertions: parent creates ConfigMap and child Graph ---

	// ConfigMap "shared-data" should be created by parent
	sharedData := &unstructured.Unstructured{}
	sharedData.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
	require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: "shared-data", Namespace: ns}, sharedData))
	t.Logf("L0: ConfigMap shared-data created")

	// Child Graph should be created by parent with ${...} stripped from $${...}
	childGraph := &unstructured.Unstructured{}
	childGraph.SetGroupVersionKind(GraphGVK)
	require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: "shared-data-child", Namespace: ns}, childGraph))
	t.Logf("L0: Child Graph shared-data-child created (uid=%s)", childGraph.GetUID())

	// Verify the child Graph's spec contains ${...} (was $${...} in parent, stripped by one $)
	childNodes, _, _ := unstructured.NestedSlice(childGraph.Object, "spec", "nodes")
	require.Len(t, childNodes, 2, "child Graph should have 2 nodes (watch + template)")

	// The result template should have ${input.metadata.name}-result (not $${...})
	resultRes := childNodes[1].(map[string]any)
	resultTmpl := resultRes["template"].(map[string]any)
	resultMeta := resultTmpl["metadata"].(map[string]any)
	assert.Equal(t, "${input.metadata.name}-result", resultMeta["name"],
		"$${...} should be stripped to ${...} after L0 evaluation and API server persistence")

	resultData := resultTmpl["data"].(map[string]any)
	assert.Equal(t, "${input.data.message}", resultData["output"],
		"$${...} should be stripped to ${...} after L0 evaluation")
	assert.Equal(t, "shared-data", resultData["parentName"],
		"${...} at L0 should be fully evaluated to the concrete value")

	// Child Graph should be managed by parent Graph
	assertManagedBy(t, childGraph, "parent")

	// --- L1 assertions: child Graph reconciled independently, creates result ConfigMap ---

	// The child controller reads "shared-data" via watch, evaluates ${input.data.message}
	// → "hello-from-parent", and creates ConfigMap "shared-data-result".
	resultCM := &unstructured.Unstructured{}
	resultCM.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
	require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: "shared-data-result", Namespace: ns}, resultCM),
		"child Graph should create the result ConfigMap")
	t.Logf("L1: ConfigMap shared-data-result created (uid=%s)", resultCM.GetUID())

	// THE PROOF: data.output = "hello-from-parent"
	// This value traversed: parent template → ConfigMap → API server → watch read by
	// child → CEL evaluation of ${input.data.message} → child template → API server
	resultCMData, _, _ := unstructured.NestedStringMap(resultCM.Object, "data")
	assert.Equal(t, "hello-from-parent", resultCMData["output"],
		"child Graph should evaluate ${input.data.message} → value from watch-read ConfigMap")
	assert.Equal(t, "shared-data", resultCMData["parentName"],
		"L0-evaluated value should survive as concrete string in child spec")
	t.Logf("L1: data.output=%q data.parentName=%q — values traversed the evaluation boundary",
		resultCMData["output"], resultCMData["parentName"])

	// Result ConfigMap should be managed by child Graph (cascading management)
	assertManagedBy(t, resultCM, "shared-data-child")
	t.Logf("L1: Management chain: parent Graph → child Graph → result ConfigMap")
}

// TestIdenticalGraphsConvergeIndependently proves that multiple Graph CRs
// with identical specs all converge correctly. This is the regression test
// for content-addressed compiled graph sharing: internally, these graphs
// share a single compiledGraph (CEL env, programs, DAG). The test verifies
// that sharing doesn't cause cross-instance interference — each graph
// produces its own distinct resources with the correct data.
//
// The proof: three graphs with the same spec but different watch targets
// each produce a correctly-evaluated ConfigMap reading from their own source.
func TestIdenticalGraphsConvergeIndependently(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Create 3 source ConfigMaps with different values.
	sources := []struct {
		name  string
		value string
	}{
		{"source-alpha", "value-alpha"},
		{"source-beta", "value-beta"},
		{"source-gamma", "value-gamma"},
	}
	for _, src := range sources {
		cm := &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name":      src.name,
					"namespace": ns,
				},
				"data": map[string]any{
					"message": src.value,
				},
			},
		}
		require.NoError(t, k8sClient.Create(ctx, cm))
	}

	// Create 3 Graph CRs with identical spec structure — they differ only
	// in which source ConfigMap they watch (passed via the watch name).
	// The spec structure is the same: watch → produce output ConfigMap.
	for i, src := range sources {
		graph := &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "experimental.kro.run/v1alpha1",
				"kind":       "Graph",
				"metadata": map[string]any{
					"name":      fmt.Sprintf("identical-%d", i),
					"namespace": ns,
				},
				"spec": map[string]any{
					"nodes": []any{
						map[string]any{
							"id": "input",
							"ref": map[string]any{
								"apiVersion": "v1",
								"kind":       "ConfigMap",
								"metadata": map[string]any{
									"name": src.name,
								},
							},
						},
						map[string]any{
							"id": "output",
							"template": map[string]any{
								"apiVersion": "v1",
								"kind":       "ConfigMap",
								"metadata": map[string]any{
									"name": fmt.Sprintf("output-%d", i),
								},
								"data": map[string]any{
									"copied": "${input.data.message}",
								},
							},
						},
					},
				},
			},
		}
		require.NoError(t, k8sClient.Create(ctx, graph))
	}

	// Verify each graph produced its output ConfigMap with the correct value.
	for i, src := range sources {
		outputName := fmt.Sprintf("output-%d", i)
		output := &unstructured.Unstructured{}
		output.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
		require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{
			Name: outputName, Namespace: ns,
		}, output), "output ConfigMap %s should exist", outputName)

		// Verify the copied value is correct — this proves the graph evaluated
		// against its own scope, not another instance's scope.
		data, _, _ := unstructured.NestedStringMap(output.Object, "data")
		assert.Equal(t, src.value, data["copied"],
			"graph %d should have copied %q from source %s", i, src.value, src.name)

		// Verify ownership labels point to the correct graph.
		assertManagedBy(t, output, fmt.Sprintf("identical-%d", i))
		t.Logf("graph identical-%d → output-%d: copied=%q (correct)", i, i, data["copied"])
	}
}
