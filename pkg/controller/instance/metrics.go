// Copyright 2025 The Kube Resource Orchestrator Authors
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

package instance

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

const (
	// MetricImpersonationTotal is the total number of impersonation requests
	// made by the controller
	MetricImpersonationTotal = "controller_impersonation_total"
	// MetricImpersonationErrors is the total number of errors encountered
	// while making impersonation requests
	MetricImpersonationErrors = "controller_impersonation_errors_total"
	// MetricImpersonationDuration tracks the duration of impersonation operations
	MetricImpersonationDuration = "controller_impersonation_duration_seconds"
)

var (
	impersonationTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: MetricImpersonationTotal,
			Help: "Total number of service account impersonation attempts by namespace and result",
		},
		[]string{"namespace", "service_account", "result"},
	)

	impersonationErrors = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: MetricImpersonationErrors,
			Help: "Total number of service account impersonation errors by category",
		},
		[]string{"namespace", "service_account", "error_type"},
	)

	impersonationDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    MetricImpersonationDuration,
			Help:    "Duration of service account impersonation operations",
			Buckets: []float64{0.01, 0.1, 0.5, 1, 2, 5},
		},
		[]string{"namespace", "service_account"},
	)
)

func recordImpersonateError(namespace, sa string, category errorCategory) {
	impersonationErrors.WithLabelValues(namespace, sa, string(category)).Inc()
}

func init() {
	metrics.Registry.MustRegister(
		impersonationTotal,
		impersonationErrors,
		impersonationDuration,
	)
}
