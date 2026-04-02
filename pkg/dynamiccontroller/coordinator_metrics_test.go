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

package dynamiccontroller

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/kubernetes-sigs/kro/pkg/metrics"
)

// resetCoordinatorMetrics zeroes the global gauge vectors used by the
// coordinator so that tests don't leak state into each other.
func resetCoordinatorMetrics() {
	metrics.DynInstanceWatchCount.Reset()
	metrics.DynWatchRequestCount.Reset()
}

func TestMetrics_InstanceWatchCount_SingleAdd(t *testing.T) {
	resetCoordinatorMetrics()
	coord, _ := newTestCoordinator(t)
	instance := types.NamespacedName{Name: "app1", Namespace: "default"}

	watcher := coord.ForInstance(testParentGVR, instance)
	require.NoError(t, watcher.Watch(WatchRequest{
		NodeID: "deploy", GVR: testDeployGVR, Name: "d1", Namespace: "default",
	}))
	watcher.Done(true)

	got := testutil.ToFloat64(metrics.DynInstanceWatchCount.WithLabelValues(testParentGVR.String()))
	assert.Equal(t, float64(1), got)
}

func TestMetrics_InstanceWatchCount_RemoveInstance(t *testing.T) {
	resetCoordinatorMetrics()
	coord, _ := newTestCoordinator(t)
	instance := types.NamespacedName{Name: "app1", Namespace: "default"}

	watcher := coord.ForInstance(testParentGVR, instance)
	require.NoError(t, watcher.Watch(WatchRequest{
		NodeID: "deploy", GVR: testDeployGVR, Name: "d1", Namespace: "default",
	}))
	watcher.Done(true)

	coord.RemoveInstance(testParentGVR, instance)

	// After removing the only instance, the label set should be deleted.
	// Collecting the metric should yield 0 children.
	assert.Equal(t, 0, testutil.CollectAndCount(metrics.DynInstanceWatchCount),
		"instanceWatchCount label set should be deleted after last instance removed")
}

func TestMetrics_InstanceWatchCount_RemoveParentGVR_MultipleInstances(t *testing.T) {
	resetCoordinatorMetrics()
	coord, _ := newTestCoordinator(t)

	// Register 3 instances under the same parent GVR.
	for _, name := range []string{"app1", "app2", "app3"} {
		inst := types.NamespacedName{Name: name, Namespace: "default"}
		w := coord.ForInstance(testParentGVR, inst)
		require.NoError(t, w.Watch(WatchRequest{
			NodeID: "deploy", GVR: testDeployGVR, Name: "d-" + name, Namespace: "default",
		}))
		w.Done(true)
	}

	got := testutil.ToFloat64(metrics.DynInstanceWatchCount.WithLabelValues(testParentGVR.String()))
	assert.Equal(t, float64(3), got, "should have 3 instances before removal")

	// RemoveParentGVR removes all 3 at once.
	coord.RemoveParentGVR(testParentGVR)

	assert.Equal(t, 0, testutil.CollectAndCount(metrics.DynInstanceWatchCount),
		"instanceWatchCount label set should be deleted after RemoveParentGVR")
}

func TestMetrics_WatchRequestCount_ScalarAddRemove(t *testing.T) {
	resetCoordinatorMetrics()
	coord, _ := newTestCoordinator(t)
	inst := types.NamespacedName{Name: "app1", Namespace: "default"}

	w := coord.ForInstance(testParentGVR, inst)
	require.NoError(t, w.Watch(WatchRequest{
		NodeID: "deploy", GVR: testDeployGVR, Name: "d1", Namespace: "default",
	}))
	w.Done(true)

	got := testutil.ToFloat64(metrics.DynWatchRequestCount.WithLabelValues(testDeployGVR.String(), "scalar"))
	assert.Equal(t, float64(1), got)

	coord.RemoveInstance(testParentGVR, inst)

	// Both scalar and collection labels should be deleted for this GVR.
	assert.Equal(t, 0, testutil.CollectAndCount(metrics.DynWatchRequestCount),
		"watchRequestCount labels should be deleted after GVR fully removed")
}

