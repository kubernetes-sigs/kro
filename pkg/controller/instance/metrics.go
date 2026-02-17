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
		instanceApplyDurationSeconds,
		instanceResourcesAppliedTotal,
		instanceResourcesPrunedTotal,
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

	instanceApplyDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "instance_apply_duration_seconds",
			Help:    "Duration of apply operations in seconds",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"gvr"},
	)

	instanceResourcesAppliedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "instance_resources_applied_total",
			Help: "Total number of resources applied per GVR",
		},
		[]string{"gvr"},
	)

	instanceResourcesPrunedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "instance_resources_pruned_total",
			Help: "Total number of resources pruned per GVR",
		},
		[]string{"gvr"},
	)

	instanceReconcileDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "instance_reconcile_duration_seconds",
			Help:    "Duration of instance reconciliation in seconds per GVR",
			Buckets: prometheus.DefBuckets,
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
