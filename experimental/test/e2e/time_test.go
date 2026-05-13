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
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ═══════════════════════════════════════════════════════════════════════════════
// time.now() and .condition() integration tests
//
// These tests exercise the time-aware scheduling and condition construction
// features described in the design:
//
//   - time.now() as a raw value (no comparison → no enqueue)
//   - time.now() in a comparison (kro solves for the target time, enqueues)
//   - time.now() in a ternary (flips value at the solved instant)
//   - .condition() constructs Kubernetes status conditions with proper
//     lastTransitionTime semantics
// ═══════════════════════════════════════════════════════════════════════════════

// TestTimeNowRawValue proves that time.now() without a comparison is
// evaluated as the current wall clock and written as a raw RFC3339 value.
// No time-based enqueue should occur — this is a pure value expression.
func TestTimeNowRawValue(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-time-raw",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "timestamped",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "timestamped",
								"annotations": map[string]any{
									"created-at": "${string(time.now())}",
								},
							},
							"data": map[string]any{
								"placeholder": "value",
							},
						},
					},
				},
			},
		},
	}

	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for the ConfigMap to appear.
	cm := &unstructured.Unstructured{}
	cm.SetGroupVersionKind(cmGVK)
	require.NoError(t, waitForResource(ctx, k8sClient,
		types.NamespacedName{Name: "timestamped", Namespace: ns}, cm))

	// Assert the annotation contains a valid RFC3339 timestamp.
	annotations := cm.GetAnnotations()
	require.Contains(t, annotations, "created-at")
	ts := annotations["created-at"]
	_, err := time.Parse(time.RFC3339, ts)
	assert.NoError(t, err, "annotation should be valid RFC3339, got: %s", ts)
	t.Logf("time.now() raw value wrote: %s", ts)
}

// TestTimeNowGateEnqueue proves that when time.now() appears in a
// propagateWhen comparison, kro solves for the moment the comparison becomes
// true and enqueues reconciliation at exactly that time.
//
// The test creates a source ConfigMap with createdAt=now(), then a Graph
// with a 5s gate. The gated node must NOT appear during the first 2s, then
// MUST appear within 10s — proving time-based enqueue, not polling.
func TestTimeNowGateEnqueue(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Pre-create source with current time.
	now := time.Now().UTC().Format(time.RFC3339)
	source := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "gate-source",
				"namespace": ns,
			},
			"data": map[string]any{
				"createdAt": now,
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, source))

	// Graph: ref the source, gate a template on time.now() >= createdAt + 5s.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-time-gate",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "source",
						"ref": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "gate-source"},
						},
					},
					map[string]any{
						"id":            "delayed",
						"propagateWhen": []any{"${time.now() - timestamp(source.data.createdAt) >= duration('5s')}"},
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "delayed-output"},
							"data":       map[string]any{"status": "appeared"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// The gated node should NOT appear during the first 2 seconds.
	err := waitForAbsence(ctx, k8sClient, cmGVK,
		types.NamespacedName{Name: "delayed-output", Namespace: ns}, 2*time.Second)
	require.NoError(t, err, "gated node must not appear before the 5s threshold")
	t.Log("Confirmed: delayed-output absent during first 2s")

	// The gated node MUST appear within 10s (5s gate + margin).
	delayed := &unstructured.Unstructured{}
	delayed.SetGroupVersionKind(cmGVK)
	require.NoError(t, waitForResource(ctx, k8sClient,
		types.NamespacedName{Name: "delayed-output", Namespace: ns}, delayed, 10*time.Second),
		"gated node must appear after 5s threshold")

	data, _, _ := unstructured.NestedString(delayed.Object, "data", "status")
	assert.Equal(t, "appeared", data)
	t.Log("time.now() gate enqueue proved: node appeared after solved threshold")
}

// TestTimeNowTernary proves that time.now() in a ternary expression causes
// the value to flip at the solved instant. Initially the value is 'before',
// then after 4s it flips to 'after'.
func TestTimeNowTernary(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Pre-create source with current time.
	now := time.Now().UTC().Format(time.RFC3339)
	source := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "ternary-source",
				"namespace": ns,
			},
			"data": map[string]any{
				"createdAt": now,
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, source))

	// Graph: ref source, template with ternary on time.now().
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-time-ternary",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "source",
						"ref": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "ternary-source"},
						},
					},
					map[string]any{
						"id": "result",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "ternary-result"},
							"data": map[string]any{
								"phase": "${time.now() - timestamp(source.data.createdAt) >= duration('4s') ? 'after' : 'before'}",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Assert it initially has value 'before'.
	require.NoError(t, waitForField(ctx, k8sClient, cmGVK,
		types.NamespacedName{Name: "ternary-result", Namespace: ns},
		[]string{"data", "phase"}, "before", 5*time.Second),
		"ternary should initially evaluate to 'before'")
	t.Log("Phase 1: ternary = 'before'")

	// Assert it eventually flips to 'after' (4s gate + margin).
	require.NoError(t, waitForField(ctx, k8sClient, cmGVK,
		types.NamespacedName{Name: "ternary-result", Namespace: ns},
		[]string{"data", "phase"}, "after", 8*time.Second),
		"ternary should flip to 'after' after 4s threshold")
	t.Log("Phase 2: ternary = 'after' — time.now() in ternary proved")
}

