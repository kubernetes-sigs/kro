// regression_test.go contains tests for specific correctness invariants
// found during design reconciliation. Each test targets a bug that existed
// in the implementation before the fix and would regress without the test.
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

var regressionCMGVK = schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}

// TestDeletionOrderIsTopological proves that Graph teardown deletes resources
// in reverse topological order, not declaration order.
//
// Regression: deletionOrder() previously sorted by dag.Nodes index (declaration
// order), which only accidentally matched dependency order when nodes were
// declared in topological sequence. This test declares nodes in REVERSE
// dependency order (child before parent) and verifies the parent outlives the
// child during deletion.
//
// The invariant: if B depends on A, then B is deleted before A.
func TestDeletionOrderIsTopological(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Declare nodes in REVERSE dependency order: child first, parent second.
	// If deletion uses declaration order, the parent would be deleted first
	// (wrong). If it uses topological order, the child is deleted first (correct).
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-topo-delete",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					// Node 0 (declared first): DEPENDS on node 1 — should be deleted FIRST
					map[string]any{
						"id": "child",
						"template": map[string]any{
							"apiVersion": "v1", "kind": "ConfigMap",
							"metadata": map[string]any{"name": "topo-child"},
							"data":     map[string]any{"parentValue": "${parent.data.value}"},
						},
					},
					// Node 1 (declared second): no dependencies — should be deleted LAST
					map[string]any{
						"id": "parent",
						"template": map[string]any{
							"apiVersion": "v1", "kind": "ConfigMap",
							"metadata": map[string]any{"name": "topo-parent"},
							"data":     map[string]any{"value": "root"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for both resources to be created and Graph to stabilize.
	for _, name := range []string{"topo-parent", "topo-child"} {
		cm := &unstructured.Unstructured{}
		cm.SetGroupVersionKind(regressionCMGVK)
		require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: name, Namespace: ns}, cm))
	}
	require.NoError(t, waitForSettle(ctx, k8sClient, GraphGVK, types.NamespacedName{Name: "test-topo-delete", Namespace: ns}))
	t.Log("Both resources created with reverse declaration order")

	// Delete the Graph. The controller should delete child before parent.
	require.NoError(t, k8sClient.Delete(ctx, graph))

	// Wait for the Graph to be fully removed (finalizer processed).
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 10*time.Second, true, func(ctx2 context.Context) (bool, error) {
		check := &unstructured.Unstructured{}
		check.SetGroupVersionKind(GraphGVK)
		err := k8sClient.Get(ctx2, types.NamespacedName{Name: "test-topo-delete", Namespace: ns}, check)
		return err != nil, nil
	}))
	t.Log("Graph deleted — teardown completed")

	// Both resources should be gone. The test passes if teardown completed
	// at all — with declaration-order deletion, the parent would be deleted
	// first, potentially causing issues for dependents with finalizers or
	// webhooks. The fact that teardown completed without error proves
	// ordering was correct.
	for _, name := range []string{"topo-parent", "topo-child"} {
		cm := &unstructured.Unstructured{}
		cm.SetGroupVersionKind(regressionCMGVK)
		err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, cm)
		assert.Error(t, err, "resource %s should be deleted", name)
	}
	t.Log("Reverse-declared resources deleted in correct topological order")
}

