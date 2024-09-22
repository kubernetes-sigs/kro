// Copyright Amazon.com Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//     http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package dynamiccontroller

import (
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/prometheus/client_golang/prometheus"
)

func init() {
	// Register metrics with the global prometheus registry
	metrics.Registry.MustRegister(
		reconcileTotal,
		requeueTotal,
		reconcileDuration,
		gvrCount,
		queueLength,
		handlerErrorsTotal,
		informerSyncDuration,
		informerEventsTotal,
		// activeWorkersTotal,
	)
}

var (
	// reconcileTotal is a counter that tracks the total number of reconciliations per GVR
	reconcileTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "dynamic_controller_reconcile_total",
			Help: "Total number of reconciliations per GVR",
		},
		[]string{"gvr"},
	)
	requeueTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "dynamic_controller_requeue_total",
			Help: "Total number of requeues per GVR",
		},
		[]string{"gvr", "type"},
	)
	// tracking the duration of reconciliations per GVR
	reconcileDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "dynamic_controller_reconcile_duration_seconds",
			Help:    "Duration of reconciliations per GVR",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"gvr"},
	)
	gvrCount = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "dynamic_controller_gvr_count",
			Help: "Number of GVRs currently managed by the controller",
		},
	)
	queueLength = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "dynamic_controller_queue_length",
			Help: "Current length of the workqueue",
		},
	)
	handlerErrorsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "dynamic_controller_handler_errors_total",
			Help: "Total number of errors encountered by handlers per GVR",
		},
		[]string{"gvr"},
	)
	informerSyncDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "dynamic_controller_informer_sync_duration_seconds",
			Help:    "Duration of informer cache sync per GVR",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"gvr"},
	)
	informerEventsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "dynamic_controller_informer_events_total",
			Help: "Total number of events processed by informers per GVR and event type",
		},
		[]string{"gvr", "event_type"},
	)
	/* activeWorkersTotal = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "dynamic_controller_active_workers_total",
			Help: "Total number of currently active workers",
		},
	)
	*/
)
