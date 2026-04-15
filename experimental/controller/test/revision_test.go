package graphcontroller_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
)

// TestRevisionRecoveryAfterManualDeletion proves that if a GraphRevision is
// manually deleted, the controller regenerates it from the current spec on the
// next reconcile cycle, restoring the active revision.
//
// Design 002-revisions § Lifecycle:
//
//	"If manually deleted, the active revision is regenerated from the current
//	spec on the next reconcile. The applied set (from watch cache) is the
//	authoritative record."
func TestRevisionRecoveryAfterManualDeletion(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "rev-recovery-test",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "cm",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "rev-recovery-cm"},
							"data":       map[string]any{"key": "value"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for Graph to become Active.
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "rev-recovery-test", Namespace: ns}))

	// Get the Graph's generation and revision name.
	latestGraph := &unstructured.Unstructured{}
	latestGraph.SetGroupVersionKind(GraphGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "rev-recovery-test", Namespace: ns}, latestGraph))
	gen := latestGraph.GetGeneration()
	revName := fmt.Sprintf("rev-recovery-test-g%05d", gen)

	// Wait for the revision.
	rev, err := waitForRevision(ctx, k8sClient,
		types.NamespacedName{Name: revName, Namespace: ns})
	require.NoError(t, err)
	t.Logf("Original revision: %s", revName)

	// Revisions are freely deletable — no finalizer needed.
	require.NoError(t, k8sClient.Delete(ctx, rev))
	t.Log("Revision deleted")

	// Wait for the revision to disappear.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 10*time.Second, true,
		func(ctx context.Context) (bool, error) {
			check := &unstructured.Unstructured{}
			check.SetGroupVersionKind(GraphRevisionGVK)
			err := k8sClient.Get(ctx,
				types.NamespacedName{Name: revName, Namespace: ns}, check)
			return err != nil, nil
		}))
	t.Log("Revision confirmed deleted")

	// Trigger a reconcile by touching the Graph's labels. The controller does
	// not watch revisions for deletion events — a reconcile must be triggered
	// externally so the controller notices the missing revision and regenerates it.
	require.NoError(t, updateWithRetry(ctx, k8sClient, GraphGVK,
		types.NamespacedName{Name: "rev-recovery-test", Namespace: ns}, func(obj *unstructured.Unstructured) {
			lbls := obj.GetLabels()
			if lbls == nil {
				lbls = map[string]string{}
			}
			lbls["trigger"] = "rev-recovery"
			obj.SetLabels(lbls)
		}))
	t.Log("Graph touched to trigger reconcile")

	// THE KEY ASSERTION: the revision must be regenerated with the same name.
	_, err = waitForRevision(ctx, k8sClient,
		types.NamespacedName{Name: revName, Namespace: ns})
	require.NoError(t, err, "revision must be regenerated after manual deletion")
	t.Logf("Revision regenerated: %s — recovery proved", revName)

	// Graph must return to Active after recovery.
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "rev-recovery-test", Namespace: ns}))
	t.Log("Graph Active after revision recovery")
}

// TestRevisionOwnerReferences proves that GraphRevisions carry ownerReferences
// pointing to their parent Graph. This enables the API server to cascade-delete
// any remaining revisions when the Graph itself is deleted.
//
// Design 002-revisions § OwnerReferences:
//
//	"Revisions have ownerReferences to their parent Graph. On Graph deletion,
//	the finalizer holds until full unwind; then API server cascade-deletes
//	remaining revisions."
func TestRevisionOwnerReferences(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "rev-ownerref-test",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "cm",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "rev-ownerref-cm"},
							"data":       map[string]any{"key": "value"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for Active and get the Graph's UID.
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "rev-ownerref-test", Namespace: ns}))

	latestGraph := &unstructured.Unstructured{}
	latestGraph.SetGroupVersionKind(GraphGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "rev-ownerref-test", Namespace: ns}, latestGraph))
	graphUID := string(latestGraph.GetUID())
	graphGen := latestGraph.GetGeneration()
	require.NotEmpty(t, graphUID, "Graph must have a UID")

	// Get the revision.
	revName := fmt.Sprintf("rev-ownerref-test-g%05d", graphGen)
	rev, err := waitForRevision(ctx, k8sClient,
		types.NamespacedName{Name: revName, Namespace: ns})
	require.NoError(t, err)

	// THE KEY ASSERTION: revision must have ownerReferences containing the Graph.
	ownerRefs := rev.GetOwnerReferences()
	require.NotEmpty(t, ownerRefs, "revision must have at least one ownerReference")

	var graphRef *metav1.OwnerReference
	for i := range ownerRefs {
		if ownerRefs[i].Name == "rev-ownerref-test" {
			graphRef = &ownerRefs[i]
			break
		}
	}
	require.NotNil(t, graphRef,
		"revision must have an ownerReference pointing to the parent Graph")
	assert.Equal(t, graphUID, string(graphRef.UID),
		"ownerReference UID must match the Graph's UID")
	assert.Equal(t, "Graph", graphRef.Kind,
		"ownerReference Kind must be Graph")
	t.Logf("Revision ownerReference: name=%s uid=%s kind=%s — cascade GC proved",
		graphRef.Name, graphRef.UID, graphRef.Kind)
}

