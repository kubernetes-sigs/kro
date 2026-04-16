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
// Bootstrap may still be running when tests start (the health check only
// confirms the HTTP endpoint, not that all CRDs are materialized).
const stdlibCRDTimeout = 60 * time.Second

// stdlibReconcileTimeout is how long to wait for multi-layer reconciliation
// chains (Kind → CRD → instance → child resource). More layers = more
// reconcile loops = more time.
const stdlibReconcileTimeout = 120 * time.Second

// ═══════════════════════════════════════════════════════════════════════════════
// Kind
//
// Define a new Kubernetes type. The Kind controller creates a CRD per Kind,
// watches instances of that CRD, and stamps a per-instance Graph whose
// nodes produce child resources.
//
// Pipeline: Kind → CRD → instance → per-instance Graph → child resources
// ═══════════════════════════════════════════════════════════════════════════════

func TestStdlibKind(t *testing.T) {
	t.Parallel()
	require.NoError(t, waitForCRD(ctx, k8sClient, "kinds.experimental.kro.run", stdlibCRDTimeout))

	// Phase 1: Create a Kind that defines TestWidget.
	// Kind + instances live in kro-system (Kind controller is namespace-scoped there).
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

	// Phase 2: Wait for the TestWidget CRD.
	t.Log("waiting for TestWidget CRD...")
	require.NoError(t, waitForCRD(ctx, k8sClient, "testwidgets.test.stdlib.kro.run", stdlibCRDTimeout))
	t.Log("TestWidget CRD established")

	// Phase 3: Create a TestWidget instance.
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

	// Phase 4: Verify the per-instance ConfigMap appears with correct data.
	// Chain: Kind → CRD → instance → per-instance Graph → ConfigMap.
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
	assert.Equal(t, "hello from kind test", data["message"],
		"ConfigMap should carry the widget's message")
	t.Log("Kind pipeline works: Kind → CRD → instance → ConfigMap")
}

// ═══════════════════════════════════════════════════════════════════════════════
// Decorator
//
// Watch instances of an existing kind and create a sub-Graph per item.
// Each sub-Graph gets an auto-prepended "item" Watch node. User nodes
// reference ${item.*} to derive child resources from the watched object.
//
// Pipeline: Decorator → controller Graph → items (WatchKind) → sub-Graph per item → child resources
// ═══════════════════════════════════════════════════════════════════════════════

func TestStdlibDecorator(t *testing.T) {
	// Intentionally serial: this Decorator's items WatchKind uses an empty
	// selector, so it picks up every ConfigMap in kro-system — including
	// those created by TestStdlibKind when running concurrently. Parallel
	// execution is safe but stamps extra "-decorated" ConfigMaps that
	// inflate runtime and muddy the test's intent. Keep serial until the
	// stdlib Decorator template honors spec.watch.selector.
	require.NoError(t, waitForCRD(ctx, k8sClient, "decorators.experimental.kro.run", stdlibCRDTimeout))

	// Phase 1: Create a Decorator that watches ConfigMaps and creates
	// a companion ConfigMap per item with derived data.
	//
	// The Decorator's controller Graph lives in kro-system, and WatchKind
	// scopes to the Graph's namespace. So watched items must be in kro-system.
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

	// Phase 2: Create a ConfigMap in kro-system with the matching label.
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

	// Phase 3: Verify the companion ConfigMap appears in kro-system.
	// Chain: Decorator → controller Graph → WatchKind picks up my-config →
	// sub-Graph → companion ConfigMap.
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
	assert.Equal(t, "my-config", data["source"],
		"companion should reference the source ConfigMap's name")
	t.Log("Decorator pipeline works: Decorator → WatchKind → sub-Graph → companion ConfigMap")
}

// ═══════════════════════════════════════════════════════════════════════════════
// Singleton
//
// Declare a resource that should exist exactly once. When multiple
// Singletons target the same resource (same GVK + namespace + name),
// the highest priority wins. Ties broken by lowest name.
//
// Implemented as Kind + Decorator. The Decorator watches all Singletons
// and creates a sub-Graph per item. Each sub-Graph self-determines if
// it's the winner via includeWhen. The target node uses TemplateExpr
// (template: "${item.spec.template}") to forward the full resource spec.
//
// Pipeline: Singleton → Decorator sub-Graph → peers WatchKind →
// includeWhen gate → target resource (if winner)
// ═══════════════════════════════════════════════════════════════════════════════

func TestStdlibSingleton(t *testing.T) {
	t.Parallel()
	require.NoError(t, waitForCRD(ctx, k8sClient, "singletons.experimental.kro.run", stdlibCRDTimeout))

	ns := createNamespace(t)

	// Phase 1: Create two Singletons targeting the same ConfigMap with
	// different priorities. The higher priority should win.
	t.Log("creating Singleton: team-a (priority 10)")
	singletonA := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "experimental.kro.run/v1alpha1",
		"kind":       "Singleton",
		"metadata": map[string]any{
			"name":      "team-a",
			"namespace": "kro-system",
		},
		"spec": map[string]any{
			"priority": int64(10),
			"template": map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]any{"name": "contested", "namespace": ns},
				"data":       map[string]any{"owner": "team-a"},
			},
		},
	}}
	require.NoError(t, k8sClient.Create(ctx, singletonA))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), singletonA) })

	t.Log("creating Singleton: team-b (priority 100)")
	singletonB := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "experimental.kro.run/v1alpha1",
		"kind":       "Singleton",
		"metadata": map[string]any{
			"name":      "team-b",
			"namespace": "kro-system",
		},
		"spec": map[string]any{
			"priority": int64(100),
			"template": map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]any{"name": "contested", "namespace": ns},
				"data":       map[string]any{"owner": "team-b"},
			},
		},
	}}
	require.NoError(t, k8sClient.Create(ctx, singletonB))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), singletonB) })

	// Phase 2: Verify the ConfigMap appears with team-b's content (higher priority).
	t.Log("waiting for contested ConfigMap (expecting team-b wins)...")
	cm := &unstructured.Unstructured{}
	cm.SetAPIVersion("v1")
	cm.SetKind("ConfigMap")
	cmKey := types.NamespacedName{Name: "contested", Namespace: ns}

	require.NoError(t, wait.PollUntilContextTimeout(ctx, 2*time.Second, stdlibReconcileTimeout, true,
		func(ctx context.Context) (bool, error) {
			if err := k8sClient.Get(ctx, cmKey, cm); err != nil {
				return false, nil
			}
			data, _, _ := unstructured.NestedStringMap(cm.Object, "data")
			return data["owner"] == "team-b", nil
		}), "ConfigMap not created with team-b content within timeout")
	t.Log("team-b (priority 100) wins over team-a (priority 10)")

	// TODO: Failover test (delete team-b, verify team-a takes over).
	// Blocked on forEach prune: when Singleton team-b is deleted, the Kind
	// controller's L1 forEach should prune team-b's per-instance Graph,
	// triggering teardown which deletes the ConfigMap. Then team-a's
	// includeWhen opens and it creates the ConfigMap. Currently forEach
	// scale-down prune doesn't fire (same root cause as
	// TestMultipleForEachNodesIndependence).
}
