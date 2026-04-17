package graphcontroller_test

import (
	"context"
	"encoding/json"
	"strings"
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

// TestMultiGraphFieldCoexistence proves that different Graphs can contribute
// different fields to the same resource without conflict or cross-graph
// interference.
//
// Design 003-ownership § Multi-graph coexistence:
//
//	"Different Graphs can manage different fields on the same resource
//	without conflict."
//
// This is the core ownership model: each Graph uses a dedicated field manager
// (<name>.<ns>.internal.kro.run), so SSA field ownership is scoped per
// Graph. Two Graphs writing disjoint field sets on the same resource must not
// produce a 409 conflict, and each Graph's fields must persist independently.
func TestMultiGraphFieldCoexistence(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	cmGVK := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}

	// Pre-create the shared resource. Both Graphs will detect Contribute shape
	// (resource exists on first reconcile → Contribute, not Own).
	shared := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "shared-coexist-cm",
				"namespace": ns,
			},
			"data": map[string]any{
				"original": "data",
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, shared))
	t.Log("Shared ConfigMap pre-created")

	// Graph A contributes data.keyA to the shared resource.
	graphA := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-coexist-a",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "contribute",
						"patch": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "shared-coexist-cm",
								"annotations": map[string]any{
									"kro.run/managed-by-a": "graph-a",
								},
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graphA))

	// Graph B contributes data.keyB to the same shared resource.
	graphB := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-coexist-b",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "contribute",
						"patch": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "shared-coexist-cm",
								"annotations": map[string]any{
									"kro.run/managed-by-b": "graph-b",
								},
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graphB))

	// Wait for both Graphs to reach Active state.
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-coexist-a", Namespace: ns}))
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-coexist-b", Namespace: ns}))
	t.Log("Both Graphs Active — no conflict detected")

	// THE KEY ASSERTION: shared resource has fields from BOTH graphs.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true,
		func(ctx context.Context) (bool, error) {
			check := &unstructured.Unstructured{}
			check.SetGroupVersionKind(cmGVK)
			if err := k8sClient.Get(ctx,
				types.NamespacedName{Name: "shared-coexist-cm", Namespace: ns}, check); err != nil {
				return false, nil
			}
			ann := check.GetAnnotations()
			return ann["kro.run/managed-by-a"] == "graph-a" && ann["kro.run/managed-by-b"] == "graph-b", nil
		}))

	result := &unstructured.Unstructured{}
	result.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "shared-coexist-cm", Namespace: ns}, result))

	ann := result.GetAnnotations()
	assert.Equal(t, "graph-a", ann["kro.run/managed-by-a"],
		"Graph A's field must be present on the shared resource")
	assert.Equal(t, "graph-b", ann["kro.run/managed-by-b"],
		"Graph B's field must be present on the shared resource")

	// Original data must be preserved.
	data, _, _ := unstructured.NestedStringMap(result.Object, "data")
	assert.Equal(t, "data", data["original"],
		"original data must be preserved despite two contributes")
	t.Log("Both Graphs' fields coexist — no conflict, original data preserved")

	// Update Graph A: write a new annotation value.
	require.NoError(t, updateWithRetry(ctx, k8sClient, GraphGVK,
		types.NamespacedName{Name: "test-coexist-a", Namespace: ns}, func(obj *unstructured.Unstructured) {
			unstructured.SetNestedSlice(obj.Object, []any{
				map[string]any{
					"id": "contribute",
					"patch": map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata": map[string]any{
							"name": "shared-coexist-cm",
							"annotations": map[string]any{
								"kro.run/managed-by-a": "graph-a-updated",
							},
						},
					},
				},
			}, "spec", "nodes")
		}))
	t.Log("Updated Graph A: new annotation value")

	// Wait for Graph A's update to propagate.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true,
		func(ctx context.Context) (bool, error) {
			check := &unstructured.Unstructured{}
			check.SetGroupVersionKind(cmGVK)
			if err := k8sClient.Get(ctx,
				types.NamespacedName{Name: "shared-coexist-cm", Namespace: ns}, check); err != nil {
				return false, nil
			}
			return check.GetAnnotations()["kro.run/managed-by-a"] == "graph-a-updated", nil
		}))

	// CRITICAL: Graph B's field must be UNCHANGED.
	final := &unstructured.Unstructured{}
	final.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "shared-coexist-cm", Namespace: ns}, final))
	finalAnn := final.GetAnnotations()
	assert.Equal(t, "graph-a-updated", finalAnn["kro.run/managed-by-a"],
		"Graph A's updated field should be applied")
	assert.Equal(t, "graph-b", finalAnn["kro.run/managed-by-b"],
		"Graph B's field must be unchanged — Graph A update must not stomp Graph B's fields")
	t.Log("Graph A updated without affecting Graph B's fields — multi-graph field coexistence proved")
}

