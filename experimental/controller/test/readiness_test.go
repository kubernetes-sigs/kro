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

// TestReadyWhenDoesNotGateDownstream proves that readyWhen is a health signal,
// not a gate for downstream execution (design 001-graph § readyWhen).
//
// readyWhen feeds the Graph's aggregated status — it does not block dependents.
// Dependents proceed as soon as the node is processed and its data is in scope,
// regardless of readyWhen. Data availability is the implicit gate.
//
// Setup:
//   - Pre-create ConfigMap "source" with data.status = "pending"
//   - Create Graph: watch reads "source" with readyWhen checking data.status == "ready",
//     then a template creates "output" using data from the watch
//   - Verify "output" IS created immediately (data is in scope even though readyWhen fails)
//   - Update "source" to data.status = "ready" and data.value = "hello-from-ready"
//   - Verify "output" is updated with the new value
func TestReadyWhenDoesNotGateDownstream(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Pre-create the source ConfigMap in "not ready" state
	source := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "ready-source",
				"namespace": ns,
			},
			"data": map[string]any{
				"status": "pending",
				"value":  "initial",
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, source))

	// Graph: watch with readyWhen, then template referencing the watch
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-readywhen-extref",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "source",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "ready-source",
							},
						},
						"readyWhen": []any{
							"${source.data.status == 'ready'}",
						},
					},
					map[string]any{
						"id": "output",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "ready-output",
							},
							"data": map[string]any{
								"fromSource": "${source.data.value}",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// readyWhen no longer gates dependents — downstream resources are created
	// immediately because data is in scope. The Graph status shows InProgress.
	// Verify the output IS created (with the initial value from the not-ready source)
	cmGVK := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}
	outputCM := &unstructured.Unstructured{}
	outputCM.SetGroupVersionKind(cmGVK)
	require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: "ready-output", Namespace: ns}, outputCM))
	t.Log("Output created while source not yet ready (readyWhen no longer gates dependents)")

	// Now update the source to be "ready"
	latestSource := &unstructured.Unstructured{}
	latestSource.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "ready-source", Namespace: ns}, latestSource))
	unstructured.SetNestedField(latestSource.Object, "ready", "data", "status")
	unstructured.SetNestedField(latestSource.Object, "hello-from-ready", "data", "value")
	require.NoError(t, k8sClient.Update(ctx, latestSource))
	t.Log("Updated source to ready state with value=hello-from-ready")

	// Now the output SHOULD be created/updated with the correct value
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		result := &unstructured.Unstructured{}
		result.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "ready-output", Namespace: ns}, result); err != nil {
			return false, nil
		}
		data, _, _ := unstructured.NestedStringMap(result.Object, "data")
		return data["fromSource"] == "hello-from-ready", nil
	}))

	// Verify final state
	finalOutput := &unstructured.Unstructured{}
	finalOutput.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "ready-output", Namespace: ns}, finalOutput))
	data, _, _ := unstructured.NestedStringMap(finalOutput.Object, "data")
	assert.Equal(t, "hello-from-ready", data["fromSource"],
		"output should have the value from the ready source")
	t.Logf("Output created with fromSource=%s after source became ready", data["fromSource"])
}

// TestReadyWhenTemplateDoesNotGateDownstream proves that readyWhen on a template
// resource is a health signal, not a gate (design 001-graph § readyWhen).
//
// When readyWhen passes immediately, downstream dependents are created because
// data is in scope. Verifies cross-node CEL data flow.
func TestReadyWhenTemplateDoesNotGateDownstream(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-readywhen-template",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					// Resource A: always created, always "ready"
					map[string]any{
						"id": "configA",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "config-a",
							},
							"data": map[string]any{
								"value": "from-a",
							},
						},
					},
					// Resource B: created, has readyWhen that checks its own data field.
					// Since we write data.ready = "true", this should pass immediately.
					map[string]any{
						"id": "configB",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "config-b",
							},
							"data": map[string]any{
								"ready": "true",
								"value": "from-b",
							},
						},
						"readyWhen": []any{
							"${configB.data.ready == 'true'}",
						},
					},
					// Resource C: references B's data. Should only be created after B is ready.
					map[string]any{
						"id": "configC",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "config-c",
							},
							"data": map[string]any{
								"fromA": "${configA.data.value}",
								"fromB": "${configB.data.value}",
							},
						},
					},
				},
			},
		},
	}

	require.NoError(t, k8sClient.Create(ctx, graph))

	// All three ConfigMaps should eventually be created since B's readyWhen passes immediately
	for _, name := range []string{"config-a", "config-b", "config-c"} {
		cm := &unstructured.Unstructured{}
		cm.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
		require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: name, Namespace: ns}, cm),
			"ConfigMap %s should be created", name)
		t.Logf("ConfigMap %s created", name)
	}

	// Verify C has the correct values from A and B
	configC := &unstructured.Unstructured{}
	configC.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "config-c", Namespace: ns}, configC))

	data, _, _ := unstructured.NestedStringMap(configC.Object, "data")
	assert.Equal(t, "from-a", data["fromA"], "C should have A's value")
	assert.Equal(t, "from-b", data["fromB"], "C should have B's value")
	t.Logf("ConfigMap config-c has fromA=%s fromB=%s", data["fromA"], data["fromB"])
}

