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
	InstanceConditionCurrentStatusSeconds.reset()

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

	assert.Equal(t, 1, InstanceConditionCurrentStatusSeconds.size())
	assert.Equal(t, 1, testutilCountMetrics(t, InstanceConditionCurrentStatusSeconds))
}

func TestEmitConditionMetrics_NoTransition(t *testing.T) {
	InstanceConditionCurrentStatusSeconds.reset()

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

	assert.Equal(t, 1, InstanceConditionCurrentStatusSeconds.size())
	assert.Equal(t, 1, testutilCountMetrics(t, InstanceConditionCurrentStatusSeconds))
}

func TestEmitConditionMetrics_StatusTransition(t *testing.T) {
	InstanceConditionCurrentStatusSeconds.reset()

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
	InstanceConditionCurrentStatusSeconds.Cache(conditionKey{
		gvr:           gvr.String(),
		namespace:     "default",
		name:          "my-app",
		conditionType: "ResourcesReady",
	}, "False", "NotReady", now.Time)
	assert.Equal(t, 1, InstanceConditionCurrentStatusSeconds.size())

	EmitConditionMetrics(log, gvr, inst, initial, final)

	assert.Equal(t, 1, InstanceConditionCurrentStatusSeconds.size())
	got := collectorEntries(t, InstanceConditionCurrentStatusSeconds)
	require.Len(t, got, 1, "the transition must leave exactly one series")
	assert.Equal(t, "True", got[0].labels["condition_status"],
		"the remaining series must reflect the new status, not the pre-seeded False")
	assert.Equal(t, "AllResourcesReady", got[0].labels["reason"])
}

func TestEmitConditionMetrics_NoPhantomWhenCacheDivergesFromEtcd(t *testing.T) {
	InstanceConditionCurrentStatusSeconds.reset()

	log := zap.New(zap.UseDevMode(true))
	gvr := schema.GroupVersionResource{Group: "kro.run", Version: "v1alpha1", Resource: "webapps"}
	inst := &unstructured.Unstructured{}
	inst.SetNamespace("default")
	inst.SetName("my-app")

	now := metav1.Now()

	// Simulates a cache/etcd divergence (e.g. a failed status write): reconcile 1's
	// condition is cached but absent from reconcile 2's initialConditions.
	EmitConditionMetrics(log, gvr, inst, nil, []v1alpha1.Condition{
		{Type: "Ready", Status: metav1.ConditionFalse, Reason: strPtr("ReasonA"), LastTransitionTime: &now},
	})

	EmitConditionMetrics(log, gvr, inst, nil, []v1alpha1.Condition{
		{Type: "Ready", Status: metav1.ConditionFalse, Reason: strPtr("ReasonB"), LastTransitionTime: &now},
	})

	got := collectorEntries(t, InstanceConditionCurrentStatusSeconds)
	require.Len(t, got, 1, "must not strand a phantom series when the cache diverges from etcd")
	assert.Equal(t, "ReasonB", got[0].labels["reason"], "only the latest series should remain")
}

func TestEmitConditionMetrics_DisappearedCondition(t *testing.T) {
	InstanceConditionCurrentStatusSeconds.reset()

	log := zap.New(zap.UseDevMode(true))
	gvr := schema.GroupVersionResource{Group: "kro.run", Version: "v1alpha1", Resource: "webapps"}
	inst := &unstructured.Unstructured{}
	inst.SetNamespace("default")
	inst.SetName("my-app")

	now := metav1.Now()

	InstanceConditionCurrentStatusSeconds.Cache(conditionKey{
		gvr:           gvr.String(),
		namespace:     "default",
		name:          "my-app",
		conditionType: "ResourcesReady",
	}, "True", "AllResourcesReady", now.Time)

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

	assert.Equal(t, 0, InstanceConditionCurrentStatusSeconds.size())
	assert.Equal(t, 0, testutilCountMetrics(t, InstanceConditionCurrentStatusSeconds))
}