func TestMetrics_WatchRequestCount_CollectionAddRemove(t *testing.T) {
	resetCoordinatorMetrics()
	coord, _ := newTestCoordinator(t)
	inst := types.NamespacedName{Name: "app1", Namespace: "default"}

	selector, _ := labels.Parse("app=test")
	w := coord.ForInstance(testParentGVR, inst)
	require.NoError(t, w.Watch(WatchRequest{
		NodeID: "configs", GVR: testCmGVR, Namespace: "default", Selector: selector,
	}))
	w.Done(true)

	got := testutil.ToFloat64(metrics.DynWatchRequestCount.WithLabelValues(testCmGVR.String(), "collection"))
	assert.Equal(t, float64(1), got)

	coord.RemoveInstance(testParentGVR, inst)

	assert.Equal(t, 0, testutil.CollectAndCount(metrics.DynWatchRequestCount),
		"watchRequestCount labels should be deleted after GVR fully removed")
}

func TestMetrics_WatchRequestCount_PartialRemoval_KeepsOtherGVR(t *testing.T) {
	resetCoordinatorMetrics()
	coord, _ := newTestCoordinator(t)

	inst1 := types.NamespacedName{Name: "app1", Namespace: "default"}
	inst2 := types.NamespacedName{Name: "app2", Namespace: "default"}

	// inst1 watches deployments.
	w1 := coord.ForInstance(testParentGVR, inst1)
	require.NoError(t, w1.Watch(WatchRequest{
		NodeID: "deploy", GVR: testDeployGVR, Name: "d1", Namespace: "default",
	}))
	w1.Done(true)

	// inst2 watches services.
	w2 := coord.ForInstance(testParentGVR, inst2)
	require.NoError(t, w2.Watch(WatchRequest{
		NodeID: "svc", GVR: testServiceGVR, Name: "s1", Namespace: "default",
	}))
	w2.Done(true)

	// Remove inst1 — services gauge should be unaffected.
	coord.RemoveInstance(testParentGVR, inst1)

	svcGauge := testutil.ToFloat64(metrics.DynWatchRequestCount.WithLabelValues(testServiceGVR.String(), "scalar"))
	assert.Equal(t, float64(1), svcGauge, "service scalar gauge should remain 1")
}

func TestMetrics_WatchRequestCount_MultipleScalarsForSameGVR(t *testing.T) {
	resetCoordinatorMetrics()
	coord, _ := newTestCoordinator(t)

	inst1 := types.NamespacedName{Name: "app1", Namespace: "default"}
	inst2 := types.NamespacedName{Name: "app2", Namespace: "default"}

	// Both instances watch different deployments (same GVR).
	w1 := coord.ForInstance(testParentGVR, inst1)
	require.NoError(t, w1.Watch(WatchRequest{
		NodeID: "deploy", GVR: testDeployGVR, Name: "d1", Namespace: "default",
	}))
	w1.Done(true)

	w2 := coord.ForInstance(testParentGVR, inst2)
	require.NoError(t, w2.Watch(WatchRequest{
		NodeID: "deploy", GVR: testDeployGVR, Name: "d2", Namespace: "default",
	}))
	w2.Done(true)

	got := testutil.ToFloat64(metrics.DynWatchRequestCount.WithLabelValues(testDeployGVR.String(), "scalar"))
	assert.Equal(t, float64(2), got, "two scalar entries for same GVR")

	// Remove one — gauge should go to 1, not 0 or stay at 2.
	coord.RemoveInstance(testParentGVR, inst1)

	got = testutil.ToFloat64(metrics.DynWatchRequestCount.WithLabelValues(testDeployGVR.String(), "scalar"))
	assert.Equal(t, float64(1), got, "one scalar entry remaining")

	// Remove the other — labels should be deleted.
	coord.RemoveInstance(testParentGVR, inst2)

	assert.Equal(t, 0, testutil.CollectAndCount(metrics.DynWatchRequestCount),
		"watchRequestCount labels should be deleted after all entries removed")
}

