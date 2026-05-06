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

// TestDryRunDetectsAgreedValueCoOwnership proves that dry-run conflict
// detection catches field overlap even when the external manager and the
// Graph write the SAME value. Without dry-run, SSA silently creates
// co-ownership; with dry-run, the overlap is surfaced as a Conflict.
//
// Per 003-ownership § Conflict Detection: "Dry-run apply detects field
// overlap regardless of value agreement — co-ownership is a conflict."
func TestDryRunDetectsAgreedValueCoOwnership(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	cmGVK := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}

	// Step 1: External manager establishes ownership on data.shared-key.
	applyConfigMapAs(t, ns, "dryrun-coown-cm", "external-manager", map[string]string{
		"shared-key": "agreed-value",
	})
	t.Log("External manager owns data.shared-key with value 'agreed-value'")

	// Step 2: Graph tries to write the SAME field with the SAME value (non-force patch).
	// Dry-run should detect the field overlap and surface Conflict.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-dryrun-coown",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "target",
						"patch": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "dryrun-coown-cm"},
							"data": map[string]any{
								"shared-key": "agreed-value",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	graphKey := types.NamespacedName{Name: "test-dryrun-coown", Namespace: ns}

	// Step 3: Wait for Graph to reach Conflict reason.
	require.NoError(t, waitForGraphReadyReason(ctx, k8sClient, graphKey, "Conflict"))
	t.Log("Graph shows Conflict — dry-run detected field overlap despite value agreement")

	// Step 4: Assert Graph is NOT Ready (Status=False, Reason=Conflict).
	g := &unstructured.Unstructured{}
	g.SetGroupVersionKind(GraphGVK)
	require.NoError(t, k8sClient.Get(ctx, graphKey, g))
	assert.Equal(t, "False", graphReadyStatus(g),
		"Graph should be Ready=False when in Conflict state")
	assert.Equal(t, "Conflict", graphReadyReason(g),
		"Graph Ready reason should be Conflict")

	// Step 5: Fix by updating Graph spec to write a DIFFERENT key (no overlap).
	require.NoError(t, updateWithRetry(ctx, k8sClient, GraphGVK, graphKey,
		func(obj *unstructured.Unstructured) {
			unstructured.SetNestedSlice(obj.Object, []any{
				map[string]any{
					"id": "target",
					"patch": map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata":   map[string]any{"name": "dryrun-coown-cm"},
						"data": map[string]any{
							"non-contested": "only-mine",
						},
					},
				},
			}, "spec", "nodes")
		}))
	t.Log("Updated Graph spec to write non-overlapping key")

	// Step 6: Wait for Graph to become Ready.
	require.NoError(t, waitForGraphReady(ctx, k8sClient, graphKey))
	t.Log("Graph is Ready — conflict resolved by removing field overlap")

	// Step 7: Verify the ConfigMap has both keys.
	cm := &unstructured.Unstructured{}
	cm.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "dryrun-coown-cm", Namespace: ns}, cm))
	data, _, _ := unstructured.NestedStringMap(cm.Object, "data")
	assert.Equal(t, "agreed-value", data["shared-key"],
		"external manager's key should still be present")
	assert.Equal(t, "only-mine", data["non-contested"],
		"Graph's non-overlapping key should be applied")
	t.Log("ConfigMap has both external's shared-key and Graph's non-contested key — coexistence proved")
}

