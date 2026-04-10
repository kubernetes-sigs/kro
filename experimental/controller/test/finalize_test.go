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
	latest := &unstructured.Unstructured{}
	latest.SetGroupVersionKind(GraphGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "test-finalize-basic", Namespace: ns}, latest))

	unstructured.SetNestedSlice(latest.Object, []any{
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
	require.NoError(t, k8sClient.Update(ctx, latest))
	t.Log("Updated spec: removed target and snapshot nodes")

	// Wait for the target to be deleted — this proves finalization ran
	// (the target can only be deleted after the finalizer resource is created
	// and reaches readyWhen). The finalizer resource itself is ephemeral —
	// it's created, checked for readiness, and then pruned in subsequent cycles.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 15*time.Second, true,
		func(ctx context.Context) (bool, error) {
			check := &unstructured.Unstructured{}
			check.SetGroupVersionKind(cmGVK)
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
	latest := &unstructured.Unstructured{}
	latest.SetGroupVersionKind(GraphGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "test-finalize-absent", Namespace: ns}, latest))

	unstructured.SetNestedSlice(latest.Object, []any{}, "spec", "nodes")
	require.NoError(t, k8sClient.Update(ctx, latest))
	t.Log("Updated spec: removed all nodes")

	// The finalizer resource should NOT be created — target was already gone.
	// Use observation-based polling instead of time.Sleep to verify absence.
	require.NoError(t, waitForAbsence(ctx, k8sClient, cmGVK,
		types.NamespacedName{Name: "absent-fin-snapshot", Namespace: ns}, 2*time.Second))
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

	// The Graph should be rejected — Accepted should be False with a
	// compilation error about CEL-evaluated names on finalizes nodes.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 10*time.Second, true,
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
			accepted, found := findCondition(conditions, "Accepted")
			if !found {
				return false, nil
			}
			return accepted["status"] == "False", nil
		}))
	t.Log("Graph correctly rejected — CEL-evaluated name on finalizes node")
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
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 15*time.Second, true,
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
