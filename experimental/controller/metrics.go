package graphcontroller

import "github.com/prometheus/client_golang/prometheus"

// Graph controller metrics. Registered via RegisterMetrics; safe to call
// .Inc() / .With() even when the metrics endpoint is disabled — the
// endpoint flag controls serving, not collector existence.
//
// Namespace deletion does not leak metric series. The namespace lifecycle
// controller sets deletionTimestamp on all resources in the namespace; the
// Graph's finalizer holds the object until reconcileDelete runs and calls
// deleteGraphMetricsForGraph. On controller restart the registry is fresh.

var (
	// DriftTimerFiresTotal counts drift timer expirations that trigger an
	// unconditional apply. Incremented in the trigger determination block
	// when a per-node drift timer expires and bypasses the apply-hash
	// check. Per 004-graph-reconciliation.md: "the drift timer bypasses the
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

	// ReconcileDurationSeconds measures the wall-clock time of each
	// reconcile cycle. This is the operator's first metric for triage:
	// "how long are reconciles taking?" Labeled by graph identity and
	// outcome (success, error) so latency can be correlated with
	// specific graphs and failure modes.
	ReconcileDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "graph_reconcile_duration_seconds",
			Help:    "Duration of Graph reconcile cycles in seconds",
			Buckets: prometheus.ExponentialBuckets(0.01, 2, 12), // 10ms to ~40s
		},
		[]string{"graph_name", "graph_namespace"},
	)

	// NodeEvalDurationSeconds measures per-node evaluation time — from
	// worker dispatch to result receipt. Identifies slow nodes that
	// dominate reconcile latency (e.g., large SSA patches, slow API
	// server responses for specific GVKs).
	NodeEvalDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "graph_node_eval_duration_seconds",
			Help:    "Duration of per-node evaluation in seconds (dispatch to result)",
			Buckets: prometheus.ExponentialBuckets(0.001, 2, 12), // 1ms to ~4s
		},
		[]string{"graph_name", "graph_namespace", "node_id"},
	)

	// NodeStateGauge reports the current state of each node in a Graph.
	// Each node has exactly one state at any given time. The gauge value
	// is 1 for the node's current state label and 0 for all others.
	// Enables "how many nodes are in Error right now?" without querying
	// the Graph status API object.
	NodeStateGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "graph_node_state",
			Help: "Current state of each node (1 = active, 0 = inactive). State label is one of: Ready, NotReady, Pending, Excluded, Blocked, Error, Conflict, SystemError",
		},
		[]string{"graph_name", "graph_namespace", "node_id", "state"},
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
		ReconcileDurationSeconds,
		NodeEvalDurationSeconds,
		NodeStateGauge,
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
	ReconcileDurationSeconds.DeletePartialMatch(labels)
	NodeEvalDurationSeconds.DeletePartialMatch(labels)
	NodeStateGauge.DeletePartialMatch(labels)
}

// deleteNodeMetrics removes time series for specific nodes within a graph.
// Used during revision transitions where the active vs superseded node
// sets are diffed to find removed nodes.
func deleteNodeMetrics(graphName, graphNamespace string, nodeIDs map[string]bool) {
	for nodeID := range nodeIDs {
		labels := graphMetricLabels(graphName, graphNamespace, nodeID)
		DriftTimerFiresTotal.Delete(labels)
		SystemErrorRetriesTotal.Delete(labels)
		NodeEvalDurationSeconds.Delete(labels)
		// Delete all state label variants for the node state gauge.
		for _, state := range nodeStateLabels {
			NodeStateGauge.Delete(prometheus.Labels{
				"graph_name":      graphName,
				"graph_namespace": graphNamespace,
				"node_id":         nodeID,
				"state":           state,
			})
		}
	}
}

// nodeStateLabels is the complete set of state labels for the node state
// gauge. Kept in sync with NodeState constants in dag.go.
var nodeStateLabels = []string{
	"Ready", "NotReady", "Pending", "Excluded", "Blocked",
	"Error", "Conflict", "SystemError",
}

// updateNodeStateMetrics sets the node state gauge for all nodes in a graph.
// Each node gets value 1 for its current state and 0 for all other states.
func updateNodeStateMetrics(graphName, graphNamespace string, plan *PlanState, dag *DAG) {
	for _, node := range dag.Nodes {
		currentState := plan.States[node.ID].String()
		// Skip unvisited nodes — they haven't been processed by the walk.
		if plan.States[node.ID] == nodeUnvisited {
			continue
		}
		for _, state := range nodeStateLabels {
			val := float64(0)
			if state == currentState {
				val = 1
			}
			NodeStateGauge.With(prometheus.Labels{
				"graph_name":      graphName,
				"graph_namespace": graphNamespace,
				"node_id":         node.ID,
				"state":           state,
			}).Set(val)
		}
	}
}
