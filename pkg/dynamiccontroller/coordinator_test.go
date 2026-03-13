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
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/metadata/fake"
	clienttesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

var (
	testParentGVR  = schema.GroupVersionResource{Group: "kro.run", Version: "v1alpha1", Resource: "webapps"}
	testDeployGVR  = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	testServiceGVR = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "services"}
	testCmGVR      = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}
)

// enqueueRecorder captures enqueue calls for assertions.
type enqueueRecorder struct {
	mu       sync.Mutex
	enqueued []struct {
		parentGVR schema.GroupVersionResource
		instance  types.NamespacedName
	}
}

func (r *enqueueRecorder) enqueue(parentGVR schema.GroupVersionResource, instance types.NamespacedName) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.enqueued = append(r.enqueued, struct {
		parentGVR schema.GroupVersionResource
		instance  types.NamespacedName
	}{parentGVR, instance})
}

func (r *enqueueRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.enqueued)
}

func (r *enqueueRecorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.enqueued = nil
}

// newTestCoordinator creates a coordinator backed by a real WatchManager
// (with a fake metadata client). The WatchManager's onEvent callback
// routes through the coordinator.
func newTestCoordinator(t *testing.T) (*WatchCoordinator, *enqueueRecorder) {
	t.Helper()
	log := zap.New(zap.UseDevMode(true))

	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddMetaToScheme(scheme))
	client := fake.NewSimpleMetadataClient(scheme)

	recorder := &enqueueRecorder{}

	// Create WatchManager with a placeholder onEvent; we'll wire the
	// coordinator's RouteEvent after construction.
	var coord *WatchCoordinator
	wm := NewWatchManager(client, 1*time.Hour, func(event Event) {
		if coord != nil {
			coord.RouteEvent(event)
		}
	}, log)

	coord = NewWatchCoordinator(wm, recorder.enqueue, log)
	return coord, recorder
}

func TestForInstance_ReturnsWatcher(t *testing.T) {
	coord, _ := newTestCoordinator(t)
	instance := types.NamespacedName{Name: "my-app", Namespace: "default"}

	watcher := coord.ForInstance(testParentGVR, instance)
	assert.NotNil(t, watcher)
}

func TestWatchAndDone_ScalarWatch(t *testing.T) {
	coord, recorder := newTestCoordinator(t)
	instance := types.NamespacedName{Name: "my-app", Namespace: "default"}

	watcher := coord.ForInstance(testParentGVR, instance)

	// Watch a deployment.
	err := watcher.Watch(WatchRequest{
		NodeID:    "deployment",
		GVR:       testDeployGVR,
		Name:      "my-deploy",
		Namespace: "default",
	})
	require.NoError(t, err)

	// Verify instance is tracked.
	assert.Equal(t, 1, coord.InstanceWatchCount())

	// Simulate an event matching the scalar watch.
	coord.RouteEvent(Event{
		Type:      EventUpdate,
		GVR:       testDeployGVR,
		Name:      "my-deploy",
		Namespace: "default",
	})
	assert.Equal(t, 1, recorder.count())

	// Non-matching event.
	coord.RouteEvent(Event{
		Type:      EventUpdate,
		GVR:       testDeployGVR,
		Name:      "other-deploy",
		Namespace: "default",
	})
	assert.Equal(t, 1, recorder.count())

	watcher.Done(true)
}

func TestWatchAndDone_CleanupStaleRequests(t *testing.T) {
	coord, recorder := newTestCoordinator(t)
	instance := types.NamespacedName{Name: "my-app", Namespace: "default"}

	// Cycle 1: watch deployment + service.
	w1 := coord.ForInstance(testParentGVR, instance)
	require.NoError(t, w1.Watch(WatchRequest{NodeID: "deployment", GVR: testDeployGVR, Name: "d1", Namespace: "default"}))
	require.NoError(t, w1.Watch(WatchRequest{NodeID: "service", GVR: testServiceGVR, Name: "s1", Namespace: "default"}))
	w1.Done(true)

	// Verify both match.
	coord.RouteEvent(Event{Type: EventUpdate, GVR: testDeployGVR, Name: "d1", Namespace: "default"})
	coord.RouteEvent(Event{Type: EventUpdate, GVR: testServiceGVR, Name: "s1", Namespace: "default"})
	assert.Equal(t, 2, recorder.count())
	recorder.reset()

	// Cycle 2: only watch deployment (service removed).
	w2 := coord.ForInstance(testParentGVR, instance)
	require.NoError(t, w2.Watch(WatchRequest{NodeID: "deployment", GVR: testDeployGVR, Name: "d1", Namespace: "default"}))
	w2.Done(true)

	// Deployment still matches, service no longer does.
	coord.RouteEvent(Event{Type: EventUpdate, GVR: testDeployGVR, Name: "d1", Namespace: "default"})
	assert.Equal(t, 1, recorder.count())
	recorder.reset()

	coord.RouteEvent(Event{Type: EventUpdate, GVR: testServiceGVR, Name: "s1", Namespace: "default"})
	assert.Equal(t, 0, recorder.count())
}

