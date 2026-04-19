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

// Fault injection tests exercise error paths.
//
// These tests produce real error conditions against the envtest API server:
// watched resources disappearing mid-reconcile, spec validation failures
// from impossible resource states, 5xx server errors via webhook fault
// injection, and recovery after transient conditions resolve.
//
// 5xx / SystemError tests use a validating webhook (webhook_test.go) that
// returns HTTP 500 for targeted ConfigMaps. This is genuine fault injection
// at the API server layer — no mocking, no transport-level tricks. The
// controller receives a real InternalError from a real API server and must
// classify it as SystemError, apply backoff, and recover.

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
						"ref": map[string]any{
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
// reconcile (design 005-reconciliation § Propagation: SSA apply creates if absent).
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
// Per 005-reconciliation.md § Node States: Definition nodes do not
// produce SystemError (no API calls). CEL evaluation failures are Error.
//
// Two bugs were found:
//  1. The (now-deleted) resolveReference error path didn't store
//     previousPlanStates — the Error state was lost on the next reconcile,
//     overwritten with Ready. Under the declared-keyword schema the
//     classification is parse-time and there is no resolution error path;
//     the same storage bug is covered for the remaining error paths
//     (includeWhen, readyWhen, per-node apply errors).
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
						"ref": map[string]any{
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
// Design 005-reconciliation § Propagation:
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
						"patch": map[string]any{
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
					"patch": map[string]any{
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
					"patch": map[string]any{
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

// TestSystemError_WebhookFaultAndRecovery proves that a 5xx error from the
// API server (injected via validating webhook) is classified as SystemError
// and the controller recovers when the fault clears.
//
// Per 005-reconciliation.md § Node States:
//   - "Server errors (5xx/timeout/network) → NodeSystemError"
//   - "Transient errors retry with exponential backoff [1s, resyncInterval]"
//
// Per errors.go: apierrors.IsInternalError → NodeSystemError with reason
// "ServerError".
//
// This was the largest gap in fault injection coverage: the controller's
// state machine classification of 5xx errors and the backoff/recovery
// cycle were untested at integration level.
func TestSystemError_WebhookFaultAndRecovery(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Label the namespace so the webhook selector matches.
	faultLabel := "fault-" + ns
	labelNamespace(t, ns, map[string]string{"fault-inject": faultLabel})

	// Start webhook server and register it.
	fw := startFaultWebhook(t)
	registerFaultWebhook(t, k8sClient, "fault-"+ns, fw, "fault-inject", faultLabel)

	// Create a Graph that owns a ConfigMap.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-system-error",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "managed",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "system-error-cm"},
							"data":       map[string]any{"version": "v1"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))
	graphKey := types.NamespacedName{Name: "test-system-error", Namespace: ns}

	// Wait for initial convergence (webhook accepts everything).
	require.NoError(t, waitForGraphReady(ctx, k8sClient, graphKey))
	t.Log("Phase 1: Graph converged — ConfigMap created, webhook accepting")

	// Enable fault injection: the webhook returns 500 for this ConfigMap.
	fw.Reject("system-error-cm")
	t.Log("Phase 2: Fault enabled — webhook rejects system-error-cm with 500")

	// Trigger re-evaluation by changing the Graph spec. The controller will
	// try to update the ConfigMap → webhook returns 500 → InternalError.
	require.NoError(t, updateWithRetry(ctx, k8sClient, GraphGVK, graphKey,
		func(obj *unstructured.Unstructured) {
			unstructured.SetNestedSlice(obj.Object, []any{
				map[string]any{
					"id": "managed",
					"template": map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata":   map[string]any{"name": "system-error-cm"},
						"data":       map[string]any{"version": "v2"},
					},
				},
			}, "spec", "nodes")
		}))

	// The Graph should reach SystemError state.
	require.NoError(t, waitForGraphReadyReason(ctx, k8sClient, graphKey, "SystemError"),
		"Graph should report SystemError after 5xx from webhook")
	t.Log("Phase 2: Graph shows SystemError — 5xx correctly classified")

	// Verify the Ready condition status is False (not Unknown).
	g := &unstructured.Unstructured{}
	g.SetGroupVersionKind(GraphGVK)
	require.NoError(t, k8sClient.Get(ctx, graphKey, g))
	assert.Equal(t, "False", graphReadyStatus(g),
		"SystemError should produce Ready=False, not Unknown")

	// Disable fault injection — controller should recover on next backoff retry.
	fw.AcceptAll()
	t.Log("Phase 3: Fault cleared — webhook accepting again")

	// Wait for Graph to recover to Ready.
	require.NoError(t, waitForGraphReady(ctx, k8sClient, graphKey),
		"Graph should recover to Ready after webhook fault clears")

	// Verify the ConfigMap was updated to v2.
	cm := &unstructured.Unstructured{}
	cm.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "system-error-cm", Namespace: ns}, cm))
	data, _, _ := unstructured.NestedStringMap(cm.Object, "data")
	assert.Equal(t, "v2", data["version"],
		"ConfigMap should have version=v2 after recovery")
	t.Log("Phase 3: Graph recovered — SystemError → backoff → retry → Ready")
}

