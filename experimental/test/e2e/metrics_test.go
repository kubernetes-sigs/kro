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

// ---------------------------------------------------------------------------
// Metrics contract tests
//
// These test the Prometheus metrics as a user-facing contract. Operators
// build dashboards and alerts on these metrics — they must be present,
// correctly labeled, and cleaned up on Graph deletion.
//
// Promoted from unit tests in controller/metrics_test.go. The unit tests
// verified metrics registration and counter increments through the internal
// Prometheus registry. These tests verify the same behavior through the
// controller's /metrics HTTP endpoint — the surface operators actually use.
// ---------------------------------------------------------------------------

// TestMetricsNodeStateGauge proves that graph_node_state gauge is emitted
// for each node after the Graph reconciles, and that the labels match the
// Graph's name, namespace, and node IDs.
//
// Replaces: TestRegisterMetrics, TestMetricLabelsPerNode (unit)
func TestMetricsNodeStateGauge(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-metrics-gauge",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "config",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "metrics-gauge-cm"},
							"data":       map[string]any{"key": "value"},
						},
					},
					map[string]any{
						"id": "downstream",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "metrics-gauge-downstream"},
							"data":       map[string]any{"ref": "${config.metadata.name}"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-metrics-gauge", Namespace: ns}))

	// Both nodes should report Ready=1 in the gauge.
	require.NoError(t, waitForMetric(ctx, t,
		"graph_node_state",
		nodeStateLabels("test-metrics-gauge", ns, "config", "Ready"),
		1,
	))
	require.NoError(t, waitForMetric(ctx, t,
		"graph_node_state",
		nodeStateLabels("test-metrics-gauge", ns, "downstream", "Ready"),
		1,
	))

	// Non-active states should be 0.
	val, ok := scrapeGauge(t,
		"graph_node_state",
		nodeStateLabels("test-metrics-gauge", ns, "config", "Error"),
	)
	if ok {
		assert.Equal(t, float64(0), val,
			"config node should not be in Error state")
	}
}

// TestMetricsReconcileDurationHistogram proves that graph_reconcile_duration_seconds
// histogram is emitted with the correct graph labels after reconciliation.
//
// Replaces: TestReconcileDurationHistogramRegistered (unit)
func TestMetricsReconcileDurationHistogram(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-metrics-hist",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "cm",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "metrics-hist-cm"},
							"data":       map[string]any{"key": "value"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-metrics-hist", Namespace: ns}))

	// Reconcile duration histogram should have at least 1 observation.
	// Poll because the deferred Observe() can race with the status write
	// that makes the graph Ready.
	require.NoError(t, waitForHistogramCount(ctx, t,
		"graph_reconcile_duration_seconds",
		graphLabels("test-metrics-hist", ns),
		1,
	), "reconcile duration histogram should have at least one observation")
}

// TestMetricsNodeEvalDurationHistogram proves that graph_node_eval_duration_seconds
// histogram is emitted per-node with the correct labels.
//
// Replaces: TestNodeEvalDurationHistogramRegistered (unit)
func TestMetricsNodeEvalDurationHistogram(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-metrics-nodeeval",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "cm",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "metrics-nodeeval-cm"},
							"data":       map[string]any{"key": "value"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-metrics-nodeeval", Namespace: ns}))

	// Per-node eval duration should have at least 1 observation.
	// Poll because the deferred Observe() can race with the status write
	// that makes the graph Ready.
	require.NoError(t, waitForHistogramCount(ctx, t,
		"graph_node_eval_duration_seconds",
		nodeLabels("test-metrics-nodeeval", ns, "cm"),
		1,
	), "node eval duration histogram should have at least one observation for node 'cm'")
}

// TestMetricsCleanupOnGraphDelete proves that Prometheus metrics for a Graph
// are cleaned up when the Graph is deleted. Without cleanup, deleted Graphs
// leave orphaned time series that inflate cardinality and confuse dashboards.
//
// Replaces: TestDeleteGraphMetricsForGraph, TestDeleteGraphMetricsForGraphIsolation (unit)
func TestMetricsCleanupOnGraphDelete(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-metrics-cleanup",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "cm",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "metrics-cleanup-cm"},
							"data":       map[string]any{"key": "value"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-metrics-cleanup", Namespace: ns}))

	// Verify metrics exist before deletion.
	require.NoError(t, waitForMetric(ctx, t,
		"graph_node_state",
		nodeStateLabels("test-metrics-cleanup", ns, "cm", "Ready"),
		1,
	))

	// Delete the Graph and wait for teardown to complete.
	require.NoError(t, k8sClient.Delete(ctx, graph))
	require.NoError(t, waitForDeletion(ctx, k8sClient, GraphGVK,
		types.NamespacedName{Name: "test-metrics-cleanup", Namespace: ns}))

	// After deletion, the node state gauge should be absent (deleted by
	// deleteGraphMetricsForGraph). We check that the Ready gauge is gone —
	// if it's still 1, cleanup failed.
	val, ok := scrapeGauge(t,
		"graph_node_state",
		nodeStateLabels("test-metrics-cleanup", ns, "cm", "Ready"),
	)
	// Either the metric is absent (ok=false) or present with value 0.
	if ok {
		assert.Equal(t, float64(0), val,
			"node state gauge should be cleaned up after Graph deletion")
	}
}

// TestMetricsSystemErrorRetries proves that graph_system_error_retries_total
// counter increments when the controller retries a node that previously
// encountered a transient server error (5xx). Uses the same webhook fault
// injection mechanism as TestSystemError_WebhookFaultAndRecovery.
//
// Replaces: TestSystemErrorRetriesIncrement, TestResyncTimerFiresIncrement (unit)
func TestMetricsSystemErrorRetries(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Label the namespace so the webhook selector matches.
	faultLabel := "fault-" + ns
	labelNamespace(t, ns, map[string]string{"fault-inject": faultLabel})

	// Start webhook server and register it.
	fw := startFaultWebhook(t)
	registerFaultWebhook(t, k8sClient, "fault-metrics-"+ns, fw, "fault-inject", faultLabel)

	// Inject fault for the ConfigMap this Graph will create.
	fw.Reject("metrics-syserr-cm")

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-metrics-syserr",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "cm",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "metrics-syserr-cm"},
							"data":       map[string]any{"key": "value"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for the Graph to enter SystemError state.
	require.NoError(t, waitForGraphReadyReason(ctx, k8sClient,
		types.NamespacedName{Name: "test-metrics-syserr", Namespace: ns}, "SystemError"))

	// The system_error_retries_total counter increments on the RETRY
	// reconcile (when previousPlanStates has SystemError), not the
	// reconcile that first produces the error. Poll until the retry fires.
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true,
		func(ctx context.Context) (bool, error) {
			val, ok := scrapeCounter(t,
				"graph_system_error_retries_total",
				nodeLabels("test-metrics-syserr", ns, "cm"),
			)
			return ok && val > 0, nil
		}), "system error retries counter should be > 0 after SystemError retry")

	// Remove the fault and verify recovery.
	fw.Accept("metrics-syserr-cm")
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-metrics-syserr", Namespace: ns}))

	// After recovery, the node state should be Ready.
	require.NoError(t, waitForMetric(ctx, t,
		"graph_node_state",
		nodeStateLabels("test-metrics-syserr", ns, "cm", "Ready"),
		1,
	))
}

// TestMetricsPerNodeIsolation proves that different node_id values within
// the same Graph produce independent time series — the core cardinality
// contract.
//
// Replaces: TestMetricLabelsPerNode (unit)
func TestMetricsPerNodeIsolation(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-metrics-isolation",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "alpha",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "metrics-iso-alpha"},
							"data":       map[string]any{"key": "a"},
						},
					},
					map[string]any{
						"id": "beta",
						"template": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "metrics-iso-beta"},
							"data":       map[string]any{"key": "b"},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-metrics-isolation", Namespace: ns}))

	// Each node should have its own independent gauge series.
	alphaReady, ok := scrapeGauge(t,
		"graph_node_state",
		nodeStateLabels("test-metrics-isolation", ns, "alpha", "Ready"),
	)
	require.True(t, ok, "alpha node state gauge should exist")
	assert.Equal(t, float64(1), alphaReady)

	betaReady, ok := scrapeGauge(t,
		"graph_node_state",
		nodeStateLabels("test-metrics-isolation", ns, "beta", "Ready"),
	)
	require.True(t, ok, "beta node state gauge should exist")
	assert.Equal(t, float64(1), betaReady)

	// Node "alpha" metrics should not appear under node_id="beta" and vice versa.
	// This is inherent in the label matching, but worth documenting.
	_, ok = scrapeGauge(t,
		"graph_node_state",
		nodeStateLabels("test-metrics-isolation", ns, "alpha", "Error"),
	)
	// If present, should be 0.
	if ok {
		val, _ := scrapeGauge(t, "graph_node_state",
			nodeStateLabels("test-metrics-isolation", ns, "alpha", "Error"))
		assert.Equal(t, float64(0), val)
	}
}

