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

// ---------------------------------------------------------------------------
// Surgical Release — design 003-ownership
// ---------------------------------------------------------------------------

// TestSurgicalReleasePatchForceApply proves that force-apply on a patch node
// surgically releases overlapping fields from co-owners while preserving their
// non-overlapping fields. This is the primary surgical release test.
//
// Setup:
//   - Pre-create a ConfigMap under "external-manager" with two keys:
//     "shared-key" (will overlap with kro) and "external-only" (no overlap).
//   - Create a Graph with a patch: node + lifecycle.apply: Force that writes
//     data.shared-key (overlapping with external-manager).
//   - Verify kro takes ownership of shared-key, external-manager retains
//     external-only, and external-manager's claim on shared-key is removed.
func TestSurgicalReleasePatchForceApply(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	gvk := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}
	cmName := "surgical-target"
	graphName := "test-surgical-release"
	key := types.NamespacedName{Name: cmName, Namespace: ns}

	// Pre-create ConfigMap under external-manager with two keys.
	applyConfigMapAs(t, ns, cmName, "external-manager", map[string]string{
		"shared-key":    "external-value",
		"external-only": "keep-me",
	})

	// Verify external-manager is present before Graph creation.
	pre := &unstructured.Unstructured{}
	pre.SetGroupVersionKind(gvk)
	require.NoError(t, k8sClient.Get(ctx, key, pre))
	var foundExternal bool
	for _, mf := range pre.GetManagedFields() {
		if mf.Manager == "external-manager" {
			foundExternal = true
		}
	}
	require.True(t, foundExternal, "external-manager should own fields before test")

	// Graph with patch + Force that overlaps on shared-key.
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
						"id": "target",
						"lifecycle": map[string]any{
							"apply": "Force",
						},
						"patch": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": cmName,
							},
							"data": map[string]any{
								"shared-key": "kro-value",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: graphName, Namespace: ns}))
	require.NoError(t, waitForSettle(ctx, k8sClient, gvk, key))

	// Poll until surgical release has completed (may fire on subsequent reconcile).
	graphManager := graphName + "." + ns + ".internal.kro.run"
	err := wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, 30*time.Second, true, func(pollCtx context.Context) (bool, error) {
		cm := &unstructured.Unstructured{}
		cm.SetGroupVersionKind(gvk)
		if err := k8sClient.Get(pollCtx, key, cm); err != nil {
			return false, err
		}

		// Check data values.
		data, _, _ := unstructured.NestedStringMap(cm.Object, "data")
		if data["shared-key"] != "kro-value" {
			return false, nil
		}
		if data["external-only"] != "keep-me" {
			return false, nil
		}

		// Check managedFields: external-manager must not own shared-key.
		for _, mf := range cm.GetManagedFields() {
			if mf.Manager == "external-manager" && mf.FieldsV1 != nil {
				fields := string(mf.FieldsV1.Raw)
				if strings.Contains(fields, "shared-key") {
					return false, nil // not yet released
				}
			}
		}

		// Check kro owns shared-key.
		for _, mf := range cm.GetManagedFields() {
			if mf.Manager == graphManager && mf.FieldsV1 != nil {
				fields := string(mf.FieldsV1.Raw)
				if strings.Contains(fields, "shared-key") {
					return true, nil
				}
			}
		}
		return false, nil
	})
	require.NoError(t, err, "surgical release did not converge")

	// Final assertions on the settled state.
	result := &unstructured.Unstructured{}
	result.SetGroupVersionKind(gvk)
	require.NoError(t, k8sClient.Get(ctx, key, result))

	data, _, _ := unstructured.NestedStringMap(result.Object, "data")
	assert.Equal(t, "kro-value", data["shared-key"], "kro should own shared-key value")
	assert.Equal(t, "keep-me", data["external-only"], "non-overlapping field must be preserved")

	// external-manager must still be in managedFields (partial release, not eviction).
	var externalPresent bool
	var externalOwnsShared, externalOwnsOnly bool
	for _, mf := range result.GetManagedFields() {
		if mf.Manager == "external-manager" && mf.FieldsV1 != nil {
			externalPresent = true
			fields := string(mf.FieldsV1.Raw)
			if strings.Contains(fields, "shared-key") {
				externalOwnsShared = true
			}
			if strings.Contains(fields, "external-only") {
				externalOwnsOnly = true
			}
		}
	}
	assert.True(t, externalPresent, "external-manager should still be in managedFields (not fully evicted)")
	assert.False(t, externalOwnsShared, "external-manager should NOT own shared-key after surgical release")
	assert.True(t, externalOwnsOnly, "external-manager should still own external-only")

	// kro's manager must own shared-key.
	var kroOwnsShared bool
	for _, mf := range result.GetManagedFields() {
		if mf.Manager == graphManager && mf.FieldsV1 != nil {
			fields := string(mf.FieldsV1.Raw)
			if strings.Contains(fields, "shared-key") {
				kroOwnsShared = true
			}
		}
	}
	assert.True(t, kroOwnsShared, "kro's field manager should own shared-key")

	t.Log("Surgical release correctly removed co-owner's claim on overlapping fields while preserving non-overlapping fields")
}