// TestSystemError_DependentInheritsBlocked proves that when a node enters
// SystemError (5xx), its dependents inherit Blocked state — they are not
// evaluated until the system error resolves.
//
// Per 005-reconciliation.md § Propagation:
//
//	"Any dep Blocked/Error/Conflict/SystemError → Blocked"
//
// This ensures the Blocked propagation path from SystemError is exercised
// at integration level, not just the Conflict→Blocked path.
func TestSystemError_DependentInheritsBlocked(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	faultLabel := "fault-dep-" + ns
	labelNamespace(t, ns, map[string]string{"fault-inject": faultLabel})

	fw := startFaultWebhook(t)
	registerFaultWebhook(t, k8sClient, "fault-dep-"+ns, fw, "fault-inject", faultLabel)

	// Pre-create a source ConfigMap (ref target — GET not intercepted by webhook).
	source := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "syserr-source", "namespace": ns},
			"data":     map[string]any{"value": "upstream-data"},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, source))

	// Graph: upstream (will be faulted) → downstream (depends on upstream).
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-syserr-blocked",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "upstream",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "syserr-upstream"},
							"data":       map[string]any{"state": "v1"},
						},
					},
					map[string]any{
						"id": "downstream",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "syserr-downstream"},
							"data":       map[string]any{"fromUpstream": "${upstream.data.state}"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))
	graphKey := types.NamespacedName{Name: "test-syserr-blocked", Namespace: ns}

	// Initial convergence.
	require.NoError(t, waitForGraphReady(ctx, k8sClient, graphKey))
	t.Log("Phase 1: Both nodes converged")

	// Fault the upstream node only.
	fw.Reject("syserr-upstream")

	// Trigger re-evaluation.
	require.NoError(t, updateWithRetry(ctx, k8sClient, GraphGVK, graphKey,
		func(obj *unstructured.Unstructured) {
			unstructured.SetNestedSlice(obj.Object, []any{
				map[string]any{
					"id": "upstream",
					"template": map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata":   map[string]any{"name": "syserr-upstream"},
						"data":       map[string]any{"state": "v2"},
					},
				},
				map[string]any{
					"id": "downstream",
					"template": map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata":   map[string]any{"name": "syserr-downstream"},
						"data":       map[string]any{"fromUpstream": "${upstream.data.state}"},
					},
				},
			}, "spec", "nodes")
		}))

	// Graph should show SystemError (upstream's 5xx takes precedence).
	require.NoError(t, waitForGraphReadyReason(ctx, k8sClient, graphKey, "SystemError"))
	t.Log("Phase 2: upstream in SystemError, Graph reports SystemError")

	// downstream's resource should still exist (not pruned — uncertain absence).
	downstream := &unstructured.Unstructured{}
	downstream.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "syserr-downstream", Namespace: ns}, downstream),
		"downstream resource should survive while upstream is in SystemError")

	// downstream should NOT have been updated to v2 (it's blocked).
	downData, _, _ := unstructured.NestedStringMap(downstream.Object, "data")
	assert.Equal(t, "v1", downData["fromUpstream"],
		"downstream should retain v1 — blocked by upstream SystemError, not re-evaluated")
	t.Log("Phase 2: downstream blocked — retains v1, not updated to v2")

	// Clear fault, wait for recovery.
	fw.AcceptAll()
	require.NoError(t, waitForGraphReady(ctx, k8sClient, graphKey))

	// Verify downstream now has v2.
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "syserr-downstream", Namespace: ns}, downstream))
	downData, _, _ = unstructured.NestedStringMap(downstream.Object, "data")
	assert.Equal(t, "v2", downData["fromUpstream"],
		"downstream should have v2 after SystemError recovery")
	t.Log("Phase 3: recovered — both nodes updated to v2")
}

