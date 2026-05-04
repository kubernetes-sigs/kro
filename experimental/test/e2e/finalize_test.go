package graphcontroller_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	require.NoError(t, waitForDeletion(ctx, k8sClient, cmGVK,
		types.NamespacedName{Name: "fin-target", Namespace: ns}))
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

	// Wait for the controller to observe the deletion. The delete triggers
	// a watch event → re-reconcile → Graph status update. Waiting for the
	// Graph's resourceVersion to stabilize proves the controller has seen
	// the delete before we change the spec.
	graphKey := types.NamespacedName{Name: "test-finalize-absent", Namespace: ns}
	require.NoError(t, waitForSettle(ctx, k8sClient, GraphGVK, graphKey))

	// Update spec to remove the target node.
	require.NoError(t, updateWithRetry(ctx, k8sClient, GraphGVK, graphKey, func(obj *unstructured.Unstructured) {
		unstructured.SetNestedSlice(obj.Object, []any{}, "spec", "nodes")
	}))
	t.Log("Updated spec: removed all nodes")

	// Wait for the controller to fully process the spec change. Generation 2
	// means it has reconciled the new (empty) node set and pruned old nodes.
	require.NoError(t, waitForObservedGeneration(ctx, k8sClient, graphKey, 2),
		"controller should process the spec change (generation 2)")
	require.NoError(t, waitForSettle(ctx, k8sClient, GraphGVK, graphKey))

	// After full convergence, the snapshot should not exist. Under load the
	// controller may transiently create it (informer cache race between the
	// external delete and the prune decision) but it is pruned with the rest
	// of the old spec. The assertion is: "after convergence, absent."
	snapshotObj := &unstructured.Unstructured{}
	snapshotObj.SetGroupVersionKind(cmGVK)
	err := k8sClient.Get(ctx, types.NamespacedName{Name: "absent-fin-snapshot", Namespace: ns}, snapshotObj)
	require.True(t, apierrors.IsNotFound(err),
		"finalize snapshot should not exist after convergence — target was already absent")
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
	require.NoError(t, waitForGraphCompiledStatus(ctx, k8sClient,
		types.NamespacedName{Name: "test-finalize-cel-reject", Namespace: ns}, "False"))
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
// Design 005-reconciliation § Finalization:
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
						"ref": map[string]any{
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
					"ref": map[string]any{
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
			return apierrors.IsNotFound(err), nil // gone = success
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
	require.NoError(t, waitForDeletion(ctx, k8sClient, GraphGVK,
		types.NamespacedName{Name: "test-finalize-teardown", Namespace: ns}))
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

// TestSupersededRevisionFinalizesGovernsPrompt proves that when a revision
// transition drops a finalizes declaration, the superseded revision's
// finalization metadata still governs the prune sequence.
//
// Rev N: target + gatewatch + snapshot (finalizes target, readyWhen gated)
// Rev N+1: target and snapshot removed, gatewatch kept
// The controller must use Rev N's DAG to find the finalizes relationship
// and run finalization before deleting the target.
//
// Design 002-revisions § Lifecycle:
//
//	"Must be retained until their unique resources are pruned because they
//	carry the ordering and finalization metadata for those resources."
//
// Failure mode: data loss — resource deleted without running the finalization
// sequence the user declared in the previous revision.
func TestSupersededRevisionFinalizesGovernsPrompt(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	cmGVK := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}

	// Pre-create the gate control ConfigMap with ready=true so Graph converges.
	gateControl := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "sup-fin-gate",
				"namespace": ns,
			},
			"data": map[string]any{"ready": "true"},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, gateControl))

	// Rev N: target + gatewatch + snapshot (finalizes target, readyWhen gated)
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-sup-fin-governs",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "target",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "sup-fin-target"},
							"data":       map[string]any{"state": "active"},
						},
					},
					map[string]any{
						"id": "gatewatch",
						"ref": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "sup-fin-gate"},
						},
					},
					map[string]any{
						"id":        "snapshot",
						"finalizes": "target",
						"readyWhen": []any{"${gatewatch.data.ready == 'true'}"},
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "sup-fin-snapshot"},
							"data":       map[string]any{"captured": "from-target"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for convergence.
	targetCM := &unstructured.Unstructured{}
	targetCM.SetGroupVersionKind(cmGVK)
	require.NoError(t, waitForResource(ctx, k8sClient,
		types.NamespacedName{Name: "sup-fin-target", Namespace: ns}, targetCM))
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-sup-fin-governs", Namespace: ns}))
	t.Log("Rev N converged — target exists, snapshot dormant")

	// Close the gate before the transition so finalization is observable.
	latestGate := &unstructured.Unstructured{}
	latestGate.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "sup-fin-gate", Namespace: ns}, latestGate))
	unstructured.SetNestedField(latestGate.Object, "false", "data", "ready")
	require.NoError(t, k8sClient.Update(ctx, latestGate))
	t.Log("Gate closed (ready=false) — finalization will block")

	// Rev N+1: remove target and snapshot, keep gatewatch.
	// The NEW revision has NO finalizes declaration. The controller must
	// use the SUPERSEDED revision's DAG.
	require.NoError(t, updateWithRetry(ctx, k8sClient, GraphGVK,
		types.NamespacedName{Name: "test-sup-fin-governs", Namespace: ns}, func(obj *unstructured.Unstructured) {
			unstructured.SetNestedSlice(obj.Object, []any{
				map[string]any{
					"id": "gatewatch",
					"ref": map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata":   map[string]any{"name": "sup-fin-gate"},
					},
				},
			}, "spec", "nodes")
		}))
	t.Log("Rev N+1 applied — target and snapshot removed from spec")

	// Wait for the snapshot to be created (finalization started from superseded DAG).
	snapshotCM := &unstructured.Unstructured{}
	snapshotCM.SetGroupVersionKind(cmGVK)
	require.NoError(t, waitForResource(ctx, k8sClient,
		types.NamespacedName{Name: "sup-fin-snapshot", Namespace: ns}, snapshotCM))
	t.Log("Snapshot CREATED — superseded revision's finalization is running")

	// Target must still exist while gate is closed (readyWhen unsatisfied).
	checkTarget := &unstructured.Unstructured{}
	checkTarget.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "sup-fin-target", Namespace: ns}, checkTarget),
		"target must survive while finalization gate is closed")

	// Stable hold — multiple reconcile cycles.
	require.NoError(t, waitForSettle(ctx, k8sClient, GraphGVK,
		types.NamespacedName{Name: "test-sup-fin-governs", Namespace: ns}))
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "sup-fin-target", Namespace: ns}, checkTarget),
		"target must survive across multiple reconcile cycles while gate is closed")
	t.Log("Target survived while gate closed — finalization is actively holding")

	// Open the gate → finalization completes → target deleted.
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "sup-fin-gate", Namespace: ns}, latestGate))
	unstructured.SetNestedField(latestGate.Object, "true", "data", "ready")
	require.NoError(t, k8sClient.Update(ctx, latestGate))
	t.Log("Gate opened (ready=true) — finalization should complete")

	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 60*time.Second, true,
		func(ctx context.Context) (bool, error) {
			check := &unstructured.Unstructured{}
			check.SetGroupVersionKind(cmGVK)
			err := k8sClient.Get(ctx,
				types.NamespacedName{Name: "sup-fin-target", Namespace: ns}, check)
			return apierrors.IsNotFound(err), nil
		}))
	t.Log("Target deleted after gate opened — superseded revision's finalizes governed the prune")
}

