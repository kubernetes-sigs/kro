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

	graphcontroller "github.com/kubernetes-sigs/kro/experimental/graph/controller"
)

// ---------------------------------------------------------------------------
// propagateWhen — design 001-graph, 004-graph-execution
// ---------------------------------------------------------------------------

// TestPropagateWhenGatesDataFlow proves that propagateWhen gates data flow to
// dependents during transitions. When propagateWhen is unsatisfied, dependents
// retain their previous scope and are not re-evaluated.
//
// Setup:
//   - Pre-create ConfigMap "deploy-sim" with replicas=3, updatedReplicas=3
//   - Create Graph: watch reads "deploy-sim" with propagateWhen checking
//     updatedReplicas == replicas, then a template creates "service-output"
//     using data from the watch.
//   - Verify "service-output" is created with correct data.
//   - Simulate a rollout: update "deploy-sim" to replicas=5, updatedReplicas=3
//     (mid-transition — propagateWhen unsatisfied).
//   - Verify "service-output" retains its previous data (not re-evaluated
//     with transitional state).
//   - Complete the rollout: updatedReplicas=5 (propagateWhen satisfied).
//   - Verify "service-output" is updated with the new data.
func TestPropagateWhenGatesDataFlow(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Pre-create the "deployment" simulator — initially converged.
	deploy := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "deploy-sim",
				"namespace": ns,
			},
			"data": map[string]any{
				"replicas":        "3",
				"updatedReplicas": "3",
				"image":           "nginx:1.25",
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, deploy))

	// Graph: watch deploy-sim with propagateWhen, then template referencing it.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-propagate-when",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "deploy",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "deploy-sim",
							},
						},
						"readyWhen": []any{
							"${deploy.data.updatedReplicas == deploy.data.replicas}",
						},
						"propagateWhen": []any{
							"${deploy.data.updatedReplicas == deploy.data.replicas}",
						},
					},
					map[string]any{
						"id": "service",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "service-output",
							},
							"data": map[string]any{
								"deployImage": "${deploy.data.image}",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for service-output to be created with initial data.
	cmGVK := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}
	svc := &unstructured.Unstructured{}
	svc.SetGroupVersionKind(cmGVK)
	require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: "service-output", Namespace: ns}, svc))

	data, _, _ := unstructured.NestedStringMap(svc.Object, "data")
	assert.Equal(t, "nginx:1.25", data["deployImage"],
		"service-output should have initial image")
	t.Log("service-output created with deployImage=nginx:1.25")

	// Wait for the Graph to settle before triggering the transition
	require.NoError(t, waitForSettle(ctx, k8sClient, GraphGVK,
		types.NamespacedName{Name: "test-propagate-when", Namespace: ns}))

	// Simulate a rollout: change image and replicas, but updatedReplicas lags.
	// This makes propagateWhen unsatisfied.
	latest := &unstructured.Unstructured{}
	latest.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "deploy-sim", Namespace: ns}, latest))
	unstructured.SetNestedField(latest.Object, "5", "data", "replicas")
	unstructured.SetNestedField(latest.Object, "3", "data", "updatedReplicas")
	unstructured.SetNestedField(latest.Object, "nginx:1.26", "data", "image")
	require.NoError(t, k8sClient.Update(ctx, latest))
	t.Log("Simulated rollout: replicas=5, updatedReplicas=3, image=nginx:1.26")

	// Wait a bit for the reconcile to process the change.
	time.Sleep(2 * time.Second)

	// Service should still have the OLD image because propagateWhen is
	// unsatisfied — the transitional state should not flow to dependents.
	svcCheck := &unstructured.Unstructured{}
	svcCheck.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "service-output", Namespace: ns}, svcCheck))
	dataCheck, _, _ := unstructured.NestedStringMap(svcCheck.Object, "data")
	assert.Equal(t, "nginx:1.25", dataCheck["deployImage"],
		"service-output should retain old image while propagateWhen is unsatisfied")
	t.Logf("service-output correctly retained deployImage=%s during transition", dataCheck["deployImage"])

	// Complete the rollout: updatedReplicas catches up.
	latest2 := &unstructured.Unstructured{}
	latest2.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "deploy-sim", Namespace: ns}, latest2))
	unstructured.SetNestedField(latest2.Object, "5", "data", "updatedReplicas")
	require.NoError(t, k8sClient.Update(ctx, latest2))
	t.Log("Completed rollout: updatedReplicas=5")

	// Now the service should be updated with the new image.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 5*time.Second, true, func(ctx context.Context) (bool, error) {
		result := &unstructured.Unstructured{}
		result.SetGroupVersionKind(cmGVK)
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "service-output", Namespace: ns}, result); err != nil {
			return false, nil
		}
		d, _, _ := unstructured.NestedStringMap(result.Object, "data")
		return d["deployImage"] == "nginx:1.26", nil
	}))

	finalSvc := &unstructured.Unstructured{}
	finalSvc.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "service-output", Namespace: ns}, finalSvc))
	finalData, _, _ := unstructured.NestedStringMap(finalSvc.Object, "data")
	assert.Equal(t, "nginx:1.26", finalData["deployImage"],
		"service-output should have new image after propagateWhen satisfied")
	t.Logf("propagateWhen lifecycle proved: gate held during transition, released after convergence")
}

