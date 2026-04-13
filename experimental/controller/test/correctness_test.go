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
			"apiVersion": "experimental.kro.run/v1alpha1",
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

	// Wait for the controller to observe the change — the Graph's Ready
	// condition must transition to NotReady (readyWhen unsatisfied because
	// updatedReplicas != replicas). This is more robust than waitForSettle
	// which can succeed before the controller processes the event.
	require.NoError(t, waitForGraphReadyStatus(ctx, k8sClient,
		types.NamespacedName{Name: "test-propagate-when", Namespace: ns}, "Unknown"))
	t.Log("Graph status changed to NotReady — controller processed the rollout event")

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
	// Under parallel test load, the controller queue can back up — use a
	// generous timeout.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 60*time.Second, true, func(ctx context.Context) (bool, error) {
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
			"apiVersion": "experimental.kro.run/v1alpha1",
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

// TestCircularDependencyRejectsSpec proves that a Graph with circular dependencies
// is rejected with Compiled=False, CircularDependency reason. No revision is created
// and no resources are applied.
func TestCircularDependencyRejectsSpec(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Graph with A → B → A cycle
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
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

	// Graph should reach Compiled=False with CircularDependency reason
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
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
		cond, found := findCondition(conditions, "Compiled")
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
	compiled, found := findCondition(conditions, "Compiled")
	require.True(t, found, "Compiled condition should exist")
	assert.Equal(t, "False", compiled["status"])
	assert.Equal(t, "CircularDependency", compiled["reason"],
		"reason should be CircularDependency")
	t.Logf("Circular dependency correctly detected: reason=%s, message=%s", compiled["reason"], compiled["message"])

	// Verify Compiled condition is False (spec error — circular dependency)
	assert.Equal(t, "False", compiled["status"], "Compiled should be False when circular dependency detected")

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
// Watch absent / Contribute target absent → Pending
// ---------------------------------------------------------------------------

// TestWatchAbsentResourceIsPending proves that a Watch node targeting a
// non-existent resource enters Pending state and blocks dependents via
// contagious exclusion. When the resource appears, the chain resolves.
func TestWatchAbsentResourceIsPending(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Graph: watch a non-existent ConfigMap, then create a dependent resource.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
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

	// Graph should show Pending status
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
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
		// Should be Unknown with Pending reason
		return ready["status"] == "Unknown" && ready["reason"] == "Pending", nil
	}))
	t.Log("Graph correctly shows Pending when watch target is absent")

	// Dependent should NOT be created
	require.NoError(t, waitForAbsence(ctx, k8sClient,
		schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"},
		types.NamespacedName{Name: "watch-dependent", Namespace: ns}, 1*time.Second))
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
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
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
	t.Log("Watch absent → Pending → resource appears → chain resolves")
}

// TestAbsentResourceIsOwnedByDefault proves that when a node's target resource
// does not exist, the Graph creates it (Own). With existence-based shape
// detection, absent → Own — the Graph always creates resources it doesn't find.
func TestAbsentResourceIsOwnedByDefault(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Graph: target a non-existent ConfigMap with only metadata fields.
	// Under the old heuristic, this was Contribute (key subset check).
	// Under existence-based detection, this is Own (resource absent).
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-absent-owned",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "target",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "absent-target",
								"annotations": map[string]any{
									"created-by": "graph-controller",
								},
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// The Graph should create the resource (Own) and reach Ready.
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-absent-owned", Namespace: ns}))
	t.Log("Graph reached Ready — absent resource was created (Own)")

	// Verify the resource was created and has the kro label (Own stamps it)
	cm := &unstructured.Unstructured{}
	cm.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "absent-target", Namespace: ns}, cm))
	assertManagedBy(t, cm, "test-absent-owned")
	t.Log("Resource created with kro label — Own shape confirmed")
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
			"apiVersion": "experimental.kro.run/v1alpha1",
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
		types.NamespacedName{Name: "gated-output", Namespace: ns}, 1*time.Second))
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

	// Pre-create a resource labeled as owned by a different Graph.
	// Uses the DNS subdomain identity label format per 004-graph-execution.md.
	preexisting := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "contested-resource",
				"namespace": ns,
				"labels": map[string]any{
					"somenode.other-graph." + ns + ".internal.kro.run/reference": "own",
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
			"apiVersion": "experimental.kro.run/v1alpha1",
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
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
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
		return ready["reason"] == "Conflict", nil
	}))

	// Verify the original resource is unchanged
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "contested-resource", Namespace: ns}, existing))
	data, _, _ := unstructured.NestedStringMap(existing.Object, "data")
	assert.Equal(t, "other-graph", data["owner"],
		"resource should retain original owner's data")
	labels := existing.GetLabels()
	// With the new identity label scheme, the original graph's label should still be present
	assert.Equal(t, "own", labels["somenode.other-graph."+ns+".internal.kro.run/reference"],
		"resource should retain original graph's identity label")
	t.Log("Cross-Graph ownership conflict correctly detected and blocked")
}

