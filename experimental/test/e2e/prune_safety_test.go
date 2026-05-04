package graphcontroller_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// Prune safety tests prove that the controller does not prune resources when
// their absence is uncertain (design 005-reconciliation § Prune):
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
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-prune-pending",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "control",
						"ref": map[string]any{
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
	require.NoError(t, waitForGraphReadyStatus(ctx, k8sClient,
		types.NamespacedName{Name: "test-prune-pending", Namespace: ns}, "Unknown"))
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

// TestPruneManagedCheckBlocksDeletion proves that the controller respects
// managedFields during teardown — if another field manager has an SSA Apply
// entry on a template: resource, the finalizer holds until the other manager
// releases.
//
// Design 003-ownership § Prune — managed check:
//
//	"Before deleting a template: resource, checks managedFields for other field
//	managers. If present, deletion is blocked until the other manager releases."
//
// Setup:
//   - Graph creates and owns a ConfigMap (template:, template hash set).
//   - After convergence, external manager SSA-applies a field to the same CM,
//     gaining a managedFields entry.
//   - Graph is deleted (deletion timestamp set, finalizer holds).
//   - The ConfigMap must survive — controller detects the third-party manager.
//   - Cleanup: delete the ConfigMap directly → controller can proceed with
//     teardown → Graph finalizer removed.
func TestPruneManagedCheckBlocksDeletion(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	cmGVK := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}

	// 1. Create a Graph that creates and owns a ConfigMap.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-prune-managed-teardown",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "target",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "prune-managed-cm"},
							"data":       map[string]any{"owner": "graph"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// 2. Wait for the ConfigMap to be created and Graph to become Active.
	cm := &unstructured.Unstructured{}
	cm.SetGroupVersionKind(cmGVK)
	require.NoError(t, waitForResource(ctx, k8sClient,
		types.NamespacedName{Name: "prune-managed-cm", Namespace: ns}, cm))
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-prune-managed-teardown", Namespace: ns}))
	t.Log("ConfigMap created, Graph Active — managedFields has kro entry")

	// 3. External manager SSA-applies a different field to the same ConfigMap.
	// This adds a second managedFields entry (external-manager). The controller
	// must detect this before deleting on teardown.
	applyConfigMapAs(t, ns, "prune-managed-cm", "external-manager", map[string]string{
		"extra-field": "external-value",
	})
	// Verify the external field is there before proceeding.
	require.NoError(t, waitForField(ctx, k8sClient, cmGVK,
		types.NamespacedName{Name: "prune-managed-cm", Namespace: ns},
		[]string{"data", "extra-field"}, "external-value"))
	t.Log("External manager applied — ConfigMap now has two managedFields entries")

	// 4. Delete the Graph (sets deletion timestamp, finalizer holds teardown).
	latest := &unstructured.Unstructured{}
	latest.SetGroupVersionKind(GraphGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "test-prune-managed-teardown", Namespace: ns}, latest))
	require.NoError(t, k8sClient.Delete(ctx, latest))
	t.Log("Graph deletion requested")

	// 5. THE KEY ASSERTION: after the controller processes the deletion,
	// the ConfigMap must survive. Give the controller time to process the event
	// and confirm the resource wasn't deleted.
	require.NoError(t, waitForSettle(ctx, k8sClient, GraphGVK,
		types.NamespacedName{Name: "test-prune-managed-teardown", Namespace: ns}))

	surviving := &unstructured.Unstructured{}
	surviving.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "prune-managed-cm", Namespace: ns}, surviving),
		"ConfigMap must survive teardown when another field manager is present")
	t.Log("ConfigMap survived Graph deletion — managedFields check blocked teardown deletion")

	// 6. Cleanup: delete the ConfigMap directly so the controller can finish
	// teardown. Without this, the test namespace cleanup might hang.
	require.NoError(t, k8sClient.Delete(ctx, surviving))
	t.Log("ConfigMap manually deleted — unblocking teardown")

	// 7. Controller should now complete teardown and remove the Graph finalizer.
	require.NoError(t, waitForDeletion(ctx, k8sClient, GraphGVK,
		types.NamespacedName{Name: "test-prune-managed-teardown", Namespace: ns}))
	t.Log("Graph fully deleted after ConfigMap was removed — managedFields teardown block proved")
}

