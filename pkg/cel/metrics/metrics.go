package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/legacyregistry"
)

var (
	// CELCompilationDuration is a Histogram that tracks the latency of CEL compilations in seconds.
	CELCompilationDuration = metrics.NewHistogram(
		&metrics.HistogramOpts{
			Name:           "cel_compilation_duration_seconds",
			Help:           "CEL compilation duration in seconds",
			Buckets:        metrics.ExponentialBuckets(1e-6, 2, 16), // 1us to ~65ms
			StabilityLevel: metrics.ALPHA,
		},
	)

	// CELCompilationResult is a Counter that tracks the result of CEL compilations.
	CELCompilationResult = metrics.NewCounterVec(
		&metrics.CounterOpts{
			Name:           "cel_compilation_result_total",
			Help:           "Count of CEL compilations by result (success/failure)",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"result"},
	)

	// CELEvaluationDuration is a Histogram that tracks the latency of CEL evaluations in seconds.
	CELEvaluationDuration = metrics.NewHistogram(
		&metrics.HistogramOpts{
			Name:           "cel_evaluation_duration_seconds",
			Help:           "CEL evaluation duration in seconds",
			Buckets:        metrics.ExponentialBuckets(1e-6, 2, 16), // 1us to ~65ms
			StabilityLevel: metrics.ALPHA,
		},
	)

	// CELEvaluationResult is a Counter that tracks the result of CEL evaluations.
	CELEvaluationResult = metrics.NewCounterVec(
		&metrics.CounterOpts{
			Name:           "cel_evaluation_result_total",
			Help:           "Count of CEL evaluations by result (success/failure)",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"result"},
	)
)

func init() {
	legacyregistry.MustRegister(CELCompilationDuration)
	legacyregistry.MustRegister(CELCompilationResult)
	legacyregistry.MustRegister(CELEvaluationDuration)
	legacyregistry.MustRegister(CELEvaluationResult)
}

// RecordCELCompilationLatency records the latency of a CEL compilation.
func RecordCELCompilationLatency(elapsed float64) {
	CELCompilationDuration.Observe(elapsed)
}

// RecordCELCompilationSuccess increments the CEL compilation success counter.
func RecordCELCompilationSuccess() {
	CELCompilationResult.WithLabelValues("success").Inc()
}

// RecordCELCompilationFailure increments the CEL compilation failure counter.
func RecordCELCompilationFailure() {
	CELCompilationResult.WithLabelValues("failure").Inc()
}

// RecordCELEvaluationLatency records the latency of a CEL evaluation.
func RecordCELEvaluationLatency(elapsed float64) {
	CELEvaluationDuration.Observe(elapsed)
}

// RecordCELEvaluationSuccess increments the CEL evaluation success counter.
func RecordCELEvaluationSuccess() {
	CELEvaluationResult.WithLabelValues("success").Inc()
}

// RecordCELEvaluationFailure increments the CEL evaluation failure counter.
func RecordCELEvaluationFailure() {
	CELEvaluationResult.WithLabelValues("failure").Inc()
} 