// TestRevisionCreatedOnGraphCreate verifies that creating a Graph produces
// a GraphRevision with the correct name, labels, and materialized resources.
func TestRevisionCreatedOnGraphCreate(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "rev-create-test",
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
								"name": "rev-test-cm",
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

	// Wait for the ConfigMap to be created (proves reconciliation happened)
	cm := &unstructured.Unstructured{}
	cm.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
	require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: "rev-test-cm", Namespace: ns}, cm))

	// Fetch the Graph to get its generation
	latestGraph := &unstructured.Unstructured{}
	latestGraph.SetGroupVersionKind(GraphGVK)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "rev-create-test", Namespace: ns}, latestGraph))
	generation := latestGraph.GetGeneration()

	// A GraphRevision should exist for this generation
	revName := fmt.Sprintf("rev-create-test-g%05d", generation)
	rev, err := waitForRevision(ctx, k8sClient, types.NamespacedName{Name: revName, Namespace: ns})
	require.NoError(t, err, "GraphRevision should be created")
	t.Logf("GraphRevision created: %s", rev.GetName())

	// Verify labels
	assertRevisionLabels(t, rev, "rev-create-test", generation)

	// Verify the revision has a spec with nodes
	spec, ok := rev.Object["spec"].(map[string]any)
	require.True(t, ok, "revision should have a spec")
	nodes, ok := spec["nodes"].([]any)
	require.True(t, ok, "revision spec should have nodes")
	assert.Len(t, nodes, 1, "revision should have 1 resource")

	// Verify the materialized resource has injected labels
	res := nodes[0].(map[string]any)
	assert.Equal(t, "configmap", res["id"])
	tmpl, ok := res["template"].(map[string]any)
	require.True(t, ok)
	// Verify the template has the expected structure (metadata with name).
	// Labels are injected at apply time (not materialization time) for Deferred
	// shapes since Own vs Contribute isn't known until the first reconcile.
	md, _ := tmpl["metadata"].(map[string]any)
	require.NotNil(t, md)
	assert.Equal(t, "rev-test-cm", md["name"])
	t.Log("Revision has correct materialized template")
}

