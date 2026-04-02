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
	"time"

	"github.com/prometheus/client_golang/prometheus"
	io_prometheus_client "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
)

func TestEmitConditionMetrics_NewCondition(t *testing.T) {
	// Reset metrics for test isolation.
	InstanceConditionCurrentStatusSeconds.Reset()

	log := zap.New(zap.UseDevMode(true))
	gvr := schema.GroupVersionResource{Group: "kro.run", Version: "v1alpha1", Resource: "webapps"}
	inst := &unstructured.Unstructured{}
	inst.SetNamespace("default")
	inst.SetName("my-app")

	now := metav1.Now()
	initial := []v1alpha1.Condition{}
	final := []v1alpha1.Condition{
		{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			Reason:             strPtr("AllReady"),
			LastTransitionTime: &now,
		},
	}

	EmitConditionMetrics(log, gvr, inst, initial, final)

	// Gauge should have one series.
	gaugeCount := testutil_countMetrics(t, InstanceConditionCurrentStatusSeconds)
	assert.Equal(t, 1, gaugeCount)
}

func TestEmitConditionMetrics_NoTransition(t *testing.T) {
	InstanceConditionCurrentStatusSeconds.Reset()

	log := zap.New(zap.UseDevMode(true))
	gvr := schema.GroupVersionResource{Group: "kro.run", Version: "v1alpha1", Resource: "webapps"}
	inst := &unstructured.Unstructured{}
	inst.SetNamespace("default")
	inst.SetName("my-app")

	now := metav1.Now()
	conds := []v1alpha1.Condition{
		{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			Reason:             strPtr("AllReady"),
			LastTransitionTime: &now,
		},
	}

	EmitConditionMetrics(log, gvr, inst, conds, conds)

	// Gauge should still be set (updated duration).
	gaugeCount := testutil_countMetrics(t, InstanceConditionCurrentStatusSeconds)
	assert.Equal(t, 1, gaugeCount)
}

func TestEmitConditionMetrics_StatusTransition(t *testing.T) {
	InstanceConditionCurrentStatusSeconds.Reset()

	log := zap.New(zap.UseDevMode(true))
	gvr := schema.GroupVersionResource{Group: "kro.run", Version: "v1alpha1", Resource: "webapps"}
	inst := &unstructured.Unstructured{}
	inst.SetNamespace("default")
	inst.SetName("my-app")

	now := metav1.Now()
	initial := []v1alpha1.Condition{
		{
			Type:               "ResourcesReady",
			Status:             metav1.ConditionFalse,
			Reason:             strPtr("NotReady"),
			LastTransitionTime: &now,
		},
	}
	final := []v1alpha1.Condition{
		{
			Type:               "ResourcesReady",
			Status:             metav1.ConditionTrue,
			Reason:             strPtr("AllResourcesReady"),
			LastTransitionTime: &now,
		},
	}

	EmitConditionMetrics(log, gvr, inst, initial, final)

	// Gauge should be updated with the new condition state.
	gaugeCount := testutil_countMetrics(t, InstanceConditionCurrentStatusSeconds)
	assert.Equal(t, 1, gaugeCount)
}

func TestEmitConditionMetrics_DisappearedCondition(t *testing.T) {
	InstanceConditionCurrentStatusSeconds.Reset()

	log := zap.New(zap.UseDevMode(true))
	gvr := schema.GroupVersionResource{Group: "kro.run", Version: "v1alpha1", Resource: "webapps"}
	inst := &unstructured.Unstructured{}
	inst.SetNamespace("default")
	inst.SetName("my-app")

	now := metav1.Now()

	// Pre-populate the gauge for the condition that will disappear.
	InstanceConditionCurrentStatusSeconds.WithLabelValues(
		gvr.String(), "default", "my-app", "ResourcesReady", "True", "AllResourcesReady",
	).Set(10)

	initial := []v1alpha1.Condition{
		{
			Type:               "ResourcesReady",
			Status:             metav1.ConditionTrue,
			Reason:             strPtr("AllResourcesReady"),
			LastTransitionTime: &now,
		},
	}
	final := []v1alpha1.Condition{} // condition disappeared

	EmitConditionMetrics(log, gvr, inst, initial, final)

	// Gauge should be cleaned up (no series left).
	gaugeCount := testutil_countMetrics(t, InstanceConditionCurrentStatusSeconds)
	assert.Equal(t, 0, gaugeCount)
}