// TestConditionConstruction proves that .condition() constructs a proper
// Kubernetes status condition with type, status, reason, message,
// observedGeneration, and a valid lastTransitionTime.
func TestConditionConstruction(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Create a custom CRD with status subresource for this test.
	group := uniqueGroup()
	crdName := fmt.Sprintf("condwidgets.%s", group)
	crd := buildCustomCRD(crdName, group, "CondWidget", "condwidgets")
	require.NoError(t, k8sClient.Create(ctx, crd))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, crd) })
	require.NoError(t, waitForCRD(ctx, k8sClient, crdName))

	crGVK := schema.GroupVersionKind{Group: group, Version: "v1alpha1", Kind: "CondWidget"}

	// Create an instance of the custom resource.
	instance := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": group + "/v1alpha1",
			"kind":       "CondWidget",
			"metadata": map[string]any{
				"name":      "test-instance",
				"namespace": ns,
			},
			"spec": map[string]any{},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, instance))

	// Graph: ref the instance, patch its status with .condition().
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-condition-construct",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "target",
						"ref": map[string]any{
							"apiVersion": group + "/v1alpha1",
							"kind":       "CondWidget",
							"metadata":   map[string]any{"name": "test-instance"},
						},
					},
					map[string]any{
						"id": "status",
						"patch": map[string]any{
							"apiVersion": group + "/v1alpha1",
							"kind":       "CondWidget",
							"metadata":   map[string]any{"name": "test-instance"},
							"status": map[string]any{
								"conditions": "${[target.condition('Ready', 'True', 'AllGood', 'Everything is ready')]}",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-condition-construct", Namespace: ns}))

	// Read the instance and verify the condition.
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(crGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "test-instance", Namespace: ns}, obj))

	conditions, found, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	require.True(t, found, "instance should have status.conditions")
	require.Len(t, conditions, 1)

	cond := conditions[0].(map[string]any)
	assert.Equal(t, "Ready", cond["type"])
	assert.Equal(t, "True", cond["status"])
	assert.Equal(t, "AllGood", cond["reason"])
	assert.Equal(t, "Everything is ready", cond["message"])

	// observedGeneration should match the instance's generation.
	gen := obj.GetGeneration()
	condGen, _ := cond["observedGeneration"].(int64)
	if condGen == 0 {
		// JSON round-trip may produce float64.
		if fg, ok := cond["observedGeneration"].(float64); ok {
			condGen = int64(fg)
		}
	}
	assert.Equal(t, gen, condGen, "observedGeneration should match metadata.generation")

	// lastTransitionTime must be a valid RFC3339 timestamp.
	ltt, _ := cond["lastTransitionTime"].(string)
	_, err := time.Parse(time.RFC3339, ltt)
	assert.NoError(t, err, "lastTransitionTime should be valid RFC3339, got: %s", ltt)
	t.Logf(".condition() construction proved: type=%s status=%s ltt=%s", cond["type"], cond["status"], ltt)
}

// TestConditionPreservesTransitionTime proves that when .condition() is
// re-evaluated with the same status, lastTransitionTime is preserved (not
// re-stamped). This is the Kubernetes condition contract: transition time
// only changes on actual status transitions.
func TestConditionPreservesTransitionTime(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Custom CRD for this test.
	group := uniqueGroup()
	crdName := fmt.Sprintf("preservewidgets.%s", group)
	crd := buildCustomCRD(crdName, group, "PreserveWidget", "preservewidgets")
	require.NoError(t, k8sClient.Create(ctx, crd))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, crd) })
	require.NoError(t, waitForCRD(ctx, k8sClient, crdName))

	crGVK := schema.GroupVersionKind{Group: group, Version: "v1alpha1", Kind: "PreserveWidget"}

	// Control ConfigMap drives the condition message (but NOT status).
	control := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "preserve-control",
				"namespace": ns,
			},
			"data": map[string]any{
				"message": "initial message",
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, control))

	// Instance of the custom resource.
	instance := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": group + "/v1alpha1",
			"kind":       "PreserveWidget",
			"metadata": map[string]any{
				"name":      "preserve-instance",
				"namespace": ns,
			},
			"spec": map[string]any{},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, instance))

	// Graph: ref the control and instance, patch status using .condition().
	// Status is always True; message comes from control ConfigMap.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-condition-preserve",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "control",
						"ref": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "preserve-control"},
						},
					},
					map[string]any{
						"id": "target",
						"ref": map[string]any{
							"apiVersion": group + "/v1alpha1",
							"kind":       "PreserveWidget",
							"metadata":   map[string]any{"name": "preserve-instance"},
						},
					},
					map[string]any{
						"id": "status",
						"patch": map[string]any{
							"apiVersion": group + "/v1alpha1",
							"kind":       "PreserveWidget",
							"metadata":   map[string]any{"name": "preserve-instance"},
							"status": map[string]any{
								"conditions": "${[target.condition('Ready', 'True', 'Operational', control.data.message)]}",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-condition-preserve", Namespace: ns}))

	// Wait for settle so we have a stable lastTransitionTime.
	require.NoError(t, waitForSettle(ctx, k8sClient, crGVK,
		types.NamespacedName{Name: "preserve-instance", Namespace: ns}))

	// Record T1.
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(crGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "preserve-instance", Namespace: ns}, obj))
	conditions, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	require.NotEmpty(t, conditions)
	t1 := conditions[0].(map[string]any)["lastTransitionTime"].(string)
	t.Logf("T1 lastTransitionTime: %s", t1)

	// Trigger a reconcile by updating the control message (status stays True).
	require.NoError(t, updateWithRetry(ctx, k8sClient, cmGVK,
		types.NamespacedName{Name: "preserve-control", Namespace: ns},
		func(obj *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(obj.Object, "updated message", "data", "message")
		}))

	// Wait for the message to propagate (proves reconcile happened).
	require.NoError(t, waitForSettle(ctx, k8sClient, crGVK,
		types.NamespacedName{Name: "preserve-instance", Namespace: ns}))

	// Read again and assert lastTransitionTime is preserved.
	obj2 := &unstructured.Unstructured{}
	obj2.SetGroupVersionKind(crGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "preserve-instance", Namespace: ns}, obj2))
	conditions2, _, _ := unstructured.NestedSlice(obj2.Object, "status", "conditions")
	require.NotEmpty(t, conditions2)
	cond2 := conditions2[0].(map[string]any)
	t2 := cond2["lastTransitionTime"].(string)

	assert.Equal(t, t1, t2,
		"lastTransitionTime must be preserved when status does not change (True→True)")
	// Verify message DID change (proves reconcile ran, just didn't re-stamp time).
	assert.Equal(t, "updated message", cond2["message"])
	t.Logf("Preservation proved: T1=%s T2=%s (same), message updated", t1, t2)
}

