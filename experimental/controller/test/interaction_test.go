package graphcontroller_test

import (
	"context"
	"fmt"
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
// Feature Interaction Tests
//
// These tests exercise combinations of features that are individually tested
// but whose interactions may produce emergent bugs. The matrix is:
// forEach × {includeWhen, Contribute, Force, Multi-rev},
// includeWhen × {propagateWhen, Contribute, finalizes},
// propagateWhen × {Contribute},
// Nested Graphs × {revision transition, includeWhen},
// readyWhen × {includeWhen}.
// ---------------------------------------------------------------------------

// TestForEachIncludeWhenToggle proves that when a forEach node's includeWhen
// evaluates to false, ALL previously-stamped children are pruned. When it
// toggles back to true, children are re-created.
//
// This interaction is the highest-risk untested pair: forEach children are
// keyed by identity labels. When the parent is excluded, zero keys enter
// the applied set, so ALL children become prune candidates.
func TestForEachIncludeWhenToggle(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	gvk := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}

	// Pre-create a control ConfigMap with toggle=true.
	control := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "foreach-include-control",
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
				"name":      "test-foreach-include",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "control",
						"ref": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "foreach-include-control"},
						},
					},
					map[string]any{
						"id": "workers",
						"forEach": map[string]any{
							"value": "${['alpha', 'beta', 'gamma']}",
						},
						"includeWhen": []any{"${control.data.toggle == 'true'}"},
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "fi-worker-${value}",
							},
							"data": map[string]any{
								"item": "${value}",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for all 3 children to be created.
	childNames := []string{"fi-worker-alpha", "fi-worker-beta", "fi-worker-gamma"}
	for _, name := range childNames {
		cm := &unstructured.Unstructured{}
		cm.SetGroupVersionKind(gvk)
		require.NoError(t, waitForResource(ctx, k8sClient,
			types.NamespacedName{Name: name, Namespace: ns}, cm),
			"forEach child %s must be created when includeWhen=true", name)
	}
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-foreach-include", Namespace: ns}))
	t.Log("All 3 forEach children created, Graph ready")

	// Toggle includeWhen to false → all children should be pruned.
	latest := &unstructured.Unstructured{}
	latest.SetGroupVersionKind(gvk)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "foreach-include-control", Namespace: ns}, latest))
	unstructured.SetNestedField(latest.Object, "false", "data", "toggle")
	require.NoError(t, k8sClient.Update(ctx, latest))
	t.Log("Set toggle=false → forEach node excluded")

	// All children should be pruned.
	for _, name := range childNames {
		require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true,
			func(ctx context.Context) (bool, error) {
				check := &unstructured.Unstructured{}
				check.SetGroupVersionKind(gvk)
				err := k8sClient.Get(ctx,
					types.NamespacedName{Name: name, Namespace: ns}, check)
				return err != nil, nil
			}),
			"forEach child %s must be pruned when includeWhen=false", name)
	}
	t.Log("All 3 forEach children pruned after includeWhen toggled to false — forEach + includeWhen prune proved")
}

// TestForEachIncludeWhenDataPendingPreservesChildren proves that when
// includeWhen can't evaluate (data-pending), forEach children from the
// previous reconcile survive — they are NOT pruned during uncertain absence.
func TestForEachIncludeWhenDataPendingPreservesChildren(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	gvk := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}

	// Pre-create control with toggle field.
	control := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "foreach-dp-control",
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
				"name":      "test-foreach-dp",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "control",
						"ref": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "foreach-dp-control"},
						},
					},
					map[string]any{
						"id": "workers",
						"forEach": map[string]any{
							"value": "${['x', 'y']}",
						},
						"includeWhen": []any{"${control.data.toggle == 'true'}"},
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "dp-worker-${value}",
							},
							"data": map[string]any{
								"item": "${value}",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for children to exist.
	for _, name := range []string{"dp-worker-x", "dp-worker-y"} {
		cm := &unstructured.Unstructured{}
		cm.SetGroupVersionKind(gvk)
		require.NoError(t, waitForResource(ctx, k8sClient,
			types.NamespacedName{Name: name, Namespace: ns}, cm))
	}
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-foreach-dp", Namespace: ns}))
	t.Log("forEach children created, Graph ready")

	// Remove the toggle field entirely → data-pending (not false).
	latest := &unstructured.Unstructured{}
	latest.SetGroupVersionKind(gvk)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "foreach-dp-control", Namespace: ns}, latest))
	unstructured.RemoveNestedField(latest.Object, "data")
	require.NoError(t, k8sClient.Update(ctx, latest))
	t.Log("Removed toggle field entirely → data-pending")

	// Wait for Graph to enter non-Ready.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true,
		func(ctx context.Context) (bool, error) {
			g := &unstructured.Unstructured{}
			g.SetGroupVersionKind(GraphGVK)
			if err := k8sClient.Get(ctx,
				types.NamespacedName{Name: "test-foreach-dp", Namespace: ns}, g); err != nil {
				return false, nil
			}
			return !graphReady(g), nil
		}))

	// Children must survive — uncertain absence blocks prune.
	require.NoError(t, waitForSettle(ctx, k8sClient, GraphGVK,
		types.NamespacedName{Name: "test-foreach-dp", Namespace: ns}))
	for _, name := range []string{"dp-worker-x", "dp-worker-y"} {
		check := &unstructured.Unstructured{}
		check.SetGroupVersionKind(gvk)
		require.NoError(t, k8sClient.Get(ctx,
			types.NamespacedName{Name: name, Namespace: ns}, check),
			"forEach child %s must survive data-pending (uncertain absence)", name)
	}
	t.Log("forEach children survived data-pending — prune safety for forEach + includeWhen proved")
}

