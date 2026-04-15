package graphcontroller_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ---------------------------------------------------------------------------
// Definition nodes — 001-graph.md § template, 004-graph-reconciliation.md
// ---------------------------------------------------------------------------

// TestDefinesBasic proves that a definition node (no apiVersion/kind) puts
// values into scope and downstream nodes can reference them. No
// Kubernetes resource is created for the definition node.
func TestDefinesBasic(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-defines-basic",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "naming",
						"template": map[string]any{
							"prefix":  "myapp",
							"version": "v1",
						},
					},
					map[string]any{
						"id": "config",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name": "${naming.prefix + '-config'}",
							},
							"data": map[string]any{
								"prefix":  "${naming.prefix}",
								"version": "${naming.version}",
								"full":    "${naming.prefix + '-' + naming.version}",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, graph) })

	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-defines-basic", Namespace: ns}))

	cm := &unstructured.Unstructured{}
	cm.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{
		Namespace: ns, Name: "myapp-config",
	}, cm))

	data, _, _ := unstructured.NestedStringMap(cm.Object, "data")
	assert.Equal(t, "myapp", data["prefix"])
	assert.Equal(t, "v1", data["version"])
	assert.Equal(t, "myapp-v1", data["full"])

	// No resource named "naming" should exist — definition nodes produce no resources.
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMapList"})
	require.NoError(t, k8sClient.List(ctx, list, client.InNamespace(ns)))
	for _, item := range list.Items {
		assert.NotEqual(t, "naming", item.GetName(), "definition node must not create a resource")
	}
}

// TestDefinesForEach proves that forEach + definition node produces []any
// of computed maps in scope.
func TestDefinesForEach(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-defines-foreach",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "workers",
						"forEach": map[string]any{
							"w": "${['alpha', 'beta', 'gamma']}",
						},
						"template": map[string]any{
							"name": "${w}",
							"tag":  "${w + '-worker'}",
						},
					},
					map[string]any{
						"id": "summary",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "worker-summary"},
							"data": map[string]any{
								"count":       "${string(size(workers))}",
								"firstWorker": "${workers[0].name}",
								"firstTag":    "${workers[0].tag}",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, graph) })

	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-defines-foreach", Namespace: ns}))

	summary := &unstructured.Unstructured{}
	summary.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{
		Namespace: ns, Name: "worker-summary",
	}, summary))

	data, _, _ := unstructured.NestedStringMap(summary.Object, "data")
	assert.Equal(t, "3", data["count"])
	assert.Equal(t, "alpha", data["firstWorker"])
	assert.Equal(t, "alpha-worker", data["firstTag"])
}
