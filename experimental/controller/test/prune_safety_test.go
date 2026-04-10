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

// Prune safety tests prove that the controller does not prune resources when
// their absence is uncertain (design 004-graph-execution § Prune):
//
//   "Not all absent resources should be pruned:
//    Uncertain absence — a dependency is Pending, Conflict, or Error. The
//    resource might appear once the blocker resolves. Not safe to prune."
//
// Each blocked state is tested independently — the prune decision might
// legitimately differ between them.

// TestPruneSafetyPendingBlocksPrune proves that when a dependency is
// data-pending (watched resource missing a field), resources that would
// depend on it via includeWhen are NOT pruned — they survive until the
// blocker resolves.
//
// Setup:
//   - Source ConfigMap with field "toggle" controlling includeWhen
//   - Conditional node included when toggle == "true"
//   - Start with toggle=true → conditional resource created
//   - Remove the toggle field entirely → source is in scope but conditional
//     cannot evaluate includeWhen → uncertain absence
//   - Assert conditional resource is NOT pruned
//   - Restore toggle=true → conditional resource still exists
func TestPruneSafetyPendingBlocksPrune(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	cmGVK := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}

	// Pre-create the control ConfigMap with toggle=true.
	control := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "prune-safety-control",
				"namespace": ns,
			},
			"data": map[string]any{
				"toggle": "true",
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, control))

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-prune-pending",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "control",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "prune-safety-control"},
						},
					},
					map[string]any{
						"id": "conditional",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "prune-conditional"},
							"data":       map[string]any{"state": "alive"},
						},
						"includeWhen": []any{"${control.data.toggle == 'true'}"},
					},
					map[string]any{
						"id": "always",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "prune-always"},
							"data":       map[string]any{"state": "permanent"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for convergence — conditional resource should be created.
	conditional := &unstructured.Unstructured{}
	conditional.SetGroupVersionKind(cmGVK)
	require.NoError(t, waitForResource(ctx, k8sClient,
		types.NamespacedName{Name: "prune-conditional", Namespace: ns}, conditional))
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-prune-pending", Namespace: ns}))
	t.Log("Conditional resource created, Graph ready")

	// Remove the toggle field entirely — this makes includeWhen evaluation
	// fail (data-pending), creating uncertain absence.
	latest := &unstructured.Unstructured{}
	latest.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "prune-safety-control", Namespace: ns}, latest))
	// Remove the data section entirely
	unstructured.RemoveNestedField(latest.Object, "data")
	require.NoError(t, k8sClient.Update(ctx, latest))
	t.Log("Removed toggle field — includeWhen cannot evaluate")

	// Wait for the Graph to enter non-Ready state.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 10*time.Second, true,
		func(ctx context.Context) (bool, error) {
			g := &unstructured.Unstructured{}
			g.SetGroupVersionKind(GraphGVK)
			if err := k8sClient.Get(ctx,
				types.NamespacedName{Name: "test-prune-pending", Namespace: ns}, g); err != nil {
				return false, nil
			}
			return !graphReady(g), nil
		}))
	t.Log("Graph entered non-Ready state (data-pending)")

	// THE KEY ASSERTION: the conditional resource should NOT be pruned.
	// Give the controller time to process the change and verify it survives.
	require.NoError(t, waitForSettle(ctx, k8sClient, GraphGVK,
		types.NamespacedName{Name: "test-prune-pending", Namespace: ns}))
	check := &unstructured.Unstructured{}
	check.SetGroupVersionKind(cmGVK)
	err := k8sClient.Get(ctx,
		types.NamespacedName{Name: "prune-conditional", Namespace: ns}, check)
	assert.NoError(t, err,
		"conditional resource should NOT be pruned during uncertain absence (data-pending)")
	t.Log("Conditional resource survived data-pending — prune safety proved")

	// Restore the toggle field — Graph should recover.
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "prune-safety-control", Namespace: ns}, latest))
	unstructured.SetNestedField(latest.Object, "true", "data", "toggle")
	require.NoError(t, k8sClient.Update(ctx, latest))

	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-prune-pending", Namespace: ns}))
	t.Log("Graph recovered after toggle restored")
}

// TestPruneSafetyConflictBlocksPrune proves that when a node is in Conflict
// state (409 from competing SSA field manager), dependent resources that
// were previously applied are not pruned.
//
// Design 004-graph-execution § Plan States: Conflict blocks dependents.
// Design 004-graph-execution § Prune: "Uncertain absence — a dependency is
// Pending, Conflict, or Error."
//
// Setup:
//   - Pre-create upstream resource owned by external manager
//   - Create Graph with upstream (will 409) + independent resource
//   - Graph enters Conflict state. Independent resource is created.
//   - Verify the independent resource survives despite Conflict on upstream.
func TestPruneSafetyConflictBlocksPrune(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	cmGVK := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}

	// Pre-create the upstream resource with an external field manager.
	// The Graph will try to write a different value and get a 409.
	applyConfigMapAs(t, ns, "conflict-prune-upstream", "external-manager", map[string]string{
		"key": "external-value",
	})
	t.Log("External manager owns upstream resource")

	// Create Graph: conflicted upstream + independent resource.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-prune-conflict",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "upstream",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "conflict-prune-upstream"},
							"data":       map[string]any{"key": "graph-wants-different-value"},
						},
					},
					map[string]any{
						"id": "independent",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "conflict-prune-independent"},
							"data":       map[string]any{"state": "alive"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for Conflict state.
	require.NoError(t, waitForGraphReadyReason(ctx, k8sClient,
		types.NamespacedName{Name: "test-prune-conflict", Namespace: ns}, "Conflict"))
	t.Log("Graph entered Conflict state")

	// Independent resource should be created despite upstream conflict.
	indep := &unstructured.Unstructured{}
	indep.SetGroupVersionKind(cmGVK)
	require.NoError(t, waitForResource(ctx, k8sClient,
		types.NamespacedName{Name: "conflict-prune-independent", Namespace: ns}, indep))
	t.Log("Independent resource created despite upstream Conflict")

	// THE KEY ASSERTION: after settling, the independent resource should NOT
	// be pruned. Conflict is a blocked state — nothing should be pruned
	// because of it.
	require.NoError(t, waitForSettle(ctx, k8sClient, GraphGVK,
		types.NamespacedName{Name: "test-prune-conflict", Namespace: ns}))
	check := &unstructured.Unstructured{}
	check.SetGroupVersionKind(cmGVK)
	err := k8sClient.Get(ctx,
		types.NamespacedName{Name: "conflict-prune-independent", Namespace: ns}, check)
	assert.NoError(t, err,
		"independent resource should NOT be pruned during Conflict state")
	t.Log("Independent resource survived Conflict — prune safety proved")
}