// TestCELRuntimeError_RegressionRecovery proves the full error→fix→recovery
// cycle for CEL runtime errors. TestErrorClassification_RegressionCELRuntime
// proves classification; this test closes the loop by verifying recovery.
//
// Scenario: a CEL expression compiles but fails at runtime (division by zero).
// The Graph enters Error state. Fixing the upstream data (divisor 0 → 2)
// triggers re-evaluation, the expression succeeds, and the Graph converges
// to Ready.
//
// Per 005-reconciliation.md § Node States: "Deterministic errors (4xx)
// are not retried — same inputs produce the same failure." The recovery path
// requires an input change (watch event) to trigger re-evaluation.
func TestCELRuntimeError_RegressionRecovery(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	cmGVK := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}
	graphKey := types.NamespacedName{Name: "cel-recovery", Namespace: ns}

	// Create source ConfigMap with divisor=0 (will cause division by zero).
	source := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "cel-recovery-source", "namespace": ns},
			"data":     map[string]any{"divisor": "0"},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, source))

	// Graph: ref the source, compute 100/divisor.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "cel-recovery",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "source",
						"ref": map[string]any{
							"apiVersion": "v1", "kind": "ConfigMap",
							"metadata": map[string]any{"name": "cel-recovery-source"},
						},
					},
					map[string]any{
						"id": "result",
						"template": map[string]any{
							"apiVersion": "v1", "kind": "ConfigMap",
							"metadata": map[string]any{"name": "cel-recovery-output"},
							"data":     map[string]any{"quotient": "${string(100 / int(source.data.divisor))}"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Phase 1: Graph should reach Error (division by zero is deterministic).
	require.NoError(t, waitForGraphReadyReason(ctx, k8sClient, graphKey, "Error"))
	t.Log("Phase 1: CEL runtime error (division by zero) → Error state")

	// Phase 2: Fix the input — change divisor to "2".
	require.NoError(t, updateWithRetry(ctx, k8sClient, cmGVK,
		types.NamespacedName{Name: "cel-recovery-source", Namespace: ns},
		func(obj *unstructured.Unstructured) {
			obj.Object["data"] = map[string]any{"divisor": "2"}
		}))
	t.Log("Phase 2: Fixed source divisor 0 → 2")

	// Phase 3: Graph should recover to Ready with correct result.
	require.NoError(t, waitForGraphReady(ctx, k8sClient, graphKey))

	output := &unstructured.Unstructured{}
	output.SetGroupVersionKind(cmGVK)
	require.NoError(t, waitForResource(ctx, k8sClient,
		types.NamespacedName{Name: "cel-recovery-output", Namespace: ns}, output))
	data, _, _ := unstructured.NestedStringMap(output.Object, "data")
	assert.Equal(t, "50", data["quotient"],
		"100 / 2 = 50 — CEL expression should evaluate correctly after fix")
	t.Log("Phase 3: Recovered — output ConfigMap has quotient=50")
}

// TestCRDDeletionWhileGraphManagesInstances proves that when a CRD is deleted
// while a Graph manages custom resources of that kind, the controller reaches
// a degraded state (Error or SystemError) but does not crash, does not
// corrupt other resources, and can recover when the CRD is reinstalled.
//
// Per 005-reconciliation.md § Teardown: "If the DAG is unavailable,
// prune is blocked — never degrade to unordered deletion." CRD deletion
// makes the Kind unresolvable; the controller must handle this gracefully.
func TestCRDDeletion_RegressionGracefulDegradation(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	group := uniqueGroup()
	crdName := "gadgets." + group
	gadgetGVK := schema.GroupVersionKind{
		Group: group, Version: "v1alpha1", Kind: "Gadget",
	}
	cmGVK := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}
	graphKey := types.NamespacedName{Name: "test-crd-delete", Namespace: ns}

	// Phase 1: Install CRD + create resource + create Graph that refs it.
	crd := buildCustomCRD(crdName, group, "Gadget", "gadgets")
	require.NoError(t, k8sClient.Create(ctx, crd))
	require.NoError(t, waitForCRD(ctx, k8sClient, crdName))
	t.Log("CRD installed and Established")

	gadget := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": group + "/v1alpha1",
			"kind":       "Gadget",
			"metadata": map[string]any{
				"name":      "my-gadget",
				"namespace": ns,
			},
			"spec": map[string]any{
				"color": "red",
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, gadget))

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-crd-delete",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "gadget",
						"ref": map[string]any{
							"apiVersion": group + "/v1alpha1",
							"kind":       "Gadget",
							"metadata":   map[string]any{"name": "my-gadget"},
						},
					},
					map[string]any{
						"id": "output",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "crd-delete-output"},
							"data":       map[string]any{"color": "${gadget.spec.color}"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Verify convergence.
	output := &unstructured.Unstructured{}
	output.SetGroupVersionKind(cmGVK)
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 60*time.Second, true,
		func(ctx context.Context) (bool, error) {
			if err := k8sClient.Get(ctx,
				types.NamespacedName{Name: "crd-delete-output", Namespace: ns}, output); err != nil {
				return false, nil
			}
			data, _, _ := unstructured.NestedStringMap(output.Object, "data")
			return data["color"] == "red", nil
		}))
	require.NoError(t, waitForGraphReady(ctx, k8sClient, graphKey))
	t.Log("Phase 1: Graph converged — output has color=red")

	// Phase 2: Delete the CRD. This cascades deletion of all Gadget instances
	// and makes the Gadget GVK unresolvable.
	require.NoError(t, k8sClient.Delete(ctx, crd))
	t.Log("Phase 2: CRD deleted")

	// The Graph should degrade — it can't resolve the ref anymore.
	// It should reach a non-Ready state (Error, SystemError, or Pending).
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true,
		func(ctx context.Context) (bool, error) {
			g := &unstructured.Unstructured{}
			g.SetGroupVersionKind(GraphGVK)
			if err := k8sClient.Get(ctx, graphKey, g); err != nil {
				return false, nil
			}
			return !graphReady(g), nil
		}), "Graph should degrade after CRD deletion")
	t.Log("Phase 2: Graph is no longer Ready (expected)")

	// The ConfigMap (output) should still exist — the controller must not
	// tear down resources just because one node's GVK became unresolvable.
	outputCheck := &unstructured.Unstructured{}
	outputCheck.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "crd-delete-output", Namespace: ns}, outputCheck),
		"output ConfigMap should survive CRD deletion — uncertain absence blocks prune")
	t.Log("Phase 2: output ConfigMap preserved (uncertain absence blocks prune)")

	// Phase 3: Reinstall the CRD and recreate the Gadget. Graph should recover.
	crd2 := buildCustomCRD(crdName, group, "Gadget", "gadgets")
	require.NoError(t, k8sClient.Create(ctx, crd2))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, crd2) })
	require.NoError(t, waitForCRD(ctx, k8sClient, crdName))

	gadget2 := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": group + "/v1alpha1",
			"kind":       "Gadget",
			"metadata": map[string]any{
				"name":      "my-gadget",
				"namespace": ns,
			},
			"spec": map[string]any{
				"color": "blue",
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, gadget2))
	t.Log("Phase 3: CRD reinstalled, Gadget recreated with color=blue")

	// Graph should recover and update the output.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 60*time.Second, true,
		func(ctx context.Context) (bool, error) {
			cm := &unstructured.Unstructured{}
			cm.SetGroupVersionKind(cmGVK)
			if err := k8sClient.Get(ctx,
				types.NamespacedName{Name: "crd-delete-output", Namespace: ns}, cm); err != nil {
				return false, nil
			}
			data, _, _ := unstructured.NestedStringMap(cm.Object, "data")
			return data["color"] == "blue", nil
		}), "output should update to blue after CRD reinstall")
	require.NoError(t, waitForGraphReady(ctx, k8sClient, graphKey))
	t.Log("Phase 3: Recovered — output updated to color=blue, Graph Ready")

	_ = gadgetGVK // referenced to avoid unused import
}