// TestConditionTransitionStamps proves that when the condition status
// actually changes (True→False), lastTransitionTime is updated to a new
// timestamp. This is the complement of TestConditionPreservesTransitionTime.
func TestConditionTransitionStamps(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Custom CRD for this test.
	group := uniqueGroup()
	crdName := fmt.Sprintf("transwidgets.%s", group)
	crd := buildCustomCRD(crdName, group, "TransWidget", "transwidgets")
	require.NoError(t, k8sClient.Create(ctx, crd))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, crd) })
	require.NoError(t, waitForCRD(ctx, k8sClient, crdName))

	crGVK := schema.GroupVersionKind{Group: group, Version: "v1alpha1", Kind: "TransWidget"}

	// Control ConfigMap drives the condition status.
	control := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "trans-control",
				"namespace": ns,
			},
			"data": map[string]any{
				"ready": "true",
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, control))

	// Instance of the custom resource.
	instance := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": group + "/v1alpha1",
			"kind":       "TransWidget",
			"metadata": map[string]any{
				"name":      "trans-instance",
				"namespace": ns,
			},
			"spec": map[string]any{},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, instance))

	// Graph: condition status depends on control ConfigMap value.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-condition-transition",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "control",
						"ref": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "trans-control"},
						},
					},
					map[string]any{
						"id": "target",
						"ref": map[string]any{
							"apiVersion": group + "/v1alpha1",
							"kind":       "TransWidget",
							"metadata":   map[string]any{"name": "trans-instance"},
						},
					},
					map[string]any{
						"id": "status",
						"patch": map[string]any{
							"apiVersion": group + "/v1alpha1",
							"kind":       "TransWidget",
							"metadata":   map[string]any{"name": "trans-instance"},
							"status": map[string]any{
								"conditions": "${[target.condition('Ready', control.data.ready == 'true' ? 'True' : 'False', 'ControlDriven', 'driven by control')]}",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-condition-transition", Namespace: ns}))

	// Wait for initial condition to settle.
	require.NoError(t, waitForSettle(ctx, k8sClient, crGVK,
		types.NamespacedName{Name: "trans-instance", Namespace: ns}))

	// Record T1 (status=True).
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(crGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "trans-instance", Namespace: ns}, obj))
	conditions, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	require.NotEmpty(t, conditions)
	cond1 := conditions[0].(map[string]any)
	assert.Equal(t, "True", cond1["status"])
	t1Str := cond1["lastTransitionTime"].(string)
	t1, err := time.Parse(time.RFC3339, t1Str)
	require.NoError(t, err)
	t.Logf("T1: status=True, lastTransitionTime=%s", t1Str)

	// Small delay to ensure T2 > T1 even at second granularity.
	time.Sleep(1100 * time.Millisecond)

	// Flip status: update control to ready=false.
	require.NoError(t, updateWithRetry(ctx, k8sClient, cmGVK,
		types.NamespacedName{Name: "trans-control", Namespace: ns},
		func(obj *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(obj.Object, "false", "data", "ready")
		}))

	// Wait for condition status to flip to False.
	require.NoError(t, waitForConditionStatus(ctx, k8sClient, crGVK,
		types.NamespacedName{Name: "trans-instance", Namespace: ns}, "Ready", "False"))

	// Read T2 and assert it's newer than T1.
	obj2 := &unstructured.Unstructured{}
	obj2.SetGroupVersionKind(crGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "trans-instance", Namespace: ns}, obj2))
	conditions2, _, _ := unstructured.NestedSlice(obj2.Object, "status", "conditions")
	require.NotEmpty(t, conditions2)
	cond2 := conditions2[0].(map[string]any)
	assert.Equal(t, "False", cond2["status"])
	t2Str := cond2["lastTransitionTime"].(string)
	t2, err := time.Parse(time.RFC3339, t2Str)
	require.NoError(t, err)

	assert.True(t, t2.After(t1),
		"lastTransitionTime must advance on status change: T1=%s T2=%s", t1Str, t2Str)
	t.Logf("Transition proved: T1=%s (True) → T2=%s (False)", t1Str, t2Str)
}

// ---------------------------------------------------------------------------
// Test-local helpers
// ---------------------------------------------------------------------------

// waitForConditionStatus polls until a resource's status.conditions contains
// a condition with the given type and status value.
func waitForConditionStatus(ctx context.Context, c client.Client, gvk schema.GroupVersionKind, key types.NamespacedName, condType, wantStatus string) error {
	return wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(gvk)
		if err := c.Get(ctx, key, obj); err != nil {
			return false, nil
		}
		conditions, found, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
		if !found {
			return false, nil
		}
		for _, c := range conditions {
			cMap, ok := c.(map[string]any)
			if !ok {
				continue
			}
			if cMap["type"] == condType && cMap["status"] == wantStatus {
				return true, nil
			}
		}
		return false, nil
	})
}

// TestConditionOnMissingStatus proves that .condition() gracefully handles
// the case where status.conditions is nil/absent — the condition is created
// fresh with a valid lastTransitionTime stamped at time.Now().
func TestConditionOnMissingStatus(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Create a custom CRD with status subresource.
	group := uniqueGroup()
	crdName := fmt.Sprintf("missingwidgets.%s", group)
	crd := buildCustomCRD(crdName, group, "MissingWidget", "missingwidgets")
	require.NoError(t, k8sClient.Create(ctx, crd))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, crd) })
	require.NoError(t, waitForCRD(ctx, k8sClient, crdName))

	crGVK := schema.GroupVersionKind{Group: group, Version: "v1alpha1", Kind: "MissingWidget"}

	// Create a fresh instance — NO status.conditions set (completely empty status).
	instance := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": group + "/v1alpha1",
			"kind":       "MissingWidget",
			"metadata": map[string]any{
				"name":      "missing-instance",
				"namespace": ns,
			},
			"spec": map[string]any{},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, instance))

	// Graph: ref the instance, patch its status with .condition().
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-condition-missing",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "target",
						"ref": map[string]any{
							"apiVersion": group + "/v1alpha1",
							"kind":       "MissingWidget",
							"metadata":   map[string]any{"name": "missing-instance"},
						},
					},
					map[string]any{
						"id": "status",
						"patch": map[string]any{
							"apiVersion": group + "/v1alpha1",
							"kind":       "MissingWidget",
							"metadata":   map[string]any{"name": "missing-instance"},
							"status": map[string]any{
								"conditions": "${[target.condition('Initialized', 'True', 'Fresh', 'first time')]}",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for condition to appear.
	require.NoError(t, waitForConditionStatus(ctx, k8sClient, crGVK,
		types.NamespacedName{Name: "missing-instance", Namespace: ns}, "Initialized", "True"))

	// Read the instance and verify the condition fields.
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(crGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "missing-instance", Namespace: ns}, obj))

	conditions, found, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	require.True(t, found, "instance should have status.conditions")
	require.Len(t, conditions, 1)

	cond := conditions[0].(map[string]any)
	assert.Equal(t, "Initialized", cond["type"])
	assert.Equal(t, "True", cond["status"])
	assert.Equal(t, "Fresh", cond["reason"])
	assert.Equal(t, "first time", cond["message"])

	// lastTransitionTime must be a valid RFC3339 timestamp (stamped fresh since no prior condition existed).
	ltt, _ := cond["lastTransitionTime"].(string)
	parsedLTT, err := time.Parse(time.RFC3339, ltt)
	assert.NoError(t, err, "lastTransitionTime should be valid RFC3339, got: %s", ltt)
	// Should be recent (within last 60s).
	assert.WithinDuration(t, time.Now().UTC(), parsedLTT, 60*time.Second,
		"lastTransitionTime should be recent")
	t.Logf("Condition on missing status proved: type=%s status=%s ltt=%s", cond["type"], cond["status"], ltt)
}