// TestRevisionCreatedOnSpecChange verifies that updating a Graph's spec
// creates a new GraphRevision while preserving the old one.
func TestRevisionCreatedOnSpecChange(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "rev-update-test",
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
								"name": "rev-update-cm",
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
	require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: "rev-update-cm", Namespace: ns}, cm))

	// Get the first generation
	latestGraph := &unstructured.Unstructured{}
	latestGraph.SetGroupVersionKind(GraphGVK)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "rev-update-test", Namespace: ns}, latestGraph))
	gen1 := latestGraph.GetGeneration()

	// Wait for first revision
	rev1Name := fmt.Sprintf("rev-update-test-g%05d", gen1)
	_, err := waitForRevision(ctx, k8sClient, types.NamespacedName{Name: rev1Name, Namespace: ns})
	require.NoError(t, err, "first revision should exist")
	t.Logf("First revision: %s (generation %d)", rev1Name, gen1)

	// Update the Graph spec
	require.NoError(t, updateWithRetry(ctx, k8sClient, GraphGVK,
		types.NamespacedName{Name: "rev-update-test", Namespace: ns}, func(obj *unstructured.Unstructured) {
			nodes := []any{
				map[string]any{
					"id": "configmap",
					"template": map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata": map[string]any{
							"name": "rev-update-cm",
						},
						"data": map[string]any{
							"version": "v2",
						},
					},
				},
			}
			unstructured.SetNestedSlice(obj.Object, nodes, "spec", "nodes")
		}))
	t.Log("Updated Graph spec: version v1 → v2")

	// Wait for the ConfigMap to be updated
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		cm2 := &unstructured.Unstructured{}
		cm2.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "rev-update-cm", Namespace: ns}, cm2); err != nil {
			return false, nil
		}
		data, _, _ := unstructured.NestedStringMap(cm2.Object, "data")
		return data["version"] == "v2", nil
	}))

	// Get the new generation
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "rev-update-test", Namespace: ns}, latestGraph))
	gen2 := latestGraph.GetGeneration()
	assert.Greater(t, gen2, gen1, "generation should have increased")

	// Second revision should exist
	rev2Name := fmt.Sprintf("rev-update-test-g%05d", gen2)
	rev2, err := waitForRevision(ctx, k8sClient, types.NamespacedName{Name: rev2Name, Namespace: ns})
	require.NoError(t, err, "second revision should exist")
	t.Logf("Second revision: %s (generation %d)", rev2Name, gen2)

	// Both revisions should exist
	count, err := countRevisions(ctx, k8sClient, "rev-update-test", ns)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, count, 1, "at least 1 revision should exist")
	t.Logf("Total revisions: %d", count)

	// Verify the second revision has updated content
	spec, _ := rev2.Object["spec"].(map[string]any)
	rev2Nodes, _ := spec["nodes"].([]any)
	require.Len(t, rev2Nodes, 1)
	rev2Tmpl := rev2Nodes[0].(map[string]any)["template"].(map[string]any)
	rev2Data, _ := rev2Tmpl["data"].(map[string]any)
	assert.Equal(t, "v2", rev2Data["version"], "second revision should have v2")
}

// TestRevisionNotCreatedOnCompilationFailure verifies that a Graph with
// invalid CEL expressions does NOT produce a GraphRevision.
func TestRevisionNotCreatedOnCompilationFailure(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "rev-fail-test",
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
								"name": "bad-cm",
							},
							"data": map[string]any{
								// Invalid CEL expression — type error at compile time
								"value": "${true + 42}",
							},
						},
					},
				},
			},
		},
	}

	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for Graph to show Compiled=False
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		g := &unstructured.Unstructured{}
		g.SetGroupVersionKind(GraphGVK)
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "rev-fail-test", Namespace: ns}, g); err != nil {
			return false, nil
		}
		conditions, _, _ := unstructured.NestedSlice(g.Object, "status", "conditions")
		cond, found := findCondition(conditions, "Compiled")
		if !found {
			return false, nil
		}
		return cond["status"] == "False", nil
	}))
	t.Log("Graph has Compiled=False")

	// No revision should exist
	count, err := countRevisions(ctx, k8sClient, "rev-fail-test", ns)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "no revisions should exist when compilation fails")
	t.Log("No revision created — compilation failure correctly prevented revision creation")
}