// TestSurgicalReleaseFullOverlapFallsBackToEviction proves that when a
// co-owner's fields ALL overlap with the patch node's fields, surgical release
// falls back to full eviction (removes the co-owner entirely from
// managedFields).
func TestSurgicalReleaseFullOverlapFallsBackToEviction(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	gvk := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}
	cmName := "surgical-full-overlap"
	graphName := "test-surgical-evict"
	key := types.NamespacedName{Name: cmName, Namespace: ns}

	// Pre-create ConfigMap under overlap-manager with a single key
	// that will be fully overlapped by kro.
	applyConfigMapAs(t, ns, cmName, "overlap-manager", map[string]string{
		"only-field": "external-value",
	})

	// Verify overlap-manager present.
	pre := &unstructured.Unstructured{}
	pre.SetGroupVersionKind(gvk)
	require.NoError(t, k8sClient.Get(ctx, key, pre))
	var found bool
	for _, mf := range pre.GetManagedFields() {
		if mf.Manager == "overlap-manager" {
			found = true
		}
	}
	require.True(t, found, "overlap-manager should own fields before test")

	// Graph with patch + Force writing the same key.
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
						"id": "target",
						"lifecycle": map[string]any{
							"apply": "Force",
						},
						"patch": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": cmName,
							},
							"data": map[string]any{
								"only-field": "kro-value",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: graphName, Namespace: ns}))
	require.NoError(t, waitForSettle(ctx, k8sClient, gvk, key))

	// Poll until overlap-manager is fully evicted.
	err := wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, 30*time.Second, true, func(pollCtx context.Context) (bool, error) {
		cm := &unstructured.Unstructured{}
		cm.SetGroupVersionKind(gvk)
		if err := k8sClient.Get(pollCtx, key, cm); err != nil {
			return false, err
		}

		data, _, _ := unstructured.NestedStringMap(cm.Object, "data")
		if data["only-field"] != "kro-value" {
			return false, nil
		}

		for _, mf := range cm.GetManagedFields() {
			if mf.Manager == "overlap-manager" {
				return false, nil // still present, not yet evicted
			}
		}
		return true, nil
	})
	require.NoError(t, err, "full overlap eviction did not converge")

	// Final assertion.
	result := &unstructured.Unstructured{}
	result.SetGroupVersionKind(gvk)
	require.NoError(t, k8sClient.Get(ctx, key, result))

	data, _, _ := unstructured.NestedStringMap(result.Object, "data")
	assert.Equal(t, "kro-value", data["only-field"], "kro should own the field value")

	for _, mf := range result.GetManagedFields() {
		assert.NotEqual(t, "overlap-manager", mf.Manager,
			"overlap-manager should be fully evicted when all fields overlap")
	}

	t.Log("Full overlap correctly falls back to eviction — co-owner entirely removed from managedFields")
}