func TestDeleteInstanceMetrics(t *testing.T) {
	InstanceConditionCurrentStatusSeconds.Reset()

	gvr := schema.GroupVersionResource{Group: "kro.run", Version: "v1alpha1", Resource: "webapps"}
	gvrKey := gvr.String()

	// Populate metrics for two instances.
	InstanceConditionCurrentStatusSeconds.WithLabelValues(gvrKey, "default", "app-1", "Ready", "True", "AllReady").Set(5)
	InstanceConditionCurrentStatusSeconds.WithLabelValues(gvrKey, "default", "app-2", "Ready", "True", "AllReady").Set(10)

	assert.Equal(t, 2, testutil_countMetrics(t, InstanceConditionCurrentStatusSeconds))

	DeleteInstanceMetrics(gvr, "default", "app-1")

	// Only app-2 should remain.
	assert.Equal(t, 1, testutil_countMetrics(t, InstanceConditionCurrentStatusSeconds))
}

func TestDeleteGVRMetrics(t *testing.T) {
	InstanceConditionCurrentStatusSeconds.Reset()

	gvr := schema.GroupVersionResource{Group: "kro.run", Version: "v1alpha1", Resource: "webapps"}
	gvrKey := gvr.String()

	// Populate metrics for two instances.
	InstanceConditionCurrentStatusSeconds.WithLabelValues(gvrKey, "default", "app-1", "Ready", "True", "AllReady").Set(5)
	InstanceConditionCurrentStatusSeconds.WithLabelValues(gvrKey, "default", "app-2", "Ready", "True", "AllReady").Set(10)

	assert.Equal(t, 2, testutil_countMetrics(t, InstanceConditionCurrentStatusSeconds))

	DeleteGVRMetrics(gvr)

	// All metrics for this GVR should be gone.
	assert.Equal(t, 0, testutil_countMetrics(t, InstanceConditionCurrentStatusSeconds))
}

func TestEmitConditionMetrics_DurationIsPositive(t *testing.T) {
	InstanceConditionCurrentStatusSeconds.Reset()

	log := zap.New(zap.UseDevMode(true))
	gvr := schema.GroupVersionResource{Group: "kro.run", Version: "v1alpha1", Resource: "webapps"}
	inst := &unstructured.Unstructured{}
	inst.SetNamespace("default")
	inst.SetName("my-app")

	past := metav1.NewTime(time.Now().Add(-30 * time.Second))
	initial := []v1alpha1.Condition{}
	final := []v1alpha1.Condition{
		{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			Reason:             strPtr("AllReady"),
			LastTransitionTime: &past,
		},
	}

	EmitConditionMetrics(log, gvr, inst, initial, final)

	// The gauge value should be >= 30 seconds.
	val := testutil_getGaugeValue(t, InstanceConditionCurrentStatusSeconds, gvr.String(), "default", "my-app", "Ready", "True", "AllReady")
	assert.GreaterOrEqual(t, val, 30.0)
}

func strPtr(s string) *string {
	return &s
}

// testutil_countMetrics returns the number of active metric series in a collector.
func testutil_countMetrics(t *testing.T, c prometheus.Collector) int {
	t.Helper()
	ch := make(chan prometheus.Metric, 100)
	c.Collect(ch)
	close(ch)
	count := 0
	for range ch {
		count++
	}
	return count
}

// testutil_getGaugeValue retrieves the value of a specific gauge series.
func testutil_getGaugeValue(t *testing.T, gv *prometheus.GaugeVec, labels ...string) float64 {
	t.Helper()
	gauge, err := gv.GetMetricWithLabelValues(labels...)
	require.NoError(t, err)

	ch := make(chan prometheus.Metric, 1)
	gauge.Collect(ch)
	close(ch)

	m := <-ch
	var dto io_prometheus_client.Metric
	require.NoError(t, m.Write(&dto))
	return dto.GetGauge().GetValue()
}