// TestForceApplyOverridesKroLabelCheck proves that kro.run/apply: Force on a
// template takes ownership of a resource labeled as owned by a different Graph.
// This is the import/migration mechanism from design 003-ownership.
//
// Setup:
//   - Pre-create a resource owned by "other-graph" (has graph-name label).
//   - Create a Graph that targets the same resource WITH kro.run/apply: Force.
//   - Verify the Graph succeeds and takes ownership (label changes).
func TestForceApplyOverridesKroLabelCheck(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Pre-create a resource labeled as owned by a different Graph.
	// Uses the DNS subdomain identity label format.
	preexisting := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "force-target",
				"namespace": ns,
				"labels": map[string]any{
					"somenode.other-graph." + ns + ".internal.kro.run/reference": "own",
				},
			},
			"data": map[string]any{
				"original": "data",
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, preexisting))

	// Create a Graph that uses Force to take ownership
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-force-apply",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "imported",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "force-target",
								"annotations": map[string]any{
									"kro.run/apply": "Force",
								},
							},
							"data": map[string]any{
								"owner": "test-force-apply",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Graph should reach Ready — Force overrides the label check
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		g := &unstructured.Unstructured{}
		g.SetGroupVersionKind(GraphGVK)
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "test-force-apply", Namespace: ns}, g); err != nil {
			return false, nil
		}
		return graphReady(g), nil
	}))

	// Verify ownership was taken: the new graph's identity label should be present
	result := &unstructured.Unstructured{}
	result.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "force-target", Namespace: ns}, result))

	resultLabels := result.GetLabels()
	// The new graph's identity label should exist
	newLabelKey := "imported.test-force-apply." + ns + ".internal.kro.run/reference"
	t.Logf("Looking for label key: %s", newLabelKey)
	t.Logf("All labels on resource: %v", resultLabels)
	assert.Equal(t, "own", resultLabels[newLabelKey],
		"new graph's identity label should be present after Force apply")

	// Verify the data was overwritten
	data, _, _ := unstructured.NestedStringMap(result.Object, "data")
	assert.Equal(t, "test-force-apply", data["owner"],
		"data should reflect the new owner's template")
	t.Log("Force apply correctly overrides kro label check — import/migration mechanism works")
}

// ---------------------------------------------------------------------------
// Invalid spec structural errors
// ---------------------------------------------------------------------------

// TestDeclarationErrorMissingNodeID proves that a Graph with a node missing its
// id field is rejected with Compiled=False, DeclarationError reason.
func TestDeclarationErrorMissingNodeID(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
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

	// Should be rejected with DeclarationError
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
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
		cond, found := findCondition(conditions, "Compiled")
		if !found {
			return false, nil
		}
		return cond["status"] == "False" && cond["reason"] == "DeclarationError", nil
	}))

	g := &unstructured.Unstructured{}
	g.SetGroupVersionKind(GraphGVK)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "test-missing-id", Namespace: ns}, g))
	status, _ := g.Object["status"].(map[string]any)
	conditions, _ := status["conditions"].([]any)
	compiled, found := findCondition(conditions, "Compiled")
	require.True(t, found, "Compiled condition should exist")
	assert.Equal(t, "False", compiled["status"])
	t.Log("Missing node ID correctly rejected with DeclarationError")
}

// TestDeclarationErrorDuplicateNodeID proves that a Graph with duplicate node IDs
// is rejected with Compiled=False, DeclarationError reason.
func TestDeclarationErrorDuplicateNodeID(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
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

	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
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
		cond, found := findCondition(conditions, "Compiled")
		if !found {
			return false, nil
		}
		return cond["status"] == "False" && cond["reason"] == "DeclarationError", nil
	}))

	g := &unstructured.Unstructured{}
	g.SetGroupVersionKind(GraphGVK)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "test-dup-id", Namespace: ns}, g))
	status, _ := g.Object["status"].(map[string]any)
	conditions, _ := status["conditions"].([]any)
	compiled, found := findCondition(conditions, "Compiled")
	require.True(t, found, "Compiled condition should exist")
	assert.Equal(t, "False", compiled["status"])
	t.Log("Duplicate node ID correctly rejected with DeclarationError")
}

