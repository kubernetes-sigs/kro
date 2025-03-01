package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Namespace & Subsystem for Prometheus Metrics
const (
	namespace = "kro"
	subsystem = "cel"
)

// CelMetrics struct contains all CEL-related Prometheus metrics
type CelMetrics struct {
	compilationTime prometheus.Histogram
	evaluationTime  prometheus.Histogram
}

// NewCelMetrics initializes CEL Prometheus metrics
func NewCelMetrics() *CelMetrics {
	m := &CelMetrics{
		compilationTime: promauto.NewHistogram(
			prometheus.HistogramOpts{
				Namespace: namespace,
				Subsystem: subsystem,
				Name:      "compilation_duration_seconds", 
				Help:      "The duration of CEL compilation in seconds",
				Buckets:   prometheus.ExponentialBuckets(0.001, 2, 15), 
			},
		),
		evaluationTime: promauto.NewHistogram(
			prometheus.HistogramOpts{
				Namespace: namespace,
				Subsystem: subsystem,
				Name:      "evaluation_duration_seconds",
				Help:      "The duration of CEL evaluation in seconds",
				Buckets:   prometheus.ExponentialBuckets(0.001, 2, 15),
			},
		),
	}


	return m
}

// ObserveCompilationTime records the duration of CEL compilation
func (m *CelMetrics) ObserveCompilationTime(d time.Duration) {
	m.compilationTime.Observe(d.Seconds()) 
}

// ObserveEvaluationTime records the duration of CEL evaluation
func (m *CelMetrics) ObserveEvaluationTime(d time.Duration) {
	m.evaluationTime.Observe(d.Seconds())
}
