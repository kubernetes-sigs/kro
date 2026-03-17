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

package instance

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

func init() {
	metrics.Registry.MustRegister(
		instanceStateTransitionsTotal,
		instanceReconcileDurationSeconds,
		instanceReconcileTotal,
		instanceReconcileErrorsTotal,
	)
}

var (
	instanceStateTransitionsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "instance_state_transitions_total",
			Help: "Total number of instance state transitions per GVR",
		},
		[]string{"gvr", "from_state", "to_state"},
	)

	instanceReconcileDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "instance_reconcile_duration_seconds",
			Help:    "Duration of instance reconciliation in seconds per GVR",
			Buckets: []float64{.01, .05, .1, .25, .5, 1, 2.5, 5, 10, 15, 20, 25, 30, 45, 60, 120},
		},
		[]string{"gvr"},
	)

	instanceReconcileTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "instance_reconcile_total",
			Help: "Total number of instance reconciliations per GVR",
		},
		[]string{"gvr"},
	)

	instanceReconcileErrorsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "instance_reconcile_errors_total",
			Help: "Total number of instance reconciliation errors per GVR",
		},
		[]string{"gvr"},
	)
)
