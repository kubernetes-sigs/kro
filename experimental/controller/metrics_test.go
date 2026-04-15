package graphcontroller

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Unit tests — metrics registration, increment, and cleanup
// ---------------------------------------------------------------------------

// TestRegisterMetrics verifies that RegisterMetrics registers all expected
// collectors without panicking on a fresh registry.
func TestRegisterMetrics(t *testing.T) {
	t.Cleanup(func() {
		DriftTimerFiresTotal.Reset()
		SystemErrorRetriesTotal.Reset()
	})

	reg := prometheus.NewRegistry()
	RegisterMetrics(reg)

	// Increment both counters so Gather() returns them.
	DriftTimerFiresTotal.With(prometheus.Labels{
		"graph_name":      "test-graph",
		"graph_namespace": "test-ns",
		"node_id":         "node-a",
	}).Inc()
	SystemErrorRetriesTotal.With(prometheus.Labels{
		"graph_name":      "test-graph",
		"graph_namespace": "test-ns",
		"node_id":         "node-a",
	}).Inc()

	families, err := reg.Gather()
	require.NoError(t, err)

	names := map[string]bool{}
	for _, f := range families {
		names[f.GetName()] = true
	}
	assert.True(t, names["graph_drift_timer_fires_total"],
		"expected graph_drift_timer_fires_total in gathered families")
	assert.True(t, names["graph_system_error_retries_total"],
		"expected graph_system_error_retries_total in gathered families")
}

// TestRegisterMetricsIdempotent verifies that calling RegisterMetrics
// multiple times against the same registry does not panic — duplicate
// registration is silently ignored.
func TestRegisterMetricsIdempotent(t *testing.T) {
	reg := prometheus.NewRegistry()
	RegisterMetrics(reg)
	// Second call must not panic.
	assert.NotPanics(t, func() {
		RegisterMetrics(reg)
	})
}

// TestRegisterMetricsMultipleRegistries verifies that RegisterMetrics
// works with different registries (e.g., test isolation).
func TestRegisterMetricsMultipleRegistries(t *testing.T) {
	t.Cleanup(func() {
		DriftTimerFiresTotal.Reset()
		SystemErrorRetriesTotal.Reset()
	})

	reg1 := prometheus.NewRegistry()
	reg2 := prometheus.NewRegistry()

	RegisterMetrics(reg1)
	// Registering against a second registry will get AlreadyRegisteredError
	// since the collectors are package-level vars already registered with reg1.
	// This should not panic.
	assert.NotPanics(t, func() {
		RegisterMetrics(reg2)
	})
}

// TestSystemErrorRetriesIncrement verifies that the counter increments with
// correct label values. This tests the graphMetricLabels helper and the
// basic contract that .Inc() produces an observable value of 1.
func TestSystemErrorRetriesIncrement(t *testing.T) {
	t.Cleanup(func() {
		SystemErrorRetriesTotal.Reset()
	})

	labels := graphMetricLabels("my-graph", "my-ns", "deploy")
	counter := SystemErrorRetriesTotal.With(labels)

	assert.Equal(t, float64(0), testutil.ToFloat64(counter),
		"counter should start at zero")

	counter.Inc()
	assert.Equal(t, float64(1), testutil.ToFloat64(counter))

	counter.Inc()
	counter.Inc()
	assert.Equal(t, float64(3), testutil.ToFloat64(counter))
}

// TestDriftTimerFiresIncrement verifies the drift timer counter increments
// correctly. Structurally identical to SystemError — both are CounterVecs
// with the same label set — but tested independently to catch copy-paste
// registration bugs (wrong var wired to wrong metric name).
func TestDriftTimerFiresIncrement(t *testing.T) {
	t.Cleanup(func() {
		DriftTimerFiresTotal.Reset()
	})

	labels := graphMetricLabels("app-graph", "production", "ingress")
	counter := DriftTimerFiresTotal.With(labels)

	assert.Equal(t, float64(0), testutil.ToFloat64(counter))

	counter.Inc()
	assert.Equal(t, float64(1), testutil.ToFloat64(counter))
}

// TestMetricLabelsPerNode verifies that different node_id values within the
// same graph produce independent time series. This is the core cardinality
// contract: per-node granularity.
func TestMetricLabelsPerNode(t *testing.T) {
	t.Cleanup(func() {
		SystemErrorRetriesTotal.Reset()
	})

	nodeA := SystemErrorRetriesTotal.With(graphMetricLabels("g", "ns", "node-a"))
	nodeB := SystemErrorRetriesTotal.With(graphMetricLabels("g", "ns", "node-b"))

	nodeA.Inc()
	nodeA.Inc()
	nodeB.Inc()

	assert.Equal(t, float64(2), testutil.ToFloat64(nodeA),
		"node-a should have independent count")
	assert.Equal(t, float64(1), testutil.ToFloat64(nodeB),
		"node-b should have independent count")
}