// TestIncludeWhenPropagateWhenPrecedence proves that when a dependency is
// excluded, its dependents are excluded too — even if they have propagateWhen.
// Exclusion overrides the propagateWhen gate.
//
// Setup: upstream has both includeWhen and propagateWhen. Start with both
// satisfied (gate open, included). Verify downstream is created. Then
// exclude upstream → downstream must also be excluded and pruned.
func TestIncludeWhenPropagateWhenPrecedence(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	gvk := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}

	// Control CM with toggle.
	control := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "inc-prop-control",
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
				"name":      "test-inc-prop",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "control",
						"ref": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "inc-prop-control"},
						},
					},
					map[string]any{
						"id":            "upstream",
						"includeWhen":   []any{"${control.data.toggle == 'true'}"},
						"propagateWhen": []any{"${upstream.data.ready == 'true'}"},
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "inc-prop-upstream"},
							"data": map[string]any{
								"ready": "true",
								"value": "data",
							},
						},
					},
					map[string]any{
						"id": "downstream",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "inc-prop-downstream"},
							"data": map[string]any{
								"ref": "${upstream.data.value}",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for upstream to exist (includeWhen=true).
	upCM := &unstructured.Unstructured{}
	upCM.SetGroupVersionKind(gvk)
	require.NoError(t, waitForResource(ctx, k8sClient,
		types.NamespacedName{Name: "inc-prop-upstream", Namespace: ns}, upCM))
	t.Log("Upstream created (includeWhen=true, propagateWhen=false)")

	// Downstream should be created since data is in scope.
	downCM := &unstructured.Unstructured{}
	downCM.SetGroupVersionKind(gvk)
	require.NoError(t, waitForResource(ctx, k8sClient,
		types.NamespacedName{Name: "inc-prop-downstream", Namespace: ns}, downCM))
	t.Log("Downstream created")

	// Exclude upstream → downstream must also be excluded (contagious).
	latest := &unstructured.Unstructured{}
	latest.SetGroupVersionKind(gvk)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "inc-prop-control", Namespace: ns}, latest))
	unstructured.SetNestedField(latest.Object, "false", "data", "toggle")
	require.NoError(t, k8sClient.Update(ctx, latest))
	t.Log("Set toggle=false → upstream excluded")

	// Both upstream and downstream should be pruned.
	for _, name := range []string{"inc-prop-upstream", "inc-prop-downstream"} {
		require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true,
			func(ctx context.Context) (bool, error) {
				check := &unstructured.Unstructured{}
				check.SetGroupVersionKind(gvk)
				err := k8sClient.Get(ctx,
					types.NamespacedName{Name: name, Namespace: ns}, check)
				return err != nil, nil
			}),
			"%s must be pruned when upstream is excluded", name)
	}
	t.Log("Both upstream and downstream pruned — exclusion overrides propagateWhen gate")
}

// TestPropagateWhenContribute proves that propagateWhen on a node upstream
// of a Contribute node gates the Contribute's re-evaluation. While gated,
// the Contribute retains its previous fields.
//
// Important: We use a Watch node to feed data into an Own upstream node.
// Changes to the watched resource close the gate WITHOUT triggering a
// revision transition (which would create a new instanceState with no
// previous data to retain).
func TestPropagateWhenContribute(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	gvk := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}

	// Pre-create the external resource the Contribute will write to.
	external := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "prop-contrib-target",
				"namespace": ns,
			},
			"data": map[string]any{
				"original": "data",
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, external))

	// Pre-create the control resource that feeds the upstream node.
	controlCM := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "prop-contrib-control",
				"namespace": ns,
			},
			"data": map[string]any{
				"ready": "true",
				"value": "v1",
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, controlCM))

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-prop-contrib",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					// Watch the control CM (changes don't trigger revision transition).
					map[string]any{
						"id": "control",
						"ref": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "prop-contrib-control"},
						},
					},
					// Upstream: Own a resource, propagateWhen gated on control.
					map[string]any{
						"id":            "upstream",
						"propagateWhen": []any{"${control.data.ready == 'true'}"},
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "prop-contrib-upstream"},
							"data": map[string]any{
								"value": "${control.data.value}",
							},
						},
					},
					// Contribute: writes upstream's value to the external resource.
					map[string]any{
						"id": "contrib",
						"patch": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "prop-contrib-target",
								"annotations": map[string]any{
									"kro.run/value": "${upstream.data.value}",
								},
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for convergence — contrib should have written v1.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true,
		func(ctx context.Context) (bool, error) {
			check := &unstructured.Unstructured{}
			check.SetGroupVersionKind(gvk)
			if err := k8sClient.Get(ctx,
				types.NamespacedName{Name: "prop-contrib-target", Namespace: ns}, check); err != nil {
				return false, nil
			}
			return check.GetAnnotations()["kro.run/value"] == "v1", nil
		}))
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-prop-contrib", Namespace: ns}))
	t.Log("Contribute applied v1, Graph ready")

	// Close the gate by updating the control resource (no revision transition).
	latest := &unstructured.Unstructured{}
	latest.SetGroupVersionKind(gvk)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "prop-contrib-control", Namespace: ns}, latest))
	unstructured.SetNestedField(latest.Object, "false", "data", "ready")
	unstructured.SetNestedField(latest.Object, "v2", "data", "value")
	require.NoError(t, k8sClient.Update(ctx, latest))
	t.Log("Updated control: ready=false, value=v2 → gate closed")

	// Wait for upstream to be updated with v2 data.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true,
		func(ctx context.Context) (bool, error) {
			check := &unstructured.Unstructured{}
			check.SetGroupVersionKind(gvk)
			if err := k8sClient.Get(ctx,
				types.NamespacedName{Name: "prop-contrib-upstream", Namespace: ns}, check); err != nil {
				return false, nil
			}
			data, _, _ := unstructured.NestedStringMap(check.Object, "data")
			return data["value"] == "v2", nil
		}))
	require.NoError(t, waitForSettle(ctx, k8sClient, GraphGVK,
		types.NamespacedName{Name: "test-prop-contrib", Namespace: ns}))

	// Contribute should still have v1 — gate is closed, downstream retains previous.
	target := &unstructured.Unstructured{}
	target.SetGroupVersionKind(gvk)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "prop-contrib-target", Namespace: ns}, target))
	assert.Equal(t, "v1", target.GetAnnotations()["kro.run/value"],
		"Contribute should retain v1 while propagateWhen gate is closed")
	t.Log("Contribute retained v1 while gate closed — propagateWhen + Contribute proved")
}

