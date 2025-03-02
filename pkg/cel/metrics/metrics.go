package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/legacyregistry"
)

// Common label names for CEL metrics
const (
	// ResourceTypeLabel identifies the type of resource (ResourceGroup, ResourceInstance, etc.)
	ResourceTypeLabel = "resource_type"
	// OperationTypeLabel identifies the operation (validation, transformation, etc.)
	OperationTypeLabel = "operation_type"
	// ResultLabel identifies the result of the operation (success, failure)
	ResultLabel = "result"
	// ExpressionTypeLabel identifies the type or purpose of the expression
	ExpressionTypeLabel = "expression_type"
)

var (
	// CELCompilationDuration is a Histogram that tracks the latency of CEL compilations in seconds.
	CELCompilationDuration = metrics.NewHistogramVec(
		&metrics.HistogramOpts{
			Name:           "cel_compilation_duration_seconds",
			Help:           "CEL compilation duration in seconds",
			Buckets:        metrics.ExponentialBuckets(1e-6, 2, 16), // 1us to ~65ms
			StabilityLevel: metrics.ALPHA,
		},
		[]string{ResourceTypeLabel, ExpressionTypeLabel},
	)

	// CELCompilationResult is a Counter that tracks the result of CEL compilations.
	CELCompilationResult = metrics.NewCounterVec(
		&metrics.CounterOpts{
			Name:           "cel_compilation_result_total",
			Help:           "Count of CEL compilations by result (success/failure)",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{ResultLabel, ResourceTypeLabel, ExpressionTypeLabel},
	)

	// CELEvaluationDuration is a Histogram that tracks the latency of CEL evaluations in seconds.
	CELEvaluationDuration = metrics.NewHistogramVec(
		&metrics.HistogramOpts{
			Name:           "cel_evaluation_duration_seconds",
			Help:           "CEL evaluation duration in seconds",
			Buckets:        metrics.ExponentialBuckets(1e-6, 2, 16), // 1us to ~65ms
			StabilityLevel: metrics.ALPHA,
		},
		[]string{ResourceTypeLabel, OperationTypeLabel, ExpressionTypeLabel},
	)

	// CELEvaluationResult is a Counter that tracks the result of CEL evaluations.
	CELEvaluationResult = metrics.NewCounterVec(
		&metrics.CounterOpts{
			Name:           "cel_evaluation_result_total",
			Help:           "Count of CEL evaluations by result (success/failure)",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{ResultLabel, ResourceTypeLabel, OperationTypeLabel, ExpressionTypeLabel},
	)

	// CELEvaluationErrors tracks specific error types during CEL evaluation
	CELEvaluationErrors = metrics.NewCounterVec(
		&metrics.CounterOpts{
			Name:           "cel_evaluation_errors_total",
			Help:           "Count of CEL evaluation errors by error type",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"error_type", ResourceTypeLabel, OperationTypeLabel},
	)

	// CELActiveEvaluations is a gauge that tracks the number of currently active CEL evaluations
	CELActiveEvaluations = metrics.NewGaugeVec(
		&metrics.GaugeOpts{
			Name:           "cel_active_evaluations",
			Help:           "Number of currently active CEL evaluations",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{ResourceTypeLabel, OperationTypeLabel},
	)
)

func init() {
	legacyregistry.MustRegister(CELCompilationDuration)
	legacyregistry.MustRegister(CELCompilationResult)
	legacyregistry.MustRegister(CELEvaluationDuration)
	legacyregistry.MustRegister(CELEvaluationResult)
	legacyregistry.MustRegister(CELEvaluationErrors)
	legacyregistry.MustRegister(CELActiveEvaluations)
}

// RecordCELCompilationLatency records the latency of a CEL compilation.
func RecordCELCompilationLatency(resourceType, expressionType string, elapsed float64) {
	CELCompilationDuration.WithLabelValues(resourceType, expressionType).Observe(elapsed)
}

// RecordCELCompilationSuccess increments the CEL compilation success counter.
func RecordCELCompilationSuccess(resourceType, expressionType string) {
	CELCompilationResult.WithLabelValues("success", resourceType, expressionType).Inc()
}

// RecordCELCompilationFailure increments the CEL compilation failure counter.
func RecordCELCompilationFailure(resourceType, expressionType string) {
	CELCompilationResult.WithLabelValues("failure", resourceType, expressionType).Inc()
}

// RecordCELEvaluationLatency records the latency of a CEL evaluation.
func RecordCELEvaluationLatency(resourceType, operationType, expressionType string, elapsed float64) {
	CELEvaluationDuration.WithLabelValues(resourceType, operationType, expressionType).Observe(elapsed)
}

// RecordCELEvaluationSuccess increments the CEL evaluation success counter.
func RecordCELEvaluationSuccess(resourceType, operationType, expressionType string) {
	CELEvaluationResult.WithLabelValues("success", resourceType, operationType, expressionType).Inc()
}

// RecordCELEvaluationFailure increments the CEL evaluation failure counter.
func RecordCELEvaluationFailure(resourceType, operationType, expressionType string) {
	CELEvaluationResult.WithLabelValues("failure", resourceType, operationType, expressionType).Inc()
}

// RecordCELEvaluationError records a specific type of CEL evaluation error.
func RecordCELEvaluationError(errorType, resourceType, operationType string) {
	CELEvaluationErrors.WithLabelValues(errorType, resourceType, operationType).Inc()
}

// IncrementActiveCELEvaluations increments the gauge of active CEL evaluations.
func IncrementActiveCELEvaluations(resourceType, operationType string) {
	CELActiveEvaluations.WithLabelValues(resourceType, operationType).Inc()
}

// DecrementActiveCELEvaluations decrements the gauge of active CEL evaluations.
func DecrementActiveCELEvaluations(resourceType, operationType string) {
	CELActiveEvaluations.WithLabelValues(resourceType, operationType).Dec()
}

// WithCELEvaluation executes the given function and records metrics about its execution.
// This is a helper to ensure that all relevant metrics are recorded, including active evaluations.
func WithCELEvaluation(resourceType, operationType, expressionType string, fn func() error) error {
	IncrementActiveCELEvaluations(resourceType, operationType)
	defer DecrementActiveCELEvaluations(resourceType, operationType)

	start := prometheus.NewTimer(CELEvaluationDuration.WithLabelValues(resourceType, operationType, expressionType))
	defer start.ObserveDuration()

	err := fn()
	if err != nil {
		RecordCELEvaluationFailure(resourceType, operationType, expressionType)
		// You could add logic here to categorize error types
		RecordCELEvaluationError("execution_error", resourceType, operationType)
		return err
	}

	RecordCELEvaluationSuccess(resourceType, operationType, expressionType)
	return nil
} 