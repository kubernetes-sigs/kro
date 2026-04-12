package graphcontroller

import "github.com/prometheus/client_golang/prometheus"

// Graph controller metrics. Registered via RegisterMetrics; safe to call
// .Inc() / .With() even when the metrics endpoint is disabled — the
// endpoint flag controls serving, not collector existence.
//
// TODO: series for graphs in a deleted namespace persist until process
// restart. A namespace-deletion watch or controller shutdown hook would
// prevent this slow leak in long-lived controllers.

var (
	// DriftTimerFiresTotal counts drift timer expirations that trigger an
	// unconditional apply. Incremented in the trigger determination block
	// when a per-node drift timer expires and bypasses the template-hash
	// check. Per 004-graph-execution.md: "the drift timer bypasses the
	// template-hash check — apply unconditionally."
	DriftTimerFiresTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "graph_drift_timer_fires_total",
			Help: "Total number of drift timer expirations that triggered unconditional apply",
		},
		[]string{"graph_name", "graph_namespace", "node_id"},
	)

	// SystemErrorRetriesTotal counts node re-evaluations caused by a
	// previous SystemError state. Server/infrastructure failures (5xx,
	// timeout, network) leave nodes in SystemError; the next reconcile
	// retries them unconditionally. This counter tracks that retry
	// volume for operator triage.
	SystemErrorRetriesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "graph_system_error_retries_total",
			Help: "Total number of node re-evaluations triggered by previous SystemError state",
		},
		[]string{"graph_name", "graph_namespace", "node_id"},
	)
)

// RegisterMetrics registers graph controller metrics with the given
// Prometheus registerer. Idempotent — duplicate registration against the
// same registry is silently ignored. Panics only on non-duplicate errors
// (e.g., metric name collision with different configuration).
func RegisterMetrics(registry prometheus.Registerer) {
	for _, c := range []prometheus.Collector{
		DriftTimerFiresTotal,
		SystemErrorRetriesTotal,
	} {
		if err := registry.Register(c); err != nil {
			if _, ok := err.(prometheus.AlreadyRegisteredError); !ok {
				panic(err)
			}
		}
	}
}

// graphMetricLabels returns the standard label set for graph-scoped metrics.
func graphMetricLabels(graphName, graphNamespace, nodeID string) prometheus.Labels {
	return prometheus.Labels{
		"graph_name":      graphName,
		"graph_namespace": graphNamespace,
		"node_id":         nodeID,
	}
}

// deleteGraphMetricsForGraph removes all time series for a graph using
// partial match on graph_name and graph_namespace. Used during graph
// deletion where individual node IDs may not be fully recoverable from
// revision specs.
func deleteGraphMetricsForGraph(graphName, graphNamespace string) {
	labels := prometheus.Labels{
		"graph_name":      graphName,
		"graph_namespace": graphNamespace,
	}
	DriftTimerFiresTotal.DeletePartialMatch(labels)
	SystemErrorRetriesTotal.DeletePartialMatch(labels)
}

// deleteNodeMetrics removes time series for specific nodes within a graph.
// Used during revision transitions where the active vs superseded node
// sets are diffed to find removed nodes.
func deleteNodeMetrics(graphName, graphNamespace string, nodeIDs map[string]bool) {
	for nodeID := range nodeIDs {
		labels := graphMetricLabels(graphName, graphNamespace, nodeID)
		DriftTimerFiresTotal.Delete(labels)
		SystemErrorRetriesTotal.Delete(labels)
	}
}