// TestConditionRapidTransitions proves that each status transition stamps a
// new lastTransitionTime, even during rapid back-and-forth transitions.
func TestConditionRapidTransitions(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Create a custom CRD with status subresource.
	group := uniqueGroup()
	crdName := fmt.Sprintf("rapidwidgets.%s", group)
	crd := buildCustomCRD(crdName, group, "RapidWidget", "rapidwidgets")
	require.NoError(t, k8sClient.Create(ctx, crd))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, crd) })
	require.NoError(t, waitForCRD(ctx, k8sClient, crdName))

	crGVK := schema.GroupVersionKind{Group: group, Version: "v1alpha1", Kind: "RapidWidget"}

	// Control ConfigMap drives the condition status.
	control := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "rapid-control",
				"namespace": ns,
			},
			"data": map[string]any{
				"ready": "true",
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, control))

	// Instance of the custom resource.
	instance := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": group + "/v1alpha1",
			"kind":       "RapidWidget",
			"metadata": map[string]any{
				"name":      "rapid-instance",
				"namespace": ns,
			},
			"spec": map[string]any{},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, instance))

	// Graph: condition status depends on control ConfigMap value.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-condition-rapid",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "control",
						"ref": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "rapid-control"},
						},
					},
					map[string]any{
						"id": "target",
						"ref": map[string]any{
							"apiVersion": group + "/v1alpha1",
							"kind":       "RapidWidget",
							"metadata":   map[string]any{"name": "rapid-instance"},
						},
					},
					map[string]any{
						"id": "status",
						"patch": map[string]any{
							"apiVersion": group + "/v1alpha1",
							"kind":       "RapidWidget",
							"metadata":   map[string]any{"name": "rapid-instance"},
							"status": map[string]any{
								"conditions": "${[target.condition('Ready', control.data.ready == 'true' ? 'True' : 'False', 'Controlled', 'msg')]}",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for initial condition (status=True).
	require.NoError(t, waitForConditionStatus(ctx, k8sClient, crGVK,
		types.NamespacedName{Name: "rapid-instance", Namespace: ns}, "Ready", "True"))

	// Record T1.
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(crGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "rapid-instance", Namespace: ns}, obj))
	conditions, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	require.NotEmpty(t, conditions)
	t1Str := conditions[0].(map[string]any)["lastTransitionTime"].(string)
	t1, err := time.Parse(time.RFC3339, t1Str)
	require.NoError(t, err)
	t.Logf("T1: status=True, lastTransitionTime=%s", t1Str)

	// Sleep to ensure RFC3339 second-granularity difference.
	time.Sleep(1100 * time.Millisecond)

	// Flip control to ready=false.
	require.NoError(t, updateWithRetry(ctx, k8sClient, cmGVK,
		types.NamespacedName{Name: "rapid-control", Namespace: ns},
		func(obj *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(obj.Object, "false", "data", "ready")
		}))

	// Wait for status=False.
	require.NoError(t, waitForConditionStatus(ctx, k8sClient, crGVK,
		types.NamespacedName{Name: "rapid-instance", Namespace: ns}, "Ready", "False"))

	// Record T2.
	obj2 := &unstructured.Unstructured{}
	obj2.SetGroupVersionKind(crGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "rapid-instance", Namespace: ns}, obj2))
	conditions2, _, _ := unstructured.NestedSlice(obj2.Object, "status", "conditions")
	require.NotEmpty(t, conditions2)
	t2Str := conditions2[0].(map[string]any)["lastTransitionTime"].(string)
	t2, err := time.Parse(time.RFC3339, t2Str)
	require.NoError(t, err)
	assert.True(t, t2.After(t1), "T2 must be after T1: T1=%s T2=%s", t1Str, t2Str)
	t.Logf("T2: status=False, lastTransitionTime=%s", t2Str)

	// Sleep again.
	time.Sleep(1100 * time.Millisecond)

	// Flip control back to ready=true.
	require.NoError(t, updateWithRetry(ctx, k8sClient, cmGVK,
		types.NamespacedName{Name: "rapid-control", Namespace: ns},
		func(obj *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(obj.Object, "true", "data", "ready")
		}))

	// Wait for status=True again.
	require.NoError(t, waitForConditionStatus(ctx, k8sClient, crGVK,
		types.NamespacedName{Name: "rapid-instance", Namespace: ns}, "Ready", "True"))

	// Record T3.
	obj3 := &unstructured.Unstructured{}
	obj3.SetGroupVersionKind(crGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "rapid-instance", Namespace: ns}, obj3))
	conditions3, _, _ := unstructured.NestedSlice(obj3.Object, "status", "conditions")
	require.NotEmpty(t, conditions3)
	t3Str := conditions3[0].(map[string]any)["lastTransitionTime"].(string)
	t3, err := time.Parse(time.RFC3339, t3Str)
	require.NoError(t, err)
	assert.True(t, t3.After(t2), "T3 must be after T2: T2=%s T3=%s", t2Str, t3Str)
	t.Logf("T3: status=True, lastTransitionTime=%s — rapid transitions proved", t3Str)
}

