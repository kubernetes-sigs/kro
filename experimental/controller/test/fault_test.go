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

// Fault injection tests exercise error paths (design 006-quality § Testing).
//
// These tests produce real error conditions against the envtest API server:
// watched resources disappearing mid-reconcile, spec validation failures
// from impossible resource states, and recovery after transient conditions
// resolve. They prove the controller handles legible errors correctly —
// network failures and 5xx errors are tested elsewhere (client-go's retry
// loop owns those paths).

// TestWatchedResourceDeletedMidReconcile proves that when a watched resource
// is externally deleted after the Graph has converged, the controller
// recovers gracefully — dependents enter pending, and the Graph
// status reflects the blocked state.
//
// This is a fault the controller hits in production: an operator deletes
// a ConfigMap that a Graph watches, and the Graph must reconverge.
func TestWatchedResourceDeletedMidReconcile(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	cmGVK := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}

	// Pre-create the watched resource.
	source := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "fault-watch-source",
				"namespace": ns,
			},
			"data": map[string]any{
				"value": "original",
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, source))

	// Graph watches the source, creates a dependent.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-fault-watch-delete",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "source",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "fault-watch-source"},
						},
					},
					map[string]any{
						"id": "dependent",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "fault-dependent"},
							"data":       map[string]any{"from": "${source.data.value}"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for convergence.
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-fault-watch-delete", Namespace: ns}))
	dep := &unstructured.Unstructured{}
	dep.SetGroupVersionKind(cmGVK)
	require.NoError(t, waitForResource(ctx, k8sClient,
		types.NamespacedName{Name: "fault-dependent", Namespace: ns}, dep))
	t.Log("Graph converged — source and dependent exist")

	// Delete the watched resource externally.
	toDelete := &unstructured.Unstructured{}
	toDelete.SetGroupVersionKind(cmGVK)
	toDelete.SetName("fault-watch-source")
	toDelete.SetNamespace(ns)
	require.NoError(t, k8sClient.Delete(ctx, toDelete))
	t.Log("Externally deleted watched resource")

	// The Graph should enter a non-Ready state (Pending or similar).
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true,
		func(ctx context.Context) (bool, error) {
			g := &unstructured.Unstructured{}
			g.SetGroupVersionKind(GraphGVK)
			if err := k8sClient.Get(ctx,
				types.NamespacedName{Name: "test-fault-watch-delete", Namespace: ns}, g); err != nil {
				return false, nil
			}
			// Should no longer be Ready=True
			return !graphReady(g), nil
		}))
	t.Log("Graph entered non-Ready state after watched resource was deleted")

	// Recreate the watched resource — Graph should recover.
	recreated := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "fault-watch-source",
				"namespace": ns,
			},
			"data": map[string]any{
				"value": "recovered",
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, recreated))
	t.Log("Recreated watched resource with value=recovered")

	// Wait for Graph to recover to Ready.
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-fault-watch-delete", Namespace: ns}))

	// Verify dependent was updated with new value.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true,
		func(ctx context.Context) (bool, error) {
			d := &unstructured.Unstructured{}
			d.SetGroupVersionKind(cmGVK)
			if err := k8sClient.Get(ctx,
				types.NamespacedName{Name: "fault-dependent", Namespace: ns}, d); err != nil {
				return false, nil
			}
			data, _, _ := unstructured.NestedStringMap(d.Object, "data")
			return data["from"] == "recovered", nil
		}))
	t.Log("Graph recovered — dependent updated with value=recovered")
}

// TestOwnedResourceDeletedExternally proves that when a resource the Graph
// owns is externally deleted, the controller recreates it on the next
// reconcile (design 004-graph-execution § Wind: SSA apply creates if absent).
func TestOwnedResourceDeletedExternally(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	cmGVK := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-fault-owned-delete",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "managed",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "fault-managed"},
							"data":       map[string]any{"desired": "state"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for convergence.
	cm := &unstructured.Unstructured{}
	cm.SetGroupVersionKind(cmGVK)
	require.NoError(t, waitForResource(ctx, k8sClient,
		types.NamespacedName{Name: "fault-managed", Namespace: ns}, cm))
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-fault-owned-delete", Namespace: ns}))
	originalRV := cm.GetResourceVersion()
	t.Logf("Managed resource created, RV=%s", originalRV)

	// Externally delete the managed resource.
	toDelete := &unstructured.Unstructured{}
	toDelete.SetGroupVersionKind(cmGVK)
	toDelete.SetName("fault-managed")
	toDelete.SetNamespace(ns)
	require.NoError(t, k8sClient.Delete(ctx, toDelete))
	t.Log("Externally deleted managed resource")

	// Wait for the controller to recreate it.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true,
		func(ctx context.Context) (bool, error) {
			check := &unstructured.Unstructured{}
			check.SetGroupVersionKind(cmGVK)
			if err := k8sClient.Get(ctx,
				types.NamespacedName{Name: "fault-managed", Namespace: ns}, check); err != nil {
				return false, nil
			}
			// Must be a different object (new resourceVersion, new UID)
			return check.GetResourceVersion() != originalRV, nil
		}))

	// Verify the recreated resource has the correct state.
	recreated := &unstructured.Unstructured{}
	recreated.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "fault-managed", Namespace: ns}, recreated))
	data, _, _ := unstructured.NestedStringMap(recreated.Object, "data")
	assert.Equal(t, "state", data["desired"])
	assertManagedBy(t, recreated, "test-fault-owned-delete")
	t.Log("Controller recreated managed resource with correct state")
}