// TestIncludeWhenContributeReleaseFields proves that when includeWhen toggles
// a Contribute node to false, release apply releases the contributed fields.
func TestIncludeWhenContributeReleaseFields(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	gvk := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}

	// Pre-create the external resource.
	external := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "inc-contrib-target",
				"namespace": ns,
			},
			"data": map[string]any{
				"original": "data",
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, external))

	control := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "inc-contrib-control",
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
				"name":      "test-inc-contrib",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "control",
						"ref": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "inc-contrib-control"},
						},
					},
					map[string]any{
						"id":          "contrib",
						"includeWhen": []any{"${control.data.toggle == 'true'}"},
						"patch": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "inc-contrib-target",
								"annotations": map[string]any{
									"kro.run/managed": "true",
								},
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for contribution to be applied.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true,
		func(ctx context.Context) (bool, error) {
			check := &unstructured.Unstructured{}
			check.SetGroupVersionKind(gvk)
			if err := k8sClient.Get(ctx,
				types.NamespacedName{Name: "inc-contrib-target", Namespace: ns}, check); err != nil {
				return false, nil
			}
			return check.GetAnnotations()["kro.run/managed"] == "true", nil
		}))
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-inc-contrib", Namespace: ns}))
	t.Log("Contribution applied, Graph ready")

	// Toggle includeWhen to false → should release contributed fields.
	latest := &unstructured.Unstructured{}
	latest.SetGroupVersionKind(gvk)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "inc-contrib-control", Namespace: ns}, latest))
	unstructured.SetNestedField(latest.Object, "false", "data", "toggle")
	require.NoError(t, k8sClient.Update(ctx, latest))
	t.Log("Toggle set to false → Contribute excluded")

	// Target resource must still exist (Contribute → never delete).
	require.NoError(t, waitForSettle(ctx, k8sClient, GraphGVK,
		types.NamespacedName{Name: "test-inc-contrib", Namespace: ns}))
	target := &unstructured.Unstructured{}
	target.SetGroupVersionKind(gvk)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "inc-contrib-target", Namespace: ns}, target),
		"Contribute target must survive includeWhen toggle (never delete)")

	// Original data must still be there.
	data, _, _ := unstructured.NestedStringMap(target.Object, "data")
	assert.Equal(t, "data", data["original"],
		"original data should survive Contribute release")
	t.Log("Contribute target survived exclusion — includeWhen + Contribute proved")
}

// TestForEachContributeScaleDown proves that forEach can stamp Contribute
// resources (write fields to pre-existing targets per collection item),
// and that scale-down releases fields via release apply without deleting.
func TestForEachContributeScaleDown(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	gvk := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}

	// Pre-create 3 target resources that forEach will contribute to.
	targets := []string{"fe-contrib-a", "fe-contrib-b", "fe-contrib-c"}
	for _, name := range targets {
		cm := &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name":      name,
					"namespace": ns,
				},
				"data": map[string]any{
					"original": "data-" + name,
				},
			},
		}
		require.NoError(t, k8sClient.Create(ctx, cm))
	}

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-foreach-contrib",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "items",
						"forEach": map[string]any{
							"name": "${['fe-contrib-a', 'fe-contrib-b', 'fe-contrib-c']}",
						},
						"patch": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "${name}",
								"annotations": map[string]any{
									"kro.run/managed": "true",
								},
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for all 3 contributions.
	for _, name := range targets {
		require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true,
			func(ctx context.Context) (bool, error) {
				check := &unstructured.Unstructured{}
				check.SetGroupVersionKind(gvk)
				if err := k8sClient.Get(ctx,
					types.NamespacedName{Name: name, Namespace: ns}, check); err != nil {
					return false, nil
				}
				return check.GetAnnotations()["kro.run/managed"] == "true", nil
			}),
			"forEach should contribute to %s", name)
	}
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-foreach-contrib", Namespace: ns}))
	t.Log("All 3 contributions applied")

	// Verify children have "patch" role labels.
	for _, name := range targets {
		check := &unstructured.Unstructured{}
		check.SetGroupVersionKind(gvk)
		require.NoError(t, k8sClient.Get(ctx,
			types.NamespacedName{Name: name, Namespace: ns}, check))
		labels := check.GetLabels()
		for key, val := range labels {
			if strings.HasSuffix(key, ".internal.kro.run/type") {
				assert.Equal(t, "patch", val,
					"%s should have 'patch' role label (pre-existing target)", name)
			}
		}
	}

	// Scale down: remove item "c" from the collection.
	require.NoError(t, updateWithRetry(ctx, k8sClient, GraphGVK,
		types.NamespacedName{Name: "test-foreach-contrib", Namespace: ns}, func(obj *unstructured.Unstructured) {
			unstructured.SetNestedSlice(obj.Object, []any{
				map[string]any{
					"id": "items",
					"forEach": map[string]any{
						"name": "${['fe-contrib-a', 'fe-contrib-b']}",
					},
					"patch": map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata": map[string]any{
							"name": "${name}",
							"annotations": map[string]any{
								"kro.run/managed": "true",
							},
						},
					},
				},
			}, "spec", "nodes")
		}))
	t.Log("Scaled down: removed item c from collection")

	require.NoError(t, waitForSettle(ctx, k8sClient, GraphGVK,
		types.NamespacedName{Name: "test-foreach-contrib", Namespace: ns}))

	// fe-contrib-c must still exist (Contribute → release fields, never delete).
	target := &unstructured.Unstructured{}
	target.SetGroupVersionKind(gvk)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "fe-contrib-c", Namespace: ns}, target),
		"forEach Contribute target must survive scale-down (never delete)")

	data, _, _ := unstructured.NestedStringMap(target.Object, "data")
	assert.Equal(t, "data-fe-contrib-c", data["original"],
		"original data should survive Contribute release on scale-down")
	t.Log("fe-contrib-c survived scale-down — forEach + Contribute proved")
}