// TestTimeNowRawValueReconcilesOnExternalChange proves that raw time.now()
// re-stamps on external events (the value is not stuck at the first evaluation).
func TestTimeNowRawValueReconcilesOnExternalChange(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Pre-create "trigger" ConfigMap.
	trigger := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "trigger",
				"namespace": ns,
			},
			"data": map[string]any{
				"version": "v1",
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, trigger))

	// Graph: ref node watching trigger, template node writing time.now() into annotation of output.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-time-raw-reconc",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "trigger",
						"ref": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "trigger"},
						},
					},
					map[string]any{
						"id": "output",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "output",
								"annotations": map[string]any{
									"timestamp": "${string(time.now())}",
								},
							},
							"data": map[string]any{
								"trigger-version": "${trigger.data.version}",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for "output" to appear.
	outputCM := &unstructured.Unstructured{}
	outputCM.SetGroupVersionKind(cmGVK)
	require.NoError(t, waitForResource(ctx, k8sClient,
		types.NamespacedName{Name: "output", Namespace: ns}, outputCM))

	// Record timestamp T1.
	annotations := outputCM.GetAnnotations()
	require.Contains(t, annotations, "timestamp")
	t1Str := annotations["timestamp"]
	t1, err := time.Parse(time.RFC3339, t1Str)
	require.NoError(t, err, "T1 should be valid RFC3339, got: %s", t1Str)
	t.Logf("T1: %s", t1Str)

	// Sleep to ensure second-granularity difference.
	time.Sleep(1100 * time.Millisecond)

	// Update "trigger" ConfigMap — this should trigger re-reconciliation.
	require.NoError(t, updateWithRetry(ctx, k8sClient, cmGVK,
		types.NamespacedName{Name: "trigger", Namespace: ns},
		func(obj *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(obj.Object, "v2", "data", "version")
		}))

	// Wait for the annotation timestamp to change (T2 > T1).
	err = wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(cmGVK)
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "output", Namespace: ns}, obj); err != nil {
			return false, nil
		}
		ann := obj.GetAnnotations()
		if ann == nil {
			return false, nil
		}
		ts := ann["timestamp"]
		if ts == t1Str {
			return false, nil
		}
		// Verify it's a valid timestamp that's newer.
		parsed, parseErr := time.Parse(time.RFC3339, ts)
		if parseErr != nil {
			return false, nil
		}
		return parsed.After(t1), nil
	})
	require.NoError(t, err, "annotation timestamp should advance after trigger update")

	// Read final state and assert.
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "output", Namespace: ns}, obj))
	t2Str := obj.GetAnnotations()["timestamp"]
	t2, err := time.Parse(time.RFC3339, t2Str)
	require.NoError(t, err)
	assert.True(t, t2.After(t1), "T2 must be after T1: T1=%s T2=%s", t1Str, t2Str)
	t.Logf("Raw time.now() re-evaluation proved: T1=%s → T2=%s", t1Str, t2Str)
}

// TestConditionWithTimeGate combines both features: .condition() status driven
// by a time comparison. Initially the condition is False (time hasn't elapsed),
// then after ~4s it flips to True via time solving inside .condition() arguments.
func TestConditionWithTimeGate(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Create a custom CRD with status subresource.
	group := uniqueGroup()
	crdName := fmt.Sprintf("bakewidgets.%s", group)
	crd := buildCustomCRD(crdName, group, "BakeWidget", "bakewidgets")
	require.NoError(t, k8sClient.Create(ctx, crd))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, crd) })
	require.NoError(t, waitForCRD(ctx, k8sClient, crdName))

	crGVK := schema.GroupVersionKind{Group: group, Version: "v1alpha1", Kind: "BakeWidget"}

	// Pre-create a ConfigMap with startedAt = now.
	now := time.Now().UTC().Format(time.RFC3339)
	startCM := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "bake-start",
				"namespace": ns,
			},
			"data": map[string]any{
				"startedAt": now,
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, startCM))

	// Create an instance of the custom resource.
	instance := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": group + "/v1alpha1",
			"kind":       "BakeWidget",
			"metadata": map[string]any{
				"name":      "bake-instance",
				"namespace": ns,
			},
			"spec": map[string]any{},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, instance))

	// Graph: ref ConfigMap, ref CR instance, patch condition with time gate.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-condition-timegate",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "source",
						"ref": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "bake-start"},
						},
					},
					map[string]any{
						"id": "target",
						"ref": map[string]any{
							"apiVersion": group + "/v1alpha1",
							"kind":       "BakeWidget",
							"metadata":   map[string]any{"name": "bake-instance"},
						},
					},
					map[string]any{
						"id": "status",
						"patch": map[string]any{
							"apiVersion": group + "/v1alpha1",
							"kind":       "BakeWidget",
							"metadata":   map[string]any{"name": "bake-instance"},
							"status": map[string]any{
								"conditions": "${[target.condition('Baked', time.now() - timestamp(source.data.startedAt) >= duration('4s') ? 'True' : 'False', 'BakeTime', 'waiting for bake period')]}",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Initially condition should be status=False (time hasn't elapsed).
	require.NoError(t, waitForConditionStatus(ctx, k8sClient, crGVK,
		types.NamespacedName{Name: "bake-instance", Namespace: ns}, "Baked", "False"))

	// Record T1 (False state).
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(crGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "bake-instance", Namespace: ns}, obj))
	conditions, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	require.NotEmpty(t, conditions)
	t1Str := conditions[0].(map[string]any)["lastTransitionTime"].(string)
	t1, err := time.Parse(time.RFC3339, t1Str)
	require.NoError(t, err)
	t.Logf("T1: status=False, lastTransitionTime=%s", t1Str)

	// Wait for condition with status=True (should happen after ~4s via time solving).
	require.NoError(t, waitForConditionStatus(ctx, k8sClient, crGVK,
		types.NamespacedName{Name: "bake-instance", Namespace: ns}, "Baked", "True"),
		"condition should flip to True after ~4s bake period")

	// Record T2 and assert lastTransitionTime advanced.
	obj2 := &unstructured.Unstructured{}
	obj2.SetGroupVersionKind(crGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "bake-instance", Namespace: ns}, obj2))
	conditions2, _, _ := unstructured.NestedSlice(obj2.Object, "status", "conditions")
	require.NotEmpty(t, conditions2)
	cond2 := conditions2[0].(map[string]any)
	assert.Equal(t, "True", cond2["status"])
	assert.Equal(t, "Baked", cond2["type"])
	assert.Equal(t, "BakeTime", cond2["reason"])
	assert.Equal(t, "waiting for bake period", cond2["message"])

	t2Str := cond2["lastTransitionTime"].(string)
	t2, err := time.Parse(time.RFC3339, t2Str)
	require.NoError(t, err)
	assert.True(t, t2.After(t1),
		"lastTransitionTime must advance on False→True: T1=%s T2=%s", t1Str, t2Str)
	t.Logf("Time gate + condition proved: T1=%s (False) → T2=%s (True)", t1Str, t2Str)
}

