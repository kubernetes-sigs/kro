package graphcontroller_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// TestOwnerRef_PatchPlacesFinalizer verifies that a Graph with:
//   - ownerReferences pointing to an external object
//   - a patch node that writes a finalizer to that object
//
// results in the owner object gaining the finalizer during normal
// reconciliation. No special controller logic places the finalizer —
// it's a standard patch node.
func TestOwnerRef_PatchPlacesFinalizer(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Create the owner object (a ConfigMap standing in for an instance).
	owner := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "owner-cm",
				"namespace": ns,
			},
			"data": map[string]any{"key": "value"},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, owner))
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "owner-cm", Namespace: ns}, owner))

	// Create a Graph with an ownerReference and a patch node that places
	// a finalizer on the owner. This mirrors what the RGD stdlib does.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-owner-patch",
				"namespace": ns,
				"ownerReferences": []any{
					map[string]any{
						"apiVersion":         "v1",
						"kind":               "ConfigMap",
						"name":               "owner-cm",
						"uid":                string(owner.GetUID()),
						"controller":         true,
						"blockOwnerDeletion": true,
					},
				},
			},
			"spec": map[string]any{
				"nodes": []any{
					// Patch node: places finalizer on the owner.
					map[string]any{
						"id": "lifecycle",
						"patch": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name":       "owner-cm",
								"namespace":  ns,
								"finalizers": []any{"experimental.kro.run/graph"},
							},
						},
					},
					// Managed resource.
					map[string]any{
						"id": "child",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "managed-child",
							},
							"data": map[string]any{"managed": "true"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for the Graph to become ready.
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-owner-patch", Namespace: ns}),
		"Graph should become ready")

	// Verify: the owner ConfigMap should have the finalizer from the patch node.
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "owner-cm", Namespace: ns}, owner))
	assert.True(t, controllerutil.ContainsFinalizer(owner, "experimental.kro.run/graph"),
		"owner should have finalizer, got: %v", owner.GetFinalizers())
}

// TestOwnerRef_TeardownOnOwnerDeletion verifies the complete owner
// deletion lifecycle using standard K8s primitives:
//
//	owner deleted → held by patch's finalizer → ownerDeleting detects →
//	Graph self-deletes → ordered teardown → patch pruned (finalizer
//	released via SSA) → owner completes deletion.
func TestOwnerRef_TeardownOnOwnerDeletion(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Create owner.
	owner := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "teardown-owner",
				"namespace": ns,
			},
			"data": map[string]any{"key": "value"},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, owner))
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "teardown-owner", Namespace: ns}, owner))

	// Create Graph with ownerReference + lifecycle patch.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-teardown",
				"namespace": ns,
				"ownerReferences": []any{
					map[string]any{
						"apiVersion":         "v1",
						"kind":               "ConfigMap",
						"name":               "teardown-owner",
						"uid":                string(owner.GetUID()),
						"controller":         true,
						"blockOwnerDeletion": true,
					},
				},
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "lifecycle",
						"patch": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name":       "teardown-owner",
								"namespace":  ns,
								"finalizers": []any{"experimental.kro.run/graph"},
							},
						},
					},
					map[string]any{
						"id": "managed",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "managed-resource",
							},
							"data": map[string]any{"managed": "true"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for Graph to become ready and managed resource to exist.
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-teardown", Namespace: ns}),
		"Graph should become ready")

	// Verify managed resource exists.
	managed := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "ConfigMap"}}
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "managed-resource", Namespace: ns}, managed))

	// Delete the owner. The patch's finalizer holds it in Terminating.
	require.NoError(t, k8sClient.Delete(ctx, owner))

	// Wait for the owner to disappear (finalizer released after teardown).
	cmGVK := schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}
	require.NoError(t, waitForDeletion(ctx, k8sClient, cmGVK,
		types.NamespacedName{Name: "teardown-owner", Namespace: ns}),
		"owner should eventually be deleted after teardown")

	// Verify: managed resource should also be gone.
	require.NoError(t, waitForDeletion(ctx, k8sClient, cmGVK,
		types.NamespacedName{Name: "managed-resource", Namespace: ns}),
		"managed resource should be cascade-deleted after owner deletion")

	// Verify: Graph should also be gone.
	require.NoError(t, waitForDeletion(ctx, k8sClient, GraphGVK,
		types.NamespacedName{Name: "test-teardown", Namespace: ns}),
		"Graph should be cascade-deleted after owner deletion")
}

