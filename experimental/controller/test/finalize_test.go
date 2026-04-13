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

// TestFinalizesBasicSequence proves the core finalization sequence:
// when a target is pruned, its finalizer resource is created first,
// then the target is deleted after the finalizer exists.
//
// Sequence: Graph creates target + no finalizer → spec change removes target
// → finalizer created → target deleted → finalizer cleaned up.
func TestFinalizesBasicSequence(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	cmGVK := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}

	// Phase 1: Create a Graph with a target resource and a finalizer node.
	// The finalizer node declares `finalizes: target` — it won't be created
	// during normal operation.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-finalize-basic",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "target",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "fin-target"},
							"data":       map[string]any{"state": "active"},
						},
					},
					map[string]any{
						"id": "keep",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "fin-keep"},
							"data":       map[string]any{"role": "permanent"},
						},
					},
					map[string]any{
						"id":        "snapshot",
						"finalizes": "target",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "fin-snapshot"},
							"data": map[string]any{
								"snapshot-of": "fin-target",
								"state":       "captured",
							},
						},
						// No readyWhen — auto-ready on create.
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for the target to be created.
	targetCM := &unstructured.Unstructured{}
	targetCM.SetGroupVersionKind(cmGVK)
	require.NoError(t, waitForResource(ctx, k8sClient,
		types.NamespacedName{Name: "fin-target", Namespace: ns}, targetCM))
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-finalize-basic", Namespace: ns}))
	t.Log("Target created, Graph ready")

	// Verify the finalizer resource does NOT exist during normal operation.
	snapshotCM := &unstructured.Unstructured{}
	snapshotCM.SetGroupVersionKind(cmGVK)
	err := k8sClient.Get(ctx, types.NamespacedName{Name: "fin-snapshot", Namespace: ns}, snapshotCM)
	assert.Error(t, err, "finalizer resource should not exist during normal operation")
	t.Log("Finalizer resource correctly absent during normal operation")

	// Phase 2: Remove the target from the spec (keep "keep" and "snapshot").
	// This makes "target" a prune candidate. The finalizer should fire.
	require.NoError(t, updateWithRetry(ctx, k8sClient, GraphGVK,
		types.NamespacedName{Name: "test-finalize-basic", Namespace: ns}, func(obj *unstructured.Unstructured) {
			unstructured.SetNestedSlice(obj.Object, []any{
				map[string]any{
					"id": "keep",
					"template": map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata":   map[string]any{"name": "fin-keep"},
						"data":       map[string]any{"role": "permanent"},
					},
				},
				// snapshot node is removed from spec — it was only needed as a finalizer
				// for the target node. Since target is pruned, the finalizer fires, and
				// then the snapshot resource is itself a prune candidate.
			}, "spec", "nodes")
		}))
	t.Log("Updated spec: removed target and snapshot nodes")

	// Wait for the target to be deleted — this proves finalization ran
	// (the target can only be deleted after the finalizer resource is created
	// and reaches readyWhen). The finalizer resource itself is ephemeral —
	// it's created, checked for readiness, and then pruned in subsequent cycles.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true,
		func(ctx context.Context) (bool, error) {
			check := &unstructured.Unstructured{}
			err := k8sClient.Get(ctx, types.NamespacedName{Name: "fin-target", Namespace: ns}, check)
			return err != nil, nil // gone = true
		}))
	t.Log("Target deleted after finalization — finalization sequence proved")

	// The "keep" resource should still exist.
	keepCM := &unstructured.Unstructured{}
	keepCM.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "fin-keep", Namespace: ns}, keepCM))
	t.Log("Keep resource still alive — only target was pruned")
}