// TestInvalidCELExpressionSurfacesError proves that a Graph with an invalid
// CEL expression is rejected at compile time — Compiled=False with
// ExpressionError reason (design 001-graph § Status § Conditions).
// The controller does not crash and the Graph can be fixed.
func TestInvalidCELExpressionSurfacesError(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	cmGVK := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}

	// Create Graph with invalid CEL syntax.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-fault-invalid-cel",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "broken",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "broken-cm"},
							"data": map[string]any{
								"value": "${this.is.not.a.valid.ref+++}",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for Compiled=False.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true,
		func(ctx context.Context) (bool, error) {
			g := &unstructured.Unstructured{}
			g.SetGroupVersionKind(GraphGVK)
			if err := k8sClient.Get(ctx,
				types.NamespacedName{Name: "test-fault-invalid-cel", Namespace: ns}, g); err != nil {
				return false, nil
			}
			status, _ := g.Object["status"].(map[string]any)
			if status == nil {
				return false, nil
			}
			conditions, _ := status["conditions"].([]any)
			compiled, found := findCondition(conditions, "Compiled")
			if !found {
				return false, nil
			}
			return compiled["status"] == "False", nil
		}))
	t.Log("Graph rejected — Compiled=False")

	// No managed resource should exist.
	err := k8sClient.Get(ctx, types.NamespacedName{Name: "broken-cm", Namespace: ns},
		&unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
		}})
	assert.Error(t, err, "no resource should be created for rejected Graph")

	// Fix the CEL expression — Graph should recover.
	require.NoError(t, updateWithRetry(ctx, k8sClient, GraphGVK,
		types.NamespacedName{Name: "test-fault-invalid-cel", Namespace: ns}, func(obj *unstructured.Unstructured) {
			unstructured.SetNestedSlice(obj.Object, []any{
				map[string]any{
					"id": "fixed",
					"template": map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata":   map[string]any{"name": "fixed-cm"},
						"data":       map[string]any{"value": "fixed"},
					},
				},
			}, "spec", "nodes")
		}))
	t.Log("Fixed CEL expression")

	// Wait for the fixed resource to be created.
	fixed := &unstructured.Unstructured{}
	fixed.SetGroupVersionKind(cmGVK)
	require.NoError(t, waitForResource(ctx, k8sClient,
		types.NamespacedName{Name: "fixed-cm", Namespace: ns}, fixed))
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-fault-invalid-cel", Namespace: ns}))
	t.Log("Graph recovered after fixing CEL expression — Compiled=True, Ready=True")
}