// TestPropagateWhenDoesNotBlockIndependentBranches proves that propagateWhen
// on one branch does not affect independent branches in the DAG.
func TestPropagateWhenDoesNotBlockIndependentBranches(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Pre-create the source ConfigMap — propagateWhen will be unsatisfied
	source := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "propagate-source",
				"namespace": ns,
			},
			"data": map[string]any{
				"ready": "false",
				"value": "initial",
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, source))

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-propagate-independent",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					// Branch A: watch with unsatisfied propagateWhen
					map[string]any{
						"id": "watched",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "propagate-source",
							},
						},
						"propagateWhen": []any{
							"${watched.data.ready == 'true'}",
						},
					},
					// Branch A dependent: depends on watched
					map[string]any{
						"id": "dependent",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "propagate-dependent",
							},
							"data": map[string]any{
								"fromWatched": "${watched.data.value}",
							},
						},
					},
					// Branch B: independent — no dependency on watched
					map[string]any{
						"id": "independent",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "propagate-independent",
							},
							"data": map[string]any{
								"value": "always-created",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Independent branch should be created regardless of propagateWhen gate.
	cmGVK := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}
	indep := &unstructured.Unstructured{}
	indep.SetGroupVersionKind(cmGVK)
	require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: "propagate-independent", Namespace: ns}, indep))
	t.Log("Independent branch created despite propagateWhen gate on other branch")
}

// ---------------------------------------------------------------------------
// Cycle detection — safety invariant
// ---------------------------------------------------------------------------

// TestCycleDetectionRejectsSpec proves that a Graph with circular dependencies
// is rejected with Accepted=False, CycleDetected reason. No revision is created
// and no resources are applied.
func TestCycleDetectionRejectsSpec(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Graph with A → B → A cycle
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-cycle",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "nodeA",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "cycle-a",
							},
							"data": map[string]any{
								"fromB": "${nodeB.data.value}",
							},
						},
					},
					map[string]any{
						"id": "nodeB",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "cycle-b",
							},
							"data": map[string]any{
								"fromA": "${nodeA.data.value}",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Graph should reach Accepted=False with CycleDetected reason
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		g := &unstructured.Unstructured{}
		g.SetGroupVersionKind(GraphGVK)
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "test-cycle", Namespace: ns}, g); err != nil {
			return false, nil
		}
		status, _ := g.Object["status"].(map[string]any)
		if status == nil {
			return false, nil
		}
		conditions, _ := status["conditions"].([]any)
		cond, found := findCondition(conditions, "Accepted")
		if !found {
			return false, nil
		}
		return cond["status"] == "False", nil
	}))

	// Verify the specific reason
	g := &unstructured.Unstructured{}
	g.SetGroupVersionKind(GraphGVK)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "test-cycle", Namespace: ns}, g))

	status, _ := g.Object["status"].(map[string]any)
	conditions, _ := status["conditions"].([]any)
	accepted, found := findCondition(conditions, "Accepted")
	require.True(t, found, "Accepted condition should exist")
	assert.Equal(t, "False", accepted["status"])
	assert.Equal(t, "CycleDetected", accepted["reason"],
		"reason should be CycleDetected")
	t.Logf("Cycle correctly detected: reason=%s, message=%s", accepted["reason"], accepted["message"])

	// Verify state is Error
	assert.Equal(t, "Error", status["state"], "state should be Error when cycle detected")

	// No revision should be created
	count, err := countRevisions(ctx, k8sClient, "test-cycle", ns)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "no revision should be created for cyclic Graph")
	t.Log("No revision created — safety invariant holds")

	// No resources should be created
	require.NoError(t, waitForAbsence(ctx, k8sClient,
		schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"},
		types.NamespacedName{Name: "cycle-a", Namespace: ns}, 1*time.Second))
	require.NoError(t, waitForAbsence(ctx, k8sClient,
		schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"},
		types.NamespacedName{Name: "cycle-b", Namespace: ns}, 500*time.Millisecond))
	t.Log("No resources created for cyclic Graph")
}