func TestCollectionWatch(t *testing.T) {
	coord, recorder := newTestCoordinator(t)
	instance := types.NamespacedName{Name: "my-app", Namespace: "default"}

	watcher := coord.ForInstance(testParentGVR, instance)
	selector, _ := labels.Parse("app=my-app")
	require.NoError(t, watcher.Watch(WatchRequest{
		NodeID:    "configs",
		GVR:       testCmGVR,
		Namespace: "default",
		Selector:  selector,
	}))
	watcher.Done(true)

	// Matching labels.
	coord.RouteEvent(Event{
		Type:      EventAdd,
		GVR:       testCmGVR,
		Name:      "config-1",
		Namespace: "default",
		Labels:    map[string]string{"app": "my-app"},
	})
	assert.Equal(t, 1, recorder.count())

	// Non-matching labels.
	coord.RouteEvent(Event{
		Type:      EventAdd,
		GVR:       testCmGVR,
		Name:      "config-2",
		Namespace: "default",
		Labels:    map[string]string{"app": "other"},
	})
	assert.Equal(t, 1, recorder.count())

	// Wrong namespace.
	coord.RouteEvent(Event{
		Type:      EventAdd,
		GVR:       testCmGVR,
		Name:      "config-3",
		Namespace: "other-ns",
		Labels:    map[string]string{"app": "my-app"},
	})
	assert.Equal(t, 1, recorder.count())
}

func TestRemoveInstance(t *testing.T) {
	coord, recorder := newTestCoordinator(t)
	instance := types.NamespacedName{Name: "my-app", Namespace: "default"}

	watcher := coord.ForInstance(testParentGVR, instance)
	require.NoError(t, watcher.Watch(WatchRequest{NodeID: "deployment", GVR: testDeployGVR, Name: "d1", Namespace: "default"}))
	watcher.Done(true)

	// Verify match.
	coord.RouteEvent(Event{Type: EventUpdate, GVR: testDeployGVR, Name: "d1", Namespace: "default"})
	assert.Equal(t, 1, recorder.count())
	recorder.reset()

	// Remove instance.
	coord.RemoveInstance(testParentGVR, instance)
	assert.Equal(t, 0, coord.InstanceWatchCount())

	// No longer matches.
	coord.RouteEvent(Event{Type: EventUpdate, GVR: testDeployGVR, Name: "d1", Namespace: "default"})
	assert.Equal(t, 0, recorder.count())
}

func TestRemoveParentGVR(t *testing.T) {
	coord, recorder := newTestCoordinator(t)
	inst1 := types.NamespacedName{Name: "app1", Namespace: "default"}
	inst2 := types.NamespacedName{Name: "app2", Namespace: "default"}

	// Two instances watching deployments.
	w1 := coord.ForInstance(testParentGVR, inst1)
	require.NoError(t, w1.Watch(WatchRequest{NodeID: "deployment", GVR: testDeployGVR, Name: "d1", Namespace: "default"}))
	w1.Done(true)

	w2 := coord.ForInstance(testParentGVR, inst2)
	require.NoError(t, w2.Watch(WatchRequest{NodeID: "deployment", GVR: testDeployGVR, Name: "d2", Namespace: "default"}))
	w2.Done(true)

	assert.Equal(t, 2, coord.InstanceWatchCount())

	// Remove all instances for testParentGVR.
	coord.RemoveParentGVR(testParentGVR)
	assert.Equal(t, 0, coord.InstanceWatchCount())

	// No more matches.
	coord.RouteEvent(Event{Type: EventUpdate, GVR: testDeployGVR, Name: "d1", Namespace: "default"})
	coord.RouteEvent(Event{Type: EventUpdate, GVR: testDeployGVR, Name: "d2", Namespace: "default"})
	assert.Equal(t, 0, recorder.count())
}

