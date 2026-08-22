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

import (
	"testing"

	io_prometheus_client "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
)

func getCounterValue(c prometheusCounter) float64 {
	m := &io_prometheus_client.Metric{}
	_ = c.Write(m)
	if m.Counter != nil {
		return m.Counter.GetValue()
	}
	return 0
}

type prometheusCounter interface {
	Write(*io_prometheus_client.Metric) error
}

func TestRuntimeMetrics_Increment(t *testing.T) {
	beforeEval := getCounterValue(NodeEvalTotal)
	NodeEvalTotal.Inc()
	afterEval := getCounterValue(NodeEvalTotal)
	assert.Equal(t, beforeEval+1, afterEval)

	beforeErrors := getCounterValue(NodeEvalErrorsTotal)
	NodeEvalErrorsTotal.Inc()
	afterErrors := getCounterValue(NodeEvalErrorsTotal)
	assert.Equal(t, beforeErrors+1, afterErrors)

	beforeCreation := getCounterValue(RuntimeCreationTotal)
	RuntimeCreationTotal.Inc()
	afterCreation := getCounterValue(RuntimeCreationTotal)
	assert.Equal(t, beforeCreation+1, afterCreation)

	beforeIgnoredCheck := getCounterValue(NodeIgnoredCheckTotal)
	NodeIgnoredCheckTotal.Inc()
	afterIgnoredCheck := getCounterValue(NodeIgnoredCheckTotal)
	assert.Equal(t, beforeIgnoredCheck+1, afterIgnoredCheck)

	beforeIgnored := getCounterValue(NodeIgnoredTotal)
	NodeIgnoredTotal.Inc()
	afterIgnored := getCounterValue(NodeIgnoredTotal)
	assert.Equal(t, beforeIgnored+1, afterIgnored)

	beforeReadyCheck := getCounterValue(NodeReadyCheckTotal)
	NodeReadyCheckTotal.Inc()
	afterReadyCheck := getCounterValue(NodeReadyCheckTotal)
	assert.Equal(t, beforeReadyCheck+1, afterReadyCheck)

	beforeNotReady := getCounterValue(NodeNotReadyTotal)
	NodeNotReadyTotal.Inc()
	afterNotReady := getCounterValue(NodeNotReadyTotal)
	assert.Equal(t, beforeNotReady+1, afterNotReady)

	// Histograms
	RuntimeCreationDuration.Observe(0.005)
	NodeEvalDuration.Observe(0.001)
	CollectionSize.Observe(10)
}

func TestInstanceGraphResolutionMetrics(t *testing.T) {
	gvr := "kro.run/v1alpha1, Resource=testapps"

	cSuccess := InstanceGraphResolutionSuccessTotal.WithLabelValues(gvr)
	beforeSuccess := getCounterValue(cSuccess)
	cSuccess.Inc()
	assert.Equal(t, beforeSuccess+1, getCounterValue(cSuccess))

	cPending := InstanceGraphResolutionPendingTotal.WithLabelValues(gvr)
	beforePending := getCounterValue(cPending)
	cPending.Inc()
	assert.Equal(t, beforePending+1, getCounterValue(cPending))

	cFailure := InstanceGraphResolutionFailuresTotal.WithLabelValues(gvr, "build_failed")
	beforeFailure := getCounterValue(cFailure)
	cFailure.Inc()
	assert.Equal(t, beforeFailure+1, getCounterValue(cFailure))
}
