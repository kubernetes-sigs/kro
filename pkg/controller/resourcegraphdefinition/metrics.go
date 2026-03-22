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
	rgdLabels                   = []string{"name"}
	stateTransitionLabels       = []string{"name", "from", "to"}
	graphRevisionReasonLabels   = []string{"reason"}
	graphRevisionResultLabels   = []string{"result"}
	graphRevisionRegistryLabels = []string{"reason"}

	graphBuildTotal                *prometheus.CounterVec
	graphBuildDuration             *prometheus.HistogramVec
	graphBuildErrorsTotal          *prometheus.CounterVec
	stateTransitionsTotal          *prometheus.CounterVec
	deletionsTotal                 *prometheus.CounterVec
	deletionDuration               *prometheus.HistogramVec
	graphRevisionIssueTotal        *prometheus.CounterVec
	graphRevisionWaitTotal         *prometheus.CounterVec
	graphRevisionResolutionTotal   *prometheus.CounterVec
	graphRevisionRegistryMissTotal *prometheus.CounterVec
	graphRevisionGCDeletedTotal    *prometheus.CounterVec
	graphRevisionGCErrorsTotal     *prometheus.CounterVec
)

func init() {
	graphBuildTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "rgd_graph_build_total",
			Help: "Total number of resource graph builds",
		},
		rgdLabels,
	)

	graphBuildDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "rgd_graph_build_duration_seconds",
			Help:    "Duration of resource graph builds in seconds",
			Buckets: []float64{.01, .05, .1, .25, .5, 1, 2.5, 5, 10, 15, 20, 25, 30, 45, 60, 120},
		},
		rgdLabels,
	)

	graphBuildErrorsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "rgd_graph_build_errors_total",
			Help: "Total number of resource graph build errors",
		},
		rgdLabels,
	)

	stateTransitionsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "rgd_state_transitions_total",
			Help: "Total number of RGD state transitions",
		},
		stateTransitionLabels,
	)

	deletionsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "rgd_deletions_total",
			Help: "Total number of RGD deletions",
		},
		rgdLabels,
	)

	deletionDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "rgd_deletion_duration_seconds",
			Help:    "Duration of RGD deletions in seconds",
			Buckets: []float64{.01, .05, .1, .25, .5, 1, 2.5, 5, 10, 15, 20, 25, 30, 45, 60, 120},
		},
		rgdLabels,
	)

	graphRevisionIssueTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "kro_rgd_graph_revision_issue_total",
			Help: "Total number of GraphRevision objects issued by the RGD controller",
		},
		graphRevisionReasonLabels,
	)

	graphRevisionWaitTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "kro_rgd_graph_revision_wait_total",
			Help: "Total number of times the RGD controller waited for GraphRevision progress",
		},
		graphRevisionReasonLabels,
	)

	graphRevisionResolutionTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "kro_rgd_graph_revision_resolution_total",
			Help: "Total number of GraphRevision resolution outcomes in the RGD controller",
		},
		graphRevisionResultLabels,
	)

	graphRevisionRegistryMissTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "kro_rgd_graph_revision_registry_miss_total",
			Help: "Total number of times the RGD controller observed GraphRevisions ahead of the in-memory registry",
		},
		graphRevisionRegistryLabels,
	)

	graphRevisionGCDeletedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "kro_rgd_graph_revision_gc_deleted_total",
			Help: "Total number of GraphRevision objects deleted by RGD garbage collection",
		},
		[]string{},
	)

	graphRevisionGCErrorsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "kro_rgd_graph_revision_gc_errors_total",
			Help: "Total number of GraphRevision garbage collection errors in the RGD controller",
		},
		[]string{},
	)

	metrics.Registry.MustRegister(
		graphBuildTotal,
		graphBuildDuration,
		graphBuildErrorsTotal,
		stateTransitionsTotal,
		deletionsTotal,
		deletionDuration,
		graphRevisionIssueTotal,
		graphRevisionWaitTotal,
		graphRevisionResolutionTotal,
		graphRevisionRegistryMissTotal,
		graphRevisionGCDeletedTotal,
		graphRevisionGCErrorsTotal,
	)
}