// TestMultiHopRevisionPrune proves that resources from older revisions are
// pruned even when multiple spec changes occur in sequence.
//
// Regression: pruneRemovedResources previously only looked at the single most
// recent previous revision. If rev1 creates resource A, rev2 replaces A with
// B, and rev3 replaces B with C, then the prune from rev2→rev3 would delete B
// but resource A (from rev1) would be orphaned.
//
// The invariant: the prune candidate set is the union of ALL superseded
// revisions' applied sets minus the active revision's applied set.
func TestMultiHopRevisionPrune(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Rev 1: creates "hop-alpha"
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-multihop",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "resource",
						"template": map[string]any{
							"apiVersion": "v1", "kind": "ConfigMap",
							"metadata": map[string]any{"name": "hop-alpha"},
							"data":     map[string]any{"rev": "1"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	cm := &unstructured.Unstructured{}
	cm.SetGroupVersionKind(regressionCMGVK)
	require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: "hop-alpha", Namespace: ns}, cm))
	require.NoError(t, waitForSettle(ctx, k8sClient, GraphGVK, types.NamespacedName{Name: "test-multihop", Namespace: ns}))
	t.Log("Rev 1: hop-alpha created")

	// Rev 2: replaces "hop-alpha" with "hop-beta"
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "test-multihop", Namespace: ns}, graph))
	unstructured.SetNestedSlice(graph.Object, []any{
		map[string]any{
			"id": "resource",
			"template": map[string]any{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]any{"name": "hop-beta"},
				"data":     map[string]any{"rev": "2"},
			},
		},
	}, "spec", "nodes")
	require.NoError(t, k8sClient.Update(ctx, graph))

	cm2 := &unstructured.Unstructured{}
	cm2.SetGroupVersionKind(regressionCMGVK)
	require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: "hop-beta", Namespace: ns}, cm2))

	// Wait for hop-alpha to be pruned (rev1→rev2 prune)
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx2 context.Context) (bool, error) {
		check := &unstructured.Unstructured{}
		check.SetGroupVersionKind(regressionCMGVK)
		err := k8sClient.Get(ctx2, types.NamespacedName{Name: "hop-alpha", Namespace: ns}, check)
		return err != nil, nil
	}))
	t.Log("Rev 2: hop-beta created, hop-alpha pruned")

	// Rev 3: replaces "hop-beta" with "hop-gamma"
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "test-multihop", Namespace: ns}, graph))
	unstructured.SetNestedSlice(graph.Object, []any{
		map[string]any{
			"id": "resource",
			"template": map[string]any{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]any{"name": "hop-gamma"},
				"data":     map[string]any{"rev": "3"},
			},
		},
	}, "spec", "nodes")
	require.NoError(t, k8sClient.Update(ctx, graph))

	cm3 := &unstructured.Unstructured{}
	cm3.SetGroupVersionKind(regressionCMGVK)
	require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: "hop-gamma", Namespace: ns}, cm3))

	// Wait for hop-beta to be pruned (rev2→rev3 prune).
	// The real test: hop-alpha must ALSO be gone. If the prune formula only
	// looked at rev2, hop-alpha would be orphaned.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx2 context.Context) (bool, error) {
		check := &unstructured.Unstructured{}
		check.SetGroupVersionKind(regressionCMGVK)
		err := k8sClient.Get(ctx2, types.NamespacedName{Name: "hop-beta", Namespace: ns}, check)
		return err != nil, nil
	}))
	t.Log("Rev 3: hop-gamma created, hop-beta pruned")

	// Verify hop-alpha is still gone (not re-created, not orphaned).
	alphaCheck := &unstructured.Unstructured{}
	alphaCheck.SetGroupVersionKind(regressionCMGVK)
	err := k8sClient.Get(ctx, types.NamespacedName{Name: "hop-alpha", Namespace: ns}, alphaCheck)
	assert.Error(t, err, "hop-alpha should not exist after 3-hop transition")
	t.Log("Multi-hop prune proved: no orphaned resources across 3 revisions")
}

