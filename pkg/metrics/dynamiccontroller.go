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

package metrics

import "github.com/prometheus/client_golang/prometheus"

var (
	DynReconcileTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "dynamic_controller_reconcile_total",
			Help: "Total number of reconciliations per GVR",
		},
		[]string{"gvr"},
	)
	DynRequeueTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "dynamic_controller_requeue_total",
			Help: "Total number of requeues per GVR",
		},
		[]string{"gvr", "type"},
	)
	DynReconcileDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "dynamic_controller_reconcile_duration_seconds",
			Help:    "Duration of reconciliations per GVR",
			Buckets: []float64{.01, .05, .1, .25, .5, 1, 2.5, 5, 10, 15, 20, 25, 30, 45, 60, 120},
		},
		[]string{"gvr"},
	)
	DynGVRCount = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "dynamic_controller_gvr_count",
			Help: "Number of Instance GVRs currently managed by the controller",
		},
	)
	DynQueueLength = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "dynamic_controller_queue_length",
			Help: "Current length of the workqueue",
		},
	)
	DynHandlerCount = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "dynamic_controller_handler_count_total",
		Help: "Number of active handlers used for distributing events to instance controllers",
	}, []string{"type"})
	DynHandlerAttachTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "dynamic_controller_handler_attach_total",
		Help: "Total number of handler attachments by handler type",
	}, []string{"type"})
	DynHandlerDetachTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "dynamic_controller_handler_detach_total",
		Help: "Total number of handler detachments by handler type",
	}, []string{"type"})
	DynHandlerErrorsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "dynamic_controller_handler_errors_total",
			Help: "Total number of errors encountered by handlers per GVR",
		},
		[]string{"gvr"},
	)
	DynInformerSyncDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "dynamic_controller_informer_sync_duration_seconds",
			Help:    "Duration of informer cache sync per GVR",
			Buckets: []float64{.01, .05, .1, .25, .5, 1, 2.5, 5, 10, 15, 20, 25, 30, 45, 60, 120},
		},
		[]string{"gvr"},
	)
	DynInformerEventsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "dynamic_controller_informer_events_total",
			Help: "Total number of events processed by informers per GVR and event type",
		},
		[]string{"gvr", "event_type"},
	)
	DynWatchCount = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "dynamic_controller_watch_count",
			Help: "Number of active informers managed by the WatchManager",
		},
	)
	DynInstanceWatchCount = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "dynamic_controller_instance_watch_count",
			Help: "Number of active instance watchers by parent GVR",
		},
		[]string{"parent_gvr"},
	)
	DynWatchRequestCount = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "dynamic_controller_watch_request_count",
			Help: "Number of active watch requests by GVR and type (scalar/collection)",
		},
		[]string{"gvr", "type"},
	)
	DynRouteTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "dynamic_controller_route_total",
			Help: "Total events routed through the coordinator by GVR",
		},
		[]string{"gvr"},
	)
	DynRouteMatchTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "dynamic_controller_route_match_total",
			Help: "Total events that matched at least one instance by GVR",
		},
		[]string{"gvr"},
	)
)