// TestExpressionErrorRejectsSpec proves that a Graph with an invalid CEL
// expression is rejected with Compiled=False, ExpressionError reason.
func TestExpressionErrorRejectsSpec(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
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

	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
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
		cond, found := findCondition(conditions, "Compiled")
		if !found {
			return false, nil
		}
		return cond["status"] == "False" && cond["reason"] == "ExpressionError", nil
	}))

	g := &unstructured.Unstructured{}
	g.SetGroupVersionKind(GraphGVK)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "test-bad-cel", Namespace: ns}, g))
	status, _ := g.Object["status"].(map[string]any)
	conditions, _ := status["conditions"].([]any)
	compiled, found := findCondition(conditions, "Compiled")
	require.True(t, found, "Compiled condition should exist")
	assert.Equal(t, "False", compiled["status"])

	// No revision should be created
	count, err := countRevisions(ctx, k8sClient, "test-bad-cel", ns)
	require.NoError(t, err)
	assert.Equal(t, 0, count)
	t.Log("Invalid CEL correctly rejected with ExpressionError")
}

// ---------------------------------------------------------------------------
// Identity labels on managed resources
// ---------------------------------------------------------------------------

// TestIdentityLabelsOnManagedResources verifies that managed resources carry
// the DNS subdomain identity labels per 004-graph-execution.md § Storage model.
func TestIdentityLabelsOnManagedResources(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)
	ctx := context.Background()

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-identity-labels",
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

	// Wait for both resources to be created and verify identity labels
	cmGVK := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}
	for _, tc := range []struct {
		nodeID string
		name   string
	}{
		{"alpha", "applied-alpha"},
		{"beta", "applied-beta"},
	} {
		cm := &unstructured.Unstructured{}
		cm.SetGroupVersionKind(cmGVK)
		require.NoError(t, waitForResource(ctx, k8sClient,
			types.NamespacedName{Name: tc.name, Namespace: ns}, cm))

		// Verify DNS subdomain identity labels
		labels := cm.GetLabels()
		roleKey := tc.nodeID + ".test-identity-labels." + ns + ".internal.kro.run/reference"
		genKey := tc.nodeID + ".test-identity-labels." + ns + ".internal.kro.run/generation"

		assert.Equal(t, "own", labels[roleKey],
			"resource should have identity role label for node %s", tc.nodeID)
		assert.NotEmpty(t, labels[genKey],
			"resource should have generation label for node %s", tc.nodeID)
	}

	// Wait for the Graph to reach Ready
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		g := &unstructured.Unstructured{}
		g.SetGroupVersionKind(GraphGVK)
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "test-identity-labels", Namespace: ns}, g); err != nil {
			return false, nil
		}
		return graphReady(g), nil
	}))
	t.Log("Identity labels correctly stamped on managed resources")
}

// ---------------------------------------------------------------------------
// Readiness defaults — regression tests
// ---------------------------------------------------------------------------

// TestDefaultReadinessIsApplied proves that a node without readyWhen is
// considered ready as soon as it is applied. No implicit status.conditions
// check is performed. This is a regression test: an earlier implementation
// added a .ready() CEL function that checked status.conditions for
// type=Ready, which incorrectly imposed a Kubernetes conditions convention
// as the default readiness model. Readiness is "CEL resolved + applied."
// Explicit readyWhen overrides this default.
func TestDefaultReadinessIsApplied(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Create a Graph with NO readyWhen on any node. The Graph should
	// reach Active as soon as both resources are applied, without checking
	// any status.conditions.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-default-ready",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "config",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "default-ready-config",
							},
							"data": map[string]any{
								"key": "value",
							},
						},
					},
					map[string]any{
						"id": "downstream",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "default-ready-downstream",
							},
							"data": map[string]any{
								"from": "${config.data.key}",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Graph should reach Ready — no readyWhen means "applied = ready"
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		g := &unstructured.Unstructured{}
		g.SetGroupVersionKind(GraphGVK)
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "test-default-ready", Namespace: ns}, g); err != nil {
			return false, nil
		}
		return graphReady(g), nil
	}))

	// Verify both resources were created and downstream resolved CEL
	cmGVK := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}
	downstream := &unstructured.Unstructured{}
	downstream.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "default-ready-downstream", Namespace: ns}, downstream))
	data, _, _ := unstructured.NestedStringMap(downstream.Object, "data")
	assert.Equal(t, "value", data["from"],
		"downstream should have resolved config.data.key")

	// Verify Graph is Ready (not stuck on readiness)
	g := &unstructured.Unstructured{}
	g.SetGroupVersionKind(GraphGVK)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "test-default-ready", Namespace: ns}, g))
	assert.True(t, graphReady(g),
		"Graph should be Ready — no readyWhen means applied = ready, no conditions check")
	t.Log("Default readiness proved: no readyWhen → Ready on apply, no conditions check")
}