func TestSharedWatchAcrossInstances(t *testing.T) {
	coord, recorder := newTestCoordinator(t)
	inst1 := types.NamespacedName{Name: "app1", Namespace: "default"}
	inst2 := types.NamespacedName{Name: "app2", Namespace: "default"}

	// Both instances watch the same shared configmap.
	w1 := coord.ForInstance(testParentGVR, inst1)
	require.NoError(t, w1.Watch(WatchRequest{NodeID: "config", GVR: testCmGVR, Name: "shared-cm", Namespace: "default"}))
	w1.Done(true)

	w2 := coord.ForInstance(testParentGVR, inst2)
	require.NoError(t, w2.Watch(WatchRequest{NodeID: "config", GVR: testCmGVR, Name: "shared-cm", Namespace: "default"}))
	w2.Done(true)

	// One event should trigger BOTH instances.
	coord.RouteEvent(Event{Type: EventUpdate, GVR: testCmGVR, Name: "shared-cm", Namespace: "default"})
	assert.Equal(t, 2, recorder.count())
}

func TestStopOrphanedWatch_RemoveInstance(t *testing.T) {
	coord, _ := newTestCoordinator(t)
	instance := types.NamespacedName{Name: "my-app", Namespace: "default"}

	watcher := coord.ForInstance(testParentGVR, instance)
	require.NoError(t, watcher.Watch(WatchRequest{NodeID: "deployment", GVR: testDeployGVR, Name: "d1", Namespace: "default"}))
	watcher.Done(true)

	// Informer should be running.
	assert.Equal(t, 1, coord.watches.ActiveWatchCount(), "expected 1 active watch for deployment GVR")

	// Remove the only instance — informer should stop.
	coord.RemoveInstance(testParentGVR, instance)
	assert.Equal(t, 0, coord.watches.ActiveWatchCount(), "expected 0 active watches after removing last requestor")

	// ensureWatch can re-create it.
	assert.NoError(t, coord.watches.ensureWatch(testDeployGVR))
	assert.Equal(t, 1, coord.watches.ActiveWatchCount(), "expected watch to be re-created after ensureWatch")
}

func TestStopOrphanedWatch_DoneCleanup(t *testing.T) {
	coord, _ := newTestCoordinator(t)
	instance := types.NamespacedName{Name: "my-app", Namespace: "default"}

	// Cycle 1: watch deployment + service.
	w1 := coord.ForInstance(testParentGVR, instance)
	require.NoError(t, w1.Watch(WatchRequest{NodeID: "deployment", GVR: testDeployGVR, Name: "d1", Namespace: "default"}))
	require.NoError(t, w1.Watch(WatchRequest{NodeID: "service", GVR: testServiceGVR, Name: "s1", Namespace: "default"}))
	w1.Done(true)

	assert.Equal(t, 2, coord.watches.ActiveWatchCount(), "expected 2 active watches after cycle 1")

	// Cycle 2: only watch deployment — service should be cleaned up.
	w2 := coord.ForInstance(testParentGVR, instance)
	require.NoError(t, w2.Watch(WatchRequest{NodeID: "deployment", GVR: testDeployGVR, Name: "d1", Namespace: "default"}))
	w2.Done(true)

	assert.Equal(t, 1, coord.watches.ActiveWatchCount(), "expected service watch to be stopped after cycle 2")

	// The remaining watch should be for deployments.
	assert.NotNil(t, coord.watches.GetInformer(testDeployGVR), "deployment informer should still be running")
	assert.Nil(t, coord.watches.GetInformer(testServiceGVR), "service informer should have been stopped")
}

func TestStopOrphanedWatch_RemoveParentGVR(t *testing.T) {
	coord, _ := newTestCoordinator(t)
	inst1 := types.NamespacedName{Name: "app1", Namespace: "default"}
	inst2 := types.NamespacedName{Name: "app2", Namespace: "default"}

	w1 := coord.ForInstance(testParentGVR, inst1)
	require.NoError(t, w1.Watch(WatchRequest{NodeID: "deployment", GVR: testDeployGVR, Name: "d1", Namespace: "default"}))
	w1.Done(true)

	w2 := coord.ForInstance(testParentGVR, inst2)
	require.NoError(t, w2.Watch(WatchRequest{NodeID: "deployment", GVR: testDeployGVR, Name: "d2", Namespace: "default"}))
	w2.Done(true)

	assert.Equal(t, 1, coord.watches.ActiveWatchCount(), "expected 1 active watch (shared deployment GVR)")

	// Remove all instances for the parent — deployment watch should stop.
	coord.RemoveParentGVR(testParentGVR)
	assert.Equal(t, 0, coord.watches.ActiveWatchCount(), "expected 0 active watches after removing parent GVR")
}