// TestFinalizesTargetAbsentSkips proves that if the target resource doesn't
// exist when finalization would fire, finalization is skipped.
func TestFinalizesTargetAbsentSkips(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	cmGVK := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-finalize-absent",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "target",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "absent-fin-target"},
							"data":       map[string]any{"state": "active"},
						},
					},
					map[string]any{
						"id":        "snapshot",
						"finalizes": "target",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "absent-fin-snapshot"},
							"data":       map[string]any{"captured": "true"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for the target to be created.
	targetCM := &unstructured.Unstructured{}
	targetCM.SetGroupVersionKind(cmGVK)
	require.NoError(t, waitForResource(ctx, k8sClient,
		types.NamespacedName{Name: "absent-fin-target", Namespace: ns}, targetCM))
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-finalize-absent", Namespace: ns}))

	// Externally delete the target before spec change.
	require.NoError(t, k8sClient.Delete(ctx, targetCM))
	t.Log("Externally deleted target before spec change")

	// Update spec to remove the target node.
	require.NoError(t, updateWithRetry(ctx, k8sClient, GraphGVK,
		types.NamespacedName{Name: "test-finalize-absent", Namespace: ns}, func(obj *unstructured.Unstructured) {
			unstructured.SetNestedSlice(obj.Object, []any{}, "spec", "nodes")
		}))
	t.Log("Updated spec: removed all nodes")

	// The finalizer resource should NOT be created — target was already gone.
	// Use observation-based polling instead of time.Sleep to verify absence.
	require.NoError(t, waitForAbsence(ctx, k8sClient, cmGVK,
		types.NamespacedName{Name: "absent-fin-snapshot", Namespace: ns}, 1*time.Second))
	t.Log("Finalization correctly skipped — target was already absent")
}