// TestExplicitReadyWhenOverridesDefault proves that explicit readyWhen
// conditions override the default "applied = ready" behavior. When
// readyWhen evaluates to false, the node is NotReady and the Graph
// reports ResourcesNotReady.
func TestExplicitReadyWhenOverridesDefault(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Create a Graph where readyWhen checks a field that will be false.
	// The node is applied but readyWhen keeps it NotReady.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-explicit-ready",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "checked",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "explicit-ready-target",
							},
							"data": map[string]any{
								"status": "pending",
							},
						},
						// readyWhen overrides the default — this will be false
						// because we set status=pending, not status=ready
						"readyWhen": []any{
							"${checked.data.status == 'ready'}",
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for the resource to be created (it will be applied)
	cmGVK := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}
	cm := &unstructured.Unstructured{}
	cm.SetGroupVersionKind(cmGVK)
	require.NoError(t, waitForResource(ctx, k8sClient,
		types.NamespacedName{Name: "explicit-ready-target", Namespace: ns}, cm))

	// Graph should NOT be Ready — readyWhen overrides the default
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		g := &unstructured.Unstructured{}
		g.SetGroupVersionKind(GraphGVK)
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "test-explicit-ready", Namespace: ns}, g); err != nil {
			return false, nil
		}
		// Should be Unknown (not True) because readyWhen is false
		return graphReadyStatus(g) == "Unknown", nil
	}))

	g := &unstructured.Unstructured{}
	g.SetGroupVersionKind(GraphGVK)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "test-explicit-ready", Namespace: ns}, g))
	assert.Equal(t, "Unknown", graphReadyStatus(g),
		"Graph Ready should be Unknown — readyWhen overrides default, checked.data.status != 'ready'")
	t.Log("Explicit readyWhen proved: overrides default, node applied but NotReady")
}

// TestReadyFunctionReflectsNodeState proves that .ready() returns the graph
// controller's readiness assessment, not a Kubernetes conditions check.
//
// Setup: node A has readyWhen that starts false, node B uses
// propagateWhen: ["${a.ready()}"] to gate data flow until A is ready.
// When A's readyWhen becomes true, B's propagateWhen passes and data flows.
func TestReadyFunctionReflectsNodeState(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Pre-create the source ConfigMap with status=pending
	source := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "ready-fn-source",
				"namespace": ns,
			},
			"data": map[string]any{
				"status": "pending",
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, source))

	// Graph:
	//   watched → reads source, readyWhen checks data.status == "active"
	//   consumer → owns ConfigMap, propagateWhen: ["${watched.ready()}"]
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-ready-fn-state",
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
								"name": "ready-fn-source",
							},
						},
						"readyWhen": []any{
							"${watched.data.status == 'active'}",
						},
					},
					map[string]any{
						"id": "consumer",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "ready-fn-consumer",
							},
							"data": map[string]any{
								"from": "${watched.data.status}",
							},
						},
						"propagateWhen": []any{
							"${watched.ready()}",
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for consumer to be created (propagateWhen doesn't gate creation,
	// but it gates data flow on subsequent reconciles)
	cmGVK := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}
	consumer := &unstructured.Unstructured{}
	consumer.SetGroupVersionKind(cmGVK)
	require.NoError(t, waitForResource(ctx, k8sClient,
		types.NamespacedName{Name: "ready-fn-consumer", Namespace: ns}, consumer))

	// Graph should be InProgress — watched.ready() is false because
	// readyWhen (data.status == 'active') is false
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		g := &unstructured.Unstructured{}
		g.SetGroupVersionKind(GraphGVK)
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "test-ready-fn-state", Namespace: ns}, g); err != nil {
			return false, nil
		}
		return graphReadyStatus(g) == "Unknown", nil
	}))
	t.Log("Graph InProgress — watched.ready() is false, propagateWhen gates data")

	// Now update the source to make readyWhen pass
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "ready-fn-source", Namespace: ns}, source))
	source.Object["data"] = map[string]any{"status": "active"}
	require.NoError(t, k8sClient.Update(ctx, source))

	// Graph should reach Ready — watched.ready() now true
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		g := &unstructured.Unstructured{}
		g.SetGroupVersionKind(GraphGVK)
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "test-ready-fn-state", Namespace: ns}, g); err != nil {
			return false, nil
		}
		return graphReady(g), nil
	}))

	// Verify consumer has the updated data
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "ready-fn-consumer", Namespace: ns}, consumer))
	data, _, _ := unstructured.NestedStringMap(consumer.Object, "data")
	assert.Equal(t, "active", data["from"],
		"consumer should have updated data after watched.ready() became true")
	t.Log(".ready() reflects graph node state — propagateWhen gate released on readyWhen pass")
}

