// Copyright 2026 The Kubernetes Authors.
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
	graphRevisionCompileLabels = []string{"result"}

	GraphRevisionCompileTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "graph_revision_compile_total",
			Help: "Total number of GraphRevision compile attempts by result",
		},
		graphRevisionCompileLabels,
	)

	GraphRevisionCompileDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "graph_revision_compile_duration_seconds",
			Help:    "Duration of GraphRevision compile attempts in seconds",
			Buckets: []float64{.01, .05, .1, .25, .5, 1, 2.5, 5, 10, 15, 20, 25, 30, 45, 60, 120},
		},
		graphRevisionCompileLabels,
	)

	GraphRevisionStatusUpdateErrorsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "graph_revision_status_update_errors_total",
			Help: "Total number of GraphRevision status update failures",
		},
		[]string{},
	)

	GraphRevisionActivationDeferredTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "graph_revision_activation_deferred_total",
			Help: "Total number of times GraphRevision activation was deferred until status persistence succeeds",
		},
		[]string{},
	)

	GraphRevisionFinalizerEvictionsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "graph_revision_finalizer_evictions_total",
			Help: "Total number of registry evictions triggered by GraphRevision finalizer cleanup",
		},
		[]string{},
	)
)