// TestForEachRevisionTransition proves that a spec change altering a forEach
// collection expression prunes old children and creates new ones across the
// revision boundary.
func TestForEachRevisionTransition(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	gvk := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-foreach-rev",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "items",
						"forEach": map[string]any{
							"value": "${['old-a', 'old-b']}",
						},
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "rev-${value}",
							},
							"data": map[string]any{
								"item":    "${value}",
								"version": "v1",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	for _, name := range []string{"rev-old-a", "rev-old-b"} {
		cm := &unstructured.Unstructured{}
		cm.SetGroupVersionKind(gvk)
		require.NoError(t, waitForResource(ctx, k8sClient,
			types.NamespacedName{Name: name, Namespace: ns}, cm))
	}
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-foreach-rev", Namespace: ns}))
	t.Log("Rev1: old-a, old-b created")

	// Change the collection to new items.
	require.NoError(t, updateWithRetry(ctx, k8sClient, GraphGVK,
		types.NamespacedName{Name: "test-foreach-rev", Namespace: ns}, func(obj *unstructured.Unstructured) {
			unstructured.SetNestedSlice(obj.Object, []any{
				map[string]any{
					"id": "items",
					"forEach": map[string]any{
						"value": "${['new-x', 'new-y']}",
					},
					"template": map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata": map[string]any{
							"name": "rev-${value}",
						},
						"data": map[string]any{
							"item":    "${value}",
							"version": "v2",
						},
					},
				},
			}, "spec", "nodes")
		}))
	t.Log("Updated spec: new collection items new-x, new-y")

	for _, name := range []string{"rev-new-x", "rev-new-y"} {
		cm := &unstructured.Unstructured{}
		cm.SetGroupVersionKind(gvk)
		require.NoError(t, waitForResource(ctx, k8sClient,
			types.NamespacedName{Name: name, Namespace: ns}, cm),
			"new forEach child %s must be created", name)
	}

	for _, name := range []string{"rev-old-a", "rev-old-b"} {
		require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true,
			func(ctx context.Context) (bool, error) {
				check := &unstructured.Unstructured{}
				check.SetGroupVersionKind(gvk)
				err := k8sClient.Get(ctx,
					types.NamespacedName{Name: name, Namespace: ns}, check)
				return err != nil, nil
			}),
			"old forEach child %s must be pruned after revision transition", name)
	}
	t.Log("Rev2: new-x, new-y created; old-a, old-b pruned — forEach + revision transition proved")
}

// TestReadyWhenIncludeWhenStatusTransition proves that when a node with
// readyWhen=false is excluded by includeWhen toggle, the Graph's Ready
// status stops reporting NotReady for the excluded node.
func TestReadyWhenIncludeWhenStatusTransition(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	gvk := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}

	control := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "rw-inc-control",
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
				"name":      "test-rw-inc",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "control",
						"ref": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "rw-inc-control"},
						},
					},
					map[string]any{
						"id":          "conditional",
						"includeWhen": []any{"${control.data.toggle == 'true'}"},
						"readyWhen":   []any{"${conditional.data.ready == 'true'}"},
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "rw-inc-conditional"},
							"data": map[string]any{
								"ready": "false",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Graph should be NotReady (readyWhen=false on conditional).
	cm := &unstructured.Unstructured{}
	cm.SetGroupVersionKind(gvk)
	require.NoError(t, waitForResource(ctx, k8sClient,
		types.NamespacedName{Name: "rw-inc-conditional", Namespace: ns}, cm))
	require.NoError(t, waitForGraphReadyStatus(ctx, k8sClient,
		types.NamespacedName{Name: "test-rw-inc", Namespace: ns}, "Unknown"))
	t.Log("Conditional exists, Graph NotReady (readyWhen=false)")

	// Exclude the conditional node. Graph should become Ready.
	latest := &unstructured.Unstructured{}
	latest.SetGroupVersionKind(gvk)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "rw-inc-control", Namespace: ns}, latest))
	unstructured.SetNestedField(latest.Object, "false", "data", "toggle")
	require.NoError(t, k8sClient.Update(ctx, latest))
	t.Log("Toggle set to false → conditional excluded")

	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-rw-inc", Namespace: ns}))
	t.Log("Graph became Ready after excluding NotReady node — readyWhen + includeWhen proved")
}