func TestStopOrphanedWatch_SharedGVRNotStopped(t *testing.T) {
	coord, _ := newTestCoordinator(t)
	parentGVR2 := schema.GroupVersionResource{Group: "kro.run", Version: "v1alpha1", Resource: "databases"}
	inst1 := types.NamespacedName{Name: "app1", Namespace: "default"}
	inst2 := types.NamespacedName{Name: "db1", Namespace: "default"}

	// Instance 1 (parent1) watches deployments.
	w1 := coord.ForInstance(testParentGVR, inst1)
	require.NoError(t, w1.Watch(WatchRequest{NodeID: "deployment", GVR: testDeployGVR, Name: "d1", Namespace: "default"}))
	w1.Done(true)

	// Instance 2 (parent2) also watches deployments.
	w2 := coord.ForInstance(parentGVR2, inst2)
	require.NoError(t, w2.Watch(WatchRequest{NodeID: "deployment", GVR: testDeployGVR, Name: "d2", Namespace: "default"}))
	w2.Done(true)

	assert.Equal(t, 1, coord.watches.ActiveWatchCount(), "expected 1 shared deployment watch")

	// Remove only inst1 — deployment watch should NOT stop because inst2 still uses it.
	coord.RemoveInstance(testParentGVR, inst1)
	assert.Equal(t, 1, coord.watches.ActiveWatchCount(), "expected deployment watch to survive — still has requestors")

	// Remove inst2 — now it should stop.
	coord.RemoveInstance(parentGVR2, inst2)
	assert.Equal(t, 0, coord.watches.ActiveWatchCount(), "expected deployment watch to stop — no more requestors")
}

func TestStopOrphanedWatch_CollectionWatch(t *testing.T) {
	coord, _ := newTestCoordinator(t)
	instance := types.NamespacedName{Name: "my-app", Namespace: "default"}

	watcher := coord.ForInstance(testParentGVR, instance)
	selector, _ := labels.Parse("app=my-app")
	require.NoError(t, watcher.Watch(WatchRequest{
		NodeID:    "configs",
		GVR:       testCmGVR,
		Namespace: "default",
		Selector:  selector,
	}))
	watcher.Done(true)

	assert.Equal(t, 1, coord.watches.ActiveWatchCount(), "expected 1 active watch for collection GVR")

	// Remove the only instance — collection watch should be stopped.
	coord.RemoveInstance(testParentGVR, instance)
	assert.Equal(t, 0, coord.watches.ActiveWatchCount(), "expected 0 active watches after removing last collection requestor")
}

func TestCollectionWatch_DoneCleanup(t *testing.T) {
	coord, _ := newTestCoordinator(t)
	instance := types.NamespacedName{Name: "my-app", Namespace: "default"}

	// Cycle 1: collection watch on configmaps + scalar watch on deployments.
	w1 := coord.ForInstance(testParentGVR, instance)
	selector, _ := labels.Parse("app=my-app")
	require.NoError(t, w1.Watch(WatchRequest{
		NodeID:    "configs",
		GVR:       testCmGVR,
		Namespace: "default",
		Selector:  selector,
	}))
	require.NoError(t, w1.Watch(WatchRequest{
		NodeID:    "deployment",
		GVR:       testDeployGVR,
		Name:      "d1",
		Namespace: "default",
	}))
	w1.Done(true)

	assert.Equal(t, 2, coord.watches.ActiveWatchCount(), "expected 2 active watches after cycle 1")

	// Cycle 2: only scalar watch on deployments — collection watch should be cleaned up.
	w2 := coord.ForInstance(testParentGVR, instance)
	require.NoError(t, w2.Watch(WatchRequest{
		NodeID:    "deployment",
		GVR:       testDeployGVR,
		Name:      "d1",
		Namespace: "default",
	}))
	w2.Done(true)

	assert.Nil(t, coord.watches.GetInformer(testCmGVR), "collection configmap informer should have been stopped")
	assert.NotNil(t, coord.watches.GetInformer(testDeployGVR), "deployment informer should still be running")
}

