package graphcontroller_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

// ═══════════════════════════════════════════════════════════════════════════════
// Kind propagateWhen and node-level readyWhen integration tests
//
// These test propagateWhen at the Kind spec level and node-level readyWhen
// controlling per-instance readiness. The pipeline is:
//
//   spec.nodes[].readyWhen → L2 Graph node readyWhen → Graph CR Ready condition
//   spec.propagateWhen → L1 instances forEach propagateWhen → rollout gate
//   L1 instances forEach readyWhen → checks Graph CR Ready → stamps __ready
//
// Tests require the stdlib type tower (Kind CRD) to be materialized.
// ═══════════════════════════════════════════════════════════════════════════════

// TestStdlibKindNodeReadyWhenNotSatisfied proves that node-level readyWhen
// on a Kind's nodes controls the per-instance Graph's Ready condition.
// When a node's readyWhen is unsatisfied, the per-instance Graph is NotReady.
func TestStdlibKindNodeReadyWhenNotSatisfied(t *testing.T) {
	t.Parallel()
	require.NoError(t, waitForCRD(ctx, k8sClient, "kinds.experimental.kro.run", stdlibCRDTimeout))

	t.Log("creating Kind with node-level readyWhen (unsatisfied)")
	kind := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "experimental.kro.run/v1alpha1",
		"kind":       "Kind",
		"metadata": map[string]any{
			"name":      "nodereadywhen",
			"namespace": "kro-system",
		},
		"spec": map[string]any{
			"schema": map[string]any{
				"apiVersion": "test.stdlib.kro.run/v1alpha1",
				"kind":       "NodeReadyWhen",
				"spec": map[string]any{
					"message": "string | default=hello",
				},
			},
			"nodes": []any{
				map[string]any{
					"id": "cm",
					"template": map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata": map[string]any{
							"name":      "${schema.metadata.name}-config",
							"namespace": "${schema.metadata.namespace}",
						},
						"data": map[string]any{
							"message": "${schema.spec.message}",
							"status":  "pending",
						},
					},
					// Node-level readyWhen: unsatisfied because status != 'healthy'
					"readyWhen": []any{
						"${cm.data.status == 'healthy'}",
					},
				},
			},
		},
	}}
	require.NoError(t, k8sClient.Create(ctx, kind))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), kind) })

	t.Log("waiting for NodeReadyWhen CRD...")
	require.NoError(t, waitForCRD(ctx, k8sClient, "nodereadywhens.test.stdlib.kro.run", stdlibCRDTimeout))

	// Create an instance.
	instance := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "test.stdlib.kro.run/v1alpha1",
		"kind":       "NodeReadyWhen",
		"metadata": map[string]any{
			"name":      "nrw-inst",
			"namespace": "kro-system",
		},
		"spec": map[string]any{"message": "test"},
	}}
	require.NoError(t, k8sClient.Create(ctx, instance))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), instance) })

	// Wait for the ConfigMap.
	cm := &unstructured.Unstructured{}
	cm.SetAPIVersion("v1")
	cm.SetKind("ConfigMap")
	require.NoError(t, waitForResource(ctx, k8sClient,
		types.NamespacedName{Name: "nrw-inst-config", Namespace: "kro-system"}, cm, stdlibReconcileTimeout),
		"ConfigMap not created")

	data, _, _ := unstructured.NestedStringMap(cm.Object, "data")
	assert.Equal(t, "pending", data["status"])

	// Per-instance Graph should be NotReady (node readyWhen unsatisfied).
	graphName := "kro-system-nrw-inst-nodereadywhen"
	require.NoError(t, waitForGraphReadyStatus(ctx, k8sClient,
		types.NamespacedName{Name: graphName, Namespace: "kro-system"}, "Unknown", stdlibReconcileTimeout),
		"per-instance Graph should be NotReady")
	t.Log("Per-instance Graph NotReady — node-level readyWhen unsatisfied (status=pending)")
}