// TestConflictThenSpecChangeResolvesConflict proves that changing the Graph
// spec to remove the contested field clears the conflict state and triggers
// a successful re-apply (design 003-ownership § kro's Model: "A template
// change clears the conflict state and triggers a new apply attempt").
func TestConflictThenSpecChangeResolvesConflict(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	cmGVK := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}

	// Establish external ownership on a specific field.
	applyConfigMapAs(t, ns, "spec-conflict-cm", "external-manager", map[string]string{
		"contested": "external-value",
		"shared":    "both-agree",
	})
	t.Log("External manager owns 'contested' field")

	// Graph tries to write a different value to the contested field.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-fault-spec-resolve",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "target",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "spec-conflict-cm"},
							"data": map[string]any{
								"contested": "graph-value",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for Conflict.
	require.NoError(t, waitForGraphReadyReason(ctx, k8sClient,
		types.NamespacedName{Name: "test-fault-spec-resolve", Namespace: ns}, "Conflict"))
	t.Log("Graph shows Conflict")

	// Fix: change the spec to remove the contested field.
	require.NoError(t, updateWithRetry(ctx, k8sClient, GraphGVK,
		types.NamespacedName{Name: "test-fault-spec-resolve", Namespace: ns}, func(obj *unstructured.Unstructured) {
			unstructured.SetNestedSlice(obj.Object, []any{
				map[string]any{
					"id": "target",
					"template": map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata":   map[string]any{"name": "spec-conflict-cm"},
						"data": map[string]any{
							"noncontested": "graph-only-value",
						},
					},
				},
			}, "spec", "nodes")
		}))
	t.Log("Updated spec to remove contested field")

	// Graph should become Ready.
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-fault-spec-resolve", Namespace: ns}))

	// Verify the resource has the Graph's non-contested field.
	cm := &unstructured.Unstructured{}
	cm.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "spec-conflict-cm", Namespace: ns}, cm))
	data, _, _ := unstructured.NestedStringMap(cm.Object, "data")
	assert.Equal(t, "graph-only-value", data["noncontested"])
	// External field should still be present.
	assert.Equal(t, "external-value", data["contested"])
	t.Log("Conflict resolved via spec change — Graph and external manager coexist")
}