func TestNodeIDReuse_ChangesTargetGVR(t *testing.T) {
	coord, _ := newTestCoordinator(t)
	instance := types.NamespacedName{Name: "my-app", Namespace: "default"}

	// Cycle 1: nodeID "resource" → testDeployGVR.
	w1 := coord.ForInstance(testParentGVR, instance)
	require.NoError(t, w1.Watch(WatchRequest{
		NodeID:    "resource",
		GVR:       testDeployGVR,
		Name:      "d1",
		Namespace: "default",
	}))
	w1.Done(true)

	assert.NotNil(t, coord.watches.GetInformer(testDeployGVR), "deployment informer should be running after cycle 1")

	// Cycle 2: same nodeID "resource" → testServiceGVR (different GVR).
	w2 := coord.ForInstance(testParentGVR, instance)
	require.NoError(t, w2.Watch(WatchRequest{
		NodeID:    "resource",
		GVR:       testServiceGVR,
		Name:      "s1",
		Namespace: "default",
	}))
	w2.Done(true)

	assert.Nil(t, coord.watches.GetInformer(testDeployGVR), "old deployment informer should be stopped after nodeID reuse")
	assert.NotNil(t, coord.watches.GetInformer(testServiceGVR), "new service informer should be running")
}

func TestMixedScalarAndCollection_RemoveParentGVR(t *testing.T) {
	coord, _ := newTestCoordinator(t)
	inst1 := types.NamespacedName{Name: "app1", Namespace: "default"}
	inst2 := types.NamespacedName{Name: "app2", Namespace: "default"}

	// Instance 1: scalar watch on deployments.
	w1 := coord.ForInstance(testParentGVR, inst1)
	require.NoError(t, w1.Watch(WatchRequest{
		NodeID:    "deployment",
		GVR:       testDeployGVR,
		Name:      "d1",
		Namespace: "default",
	}))
	w1.Done(true)

	// Instance 2: collection watch on configmaps.
	w2 := coord.ForInstance(testParentGVR, inst2)
	selector, _ := labels.Parse("app=my-app")
	require.NoError(t, w2.Watch(WatchRequest{
		NodeID:    "configs",
		GVR:       testCmGVR,
		Namespace: "default",
		Selector:  selector,
	}))
	w2.Done(true)

	assert.Equal(t, 2, coord.watches.ActiveWatchCount(), "expected 2 active watches (deployment + configmap)")

	// Remove all instances for testParentGVR — both watches should stop.
	coord.RemoveParentGVR(testParentGVR)
	assert.Equal(t, 0, coord.watches.ActiveWatchCount(), "expected 0 active watches after removing parent GVR")
}

func TestDuplicateCollectionRegistration_Idempotent(t *testing.T) {
	coord, recorder := newTestCoordinator(t)
	instance := types.NamespacedName{Name: "my-app", Namespace: "default"}

	selector, _ := labels.Parse("app=my-app")

	// Register the same collection watch twice in the same cycle.
	watcher := coord.ForInstance(testParentGVR, instance)
	require.NoError(t, watcher.Watch(WatchRequest{
		NodeID:    "configs",
		GVR:       testCmGVR,
		Namespace: "default",
		Selector:  selector,
	}))
	require.NoError(t, watcher.Watch(WatchRequest{
		NodeID:    "configs",
		GVR:       testCmGVR,
		Namespace: "default",
		Selector:  selector,
	}))
	watcher.Done(true)

	// Should only have 1 collection entry, not 2.
	_, collection := coord.WatchRequestCount()
	assert.Equal(t, 1, collection, "duplicate collection registration should be deduped")

	// One matching event should trigger exactly one enqueue, not two.
	coord.RouteEvent(Event{
		Type:      EventAdd,
		GVR:       testCmGVR,
		Name:      "config-1",
		Namespace: "default",
		Labels:    map[string]string{"app": "my-app"},
	})
	assert.Equal(t, 1, recorder.count(), "should enqueue once, not twice")
}

func TestScalarAndCollectionDedup(t *testing.T) {
	coord, recorder := newTestCoordinator(t)
	instance := types.NamespacedName{Name: "my-app", Namespace: "default"}

	// Instance watches a configmap both as a scalar and via a collection selector
	// that would also match it.
	watcher := coord.ForInstance(testParentGVR, instance)
	require.NoError(t, watcher.Watch(WatchRequest{
		NodeID:    "specific-cm",
		GVR:       testCmGVR,
		Name:      "config-1",
		Namespace: "default",
	}))
	selector, _ := labels.Parse("app=my-app")
	require.NoError(t, watcher.Watch(WatchRequest{
		NodeID:    "all-configs",
		GVR:       testCmGVR,
		Namespace: "default",
		Selector:  selector,
	}))
	watcher.Done(true)

	// An event matching both scalar and collection should enqueue only once.
	coord.RouteEvent(Event{
		Type:      EventUpdate,
		GVR:       testCmGVR,
		Name:      "config-1",
		Namespace: "default",
		Labels:    map[string]string{"app": "my-app"},
	})
	assert.Equal(t, 1, recorder.count(), "instance matching both scalar and collection should be enqueued once")
}