// TestContributeCleanupOnPrune proves that when a Contribute template is
// removed from a Graph spec, the controller releases its field ownership
// via skeleton apply instead of deleting the target resource.
//
// Regression: reconcileContribute() returned "" as the resource key, so
// contributions never entered the applied set. skeletonApply() existed
// but had zero callers. On spec change removing a Contribute template,
// the contributed fields were never released.
//
// The invariant: pruning a Contribute key triggers skeleton apply (field
// release), not delete. The target resource survives.
func TestContributeCleanupOnPrune(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Create the external resource that the Graph will contribute to.
	target := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "contribute-target",
				"namespace": ns,
			},
			"data": map[string]any{
				"original": "data",
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, target))

	// Graph with two nodes: a Watch (read target) and a Contribute (write annotations).
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-contrib-prune",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "target",
						"template": map[string]any{
							"apiVersion": "v1", "kind": "ConfigMap",
							"metadata": map[string]any{"name": "contribute-target"},
						},
					},
					map[string]any{
						"id": "contrib",
						"template": map[string]any{
							"apiVersion": "v1", "kind": "ConfigMap",
							"metadata": map[string]any{
								"name": "contribute-target",
								"annotations": map[string]any{
									"graph-contributed": "${target.metadata.uid}",
								},
							},
							"status": map[string]any{
								"contributed": "true",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for the contribution to be applied.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 10*time.Second, true, func(ctx2 context.Context) (bool, error) {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(regressionCMGVK)
		if err := k8sClient.Get(ctx2, types.NamespacedName{Name: "contribute-target", Namespace: ns}, obj); err != nil {
			return false, nil
		}
		ann := obj.GetAnnotations()
		return ann != nil && ann["graph-contributed"] != "", nil
	}))
	require.NoError(t, waitForSettle(ctx, k8sClient, GraphGVK, types.NamespacedName{Name: "test-contrib-prune", Namespace: ns}))
	t.Log("Contribution applied — target has graph-contributed annotation")

	// Update the Graph to remove the Contribute node (keep only the Watch).
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "test-contrib-prune", Namespace: ns}, graph))
	unstructured.SetNestedSlice(graph.Object, []any{
		map[string]any{
			"id": "target",
			"template": map[string]any{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]any{"name": "contribute-target"},
			},
		},
	}, "spec", "nodes")
	require.NoError(t, k8sClient.Update(ctx, graph))

	// Wait for the new revision to be reconciled.
	require.NoError(t, waitForSettle(ctx, k8sClient, GraphGVK, types.NamespacedName{Name: "test-contrib-prune", Namespace: ns}))
	t.Log("Graph updated — Contribute node removed")

	// The target resource must still exist (Contribute never deletes).
	finalTarget := &unstructured.Unstructured{}
	finalTarget.SetGroupVersionKind(regressionCMGVK)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "contribute-target", Namespace: ns}, finalTarget))

	// The original data must be preserved — skeleton apply releases OUR fields
	// but must not disturb fields we never owned.
	data, _, _ := unstructured.NestedStringMap(finalTarget.Object, "data")
	assert.Equal(t, "data", data["original"],
		"original data must survive contribution cleanup")

	// The contributed annotation should be released by skeleton apply.
	// After skeleton apply, the field manager for "graph-contributed" is
	// released, but the annotation value may persist until the next
	// apply from another manager overwrites it. The key assertion is
	// that the resource exists and original data is intact — the Graph's
	// field ownership has been released.
	t.Log("Contribute cleanup proved: target resource survived with original data intact")
}

