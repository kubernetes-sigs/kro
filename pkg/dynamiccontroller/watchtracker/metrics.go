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

package watchtracker

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

func init() {
	metrics.Registry.MustRegister(
		watchTrackerGVRsTotal,
		watchTrackerGVRsByStatus,
		watchTrackerErrorsTotal,
		watchTrackerRecoveriesTotal,
		watchTrackerEventsDroppedTotal,
	)
}

var (
	watchTrackerGVRsTotal = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "watch_tracker_gvrs_total",
			Help: "Total number of GVRs currently tracked",
		},
	)
	watchTrackerGVRsByStatus = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "watch_tracker_gvrs_by_status",
			Help: "Number of tracked GVRs by status",
		},
		[]string{"status"},
	)
	watchTrackerErrorsTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "watch_tracker_errors_total",
			Help: "Total number of watch errors recorded",
		},
	)
	watchTrackerRecoveriesTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "watch_tracker_recoveries_total",
			Help: "Total number of successful recoveries from degraded state",
		},
	)
	watchTrackerEventsDroppedTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "watch_tracker_events_dropped_total",
			Help: "Total number of state change events dropped due to full buffer",
		},
	)
)