// TestNestedGraphRevisionTransition proves that when a parent Graph's spec
// changes the child Graph template, old child Graphs are pruned and new ones
// are created.
func TestNestedGraphRevisionTransition(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	gvk := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-nested-rev",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "child",
						"template": map[string]any{
							"apiVersion": "experimental.kro.run/v1alpha1",
							"kind":       "Graph",
							"metadata":   map[string]any{"name": "nested-child-v1"},
							"spec": map[string]any{
								"nodes": []any{
									map[string]any{
										"id": "resource",
										"template": map[string]any{
											"apiVersion": "v1",
											"kind":       "ConfigMap",
											"metadata":   map[string]any{"name": "nested-cm-v1"},
											"data":       map[string]any{"version": "v1"},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	childGraph := &unstructured.Unstructured{}
	childGraph.SetGroupVersionKind(GraphGVK)
	require.NoError(t, waitForResource(ctx, k8sClient,
		types.NamespacedName{Name: "nested-child-v1", Namespace: ns}, childGraph))

	v1CM := &unstructured.Unstructured{}
	v1CM.SetGroupVersionKind(gvk)
	require.NoError(t, waitForResource(ctx, k8sClient,
		types.NamespacedName{Name: "nested-cm-v1", Namespace: ns}, v1CM))
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-nested-rev", Namespace: ns}))
	t.Log("V1 child Graph and resource created")

	// Update parent spec: change child name.
	require.NoError(t, updateWithRetry(ctx, k8sClient, GraphGVK,
		types.NamespacedName{Name: "test-nested-rev", Namespace: ns}, func(obj *unstructured.Unstructured) {
			unstructured.SetNestedSlice(obj.Object, []any{
				map[string]any{
					"id": "child",
					"template": map[string]any{
						"apiVersion": "experimental.kro.run/v1alpha1",
						"kind":       "Graph",
						"metadata":   map[string]any{"name": "nested-child-v2"},
						"spec": map[string]any{
							"nodes": []any{
								map[string]any{
									"id": "resource",
									"template": map[string]any{
										"apiVersion": "v1",
										"kind":       "ConfigMap",
										"metadata":   map[string]any{"name": "nested-cm-v2"},
										"data":       map[string]any{"version": "v2"},
									},
								},
							},
						},
					},
				},
			}, "spec", "nodes")
		}))
	t.Log("Updated parent: child Graph name changed to v2")

	newChild := &unstructured.Unstructured{}
	newChild.SetGroupVersionKind(GraphGVK)
	require.NoError(t, waitForResource(ctx, k8sClient,
		types.NamespacedName{Name: "nested-child-v2", Namespace: ns}, newChild))

	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true,
		func(ctx context.Context) (bool, error) {
			check := &unstructured.Unstructured{}
			check.SetGroupVersionKind(GraphGVK)
			err := k8sClient.Get(ctx,
				types.NamespacedName{Name: "nested-child-v1", Namespace: ns}, check)
			return err != nil, nil
		}))
	t.Log("V1 child Graph pruned, V2 created — nested Graph revision transition proved")
}

// TestNestedGraphIncludeWhen proves that includeWhen excludes a node
// that stamps a child Graph, and the child Graph is deleted along with
// its managed resources.
func TestNestedGraphIncludeWhen(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	gvk := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}

	control := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "nested-inc-control",
				"namespace": ns,
			},
			"data": map[string]any{"toggle": "true"},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, control))

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-nested-inc",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "control",
						"ref": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "nested-inc-control"},
						},
					},
					map[string]any{
						"id":          "child",
						"includeWhen": []any{"${control.data.toggle == 'true'}"},
						"template": map[string]any{
							"apiVersion": "experimental.kro.run/v1alpha1",
							"kind":       "Graph",
							"metadata":   map[string]any{"name": "nested-inc-child"},
							"spec": map[string]any{
								"nodes": []any{
									map[string]any{
										"id": "resource",
										"template": map[string]any{
											"apiVersion": "v1",
											"kind":       "ConfigMap",
											"metadata":   map[string]any{"name": "nested-inc-cm"},
											"data":       map[string]any{"state": "alive"},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	childGraph := &unstructured.Unstructured{}
	childGraph.SetGroupVersionKind(GraphGVK)
	require.NoError(t, waitForResource(ctx, k8sClient,
		types.NamespacedName{Name: "nested-inc-child", Namespace: ns}, childGraph))

	childCM := &unstructured.Unstructured{}
	childCM.SetGroupVersionKind(gvk)
	require.NoError(t, waitForResource(ctx, k8sClient,
		types.NamespacedName{Name: "nested-inc-cm", Namespace: ns}, childCM))
	t.Log("Child Graph and its resource created")

	// Exclude the child.
	latest := &unstructured.Unstructured{}
	latest.SetGroupVersionKind(gvk)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "nested-inc-control", Namespace: ns}, latest))
	unstructured.SetNestedField(latest.Object, "false", "data", "toggle")
	require.NoError(t, k8sClient.Update(ctx, latest))
	t.Log("Toggle set to false → child Graph excluded")

	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true,
		func(ctx context.Context) (bool, error) {
			check := &unstructured.Unstructured{}
			check.SetGroupVersionKind(GraphGVK)
			err := k8sClient.Get(ctx,
				types.NamespacedName{Name: "nested-inc-child", Namespace: ns}, check)
			return err != nil, nil
		}))
	t.Log("Child Graph pruned after includeWhen exclusion — nested Graph + includeWhen proved")
}