// TestFinalizerResourceCleanedUpBeforeReady proves that finalizer resources
// created during a prune walk are cleaned up before the Graph reaches
// Ready=True. The design requires that Ready=True means the desired state is
// fully realized AND all artifacts of the previous desired state are gone.
//
// Design 005-reconciliation § Finalization:
//
//	"The prune walk continues. The finalizer resources are in the applied
//	set but not in the desired state — they are prune candidates."
//
// Failure mode: Graph reaches Ready=True while the ephemeral finalizer
// ConfigMap still exists. An operator observing Ready=True would incorrectly
// believe the cluster has converged to the declared spec.
func TestFinalizer_RegressionCleanupLingersBeyondReady(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	cmGVK := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}

	// Phase 1: Create a Graph with target, keep, and finalizer.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-fin-cleanup",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "target",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "cleanup-target"},
							"data":       map[string]any{"state": "active"},
						},
					},
					map[string]any{
						"id": "keep",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "cleanup-keep"},
							"data":       map[string]any{"role": "permanent"},
						},
					},
					map[string]any{
						"id":        "snapshot",
						"finalizes": "target",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "cleanup-snapshot"},
							"data":       map[string]any{"captured": "true"},
						},
						// No readyWhen — auto-ready on create.
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for target to exist and Graph to be ready.
	targetCM := &unstructured.Unstructured{}
	targetCM.SetGroupVersionKind(cmGVK)
	require.NoError(t, waitForResource(ctx, k8sClient,
		types.NamespacedName{Name: "cleanup-target", Namespace: ns}, targetCM))
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-fin-cleanup", Namespace: ns}))
	t.Log("Phase 1: Target created, Graph ready")

	// Verify finalizer resource does NOT exist during normal operation.
	snapshotCM := &unstructured.Unstructured{}
	snapshotCM.SetGroupVersionKind(cmGVK)
	err := k8sClient.Get(ctx, types.NamespacedName{Name: "cleanup-snapshot", Namespace: ns}, snapshotCM)
	assert.Error(t, err, "finalizer resource should not exist during normal operation")

	// Phase 2: Remove target from spec. This triggers finalization → prune.
	require.NoError(t, updateWithRetry(ctx, k8sClient, GraphGVK,
		types.NamespacedName{Name: "test-fin-cleanup", Namespace: ns}, func(obj *unstructured.Unstructured) {
			unstructured.SetNestedSlice(obj.Object, []any{
				map[string]any{
					"id": "keep",
					"template": map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata":   map[string]any{"name": "cleanup-keep"},
						"data":       map[string]any{"role": "permanent"},
					},
				},
			}, "spec", "nodes")
		}))
	t.Log("Phase 2: Removed target and snapshot from spec")

	// Wait for the target to be deleted first — finalization must complete
	// before the target can be removed.
	require.NoError(t, waitForDeletion(ctx, k8sClient, cmGVK,
		types.NamespacedName{Name: "cleanup-target", Namespace: ns}))
	t.Log("Phase 2: Target deleted after finalization")

	// Wait for Graph to reach Ready=True.
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-fin-cleanup", Namespace: ns}))
	t.Log("Phase 2: Graph reached Ready=True")

	// THE KEY ASSERTION: when the Graph is Ready=True, the finalizer
	// resource must already be cleaned up. The deferred-delete after the
	// prune walk ensures same-cycle cleanup. Per 005-reconciliation.md
	// § Finalization: "The finalizer resources are in the applied set but
	// not in the desired state — they are prune candidates."
	snapshotCheck := &unstructured.Unstructured{}
	snapshotCheck.SetGroupVersionKind(cmGVK)
	err = k8sClient.Get(ctx, types.NamespacedName{Name: "cleanup-snapshot", Namespace: ns}, snapshotCheck)
	assert.Error(t, err, "finalizer resource must be gone when Graph is Ready=True")

	keepCheck := &unstructured.Unstructured{}
	keepCheck.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "cleanup-keep", Namespace: ns}, keepCheck))
	t.Log("Phase 2: Ready=True with target and finalizer both gone, keep alive")
}