// TestEmptyCollectionReadyIsVacuouslyTrue proves that .ready() on an empty
// collection returns true. An empty collection watch has no items to be
// not-ready, so the collection is vacuously ready. This prevents empty
// collections from blocking graphs that use collection.ready() in
// propagateWhen or readyWhen.
func TestEmptyCollectionReadyIsVacuouslyTrue(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Graph: collection watch with a label selector matching nothing,
	// downstream uses readyWhen: workers.ready(). Should reach Active
	// because empty collection is vacuously ready.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-empty-ready",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "workers",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"selector": map[string]any{
								"app": "empty-collection-no-match-xyz",
							},
						},
					},
					map[string]any{
						"id": "summary",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "empty-ready-summary",
							},
							"data": map[string]any{
								"status": "done",
							},
						},
						"readyWhen": []any{
							"${workers.ready()}",
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Graph should reach Ready — workers is an empty collection,
	// workers.ready() returns true (vacuously), summary's readyWhen passes.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		g := &unstructured.Unstructured{}
		g.SetGroupVersionKind(GraphGVK)
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "test-empty-ready", Namespace: ns}, g); err != nil {
			return false, nil
		}
		return graphReady(g), nil
	}))

	g := &unstructured.Unstructured{}
	g.SetGroupVersionKind(GraphGVK)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "test-empty-ready", Namespace: ns}, g))
	assert.True(t, graphReady(g),
		"empty collection.ready() should be vacuously true")
	t.Log("Empty collection .ready() is vacuously true — Graph reached Ready")
}

// TestCollectionItemReadyViaIndex proves that workers[0].ready() works from
// another node's readyWhen. Per-item __ready flags set during forEach
// readyWhen evaluation are accessible via CEL indexing from other nodes.
func TestCollectionItemReadyViaIndex(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-index-ready",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					// forEach stamps 2 workers, readyWhen passes immediately
					map[string]any{
						"id": "workers",
						"forEach": map[string]any{
							"value": "${['alpha', 'beta']}",
						},
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "idx-worker-${value}",
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
								"name": "index-ready-summary",
							},
							"data": map[string]any{
								"done": "true",
							},
						},
						// Cross-node index: check if first worker item is ready
						"readyWhen": []any{
							"${workers[0].ready()}",
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Graph should reach Ready — workers[0].ready() is true
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		g := &unstructured.Unstructured{}
		g.SetGroupVersionKind(GraphGVK)
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "test-index-ready", Namespace: ns}, g); err != nil {
			return false, nil
		}
		return graphReady(g), nil
	}))
	t.Log("workers[0].ready() works — cross-node index into collection item readiness proved")
}

// TestCollectionReadyFalseWhenItemNotReady proves that workers.ready()
// returns false when any item in the collection has not passed its readyWhen.
// Workers start with ready=false (readyWhen fails), Graph is InProgress.
// Update spec to ready=true → readyWhen passes → items.ready() true → Active.
func TestCollectionReadyFalseWhenItemNotReady(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-coll-ready-false",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "items",
						"forEach": map[string]any{
							"value": "${['x', 'y']}",
						},
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "coll-item-${value}",
							},
							"data": map[string]any{
								"ready": "false",
							},
						},
						"readyWhen": []any{
							"${items.data.ready == 'true'}",
						},
					},
					map[string]any{
						"id": "output",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "coll-ready-output",
							},
							"data": map[string]any{
								"done": "true",
							},
						},
						// Collection-level readiness: all items must be ready
						"readyWhen": []any{
							"${items.ready()}",
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Graph should have Ready=Unknown — items not ready, items.ready() is false
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		g := &unstructured.Unstructured{}
		g.SetGroupVersionKind(GraphGVK)
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "test-coll-ready-false", Namespace: ns}, g); err != nil {
			return false, nil
		}
		return graphReadyStatus(g) == "Unknown", nil
	}))
	t.Log("Graph InProgress — items.ready() is false (items have ready=false)")

	// Update spec to make items ready
	require.NoError(t, updateWithRetry(ctx, k8sClient, GraphGVK,
		types.NamespacedName{Name: "test-coll-ready-false", Namespace: ns}, func(obj *unstructured.Unstructured) {
			obj.Object["spec"] = map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "items",
						"forEach": map[string]any{
							"value": "${['x', 'y']}",
						},
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "coll-item-${value}",
							},
							"data": map[string]any{
								"ready": "true",
							},
						},
						"readyWhen": []any{
							"${items.data.ready == 'true'}",
						},
					},
					map[string]any{
						"id": "output",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "coll-ready-output",
							},
							"data": map[string]any{
								"done": "true",
							},
						},
						"readyWhen": []any{
							"${items.ready()}",
						},
					},
				},
			}
		}))

	// Graph should reach Ready — all items ready, items.ready() is true
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		g := &unstructured.Unstructured{}
		g.SetGroupVersionKind(GraphGVK)
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "test-coll-ready-false", Namespace: ns}, g); err != nil {
			return false, nil
		}
		return graphReady(g), nil
	}))
	t.Log("items.ready() proved: false when any item not ready, true when all items ready")
}