func TestCollectionWatch_SelectorChange(t *testing.T) {
	coord, recorder := newTestCoordinator(t)
	instance := types.NamespacedName{Name: "my-app", Namespace: "default"}

	// Cycle 1: collection watch with selector app=v1.
	w1 := coord.ForInstance(testParentGVR, instance)
	selectorV1, _ := labels.Parse("app=v1")
	require.NoError(t, w1.Watch(WatchRequest{
		NodeID:    "configs",
		GVR:       testCmGVR,
		Namespace: "default",
		Selector:  selectorV1,
	}))
	w1.Done(true)

	// Cycle 2: same nodeID, different selector (app=v2).
	w2 := coord.ForInstance(testParentGVR, instance)
	selectorV2, _ := labels.Parse("app=v2")
	require.NoError(t, w2.Watch(WatchRequest{
		NodeID:    "configs",
		GVR:       testCmGVR,
		Namespace: "default",
		Selector:  selectorV2,
	}))
	w2.Done(true)

	// Old selector should not match.
	coord.RouteEvent(Event{
		Type:      EventAdd,
		GVR:       testCmGVR,
		Name:      "config-1",
		Namespace: "default",
		Labels:    map[string]string{"app": "v1"},
	})
	assert.Equal(t, 0, recorder.count(), "old selector should not match after change")

	// New selector should match.
	coord.RouteEvent(Event{
		Type:      EventAdd,
		GVR:       testCmGVR,
		Name:      "config-2",
		Namespace: "default",
		Labels:    map[string]string{"app": "v2"},
	})
	assert.Equal(t, 1, recorder.count(), "new selector should match")

	// Only 1 collection entry should exist.
	_, collection := coord.WatchRequestCount()
	assert.Equal(t, 1, collection, "should have exactly 1 collection entry after selector change")
}

func TestRouteEvent_OldLabelsMatch(t *testing.T) {
	coord, recorder := newTestCoordinator(t)
	instance := types.NamespacedName{Name: "my-app", Namespace: "default"}

	watcher := coord.ForInstance(testParentGVR, instance)
	selector, _ := labels.Parse("team=alpha")
	require.NoError(t, watcher.Watch(WatchRequest{
		NodeID:    "configs",
		GVR:       testCmGVR,
		Namespace: "default",
		Selector:  selector,
	}))
	watcher.Done(true)

	// Update event: new labels no longer match, but old labels did.
	// Should still trigger re-reconciliation (label-loss detection).
	coord.RouteEvent(Event{
		Type:      EventUpdate,
		GVR:       testCmGVR,
		Name:      "config-1",
		Namespace: "default",
		Labels:    map[string]string{"team": "beta"},
		OldLabels: map[string]string{"team": "alpha"},
	})
	assert.Equal(t, 1, recorder.count(), "should enqueue when old labels matched selector (label-loss)")
}

func TestWatchRequestCount(t *testing.T) {
	coord, _ := newTestCoordinator(t)
	instance := types.NamespacedName{Name: "my-app", Namespace: "default"}

	watcher := coord.ForInstance(testParentGVR, instance)
	require.NoError(t, watcher.Watch(WatchRequest{NodeID: "deploy", GVR: testDeployGVR, Name: "d1", Namespace: "default"}))
	selector, _ := labels.Parse("app=test")
	require.NoError(t, watcher.Watch(WatchRequest{NodeID: "configs", GVR: testCmGVR, Namespace: "default", Selector: selector}))
	watcher.Done(true)

	scalar, collection := coord.WatchRequestCount()
	assert.Equal(t, 1, scalar)
	assert.Equal(t, 1, collection)
}

func TestNoopInstanceWatcher(t *testing.T) {
	noop := NoopInstanceWatcher{}

	// Watch should return nil.
	err := noop.Watch(WatchRequest{
		NodeID:    "test",
		GVR:       testDeployGVR,
		Name:      "d1",
		Namespace: "default",
	})
	assert.NoError(t, err)

	// Done should not panic.
	noop.Done(true)
	noop.Done(false)
}

