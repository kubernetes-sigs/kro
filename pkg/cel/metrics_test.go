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

package cel

import (
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCelMetrics(t *testing.T) {
	// Create a custom registry for testing to avoid conflicts
	registry := prometheus.NewRegistry()
	metrics := newCelMetrics()
	metrics.MustRegister(registry)

	t.Run("ObserveCompilation success", func(t *testing.T) {
		metrics.ObserveCompilation(0.001, nil)

		// Collect metrics and verify count
		count := testutil.CollectAndCount(metrics.compilationTime)
		assert.Greater(t, count, 0)
	})

	t.Run("ObserveCompilation error", func(t *testing.T) {
		metrics.ObserveCompilation(0.002, errors.New("compilation error"))

		// Collect metrics and verify count
		count := testutil.CollectAndCount(metrics.compilationTime)
		assert.Greater(t, count, 0)
	})

	t.Run("ObserveEvaluation success", func(t *testing.T) {
		metrics.ObserveEvaluation(0.0005, nil)

		// Collect metrics and verify count
		count := testutil.CollectAndCount(metrics.evaluationTime)
		assert.Greater(t, count, 0)
	})

	t.Run("ObserveEvaluation error", func(t *testing.T) {
		metrics.ObserveEvaluation(0.0003, errors.New("evaluation error"))

		// Collect metrics and verify count
		count := testutil.CollectAndCount(metrics.evaluationTime)
		assert.Greater(t, count, 0)
	})

	t.Run("Verify labels are used correctly", func(t *testing.T) {
		// Create fresh metrics to test labels
		testMetrics := newCelMetrics()
		testRegistry := prometheus.NewRegistry()
		testMetrics.MustRegister(testRegistry)

		// Record both success and error
		testMetrics.ObserveCompilation(0.001, nil)
		testMetrics.ObserveCompilation(0.002, errors.New("error"))
		testMetrics.ObserveEvaluation(0.001, nil)
		testMetrics.ObserveEvaluation(0.002, errors.New("error"))

		// Gather metrics and verify labels exist
		metricFamilies, err := testRegistry.Gather()
		require.NoError(t, err)

		for _, mf := range metricFamilies {
			// Each metric family should have metrics with labels
			assert.Greater(t, len(mf.GetMetric()), 0)
			for _, m := range mf.GetMetric() {
				// Find the "result" label
				hasResultLabel := false
				for _, label := range m.GetLabel() {
					if label.GetName() == "result" {
						hasResultLabel = true
						// Verify label value is either "success" or "error"
						value := label.GetValue()
						assert.Contains(t, []string{"success", "error"}, value)
					}
				}
				assert.True(t, hasResultLabel, "metric should have 'result' label")
			}
		}
	})
}

func TestMetricsRegistration(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := newCelMetrics()

	// Should not panic
	require.NotPanics(t, func() {
		metrics.MustRegister(registry)
	})

	// Record some observations to create the metrics
	metrics.ObserveCompilation(0.001, nil)
	metrics.ObserveEvaluation(0.001, nil)

	// Verify both metrics are registered
	metricFamilies, err := registry.Gather()
	require.NoError(t, err)
	assert.Len(t, metricFamilies, 2)

	names := make(map[string]bool)
	for _, mf := range metricFamilies {
		names[mf.GetName()] = true
	}

	assert.True(t, names["kro_cel_compilation_duration_seconds"])
	assert.True(t, names["kro_cel_evaluation_duration_seconds"])
}