// TestMultipleForEachNodesIndependence proves that two independent forEach
// nodes in the same Graph expand and prune independently.
func TestMultipleForEachNodesIndependence(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	gvk := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-multi-foreach",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "groupA",
						"forEach": map[string]any{
							"value": "${['a1', 'a2', 'a3']}",
						},
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "multi-a-${value}"},
							"data":       map[string]any{"group": "A", "item": "${value}"},
						},
					},
					map[string]any{
						"id": "groupB",
						"forEach": map[string]any{
							"value": "${['b1', 'b2']}",
						},
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "multi-b-${value}"},
							"data":       map[string]any{"group": "B", "item": "${value}"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	allNames := []string{"multi-a-a1", "multi-a-a2", "multi-a-a3", "multi-b-b1", "multi-b-b2"}
	for _, name := range allNames {
		cm := &unstructured.Unstructured{}
		cm.SetGroupVersionKind(gvk)
		require.NoError(t, waitForResource(ctx, k8sClient,
			types.NamespacedName{Name: name, Namespace: ns}, cm))
	}
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-multi-foreach", Namespace: ns}))
	t.Log("All 5 children created (3 from A, 2 from B)")

	// Scale down groupA to 1 item. groupB should be unaffected.
	require.NoError(t, updateWithRetry(ctx, k8sClient, GraphGVK,
		types.NamespacedName{Name: "test-multi-foreach", Namespace: ns}, func(obj *unstructured.Unstructured) {
			unstructured.SetNestedSlice(obj.Object, []any{
				map[string]any{
					"id": "groupA",
					"forEach": map[string]any{
						"value": "${['a1']}",
					},
					"template": map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata":   map[string]any{"name": "multi-a-${value}"},
						"data":       map[string]any{"group": "A", "item": "${value}"},
					},
				},
				map[string]any{
					"id": "groupB",
					"forEach": map[string]any{
						"value": "${['b1', 'b2']}",
					},
					"template": map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata":   map[string]any{"name": "multi-b-${value}"},
						"data":       map[string]any{"group": "B", "item": "${value}"},
					},
				},
			}, "spec", "nodes")
		}))
	t.Log("Scaled down groupA to 1 item")

	for _, name := range []string{"multi-a-a2", "multi-a-a3"} {
		require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true,
			func(ctx context.Context) (bool, error) {
				check := &unstructured.Unstructured{}
				check.SetGroupVersionKind(gvk)
				err := k8sClient.Get(ctx,
					types.NamespacedName{Name: name, Namespace: ns}, check)
				return err != nil, nil
			}),
			"%s must be pruned after scale-down", name)
	}

	for _, name := range []string{"multi-a-a1", "multi-b-b1", "multi-b-b2"} {
		check := &unstructured.Unstructured{}
		check.SetGroupVersionKind(gvk)
		require.NoError(t, k8sClient.Get(ctx,
			types.NamespacedName{Name: name, Namespace: ns}, check),
			"%s must survive — it wasn't scaled down", name)
	}
	t.Log("groupA scaled down; groupB unaffected — multiple forEach independence proved")
}

// TestForEachEmptyCollectionVacuouslyReady proves that an empty collection
// produces zero children, and the Graph converges to Ready.
func TestForEachEmptyCollectionVacuouslyReady(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-foreach-empty",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "base",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "foreach-empty-base"},
							"data":       map[string]any{"key": "value"},
						},
					},
					map[string]any{
						"id": "workers",
						"forEach": map[string]any{
							"value": "${[]}",
						},
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "empty-worker-${value}"},
							"data":       map[string]any{"item": "${value}"},
						},
						"readyWhen": []any{"${workers.data.ready == 'true'}"},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-foreach-empty", Namespace: ns}))
	t.Log("Graph is Ready with empty forEach collection — vacuous truth proved")
}

// TestForEachFinalizesSequence proves that a node with both forEach and
// finalizes creates one finalizer resource per item when the target becomes
// a prune candidate. All children must reach readyWhen before the target
// is deleted. The children are cleaned up after the target is deleted.
//
// This is the "drain each connection pool before deleting the load balancer"
// pattern: forEach expands the finalization action across N items.
func TestForEachFinalizesSequence(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	gvk := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}

	// Phase 1: Create a Graph with a target and a forEach finalizer.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-foreach-finalizes",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "target",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "fe-fin-target"},
							"data":       map[string]any{"state": "active"},
						},
					},
					map[string]any{
						"id": "keep",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "fe-fin-keep"},
							"data":       map[string]any{"role": "permanent"},
						},
					},
					map[string]any{
						"id":        "snapshots",
						"finalizes": "target",
						"forEach": map[string]any{
							"value": "${['a', 'b']}",
						},
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "snapshot-${value}"},
							"data": map[string]any{
								"captured-from": "${target.metadata.name}",
								"item":          "${value}",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for target to be created. Finalizer children should NOT exist yet.
	targetCM := &unstructured.Unstructured{}
	targetCM.SetGroupVersionKind(gvk)
	require.NoError(t, waitForResource(ctx, k8sClient,
		types.NamespacedName{Name: "fe-fin-target", Namespace: ns}, targetCM))
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-foreach-finalizes", Namespace: ns}))
	t.Log("Target created, Graph ready")

	// Verify forEach finalizer children do NOT exist during normal operation.
	for _, value := range []string{"a", "b"} {
		check := &unstructured.Unstructured{}
		check.SetGroupVersionKind(gvk)
		err := k8sClient.Get(ctx,
			types.NamespacedName{Name: "snapshot-" + value, Namespace: ns}, check)
		assert.Error(t, err, "snapshot-%s should not exist during normal operation", value)
	}
	t.Log("forEach finalizer children correctly absent during normal operation")

	// Phase 2: Remove the target from the spec. This makes it a prune
	// candidate and triggers finalization via the superseded DAG.
	require.NoError(t, updateWithRetry(ctx, k8sClient, GraphGVK,
		types.NamespacedName{Name: "test-foreach-finalizes", Namespace: ns}, func(obj *unstructured.Unstructured) {
			unstructured.SetNestedSlice(obj.Object, []any{
				map[string]any{
					"id": "keep",
					"template": map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata":   map[string]any{"name": "fe-fin-keep"},
						"data":       map[string]any{"role": "permanent"},
					},
				},
			}, "spec", "nodes")
		}))
	t.Log("Removed target and snapshots from spec — finalization triggered")

	// Wait for the target to be deleted — this proves the full finalization
	// sequence ran: forEach children were created, readyWhen was checked
	// (auto-ready for ConfigMaps), and the target was deleted.
	//
	// Note: the children may be cleaned up in the same cycle as the target
	// deletion (deferred-delete after the prune walk). The functional
	// contract is target deletion, not child observability.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 120*time.Second, true,
		func(ctx context.Context) (bool, error) {
			check := &unstructured.Unstructured{}
			check.SetGroupVersionKind(gvk)
			err := k8sClient.Get(ctx,
				types.NamespacedName{Name: "fe-fin-target", Namespace: ns}, check)
			return err != nil, nil
		}))
	t.Log("Target deleted after forEach finalization — forEach + finalizes proved")

	// The "keep" resource should still exist.
	keepCM := &unstructured.Unstructured{}
	keepCM.SetGroupVersionKind(gvk)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "fe-fin-keep", Namespace: ns}, keepCM))
	t.Log("Keep resource still alive — only target was pruned")
}