// TestOwnerRef_OwnerGoneBeforeGraph exercises the race window: if the
// owner is deleted before the Graph is created, the patch node targeting
// the owner stays Pending (target doesn't exist). The Graph still
// reconciles its other nodes normally — it's effectively orphaned.
func TestOwnerRef_OwnerGoneBeforeGraph(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Create owner and immediately delete it (race simulation).
	owner := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "race-owner",
				"namespace": ns,
			},
			"data": map[string]any{"key": "value"},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, owner))
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "race-owner", Namespace: ns}, owner))
	uid := string(owner.GetUID())

	// Delete the owner before creating the graph.
	require.NoError(t, k8sClient.Delete(ctx, owner, &client.DeleteOptions{
		GracePeriodSeconds: ptr.To(int64(0)),
	}))
	require.NoError(t, waitForDeletion(ctx, k8sClient,
		schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"},
		types.NamespacedName{Name: "race-owner", Namespace: ns}))

	// Create Graph pointing to the now-gone owner. The lifecycle patch
	// can't find its target so stays Pending. ownerDeleting() can't GET
	// the owner so returns false. The Graph reconciles normally.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-race",
				"namespace": ns,
				"ownerReferences": []any{
					map[string]any{
						"apiVersion":         "v1",
						"kind":               "ConfigMap",
						"name":               "race-owner",
						"uid":                uid,
						"controller":         true,
						"blockOwnerDeletion": true,
					},
				},
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "lifecycle",
						"patch": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name":       "race-owner",
								"namespace":  ns,
								"finalizers": []any{"experimental.kro.run/graph"},
							},
						},
					},
					map[string]any{
						"id": "child",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "race-child",
							},
							"data": map[string]any{"orphaned": "true"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// The managed resource (child) should still be created even though
	// the lifecycle patch is Pending (owner is gone).
	childCM := &unstructured.Unstructured{}
	childCM.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"})
	require.NoError(t, waitForResource(ctx, k8sClient,
		types.NamespacedName{Name: "race-child", Namespace: ns}, childCM),
		"managed resource should be created despite lifecycle patch being Pending")

	// Cleanup.
	require.NoError(t, k8sClient.Delete(ctx, graph))
}

// TestOwnerRef_ManagedResourcesGoneBeforeOwnerReleased verifies that
// during owner-triggered teardown, managed resources are cleaned up
// and the owner stays in Terminating until teardown completes.
func TestOwnerRef_ManagedResourcesGoneBeforeOwnerReleased(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Create owner.
	owner := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "order-owner",
				"namespace": ns,
			},
			"data": map[string]any{"key": "value"},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, owner))
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "order-owner", Namespace: ns}, owner))

	// Create Graph with ownerReference + lifecycle patch + 2 managed resources.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-order",
				"namespace": ns,
				"ownerReferences": []any{
					map[string]any{
						"apiVersion":         "v1",
						"kind":               "ConfigMap",
						"name":               "order-owner",
						"uid":                string(owner.GetUID()),
						"controller":         true,
						"blockOwnerDeletion": true,
					},
				},
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "lifecycle",
						"patch": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name":       "order-owner",
								"namespace":  ns,
								"finalizers": []any{"experimental.kro.run/graph"},
							},
						},
					},
					map[string]any{
						"id": "cm1",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "order-child-1"},
							"data":       map[string]any{"seq": "1"},
						},
					},
					map[string]any{
						"id": "cm2",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "order-child-2"},
							"data":       map[string]any{"seq": "2"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for ready.
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-order", Namespace: ns}))

	// Delete owner.
	require.NoError(t, k8sClient.Delete(ctx, owner))

	// Wait for the owner to disappear (teardown complete, finalizer released).
	require.NoError(t, waitForDeletion(ctx, k8sClient,
		schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"},
		types.NamespacedName{Name: "order-owner", Namespace: ns}))

	// After the owner is gone, managed resources must also be gone.
	cm1 := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "ConfigMap"}}
	err := k8sClient.Get(ctx, types.NamespacedName{Name: "order-child-1", Namespace: ns}, cm1)
	assert.True(t, apierrors.IsNotFound(err), "managed child 1 should be gone")

	cm2 := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "ConfigMap"}}
	err = k8sClient.Get(ctx, types.NamespacedName{Name: "order-child-2", Namespace: ns}, cm2)
	assert.True(t, apierrors.IsNotFound(err), "managed child 2 should be gone")
}