// ═══════════════════════════════════════════════════════════════════════════════
// Additional time.now() and .condition() tests
// ═══════════════════════════════════════════════════════════════════════════════

// TestTimeNowWithSoftDependency proves that time.now() comparisons work with
// optional/soft dependencies without panicking. When the source doesn't exist,
// the orValue provides a very old timestamp, so the gate is immediately true.
func TestTimeNowWithSoftDependency(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Graph: ref node watches a ConfigMap that doesn't exist.
	// The template uses optional chaining with orValue to provide a fallback.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-time-soft-dep",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "source",
						"ref": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "soft-source"},
						},
					},
					map[string]any{
						"id":            "gated",
						"propagateWhen": []any{"${time.now() - timestamp(source.?data.?createdAt.orValue('2000-01-01T00:00:00Z')) >= duration('3s')}"},
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "soft-output"},
							"data":       map[string]any{"result": "passed"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// The orValue is year 2000, so time.now() - that >= 3s is immediately true.
	// Assert "soft-output" appears within 5s.
	cm := &unstructured.Unstructured{}
	cm.SetGroupVersionKind(cmGVK)
	require.NoError(t, waitForResource(ctx, k8sClient,
		types.NamespacedName{Name: "soft-output", Namespace: ns}, cm, 5*time.Second),
		"soft-output must appear quickly since orValue provides an old timestamp")

	data, _, _ := unstructured.NestedString(cm.Object, "data", "result")
	assert.Equal(t, "passed", data)
	t.Log("time.now() with soft/optional dependency proved: gate passed immediately with orValue fallback")
}

// TestTimeNowDeterministicWithinReconcile proves that time.now() is captured
// once per reconcile cycle and returns the same value across all nodes.
func TestTimeNowDeterministicWithinReconcile(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Graph: two template nodes both writing ${string(time.now())} as annotations.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-time-deterministic",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "first",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "first-stamp",
								"annotations": map[string]any{
									"ts": "${string(time.now())}",
								},
							},
							"data": map[string]any{"placeholder": "a"},
						},
					},
					map[string]any{
						"id": "second",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "second-stamp",
								"annotations": map[string]any{
									"ts": "${string(time.now())}",
								},
							},
							"data": map[string]any{"placeholder": "b"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for both ConfigMaps to appear.
	first := &unstructured.Unstructured{}
	first.SetGroupVersionKind(cmGVK)
	require.NoError(t, waitForResource(ctx, k8sClient,
		types.NamespacedName{Name: "first-stamp", Namespace: ns}, first))

	second := &unstructured.Unstructured{}
	second.SetGroupVersionKind(cmGVK)
	require.NoError(t, waitForResource(ctx, k8sClient,
		types.NamespacedName{Name: "second-stamp", Namespace: ns}, second))

	// Read annotations and assert they are the same timestamp.
	firstTS := first.GetAnnotations()["ts"]
	secondTS := second.GetAnnotations()["ts"]
	require.NotEmpty(t, firstTS, "first-stamp must have ts annotation")
	require.NotEmpty(t, secondTS, "second-stamp must have ts annotation")

	assert.Equal(t, firstTS, secondTS,
		"time.now() must be deterministic within a reconcile: first=%s second=%s", firstTS, secondTS)
	t.Logf("Deterministic time.now() proved: both nodes got %s", firstTS)
}

// TestConditionMultipleTypes proves that .condition() can construct multiple
// independent conditions with different types, statuses, reasons, and messages.
func TestConditionMultipleTypes(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Custom CRD for this test.
	group := uniqueGroup()
	crdName := fmt.Sprintf("multiwidgets.%s", group)
	crd := buildCustomCRD(crdName, group, "MultiWidget", "multiwidgets")
	require.NoError(t, k8sClient.Create(ctx, crd))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, crd) })
	require.NoError(t, waitForCRD(ctx, k8sClient, crdName))

	crGVK := schema.GroupVersionKind{Group: group, Version: "v1alpha1", Kind: "MultiWidget"}

	// Create an instance of the custom resource.
	instance := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": group + "/v1alpha1",
			"kind":       "MultiWidget",
			"metadata": map[string]any{
				"name":      "multi-instance",
				"namespace": ns,
			},
			"spec": map[string]any{},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, instance))

	// Graph: ref the instance, patch status with TWO conditions.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-condition-multi",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "target",
						"ref": map[string]any{
							"apiVersion": group + "/v1alpha1",
							"kind":       "MultiWidget",
							"metadata":   map[string]any{"name": "multi-instance"},
						},
					},
					map[string]any{
						"id": "status",
						"patch": map[string]any{
							"apiVersion": group + "/v1alpha1",
							"kind":       "MultiWidget",
							"metadata":   map[string]any{"name": "multi-instance"},
							"status": map[string]any{
								"conditions": "${[target.condition('Ready', 'True', 'AllGood', 'ready message'), target.condition('DatabaseReady', 'False', 'Connecting', 'db message')]}",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-condition-multi", Namespace: ns}))

	// Wait for settle.
	require.NoError(t, waitForSettle(ctx, k8sClient, crGVK,
		types.NamespacedName{Name: "multi-instance", Namespace: ns}))

	// Read the instance and verify both conditions.
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(crGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "multi-instance", Namespace: ns}, obj))

	conditions, found, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	require.True(t, found, "instance should have status.conditions")
	require.Len(t, conditions, 2, "should have exactly 2 conditions")

	// Build a map by type for easier assertions.
	condByType := map[string]map[string]any{}
	for _, c := range conditions {
		cMap := c.(map[string]any)
		condByType[cMap["type"].(string)] = cMap
	}

	// Assert Ready condition.
	ready, ok := condByType["Ready"]
	require.True(t, ok, "must have Ready condition")
	assert.Equal(t, "True", ready["status"])
	assert.Equal(t, "AllGood", ready["reason"])
	assert.Equal(t, "ready message", ready["message"])
	readyLTT, _ := ready["lastTransitionTime"].(string)
	_, err := time.Parse(time.RFC3339, readyLTT)
	assert.NoError(t, err, "Ready lastTransitionTime should be valid RFC3339, got: %s", readyLTT)

	// Assert DatabaseReady condition.
	dbReady, ok := condByType["DatabaseReady"]
	require.True(t, ok, "must have DatabaseReady condition")
	assert.Equal(t, "False", dbReady["status"])
	assert.Equal(t, "Connecting", dbReady["reason"])
	assert.Equal(t, "db message", dbReady["message"])
	dbLTT, _ := dbReady["lastTransitionTime"].(string)
	_, err = time.Parse(time.RFC3339, dbLTT)
	assert.NoError(t, err, "DatabaseReady lastTransitionTime should be valid RFC3339, got: %s", dbLTT)

	// Assert they are tracked independently (different statuses prove this).
	assert.NotEqual(t, ready["status"], dbReady["status"],
		"conditions should have different statuses proving independent tracking")
	t.Logf("Multiple condition types proved: Ready=%s/%s, DatabaseReady=%s/%s",
		ready["status"], readyLTT, dbReady["status"], dbLTT)
}