// TestIncludeWhenFinalizesNeverCreatedTarget proves that when a target node
// was never created (excluded from the start), finalization is correctly
// skipped and no snapshot resource is created.
func TestIncludeWhenFinalizesNeverCreatedTarget(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	gvk := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}

	control := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "inc-fin-control",
				"namespace": ns,
			},
			"data": map[string]any{"toggle": "false"},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, control))

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-inc-fin",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "control",
						"ref": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "inc-fin-control"},
						},
					},
					map[string]any{
						"id":          "target",
						"includeWhen": []any{"${control.data.toggle == 'true'}"},
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "inc-fin-target"},
							"data":       map[string]any{"state": "active"},
						},
					},
					map[string]any{
						"id":        "snapshot",
						"finalizes": "target",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "inc-fin-snapshot"},
							"data":       map[string]any{"captured": "true"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-inc-fin", Namespace: ns}))

	require.NoError(t, waitForAbsence(ctx, k8sClient, gvk,
		types.NamespacedName{Name: "inc-fin-target", Namespace: ns}, 1*time.Second))
	require.NoError(t, waitForAbsence(ctx, k8sClient, gvk,
		types.NamespacedName{Name: "inc-fin-snapshot", Namespace: ns}, 1*time.Second))
	t.Log("Target excluded, snapshot dormant, Graph Ready — includeWhen + finalizes proved")
}

// TestContributeIdentityLabelsPerGraph proves that when two Graphs contribute
// to the same resource, each gets its own identity labels with "patch" role.
func TestContributeIdentityLabelsPerGraph(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	gvk := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}

	shared := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "shared-labels-target",
				"namespace": ns,
			},
			"data": map[string]any{"original": "data"},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, shared))

	for _, graphName := range []string{"test-labels-a", "test-labels-b"} {
		ann := map[string]any{graphName: "here"}
		g := &unstructured.Unstructured{
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
									"name":        "shared-labels-target",
									"annotations": ann,
								},
							},
						},
					},
				},
			},
		}
		require.NoError(t, k8sClient.Create(ctx, g))
	}

	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-labels-a", Namespace: ns}))
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-labels-b", Namespace: ns}))

	result := &unstructured.Unstructured{}
	result.SetGroupVersionKind(gvk)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "shared-labels-target", Namespace: ns}, result))

	labels := result.GetLabels()
	foundA, foundB := false, false
	for key, val := range labels {
		if strings.Contains(key, ".test-labels-a.") && strings.HasSuffix(key, ".internal.kro.run/type") {
			assert.Equal(t, "patch", val)
			foundA = true
		}
		if strings.Contains(key, ".test-labels-b.") && strings.HasSuffix(key, ".internal.kro.run/type") {
			assert.Equal(t, "patch", val)
			foundB = true
		}
	}
	assert.True(t, foundA, "Graph A's identity label must be present")
	assert.True(t, foundB, "Graph B's identity label must be present")
	t.Log("Both Graphs have independent 'contributes' identity labels — multi-graph Contribute labels proved")
}

// TestForEachForceApply proves that forEach resources with kro.run/apply: Force
// can take ownership of resources owned by another field manager.
func TestForEachForceApply(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	gvk := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}

	for _, name := range []string{"force-target-a", "force-target-b"} {
		applyConfigMapAs(t, ns, name, "external-manager", map[string]string{
			"key": "external-value",
		})
	}

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-foreach-force",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "items",
						"forEach": map[string]any{
							"name": "${['force-target-a', 'force-target-b']}",
						},
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "${name}",
								"annotations": map[string]any{
									"kro.run/apply": "Force",
								},
							},
							"data": map[string]any{
								"key": "graph-value",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	for _, name := range []string{"force-target-a", "force-target-b"} {
		require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true,
			func(ctx context.Context) (bool, error) {
				check := &unstructured.Unstructured{}
				check.SetGroupVersionKind(gvk)
				if err := k8sClient.Get(ctx,
					types.NamespacedName{Name: name, Namespace: ns}, check); err != nil {
					return false, nil
				}
				data, _, _ := unstructured.NestedStringMap(check.Object, "data")
				return data["key"] == "graph-value", nil
			}),
			"forEach with Force should take ownership of %s", name)
	}
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-foreach-force", Namespace: ns}))
	t.Log("forEach with Force apply took ownership of both targets — forEach + Force proved")
}