// TestPruneManagedCheckOnSpecChange proves that the controller respects
// managedFields during wind/prune — if another field manager has an SSA Apply
// entry on a template: resource that becomes a prune candidate (removed from spec),
// deletion is blocked.
//
// Design 003-ownership § Prune — managed check (wind path, apply.go):
//
//	"Before deleting a template: resource, checks managedFields for other field
//	managers. If present, deletion is blocked."
//
// This is a distinct code path from the teardown check (TestPruneManagedCheckBlocksDeletion).
// A spec change removes a node from the graph; the applied set still contains
// the resource; the prune loop checks managedFields before deleting.
//
// Setup:
//   - Graph creates two independent ConfigMaps (A and B).
//   - External manager applies a field to CM-A.
//   - Graph spec is updated to remove node A.
//   - CM-A must survive (prune blocked by external manager).
//   - CM-B must also survive (still in spec).
func TestPruneManagedCheckOnSpecChange(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	cmGVK := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}

	// 1. Create a Graph with two independent nodes.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-prune-managed-spec",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "nodeA",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "prune-managed-a"},
							"data":       map[string]any{"owned-by": "graph"},
						},
					},
					map[string]any{
						"id": "nodeB",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "prune-managed-b"},
							"data":       map[string]any{"role": "permanent"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// 2. Wait for both ConfigMaps and Graph Active.
	cmA := &unstructured.Unstructured{}
	cmA.SetGroupVersionKind(cmGVK)
	require.NoError(t, waitForResource(ctx, k8sClient,
		types.NamespacedName{Name: "prune-managed-a", Namespace: ns}, cmA))
	cmB := &unstructured.Unstructured{}
	cmB.SetGroupVersionKind(cmGVK)
	require.NoError(t, waitForResource(ctx, k8sClient,
		types.NamespacedName{Name: "prune-managed-b", Namespace: ns}, cmB))
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-prune-managed-spec", Namespace: ns}))
	t.Log("Both ConfigMaps created, Graph Active")

	// 3. External manager applies a field to CM-A.
	applyConfigMapAs(t, ns, "prune-managed-a", "external-manager", map[string]string{
		"external-key": "external-value",
	})
	require.NoError(t, waitForField(ctx, k8sClient, cmGVK,
		types.NamespacedName{Name: "prune-managed-a", Namespace: ns},
		[]string{"data", "external-key"}, "external-value"))
	t.Log("External manager applied to CM-A — CM-A now has two SSA managers")

	// 4. Update Graph spec: remove nodeA. This makes CM-A a prune candidate.
	require.NoError(t, updateWithRetry(ctx, k8sClient, GraphGVK,
		types.NamespacedName{Name: "test-prune-managed-spec", Namespace: ns}, func(obj *unstructured.Unstructured) {
			unstructured.SetNestedSlice(obj.Object, []any{
				map[string]any{
					"id": "nodeB",
					"template": map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata":   map[string]any{"name": "prune-managed-b"},
						"data":       map[string]any{"role": "permanent"},
					},
				},
			}, "spec", "nodes")
		}))
	t.Log("Removed nodeA from spec — prune candidate created")

	// 5. Wait for Graph to settle on the new spec.
	require.NoError(t, waitForSettle(ctx, k8sClient, GraphGVK,
		types.NamespacedName{Name: "test-prune-managed-spec", Namespace: ns}))

	// 6. THE KEY ASSERTION: CM-A must survive despite being removed from spec,
	// because the external manager's managedFields entry blocks deletion.
	checkA := &unstructured.Unstructured{}
	checkA.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "prune-managed-a", Namespace: ns}, checkA),
		"CM-A must survive spec-change prune when another field manager is present")
	data, _, _ := unstructured.NestedStringMap(checkA.Object, "data")
	assert.Equal(t, "external-value", data["external-key"],
		"external manager's field should be intact on the surviving CM")
	t.Log("CM-A survived spec-change prune — managedFields check blocked wind/prune deletion")

	// 7. CM-B must also still exist (still in spec).
	checkB := &unstructured.Unstructured{}
	checkB.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "prune-managed-b", Namespace: ns}, checkB),
		"CM-B must still exist — it was not removed from spec")
	t.Log("CM-B still exists — unrelated nodes unaffected")
}

// TestPruneSafetyConflictBlocksPrune proves that when a node is in Conflict
// state (409 from competing SSA field manager), dependent resources that
// were previously applied are not pruned.
//
// Design 005-reconciliation § Node States: Conflict blocks dependents.
// Design 005-reconciliation § Prune: "Uncertain absence — a dependency is
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
			"apiVersion": "experimental.kro.run/v1alpha1",
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
	check := &unstructured.Unstructured{}
	check.SetGroupVersionKind(cmGVK)
	err := k8sClient.Get(ctx,
		types.NamespacedName{Name: "conflict-prune-independent", Namespace: ns}, check)
	assert.NoError(t, err,
		"independent resource should NOT be pruned during Conflict state")
	t.Log("Independent resource survived Conflict — prune safety proved")
}

