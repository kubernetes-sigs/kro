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

package revisions

import (
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	graphRevisionRegistryStateLabels      = []string{"state"}
	graphRevisionRegistryTransitionLabels = []string{"from", "to"}

	graphRevisionRegistryEntries     *prometheus.GaugeVec
	graphRevisionRegistryTransitions *prometheus.CounterVec
	graphRevisionRegistryEvictions   *prometheus.CounterVec
)

func init() {
	graphRevisionRegistryEntries = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "kro_graph_revision_registry_entries",
			Help: "Current number of GraphRevision entries in the in-memory registry by state",
		},
		graphRevisionRegistryStateLabels,
	)

	graphRevisionRegistryTransitions = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "kro_graph_revision_registry_transitions_total",
			Help: "Total number of GraphRevision registry state transitions",
		},
		graphRevisionRegistryTransitionLabels,
	)

	graphRevisionRegistryEvictions = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "kro_graph_revision_registry_evictions_total",
			Help: "Total number of GraphRevision registry evictions",
		},
		[]string{},
	)

	metrics.Registry.MustRegister(
		graphRevisionRegistryEntries,
		graphRevisionRegistryTransitions,
		graphRevisionRegistryEvictions,
	)
}

func revisionStateLabel(state RevisionState) string {
	return strings.ToLower(string(state))
}