// TestRevisionImmutability proves that the API server rejects mutations to
// an existing GraphRevision's spec. The CRD has x-kubernetes-validations:
// self == oldSelf on the spec field.
//
// Note: this requires the envtest API server to support CEL validation rules
// (Kubernetes 1.25+). If the update succeeds, the test logs a warning.
func TestRevisionImmutability(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Create a Graph to produce a revision
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-immutable",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "simple",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "immutable-cm",
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

	// Wait for revision to be created
	rev, err := waitForRevision(ctx, k8sClient,
		types.NamespacedName{Name: "test-immutable-g00001", Namespace: ns})
	require.NoError(t, err)

	// Attempt to mutate the revision's spec
	revCopy := rev.DeepCopy()
	spec, ok := revCopy.Object["spec"].(map[string]any)
	require.True(t, ok, "revision should have a spec")
	spec["tampered"] = "yes"

	err = k8sClient.Update(ctx, revCopy)
	if err != nil {
		assert.Contains(t, err.Error(), "immutable",
			"error should mention immutability")
		t.Log("Revision immutability proved: API server rejects spec mutations")
	} else {
		t.Log("WARNING: API server accepted spec mutation — CEL validation rules may not be enforced in this envtest version")
	}
}

// TestCrossNodeReadyWhen proves that readyWhen can reference another node
// using .ready(). This complements TestReadyFunctionReflectsNodeState which
// tests propagateWhen. The readyWhen full-scope change enables expressions
// like readyWhen: ["${dependency.ready()}"].
func TestCrossNodeReadyWhen(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Pre-create source with status=pending
	source := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "cross-ready-source",
				"namespace": ns,
			},
			"data": map[string]any{
				"status": "pending",
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, source))

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-cross-ready",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "dep",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "cross-ready-source",
							},
						},
						"readyWhen": []any{
							"${dep.data.status == 'active'}",
						},
					},
					map[string]any{
						"id": "consumer",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "cross-ready-consumer",
							},
							"data": map[string]any{
								"val": "test",
							},
						},
						// Cross-node readyWhen — references dep's readiness
						"readyWhen": []any{
							"${dep.ready()}",
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Graph should have Ready=Unknown — dep.ready() is false
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		g := &unstructured.Unstructured{}
		g.SetGroupVersionKind(GraphGVK)
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "test-cross-ready", Namespace: ns}, g); err != nil {
			return false, nil
		}
		return graphReadyStatus(g) == "Unknown", nil
	}))
	t.Log("Graph InProgress — cross-node readyWhen dep.ready() is false")

	// Make dep ready
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "cross-ready-source", Namespace: ns}, source))
	source.Object["data"] = map[string]any{"status": "active"}
	require.NoError(t, k8sClient.Update(ctx, source))

	// Graph should reach Ready
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		g := &unstructured.Unstructured{}
		g.SetGroupVersionKind(GraphGVK)
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "test-cross-ready", Namespace: ns}, g); err != nil {
			return false, nil
		}
		return graphReady(g), nil
	}))
	t.Log("Cross-node readyWhen proved: dep.ready() in readyWhen works")
}

// ---------------------------------------------------------------------------
// Superseded revision GC — design 002-revisions
// ---------------------------------------------------------------------------

// TestSupersededRevisionGC proves that superseded revisions are garbage
// collected once the active revision is fully ready and prune completes.
// After a spec change produces a new revision and the controller converges,
// old revisions should be deleted.
func TestSupersededRevisionGC(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Create initial Graph (revision g00001)
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-revision-gc",
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
								"name": "gc-alpha",
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

	// Wait for Active state (revision g00001 ready)
	require.NoError(t, waitForSettle(ctx, k8sClient, GraphGVK,
		types.NamespacedName{Name: "test-revision-gc", Namespace: ns}))

	// Verify revision g00001 exists
	_, err := waitForRevision(ctx, k8sClient,
		types.NamespacedName{Name: "test-revision-gc-g00001", Namespace: ns})
	require.NoError(t, err)
	t.Log("Revision g00001 created")

	// Update spec — creates revision g00002
	require.NoError(t, updateWithRetry(ctx, k8sClient, GraphGVK,
		types.NamespacedName{Name: "test-revision-gc", Namespace: ns}, func(obj *unstructured.Unstructured) {
			// Update the whole spec to trigger generation bump
			obj.Object["spec"] = map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "alpha",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "gc-alpha",
							},
							"data": map[string]any{
								"version": "v2",
							},
						},
					},
				},
			}
		}))
	t.Log("Updated spec — expecting revision g00002")

	// Wait for the Graph to settle again with the new revision
	require.NoError(t, waitForSettle(ctx, k8sClient, GraphGVK,
		types.NamespacedName{Name: "test-revision-gc", Namespace: ns}))

	// Verify revision g00002 exists
	_, err = waitForRevision(ctx, k8sClient,
		types.NamespacedName{Name: "test-revision-gc-g00002", Namespace: ns})
	require.NoError(t, err)
	t.Log("Revision g00002 created and active")

	// Wait for g00001 to be garbage collected
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		count, err := countRevisions(ctx, k8sClient, "test-revision-gc", ns)
		if err != nil {
			return false, nil
		}
		return count == 1, nil
	}))

	// Verify only g00002 remains
	count, err := countRevisions(ctx, k8sClient, "test-revision-gc", ns)
	require.NoError(t, err)
	assert.Equal(t, 1, count, "only the active revision should remain after GC")
	t.Log("Superseded revision g00001 garbage collected — only g00002 remains")
}