// TestStdlibKindNodeReadyWhenSatisfied proves the happy path: node-level
// readyWhen satisfied → per-instance Graph Ready.
func TestStdlibKindNodeReadyWhenSatisfied(t *testing.T) {
	t.Parallel()
	require.NoError(t, waitForCRD(ctx, k8sClient, "kinds.experimental.kro.run", stdlibCRDTimeout))

	t.Log("creating Kind with node-level readyWhen (satisfied)")
	kind := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "experimental.kro.run/v1alpha1",
		"kind":       "Kind",
		"metadata": map[string]any{
			"name":      "nodereadywhenok",
			"namespace": "kro-system",
		},
		"spec": map[string]any{
			"schema": map[string]any{
				"apiVersion": "test.stdlib.kro.run/v1alpha1",
				"kind":       "NodeReadyWhenOk",
				"spec": map[string]any{
					"value": "string | default=test",
				},
			},
			"nodes": []any{
				map[string]any{
					"id": "cm",
					"template": map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata": map[string]any{
							"name":      "${schema.metadata.name}-config",
							"namespace": "${schema.metadata.namespace}",
						},
						"data": map[string]any{
							"value":  "${schema.spec.value}",
							"status": "healthy",
						},
					},
					// Node-level readyWhen: always satisfied.
					"readyWhen": []any{
						"${cm.data.status == 'healthy'}",
					},
				},
			},
		},
	}}
	require.NoError(t, k8sClient.Create(ctx, kind))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), kind) })

	t.Log("waiting for NodeReadyWhenOk CRD...")
	require.NoError(t, waitForCRD(ctx, k8sClient, "nodereadywhenoks.test.stdlib.kro.run", stdlibCRDTimeout))

	instance := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "test.stdlib.kro.run/v1alpha1",
		"kind":       "NodeReadyWhenOk",
		"metadata": map[string]any{
			"name":      "nrwok-inst",
			"namespace": "kro-system",
		},
		"spec": map[string]any{"value": "test-value"},
	}}
	require.NoError(t, k8sClient.Create(ctx, instance))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), instance) })

	// Per-instance Graph should be Ready.
	graphName := "kro-system-nrwok-inst-nodereadywhenok"
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: graphName, Namespace: "kro-system"}, stdlibReconcileTimeout),
		"per-instance Graph should be Ready")
	t.Log("Per-instance Graph Ready — node-level readyWhen satisfied")
}

// TestStdlibKindDefaultBehavior proves that a Kind without propagateWhen
// behaves exactly as before: all instances processed, per-instance Graph
// Ready when all nodes applied.
func TestStdlibKindDefaultBehavior(t *testing.T) {
	t.Parallel()
	require.NoError(t, waitForCRD(ctx, k8sClient, "kinds.experimental.kro.run", stdlibCRDTimeout))

	t.Log("creating Kind without propagateWhen")
	kind := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "experimental.kro.run/v1alpha1",
		"kind":       "Kind",
		"metadata": map[string]any{
			"name":      "defaultbehaviorthing",
			"namespace": "kro-system",
		},
		"spec": map[string]any{
			"schema": map[string]any{
				"apiVersion": "test.stdlib.kro.run/v1alpha1",
				"kind":       "DefaultBehaviorThing",
				"spec": map[string]any{
					"value": "string | default=test",
				},
			},
			"nodes": []any{
				map[string]any{
					"id": "cm",
					"template": map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata": map[string]any{
							"name":      "${schema.metadata.name}-config",
							"namespace": "${schema.metadata.namespace}",
						},
						"data": map[string]any{
							"value": "${schema.spec.value}",
						},
					},
				},
			},
		},
	}}
	require.NoError(t, k8sClient.Create(ctx, kind))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), kind) })

	t.Log("waiting for DefaultBehaviorThing CRD...")
	require.NoError(t, waitForCRD(ctx, k8sClient, "defaultbehaviorthings.test.stdlib.kro.run", stdlibCRDTimeout))

	instance := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "test.stdlib.kro.run/v1alpha1",
		"kind":       "DefaultBehaviorThing",
		"metadata": map[string]any{
			"name":      "db-inst",
			"namespace": "kro-system",
		},
		"spec": map[string]any{"value": "default-test"},
	}}
	require.NoError(t, k8sClient.Create(ctx, instance))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), instance) })

	cm := &unstructured.Unstructured{}
	cm.SetAPIVersion("v1")
	cm.SetKind("ConfigMap")
	require.NoError(t, waitForResource(ctx, k8sClient,
		types.NamespacedName{Name: "db-inst-config", Namespace: "kro-system"}, cm, stdlibReconcileTimeout),
		"ConfigMap not created")

	data, _, _ := unstructured.NestedStringMap(cm.Object, "data")
	assert.Equal(t, "default-test", data["value"])

	graphName := "kro-system-db-inst-defaultbehaviorthing"
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: graphName, Namespace: "kro-system"}, stdlibReconcileTimeout),
		"per-instance Graph should be Ready")
	t.Log("Default behavior preserved — no propagateWhen, no readyWhen, Graph Ready on apply")
}