func TestMetrics_WatchRequestCount_MixedScalarAndCollection_RemoveParentGVR(t *testing.T) {
	resetCoordinatorMetrics()
	coord, _ := newTestCoordinator(t)

	inst1 := types.NamespacedName{Name: "app1", Namespace: "default"}
	inst2 := types.NamespacedName{Name: "app2", Namespace: "default"}

	// inst1: scalar watch on deployments.
	w1 := coord.ForInstance(testParentGVR, inst1)
	require.NoError(t, w1.Watch(WatchRequest{
		NodeID: "deploy", GVR: testDeployGVR, Name: "d1", Namespace: "default",
	}))
	w1.Done(true)

	// inst2: collection watch on configmaps.
	selector, _ := labels.Parse("app=test")
	w2 := coord.ForInstance(testParentGVR, inst2)
	require.NoError(t, w2.Watch(WatchRequest{
		NodeID: "configs", GVR: testCmGVR, Namespace: "default", Selector: selector,
	}))
	w2.Done(true)

	deployScalar := testutil.ToFloat64(metrics.DynWatchRequestCount.WithLabelValues(testDeployGVR.String(), "scalar"))
	cmCollection := testutil.ToFloat64(metrics.DynWatchRequestCount.WithLabelValues(testCmGVR.String(), "collection"))
	assert.Equal(t, float64(1), deployScalar)
	assert.Equal(t, float64(1), cmCollection)

	// RemoveParentGVR removes both instances.
	coord.RemoveParentGVR(testParentGVR)

	assert.Equal(t, 0, testutil.CollectAndCount(metrics.DynWatchRequestCount),
		"all watchRequestCount labels should be deleted after RemoveParentGVR")
	assert.Equal(t, 0, testutil.CollectAndCount(metrics.DynInstanceWatchCount),
		"instanceWatchCount should be deleted after RemoveParentGVR")
}

func TestMetrics_WatchRequestCount_DoneCleansUpStaleRequests(t *testing.T) {
	resetCoordinatorMetrics()
	coord, _ := newTestCoordinator(t)
	inst := types.NamespacedName{Name: "app1", Namespace: "default"}

	// Cycle 1: watch deployment + service.
	w1 := coord.ForInstance(testParentGVR, inst)
	require.NoError(t, w1.Watch(WatchRequest{
		NodeID: "deploy", GVR: testDeployGVR, Name: "d1", Namespace: "default",
	}))
	require.NoError(t, w1.Watch(WatchRequest{
		NodeID: "svc", GVR: testServiceGVR, Name: "s1", Namespace: "default",
	}))
	w1.Done(true)

	deployGauge := testutil.ToFloat64(metrics.DynWatchRequestCount.WithLabelValues(testDeployGVR.String(), "scalar"))
	svcGauge := testutil.ToFloat64(metrics.DynWatchRequestCount.WithLabelValues(testServiceGVR.String(), "scalar"))
	assert.Equal(t, float64(1), deployGauge)
	assert.Equal(t, float64(1), svcGauge)

	// Cycle 2: only watch deployment — service should be cleaned up.
	w2 := coord.ForInstance(testParentGVR, inst)
	require.NoError(t, w2.Watch(WatchRequest{
		NodeID: "deploy", GVR: testDeployGVR, Name: "d1", Namespace: "default",
	}))
	w2.Done(true)

	deployGauge = testutil.ToFloat64(metrics.DynWatchRequestCount.WithLabelValues(testDeployGVR.String(), "scalar"))
	assert.Equal(t, float64(1), deployGauge, "deployment gauge should remain 1")

	// Service GVR should have its labels deleted (not just set to 0).
	// We check the total count of label sets in watchRequestCount:
	// only the deployment scalar label pair should remain (the deployment scalar
	// was touched via WithLabelValues above, which re-creates it even if deleted).
	// Use the WatchRequestCount helper instead for a clean check.
	scalar, collection := coord.WatchRequestCount()
	assert.Equal(t, 1, scalar, "only deployment scalar should remain")
	assert.Equal(t, 0, collection, "no collection watches")
}