// TestConditionObservedGenerationUpdates proves that .condition() reads the
// current metadata.generation. When the spec changes (generation bumps),
// the condition's observedGeneration updates accordingly.
func TestConditionObservedGenerationUpdates(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Custom CRD for this test.
	group := uniqueGroup()
	crdName := fmt.Sprintf("genwidgets.%s", group)
	crd := buildCustomCRD(crdName, group, "GenWidget", "genwidgets")
	require.NoError(t, k8sClient.Create(ctx, crd))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, crd) })
	require.NoError(t, waitForCRD(ctx, k8sClient, crdName))

	crGVK := schema.GroupVersionKind{Group: group, Version: "v1alpha1", Kind: "GenWidget"}

	// Create an instance (generation=1).
	instance := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": group + "/v1alpha1",
			"kind":       "GenWidget",
			"metadata": map[string]any{
				"name":      "gen-instance",
				"namespace": ns,
			},
			"spec": map[string]any{
				"someField": "initial",
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, instance))

	// Graph: ref the instance, patch status with .condition().
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-condition-gen",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "target",
						"ref": map[string]any{
							"apiVersion": group + "/v1alpha1",
							"kind":       "GenWidget",
							"metadata":   map[string]any{"name": "gen-instance"},
						},
					},
					map[string]any{
						"id": "status",
						"patch": map[string]any{
							"apiVersion": group + "/v1alpha1",
							"kind":       "GenWidget",
							"metadata":   map[string]any{"name": "gen-instance"},
							"status": map[string]any{
								"conditions": "${[target.condition('Ready', 'True', 'Updated', 'msg')]}",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-condition-gen", Namespace: ns}))

	// Wait for condition to appear and record observedGeneration.
	key := types.NamespacedName{Name: "gen-instance", Namespace: ns}
	require.NoError(t, waitForConditionStatus(ctx, k8sClient, crGVK, key, "Ready", "True"))

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(crGVK)
	require.NoError(t, k8sClient.Get(ctx, key, obj))
	conditions, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	require.NotEmpty(t, conditions)
	cond1 := conditions[0].(map[string]any)
	gen1 := getConditionObservedGeneration(cond1)
	assert.Equal(t, int64(1), gen1, "initial observedGeneration should be 1")
	t.Logf("Initial observedGeneration: %d", gen1)

	// Update the instance's spec to trigger a generation bump.
	updateWithRetry(ctx, k8sClient, crGVK, key, func(obj *unstructured.Unstructured) {
		_ = unstructured.SetNestedField(obj.Object, "updated", "spec", "someField")
	})

	// Wait for observedGeneration to become 2.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(crGVK)
		if err := k8sClient.Get(ctx, key, obj); err != nil {
			return false, nil
		}
		conditions, found, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
		if !found || len(conditions) == 0 {
			return false, nil
		}
		cond := conditions[0].(map[string]any)
		gen := getConditionObservedGeneration(cond)
		return gen == int64(2), nil
	}), "observedGeneration should update to 2 after spec change")

	// Final verification.
	obj2 := &unstructured.Unstructured{}
	obj2.SetGroupVersionKind(crGVK)
	require.NoError(t, k8sClient.Get(ctx, key, obj2))
	conditions2, _, _ := unstructured.NestedSlice(obj2.Object, "status", "conditions")
	cond2 := conditions2[0].(map[string]any)
	gen2 := getConditionObservedGeneration(cond2)
	assert.Equal(t, int64(2), gen2, "observedGeneration must be 2 after spec update")
	t.Logf("ObservedGeneration updated: %d → %d (proved generation tracking)", gen1, gen2)
}

// getConditionObservedGeneration extracts observedGeneration from a condition
// map, handling both int64 and float64 (JSON round-trip) representations.
func getConditionObservedGeneration(cond map[string]any) int64 {
	if v, ok := cond["observedGeneration"].(int64); ok {
		return v
	}
	if v, ok := cond["observedGeneration"].(float64); ok {
		return int64(v)
	}
	return 0
}

// ═══════════════════════════════════════════════════════════════════════════════
// Additional time.now() gate tests
// ═══════════════════════════════════════════════════════════════════════════════

// TestTimeNowGateAlreadyPassed proves that when the time threshold is already
// in the past at Graph creation, the gated node appears immediately — kro
// doesn't wait unnecessarily.
func TestTimeNowGateAlreadyPassed(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Pre-create source with time 10 minutes in the past — gate is already satisfied.
	pastTime := time.Now().UTC().Add(-10 * time.Minute).Format(time.RFC3339)
	source := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "already-passed-source",
				"namespace": ns,
			},
			"data": map[string]any{
				"createdAt": pastTime,
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, source))

	// Graph: ref source, gate a template on time.now() >= createdAt + 5s.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-time-already-passed",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "source",
						"ref": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "already-passed-source"},
						},
					},
					map[string]any{
						"id":            "gated",
						"propagateWhen": []any{"${time.now() - timestamp(source.data.createdAt) >= duration('5s')}"},
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "already-passed-output"},
							"data":       map[string]any{"status": "immediate"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// The gated node should appear immediately (within 5s) since the threshold
	// is already 10 minutes in the past.
	output := &unstructured.Unstructured{}
	output.SetGroupVersionKind(cmGVK)
	require.NoError(t, waitForResource(ctx, k8sClient,
		types.NamespacedName{Name: "already-passed-output", Namespace: ns}, output, 5*time.Second),
		"gated node must appear immediately when threshold is already passed")

	data, _, _ := unstructured.NestedString(output.Object, "data", "status")
	assert.Equal(t, "immediate", data)
	t.Log("Gate already passed: node appeared immediately without waiting")
}