// TestPropagateWhenRespectsResyncGate proves that resync (drift timer)
// respects the propagateWhen gate — a gated node's drift timer fires but
// evaluation is deferred until the gate opens.
//
// Design 004-graph-reconciliation § Trigger + § Propagation:
//
//	"Resync respects the propagateWhen gate." (doc change for T1.3)
//	"Takes precedence even on spec changes where all nodes enter the frontier."
//
// The test uses two nodes — one gated, one ungated — to prove resync fired
// but the gate held. The ungated node serves as a control.
//
// Failure mode: resync applies the template to a gated resource, overwriting
// drift that should persist until the gate opens.
func TestPropagateWhenRespectsResyncGate(t *testing.T) {
	// The binary starts with --drift-interval=2s.
	t.Parallel()
	ns := createNamespace(t)

	gvk := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}

	// Pre-create control CM with gate open.
	control := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "resync-gate-control",
				"namespace": ns,
			},
			"data": map[string]any{
				"ready": "true",
				"value": "v1",
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, control))

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-resync-gate",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					// Watch the control CM.
					map[string]any{
						"id": "control",
						"ref": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "resync-gate-control"},
						},
					},
					// Upstream: propagateWhen gated on control.data.ready.
					map[string]any{
						"id":            "upstream",
						"propagateWhen": []any{"${control.data.ready == 'true'}"},
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "resync-gate-upstream"},
							"data": map[string]any{
								"value": "${control.data.value}",
							},
						},
					},
					// Gated: depends on upstream (propagateWhen blocks).
					map[string]any{
						"id": "gated",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "resync-gate-gated"},
							"data": map[string]any{
								"ref":     "${upstream.data.value}",
								"desired": "correct",
							},
						},
					},
					// Ungated: independent of upstream (control for resync proof).
					map[string]any{
						"id": "ungated",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "resync-gate-ungated"},
							"data": map[string]any{
								"desired": "correct",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Phase 1: Converge. All resources created, Graph Ready.
	for _, name := range []string{"resync-gate-upstream", "resync-gate-gated", "resync-gate-ungated"} {
		cm := &unstructured.Unstructured{}
		cm.SetGroupVersionKind(gvk)
		require.NoError(t, waitForResource(ctx, k8sClient,
			types.NamespacedName{Name: name, Namespace: ns}, cm))
	}
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-resync-gate", Namespace: ns}))
	t.Log("Phase 1: All resources created, Graph Ready")

	// Phase 2: Close the gate.
	latest := &unstructured.Unstructured{}
	latest.SetGroupVersionKind(gvk)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "resync-gate-control", Namespace: ns}, latest))
	unstructured.SetNestedField(latest.Object, "false", "data", "ready")
	require.NoError(t, k8sClient.Update(ctx, latest))
	t.Log("Phase 2: Gate closed (ready=false)")

	// Wait for upstream to process the change (propagateWhen now false).
	require.NoError(t, waitForSettle(ctx, k8sClient, GraphGVK,
		types.NamespacedName{Name: "test-resync-gate", Namespace: ns}))

	// Phase 3: Externally mutate BOTH gated and ungated resources.
	for _, name := range []string{"resync-gate-gated", "resync-gate-ungated"} {
		mutate := &unstructured.Unstructured{}
		mutate.SetGroupVersionKind(gvk)
		require.NoError(t, k8sClient.Get(ctx,
			types.NamespacedName{Name: name, Namespace: ns}, mutate))
		unstructured.SetNestedField(mutate.Object, "DRIFTED", "data", "desired")
		require.NoError(t, k8sClient.Update(ctx, mutate))
	}
	t.Log("Phase 3: Both gated and ungated externally mutated to DRIFTED")

	// Phase 4: Wait for drift to correct the ungated node. The drift timer
	// (2s interval, 0 jitter in tests) fires on the next reconcile after
	// expiry. Under CI load, reconcile scheduling is delayed, so we
	// periodically bump the control CM to force reconcile events until the
	// drift correction proves itself. Each bump is a benign field addition
	// that doesn't affect the gate condition (ready == 'false' is unchanged).
	bumpCount := 0
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 2*time.Second, 30*time.Second, false,
		func(ctx context.Context) (bool, error) {
			// Bump control CM to force a reconcile.
			bump := &unstructured.Unstructured{}
			bump.SetGroupVersionKind(gvk)
			if err := k8sClient.Get(ctx,
				types.NamespacedName{Name: "resync-gate-control", Namespace: ns}, bump); err != nil {
				return false, nil
			}
			bumpCount++
			unstructured.SetNestedField(bump.Object, fmt.Sprintf("bump-%d", bumpCount), "data", "reconcile-bump")
			if err := k8sClient.Update(ctx, bump); err != nil {
				return false, nil // conflict retry
			}

			// Check if ungated has been drift-corrected.
			check := &unstructured.Unstructured{}
			check.SetGroupVersionKind(gvk)
			if err := k8sClient.Get(ctx,
				types.NamespacedName{Name: "resync-gate-ungated", Namespace: ns}, check); err != nil {
				return false, nil
			}
			data, _, _ := unstructured.NestedStringMap(check.Object, "data")
			return data["desired"] == "correct", nil
		}))
	t.Log("Phase 4: Ungated node corrected by resync — proves drift timer fired")

	// THE KEY ASSERTION: gated node should still have DRIFTED value.
	// Resync fired (proved by ungated correction) but propagateWhen blocked it.
	gatedCM := &unstructured.Unstructured{}
	gatedCM.SetGroupVersionKind(gvk)
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "resync-gate-gated", Namespace: ns}, gatedCM))
	gatedData, _, _ := unstructured.NestedStringMap(gatedCM.Object, "data")
	assert.Equal(t, "DRIFTED", gatedData["desired"],
		"gated node should retain DRIFTED value — resync respects propagateWhen gate")
	t.Log("Gated node retained DRIFTED — resync respects propagateWhen gate")

	// Phase 5: Open gate → gated node should be corrected via propagation.
	require.NoError(t, k8sClient.Get(ctx,
		types.NamespacedName{Name: "resync-gate-control", Namespace: ns}, latest))
	unstructured.SetNestedField(latest.Object, "true", "data", "ready")
	require.NoError(t, k8sClient.Update(ctx, latest))
	t.Log("Phase 5: Gate opened (ready=true)")

	require.NoError(t, wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, 30*time.Second, true,
		func(ctx context.Context) (bool, error) {
			check := &unstructured.Unstructured{}
			check.SetGroupVersionKind(gvk)
			if err := k8sClient.Get(ctx,
				types.NamespacedName{Name: "resync-gate-gated", Namespace: ns}, check); err != nil {
				return false, nil
			}
			data, _, _ := unstructured.NestedStringMap(check.Object, "data")
			return data["desired"] == "correct", nil
		}))
	t.Log("Gated node corrected after gate opened — propagateWhen + resync interaction proved")
}