func TestMetrics_WatchRequestCount_CollectionRemoval_WithScalarRemaining(t *testing.T) {
	resetCoordinatorMetrics()
	coord, _ := newTestCoordinator(t)

	inst1 := types.NamespacedName{Name: "app1", Namespace: "default"}
	inst2 := types.NamespacedName{Name: "app2", Namespace: "default"}

	// inst1: scalar watch on configmaps.
	w1 := coord.ForInstance(testParentGVR, inst1)
	require.NoError(t, w1.Watch(WatchRequest{
		NodeID: "cm", GVR: testCmGVR, Name: "my-cm", Namespace: "default",
	}))
	w1.Done(true)

	// inst2: collection watch on the same GVR (configmaps).
	selector, _ := labels.Parse("app=test")
	w2 := coord.ForInstance(testParentGVR, inst2)
	require.NoError(t, w2.Watch(WatchRequest{
		NodeID: "configs", GVR: testCmGVR, Namespace: "default", Selector: selector,
	}))
	w2.Done(true)

	cmScalar := testutil.ToFloat64(metrics.DynWatchRequestCount.WithLabelValues(testCmGVR.String(), "scalar"))
	cmCollection := testutil.ToFloat64(metrics.DynWatchRequestCount.WithLabelValues(testCmGVR.String(), "collection"))
	assert.Equal(t, float64(1), cmScalar)
	assert.Equal(t, float64(1), cmCollection)

	// Remove inst2 (collection watcher). Scalar watcher should be unaffected.
	coord.RemoveInstance(testParentGVR, inst2)

	cmScalar = testutil.ToFloat64(metrics.DynWatchRequestCount.WithLabelValues(testCmGVR.String(), "scalar"))
	assert.Equal(t, float64(1), cmScalar, "scalar gauge should remain 1 after collection removal")

	// Collection gauge should be 0 (Sub was called), and labels should NOT
	// be deleted yet because the scalar index still has entries for this GVR.
	cmCollection = testutil.ToFloat64(metrics.DynWatchRequestCount.WithLabelValues(testCmGVR.String(), "collection"))
	assert.Equal(t, float64(0), cmCollection, "collection gauge should be 0 after removal")
}

func TestMetrics_InstanceWatchCount_MultipleParentGVRs(t *testing.T) {
	resetCoordinatorMetrics()
	coord, _ := newTestCoordinator(t)

	parentGVR2 := schema.GroupVersionResource{Group: "kro.run", Version: "v1alpha1", Resource: "databases"}

	inst1 := types.NamespacedName{Name: "app1", Namespace: "default"}
	inst2 := types.NamespacedName{Name: "db1", Namespace: "default"}

	w1 := coord.ForInstance(testParentGVR, inst1)
	require.NoError(t, w1.Watch(WatchRequest{
		NodeID: "deploy", GVR: testDeployGVR, Name: "d1", Namespace: "default",
	}))
	w1.Done(true)

	w2 := coord.ForInstance(parentGVR2, inst2)
	require.NoError(t, w2.Watch(WatchRequest{
		NodeID: "deploy", GVR: testDeployGVR, Name: "d2", Namespace: "default",
	}))
	w2.Done(true)

	got1 := testutil.ToFloat64(metrics.DynInstanceWatchCount.WithLabelValues(testParentGVR.String()))
	got2 := testutil.ToFloat64(metrics.DynInstanceWatchCount.WithLabelValues(parentGVR2.String()))
	assert.Equal(t, float64(1), got1)
	assert.Equal(t, float64(1), got2)

	// Remove one parent — the other should be unaffected.
	coord.RemoveParentGVR(testParentGVR)

	got2 = testutil.ToFloat64(metrics.DynInstanceWatchCount.WithLabelValues(parentGVR2.String()))
	assert.Equal(t, float64(1), got2, "other parent GVR gauge should be unaffected")

	// The removed parent's labels should be cleaned up.
	assert.Equal(t, 1, testutil.CollectAndCount(metrics.DynInstanceWatchCount),
		"only one parent GVR label set should remain")
}
