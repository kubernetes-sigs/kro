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

package resourcegraphdefinition

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	graphBuildTotal       prometheus.Counter
	graphBuildDuration    prometheus.Histogram
	graphBuildErrorsTotal prometheus.Counter
	stateTransitionsTotal *prometheus.CounterVec
	deletionsTotal        prometheus.Counter
	deletionDuration      prometheus.Histogram
)

func init() {
	graphBuildTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "rgd_graph_build_total",
			Help: "Total number of resource graph builds",
		},
	)

	graphBuildDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "rgd_graph_build_duration_seconds",
			Help:    "Duration of resource graph builds in seconds",
			Buckets: prometheus.DefBuckets,
		},
	)

	graphBuildErrorsTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "rgd_graph_build_errors_total",
			Help: "Total number of resource graph build errors",
		},
	)

	stateTransitionsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "rgd_state_transitions_total",
			Help: "Total number of RGD state transitions",
		},
		[]string{"from", "to"},
	)

	deletionsTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "rgd_deletions_total",
			Help: "Total number of RGD deletions",
		},
	)

	deletionDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "rgd_deletion_duration_seconds",
			Help:    "Duration of RGD deletions in seconds",
			Buckets: prometheus.DefBuckets,
		},
	)

	metrics.Registry.MustRegister(
		graphBuildTotal,
		graphBuildDuration,
		graphBuildErrorsTotal,
		stateTransitionsTotal,
		deletionsTotal,
		deletionDuration,
	)
}
