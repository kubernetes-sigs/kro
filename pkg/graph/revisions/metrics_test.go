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
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"

	"github.com/kubernetes-sigs/kro/pkg/graph"
)

func TestRegistryMetricsTrackEntriesTransitionsAndEvictions(t *testing.T) {
	resetRegistryMetrics()

	reg := NewRegistry()
	reg.Put(Entry{RGDName: "demo-rgd", Revision: 1, State: RevisionStatePending})
	assert.Equal(t, 1.0, testutil.ToFloat64(graphRevisionRegistryEntries.WithLabelValues("pending")))

	reg.Put(Entry{
		RGDName:       "demo-rgd",
		Revision:      1,
		State:         RevisionStateActive,
		CompiledGraph: &graph.Graph{},
	})
	assert.Equal(t, 0.0, testutil.ToFloat64(graphRevisionRegistryEntries.WithLabelValues("pending")))
	assert.Equal(t, 1.0, testutil.ToFloat64(graphRevisionRegistryEntries.WithLabelValues("active")))
	assert.Equal(t, 1.0, testutil.ToFloat64(graphRevisionRegistryTransitions.WithLabelValues("pending", "active")))

	reg.Put(Entry{RGDName: "demo-rgd", Revision: 1, State: RevisionStateFailed})
	assert.Equal(t, 0.0, testutil.ToFloat64(graphRevisionRegistryEntries.WithLabelValues("active")))
	assert.Equal(t, 1.0, testutil.ToFloat64(graphRevisionRegistryEntries.WithLabelValues("failed")))
	assert.Equal(t, 1.0, testutil.ToFloat64(graphRevisionRegistryTransitions.WithLabelValues("active", "failed")))

	reg.Delete("demo-rgd", 1)
	assert.Equal(t, 0.0, testutil.ToFloat64(graphRevisionRegistryEntries.WithLabelValues("failed")))
	assert.Equal(t, 1.0, testutil.ToFloat64(graphRevisionRegistryEvictions.WithLabelValues()))
}

func TestRegistryMetricsTrackBulkEvictions(t *testing.T) {
	resetRegistryMetrics()

	reg := NewRegistry()
	reg.Put(Entry{RGDName: "demo-rgd", Revision: 1, State: RevisionStateActive, CompiledGraph: &graph.Graph{}})
	reg.Put(Entry{RGDName: "demo-rgd", Revision: 2, State: RevisionStatePending})
	reg.Put(Entry{RGDName: "other-rgd", Revision: 1, State: RevisionStateActive, CompiledGraph: &graph.Graph{}})

	reg.DeleteAll("demo-rgd")
	assert.Equal(t, 1.0, testutil.ToFloat64(graphRevisionRegistryEntries.WithLabelValues("active")))
	assert.Equal(t, 0.0, testutil.ToFloat64(graphRevisionRegistryEntries.WithLabelValues("pending")))
	assert.Equal(t, 2.0, testutil.ToFloat64(graphRevisionRegistryEvictions.WithLabelValues()))

	reg.Put(Entry{RGDName: "demo-rgd", Revision: 1, State: RevisionStateFailed})
	reg.Put(Entry{RGDName: "demo-rgd", Revision: 2, State: RevisionStateActive, CompiledGraph: &graph.Graph{}})
	reg.Put(Entry{RGDName: "demo-rgd", Revision: 3, State: RevisionStatePending})

	reg.DeleteRevisionsBefore("demo-rgd", 3)
	assert.Equal(t, 1.0, testutil.ToFloat64(graphRevisionRegistryEntries.WithLabelValues("active")))
	assert.Equal(t, 1.0, testutil.ToFloat64(graphRevisionRegistryEntries.WithLabelValues("pending")))
	assert.Equal(t, 0.0, testutil.ToFloat64(graphRevisionRegistryEntries.WithLabelValues("failed")))
	assert.Equal(t, 4.0, testutil.ToFloat64(graphRevisionRegistryEvictions.WithLabelValues()))
}

func resetRegistryMetrics() {
	graphRevisionRegistryEntries.Reset()
	graphRevisionRegistryTransitions.Reset()
	graphRevisionRegistryEvictions.Reset()
}