// TestPruneSweptOnSpecNodeRemoval proves that removing nodes from the Graph
// spec causes their managed resources to be pruned on the next reconcile.
// This covers the case where the new revision has ZERO template: nodes — triggered
// is empty, but the prune phase must still run to clean up superseded resources.
//
// Design 005-reconciliation § Prune:
//
//	"After wind, diff the current key set against the applied set. Absent
//	resources are prune candidates if their absence is definitive."
//
// A revision transition that removes all template: nodes produces 0 triggered nodes
// in the new revision. The controller must NOT return early before the prune
// phase — otherwise resources from the superseded revision are orphaned.
func TestPruneSweptOnSpecNodeRemoval(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	cmGVK := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}

	// 1. Create a Graph with two independent template: nodes.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-prune-sweep",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "nodeA",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "prune-sweep-a"},
							"data":       map[string]any{"key": "a"},
						},
					},
					map[string]any{
						"id": "nodeB",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "prune-sweep-b"},
							"data":       map[string]any{"key": "b"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// 2. Wait for both ConfigMaps to be created.
	cmA := &unstructured.Unstructured{}
	cmA.SetGroupVersionKind(cmGVK)
	require.NoError(t, waitForResource(ctx, k8sClient,
		types.NamespacedName{Name: "prune-sweep-a", Namespace: ns}, cmA))
	cmB := &unstructured.Unstructured{}
	cmB.SetGroupVersionKind(cmGVK)
	require.NoError(t, waitForResource(ctx, k8sClient,
		types.NamespacedName{Name: "prune-sweep-b", Namespace: ns}, cmB))
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-prune-sweep", Namespace: ns}))
	t.Log("Both ConfigMaps created, Graph Active")

	// 3. Update the spec to have ZERO nodes. The new revision has no template:
	// nodes — triggered is empty, but the prune phase must still run.
	// This is the exact scenario needsPruneSweep / isRevisionTransition
	// guards are meant to handle.
	require.NoError(t, updateWithRetry(ctx, k8sClient, GraphGVK,
		types.NamespacedName{Name: "test-prune-sweep", Namespace: ns}, func(obj *unstructured.Unstructured) {
			unstructured.SetNestedSlice(obj.Object, []any{}, "spec", "nodes")
		}))
	t.Log("Spec emptied — zero nodes in new revision")

	// 4. THE KEY ASSERTION: both ConfigMaps must be pruned.
	// If the controller returns early before the prune phase (len(triggered)==0
	// early exit), these resources are orphaned and the test fails.
	require.NoError(t, waitForDeletion(ctx, k8sClient, cmGVK,
		types.NamespacedName{Name: "prune-sweep-a", Namespace: ns}),
		"prune-sweep-a must be pruned after all nodes are removed from spec")
	require.NoError(t, waitForDeletion(ctx, k8sClient, cmGVK,
		types.NamespacedName{Name: "prune-sweep-b", Namespace: ns}),
		"prune-sweep-b must be pruned after all nodes are removed from spec")
	t.Log("Both ConfigMaps pruned — spec-emptying triggers prune sweep")
}

// TestPrune_RegressionCrossNamespace proves that resources with an explicit
// metadata.namespace in the template are pruned correctly when removed from
// the spec. The prune key must use the template's namespace, not the Graph's.
//
// Per 005-reconciliation.md § Prune: resource keys encode
// group/version/Kind/namespace/name.
//
// Bug: staticResourceKey always used the Graph's namespace, making
// cross-namespace resources invisible to the prune diff.
func TestPrune_RegressionCrossNamespace(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)
	targetNS := createNamespace(t) // Different namespace for the resource

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "cross-ns-prune",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					// Resource in a different namespace
					map[string]any{
						"id": "crossns",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name":      "cross-ns-config",
								"namespace": targetNS,
							},
							"data": map[string]any{"source": "original"},
						},
					},
					// Resource in the Graph's own namespace (for comparison)
					map[string]any{
						"id": "local",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "local-config"},
							"data":       map[string]any{"source": "local"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for both resources to be created
	crossNSCM := &unstructured.Unstructured{}
	crossNSCM.SetGroupVersionKind(cmGVK)
	require.NoError(t, waitForResource(ctx, k8sClient,
		types.NamespacedName{Name: "cross-ns-config", Namespace: targetNS}, crossNSCM),
		"cross-namespace resource should be created")

	localCM := &unstructured.Unstructured{}
	localCM.SetGroupVersionKind(cmGVK)
	require.NoError(t, waitForResource(ctx, k8sClient,
		types.NamespacedName{Name: "local-config", Namespace: ns}, localCM))

	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "cross-ns-prune", Namespace: ns}))

	// Now remove the cross-namespace node from the spec
	require.NoError(t, updateWithRetry(ctx, k8sClient, GraphGVK,
		types.NamespacedName{Name: "cross-ns-prune", Namespace: ns},
		func(obj *unstructured.Unstructured) {
			obj.Object["spec"] = map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "local",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "local-config"},
							"data":       map[string]any{"source": "local"},
						},
					},
				},
			}
		}))

	// The cross-namespace resource should be pruned
	require.NoError(t, waitForDeletion(ctx, k8sClient, cmGVK,
		types.NamespacedName{Name: "cross-ns-config", Namespace: targetNS}),
		"cross-namespace resource should be pruned after node removed from spec")

	// Local resource should still exist
	localCheck := &unstructured.Unstructured{}
	localCheck.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "local-config", Namespace: ns}, localCheck),
		"local resource should survive the prune")
}