// TestSurgicalReleaseMultipleCoOwners proves that surgical release handles
// multiple third-party managers co-owning fields with the patch node. Each
// co-owner's overlapping fields are released while their non-overlapping
// fields are preserved.
func TestSurgicalReleaseMultipleCoOwners(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	gvk := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}
	cmName := "surgical-multi-owners"
	graphName := "test-surgical-multi"
	key := types.NamespacedName{Name: cmName, Namespace: ns}

	// manager-a owns a-shared and a-only.
	applyConfigMapAs(t, ns, cmName, "manager-a", map[string]string{
		"a-shared": "a-val",
		"a-only":   "a-keep",
	})
	// manager-b owns b-shared and b-only.
	applyConfigMapAs(t, ns, cmName, "manager-b", map[string]string{
		"b-shared": "b-val",
		"b-only":   "b-keep",
	})

	// Verify both managers present.
	pre := &unstructured.Unstructured{}
	pre.SetGroupVersionKind(gvk)
	require.NoError(t, k8sClient.Get(ctx, key, pre))
	managers := map[string]bool{}
	for _, mf := range pre.GetManagedFields() {
		managers[mf.Manager] = true
	}
	require.True(t, managers["manager-a"], "manager-a should be present before test")
	require.True(t, managers["manager-b"], "manager-b should be present before test")

	// Graph with patch + Force writing both shared keys.
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
						"id": "target",
						"lifecycle": map[string]any{
							"apply": "Force",
						},
						"patch": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": cmName,
							},
							"data": map[string]any{
								"a-shared": "kro-a",
								"b-shared": "kro-b",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: graphName, Namespace: ns}))
	require.NoError(t, waitForSettle(ctx, k8sClient, gvk, key))

	// Poll until surgical release completes for both managers.
	err := wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, 30*time.Second, true, func(pollCtx context.Context) (bool, error) {
		cm := &unstructured.Unstructured{}
		cm.SetGroupVersionKind(gvk)
		if err := k8sClient.Get(pollCtx, key, cm); err != nil {
			return false, err
		}

		data, _, _ := unstructured.NestedStringMap(cm.Object, "data")
		if data["a-shared"] != "kro-a" || data["b-shared"] != "kro-b" {
			return false, nil
		}

		// Check that neither manager owns their shared key.
		for _, mf := range cm.GetManagedFields() {
			if mf.FieldsV1 == nil {
				continue
			}
			fields := string(mf.FieldsV1.Raw)
			if mf.Manager == "manager-a" && strings.Contains(fields, "a-shared") {
				return false, nil
			}
			if mf.Manager == "manager-b" && strings.Contains(fields, "b-shared") {
				return false, nil
			}
		}
		return true, nil
	})
	require.NoError(t, err, "surgical release for multiple co-owners did not converge")

	// Final assertions.
	result := &unstructured.Unstructured{}
	result.SetGroupVersionKind(gvk)
	require.NoError(t, k8sClient.Get(ctx, key, result))

	data, _, _ := unstructured.NestedStringMap(result.Object, "data")
	assert.Equal(t, "kro-a", data["a-shared"], "kro should own a-shared value")
	assert.Equal(t, "kro-b", data["b-shared"], "kro should own b-shared value")
	assert.Equal(t, "a-keep", data["a-only"], "manager-a's non-overlapping field must be preserved")
	assert.Equal(t, "b-keep", data["b-only"], "manager-b's non-overlapping field must be preserved")

	// manager-a: still present, owns a-only, does NOT own a-shared.
	var aPresent, aOwnsShared, aOwnsOnly bool
	// manager-b: still present, owns b-only, does NOT own b-shared.
	var bPresent, bOwnsShared, bOwnsOnly bool
	for _, mf := range result.GetManagedFields() {
		if mf.FieldsV1 == nil {
			continue
		}
		fields := string(mf.FieldsV1.Raw)
		switch mf.Manager {
		case "manager-a":
			aPresent = true
			if strings.Contains(fields, "a-shared") {
				aOwnsShared = true
			}
			if strings.Contains(fields, "a-only") {
				aOwnsOnly = true
			}
		case "manager-b":
			bPresent = true
			if strings.Contains(fields, "b-shared") {
				bOwnsShared = true
			}
			if strings.Contains(fields, "b-only") {
				bOwnsOnly = true
			}
		}
	}

	assert.True(t, aPresent, "manager-a should still be in managedFields")
	assert.False(t, aOwnsShared, "manager-a should NOT own a-shared after surgical release")
	assert.True(t, aOwnsOnly, "manager-a should still own a-only")

	assert.True(t, bPresent, "manager-b should still be in managedFields")
	assert.False(t, bOwnsShared, "manager-b should NOT own b-shared after surgical release")
	assert.True(t, bOwnsOnly, "manager-b should still own b-only")

	t.Log("Surgical release correctly handled multiple co-owners — each manager retains only non-overlapping fields")
}