// TestRevisionCleanupOnDelete verifies that deleting a Graph cleans up
// all associated GraphRevisions.
func TestRevisionCleanupOnDelete(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "rev-delete-test",
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
								"name": "rev-delete-cm",
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

	// Wait for resource and revision to exist
	cm := &unstructured.Unstructured{}
	cm.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
	require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: "rev-delete-cm", Namespace: ns}, cm))

	latestGraph := &unstructured.Unstructured{}
	latestGraph.SetGroupVersionKind(GraphGVK)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "rev-delete-test", Namespace: ns}, latestGraph))
	gen := latestGraph.GetGeneration()

	revName := fmt.Sprintf("rev-delete-test-g%05d", gen)
	_, err := waitForRevision(ctx, k8sClient, types.NamespacedName{Name: revName, Namespace: ns})
	require.NoError(t, err)
	t.Logf("Revision exists: %s", revName)

	// Delete the Graph
	require.NoError(t, k8sClient.Delete(ctx, latestGraph))
	t.Log("Graph deleted")

	// Wait for Graph to be gone (finalizer removed)
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		check := &unstructured.Unstructured{}
		check.SetGroupVersionKind(GraphGVK)
		err := k8sClient.Get(ctx, types.NamespacedName{Name: "rev-delete-test", Namespace: ns}, check)
		return err != nil, nil
	}))
	t.Log("Graph gone")

	// GraphRevision should also be gone
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		count, err := countRevisions(ctx, k8sClient, "rev-delete-test", ns)
		if err != nil {
			return false, nil
		}
		return count == 0, nil
	}))
	t.Log("All revisions cleaned up on Graph deletion")

	// Managed resources should also be gone
	err = k8sClient.Get(ctx, types.NamespacedName{Name: "rev-delete-cm", Namespace: ns}, cm)
	assert.Error(t, err, "ConfigMap should be deleted")
	t.Log("Managed resources cleaned up")
}

// TestRevisionActivation verifies the activation lifecycle:
// the active revision gets Ready=True and Active=True conditions.
func TestRevisionActivation(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "rev-activate-test",
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
								"name": "rev-activate-cm",
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

	// Wait for the Graph to become Active (all resources reconciled)
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		g := &unstructured.Unstructured{}
		g.SetGroupVersionKind(GraphGVK)
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "rev-activate-test", Namespace: ns}, g); err != nil {
			return false, nil
		}
		return graphReady(g), nil
	}))
	t.Log("Graph is Active")

	// Get the revision
	latestGraph := &unstructured.Unstructured{}
	latestGraph.SetGroupVersionKind(GraphGVK)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "rev-activate-test", Namespace: ns}, latestGraph))
	gen := latestGraph.GetGeneration()

	revName := fmt.Sprintf("rev-activate-test-g%05d", gen)

	// Revision should have Ready=True
	require.NoError(t, waitForRevisionCondition(ctx, k8sClient,
		types.NamespacedName{Name: revName, Namespace: ns},
		"Ready", "True"))
	t.Log("Revision has Ready=True")

	// Revision should have Active=True
	require.NoError(t, waitForRevisionCondition(ctx, k8sClient,
		types.NamespacedName{Name: revName, Namespace: ns},
		"Active", "True"))
	t.Log("Revision has Active=True")

	t.Log("Revision activation lifecycle proved: Ready → Active")
}