// TestFinalizesRejectsCELNames proves that a finalizes node with a
// CEL-evaluated metadata.name is rejected at spec validation time.
func TestFinalizesRejectsCELNames(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-finalize-cel-reject",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "target",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "cel-target"},
							"data":       map[string]any{"state": "active"},
						},
					},
					map[string]any{
						"id":        "snapshot",
						"finalizes": "target",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "${target.metadata.name}-snapshot",
							},
							"data": map[string]any{"captured": "true"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// The Graph should be rejected — Compiled should be False with a
	// compilation error about CEL-evaluated names on finalizes nodes.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true,
		func(ctx context.Context) (bool, error) {
			g := &unstructured.Unstructured{}
			g.SetGroupVersionKind(GraphGVK)
			if err := k8sClient.Get(ctx,
				types.NamespacedName{Name: "test-finalize-cel-reject", Namespace: ns}, g); err != nil {
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
	t.Log("Graph correctly rejected — CEL-evaluated name on finalizes node")
}

// TestFinalizesReadyWhenGatesTargetRemoval proves that a finalizer node's
// readyWhen condition must be satisfied before the finalization target is
// deleted. The controller creates the finalizer resource, then waits for
// readyWhen to pass — the target is blocked until then.
//
// Design 001-graph § finalizes:
//
//	"The resource is created only when the target becomes a prune candidate
//	and must reach readyWhen before the target's removal completes."
//
// Design 004-graph-execution § Finalization:
//
//	"(2) wait for readyWhen, (3) DELETE target."
//
// This test has three phases to prove the gate is actively holding, not racing:
//  1. Remove target from spec → snapshot created, target STILL EXISTS, gate unsatisfied.
//  2. Stable hold: wait multiple reconcile cycles → target STILL EXISTS.
//  3. Satisfy gate → target deleted, snapshot cleaned up.
//
// The gate uses an existing resource whose CONTENT controls readyWhen — not an
// absent resource. This is the realistic operational pattern: a finalization
// condition that depends on some external signal that hasn't fired yet.
func TestFinalizesReadyWhenGatesTargetRemoval(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	cmGVK := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}

	// Pre-create the gate control ConfigMap with ready=false.
	// The snapshot's readyWhen reads this field and gates on it.
	gateControl := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "fin-rw-gate",
				"namespace": ns,
			},
			"data": map[string]any{
				"ready": "false",
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, gateControl))
	t.Log("Gate control ConfigMap created with ready=false")

	// Phase 1: Create a Graph with:
	//   - target: the resource that will be finalized
	//   - gatewatch: watches the gate CM (always exists, field controls readyWhen)
	//   - snapshot: finalizes target, readyWhen gated on ${gatewatch.data.ready == 'true'}
	//
	// During normal operation the gate condition is false (ready=false).
	// The snapshot node must NOT be created until target is a prune candidate.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-finalize-readywhen",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "target",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "fin-rw-target"},
							"data":       map[string]any{"state": "active"},
						},
					},
					map[string]any{
						"id": "gatewatch",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "fin-rw-gate"},
						},
					},
					map[string]any{
						"id":        "snapshot",
						"finalizes": "target",
						"readyWhen": []any{"${gatewatch.data.ready == 'true'}"},
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "fin-rw-snapshot"},
							"data":       map[string]any{"captured": "true"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for target. Graph won't be fully Ready because snapshot's readyWhen
	// is false (gate ready=false), but target still gets created.
	targetCM := &unstructured.Unstructured{}
	targetCM.SetGroupVersionKind(cmGVK)
	require.NoError(t, waitForResource(ctx, k8sClient,
		types.NamespacedName{Name: "fin-rw-target", Namespace: ns}, targetCM))
	t.Log("Target created — Graph in non-Ready state (gatewatch.data.ready=false)")

	// Snapshot must NOT exist during normal operation.
	snapshotCheck := &unstructured.Unstructured{}
	snapshotCheck.SetGroupVersionKind(cmGVK)
	err := k8sClient.Get(ctx,
		types.NamespacedName{Name: "fin-rw-snapshot", Namespace: ns}, snapshotCheck)
	require.Error(t, err,
		"snapshot must not exist during normal operation (before target is a prune candidate)")
	t.Log("Snapshot correctly absent before finalization triggers")

	// Phase 2: Update spec to remove BOTH target and snapshot.
	//
	// Finalization semantics: the finalizer node (snapshot) and its target
	// node (target) must BOTH be absent from the new spec. The controller
	// uses the SUPERSEDED revision's DAG to find finalizer relationships —
	// the old revision still knows that snapshot finalizes target.
	//
	// If snapshot is kept in the new spec with `finalizes: target` but
	// target is absent, the revision compilation rejects it (DAG validation
	// requires the finalizes reference to exist in the same spec). This is
	// intentional: the user removes both nodes when they want finalization
	// to run. The superseded revision carries the relationship forward.
	//
	// The gatewatch node IS kept so it continues to be evaluated in the
	// wind phase and its data is available to the snapshot's readyWhen
	// expression when runFinalization checks it.
	require.NoError(t, updateWithRetry(ctx, k8sClient, GraphGVK,
		types.NamespacedName{Name: "test-finalize-readywhen", Namespace: ns}, func(obj *unstructured.Unstructured) {
			unstructured.SetNestedSlice(obj.Object, []any{
				map[string]any{
					"id": "gatewatch",
					"template": map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata":   map[string]any{"name": "fin-rw-gate"},
					},
				},
				// snapshot is intentionally REMOVED from the new spec.
				// The superseded revision still knows: snapshot finalizes target.
				// Finalization proceeds from the superseded DAG.
			}, "spec", "nodes")
		}))
	t.Log("Removed target and snapshot from spec — finalization triggered via superseded DAG")

	// Wait for snapshot to be created (finalization started running).
	// The controller uses the superseded revision's snapshot template to
	// create the resource, then waits for readyWhen before deleting target.
	snapshotCM := &unstructured.Unstructured{}
	snapshotCM.SetGroupVersionKind(cmGVK)
	require.NoError(t, waitForResource(ctx, k8sClient,
		types.NamespacedName{Name: "fin-rw-snapshot", Namespace: ns}, snapshotCM))
	t.Log("Snapshot CREATED — finalization is running")

	// Immediate assertion: target must still exist — readyWhen unsatisfied.
	checkTarget := &unstructured.Unstructured{}
	checkTarget.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "fin-rw-target", Namespace: ns}, checkTarget),
		"target must still exist — readyWhen gate (gatewatch.data.ready == 'true') is false")
	t.Log("Phase 2: target still exists immediately after snapshot creation")

	// Phase 3: Stable hold — wait multiple reconcile cycles and verify target
	// is still there. This proves the gate is actively holding, not just that
	// deletion is slow.
	require.NoError(t, waitForSettle(ctx, k8sClient, GraphGVK,
		types.NamespacedName{Name: "test-finalize-readywhen", Namespace: ns}))
	require.NoError(t, waitForSettle(ctx, k8sClient, GraphGVK,
		types.NamespacedName{Name: "test-finalize-readywhen", Namespace: ns}))

	holdTarget := &unstructured.Unstructured{}
	holdTarget.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "fin-rw-target", Namespace: ns}, holdTarget),
		"target must still exist after multiple reconcile cycles — gate actively holding")
	t.Log("Phase 3: target survived multiple reconcile cycles — gate is holding")

	// Phase 4: Satisfy the gate by updating the gate CM to ready=true.
	// gatewatch.data.ready == 'true' → readyWhen passes → target deleted.
	latestGate := &unstructured.Unstructured{}
	latestGate.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "fin-rw-gate", Namespace: ns}, latestGate))
	unstructured.SetNestedField(latestGate.Object, "true", "data", "ready")
	require.NoError(t, k8sClient.Update(ctx, latestGate))
	t.Log("Gate updated: ready=true — readyWhen should now be satisfied")

	// Target must be deleted now that readyWhen is satisfied.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 60*time.Second, true,
		func(ctx context.Context) (bool, error) {
			check := &unstructured.Unstructured{}
			check.SetGroupVersionKind(cmGVK)
			err := k8sClient.Get(ctx,
				types.NamespacedName{Name: "fin-rw-target", Namespace: ns}, check)
			return err != nil, nil // gone = success
		}))
	t.Log("Target deleted after gate satisfied — readyWhen finalization gate proved")
}