// ---------------------------------------------------------------------------
// Watch absent / Contribute target absent → DataPending
// ---------------------------------------------------------------------------

// TestWatchAbsentResourceIsDataPending proves that a Watch node targeting a
// non-existent resource enters DataPending state and blocks dependents via
// contagious exclusion. When the resource appears, the chain resolves.
func TestWatchAbsentResourceIsDataPending(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Graph: watch a non-existent ConfigMap, then create a dependent resource.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-watch-absent",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "watched",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "absent-target",
							},
						},
					},
					map[string]any{
						"id": "dependent",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "watch-dependent",
							},
							"data": map[string]any{
								"value": "${watched.data.key}",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Graph should show DataPending status
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		g := &unstructured.Unstructured{}
		g.SetGroupVersionKind(GraphGVK)
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "test-watch-absent", Namespace: ns}, g); err != nil {
			return false, nil
		}
		status, _ := g.Object["status"].(map[string]any)
		if status == nil {
			return false, nil
		}
		conditions, _ := status["conditions"].([]any)
		ready, found := findCondition(conditions, "Ready")
		if !found {
			return false, nil
		}
		// Should be False with DataPending reason
		return ready["status"] == "False" && ready["reason"] == "DataPending", nil
	}))
	t.Log("Graph correctly shows DataPending when watch target is absent")

	// Dependent should NOT be created
	require.NoError(t, waitForAbsence(ctx, k8sClient,
		schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"},
		types.NamespacedName{Name: "watch-dependent", Namespace: ns}, 1500*time.Millisecond))
	t.Log("Dependent correctly not created while watch target is absent")

	// Now create the missing resource
	target := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "absent-target",
				"namespace": ns,
			},
			"data": map[string]any{
				"key": "resolved-value",
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, target))
	t.Log("Created absent-target ConfigMap")

	// Dependent should now be created with the resolved value
	cmGVK := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		dep := &unstructured.Unstructured{}
		dep.SetGroupVersionKind(cmGVK)
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "watch-dependent", Namespace: ns}, dep); err != nil {
			return false, nil
		}
		d, _, _ := unstructured.NestedStringMap(dep.Object, "data")
		return d["value"] == "resolved-value", nil
	}))

	// Verify final state
	dep := &unstructured.Unstructured{}
	dep.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "watch-dependent", Namespace: ns}, dep))
	data, _, _ := unstructured.NestedStringMap(dep.Object, "data")
	assert.Equal(t, "resolved-value", data["value"])
	t.Log("Watch absent → DataPending → resource appears → chain resolves")
}

