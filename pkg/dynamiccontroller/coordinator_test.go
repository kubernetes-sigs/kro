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

	watcher.Done()
}

func TestWatchAndDone_CleanupStaleRequests(t *testing.T) {
	coord, recorder := newTestCoordinator(t)
	instance := types.NamespacedName{Name: "my-app", Namespace: "default"}

	// Cycle 1: watch deployment + service.
	w1 := coord.ForInstance(testParentGVR, instance)
	require.NoError(t, w1.Watch(WatchRequest{NodeID: "deployment", GVR: testDeployGVR, Name: "d1", Namespace: "default"}))
	require.NoError(t, w1.Watch(WatchRequest{NodeID: "service", GVR: testServiceGVR, Name: "s1", Namespace: "default"}))
	w1.Done()

	// Verify both match.
	coord.RouteEvent(Event{Type: EventUpdate, GVR: testDeployGVR, Name: "d1", Namespace: "default"})
	coord.RouteEvent(Event{Type: EventUpdate, GVR: testServiceGVR, Name: "s1", Namespace: "default"})
	assert.Equal(t, 2, recorder.count())
	recorder.reset()

	// Cycle 2: only watch deployment (service removed).
	w2 := coord.ForInstance(testParentGVR, instance)
	require.NoError(t, w2.Watch(WatchRequest{NodeID: "deployment", GVR: testDeployGVR, Name: "d1", Namespace: "default"}))
	w2.Done()

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
	watcher.Done()

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
	watcher.Done()

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
	w1.Done()

	w2 := coord.ForInstance(testParentGVR, inst2)
	require.NoError(t, w2.Watch(WatchRequest{NodeID: "deployment", GVR: testDeployGVR, Name: "d2", Namespace: "default"}))
	w2.Done()

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
	w1.Done()

	w2 := coord.ForInstance(testParentGVR, inst2)
	require.NoError(t, w2.Watch(WatchRequest{NodeID: "config", GVR: testCmGVR, Name: "shared-cm", Namespace: "default"}))
	w2.Done()

	// One event should trigger BOTH instances.
	coord.RouteEvent(Event{Type: EventUpdate, GVR: testCmGVR, Name: "shared-cm", Namespace: "default"})
	assert.Equal(t, 2, recorder.count())
}

func TestStopOrphanedWatch_RemoveInstance(t *testing.T) {
	coord, _ := newTestCoordinator(t)
	instance := types.NamespacedName{Name: "my-app", Namespace: "default"}

	watcher := coord.ForInstance(testParentGVR, instance)
	require.NoError(t, watcher.Watch(WatchRequest{NodeID: "deployment", GVR: testDeployGVR, Name: "d1", Namespace: "default"}))
	watcher.Done()

	// Informer should be running.
	assert.Equal(t, 1, coord.watches.ActiveWatchCount(), "expected 1 active watch for deployment GVR")

	// Remove the only instance — informer should stop.
	coord.RemoveInstance(testParentGVR, instance)
	assert.Equal(t, 0, coord.watches.ActiveWatchCount(), "expected 0 active watches after removing last requestor")

	// EnsureWatch can re-create it.
	coord.watches.EnsureWatch(testDeployGVR)
	assert.Equal(t, 1, coord.watches.ActiveWatchCount(), "expected watch to be re-created after EnsureWatch")
}

func TestStopOrphanedWatch_DoneCleanup(t *testing.T) {
	coord, _ := newTestCoordinator(t)
	instance := types.NamespacedName{Name: "my-app", Namespace: "default"}

	// Cycle 1: watch deployment + service.
	w1 := coord.ForInstance(testParentGVR, instance)
	require.NoError(t, w1.Watch(WatchRequest{NodeID: "deployment", GVR: testDeployGVR, Name: "d1", Namespace: "default"}))
	require.NoError(t, w1.Watch(WatchRequest{NodeID: "service", GVR: testServiceGVR, Name: "s1", Namespace: "default"}))
	w1.Done()

	assert.Equal(t, 2, coord.watches.ActiveWatchCount(), "expected 2 active watches after cycle 1")

	// Cycle 2: only watch deployment — service should be cleaned up.
	w2 := coord.ForInstance(testParentGVR, instance)
	require.NoError(t, w2.Watch(WatchRequest{NodeID: "deployment", GVR: testDeployGVR, Name: "d1", Namespace: "default"}))
	w2.Done()

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
	w1.Done()

	w2 := coord.ForInstance(testParentGVR, inst2)
	require.NoError(t, w2.Watch(WatchRequest{NodeID: "deployment", GVR: testDeployGVR, Name: "d2", Namespace: "default"}))
	w2.Done()

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
	w1.Done()

	// Instance 2 (parent2) also watches deployments.
	w2 := coord.ForInstance(parentGVR2, inst2)
	require.NoError(t, w2.Watch(WatchRequest{NodeID: "deployment", GVR: testDeployGVR, Name: "d2", Namespace: "default"}))
	w2.Done()

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
	watcher.Done()

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
	w1.Done()

	assert.Equal(t, 2, coord.watches.ActiveWatchCount(), "expected 2 active watches after cycle 1")

	// Cycle 2: only scalar watch on deployments — collection watch should be cleaned up.
	w2 := coord.ForInstance(testParentGVR, instance)
	require.NoError(t, w2.Watch(WatchRequest{
		NodeID:    "deployment",
		GVR:       testDeployGVR,
		Name:      "d1",
		Namespace: "default",
	}))
	w2.Done()

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
	w1.Done()

	assert.NotNil(t, coord.watches.GetInformer(testDeployGVR), "deployment informer should be running after cycle 1")

	// Cycle 2: same nodeID "resource" → testServiceGVR (different GVR).
	w2 := coord.ForInstance(testParentGVR, instance)
	require.NoError(t, w2.Watch(WatchRequest{
		NodeID:    "resource",
		GVR:       testServiceGVR,
		Name:      "s1",
		Namespace: "default",
	}))
	w2.Done()

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
	w1.Done()

	// Instance 2: collection watch on configmaps.
	w2 := coord.ForInstance(testParentGVR, inst2)
	selector, _ := labels.Parse("app=my-app")
	require.NoError(t, w2.Watch(WatchRequest{
		NodeID:    "configs",
		GVR:       testCmGVR,
		Namespace: "default",
		Selector:  selector,
	}))
	w2.Done()

	assert.Equal(t, 2, coord.watches.ActiveWatchCount(), "expected 2 active watches (deployment + configmap)")

	// Remove all instances for testParentGVR — both watches should stop.
	coord.RemoveParentGVR(testParentGVR)
	assert.Equal(t, 0, coord.watches.ActiveWatchCount(), "expected 0 active watches after removing parent GVR")
}

func TestWatchRequestCount(t *testing.T) {
	coord, _ := newTestCoordinator(t)
	instance := types.NamespacedName{Name: "my-app", Namespace: "default"}

	watcher := coord.ForInstance(testParentGVR, instance)
	require.NoError(t, watcher.Watch(WatchRequest{NodeID: "deploy", GVR: testDeployGVR, Name: "d1", Namespace: "default"}))
	selector, _ := labels.Parse("app=test")
	require.NoError(t, watcher.Watch(WatchRequest{NodeID: "configs", GVR: testCmGVR, Namespace: "default", Selector: selector}))
	watcher.Done()

	scalar, collection := coord.WatchRequestCount()
	assert.Equal(t, 1, scalar)
	assert.Equal(t, 1, collection)
}