// TestSurgicalReleaseTeardown proves that when a Graph with a patch + Force
// node is deleted, kro releases ALL its fields (full release). After teardown
// the ConfigMap still exists (patch never deletes) and the external-manager's
// non-overlapping field remains.
func TestSurgicalReleaseTeardown(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	gvk := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}
	graphGVK := schema.GroupVersionKind{Group: "experimental.kro.run", Version: "v1alpha1", Kind: "Graph"}
	cmName := "surgical-teardown"
	graphName := "test-surgical-teardown"
	key := types.NamespacedName{Name: cmName, Namespace: ns}
	graphKey := types.NamespacedName{Name: graphName, Namespace: ns}

	// Pre-create ConfigMap under external-manager.
	applyConfigMapAs(t, ns, cmName, "external-manager", map[string]string{
		"shared-key":    "external-value",
		"external-only": "keep-me",
	})

	// Graph with patch + Force overlapping on shared-key.
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
						"id": "target",
						"lifecycle": map[string]any{
							"apply": "Force",
						},
						"patch": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": cmName,
							},
							"data": map[string]any{
								"shared-key": "kro-value",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	require.NoError(t, waitForGraphReady(ctx, k8sClient, graphKey))
	require.NoError(t, waitForSettle(ctx, k8sClient, gvk, key))

	// Confirm kro took ownership before teardown.
	mid := &unstructured.Unstructured{}
	mid.SetGroupVersionKind(gvk)
	require.NoError(t, k8sClient.Get(ctx, key, mid))
	midData, _, _ := unstructured.NestedStringMap(mid.Object, "data")
	require.Equal(t, "kro-value", midData["shared-key"], "kro should own shared-key before teardown")

	// Delete the Graph.
	graphObj := &unstructured.Unstructured{}
	graphObj.SetGroupVersionKind(graphGVK)
	graphObj.SetName(graphName)
	graphObj.SetNamespace(ns)
	require.NoError(t, k8sClient.Delete(ctx, graphObj))

	// Wait for Graph to be fully deleted.
	require.NoError(t, waitForDeletion(ctx, k8sClient, graphGVK, graphKey))

	// ConfigMap must still exist (patch nodes never delete the resource).
	result := &unstructured.Unstructured{}
	result.SetGroupVersionKind(gvk)
	require.NoError(t, k8sClient.Get(ctx, key, result))

	data, _, _ := unstructured.NestedStringMap(result.Object, "data")

	// After kro releases, shared-key has no remaining owner (external-manager
	// was surgically released from it earlier). SSA removes unowned fields,
	// so shared-key should be absent.
	assert.Empty(t, data["shared-key"],
		"shared-key should be absent after both owners released it")

	// external-only remains under external-manager.
	assert.Equal(t, "keep-me", data["external-only"],
		"external-manager's non-overlapping field must survive teardown")

	// Verify kro's field manager is gone from managedFields.
	graphManager := graphName + "." + ns + ".internal.kro.run"
	for _, mf := range result.GetManagedFields() {
		assert.NotEqual(t, graphManager, mf.Manager,
			"kro's field manager should be removed after Graph deletion")
	}

	t.Log("Teardown correctly released kro's fields — ConfigMap persists with only external-manager's fields")
}
