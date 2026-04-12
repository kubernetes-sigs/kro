package graphcontroller_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
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
// (experimental.kro.run/<ns>/<name>), so SSA field ownership is scoped per
// Graph. Two Graphs writing disjoint field sets on the same resource must not
// produce a 409 conflict, and each Graph's fields must persist independently.
func TestMultiGraphFieldCoexistence(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	cmGVK := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}

	// Pre-create the shared resource. Both Graphs will detect Contribute shape
	// (resource exists on first reconcile → Contribute, not Owns).
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
						"template": map[string]any{
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
						"template": map[string]any{
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
	latestA := &unstructured.Unstructured{}
	latestA.SetGroupVersionKind(GraphGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "test-coexist-a", Namespace: ns}, latestA))
	unstructured.SetNestedSlice(latestA.Object, []any{
		map[string]any{
			"id": "contribute",
			"template": map[string]any{
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
	require.NoError(t, k8sClient.Update(ctx, latestA))
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
						"template": map[string]any{
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
						"template": map[string]any{
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

	// THE KEY ASSERTION: The external object should NOT have an Owns identity
	// label from our Graph. Contributions are partial — they set a "contributes"
	// role, not "owns".
	extLabels := updatedExternal.GetLabels()
	// Check that no identity label for this graph has role "owns"
	for key, val := range extLabels {
		if strings.HasSuffix(key, ".test-contribution."+ns+".internal.kro.run/reference") {
			assert.NotEqual(t, "owns", val,
				"contribution should NOT set 'owns' role label on external object")
		}
	}

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
	latest := &unstructured.Unstructured{}
	latest.SetGroupVersionKind(GraphGVK)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "test-pruning", Namespace: ns}, latest))

	unstructured.SetNestedSlice(latest.Object, []any{
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
	require.NoError(t, k8sClient.Update(ctx, latest))

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
// template is removed from the Graph spec. Owns shape would delete it.
//
// Design 003-ownership § Template Shapes: "Owns — Creates the resource if
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
						"template": map[string]any{
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
	latest := &unstructured.Unstructured{}
	latest.SetGroupVersionKind(GraphGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "test-contribute-shape", Namespace: ns}, latest))
	unstructured.SetNestedSlice(latest.Object, []any{}, "spec", "nodes")
	require.NoError(t, k8sClient.Update(ctx, latest))
	t.Log("Removed both nodes from spec — prune triggered")

	// Wait for the owned resource to be deleted (Owns shape → delete on prune).
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true,
		func(ctx context.Context) (bool, error) {
			check := &unstructured.Unstructured{}
			check.SetGroupVersionKind(cmGVK)
			err := k8sClient.Get(ctx,
				types.NamespacedName{Name: "contribute-owned", Namespace: ns}, check)
			return err != nil, nil // gone = true
		}))
	t.Log("Owned resource deleted on prune — Owns shape confirmed")

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
