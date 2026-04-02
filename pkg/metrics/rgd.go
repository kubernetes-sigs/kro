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
	rgdLabels                   = []string{"name"}
	rgdStateTransitionLabels    = []string{"name", "from", "to"}
	rgdGraphRevisionReasonLabel = []string{"reason"}
	rgdGraphRevisionResultLabel = []string{"result"}

	RGDGraphBuildTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "rgd_graph_build_total",
			Help: "Total number of resource graph builds",
		},
		rgdLabels,
	)

	RGDGraphBuildDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "rgd_graph_build_duration_seconds",
			Help:    "Duration of resource graph builds in seconds",
			Buckets: []float64{.01, .05, .1, .25, .5, 1, 2.5, 5, 10, 15, 20, 25, 30, 45, 60, 120},
		},
		rgdLabels,
	)

	RGDGraphBuildErrorsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "rgd_graph_build_errors_total",
			Help: "Total number of resource graph build errors",
		},
		rgdLabels,
	)

	RGDStateTransitionsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "rgd_state_transitions_total",
			Help: "Total number of RGD state transitions",
		},
		rgdStateTransitionLabels,
	)

	RGDDeletionsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "rgd_deletions_total",
			Help: "Total number of RGD deletions",
		},
		rgdLabels,
	)

	RGDDeletionDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "rgd_deletion_duration_seconds",
			Help:    "Duration of RGD deletions in seconds",
			Buckets: []float64{.01, .05, .1, .25, .5, 1, 2.5, 5, 10, 15, 20, 25, 30, 45, 60, 120},
		},
		rgdLabels,
	)

	RGDGraphRevisionIssueTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "kro_rgd_graph_revision_issue_total",
			Help: "Total number of GraphRevision objects issued by the RGD controller",
		},
		rgdGraphRevisionReasonLabel,
	)

	RGDGraphRevisionWaitTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "kro_rgd_graph_revision_wait_total",
			Help: "Total number of times the RGD controller waited for GraphRevision progress",
		},
		rgdGraphRevisionReasonLabel,
	)

	RGDGraphRevisionResolutionTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "kro_rgd_graph_revision_resolution_total",
			Help: "Total number of GraphRevision resolution outcomes in the RGD controller",
		},
		rgdGraphRevisionResultLabel,
	)

	RGDGraphRevisionRegistryMissTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "kro_rgd_graph_revision_registry_miss_total",
			Help: "Total number of times the RGD controller observed GraphRevisions ahead of the in-memory registry",
		},
		rgdGraphRevisionReasonLabel,
	)

	RGDGraphRevisionGCDeletedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "kro_rgd_graph_revision_gc_deleted_total",
			Help: "Total number of GraphRevision objects deleted by RGD garbage collection",
		},
		[]string{},
	)

	RGDGraphRevisionGCErrorsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "kro_rgd_graph_revision_gc_errors_total",
			Help: "Total number of GraphRevision garbage collection errors in the RGD controller",
		},
		[]string{},
	)
)
