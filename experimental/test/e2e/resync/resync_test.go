package resync_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
)

// TestResyncTimerCorrectsDrift proves the resync timer ownership enforcement:
// external mutations to managed fields persist during normal watch-triggered
// reconciles (content-addressed apply optimization — template hash match skips
// the Patch), but the resync timer bypasses the hash check and applies
// unconditionally, restoring managed fields to the desired state.
//
// Per 005-reconciliation.md § Reconcile: "The resync timer bypasses the
// template-hash check — apply unconditionally, because server-side
// defaulters and mutating webhooks can change fields without changing
// the desired state hash. SSA is idempotent; the apply corrects drift
// as a side effect."
func TestResyncTimerCorrectsDrift(t *testing.T) {
	// The test binary starts with --node-resync-interval=5s, so resync correction
	// fires within a few seconds after external mutation.
	t.Parallel()
	ns := createNamespace(t)

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-resync",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "managed",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "resync-target"},
							"data": map[string]any{
								"desired": "correct-value",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for ConfigMap
	cmGVK := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}
	cm := &unstructured.Unstructured{}
	cm.SetGroupVersionKind(cmGVK)
	require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: "resync-target", Namespace: ns}, cm))
	data, _, _ := unstructured.NestedStringMap(cm.Object, "data")
	assert.Equal(t, "correct-value", data["desired"])
	t.Log("ConfigMap created with desired=correct-value")

	// Externally mutate the ConfigMap
	latest := &unstructured.Unstructured{}
	latest.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "resync-target", Namespace: ns}, latest))
	unstructured.SetNestedField(latest.Object, "DRIFTED", "data", "desired")
	require.NoError(t, k8sClient.Update(ctx, latest))
	t.Log("Externally mutated: desired=DRIFTED")

	// Wait for reconcile to settle — poll for correction.
	// The watch fires (resourceVersion changed), but the template hash matches
	// so the Patch is skipped during the watch-triggered reconcile. The resync
	// timer fires after the shortened interval (5s) and applies unconditionally,
	// correcting the drift.
	//
	// Per 005-reconciliation.md § Reconcile: "The resync timer bypasses the
	// template-hash check — apply unconditionally."
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		check := &unstructured.Unstructured{}
		check.SetGroupVersionKind(cmGVK)
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "resync-target", Namespace: ns}, check); err != nil {
			return false, nil
		}
		d, _, _ := unstructured.NestedStringMap(check.Object, "data")
		return d["desired"] == "correct-value", nil
	}))
	t.Log("Drift corrected by resync timer — managed field restored to desired state")

	// Now update the Graph spec to change the desired value — this SHOULD apply
	require.NoError(t, updateWithRetry(ctx, k8sClient, GraphGVK, types.NamespacedName{Name: "test-resync", Namespace: ns}, func(obj *unstructured.Unstructured) {
		nodes := []any{
			map[string]any{
				"id": "managed",
				"template": map[string]any{
					"apiVersion": "v1",
					"kind":       "ConfigMap",
					"metadata":   map[string]any{"name": "resync-target"},
					"data": map[string]any{
						"desired": "new-value",
					},
				},
			},
		}
		unstructured.SetNestedSlice(obj.Object, nodes, "spec", "nodes")
	}))
	t.Log("Updated Graph spec: desired=new-value")

	// Wait for the new value to be applied (template hash changed → Patch fires)
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		check := &unstructured.Unstructured{}
		check.SetGroupVersionKind(cmGVK)
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "resync-target", Namespace: ns}, check); err != nil {
			return false, nil
		}
		d, _, _ := unstructured.NestedStringMap(check.Object, "data")
		return d["desired"] == "new-value", nil
	}))
	t.Log("Spec change applied: desired=new-value — content-addressed apply proved")
}