// TestContributeTargetAbsentIsDataPending proves that a Contribute node
// targeting a non-existent resource enters DataPending state. When the target
// is created externally, the contribution is applied.
func TestContributeTargetAbsentIsDataPending(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Graph: contribute annotations to a non-existent ConfigMap.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-contribute-absent",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					// Contribute shape: apiVersion + kind + metadata only
					map[string]any{
						"id": "contrib",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "contrib-target",
								"annotations": map[string]any{
									"contributed-by": "graph-controller",
								},
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Graph should show DataPending (contribute target doesn't exist)
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		g := &unstructured.Unstructured{}
		g.SetGroupVersionKind(GraphGVK)
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "test-contribute-absent", Namespace: ns}, g); err != nil {
			return false, nil
		}
		status, _ := g.Object["status"].(map[string]any)
		if status == nil {
			return false, nil
		}
		// State should be InProgress while DataPending
		state, _ := status["state"].(string)
		return state == "InProgress", nil
	}))
	t.Log("Graph shows InProgress when contribute target is absent")

	// Now create the target externally
	target := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "contrib-target",
				"namespace": ns,
			},
			"data": map[string]any{
				"existing": "data",
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, target))
	t.Log("Created contrib-target externally")

	// Graph should reach Active — contribution applied
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		g := &unstructured.Unstructured{}
		g.SetGroupVersionKind(GraphGVK)
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "test-contribute-absent", Namespace: ns}, g); err != nil {
			return false, nil
		}
		status, _ := g.Object["status"].(map[string]any)
		if status == nil {
			return false, nil
		}
		state, _ := status["state"].(string)
		return state == "Active", nil
	}))

	// Verify the contribution was applied (annotations added)
	result := &unstructured.Unstructured{}
	result.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "contrib-target", Namespace: ns}, result))
	anns := result.GetAnnotations()
	assert.Equal(t, "graph-controller", anns["contributed-by"],
		"contribution should have been applied to the target")
	t.Log("Contribute target absent → DataPending → target created → contribution applied")
}

// ---------------------------------------------------------------------------
// Multiple includeWhen conditions — AND semantics
// ---------------------------------------------------------------------------

// TestMultipleIncludeWhenConditionsAreANDed proves that multiple includeWhen
// conditions are AND-ed: all must be true for the node to be included.
func TestMultipleIncludeWhenConditionsAreANDed(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Source with two flags
	source := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "include-source",
				"namespace": ns,
			},
			"data": map[string]any{
				"flagA": "true",
				"flagB": "false",
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, source))

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-include-and",
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
								"name": "include-source",
							},
						},
					},
					// This node has TWO includeWhen conditions — both must be true.
					map[string]any{
						"id": "gated",
						"includeWhen": []any{
							"${source.data.flagA == 'true'}",
							"${source.data.flagB == 'true'}",
						},
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "gated-output",
							},
							"data": map[string]any{
								"created": "yes",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// gated-output should NOT be created: flagA=true but flagB=false
	require.NoError(t, waitForAbsence(ctx, k8sClient,
		schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"},
		types.NamespacedName{Name: "gated-output", Namespace: ns}, 2*time.Second))
	t.Log("gated-output correctly excluded when one includeWhen condition is false")

	// Set both flags to true
	latest := &unstructured.Unstructured{}
	latest.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "include-source", Namespace: ns}, latest))
	unstructured.SetNestedField(latest.Object, "true", "data", "flagB")
	require.NoError(t, k8sClient.Update(ctx, latest))
	t.Log("Set flagB=true")

	// Now gated-output should be created
	gated := &unstructured.Unstructured{}
	gated.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
	require.NoError(t, waitForResource(ctx, k8sClient,
		types.NamespacedName{Name: "gated-output", Namespace: ns}, gated))
	t.Log("gated-output created when both includeWhen conditions are true — AND semantics confirmed")
}

// ---------------------------------------------------------------------------
// kro label check — cross-Graph conflict detection
// ---------------------------------------------------------------------------