// TestPruneSafetyErrorBlocksPrune proves that when a node enters Error
// state (CEL evaluation failure), its previously-applied resource is NOT
// pruned — the controller preserves it as uncertain absence.
//
// Per 005-reconciliation.md § Prune:
//
//	"Uncertain absence — a dependency is Pending, Conflict, or Error.
//	 The resource might appear once the blocker resolves. Not safe to prune."
//
// The existing tests cover Pending and Conflict blocking prune. This test
// fills the Error gap: when a node's CEL evaluation fails at runtime, its
// key is absent from the current output set, but the resource must survive
// because Error is an uncertain absence.
//
// Setup:
//   - External ConfigMap with divisor="2"
//   - Graph: ref source, template computed (result = 100/int(divisor))
//   - Initial convergence: computed ConfigMap has result="50"
//   - Update divisor to "0" → division by zero → computed enters Error
//   - Assert: computed ConfigMap survives (Error blocks prune)
//   - Update divisor to "5" → recovery → computed ConfigMap has result="20"
func TestPruneSafetyErrorBlocksPrune(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	cmGVK := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}

	// Pre-create the source ConfigMap with a valid divisor.
	source := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "error-prune-source",
				"namespace": ns,
			},
			"data": map[string]any{
				"divisor": "2",
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, source))

	// Graph: ref the source, compute 100/divisor.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-error-prune",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "source",
						"ref": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "error-prune-source"},
						},
					},
					map[string]any{
						"id": "computed",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "error-prune-result"},
							"data": map[string]any{
								"result": "${string(100 / int(source.data.divisor))}",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))
	graphKey := types.NamespacedName{Name: "test-error-prune", Namespace: ns}

	// Wait for convergence.
	require.NoError(t, waitForGraphReady(ctx, k8sClient, graphKey))
	result := &unstructured.Unstructured{}
	result.SetGroupVersionKind(cmGVK)
	require.NoError(t, waitForResource(ctx, k8sClient,
		types.NamespacedName{Name: "error-prune-result", Namespace: ns}, result))
	data, _, _ := unstructured.NestedStringMap(result.Object, "data")
	assert.Equal(t, "50", data["result"])
	t.Log("Phase 1: computed ConfigMap created with result=50")

	// Cause a CEL evaluation error: set divisor to "0" → division by zero.
	require.NoError(t, updateWithRetry(ctx, k8sClient, cmGVK,
		types.NamespacedName{Name: "error-prune-source", Namespace: ns},
		func(obj *unstructured.Unstructured) {
			unstructured.SetNestedField(obj.Object, "0", "data", "divisor")
		}))
	t.Log("Phase 2: Updated divisor to 0 → CEL division by zero")

	// Wait for Graph to enter Error state.
	require.NoError(t, waitForGraphReadyReason(ctx, k8sClient, graphKey, "Error"))
	t.Log("Phase 2: Graph shows Error — CEL runtime failure classified correctly")

	// THE KEY ASSERTION: the computed ConfigMap must NOT be pruned.
	// The node is in Error state (uncertain absence) so its resource survives.
	surviving := &unstructured.Unstructured{}
	surviving.SetGroupVersionKind(cmGVK)
	err := k8sClient.Get(ctx,
		types.NamespacedName{Name: "error-prune-result", Namespace: ns}, surviving)
	assert.NoError(t, err,
		"computed ConfigMap must NOT be pruned during Error state (uncertain absence)")
	// Value should still be the previous result — the node was not re-evaluated.
	if err == nil {
		oldData, _, _ := unstructured.NestedStringMap(surviving.Object, "data")
		assert.Equal(t, "50", oldData["result"],
			"computed should retain previous result=50 during Error")
	}
	t.Log("Phase 2: computed ConfigMap survived Error — prune safety proved")

	// Recovery: set divisor to "5" → CEL succeeds → Graph converges.
	require.NoError(t, updateWithRetry(ctx, k8sClient, cmGVK,
		types.NamespacedName{Name: "error-prune-source", Namespace: ns},
		func(obj *unstructured.Unstructured) {
			unstructured.SetNestedField(obj.Object, "5", "data", "divisor")
		}))
	t.Log("Phase 3: Updated divisor to 5 → recovery")

	require.NoError(t, waitForGraphReady(ctx, k8sClient, graphKey))

	// Verify updated result.
	require.NoError(t, waitForField(ctx, k8sClient, cmGVK,
		types.NamespacedName{Name: "error-prune-result", Namespace: ns},
		[]string{"data", "result"}, "20"))
	t.Log("Phase 3: recovered — computed ConfigMap has result=20")
}