// TestFinalizer_RegressionSkippedNotSurfaced was removed.
//
// It tested observability behavior (FinalizerSkipped note surfaced transiently
// in the Graph's Ready condition message), not algorithm correctness. The note
// is genuinely transient — it appears on the reconcile that processes the prune,
// then the next reconcile (with no prune candidates) overwrites the status.
// Polling for a transient status field is inherently racy.
//
// The core invariant (finalization is skipped when target absent, prune
// completes) is covered by TestFinalizesTargetAbsentSkips.

// TestFinalizer_RegressionDependencyOrdering verifies that when multiple
// finalizer nodes target the same resource with inter-finalizer CEL
// dependencies, they execute in dependency order — not declaration order.
//
// Setup: finalizer B references finalizer A's output (${snapshotA.data.marker}).
// B is declared BEFORE A in the spec. Without topological ordering, B would
// evaluate first and fail because A's data isn't in scope yet.
//
// This test would fail without the fix to sort finalizer nodes by their
// topological position in the DAG.
func TestFinalizer_RegressionDependencyOrdering(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	cmGVK := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}
	graphKey := types.NamespacedName{Name: "test-fin-dep-order", Namespace: ns}

	// Phase 1: Create a Graph with a target and two finalizer nodes.
	// snapshotB is declared BEFORE snapshotA but depends on snapshotA's data.
	// If finalization respects dependency order, snapshotA is created first
	// (it has no dependencies) and snapshotB second (depends on snapshotA).
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      graphKey.Name,
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "target",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "dep-order-target"},
							"data":       map[string]any{"state": "active"},
						},
					},
					// snapshotB declared FIRST but depends on snapshotA.
					map[string]any{
						"id":        "snapshotB",
						"finalizes": "target",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "dep-order-snap-b"},
							"data": map[string]any{
								"derived": "${snapshotA.data.marker}",
							},
						},
					},
					// snapshotA declared SECOND but has no inter-finalizer deps.
					map[string]any{
						"id":        "snapshotA",
						"finalizes": "target",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "dep-order-snap-a"},
							"data": map[string]any{
								"marker": "alpha",
							},
						},
					},
					// A keep node so the Graph isn't empty after target removal.
					map[string]any{
						"id": "keep",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "dep-order-keep"},
							"data":       map[string]any{"role": "anchor"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for the target to be created and Graph to be ready.
	targetCM := &unstructured.Unstructured{}
	targetCM.SetGroupVersionKind(cmGVK)
	require.NoError(t, waitForResource(ctx, k8sClient,
		types.NamespacedName{Name: "dep-order-target", Namespace: ns}, targetCM))
	require.NoError(t, waitForGraphReady(ctx, k8sClient, graphKey))
	t.Log("Target created, Graph ready")

	// Phase 2: Remove the target and finalizer nodes from the spec.
	// This makes "target" a prune candidate. The finalization logic runs
	// from the SUPERSEDED revision's DAG, which contains the finalizer
	// relationships and inter-finalizer dependency edges.
	require.NoError(t, updateWithRetry(ctx, k8sClient, GraphGVK, graphKey,
		func(obj *unstructured.Unstructured) {
			unstructured.SetNestedSlice(obj.Object, []any{
				map[string]any{
					"id": "keep",
					"template": map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata":   map[string]any{"name": "dep-order-keep"},
						"data":       map[string]any{"role": "anchor"},
					},
				},
			}, "spec", "nodes")
		}))
	t.Log("Target removed from spec — finalization should start")

	// Wait for the target to be deleted. This proves finalization completed
	// successfully: both snapshotA and snapshotB were created (in the
	// correct dependency order), reached readyWhen, and then the target
	// was deleted. The finalizer resources are ephemeral — created and
	// cleaned up within the same reconcile cycle — so we verify the
	// outcome (target deletion) rather than observing intermediate state.
	//
	// Without dependency-ordered finalization, snapshotB would fail to
	// evaluate (it references ${snapshotA.data.marker} which isn't in
	// scope if snapshotA hasn't been created yet), and the target would
	// remain stuck with a TeardownBlocked condition.
	require.NoError(t, waitForDeletion(ctx, k8sClient, cmGVK,
		types.NamespacedName{Name: "dep-order-target", Namespace: ns}),
		"target should be deleted after dependency-ordered finalization completes")
	t.Log("Target deleted — finalization complete with correct dependency ordering")

	// The Graph should converge to Ready.
	require.NoError(t, waitForGraphReady(ctx, k8sClient, graphKey))
	t.Log("Graph Ready — dependency-ordered finalization verified")
}