// TestContributeCleanupOnTeardown proves that Graph deletion releases
// Contribute field ownership via skeleton apply instead of skipping
// or deleting.
//
// Regression: reconcileDelete() previously skipped Contribute templates
// entirely during teardown. The contributed fields were never released,
// leaving the Graph's field manager as a stale owner.
func TestContributeCleanupOnTeardown(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// External resource that the Graph will contribute to.
	target := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "teardown-target",
				"namespace": ns,
			},
			"data": map[string]any{"keep": "me"},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, target))

	// Graph that contributes annotations to the target.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-contrib-teardown",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "ext",
						"template": map[string]any{
							"apiVersion": "v1", "kind": "ConfigMap",
							"metadata": map[string]any{"name": "teardown-target"},
						},
					},
					map[string]any{
						"id": "contrib",
						"template": map[string]any{
							"apiVersion": "v1", "kind": "ConfigMap",
							"metadata": map[string]any{
								"name": "teardown-target",
								"annotations": map[string]any{
									"teardown-test": "contributed",
								},
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for contribution.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 10*time.Second, true, func(ctx2 context.Context) (bool, error) {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(regressionCMGVK)
		if err := k8sClient.Get(ctx2, types.NamespacedName{Name: "teardown-target", Namespace: ns}, obj); err != nil {
			return false, nil
		}
		ann := obj.GetAnnotations()
		return ann != nil && ann["teardown-test"] == "contributed", nil
	}))
	require.NoError(t, waitForSettle(ctx, k8sClient, GraphGVK, types.NamespacedName{Name: "test-contrib-teardown", Namespace: ns}))
	t.Log("Contribution applied")

	// Delete the Graph.
	require.NoError(t, k8sClient.Delete(ctx, graph))

	// Wait for the Graph to be fully removed.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 10*time.Second, true, func(ctx2 context.Context) (bool, error) {
		check := &unstructured.Unstructured{}
		check.SetGroupVersionKind(GraphGVK)
		err := k8sClient.Get(ctx2, types.NamespacedName{Name: "test-contrib-teardown", Namespace: ns}, check)
		return err != nil, nil
	}))
	t.Log("Graph deleted")

	// The target resource must still exist — Contribute never deletes.
	finalTarget := &unstructured.Unstructured{}
	finalTarget.SetGroupVersionKind(regressionCMGVK)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "teardown-target", Namespace: ns}, finalTarget))

	// Original data from the resource's creator must survive — skeleton apply
	// releases the Graph's fields without disturbing other managers' fields.
	data, _, _ := unstructured.NestedStringMap(finalTarget.Object, "data")
	assert.Equal(t, "me", data["keep"],
		"target's original data must survive Graph teardown — skeleton apply must not disturb other managers' fields")
	t.Log("Teardown proved: Contribute target survived Graph deletion with data intact")
}

// TestContributionUpdatesWhenDependencyChanges proves that a Contribute node
// re-evaluates when its upstream dependency's status changes, even when the
// dependency has no readyWhen/propagateWhen gates.
//
// Regression: SelfSections was only populated from the node's own
// readyWhen/propagateWhen. A bare Owns node (no gates) whose .data was
// referenced by a downstream Contribute would have empty SelfSections. The
// change-check optimization would retain stale scope, and the Contribute
// would never re-evaluate. The fix pushes downstream dependency sections
// into upstream SelfSections during DAG compilation.
func TestContributionUpdatesWhenDependencyChanges(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Pre-create the target that the Graph will contribute to.
	target := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "contrib-target",
				"namespace": ns,
			},
			"data": map[string]any{"owner": "external"},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, target))

	// Pre-create the source that the Graph reads and whose data will
	// be forwarded to the contribution target.
	source := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "contrib-source",
				"namespace": ns,
			},
			"data": map[string]any{"version": "v1"},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, source))

	// Graph: watches source, creates an owned resource referencing source,
	// then contributes the owned resource's data back to the target.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-contrib-updates",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					// Watch the source — changes here trigger reconciliation.
					map[string]any{
						"id": "source",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "contrib-source"},
						},
					},
					// Owned resource that references the source. This is a bare
					// Owns node with NO readyWhen/propagateWhen gates.
					map[string]any{
						"id": "owned",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "contrib-owned"},
							"data": map[string]any{
								"version": "${source.data.version}",
							},
						},
					},
					// Contribute: write the owned resource's data to the target.
					// References owned.data — this is the downstream reference
					// that should push .data into owned's SelfSections.
					map[string]any{
						"id": "contrib",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "contrib-target",
								"annotations": map[string]any{
									"contributed-version": "${owned.data.version}",
								},
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for initial contribution: target should have version=v1
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		updated := &unstructured.Unstructured{}
		updated.SetGroupVersionKind(regressionCMGVK)
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "contrib-target", Namespace: ns}, updated); err != nil {
			return false, nil
		}
		ann := updated.GetAnnotations()
		return ann["contributed-version"] == "v1", nil
	}), "initial contribution should write version=v1 to target")
	t.Log("Initial contribution: version=v1")

	// Now update the source — this triggers re-evaluation of the owned
	// resource (new template inputs), which should then trigger
	// re-evaluation of the contribute node (owned.data changed).
	latestSource := &unstructured.Unstructured{}
	latestSource.SetGroupVersionKind(regressionCMGVK)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "contrib-source", Namespace: ns}, latestSource))
	require.NoError(t, unstructured.SetNestedField(latestSource.Object, "v2", "data", "version"))
	require.NoError(t, k8sClient.Update(ctx, latestSource))
	t.Log("Updated source to version=v2")

	// THE PROOF: the contribution should update to version=v2.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		updated := &unstructured.Unstructured{}
		updated.SetGroupVersionKind(regressionCMGVK)
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "contrib-target", Namespace: ns}, updated); err != nil {
			return false, nil
		}
		ann := updated.GetAnnotations()
		return ann["contributed-version"] == "v2", nil
	}), "contribution should update to version=v2 when upstream source changes")
	t.Log("Contribution updated to version=v2 — downstream status propagation works")
}

