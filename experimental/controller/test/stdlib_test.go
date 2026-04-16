package graphcontroller_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
)

// ═══════════════════════════════════════════════════════════════════════════════
// Standard library integration tests
//
// These tests exercise the three stdlib types that bootstrap installs:
// Kind, Decorator, and Singleton. The controller binary starts with
// --bootstrap, which installs Graph/GraphRevision CRDs and applies stdlib
// resources. By the time tests run, the type tower is materialized:
//
//   kind.yaml (Graph) → Kind CRD
//   decorator.yaml (Kind) → Decorator CRD
//   singleton.yaml (Kind) → Singleton CRD
//
// Each test creates instances of these types and verifies end-to-end
// behavior through multiple layers of Graph reconciliation.
// ═══════════════════════════════════════════════════════════════════════════════

// stdlibCRDTimeout is how long to wait for a CRD that bootstrap creates.
const stdlibCRDTimeout = 60 * time.Second

// stdlibReconcileTimeout is how long to wait for multi-layer reconciliation.
const stdlibReconcileTimeout = 120 * time.Second

// ═══════════════════════════════════════════════════════════════════════════════
// Kind
// ═══════════════════════════════════════════════════════════════════════════════

func TestStdlibKind(t *testing.T) {
	require.NoError(t, waitForCRD(ctx, k8sClient, "kinds.experimental.kro.run", stdlibCRDTimeout))

	t.Log("creating Kind: TestWidget")
	kind := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "experimental.kro.run/v1alpha1",
		"kind":       "Kind",
		"metadata": map[string]any{
			"name":      "testwidget",
			"namespace": "kro-system",
		},
		"spec": map[string]any{
			"kind":    "TestWidget",
			"group":   "test.stdlib.kro.run",
			"version": "v1alpha1",
			"schema": map[string]any{
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
						},
					},
				},
			},
		},
	}}
	require.NoError(t, k8sClient.Create(ctx, kind))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), kind) })

	t.Log("waiting for TestWidget CRD...")
	require.NoError(t, waitForCRD(ctx, k8sClient, "testwidgets.test.stdlib.kro.run", stdlibCRDTimeout))
	t.Log("TestWidget CRD established")

	widget := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "test.stdlib.kro.run/v1alpha1",
		"kind":       "TestWidget",
		"metadata": map[string]any{
			"name":      "my-widget",
			"namespace": "kro-system",
		},
		"spec": map[string]any{
			"message": "hello from kind test",
		},
	}}
	require.NoError(t, k8sClient.Create(ctx, widget))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), widget) })

	t.Log("waiting for per-instance ConfigMap...")
	cm := &unstructured.Unstructured{}
	cm.SetAPIVersion("v1")
	cm.SetKind("ConfigMap")
	cmKey := types.NamespacedName{Name: "my-widget-config", Namespace: "kro-system"}

	require.NoError(t, wait.PollUntilContextTimeout(ctx, 2*time.Second, stdlibReconcileTimeout, true,
		func(ctx context.Context) (bool, error) {
			if err := k8sClient.Get(ctx, cmKey, cm); err != nil {
				return false, nil
			}
			return true, nil
		}), "ConfigMap my-widget-config not created within timeout")

	data, _, _ := unstructured.NestedStringMap(cm.Object, "data")
	assert.Equal(t, "hello from kind test", data["message"])
	t.Log("Kind pipeline works: Kind → CRD → instance → ConfigMap")
}

// ═══════════════════════════════════════════════════════════════════════════════
// Decorator
// ═══════════════════════════════════════════════════════════════════════════════

func TestStdlibDecorator(t *testing.T) {
	require.NoError(t, waitForCRD(ctx, k8sClient, "decorators.experimental.kro.run", stdlibCRDTimeout))

	t.Log("creating Decorator: configmap-annotator")
	decorator := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "experimental.kro.run/v1alpha1",
		"kind":       "Decorator",
		"metadata": map[string]any{
			"name":      "configmap-annotator",
			"namespace": "kro-system",
		},
		"spec": map[string]any{
			"watch": map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"selector":   map[string]any{"stdlib-test": "decorator"},
			},
			"nodes": []any{
				map[string]any{
					"id": "companion",
					"template": map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata": map[string]any{
							"name":      "${item.metadata.name}-decorated",
							"namespace": "${item.metadata.namespace}",
						},
						"data": map[string]any{
							"source": "${item.metadata.name}",
						},
					},
				},
			},
		},
	}}
	require.NoError(t, k8sClient.Create(ctx, decorator))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), decorator) })

	// WatchKind scopes to the Graph's namespace, so watched ConfigMaps must be in kro-system.
	t.Log("creating watched ConfigMap in kro-system")
	source := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":      "my-config",
			"namespace": "kro-system",
			"labels":    map[string]any{"stdlib-test": "decorator"},
		},
		"data": map[string]any{"key": "value"},
	}}
	require.NoError(t, k8sClient.Create(ctx, source))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), source) })

	t.Log("waiting for companion ConfigMap...")
	companion := &unstructured.Unstructured{}
	companion.SetAPIVersion("v1")
	companion.SetKind("ConfigMap")
	companionKey := types.NamespacedName{Name: "my-config-decorated", Namespace: "kro-system"}

	require.NoError(t, wait.PollUntilContextTimeout(ctx, 2*time.Second, stdlibReconcileTimeout, true,
		func(ctx context.Context) (bool, error) {
			if err := k8sClient.Get(ctx, companionKey, companion); err != nil {
				return false, nil
			}
			return true, nil
		}), "companion ConfigMap my-config-decorated not created within timeout")

	data, _, _ := unstructured.NestedStringMap(companion.Object, "data")
	assert.Equal(t, "my-config", data["source"])
	t.Log("Decorator pipeline works: Decorator → WatchKind → sub-Graph → companion ConfigMap")
}

// ═══════════════════════════════════════════════════════════════════════════════
// Singleton
// ═══════════════════════════════════════════════════════════════════════════════

func TestStdlibSingleton(t *testing.T) {
	// Singleton end-to-end is blocked on rearchitecting the shared resolution
	// Graph (SSA field manager deadlock on failover) and implementing
	// TemplateExpr support for dynamic templates. The CRD materializes
	// correctly; exercising priority resolution requires a follow-up PR.
	t.Skip("Singleton end-to-end blocked on resolution Graph rearchitecture")
}