// TestContribution proves that a Graph can write fields to an object it doesn't own.
// This is partial SSA — the Graph writes specific fields (like status) without
// taking ownership. No ownerReference is set on the target.
//
// This is how child Graphs write status back to the WebApp instance in the RGD model.
func TestContribution(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Pre-create an "external" ConfigMap that the Graph will contribute to.
	// This simulates a WebApp instance created by a user — the Graph doesn't own it.
	external := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "webapp-instance",
				"namespace": ns,
			},
			"data": map[string]any{
				"image":    "nginx:latest",
				"replicas": "3",
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, external))

	// Graph: reads the external object, creates a Deployment, then contributes
	// status back to the external object.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-contribution",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					// Read the external object into scope
					map[string]any{
						"id": "schema",
						"ref": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "webapp-instance",
							},
						},
					},
					// Create an owned resource (Deployment-like ConfigMap)
					map[string]any{
						"id": "deployment",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "${schema.metadata.name}-deployment",
							},
							"data": map[string]any{
								"image":    "${schema.data.image}",
								"replicas": "${schema.data.replicas}",
							},
						},
					},
					// Contribution: write metadata back to the external object.
					// Auto-detected because the template has only apiVersion,
					// kind, and metadata keys (no spec/data fields).
					map[string]any{
						"id": "status",
						"patch": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "${schema.metadata.name}",
								"annotations": map[string]any{
									"kro.run/deployment-name": "${deployment.metadata.name}",
									"kro.run/deployment-uid":  "${deployment.metadata.uid}",
								},
							},
						},
					},
				},
			},
		},
	}

	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for the owned deployment ConfigMap
	deplCM := &unstructured.Unstructured{}
	deplCM.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
	require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: "webapp-instance-deployment", Namespace: ns}, deplCM))

	// The deployment CM should be managed by the Graph
	assertManagedBy(t, deplCM, "test-contribution")
	t.Logf("Owned resource created: %s (managed by Graph)", deplCM.GetName())

	// Wait for the contribution to be applied to the external object
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		updated := &unstructured.Unstructured{}
		updated.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "webapp-instance", Namespace: ns}, updated); err != nil {
			return false, nil
		}
		ann := updated.GetAnnotations()
		return ann["kro.run/deployment-name"] == "webapp-instance-deployment", nil
	}))

	// Re-read the external object to verify
	updatedExternal := &unstructured.Unstructured{}
	updatedExternal.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "webapp-instance", Namespace: ns}, updatedExternal))

	// THE PROOF: The external object now has annotations written by the Graph
	annotations := updatedExternal.GetAnnotations()
	assert.Equal(t, "webapp-instance-deployment", annotations["kro.run/deployment-name"],
		"contribution should write annotations to external object")
	assert.NotEmpty(t, annotations["kro.run/deployment-uid"],
		"contribution should write server-assigned UID")

	// Original data should be preserved — contribution only touched metadata
	data, _, _ := unstructured.NestedStringMap(updatedExternal.Object, "data")
	assert.Equal(t, "nginx:latest", data["image"],
		"contribution should preserve existing data fields")
	assert.Equal(t, "3", data["replicas"],
		"contribution should preserve existing data fields")

	// THE KEY ASSERTION: the external object carries a "patch" identity
	// label from our Graph, not "template". Patches are partial writes —
	// they must never claim full ownership of a resource they didn't create.
	assertReferenceClassification(t, updatedExternal, "test-contribution", "patch")

	t.Logf("Contribution applied: webapp-instance now has deployment-name=%s, deployment-uid=%s",
		annotations["kro.run/deployment-name"], annotations["kro.run/deployment-uid"])
	t.Log("Partial SSA proved: Graph wrote metadata to external object without taking ownership")
}