func TestRemoveInstance_NonExistent(t *testing.T) {
	coord, _ := newTestCoordinator(t)

	// Removing a non-existent instance should not panic.
	coord.RemoveInstance(testParentGVR, types.NamespacedName{Name: "nonexistent", Namespace: "default"})
	assert.Equal(t, 0, coord.InstanceWatchCount())
}

func TestDoneInstance_NoState(t *testing.T) {
	coord, _ := newTestCoordinator(t)

	// Calling Done on a watcher that never called Watch should not panic.
	watcher := coord.ForInstance(testParentGVR, types.NamespacedName{Name: "empty", Namespace: "default"})
	watcher.Done(true)
	assert.Equal(t, 0, coord.InstanceWatchCount())
}

func TestAbortInstance_RollsBackFailedCycleTargetChange(t *testing.T) {
	coord, recorder := newTestCoordinator(t)
	instance := types.NamespacedName{Name: "my-app", Namespace: "default"}

	w1 := coord.ForInstance(testParentGVR, instance)
	require.NoError(t, w1.Watch(WatchRequest{
		NodeID:    "dependency",
		GVR:       testDeployGVR,
		Name:      "d1",
		Namespace: "default",
	}))
	w1.Done(true)

	w2 := coord.ForInstance(testParentGVR, instance)
	require.NoError(t, w2.Watch(WatchRequest{
		NodeID:    "dependency",
		GVR:       testServiceGVR,
		Name:      "s1",
		Namespace: "default",
	}))
	w2.Done(false)

	coord.RouteEvent(Event{Type: EventUpdate, GVR: testServiceGVR, Name: "s1", Namespace: "default"})
	assert.Equal(t, 0, recorder.count(), "aborted target change should not stay active")

	coord.RouteEvent(Event{Type: EventUpdate, GVR: testDeployGVR, Name: "d1", Namespace: "default"})
	assert.Equal(t, 1, recorder.count(), "previous committed watch should remain active after abort")

	assert.NotNil(t, coord.watches.GetInformer(testDeployGVR), "previous informer should stay running")
	assert.Nil(t, coord.watches.GetInformer(testServiceGVR), "orphaned failed-cycle informer should be stopped")

	scalar, collection := coord.WatchRequestCount()
	assert.Equal(t, 1, scalar)
	assert.Equal(t, 0, collection)
}

func TestAbortInstance_SameTargetKeepsPreviousWatch(t *testing.T) {
	coord, recorder := newTestCoordinator(t)
	instance := types.NamespacedName{Name: "my-app", Namespace: "default"}

	w1 := coord.ForInstance(testParentGVR, instance)
	require.NoError(t, w1.Watch(WatchRequest{
		NodeID:    "dependency",
		GVR:       testDeployGVR,
		Name:      "d1",
		Namespace: "default",
	}))
	w1.Done(true)

	w2 := coord.ForInstance(testParentGVR, instance)
	require.NoError(t, w2.Watch(WatchRequest{
		NodeID:    "dependency",
		GVR:       testDeployGVR,
		Name:      "d1",
		Namespace: "default",
	}))
	w2.Done(false)

	coord.RouteEvent(Event{Type: EventUpdate, GVR: testDeployGVR, Name: "d1", Namespace: "default"})
	assert.Equal(t, 1, recorder.count(), "aborting an unchanged request must not drop the previous watch")

	scalar, collection := coord.WatchRequestCount()
	assert.Equal(t, 1, scalar)
	assert.Equal(t, 0, collection)
}

func TestAbortInstance_DoesNotPromoteFailedCurrentOnLaterSuccess(t *testing.T) {
	coord, recorder := newTestCoordinator(t)
	instance := types.NamespacedName{Name: "my-app", Namespace: "default"}

	w1 := coord.ForInstance(testParentGVR, instance)
	require.NoError(t, w1.Watch(WatchRequest{
		NodeID:    "dependency",
		GVR:       testDeployGVR,
		Name:      "d1",
		Namespace: "default",
	}))
	w1.Done(true)

	w2 := coord.ForInstance(testParentGVR, instance)
	require.NoError(t, w2.Watch(WatchRequest{
		NodeID:    "dependency",
		GVR:       testServiceGVR,
		Name:      "s1",
		Namespace: "default",
	}))
	w2.Done(false)

	w3 := coord.ForInstance(testParentGVR, instance)
	require.NoError(t, w3.Watch(WatchRequest{
		NodeID:    "dependency",
		GVR:       testDeployGVR,
		Name:      "d1",
		Namespace: "default",
	}))
	w3.Done(true)

	coord.RouteEvent(Event{Type: EventUpdate, GVR: testServiceGVR, Name: "s1", Namespace: "default"})
	assert.Equal(t, 0, recorder.count(), "aborted requests must not be committed by a later successful Done")

	coord.RouteEvent(Event{Type: EventUpdate, GVR: testDeployGVR, Name: "d1", Namespace: "default"})
	assert.Equal(t, 1, recorder.count(), "successful cycle should keep only the committed watch")
}