// TestKroLabelCheckRejectsOwnedByOtherGraph proves that when a resource has a
// graph-name label from a different Graph, the controller surfaces a
// FieldConflict error instead of silently taking ownership.
func TestKroLabelCheckRejectsOwnedByOtherGraph(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Pre-create a resource labeled as owned by a different Graph
	preexisting := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "contested-resource",
				"namespace": ns,
				"labels": map[string]any{
					"internal.kro.run/graph-name":      "other-graph",
					"internal.kro.run/graph-namespace": ns,
				},
			},
			"data": map[string]any{
				"owner": "other-graph",
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, preexisting))

	// Create a Graph that tries to own the same resource
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-label-check",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "contested",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "contested-resource",
							},
							"data": map[string]any{
								"owner": "test-label-check",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Graph should show FieldConflict
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		g := &unstructured.Unstructured{}
		g.SetGroupVersionKind(GraphGVK)
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "test-label-check", Namespace: ns}, g); err != nil {
			return false, nil
		}
		status, _ := g.Object["status"].(map[string]any)
		if status == nil {
			return false, nil
		}
		conditions, _ := status["conditions"].([]any)
		ready, found := findCondition(conditions, "Ready")
		if !found {
			return false, nil
		}
		return ready["reason"] == "FieldConflict", nil
	}))

	// Verify the original resource is unchanged
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "contested-resource", Namespace: ns}, existing))
	data, _, _ := unstructured.NestedStringMap(existing.Object, "data")
	assert.Equal(t, "other-graph", data["owner"],
		"resource should retain original owner's data")
	labels := existing.GetLabels()
	assert.Equal(t, "other-graph", labels["internal.kro.run/graph-name"],
		"resource should retain original graph-name label")
	t.Log("Cross-Graph ownership conflict correctly detected and blocked")
}

// ---------------------------------------------------------------------------
// Invalid spec structural errors
// ---------------------------------------------------------------------------

// TestInvalidSpecMissingNodeID proves that a Graph with a node missing its
// id field is rejected with Accepted=False, InvalidSpec reason.
func TestInvalidSpecMissingNodeID(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-missing-id",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						// No "id" field
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "orphan",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Should be rejected with InvalidSpec
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		g := &unstructured.Unstructured{}
		g.SetGroupVersionKind(GraphGVK)
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "test-missing-id", Namespace: ns}, g); err != nil {
			return false, nil
		}
		status, _ := g.Object["status"].(map[string]any)
		if status == nil {
			return false, nil
		}
		conditions, _ := status["conditions"].([]any)
		cond, found := findCondition(conditions, "Accepted")
		if !found {
			return false, nil
		}
		return cond["status"] == "False" && cond["reason"] == "InvalidSpec", nil
	}))

	g := &unstructured.Unstructured{}
	g.SetGroupVersionKind(GraphGVK)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "test-missing-id", Namespace: ns}, g))
	status, _ := g.Object["status"].(map[string]any)
	assert.Equal(t, "Error", status["state"])
	t.Log("Missing node ID correctly rejected with InvalidSpec")
}

// TestInvalidSpecDuplicateNodeID proves that a Graph with duplicate node IDs
// is rejected with Accepted=False, InvalidSpec reason.
func TestInvalidSpecDuplicateNodeID(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-dup-id",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "mynode",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "dup-a",
							},
						},
					},
					map[string]any{
						"id": "mynode", // duplicate
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "dup-b",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		g := &unstructured.Unstructured{}
		g.SetGroupVersionKind(GraphGVK)
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "test-dup-id", Namespace: ns}, g); err != nil {
			return false, nil
		}
		status, _ := g.Object["status"].(map[string]any)
		if status == nil {
			return false, nil
		}
		conditions, _ := status["conditions"].([]any)
		cond, found := findCondition(conditions, "Accepted")
		if !found {
			return false, nil
		}
		return cond["status"] == "False" && cond["reason"] == "InvalidSpec", nil
	}))

	g := &unstructured.Unstructured{}
	g.SetGroupVersionKind(GraphGVK)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "test-dup-id", Namespace: ns}, g))
	status, _ := g.Object["status"].(map[string]any)
	assert.Equal(t, "Error", status["state"])
	t.Log("Duplicate node ID correctly rejected with InvalidSpec")
}