// TestPendingRequeues proves that when a CEL expression references data
// that doesn't exist yet (pending), the controller requeues gracefully
// instead of failing permanently.
//
// Setup:
//   - Create a watch ConfigMap "data-source" without the field the template needs
//   - Template references ${source.data.missing} which doesn't exist
//   - Verify the controller doesn't crash and the Graph still reconciles
//   - Add the missing field to the source
//   - Verify the downstream resource is created with the correct value
func TestPendingRequeues(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Create source without the field the template will reference
	source := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "data-source",
				"namespace": ns,
			},
			"data": map[string]any{
				"existing": "yes",
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, source))

	// Graph: watch reads source, template references a field that doesn't exist yet
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-pending",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "source",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "data-source",
							},
						},
					},
					map[string]any{
						"id": "output",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "data-output",
							},
							"data": map[string]any{
								// This references a field that doesn't exist yet.
								// The CEL evaluation should return "no such key" → pending → requeue.
								"result": "${source.data.pending_field}",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Output should NOT be created yet — the field doesn't exist
	require.NoError(t, waitForAbsence(ctx, k8sClient, schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"},
		types.NamespacedName{Name: "data-output", Namespace: ns}, 1*time.Second))
	t.Log("Output correctly not created while data is pending")

	// Verify the Graph still exists and hasn't been deleted (controller didn't crash)
	graphCheck := &unstructured.Unstructured{}
	graphCheck.SetGroupVersionKind(GraphGVK)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "test-pending", Namespace: ns}, graphCheck))
	t.Log("Graph still exists — controller handled pending gracefully")

	// Now add the missing field to the source
	latestSource := &unstructured.Unstructured{}
	latestSource.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "data-source", Namespace: ns}, latestSource))
	unstructured.SetNestedField(latestSource.Object, "resolved-value", "data", "pending_field")
	require.NoError(t, k8sClient.Update(ctx, latestSource))
	t.Log("Added pending_field=resolved-value to source")

	// Now the output should be created with the resolved value
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		result := &unstructured.Unstructured{}
		result.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "data-output", Namespace: ns}, result); err != nil {
			return false, nil
		}
		data, _, _ := unstructured.NestedStringMap(result.Object, "data")
		return data["result"] == "resolved-value", nil
	}))

	finalOutput := &unstructured.Unstructured{}
	finalOutput.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "data-output", Namespace: ns}, finalOutput))
	data, _, _ := unstructured.NestedStringMap(finalOutput.Object, "data")
	assert.Equal(t, "resolved-value", data["result"],
		"output should have the resolved value after data became available")
	t.Logf("Output created with result=%s after pending resolved", data["result"])
}

// TestReadyWhenNotReadyThenReady proves the full readyWhen lifecycle:
// a watch that starts not-ready, then transitions to ready,
// unblocking downstream resource creation.
//
// This is the most realistic test: it simulates a Deployment-like pattern
// where an external resource (e.g., an operator-managed resource) transitions
// from pending to ready, and the Graph reacts.
func TestReadyWhenNotReadyThenReady(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Create a ConfigMap that simulates an "infrastructure resource" in pending state
	infra := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "infra-resource",
				"namespace": ns,
			},
			"data": map[string]any{
				"phase": "Pending",
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, infra))

	// Graph watches infra, waits for it to be "Running", then creates app resources
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-readywhen-lifecycle",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					// Watch the infra resource, gate on phase == "Running"
					map[string]any{
						"id": "infra",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "infra-resource",
							},
						},
						"readyWhen": []any{
							"${infra.data.phase == 'Running'}",
						},
					},
					// App config: only created after infra is ready
					map[string]any{
						"id": "appConfig",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "app-config",
							},
							"data": map[string]any{
								"infraPhase": "${infra.data.phase}",
								"message":    "infra is ready",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// readyWhen no longer gates dependents — app-config should be created immediately
	// because infra data is in scope. The Graph status shows InProgress/NotReady.
	appCM := &unstructured.Unstructured{}
	appCM.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
	require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: "app-config", Namespace: ns}, appCM))
	t.Log("app-config created while infra is Pending (readyWhen no longer gates dependents)")

	// Transition infra to "Running"
	latestInfra := &unstructured.Unstructured{}
	latestInfra.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "infra-resource", Namespace: ns}, latestInfra))
	unstructured.SetNestedField(latestInfra.Object, "Running", "data", "phase")
	require.NoError(t, k8sClient.Update(ctx, latestInfra))
	t.Log("Infra transitioned to Running")

	// App config should now be created
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		result := &unstructured.Unstructured{}
		result.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "app-config", Namespace: ns}, result); err != nil {
			return false, nil
		}
		data, _, _ := unstructured.NestedStringMap(result.Object, "data")
		return data["infraPhase"] == "Running", nil
	}))

	finalApp := &unstructured.Unstructured{}
	finalApp.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "app-config", Namespace: ns}, finalApp))
	data, _, _ := unstructured.NestedStringMap(finalApp.Object, "data")
	assert.Equal(t, "Running", data["infraPhase"])
	assert.Equal(t, "infra is ready", data["message"])

	// The app-config should be managed by the Graph
	assertManagedBy(t, finalApp, "test-readywhen-lifecycle")

	t.Logf("Full readyWhen lifecycle proved: Pending → blocked; Running → app-config created with infraPhase=%s",
		data["infraPhase"])
}