func TestAbortInstance_RollsBackCollectionSelectorChange(t *testing.T) {
	coord, recorder := newTestCoordinator(t)
	instance := types.NamespacedName{Name: "my-app", Namespace: "default"}

	selectorV1, err := labels.Parse("app=v1")
	require.NoError(t, err)
	selectorV2, err := labels.Parse("app=v2")
	require.NoError(t, err)

	w1 := coord.ForInstance(testParentGVR, instance)
	require.NoError(t, w1.Watch(WatchRequest{
		NodeID:    "configs",
		GVR:       testCmGVR,
		Namespace: "default",
		Selector:  selectorV1,
	}))
	w1.Done(true)

	w2 := coord.ForInstance(testParentGVR, instance)
	require.NoError(t, w2.Watch(WatchRequest{
		NodeID:    "configs",
		GVR:       testCmGVR,
		Namespace: "default",
		Selector:  selectorV2,
	}))
	w2.Done(false)

	coord.RouteEvent(Event{
		Type:      EventUpdate,
		GVR:       testCmGVR,
		Name:      "cm-v2",
		Namespace: "default",
		Labels:    map[string]string{"app": "v2"},
	})
	assert.Equal(t, 0, recorder.count(), "aborted selector change should not stay active")

	coord.RouteEvent(Event{
		Type:      EventUpdate,
		GVR:       testCmGVR,
		Name:      "cm-v1",
		Namespace: "default",
		Labels:    map[string]string{"app": "v1"},
	})
	assert.Equal(t, 1, recorder.count(), "previous committed selector should remain active after abort")
}

func TestAddWatch_EnsureWatchSyncError(t *testing.T) {
	log := zap.New(zap.UseDevMode(true))

	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddMetaToScheme(scheme))
	client := fake.NewSimpleMetadataClient(scheme)
	// Fail all list calls.
	client.PrependReactor("list", "*", func(action clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("simulated list error")
	})

	recorder := &enqueueRecorder{}
	wm := NewWatchManager(client, 1*time.Hour, func(event Event) {}, log)
	wm.SyncTimeout = 100 * time.Millisecond

	coord := NewWatchCoordinator(wm, recorder.enqueue, log)
	instance := types.NamespacedName{Name: "my-app", Namespace: "default"}

	watcher := coord.ForInstance(testParentGVR, instance)

	// ensureWatch will fail sync but addWatch logs and returns nil.
	err := watcher.Watch(WatchRequest{
		NodeID:    "deploy",
		GVR:       testDeployGVR,
		Name:      "d1",
		Namespace: "default",
	})
	assert.NoError(t, err) // addWatch does not propagate ensureWatch errors

	wm.Shutdown()
}

func TestRemoveInstance_ScalarIndexCleanup(t *testing.T) {
	coord, _ := newTestCoordinator(t)
	instance := types.NamespacedName{Name: "my-app", Namespace: "default"}

	// Watch, Done, then remove.
	watcher := coord.ForInstance(testParentGVR, instance)
	require.NoError(t, watcher.Watch(WatchRequest{NodeID: "deploy", GVR: testDeployGVR, Name: "d1", Namespace: "default"}))
	// Don't call Done — tests that RemoveInstance cleans up current entries too.
	coord.RemoveInstance(testParentGVR, instance)

	assert.Equal(t, 0, coord.InstanceWatchCount())
	scalar, collection := coord.WatchRequestCount()
	assert.Equal(t, 0, scalar)
	assert.Equal(t, 0, collection)
}

func TestRemoveParentGVR_NonExistent(t *testing.T) {
	coord, _ := newTestCoordinator(t)
	nonExistentGVR := schema.GroupVersionResource{Group: "fake", Version: "v1", Resource: "fakes"}

	// Should not panic.
	coord.RemoveParentGVR(nonExistentGVR)
	assert.Equal(t, 0, coord.InstanceWatchCount())
}