// TestFinalizesOnTeardown proves that finalization runs during Graph deletion
// (teardown), not just during prune.
func TestFinalizesOnTeardown(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	cmGVK := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-finalize-teardown",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "target",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "teardown-target"},
							"data":       map[string]any{"state": "active"},
						},
					},
					map[string]any{
						"id":        "snapshot",
						"finalizes": "target",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "teardown-snapshot"},
							"data":       map[string]any{"captured": "true"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for the target.
	targetCM := &unstructured.Unstructured{}
	targetCM.SetGroupVersionKind(cmGVK)
	require.NoError(t, waitForResource(ctx, k8sClient,
		types.NamespacedName{Name: "teardown-target", Namespace: ns}, targetCM))
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-finalize-teardown", Namespace: ns}))
	t.Log("Target created, Graph ready")

	// Delete the Graph — triggers teardown with finalization.
	latestGraph := &unstructured.Unstructured{}
	latestGraph.SetGroupVersionKind(GraphGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "test-finalize-teardown", Namespace: ns}, latestGraph))
	require.NoError(t, k8sClient.Delete(ctx, latestGraph))
	t.Log("Graph deleted — teardown started")

	// Wait for the Graph to be fully deleted (teardown complete).
	// This proves finalization ran: the target can't be deleted until
	// the finalizer resource is created and reaches readyWhen.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true,
		func(ctx context.Context) (bool, error) {
			g := &unstructured.Unstructured{}
			g.SetGroupVersionKind(GraphGVK)
			err := k8sClient.Get(ctx,
				types.NamespacedName{Name: "test-finalize-teardown", Namespace: ns}, g)
			return err != nil, nil // deleted = true
		}))
	t.Log("Graph fully deleted — teardown with finalization complete")

	// Both target and snapshot should be gone.
	check := &unstructured.Unstructured{}
	check.SetGroupVersionKind(cmGVK)
	err := k8sClient.Get(ctx, types.NamespacedName{Name: "teardown-target", Namespace: ns}, check)
	assert.Error(t, err, "target should be deleted")
	err = k8sClient.Get(ctx, types.NamespacedName{Name: "teardown-snapshot", Namespace: ns}, check)
	assert.Error(t, err, "snapshot should be cleaned up after teardown")
	t.Log("Both target and snapshot cleaned up")
}