// TestResourcePruning proves that removing a resource from the Graph spec
// causes the previously-created resource to be deleted.
func TestResourcePruning(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-pruning",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "keep",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "keep-me"},
							"data":       map[string]any{"state": "permanent"},
						},
					},
					map[string]any{
						"id": "remove",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "remove-me"},
							"data":       map[string]any{"state": "temporary"},
						},
					},
				},
			},
		},
	}

	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for both ConfigMaps
	keepCM := &unstructured.Unstructured{}
	keepCM.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
	require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: "keep-me", Namespace: ns}, keepCM))

	removeCM := &unstructured.Unstructured{}
	removeCM.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
	require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: "remove-me", Namespace: ns}, removeCM))
	t.Log("Both ConfigMaps created")

	// Update the Graph: remove the second resource
	require.NoError(t, updateWithRetry(ctx, k8sClient, GraphGVK, types.NamespacedName{Name: "test-pruning", Namespace: ns}, func(obj *unstructured.Unstructured) {
		unstructured.SetNestedSlice(obj.Object, []any{
			map[string]any{
				"id": "keep",
				"template": map[string]any{
					"apiVersion": "v1",
					"kind":       "ConfigMap",
					"metadata":   map[string]any{"name": "keep-me"},
					"data":       map[string]any{"state": "permanent"},
				},
			},
		}, "spec", "nodes")
	}))

	// Wait for the removed ConfigMap to be deleted
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		check := &unstructured.Unstructured{}
		check.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
		err := k8sClient.Get(ctx, types.NamespacedName{Name: "remove-me", Namespace: ns}, check)
		if err != nil {
			return true, nil // deleted
		}
		return false, nil
	}))
	t.Log("Removed ConfigMap was pruned")

	// The kept ConfigMap should still exist
	stillThere := &unstructured.Unstructured{}
	stillThere.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "keep-me", Namespace: ns}, stillThere))
	data, _, _ := unstructured.NestedStringMap(stillThere.Object, "data")
	assert.Equal(t, "permanent", data["state"])
	t.Log("Kept ConfigMap still exists with correct data")
}