// TestMetricsNodeStateTransition proves that graph_node_state gauge transitions
// from one state to another as the Graph's reconcile state changes.
// Uses a readyWhen that starts unsatisfied (NotReady) then transitions to Ready.
func TestMetricsNodeStateTransition(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Create source ConfigMap with ready=false.
	source := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "metrics-transition-src",
				"namespace": ns,
			},
			"data": map[string]any{"ready": "false"},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, source))

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-metrics-transition",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "src",
						"ref": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "metrics-transition-src"},
						},
						"readyWhen": []any{"${src.data.ready == 'true'}"},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	// Wait for NotReady state in the gauge.
	require.NoError(t, waitForMetric(ctx, t,
		"graph_node_state",
		nodeStateLabels("test-metrics-transition", ns, "src", "NotReady"),
		1,
	))

	// Ready should be 0 while NotReady is 1.
	readyVal, ok := scrapeGauge(t,
		"graph_node_state",
		nodeStateLabels("test-metrics-transition", ns, "src", "Ready"),
	)
	if ok {
		assert.Equal(t, float64(0), readyVal, "Ready should be 0 when NotReady is 1")
	}

	// Transition source to ready.
	require.NoError(t, updateWithRetry(ctx, k8sClient,
		schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"},
		types.NamespacedName{Name: "metrics-transition-src", Namespace: ns},
		func(obj *unstructured.Unstructured) {
			unstructured.SetNestedField(obj.Object, "true", "data", "ready")
		},
	))

	// Wait for Ready=1 transition.
	require.NoError(t, waitForMetric(ctx, t,
		"graph_node_state",
		nodeStateLabels("test-metrics-transition", ns, "src", "Ready"),
		1,
	))

	// NotReady should now be 0.
	notReadyVal, ok := scrapeGauge(t,
		"graph_node_state",
		nodeStateLabels("test-metrics-transition", ns, "src", "NotReady"),
	)
	if ok {
		assert.Equal(t, float64(0), notReadyVal, "NotReady should be 0 after transition to Ready")
	}
}