// TestStdlibKindWithPropagateWhen proves that Kind-level propagateWhen is
// threaded to the L1 instances forEach. Uses an empty gate to verify the
// field is accepted without breaking the Kind pipeline.
func TestStdlibKindWithPropagateWhen(t *testing.T) {
	t.Parallel()
	require.NoError(t, waitForCRD(ctx, k8sClient, "kinds.experimental.kro.run", stdlibCRDTimeout))

	t.Log("creating Kind with propagateWhen")
	kind := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "experimental.kro.run/v1alpha1",
		"kind":       "Kind",
		"metadata": map[string]any{
			"name":      "propagatewhenthing",
			"namespace": "kro-system",
		},
		"spec": map[string]any{
			"schema": map[string]any{
				"apiVersion": "test.stdlib.kro.run/v1alpha1",
				"kind":       "PropagateWhenThing",
				"spec": map[string]any{
					"label": "string | default=default",
				},
			},
			// Empty propagateWhen — verifies the field is accepted and
			// threads through to the L1 instances forEach without
			// breaking the Kind pipeline.
			"propagateWhen": []any{},
			"nodes": []any{
				map[string]any{
					"id": "cm",
					"template": map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata": map[string]any{
							"name":      "${schema.metadata.name}-config",
							"namespace": "${schema.metadata.namespace}",
						},
						"data": map[string]any{
							"label": "${schema.spec.label}",
						},
					},
				},
			},
		},
	}}
	require.NoError(t, k8sClient.Create(ctx, kind))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), kind) })

	t.Log("waiting for PropagateWhenThing CRD...")
	require.NoError(t, waitForCRD(ctx, k8sClient, "propagatewhenthings.test.stdlib.kro.run", stdlibCRDTimeout))

	inst := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "test.stdlib.kro.run/v1alpha1",
		"kind":       "PropagateWhenThing",
		"metadata": map[string]any{
			"name":      "pw-inst",
			"namespace": "kro-system",
		},
		"spec": map[string]any{"label": "test"},
	}}
	require.NoError(t, k8sClient.Create(ctx, inst))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), inst) })

	cm := &unstructured.Unstructured{}
	cm.SetAPIVersion("v1")
	cm.SetKind("ConfigMap")
	require.NoError(t, waitForResource(ctx, k8sClient,
		types.NamespacedName{Name: "pw-inst-config", Namespace: "kro-system"}, cm, stdlibReconcileTimeout),
		"ConfigMap not created")

	data, _, _ := unstructured.NestedStringMap(cm.Object, "data")
	assert.Equal(t, "test", data["label"])
	t.Log("Kind with propagateWhen field accepted — instance processed normally")
}