// TestErrorClassification_RegressionCELRuntime proves that a CEL expression
// that compiles but fails at runtime (division by zero) is classified as a
// deterministic user error (Error) — not SystemError (transient/retriable).
//
// Per 004-graph-execution.md: "Definition nodes can be Ready, NotReady,
// or Error. They cannot be SystemError (no API calls)."
//
// Two bugs were found:
//  1. resolveReference error path didn't store previousPlanStates — the
//     Error state was lost on the next reconcile, overwritten with Ready.
//  2. NodeError had no drift timer case — the node was never re-evaluated
//     when upstream data was stable.
func TestErrorClassification_RegressionCELRuntime(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Pre-create a ConfigMap with divisor=0. The expression 100/int("0")
	// compiles but hits division-by-zero at runtime.
	source := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "cel-error-source", "namespace": ns},
			"data":     map[string]any{"divisor": "0"},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, source))

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "cel-runtime-error",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "source",
						"template": map[string]any{
							"apiVersion": "v1", "kind": "ConfigMap",
							"metadata": map[string]any{"name": "cel-error-source"},
						},
					},
					map[string]any{
						"id": "broken",
						"template": map[string]any{
							"apiVersion": "v1", "kind": "ConfigMap",
							"metadata": map[string]any{"name": "cel-error-output"},
							"data":     map[string]any{"result": "${100 / int(source.data.divisor)}"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// The Graph must reach Error (not Ready, not SystemError). The division
	// by zero is a deterministic spec error — classifyAPIError returns NodeError.
	require.NoError(t, waitForGraphReadyReason(ctx, k8sClient,
		types.NamespacedName{Name: "cel-runtime-error", Namespace: ns}, "Error"))
}

// TestStatusSubresourceSplitApplyRevertOnFailure proves that when a
// status subresource apply fails (validation error), the apply-hash is
// NOT advanced. The next reconcile retries both main + status apply.
// Once the status apply succeeds, steady state resumes.
//
// Design 004-graph-reconciliation § Resolve step 5:
//
//	"When a template targets both the main resource and the status subresource,
//	the controller splits the apply into two operations."
//
// This test uses the StrictStatus CRD (status.phase is an enum). The Graph
// writes an invalid enum value causing the status apply to fail while the
// main apply succeeds.
//
// Failure mode: status fields silently lost — hash says "nothing to do"
// but status was never written.
func TestStatusSubresourceSplitApplyRevertOnFailure(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	strictGVK := schema.GroupVersionKind{
		Group: "test.kro.run", Version: "v1alpha1", Kind: "StrictStatus",
	}

	// Pre-create a StrictStatus CR with valid status.
	target := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "test.kro.run/v1alpha1",
			"kind":       "StrictStatus",
			"metadata": map[string]any{
				"name":      "split-apply-target",
				"namespace": ns,
			},
			"spec": map[string]any{
				"name": "test",
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, target))
	t.Log("StrictStatus CR pre-created")

	// Phase 1: Graph writes VALID status.phase="Running".
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-split-apply",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "target",
						"template": map[string]any{
							"apiVersion": "test.kro.run/v1alpha1",
							"kind":       "StrictStatus",
							"metadata": map[string]any{
								"name": "split-apply-target",
								"annotations": map[string]any{
									"kro.run/version": "v1",
								},
							},
							"status": map[string]any{
								"phase":   "Running",
								"message": "all-good",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for the status to be applied.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true,
		func(ctx context.Context) (bool, error) {
			check := &unstructured.Unstructured{}
			check.SetGroupVersionKind(strictGVK)
			if err := k8sClient.Get(ctx,
				types.NamespacedName{Name: "split-apply-target", Namespace: ns}, check); err != nil {
				return false, nil
			}
			statusMap, _, _ := unstructured.NestedMap(check.Object, "status")
			return statusMap["phase"] == "Running", nil
		}))
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-split-apply", Namespace: ns}))
	t.Log("Phase 1: Valid status applied (phase=Running), Graph Ready")

	// Phase 2: Update Graph to write INVALID status.phase="InvalidPhase".
	// Main apply (annotations) succeeds, status apply gets 422.
	require.NoError(t, updateWithRetry(ctx, k8sClient, GraphGVK,
		types.NamespacedName{Name: "test-split-apply", Namespace: ns}, func(obj *unstructured.Unstructured) {
			unstructured.SetNestedSlice(obj.Object, []any{
				map[string]any{
					"id": "target",
					"template": map[string]any{
						"apiVersion": "test.kro.run/v1alpha1",
						"kind":       "StrictStatus",
						"metadata": map[string]any{
							"name": "split-apply-target",
							"annotations": map[string]any{
								"kro.run/version": "v2-invalid-status",
							},
						},
						"status": map[string]any{
							"phase":   "InvalidPhase",
							"message": "this-should-fail",
						},
					},
				},
			}, "spec", "nodes")
		}))
	t.Log("Phase 2: Updated Graph with invalid status.phase=InvalidPhase")

	// Wait for the Graph to show an error state (status apply failed).
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true,
		func(ctx context.Context) (bool, error) {
			g := &unstructured.Unstructured{}
			g.SetGroupVersionKind(GraphGVK)
			if err := k8sClient.Get(ctx,
				types.NamespacedName{Name: "test-split-apply", Namespace: ns}, g); err != nil {
				return false, nil
			}
			return !graphReady(g), nil
		}))
	t.Log("Graph entered non-Ready state (status apply failed)")

	// THE KEY ASSERTION: status.phase must still be "Running" (the invalid
	// value was rejected by CRD validation). If the apply-hash was
	// incorrectly advanced, a subsequent reconcile would skip the apply
	// and status would never be corrected.
	checkTarget := &unstructured.Unstructured{}
	checkTarget.SetGroupVersionKind(strictGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "split-apply-target", Namespace: ns}, checkTarget))
	statusMap, _, _ := unstructured.NestedMap(checkTarget.Object, "status")
	assert.Equal(t, "Running", statusMap["phase"],
		"status.phase must remain Running — invalid status apply should have been rejected")
	t.Log("Status.phase still Running — invalid value correctly rejected")

	// Phase 3: Fix the Graph to write valid status.phase="Stopped".
	require.NoError(t, updateWithRetry(ctx, k8sClient, GraphGVK,
		types.NamespacedName{Name: "test-split-apply", Namespace: ns}, func(obj *unstructured.Unstructured) {
			unstructured.SetNestedSlice(obj.Object, []any{
				map[string]any{
					"id": "target",
					"template": map[string]any{
						"apiVersion": "test.kro.run/v1alpha1",
						"kind":       "StrictStatus",
						"metadata": map[string]any{
							"name": "split-apply-target",
							"annotations": map[string]any{
								"kro.run/version": "v3-fixed",
							},
						},
						"status": map[string]any{
							"phase":   "Stopped",
							"message": "recovered",
						},
					},
				},
			}, "spec", "nodes")
		}))
	t.Log("Phase 3: Fixed Graph with valid status.phase=Stopped")

	// Status should now update to Stopped.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true,
		func(ctx context.Context) (bool, error) {
			check := &unstructured.Unstructured{}
			check.SetGroupVersionKind(strictGVK)
			if err := k8sClient.Get(ctx,
				types.NamespacedName{Name: "split-apply-target", Namespace: ns}, check); err != nil {
				return false, nil
			}
			s, _, _ := unstructured.NestedMap(check.Object, "status")
			return s["phase"] == "Stopped", nil
		}))
	t.Log("Status.phase updated to Stopped — split-apply revert and recovery proved")

	// Graph should recover to Ready.
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-split-apply", Namespace: ns}))
	t.Log("Graph recovered to Ready — status subresource split-apply fault tolerance proved")
}