// TestDryRunDetectsTemplateLifecycleConflict proves that when two Graphs
// both declare template: nodes targeting the same resource, the second
// Graph's dry-run detects the first Graph's template identity label and
// surfaces a Conflict. Even with disjoint field sets, template ownership
// is exclusive.
//
// Per 003-ownership § Template Exclusivity: "A resource can have at most
// one template owner. The identity label makes this detectable at dry-run
// time."
func TestDryRunDetectsTemplateLifecycleConflict(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	cmGVK := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}

	// Step 1: Create Graph A with a template: node creating ConfigMap "lifecycle-cm".
	graphA := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-dryrun-lifecycle-a",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "target",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "lifecycle-cm"},
							"data":       map[string]any{"keyA": "valueA"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graphA))

	graphAKey := types.NamespacedName{Name: "test-dryrun-lifecycle-a", Namespace: ns}

	// Step 2: Wait for Graph A to become Ready.
	require.NoError(t, waitForGraphReady(ctx, k8sClient, graphAKey))
	t.Log("Graph A is Ready — lifecycle-cm created")

	// Step 3: Verify ConfigMap exists with data.keyA.
	require.NoError(t, waitForField(ctx, k8sClient, cmGVK,
		types.NamespacedName{Name: "lifecycle-cm", Namespace: ns},
		[]string{"data", "keyA"}, "valueA"))
	t.Log("lifecycle-cm has data.keyA=valueA")

	// Step 4: Create Graph B with a template: node targeting the SAME ConfigMap
	// but with DIFFERENT fields.
	graphB := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-dryrun-lifecycle-b",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "target",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "lifecycle-cm"},
							"data":       map[string]any{"keyB": "valueB"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graphB))

	graphBKey := types.NamespacedName{Name: "test-dryrun-lifecycle-b", Namespace: ns}

	// Step 5: Wait for Graph B to reach Conflict reason.
	require.NoError(t, waitForGraphReadyReason(ctx, k8sClient, graphBKey, "Conflict"))
	t.Log("Graph B shows Conflict — dry-run detected Graph A's template identity label")

	// Step 6: Assert Graph B is NOT Ready.
	gB := &unstructured.Unstructured{}
	gB.SetGroupVersionKind(GraphGVK)
	require.NoError(t, k8sClient.Get(ctx, graphBKey, gB))
	assert.Equal(t, "False", graphReadyStatus(gB),
		"Graph B should be Ready=False when in Conflict state")
	assert.Equal(t, "Conflict", graphReadyReason(gB),
		"Graph B Ready reason should be Conflict")

	// Step 7: Delete Graph A.
	require.NoError(t, k8sClient.Delete(ctx, graphA))
	t.Log("Deleted Graph A")

	// Step 8: Wait for Graph A to be fully deleted (finalizer ran, teardown complete).
	// We wait for the Graph resource itself to disappear rather than the ConfigMap,
	// because Graph B is already retrying and will immediately recreate the ConfigMap
	// once Graph A's teardown removes it — making ConfigMap NotFound a transient race.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 60*time.Second, true,
		func(ctx context.Context) (bool, error) {
			obj := &unstructured.Unstructured{}
			obj.SetGroupVersionKind(GraphGVK)
			err := k8sClient.Get(ctx, graphAKey, obj)
			if apierrors.IsNotFound(err) {
				return true, nil
			}
			return false, nil
		}))
	t.Log("Graph A fully deleted (teardown complete)")

	// Step 9: Wait for Graph B to become Ready. Graph B retries on conflict
	// with exponential backoff; once Graph A's teardown removes the identity
	// label from the ConfigMap (or deletes it), Graph B's next reconcile succeeds.
	require.NoError(t, waitForGraphReady(ctx, k8sClient, graphBKey, 60*time.Second))
	t.Log("Graph B is Ready — now the sole template owner of lifecycle-cm")

	// Step 10: Verify ConfigMap exists with data.keyB (Graph B's fields).
	cm := &unstructured.Unstructured{}
	cm.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "lifecycle-cm", Namespace: ns}, cm))
	data, _, _ := unstructured.NestedStringMap(cm.Object, "data")
	assert.Equal(t, "valueB", data["keyB"],
		"lifecycle-cm should have Graph B's data.keyB=valueB")
	assert.Empty(t, data["keyA"],
		"lifecycle-cm should NOT have Graph A's data.keyA (Graph A was deleted)")
	t.Log("lifecycle-cm owned by Graph B with data.keyB=valueB — template lifecycle conflict detection proved")
}