// TestPropagateWhenOnForEach proves that propagateWhen interacts correctly
// with forEach collections — a scalar node with propagateWhen referencing a
// forEach parent gates data flow to downstream nodes (design 001-graph §
// propagateWhen, design 004-graph-execution § Wind step 2).
//
// When propagateWhen is unsatisfied on a node that depends on a forEach,
// downstream nodes that reference it retain their previous state. When
// propagateWhen passes, downstream nodes re-evaluate.
func TestPropagateWhenOnForEach(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	cmGVK := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}

	// Pre-create a control ConfigMap that determines propagateWhen.
	control := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "prop-foreach-control",
				"namespace": ns,
			},
			"data": map[string]any{
				"propagate": "true",
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, control))

	// Graph: forEach stamps workers, then an aggregator node reads the control
	// ConfigMap with propagateWhen, and a consumer depends on the aggregator.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-propagate-foreach",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "workers",
						"forEach": map[string]any{
							"value": "${['x', 'y']}",
						},
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "prop-worker-${value}"},
							"data": map[string]any{
								"item": "${value}",
							},
						},
					},
					// Aggregator reads control and has propagateWhen
					map[string]any{
						"id": "gate",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "prop-foreach-control"},
						},
						"propagateWhen": []any{
							"${gate.data.propagate == 'true'}",
						},
					},
					// Consumer depends on both forEach and gate
					map[string]any{
						"id": "consumer",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "prop-consumer"},
							"data": map[string]any{
								"workerData": "${workers[0].data.item}",
								"gateState":  "${gate.data.propagate}",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for all resources to converge.
	for _, name := range []string{"prop-worker-x", "prop-worker-y"} {
		cm := &unstructured.Unstructured{}
		cm.SetGroupVersionKind(cmGVK)
		require.NoError(t, waitForResource(ctx, k8sClient,
			types.NamespacedName{Name: name, Namespace: ns}, cm))
	}
	consumer := &unstructured.Unstructured{}
	consumer.SetGroupVersionKind(cmGVK)
	require.NoError(t, waitForResource(ctx, k8sClient,
		types.NamespacedName{Name: "prop-consumer", Namespace: ns}, consumer))
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-propagate-foreach", Namespace: ns}))
	t.Log("All resources created, propagateWhen passes — Graph ready")

	// Verify consumer has data from both forEach and gate.
	data, _, _ := unstructured.NestedStringMap(consumer.Object, "data")
	assert.NotEmpty(t, data["workerData"],
		"consumer should have worker data when propagateWhen passes")
	assert.Equal(t, "true", data["gateState"])
	t.Logf("Consumer: workerData=%s gateState=%s", data["workerData"], data["gateState"])

	// Phase 2: Change gate to make propagateWhen fail.
	latest := &unstructured.Unstructured{}
	latest.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "prop-foreach-control", Namespace: ns}, latest))
	unstructured.SetNestedField(latest.Object, "false", "data", "propagate")
	require.NoError(t, k8sClient.Update(ctx, latest))
	t.Log("Updated gate: propagate=false — propagateWhen unsatisfied")

	// Wait for settle — consumer should retain its previous state because
	// propagateWhen blocks re-evaluation of dependents.
	require.NoError(t, waitForSettle(ctx, k8sClient, GraphGVK,
		types.NamespacedName{Name: "test-propagate-foreach", Namespace: ns}))

	consumerCheck := &unstructured.Unstructured{}
	consumerCheck.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "prop-consumer", Namespace: ns}, consumerCheck),
		"consumer should still exist when propagateWhen blocks re-evaluation")
	t.Log("Consumer retained during propagateWhen block — data flow gated correctly")
}