// TestTimeNowGateResolves proves that when the source input changes to make
// a time gate true, kro re-evaluates and dispatches the gated node.
func TestTimeNowGateResolves(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Pre-create source with current time — gate will NOT be satisfied yet.
	nowTime := time.Now().UTC().Format(time.RFC3339)
	source := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "resolves-source",
				"namespace": ns,
			},
			"data": map[string]any{
				"createdAt": nowTime,
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, source))

	// Graph: ref source, gate on 4s threshold.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-time-resolves",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "source",
						"ref": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "resolves-source"},
						},
					},
					map[string]any{
						"id":            "gated",
						"propagateWhen": []any{"${time.now() - timestamp(source.data.createdAt) >= duration('4s')}"},
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "resolves-output"},
							"data":       map[string]any{"status": "resolved"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Confirm the node is absent for 2s (gate not yet satisfied).
	err := waitForAbsence(ctx, k8sClient, cmGVK,
		types.NamespacedName{Name: "resolves-output", Namespace: ns}, 2*time.Second)
	require.NoError(t, err, "gated node must not appear before gate resolves")
	t.Log("Confirmed: resolves-output absent during first 2s")

	// Update the source's createdAt to 30 seconds in the past — gate immediately true.
	pastTime := time.Now().UTC().Add(-30 * time.Second).Format(time.RFC3339)
	require.NoError(t, updateWithRetry(ctx, k8sClient, cmGVK,
		types.NamespacedName{Name: "resolves-source", Namespace: ns},
		func(obj *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(obj.Object, pastTime, "data", "createdAt")
		}))

	// The gated node should appear within 3s (kro re-evaluates on source change).
	output := &unstructured.Unstructured{}
	output.SetGroupVersionKind(cmGVK)
	require.NoError(t, waitForResource(ctx, k8sClient,
		types.NamespacedName{Name: "resolves-output", Namespace: ns}, output, 3*time.Second),
		"gated node must appear after source update makes gate true")

	data, _, _ := unstructured.NestedString(output.Object, "data", "status")
	assert.Equal(t, "resolved", data)
	t.Log("Gate resolves on source update: node appeared after input change")
}

// TestTimeNowMultipleGates proves that multiple nodes with different time
// gates resolve independently — the faster gate fires first, the slower
// gate fires later.
func TestTimeNowMultipleGates(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Pre-create source with current time.
	nowTime := time.Now().UTC().Format(time.RFC3339)
	source := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "multi-gate-source",
				"namespace": ns,
			},
			"data": map[string]any{
				"createdAt": nowTime,
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, source))

	// Graph: two gated nodes with different thresholds (3s and 7s).
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-time-multi-gates",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "source",
						"ref": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "multi-gate-source"},
						},
					},
					map[string]any{
						"id":            "fast",
						"propagateWhen": []any{"${time.now() - timestamp(source.data.createdAt) >= duration('3s')}"},
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "fast-output"},
							"data":       map[string]any{"gate": "fast"},
						},
					},
					map[string]any{
						"id":            "slow",
						"propagateWhen": []any{"${time.now() - timestamp(source.data.createdAt) >= duration('7s')}"},
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "slow-output"},
							"data":       map[string]any{"gate": "slow"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Fast gate should fire within 6s.
	fastOutput := &unstructured.Unstructured{}
	fastOutput.SetGroupVersionKind(cmGVK)
	require.NoError(t, waitForResource(ctx, k8sClient,
		types.NamespacedName{Name: "fast-output", Namespace: ns}, fastOutput, 6*time.Second),
		"fast gate (3s) must fire within 6s")
	t.Log("Fast gate fired")

	// Slow gate should still be absent for 5s after fast appeared.
	err := waitForAbsence(ctx, k8sClient, cmGVK,
		types.NamespacedName{Name: "slow-output", Namespace: ns}, 2*time.Second)
	require.NoError(t, err, "slow gate must not fire before its threshold")
	t.Log("Confirmed: slow-output still absent after fast appeared")

	// Slow gate should fire within 10s from test start.
	slowOutput := &unstructured.Unstructured{}
	slowOutput.SetGroupVersionKind(cmGVK)
	require.NoError(t, waitForResource(ctx, k8sClient,
		types.NamespacedName{Name: "slow-output", Namespace: ns}, slowOutput, 10*time.Second),
		"slow gate (7s) must fire within 10s")

	fastData, _, _ := unstructured.NestedString(fastOutput.Object, "data", "gate")
	slowData, _, _ := unstructured.NestedString(slowOutput.Object, "data", "gate")
	assert.Equal(t, "fast", fastData)
	assert.Equal(t, "slow", slowData)
	t.Log("Multiple gates proved: each resolved independently at its own threshold")
}

// TestTimeNowInReadyWhen proves that time.now() works in readyWhen context.
// readyWhen doesn't gate node creation — the node appears immediately — but
// the Graph only becomes Ready once the readyWhen condition is satisfied.
func TestTimeNowInReadyWhen(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Pre-create source with current time.
	nowTime := time.Now().UTC().Format(time.RFC3339)
	source := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "readywhen-source",
				"namespace": ns,
			},
			"data": map[string]any{
				"createdAt": nowTime,
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, source))

	// Graph: ref source, template with readyWhen time gate (4s).
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-time-readywhen",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "source",
						"ref": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "readywhen-source"},
						},
					},
					map[string]any{
						"id":        "output",
						"readyWhen": []any{"${time.now() - timestamp(source.data.createdAt) >= duration('4s')}"},
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "readywhen-output"},
							"data":       map[string]any{"status": "created"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// readyWhen doesn't gate creation — the output ConfigMap should appear immediately.
	output := &unstructured.Unstructured{}
	output.SetGroupVersionKind(cmGVK)
	require.NoError(t, waitForResource(ctx, k8sClient,
		types.NamespacedName{Name: "readywhen-output", Namespace: ns}, output, 5*time.Second),
		"readyWhen must not gate node creation — output should appear immediately")
	t.Log("Output created immediately (readyWhen doesn't gate creation)")

	data, _, _ := unstructured.NestedString(output.Object, "data", "status")
	assert.Equal(t, "created", data)

	// The Graph should become Ready after ~4s when the readyWhen condition flips.
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-time-readywhen", Namespace: ns}, 10*time.Second),
		"Graph must become Ready after readyWhen time threshold passes")
	t.Log("Graph became Ready after time threshold — time.now() in readyWhen proved")
}