// TestContributeReferenceDetectedByExistence proves that when a resource
// pre-exists before the Graph creates it, the controller detects Contribute
// shape — behavioral consequence: the resource is NOT deleted when the
// template is removed from the Graph spec. Own shape would delete it.
//
// Design 003-ownership § NodeType Types: "Own — Creates the resource if
// absent. Deletes on prune." vs "Contribute — Writes fields on a resource
// the Graph does not create. Releases fields on prune, never deletes."
//
// The assertion is on behavioral consequence (no delete on prune), not
// internal classification.
func TestContributeReferenceDetectedByExistence(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	cmGVK := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}

	// Pre-create the target resource — this makes the template a Contribute.
	external := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "contribute-target",
				"namespace": ns,
			},
			"data": map[string]any{
				"owner":    "external",
				"original": "data",
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, external))
	t.Log("Pre-created external resource")

	// Graph contributes annotations to the pre-existing resource.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-contribute-shape",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "target",
						"patch": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "contribute-target",
								"annotations": map[string]any{
									"kro.run/managed": "true",
								},
							},
						},
					},
					map[string]any{
						"id": "owned",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "contribute-owned"},
							"data":       map[string]any{"state": "created-by-graph"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for the owned resource and Graph to converge.
	owned := &unstructured.Unstructured{}
	owned.SetGroupVersionKind(cmGVK)
	require.NoError(t, waitForResource(ctx, k8sClient,
		types.NamespacedName{Name: "contribute-owned", Namespace: ns}, owned))
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-contribute-shape", Namespace: ns}))

	// Verify the contribution was applied.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true,
		func(ctx context.Context) (bool, error) {
			check := &unstructured.Unstructured{}
			check.SetGroupVersionKind(cmGVK)
			if err := k8sClient.Get(ctx,
				types.NamespacedName{Name: "contribute-target", Namespace: ns}, check); err != nil {
				return false, nil
			}
			ann := check.GetAnnotations()
			return ann["kro.run/managed"] == "true", nil
		}))
	t.Log("Contribution applied — annotation set on external resource")

	// Remove both nodes from the spec (prune).
	require.NoError(t, updateWithRetry(ctx, k8sClient, GraphGVK,
		types.NamespacedName{Name: "test-contribute-shape", Namespace: ns}, func(obj *unstructured.Unstructured) {
			unstructured.SetNestedSlice(obj.Object, []any{}, "spec", "nodes")
		}))
	t.Log("Removed both nodes from spec — prune triggered")

	// Wait for the owned resource to be deleted (Own shape → delete on prune).
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true,
		func(ctx context.Context) (bool, error) {
			check := &unstructured.Unstructured{}
			check.SetGroupVersionKind(cmGVK)
			err := k8sClient.Get(ctx,
				types.NamespacedName{Name: "contribute-owned", Namespace: ns}, check)
			return err != nil, nil // gone = true
		}))
	t.Log("Owned resource deleted on prune — Own shape confirmed")

	// THE KEY ASSERTION: the contributed resource should still exist
	// (Contribute shape → release fields on prune, never delete).
	target := &unstructured.Unstructured{}
	target.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "contribute-target", Namespace: ns}, target),
		"contributed resource should NOT be deleted on prune (Contribute shape)")

	// Original data should still be there.
	data, _, _ := unstructured.NestedStringMap(target.Object, "data")
	assert.Equal(t, "external", data["owner"],
		"contributed resource should preserve original data")
	assert.Equal(t, "data", data["original"],
		"contributed resource should preserve original data")
	t.Log("Contributed resource survived prune — Contribute shape detection proved via behavioral consequence")
}

