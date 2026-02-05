// Copyright 2025 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cel

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

const (
	namespace = "kro"
	subsystem = "cel"
)

// Metrics provides access to CEL metrics.
var Metrics = newCelMetrics()

// CelMetrics holds prometheus metrics for CEL compilation and evaluation.
type CelMetrics struct {
	compilationTime *prometheus.HistogramVec
	evaluationTime  *prometheus.HistogramVec
}

func newCelMetrics() *CelMetrics {
	m := &CelMetrics{
		compilationTime: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace: namespace,
				Subsystem: subsystem,
				Name:      "compilation_duration_seconds",
				Help:      "CEL compilation time in seconds.",
				Buckets:   prometheus.ExponentialBuckets(0.00001, 2, 10), // 10µs to ~10ms
			},
			[]string{"result"}, // "success" or "error"
		),
		evaluationTime: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace: namespace,
				Subsystem: subsystem,
				Name:      "evaluation_duration_seconds",
				Help:      "CEL evaluation time in seconds.",
				Buckets:   prometheus.ExponentialBuckets(0.00001, 2, 10), // 10µs to ~10ms
			},
			[]string{"result"}, // "success" or "error"
		),
	}

	return m
}

// ObserveCompilation records a CEL compilation duration.
func (m *CelMetrics) ObserveCompilation(durationSeconds float64, err error) {
	result := "success"
	if err != nil {
		result = "error"
	}
	m.compilationTime.WithLabelValues(result).Observe(durationSeconds)
}

// ObserveEvaluation records a CEL evaluation duration.
func (m *CelMetrics) ObserveEvaluation(durationSeconds float64, err error) {
	result := "success"
	if err != nil {
		result = "error"
	}
	m.evaluationTime.WithLabelValues(result).Observe(durationSeconds)
}

// MustRegister registers the metrics with the given Prometheus registry.
func (m *CelMetrics) MustRegister(registry prometheus.Registerer) {
	registry.MustRegister(m.compilationTime)
	registry.MustRegister(m.evaluationTime)
}

func init() {
	Metrics.MustRegister(metrics.Registry)
}
