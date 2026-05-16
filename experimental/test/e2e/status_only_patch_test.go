package graphcontroller_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var simpleAppGVK = schema.GroupVersionKind{Group: "test.kro.run", Version: "v1alpha1", Kind: "SimpleApp"}

// applySimpleAppAs creates or updates a SimpleApp via SSA with the given field
// manager. This establishes field ownership for the specified manager.
func applySimpleAppAs(t *testing.T, ns, name, fieldManager string, spec map[string]any) {
	t.Helper()
	payload := map[string]any{
		"apiVersion": "test.kro.run/v1alpha1",
		"kind":       "SimpleApp",
		"metadata": map[string]any{
			"name":      name,
			"namespace": ns,
		},
		"spec": spec,
	}
	raw, err := json.Marshal(payload)
	require.NoError(t, err)

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(simpleAppGVK)
	obj.SetName(name)
	obj.SetNamespace(ns)
	require.NoError(t, k8sClient.Patch(ctx, obj, client.RawPatch(
		types.ApplyPatchType, raw),
		client.ForceOwnership,
		client.FieldOwner(fieldManager),
	))
}

// TestPatch_StatusOnlyDoesNotClaimMainResource verifies that a patch node
// containing only identity fields (apiVersion, kind, metadata) plus status
// does NOT register a field manager on the main resource. This prevents
// ownership conflicts when another graph also manages the same resource.
//
// Regression: before the fix, the kindStatus patch (which only writes
// status) would SSA-apply identity fields to the main resource, causing
// the kind controller's field manager to conflict with the parent graph.
func TestPatch_StatusOnlyDoesNotClaimMainResource(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// 1. Pre-create a SimpleApp resource (has status subresource).
	// Apply it under a specific field manager to simulate the "parent graph" owning it.
	parentManager := "parent-graph." + ns + ".internal.kro.run"
	applySimpleAppAs(t, ns, "status-target", parentManager, map[string]any{
		"name": "owned-by-parent",
	})

	// 2. Create a Graph with a status-only patch node (like kindStatus).
	graph := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "experimental.kro.run/v1alpha1",
		"kind":       "Graph",
		"metadata":   map[string]any{"name": "status-only-patch", "namespace": ns},
		"spec": map[string]any{
			"nodes": []any{
				map[string]any{
					"id": "statuspatch",
					"patch": map[string]any{
						"apiVersion": "test.kro.run/v1alpha1",
						"kind":       "SimpleApp",
						"metadata":   map[string]any{"name": "status-target"},
						"status":     map[string]any{"message": "patched-status"},
					},
				},
			},
		},
	}}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// 3. Wait for graph to converge.
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "status-only-patch", Namespace: ns}))

	// 4. Verify status was applied.
	check := &unstructured.Unstructured{}
	check.SetGroupVersionKind(simpleAppGVK)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "status-target", Namespace: ns}, check))

	statusMap, _, _ := unstructured.NestedMap(check.Object, "status")
	assert.Equal(t, "patched-status", statusMap["message"])

	// 5. Verify that the graph's main field manager is NOT present — only the
	//    status sub-manager should exist. The parent manager should still own
	//    the main resource exclusively.
	graphManager := "status-only-patch." + ns + ".internal.kro.run"
	statusManager := graphManager + ".status"

	var hasGraphMainManager, hasGraphStatusManager, hasParentManager bool
	for _, mf := range check.GetManagedFields() {
		switch mf.Manager {
		case graphManager:
			if mf.Subresource == "" {
				hasGraphMainManager = true
			}
		case statusManager:
			if mf.Subresource == "status" {
				hasGraphStatusManager = true
			}
		case parentManager:
			hasParentManager = true
		}
	}

	assert.False(t, hasGraphMainManager,
		"status-only patch should NOT register a main-object field manager")
	assert.True(t, hasGraphStatusManager,
		"status-only patch should register a status sub-manager")
	assert.True(t, hasParentManager,
		"parent field manager should still own the main resource")
}