// TestContribute_RegressionStatusSubresourceTeardown proves that a Contribute
// node writing status fields releases field ownership via release apply during
// Graph deletion. The target must survive (it's Contribute, not Own) and the
// Graph's field manager must not retain contributed status fields.
//
// Per 003-ownership.md § Status Subresource: "Releases only target the
// subresources the template actually applied to."
func TestContribute_RegressionStatusSubresourceTeardown(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Pre-create a SimpleApp (which has a status subresource)
	target := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "test.kro.run/v1alpha1",
			"kind":       "SimpleApp",
			"metadata": map[string]any{
				"name":      "status-target",
				"namespace": ns,
			},
			"spec": map[string]any{
				"name": "my-app",
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, target))

	// Graph contributes status fields to the target
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "contrib-status-teardown",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "statusContrib",
						"patch": map[string]any{
							"apiVersion": "test.kro.run/v1alpha1",
							"kind":       "SimpleApp",
							"metadata":   map[string]any{"name": "status-target"},
							"status": map[string]any{
								"ready":   true,
								"message": "contributed-by-graph",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for the Graph to settle
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "contrib-status-teardown", Namespace: ns}))

	// Verify the status was applied
	targetCheck := &unstructured.Unstructured{}
	targetCheck.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "test.kro.run", Version: "v1alpha1", Kind: "SimpleApp",
	})
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "status-target", Namespace: ns}, targetCheck))
	statusMap, _, _ := unstructured.NestedMap(targetCheck.Object, "status")
	assert.Equal(t, "contributed-by-graph", statusMap["message"],
		"status should have been contributed by the Graph")

	// Delete the Graph
	require.NoError(t, k8sClient.Delete(ctx, graph))

	// Wait for Graph to be deleted
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true,
		func(ctx context.Context) (bool, error) {
			g := &unstructured.Unstructured{}
			g.SetGroupVersionKind(GraphGVK)
			err := k8sClient.Get(ctx, types.NamespacedName{Name: "contrib-status-teardown", Namespace: ns}, g)
			return err != nil, nil
		}))

	// The target MUST still exist (it's a Contribute, not Own)
	finalTarget := &unstructured.Unstructured{}
	finalTarget.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "test.kro.run", Version: "v1alpha1", Kind: "SimpleApp",
	})
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "status-target", Namespace: ns}, finalTarget),
		"Contribute target must survive Graph deletion")

	// Verify the Graph's field manager state after release apply.
	managedFields := finalTarget.GetManagedFields()
	graphManager := "contrib-status-teardown." + ns + ".internal.kro.run"

	// Main resource: release apply should release identity labels/annotations.
	for _, mf := range managedFields {
		if mf.Manager == graphManager && mf.Subresource == "" {
			if mf.FieldsV1 != nil {
				fields := string(mf.FieldsV1.Raw)
				assert.NotContains(t, fields, "f:spec",
					"main resource fields should be released after release apply")
			}
		}
	}

	// Status subresource: release apply should release status fields.
	var graphOwnsStatusFields bool
	for _, mf := range managedFields {
		if mf.Manager == graphManager && mf.Subresource == "status" && mf.FieldsV1 != nil {
			fields := string(mf.FieldsV1.Raw)
			if strings.Contains(fields, "message") {
				graphOwnsStatusFields = true
			}
		}
	}
	assert.False(t, graphOwnsStatusFields,
		"Graph's field manager should not own status.message after teardown")

	// Status values should be removed (Graph was sole owner).
	finalStatus, _, _ := unstructured.NestedMap(finalTarget.Object, "status")
	assert.Nil(t, finalStatus["message"],
		"contributed status.message should be removed after teardown")
}