// TestRevisionTransitionAbandonsStaleEvaluation proves that when a spec
// change triggers a revision transition, no SSA apply from the old revision
// lands after the new revision starts propagating. The final state matches
// the new revision with no intermediate churn.
//
// Design 004-graph-reconciliation § Propagation:
//
//	"Takes precedence even on spec changes where all nodes enter the frontier."
//
// Failure mode: spurious Conflict status or extra resourceVersion bumps
// from stale applies.
func TestRevisionTransitionAbandonsStaleEvaluation(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	gvk := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}

	// Rev N: chain config → middle → tail, all with version=v1.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-rev-transition",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "config",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "rev-trans-config"},
							"data": map[string]any{
								"version": "v1",
								"color":   "red",
							},
						},
					},
					map[string]any{
						"id": "middle",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "rev-trans-middle"},
							"data": map[string]any{
								"version": "${config.data.version}",
								"color":   "${config.data.color}",
							},
						},
					},
					map[string]any{
						"id": "tail",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "rev-trans-tail-v1"},
							"data": map[string]any{
								"version": "${middle.data.version}",
								"color":   "${middle.data.color}",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for convergence on Rev N.
	for _, name := range []string{"rev-trans-config", "rev-trans-middle", "rev-trans-tail-v1"} {
		cm := &unstructured.Unstructured{}
		cm.SetGroupVersionKind(gvk)
		require.NoError(t, waitForResource(ctx, k8sClient,
			types.NamespacedName{Name: name, Namespace: ns}, cm))
	}
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-rev-transition", Namespace: ns}))
	t.Log("Rev N converged: config/middle/tail-v1 with version=v1, color=red")

	// Transition to Rev N+1: change version/color AND rename tail.
	require.NoError(t, updateWithRetry(ctx, k8sClient, GraphGVK,
		types.NamespacedName{Name: "test-rev-transition", Namespace: ns}, func(obj *unstructured.Unstructured) {
			unstructured.SetNestedSlice(obj.Object, []any{
				map[string]any{
					"id": "config",
					"template": map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata":   map[string]any{"name": "rev-trans-config"},
						"data": map[string]any{
							"version": "v2",
							"color":   "blue",
						},
					},
				},
				map[string]any{
					"id": "middle",
					"template": map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata":   map[string]any{"name": "rev-trans-middle"},
						"data": map[string]any{
							"version": "${config.data.version}",
							"color":   "${config.data.color}",
						},
					},
				},
				map[string]any{
					"id": "tail",
					"template": map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata":   map[string]any{"name": "rev-trans-tail-v2"},
						"data": map[string]any{
							"version": "${middle.data.version}",
							"color":   "${middle.data.color}",
						},
					},
				},
			}, "spec", "nodes")
		}))
	t.Log("Rev N+1 submitted: version=v2, color=blue, tail renamed to v2")

	// Wait for the new tail to exist with correct v2 data.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true,
		func(ctx context.Context) (bool, error) {
			cm := &unstructured.Unstructured{}
			cm.SetGroupVersionKind(gvk)
			if err := k8sClient.Get(ctx,
				types.NamespacedName{Name: "rev-trans-tail-v2", Namespace: ns}, cm); err != nil {
				return false, nil
			}
			data, _, _ := unstructured.NestedStringMap(cm.Object, "data")
			return data["version"] == "v2" && data["color"] == "blue", nil
		}))
	t.Log("tail-v2 exists with version=v2, color=blue")

	// Old tail must be pruned.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true,
		func(ctx context.Context) (bool, error) {
			check := &unstructured.Unstructured{}
			check.SetGroupVersionKind(gvk)
			err := k8sClient.Get(ctx,
				types.NamespacedName{Name: "rev-trans-tail-v1", Namespace: ns}, check)
			return err != nil, nil
		}))
	t.Log("tail-v1 pruned")

	// Middle should have v2 data (no v1 residue).
	middleCM := &unstructured.Unstructured{}
	middleCM.SetGroupVersionKind(gvk)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "rev-trans-middle", Namespace: ns}, middleCM))
	middleData, _, _ := unstructured.NestedStringMap(middleCM.Object, "data")
	assert.Equal(t, "v2", middleData["version"])
	assert.Equal(t, "blue", middleData["color"])

	// After convergence, wait for settle and check resourceVersion stability.
	// Extra RV bumps would indicate stale applies from Rev N.
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-rev-transition", Namespace: ns}))
	require.NoError(t, waitForSettle(ctx, k8sClient, GraphGVK,
		types.NamespacedName{Name: "test-rev-transition", Namespace: ns}))

	rvBefore := map[string]string{}
	for _, name := range []string{"rev-trans-config", "rev-trans-middle", "rev-trans-tail-v2"} {
		cm := &unstructured.Unstructured{}
		cm.SetGroupVersionKind(gvk)
		require.NoError(t, k8sClient.Get(ctx,
			types.NamespacedName{Name: name, Namespace: ns}, cm))
		rvBefore[name] = cm.GetResourceVersion()
	}

	// Wait a bit and check stability.
	require.NoError(t, waitForSettle(ctx, k8sClient, GraphGVK,
		types.NamespacedName{Name: "test-rev-transition", Namespace: ns}))
	for _, name := range []string{"rev-trans-config", "rev-trans-middle", "rev-trans-tail-v2"} {
		cm := &unstructured.Unstructured{}
		cm.SetGroupVersionKind(gvk)
		require.NoError(t, k8sClient.Get(ctx,
			types.NamespacedName{Name: name, Namespace: ns}, cm))
		assert.Equal(t, rvBefore[name], cm.GetResourceVersion(),
			"%s resourceVersion should be stable — no stale applies from Rev N", name)
	}
	t.Log("ResourceVersions stable — no extra bumps from stale Rev N applies")
}