func TestDeleteInstanceMetrics(t *testing.T) {
	InstanceConditionCurrentStatusSeconds.reset()

	gvr := schema.GroupVersionResource{Group: "kro.run", Version: "v1alpha1", Resource: "webapps"}
	gvrKey := gvr.String()
	now := time.Now()

	InstanceConditionCurrentStatusSeconds.Cache(conditionKey{
		gvr: gvrKey, namespace: "default", name: "app-1", conditionType: "Ready",
	}, "True", "AllReady", now)
	InstanceConditionCurrentStatusSeconds.Cache(conditionKey{
		gvr: gvrKey, namespace: "default", name: "app-2", conditionType: "Ready",
	}, "True", "AllReady", now)

	assert.Equal(t, 2, InstanceConditionCurrentStatusSeconds.size())

	DeleteInstanceMetrics(gvr, "default", "app-1")

	assert.Equal(t, 1, InstanceConditionCurrentStatusSeconds.size())
}

func TestDeleteGVRMetrics(t *testing.T) {
	InstanceConditionCurrentStatusSeconds.reset()

	gvr := schema.GroupVersionResource{Group: "kro.run", Version: "v1alpha1", Resource: "webapps"}
	gvrKey := gvr.String()
	now := time.Now()

	InstanceConditionCurrentStatusSeconds.Cache(conditionKey{
		gvr: gvrKey, namespace: "default", name: "app-1", conditionType: "Ready",
	}, "True", "AllReady", now)
	InstanceConditionCurrentStatusSeconds.Cache(conditionKey{
		gvr: gvrKey, namespace: "default", name: "app-2", conditionType: "Ready",
	}, "True", "AllReady", now)

	assert.Equal(t, 2, InstanceConditionCurrentStatusSeconds.size())

	DeleteGVRMetrics(gvr)

	assert.Equal(t, 0, InstanceConditionCurrentStatusSeconds.size())
}

func TestEmitConditionMetrics_DurationIsPositive(t *testing.T) {
	InstanceConditionCurrentStatusSeconds.reset()

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

	val := testutilGetCollectorValue(t, InstanceConditionCurrentStatusSeconds,
		gvr.String(), "default", "my-app", "Ready", "True", "AllReady")
	assert.GreaterOrEqual(t, val, 30.0)
}

func TestEmitConditionMetrics_DurationStaysAccurateBetweenReconciles(t *testing.T) {
	InstanceConditionCurrentStatusSeconds.reset()

	log := zap.New(zap.UseDevMode(true))
	gvr := schema.GroupVersionResource{Group: "kro.run", Version: "v1alpha1", Resource: "webapps"}
	inst := &unstructured.Unstructured{}
	inst.SetNamespace("default")
	inst.SetName("my-app")

	past := metav1.NewTime(time.Now().Add(-10 * time.Second))
	final := []v1alpha1.Condition{
		{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			Reason:             strPtr("AllReady"),
			LastTransitionTime: &past,
		},
	}

	EmitConditionMetrics(log, gvr, inst, nil, final)

	val1 := testutilGetCollectorValue(t, InstanceConditionCurrentStatusSeconds,
		gvr.String(), "default", "my-app", "Ready", "True", "AllReady")

	time.Sleep(150 * time.Millisecond)

	val2 := testutilGetCollectorValue(t, InstanceConditionCurrentStatusSeconds,
		gvr.String(), "default", "my-app", "Ready", "True", "AllReady")

	assert.Greater(t, val2, val1, "scrape-time duration must advance even without a reconcile")
}

func strPtr(s string) *string {
	return &s
}

// testutilCountMetrics returns the number of active metric series a
// collector emits.
func testutilCountMetrics(t *testing.T, c prometheus.Collector) int {
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

// testutilGetCollectorValue retrieves the value of a specific gauge series.
func testutilGetCollectorValue(t *testing.T, c prometheus.Collector,
	gvr, namespace, name, conditionType, conditionStatus, reason string) float64 {
	t.Helper()
	want := map[string]string{
		labelGVR:             gvr,
		labelNamespace:       namespace,
		labelName:            name,
		labelConditionType:   conditionType,
		labelConditionStatus: conditionStatus,
		labelReason:          reason,
	}
	ch := make(chan prometheus.Metric, 100)
	c.Collect(ch)
	close(ch)

	for m := range ch {
		var dto io_prometheus_client.Metric
		require.NoError(t, m.Write(&dto))
		match := true
		for _, lp := range dto.Label {
			if v, ok := want[lp.GetName()]; !ok || v != lp.GetValue() {
				match = false
				break
			}
		}
		if match && len(dto.Label) == len(want) {
			return dto.GetGauge().GetValue()
		}
	}
	return 0
}