// TestContributeMetadataAndStatus proves that a Contribute node writing both
// metadata (annotations) and status fields to a resource with a real status
// subresource exercises both apply paths: main resource apply for metadata,
// status subresource apply for status. On Graph deletion, release apply must
// release field ownership on both subresources.
//
// This is the dual-subresource path: applySSA splits the template into a
// main payload (everything except status) and a status payload, applies each
// to the appropriate endpoint. The contribute key encodes hasStatus=true so
// release apply knows to release both.
//
// Per 003-ownership.md § Status Subresource: "Releases only target the
// subresources the template actually applied to."
func TestContributeMetadataAndStatus(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	saGVK := schema.GroupVersionKind{
		Group: "test.kro.run", Version: "v1alpha1", Kind: "SimpleApp",
	}

	// Pre-create a SimpleApp (has a real status subresource).
	target := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "test.kro.run/v1alpha1",
			"kind":       "SimpleApp",
			"metadata": map[string]any{
				"name":      "dual-target",
				"namespace": ns,
			},
			"spec": map[string]any{
				"name": "my-app",
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, target))

	graphName := "contrib-dual-subresource"

	// Graph contributes both metadata (annotations) and status to the target.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      graphName,
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "contrib",
						"patch": map[string]any{
							"apiVersion": "test.kro.run/v1alpha1",
							"kind":       "SimpleApp",
							"metadata": map[string]any{
								"name": "dual-target",
								"annotations": map[string]any{
									"kro.run/managed":  "true",
									"kro.run/revision": "1",
								},
							},
							"status": map[string]any{
								"ready":   true,
								"message": "contributed-status",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for the Graph to converge.
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: graphName, Namespace: ns}))

	// Verify metadata was applied (main resource endpoint).
	check := &unstructured.Unstructured{}
	check.SetGroupVersionKind(saGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "dual-target", Namespace: ns}, check))
	ann := check.GetAnnotations()
	assert.Equal(t, "true", ann["kro.run/managed"],
		"metadata annotation should be applied via main resource endpoint")
	assert.Equal(t, "1", ann["kro.run/revision"],
		"metadata annotation should be applied via main resource endpoint")

	// Verify status was applied (status subresource endpoint).
	statusMap, _, _ := unstructured.NestedMap(check.Object, "status")
	assert.Equal(t, "contributed-status", statusMap["message"],
		"status.message should be applied via status subresource endpoint")
	t.Log("Both metadata and status contributed successfully")

	// Record the field manager name for later assertions.
	graphManager := graphName + "." + ns + ".internal.kro.run"

	// Verify the field manager has entries for BOTH subresources.
	managedFields := check.GetManagedFields()
	var hasMainEntry, hasStatusEntry bool
	for _, mf := range managedFields {
		if mf.Manager == graphManager {
			if mf.Subresource == "" {
				hasMainEntry = true
			}
			if mf.Subresource == "status" {
				hasStatusEntry = true
			}
		}
	}
	assert.True(t, hasMainEntry,
		"Graph's field manager should have a main resource entry (metadata)")
	assert.True(t, hasStatusEntry,
		"Graph's field manager should have a status subresource entry")
	t.Log("Field manager has entries for both main resource and status subresource")

	// Delete the Graph — triggers release apply on both subresources.
	require.NoError(t, k8sClient.Delete(ctx, graph))

	// Wait for Graph to be fully deleted.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true,
		func(ctx context.Context) (bool, error) {
			g := &unstructured.Unstructured{}
			g.SetGroupVersionKind(GraphGVK)
			err := k8sClient.Get(ctx, types.NamespacedName{Name: graphName, Namespace: ns}, g)
			return err != nil, nil
		}))
	t.Log("Graph deleted — release apply should have run on both subresources")

	// The target MUST still exist (Contribute, not Own).
	finalTarget := &unstructured.Unstructured{}
	finalTarget.SetGroupVersionKind(saGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "dual-target", Namespace: ns}, finalTarget),
		"Contribute target must survive Graph deletion")

	// Assert contributed annotation VALUES are gone after teardown.
	// Release apply sends an identity-only release body to the main resource.
	// SSA releases ownership of all previously-owned fields not in the release body.
	// Since the Graph was the sole owner of these annotations, SSA removes them.
	finalAnn := finalTarget.GetAnnotations()
	assert.Empty(t, finalAnn["kro.run/managed"],
		"contributed annotation should be removed after release apply during teardown")
	assert.Empty(t, finalAnn["kro.run/revision"],
		"contributed annotation should be removed after release apply during teardown")

	// Assert contributed status VALUES are gone after teardown.
	// Release apply sends {status: {}} to the /status subresource, releasing
	// ownership of all previously-owned status fields. Since the Graph was
	// the sole owner, SSA removes them.
	finalStatus, _, _ := unstructured.NestedMap(finalTarget.Object, "status")
	assert.Nil(t, finalStatus["message"],
		"contributed status.message should be removed after release apply during teardown")

	// Assert the Graph's field manager no longer owns contributed fields.
	finalMF := finalTarget.GetManagedFields()
	var graphOwnsMainFields, graphOwnsStatusFields bool
	for _, mf := range finalMF {
		if mf.Manager == graphManager {
			if mf.Subresource == "" && mf.FieldsV1 != nil {
				fields := string(mf.FieldsV1.Raw)
				if strings.Contains(fields, "kro.run/managed") {
					graphOwnsMainFields = true
				}
			}
			if mf.Subresource == "status" && mf.FieldsV1 != nil {
				fields := string(mf.FieldsV1.Raw)
				if strings.Contains(fields, "message") {
					graphOwnsStatusFields = true
				}
			}
		}
	}
	assert.False(t, graphOwnsMainFields,
		"Graph's field manager should not own contributed annotations after teardown")
	assert.False(t, graphOwnsStatusFields,
		"Graph's field manager should not own contributed status fields after teardown")

	// Original spec must survive.
	specName, _, _ := unstructured.NestedString(finalTarget.Object, "spec", "name")
	assert.Equal(t, "my-app", specName,
		"original spec.name must survive Contribute teardown")
	t.Log("Dual-subresource Contribute: metadata and status released cleanly after teardown")
}