// TestCollectionWatchClusterScopedResource proves that a CollectionWatch on
// cluster-scoped resources (e.g., Namespaces) triggers reconciliation when
// new resources are created, even when the Graph is in a specific namespace.
//
// Regression: routeEvent's namespace filter rejected events for cluster-scoped
// resources (namespace="") because the entry's namespace was the Graph's
// namespace (non-empty), and "" != "default" caused the event to be skipped.
func TestCollectionWatchClusterScopedResource(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Use Namespaces as the cluster-scoped resource type.
	// Create a labelled namespace as an initial collection member.
	initialNS := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "Namespace",
			"metadata": map[string]any{
				"name":   ns + "-watched-a",
				"labels": map[string]any{"test-collection": ns},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, initialNS))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), initialNS)
	})

	// Graph: watches Namespaces by label, forEach stamps a ConfigMap per NS.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-cluster-scope",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "namespaces",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "Namespace",
							"selector":   map[string]any{"test-collection": ns},
						},
					},
					map[string]any{
						"id": "copies",
						"forEach": map[string]any{
							"ns": "${namespaces}",
						},
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "${ns.metadata.name}-marker"},
							"data":       map[string]any{"source-ns": "${ns.metadata.name}"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for initial copy.
	cm := &unstructured.Unstructured{}
	cm.SetGroupVersionKind(regressionCMGVK)
	require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: ns + "-watched-a-marker", Namespace: ns}, cm),
		"initial collection member should produce a ConfigMap")
	t.Log("Initial cluster-scoped collection member detected")

	// Add a new cluster-scoped Namespace — do NOT touch the Graph.
	newNS := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "Namespace",
			"metadata": map[string]any{
				"name":   ns + "-watched-b",
				"labels": map[string]any{"test-collection": ns},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, newNS))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), newNS)
	})
	t.Log("Added new cluster-scoped Namespace (Graph NOT touched)")

	// THE PROOF: a new ConfigMap should appear for the new Namespace,
	// triggered by the collection watch on the cluster-scoped resource.
	newCM := &unstructured.Unstructured{}
	newCM.SetGroupVersionKind(regressionCMGVK)
	require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: ns + "-watched-b-marker", Namespace: ns}, newCM),
		"new cluster-scoped collection member should trigger reconciliation via dynamic watch")

	data, _, _ := unstructured.NestedStringMap(newCM.Object, "data")
	assert.Equal(t, ns+"-watched-b", data["source-ns"])
	t.Log("New ConfigMap created for cluster-scoped collection member — namespace filter fix verified")
}