// TestDeleteNodeMetrics verifies that deleteNodeMetrics removes time series
// for the specified nodes. After deletion, the label set should be absent
// — testutil.ToFloat64 on a freshly-created With() returns 0 (new series).
func TestDeleteNodeMetrics(t *testing.T) {
	t.Cleanup(func() {
		DriftTimerFiresTotal.Reset()
		SystemErrorRetriesTotal.Reset()
	})

	// Seed metrics for two nodes.
	DriftTimerFiresTotal.With(graphMetricLabels("g", "ns", "node-a")).Inc()
	DriftTimerFiresTotal.With(graphMetricLabels("g", "ns", "node-b")).Inc()
	SystemErrorRetriesTotal.With(graphMetricLabels("g", "ns", "node-a")).Inc()
	SystemErrorRetriesTotal.With(graphMetricLabels("g", "ns", "node-b")).Inc()

	// Delete metrics for node-a only.
	deleteNodeMetrics("g", "ns", map[string]bool{"node-a": true})

	// node-a should be gone — a new With() creates a fresh zero series.
	assert.Equal(t, float64(0),
		testutil.ToFloat64(DriftTimerFiresTotal.With(graphMetricLabels("g", "ns", "node-a"))),
		"node-a drift timer should be deleted (reset to 0)")
	assert.Equal(t, float64(0),
		testutil.ToFloat64(SystemErrorRetriesTotal.With(graphMetricLabels("g", "ns", "node-a"))),
		"node-a system error should be deleted (reset to 0)")

	// node-b should be untouched.
	assert.Equal(t, float64(1),
		testutil.ToFloat64(DriftTimerFiresTotal.With(graphMetricLabels("g", "ns", "node-b"))),
		"node-b drift timer should be preserved")
	assert.Equal(t, float64(1),
		testutil.ToFloat64(SystemErrorRetriesTotal.With(graphMetricLabels("g", "ns", "node-b"))),
		"node-b system error should be preserved")
}

// TestDeleteGraphMetricsForGraph verifies that deleteGraphMetricsForGraph
// removes all time series for a graph via partial match, regardless of
// which node IDs exist.
func TestDeleteGraphMetricsForGraph(t *testing.T) {
	t.Cleanup(func() {
		DriftTimerFiresTotal.Reset()
		SystemErrorRetriesTotal.Reset()
	})

	nodes := []string{"node-a", "node-b", "node-c"}
	for _, id := range nodes {
		DriftTimerFiresTotal.With(graphMetricLabels("g", "ns", id)).Add(5)
		SystemErrorRetriesTotal.With(graphMetricLabels("g", "ns", id)).Add(3)
	}

	deleteGraphMetricsForGraph("g", "ns")

	for _, id := range nodes {
		assert.Equal(t, float64(0),
			testutil.ToFloat64(DriftTimerFiresTotal.With(graphMetricLabels("g", "ns", id))),
			"drift timer for %s should be deleted", id)
		assert.Equal(t, float64(0),
			testutil.ToFloat64(SystemErrorRetriesTotal.With(graphMetricLabels("g", "ns", id))),
			"system error for %s should be deleted", id)
	}
}

// TestDeleteGraphMetricsForGraphIsolation verifies that deleting metrics
// for one graph does not affect a different graph's metrics (same node IDs).
func TestDeleteGraphMetricsForGraphIsolation(t *testing.T) {
	t.Cleanup(func() {
		DriftTimerFiresTotal.Reset()
		SystemErrorRetriesTotal.Reset()
	})

	// Same node ID in two different graphs.
	DriftTimerFiresTotal.With(graphMetricLabels("graph-1", "ns", "shared-node")).Inc()
	DriftTimerFiresTotal.With(graphMetricLabels("graph-2", "ns", "shared-node")).Inc()

	// Delete graph-1 only.
	deleteGraphMetricsForGraph("graph-1", "ns")

	assert.Equal(t, float64(0),
		testutil.ToFloat64(DriftTimerFiresTotal.With(graphMetricLabels("graph-1", "ns", "shared-node"))),
		"graph-1 should be cleaned up")
	assert.Equal(t, float64(1),
		testutil.ToFloat64(DriftTimerFiresTotal.With(graphMetricLabels("graph-2", "ns", "shared-node"))),
		"graph-2 should be unaffected")
}

// TestGraphMetricLabelsHelper verifies the label helper produces the
// expected keys and values.
func TestGraphMetricLabelsHelper(t *testing.T) {
	labels := graphMetricLabels("my-graph", "my-ns", "my-node")
	assert.Equal(t, "my-graph", labels["graph_name"])
	assert.Equal(t, "my-ns", labels["graph_namespace"])
	assert.Equal(t, "my-node", labels["node_id"])
	assert.Len(t, labels, 3, "should have exactly 3 labels")
}

// TestReconcileDurationHistogramRegistered verifies that the reconcile
// duration histogram is registered and observable.
func TestReconcileDurationHistogramRegistered(t *testing.T) {
	t.Cleanup(func() {
		ReconcileDurationSeconds.Reset()
	})

	reg := prometheus.NewRegistry()
	RegisterMetrics(reg)

	ReconcileDurationSeconds.With(prometheus.Labels{
		"graph_name":      "test-graph",
		"graph_namespace": "test-ns",
	}).Observe(0.5)

	families, err := reg.Gather()
	require.NoError(t, err)

	names := map[string]bool{}
	for _, f := range families {
		names[f.GetName()] = true
	}
	assert.True(t, names["graph_reconcile_duration_seconds"],
		"expected graph_reconcile_duration_seconds in gathered families")
}

// TestNodeEvalDurationHistogramRegistered verifies that the per-node
// evaluation duration histogram is registered and observable.
func TestNodeEvalDurationHistogramRegistered(t *testing.T) {
	t.Cleanup(func() {
		NodeEvalDurationSeconds.Reset()
	})

	reg := prometheus.NewRegistry()
	RegisterMetrics(reg)

	NodeEvalDurationSeconds.With(graphMetricLabels("g", "ns", "deploy")).Observe(0.1)

	families, err := reg.Gather()
	require.NoError(t, err)

	names := map[string]bool{}
	for _, f := range families {
		names[f.GetName()] = true
	}
	assert.True(t, names["graph_node_eval_duration_seconds"],
		"expected graph_node_eval_duration_seconds in gathered families")
}