// TestInvalidSpecCompilationFailure proves that a Graph with an invalid CEL
// expression is rejected with Accepted=False, CompilationFailed reason.
func TestInvalidSpecCompilationFailure(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-bad-cel",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "bad",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "bad-cel",
							},
							"data": map[string]any{
								// Invalid CEL: unbalanced parens
								"value": "${broken_func(((}",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		g := &unstructured.Unstructured{}
		g.SetGroupVersionKind(GraphGVK)
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "test-bad-cel", Namespace: ns}, g); err != nil {
			return false, nil
		}
		status, _ := g.Object["status"].(map[string]any)
		if status == nil {
			return false, nil
		}
		conditions, _ := status["conditions"].([]any)
		cond, found := findCondition(conditions, "Accepted")
		if !found {
			return false, nil
		}
		return cond["status"] == "False" && cond["reason"] == "CompilationFailed", nil
	}))

	g := &unstructured.Unstructured{}
	g.SetGroupVersionKind(GraphGVK)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "test-bad-cel", Namespace: ns}, g))
	status, _ := g.Object["status"].(map[string]any)
	assert.Equal(t, "Error", status["state"])

	// No revision should be created
	count, err := countRevisions(ctx, k8sClient, "test-bad-cel", ns)
	require.NoError(t, err)
	assert.Equal(t, 0, count)
	t.Log("Invalid CEL correctly rejected with CompilationFailed")
}

// ---------------------------------------------------------------------------
// Applied set stored on revision annotation
// ---------------------------------------------------------------------------

// TestAppliedSetStoredOnRevisionAnnotation proves that the applied set is
// persisted as an annotation on the GraphRevision, not on the Graph. This
// verifies the design commitment from 004-graph-execution § Applied Set.
func TestAppliedSetStoredOnRevisionAnnotation(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-applied-set",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "alpha",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "applied-alpha",
							},
							"data": map[string]any{
								"key": "value",
							},
						},
					},
					map[string]any{
						"id": "beta",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "applied-beta",
							},
							"data": map[string]any{
								"key": "value",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for both resources to be created
	cmGVK := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}
	for _, name := range []string{"applied-alpha", "applied-beta"} {
		cm := &unstructured.Unstructured{}
		cm.SetGroupVersionKind(cmGVK)
		require.NoError(t, waitForResource(ctx, k8sClient,
			types.NamespacedName{Name: name, Namespace: ns}, cm))
	}

	// Wait for the Graph to reach Active
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		g := &unstructured.Unstructured{}
		g.SetGroupVersionKind(GraphGVK)
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "test-applied-set", Namespace: ns}, g); err != nil {
			return false, nil
		}
		status, _ := g.Object["status"].(map[string]any)
		if status == nil {
			return false, nil
		}
		return status["state"] == "Active", nil
	}))

	// Get the revision and check its applied set annotation
	revName := "test-applied-set-g00001"
	rev, err := waitForRevision(ctx, k8sClient, types.NamespacedName{Name: revName, Namespace: ns})
	require.NoError(t, err)

	anns := rev.GetAnnotations()
	rawAppliedSet, ok := anns[graphcontroller.AnnotationAppliedSet]
	require.True(t, ok, "revision should have applied set annotation")

	var appliedSet []string
	require.NoError(t, json.Unmarshal([]byte(rawAppliedSet), &appliedSet),
		"applied set should be valid JSON")
	assert.Len(t, appliedSet, 2, "applied set should contain 2 keys")

	// Verify the keys contain both ConfigMaps
	keySet := map[string]bool{}
	for _, k := range appliedSet {
		keySet[k] = true
	}
	assert.True(t, keySet["/v1/ConfigMap/"+ns+"/applied-alpha"],
		"applied set should contain alpha key")
	assert.True(t, keySet["/v1/ConfigMap/"+ns+"/applied-beta"],
		"applied set should contain beta key")

	// Verify the Graph itself has NO applied set annotation
	g := &unstructured.Unstructured{}
	g.SetGroupVersionKind(GraphGVK)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "test-applied-set", Namespace: ns}, g))
	graphAnns := g.GetAnnotations()
	_, hasAppliedSet := graphAnns[graphcontroller.AnnotationAppliedSet]
	assert.False(t, hasAppliedSet,
		"Graph should NOT have applied set annotation — it lives on the revision")
	t.Logf("Applied set correctly stored on revision: %v", appliedSet)
}