// TestContributeMapFieldOwnership proves that a Contribute node writing
// specific keys to a map field (ConfigMap .data) takes field-level ownership
// of only those keys, and release apply releases that ownership on prune.
//
// SSA semantics for maps: each key is an independently owned field. When a
// field manager applies a map with keys {a, b}, it owns those keys. When it
// later applies without key b, ownership of b is released. When the SOLE
// owner releases a field, SSA removes the field entirely — the value does
// not persist as an unmanaged field.
//
// This test asserts:
//  1. Contributed keys are applied (values present on resource)
//  2. Original keys from another field manager are untouched
//  3. After prune (release apply), contributed key VALUES are deleted
//     (sole owner released → SSA removes the field)
//  4. Graph's field manager no longer owns the contributed keys
//  5. External manager's field ownership is unaffected
func TestContributeMapFieldOwnership(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	cmGVK := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}

	// Pre-create target ConfigMap with keys owned by "external-manager".
	// Using SSA so the field manager ownership is explicit.
	externalPayload := map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":      "map-target",
			"namespace": ns,
		},
		"data": map[string]any{
			"original-key": "external-value",
			"keep-me":      "untouched",
		},
	}
	raw, err := json.Marshal(externalPayload)
	require.NoError(t, err)
	extCM := &unstructured.Unstructured{}
	extCM.SetGroupVersionKind(cmGVK)
	extCM.SetName("map-target")
	extCM.SetNamespace(ns)
	require.NoError(t, k8sClient.Patch(ctx, extCM, client.RawPatch(
		types.ApplyPatchType, raw),
		client.ForceOwnership,
		client.FieldOwner("external-manager"),
	))
	t.Log("Pre-created ConfigMap with external-manager owning .data keys")

	graphName := "contrib-map-fields"

	// Graph contributes additional data keys to the ConfigMap.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      graphName,
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "contrib",
						"patch": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "map-target",
								"annotations": map[string]any{
									"kro.run/contributed": "true",
								},
							},
							"data": map[string]any{
								"contributed-key": "graph-value",
								"another-key":     "more-data",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for the contribution to land.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true,
		func(ctx context.Context) (bool, error) {
			check := &unstructured.Unstructured{}
			check.SetGroupVersionKind(cmGVK)
			if err := k8sClient.Get(ctx,
				types.NamespacedName{Name: "map-target", Namespace: ns}, check); err != nil {
				return false, nil
			}
			data, _, _ := unstructured.NestedStringMap(check.Object, "data")
			return data["contributed-key"] == "graph-value" && data["another-key"] == "more-data", nil
		}))
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: graphName, Namespace: ns}))

	// Verify all keys coexist — original and contributed.
	check := &unstructured.Unstructured{}
	check.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "map-target", Namespace: ns}, check))
	data, _, _ := unstructured.NestedStringMap(check.Object, "data")
	assert.Equal(t, "external-value", data["original-key"], "external key must coexist")
	assert.Equal(t, "untouched", data["keep-me"], "external key must coexist")
	assert.Equal(t, "graph-value", data["contributed-key"], "contributed key must be present")
	assert.Equal(t, "more-data", data["another-key"], "contributed key must be present")
	t.Log("All keys coexist: 2 external + 2 contributed")

	// Verify field manager ownership before prune.
	graphManager := graphName + "." + ns + ".internal.kro.run"
	managedFields := check.GetManagedFields()
	var graphOwnsDataKeys bool
	for _, mf := range managedFields {
		if mf.Manager == graphManager && mf.Subresource == "" && mf.FieldsV1 != nil {
			fields := string(mf.FieldsV1.Raw)
			if strings.Contains(fields, "contributed-key") {
				graphOwnsDataKeys = true
			}
		}
	}
	assert.True(t, graphOwnsDataKeys,
		"Graph's field manager should own .data.contributed-key before prune")
	t.Log("Graph field manager owns contributed data keys")

	// Prune: remove the Contribute node from the Graph spec.
	require.NoError(t, updateWithRetry(ctx, k8sClient, GraphGVK,
		types.NamespacedName{Name: graphName, Namespace: ns}, func(obj *unstructured.Unstructured) {
			unstructured.SetNestedSlice(obj.Object, []any{}, "spec", "nodes")
		}))
	t.Log("Removed Contribute node — prune triggered")

	// Wait for the Graph to settle after prune.
	require.NoError(t, waitForSettle(ctx, k8sClient, GraphGVK,
		types.NamespacedName{Name: graphName, Namespace: ns}))

	// Re-read the target to check field ownership after release apply.
	final := &unstructured.Unstructured{}
	final.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "map-target", Namespace: ns}, final),
		"Contribute target must survive prune")

	// ASSERTION 1: External manager's keys must be intact.
	finalData, _, _ := unstructured.NestedStringMap(final.Object, "data")
	assert.Equal(t, "external-value", finalData["original-key"],
		"external manager's key must survive Contribute prune")
	assert.Equal(t, "untouched", finalData["keep-me"],
		"external manager's key must survive Contribute prune")

	// ASSERTION 2: Contributed key VALUES are deleted — when the sole owner
	// releases a field via SSA, the API server removes it entirely. This is
	// the correct Contribute prune behavior: contributed data disappears when
	// the Graph stops contributing.
	_, contributedExists := finalData["contributed-key"]
	assert.False(t, contributedExists,
		"contributed key should be removed entirely when sole owner releases via release apply")
	_, anotherExists := finalData["another-key"]
	assert.False(t, anotherExists,
		"contributed key should be removed entirely when sole owner releases via release apply")

	// ASSERTION 3: Graph's field manager should no longer own the data keys.
	finalMF := final.GetManagedFields()
	var graphStillOwnsDataKeys bool
	for _, mf := range finalMF {
		if mf.Manager == graphManager && mf.Subresource == "" && mf.FieldsV1 != nil {
			fields := string(mf.FieldsV1.Raw)
			if strings.Contains(fields, "contributed-key") {
				graphStillOwnsDataKeys = true
			}
		}
	}
	assert.False(t, graphStillOwnsDataKeys,
		"after release apply, Graph's field manager should not own .data.contributed-key")

	// ASSERTION 4: External manager's ownership is unaffected.
	var externalStillOwnsKeys bool
	for _, mf := range finalMF {
		if mf.Manager == "external-manager" && mf.Subresource == "" && mf.FieldsV1 != nil {
			fields := string(mf.FieldsV1.Raw)
			if strings.Contains(fields, "original-key") {
				externalStillOwnsKeys = true
			}
		}
	}
	assert.True(t, externalStillOwnsKeys,
		"external manager's field ownership must be unaffected by release apply")
	t.Log("Map field ownership proved: contributed keys deleted (sole owner released), external keys untouched")
}