// TestResyncTimer_RegressionSkippedNodeReset proves that resync timers for
// non-triggered nodes are NOT reset by reconciles driven by other nodes.
//
// Regression: the resync timer reset loop at the end of each reconcile
// unconditionally reset timers for ALL Ready/NotReady nodes, including
// nodes that were skipped (no trigger, eval hash match). In a multi-node
// graph, frequent reconciles driven by watch events on one node would
// perpetually reset the resync timer for stable nodes, preventing resync
// from ever firing.
//
// Per 005-reconciliation.md § Reconcile: "An SSA apply resets the resync
// timer. A skipped write during normal evaluation (hash match from a watch
// event or propagation trigger) does not — the timer still fires to catch
// divergence that the hash cannot detect."
//
// The invariant: external mutations to a stable node's resource are
// corrected within the resync interval, even when other nodes cause
// frequent reconciles.
func TestResyncTimer_RegressionSkippedNodeReset(t *testing.T) {
	// The test binary starts with --node-resync-interval=5s.
	t.Parallel()
	ns := createNamespace(t)

	// Two independent nodes — no dependency between them. Changes to one
	// should NOT affect the other's resync timer.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-resync-multi",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "stable",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "resync-stable"},
							"data":       map[string]any{"desired": "correct"},
						},
					},
					map[string]any{
						"id": "noisy",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "resync-noisy"},
							"data":       map[string]any{"counter": "0"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for both ConfigMaps to be created.
	cmGVK := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}
	for _, name := range []string{"resync-stable", "resync-noisy"} {
		cm := &unstructured.Unstructured{}
		cm.SetGroupVersionKind(cmGVK)
		require.NoError(t, waitForResource(ctx, k8sClient, types.NamespacedName{Name: name, Namespace: ns}, cm))
	}
	require.NoError(t, waitForSettle(ctx, k8sClient, GraphGVK, types.NamespacedName{Name: "test-resync-multi", Namespace: ns}))
	t.Log("Both ConfigMaps created, Graph is Ready")

	// Externally mutate the stable node's ConfigMap.
	stable := &unstructured.Unstructured{}
	stable.SetGroupVersionKind(cmGVK)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "resync-stable", Namespace: ns}, stable))
	unstructured.SetNestedField(stable.Object, "DRIFTED", "data", "desired")
	require.NoError(t, k8sClient.Update(ctx, stable))
	t.Log("Externally mutated stable node: desired=DRIFTED")

	// Generate reconcile traffic by updating the noisy node's spec.
	// This causes watch events → reconciles. With the old bug, each
	// reconcile would reset the stable node's resync timer, preventing
	// correction. With the fix, the stable node's timer is untouched
	// because the node is skipped (no trigger for it).
	require.NoError(t, updateWithRetry(ctx, k8sClient, GraphGVK,
		types.NamespacedName{Name: "test-resync-multi", Namespace: ns}, func(obj *unstructured.Unstructured) {
			unstructured.SetNestedSlice(obj.Object, []any{
				map[string]any{
					"id": "stable",
					"template": map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata":   map[string]any{"name": "resync-stable"},
						"data":       map[string]any{"desired": "correct"},
					},
				},
				map[string]any{
					"id": "noisy",
					"template": map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata":   map[string]any{"name": "resync-noisy"},
						"data":       map[string]any{"counter": "1"},
					},
				},
			}, "spec", "nodes")
		}))
	t.Log("Updated noisy node spec to force reconciles")

	// Wait for the resync timer to fire and correct the stable node.
	// With node-resync-interval=5s, this should happen within a few seconds.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		check := &unstructured.Unstructured{}
		check.SetGroupVersionKind(cmGVK)
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "resync-stable", Namespace: ns}, check); err != nil {
			return false, nil
		}
		data, _, _ := unstructured.NestedStringMap(check.Object, "data")
		return data["desired"] == "correct", nil
	}))
	t.Log("Drift corrected on stable node despite noisy reconciles — timer was not reset by unrelated activity")
}

// TestPropagateWhenRespectsResyncGate proves that resync respects the
// propagateWhen gate — a gated node's resync timer fires but evaluation
// is deferred until the gate opens.
//
// Design 005-reconciliation § Trigger + § Propagation:
//
//	"Resync respects the propagateWhen gate."
//	"Takes precedence even on spec changes where all nodes enter the frontier."
//
// The test uses two nodes — one gated, one ungated — to prove resync fired
// but the gate held. The ungated node serves as a control.
//
// Failure mode: resync applies the template to a gated resource, overwriting
// drift that should persist until the gate opens.
func TestPropagateWhenRespectsResyncGate(t *testing.T) {
	// The binary starts with --node-resync-interval=5s.
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
					// Upstream: no propagateWhen.
					map[string]any{
						"id": "upstream",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "resync-gate-upstream"},
							"data": map[string]any{
								"value": "${control.data.value}",
							},
						},
					},
					// Gated: depends on upstream, input-gated on control.
					map[string]any{
						"id":            "gated",
						"propagateWhen": []any{"${control.data.ready == 'true'}"},
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

	// Phase 4: Wait for resync to correct the ungated node. The resync timer
	// (5s interval) fires on the next reconcile after expiry. Under CI load,
	// reconcile scheduling is delayed, so we periodically bump the control CM
	// to force reconcile events until the resync correction proves itself.
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

			// Check if ungated has been resync-corrected.
			check := &unstructured.Unstructured{}
			check.SetGroupVersionKind(gvk)
			if err := k8sClient.Get(ctx,
				types.NamespacedName{Name: "resync-gate-ungated", Namespace: ns}, check); err != nil {
				return false, nil
			}
			data, _, _ := unstructured.NestedStringMap(check.Object, "data")
			return data["desired"] == "correct", nil
		}))
	t.Log("Phase 4: Ungated node corrected by resync — proves resync timer fired")

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