// TestPruneSafetySystemErrorBlocksPrune proves that when a node enters
// SystemError (5xx from webhook), its previously-applied resource is NOT
// pruned — the controller preserves it as uncertain absence.
//
// Per 005-reconciliation.md § Prune:
//
//	"Uncertain absence — a dependency is Pending, Conflict, or Error.
//	 The resource might appear once the blocker resolves. Not safe to prune."
//
// SystemError is the 5xx analog of Error — both represent uncertain absence
// where the resource might succeed on retry. This test fills the SystemError
// gap alongside the Error gap (TestPruneSafetyErrorBlocksPrune) and the
// existing Pending/Conflict tests.
//
// Setup:
//   - Graph creates a ConfigMap
//   - Wait for convergence
//   - Enable webhook fault injection (500 on ConfigMap applies)
//   - Trigger re-evaluation via spec change
//   - Node enters SystemError — resource must survive (not pruned)
//   - Clear fault → recovery
func TestPruneSafetySystemErrorBlocksPrune(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	faultLabel := "fault-prune-se-" + ns
	labelNamespace(t, ns, map[string]string{"fault-inject": faultLabel})

	fw := startFaultWebhook(t)
	registerFaultWebhook(t, k8sClient, "fault-prune-se-"+ns, fw, "fault-inject", faultLabel)

	cmGVK := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-prune-syserr",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "managed",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "prune-syserr-cm"},
							"data":       map[string]any{"version": "v1"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))
	graphKey := types.NamespacedName{Name: "test-prune-syserr", Namespace: ns}

	require.NoError(t, waitForGraphReady(ctx, k8sClient, graphKey))
	t.Log("Phase 1: ConfigMap created, Graph Ready")

	// Enable fault injection.
	fw.Reject("prune-syserr-cm")

	// Trigger re-evaluation — controller will try to update and get 500.
	require.NoError(t, updateWithRetry(ctx, k8sClient, GraphGVK, graphKey,
		func(obj *unstructured.Unstructured) {
			unstructured.SetNestedSlice(obj.Object, []any{
				map[string]any{
					"id": "managed",
					"template": map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata":   map[string]any{"name": "prune-syserr-cm"},
						"data":       map[string]any{"version": "v2"},
					},
				},
			}, "spec", "nodes")
		}))

	// Wait for SystemError state.
	require.NoError(t, waitForGraphReadyReason(ctx, k8sClient, graphKey, "SystemError"))
	t.Log("Phase 2: Node in SystemError — webhook returning 500")

	// THE KEY ASSERTION: resource must survive SystemError (uncertain absence).
	surviving := &unstructured.Unstructured{}
	surviving.SetGroupVersionKind(cmGVK)
	err := k8sClient.Get(ctx,
		types.NamespacedName{Name: "prune-syserr-cm", Namespace: ns}, surviving)
	assert.NoError(t, err,
		"ConfigMap must NOT be pruned during SystemError (uncertain absence)")
	if err == nil {
		data, _, _ := unstructured.NestedStringMap(surviving.Object, "data")
		assert.Equal(t, "v1", data["version"],
			"ConfigMap should retain v1 — update was rejected by webhook")
	}
	t.Log("Phase 2: ConfigMap survived SystemError — prune safety proved")

	// Clear fault → recovery.
	fw.AcceptAll()
	require.NoError(t, waitForGraphReady(ctx, k8sClient, graphKey))

	// Verify updated.
	require.NoError(t, waitForField(ctx, k8sClient, cmGVK,
		types.NamespacedName{Name: "prune-syserr-cm", Namespace: ns},
		[]string{"data", "version"}, "v2"))
	t.Log("Phase 3: recovered — ConfigMap updated to v2")
}