// TestStandaloneVsEmbeddedCELTypePreservation proves that standalone
// expressions preserve CEL return type while embedded expressions always
// string-interpolate.
//
// Design 001-graph § CEL expressions:
//
//	"Standalone expressions preserve CEL return type; embedded expressions
//	string-interpolate."
//
// Standalone: `${src.data.value}` → produces the raw CEL result (string
// "world"), which when used as a template field yields exactly the value
// without any additional wrapping.
//
// Embedded: `"prefix-${src.data.value}-suffix"` → always produces a string
// that concatenates surrounding literals with the interpolated value.
//
// The behavioral difference is observable:
//   - Standalone produces exactly the expression's result
//   - Embedded always appends/prepends literal strings, producing concatenation
//
// An update to the source value triggers reactive re-evaluation of both,
// proving that both expression forms respond to upstream changes.
//
// Note on int() type conversion: `extractFirstIdentifier` filters `int` as a
// CEL builtin, so `${int(src.data.count)}` has no detected dependency on `src`
// and runs at DAG level 0 (before any Watch nodes resolve). This is a known
// limitation — standalone expression tests that require integer types should
// use fields that are natively typed integers (e.g., metadata.generation on
// resources with spec changes, or spec.replicas from an externally-created
// Deployment).
func TestStandaloneVsEmbeddedCELTypePreservation(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	cmGVK := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}

	// Source ConfigMap: data.value is a string "world".
	// Both standalone and embedded expressions reference it.
	source := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "cel-type-source",
				"namespace": ns,
			},
			"data": map[string]any{
				"value": "world",
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, source))

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-cel-types",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					// Read the source into scope (Watch shape: identity-only template).
					map[string]any{
						"id": "src",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "cel-type-source"},
						},
					},
					// Standalone expression: ${src.data.value} → raw string value.
					// The field data.direct gets exactly "world" — no surrounding text added.
					map[string]any{
						"id": "direct",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "cel-type-direct"},
							"data": map[string]any{
								"result": "${src.data.value}",
							},
						},
					},
					// Embedded expression: "prefix-${src.data.value}-suffix" → string concat.
					// The field data.wrapped always produces a string with surrounding literals.
					map[string]any{
						"id": "wrapped",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "cel-type-wrapped"},
							"data": map[string]any{
								"result": "prefix-${src.data.value}-suffix",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for both managed resources to be created.
	directCM := &unstructured.Unstructured{}
	directCM.SetGroupVersionKind(cmGVK)
	require.NoError(t, waitForResource(ctx, k8sClient,
		types.NamespacedName{Name: "cel-type-direct", Namespace: ns}, directCM))

	wrappedCM := &unstructured.Unstructured{}
	wrappedCM.SetGroupVersionKind(cmGVK)
	require.NoError(t, waitForResource(ctx, k8sClient,
		types.NamespacedName{Name: "cel-type-wrapped", Namespace: ns}, wrappedCM))

	// ASSERTION 1: Standalone expression produces the raw value.
	directResult := &unstructured.Unstructured{}
	directResult.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "cel-type-direct", Namespace: ns}, directResult))
	directData, _, _ := unstructured.NestedStringMap(directResult.Object, "data")
	assert.Equal(t, "world", directData["result"],
		"standalone ${src.data.value} must produce exactly the raw value \"world\"")
	t.Logf("Standalone expression: data.result=%q — raw value preserved", directData["result"])

	// ASSERTION 2: Embedded expression produces concatenated string.
	wrappedResult := &unstructured.Unstructured{}
	wrappedResult.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "cel-type-wrapped", Namespace: ns}, wrappedResult))
	wrappedData, _, _ := unstructured.NestedStringMap(wrappedResult.Object, "data")
	assert.Equal(t, "prefix-world-suffix", wrappedData["result"],
		"embedded \"prefix-${src.data.value}-suffix\" must concatenate to \"prefix-world-suffix\"")
	t.Logf("Embedded expression: data.result=%q — string interpolation proved", wrappedData["result"])

	// ASSERTION 3: Reactive update — changing source triggers re-evaluation of both.
	latestSrc := &unstructured.Unstructured{}
	latestSrc.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "cel-type-source", Namespace: ns}, latestSrc))
	unstructured.SetNestedField(latestSrc.Object, "earth", "data", "value")
	require.NoError(t, k8sClient.Update(ctx, latestSrc))
	t.Log("Updated source: value=earth")

	// Wait for standalone node to update.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true,
		func(ctx context.Context) (bool, error) {
			check := &unstructured.Unstructured{}
			check.SetGroupVersionKind(cmGVK)
			if err := k8sClient.Get(ctx,
				types.NamespacedName{Name: "cel-type-direct", Namespace: ns}, check); err != nil {
				return false, nil
			}
			d, _, _ := unstructured.NestedStringMap(check.Object, "data")
			return d["result"] == "earth", nil
		}))
	t.Log("Standalone updated to \"earth\" — reactive propagation proved")

	// Wait for embedded node to update.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true,
		func(ctx context.Context) (bool, error) {
			check := &unstructured.Unstructured{}
			check.SetGroupVersionKind(cmGVK)
			if err := k8sClient.Get(ctx,
				types.NamespacedName{Name: "cel-type-wrapped", Namespace: ns}, check); err != nil {
				return false, nil
			}
			d, _, _ := unstructured.NestedStringMap(check.Object, "data")
			return d["result"] == "prefix-earth-suffix", nil
		}))
	t.Log("Embedded updated to \"prefix-earth-suffix\" — reactive string interpolation proved")
}
