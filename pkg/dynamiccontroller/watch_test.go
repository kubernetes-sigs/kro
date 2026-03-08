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
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/metadata/fake"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
)

func newTestWatchManager(t *testing.T) *WatchManager {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1.AddMetaToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	client := fake.NewSimpleMetadataClient(scheme)
	return NewWatchManager(client, 1*time.Hour, func(Event) {}, noopLogger())
}

func TestRelease_Idempotent(t *testing.T) {
	wm := newTestWatchManager(t)
	gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}

	_, err := wm.Acquire(gvr, "test")
	assert.NoError(t, err)
	assert.Equal(t, 1, wm.ActiveWatchCount())

	wm.Release(gvr, "test")
	assert.Equal(t, 0, wm.ActiveWatchCount())

	// Second Release should not panic and count stays 0.
	wm.Release(gvr, "test")
	assert.Equal(t, 0, wm.ActiveWatchCount())
}

func TestRelease_ThenAcquire_CreatesFresh(t *testing.T) {
	wm := newTestWatchManager(t)
	gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}

	_, err := wm.Acquire(gvr, "test")
	assert.NoError(t, err)
	inf1 := wm.GetInformer(gvr)
	assert.NotNil(t, inf1)

	wm.Release(gvr, "test")
	assert.Nil(t, wm.GetInformer(gvr))

	_, err = wm.Acquire(gvr, "test")
	assert.NoError(t, err)
	inf2 := wm.GetInformer(gvr)
	assert.NotNil(t, inf2)

	// Must be a new informer instance, not the old one.
	assert.NotSame(t, inf1, inf2, "expected fresh informer after Release + Acquire")
}

func TestDeleteFunc_Tombstone(t *testing.T) {
	gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}

	var received []Event
	wm := NewWatchManager(
		fake.NewSimpleMetadataClient(runtime.NewScheme()),
		1*time.Hour,
		func(e Event) { received = append(received, e) },
		noopLogger(),
	)

	// Create a gvrWatch and get its event handler.
	w := wm.newWatch(gvr)

	// Simulate a tombstone (DeletedFinalStateUnknown wrapping a PartialObjectMetadata).
	obj := &v1.PartialObjectMetadata{
		ObjectMeta: v1.ObjectMeta{
			Name:      "my-deploy",
			Namespace: "default",
			Labels:    map[string]string{"app": "test"},
		},
	}
	tombstone := cache.DeletedFinalStateUnknown{
		Key: "default/my-deploy",
		Obj: obj,
	}

	handler := w.eventHandlerFuncs(func(e Event) { received = append(received, e) })
	handler.OnDelete(tombstone)

	assert.Equal(t, 1, len(received), "tombstone should be unwrapped and produce an event")
	assert.Equal(t, EventDelete, received[0].Type)
	assert.Equal(t, "my-deploy", received[0].Name)
	assert.Equal(t, "default", received[0].Namespace)
	assert.Equal(t, map[string]string{"app": "test"}, received[0].Labels)
}

func TestAcquire_MultipleOwners(t *testing.T) {
	wm := newTestWatchManager(t)
	gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}

	_, err := wm.Acquire(gvr, "owner-a")
	assert.NoError(t, err)
	inf1 := wm.GetInformer(gvr)
	assert.NotNil(t, inf1)
	assert.Equal(t, 1, wm.ActiveWatchCount())

	// Second Acquire with a different key returns the same informer.
	_, err = wm.Acquire(gvr, "owner-b")
	assert.NoError(t, err)
	inf2 := wm.GetInformer(gvr)
	assert.Same(t, inf1, inf2)
	assert.Equal(t, 1, wm.ActiveWatchCount())

	// Releasing one owner does not stop the informer.
	wm.Release(gvr, "owner-a")
	assert.Equal(t, 1, wm.ActiveWatchCount(), "informer should still be running with one owner remaining")
	assert.NotNil(t, wm.GetInformer(gvr))

	// Releasing last owner stops the informer.
	wm.Release(gvr, "owner-b")
	assert.Equal(t, 0, wm.ActiveWatchCount(), "informer should stop when all owners are released")
	assert.Nil(t, wm.GetInformer(gvr))
}

func TestAcquire_Idempotent(t *testing.T) {
	wm := newTestWatchManager(t)
	gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}

	added, err := wm.Acquire(gvr, "test")
	assert.NoError(t, err)
	assert.True(t, added, "first Acquire should return true")

	// Same key again — idempotent, returns false.
	added, err = wm.Acquire(gvr, "test")
	assert.NoError(t, err)
	assert.False(t, added, "duplicate Acquire with same key should return false")

	assert.Equal(t, 1, wm.ActiveWatchCount())

	// Single Release is sufficient since the key was only counted once.
	wm.Release(gvr, "test")
	assert.Equal(t, 0, wm.ActiveWatchCount(), "informer should stop after single Release")
	assert.Nil(t, wm.GetInformer(gvr))
}

func TestShutdown(t *testing.T) {
	wm := newTestWatchManager(t)
	gvr1 := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	gvr2 := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "services"}

	_, _ = wm.Acquire(gvr1, "test")
	_, _ = wm.Acquire(gvr2, "test")
	assert.Equal(t, 2, wm.ActiveWatchCount())

	wm.Shutdown()
	assert.Equal(t, 0, wm.ActiveWatchCount())
	assert.Nil(t, wm.GetInformer(gvr1))
	assert.Nil(t, wm.GetInformer(gvr2))
}

func TestAddFunc(t *testing.T) {
	gvr := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"}

	var received []Event
	wm := NewWatchManager(
		fake.NewSimpleMetadataClient(runtime.NewScheme()),
		1*time.Hour,
		func(e Event) { received = append(received, e) },
		noopLogger(),
	)

	w := wm.newWatch(gvr)
	handler := w.eventHandlerFuncs(func(e Event) { received = append(received, e) })

	obj := &v1.PartialObjectMetadata{
		ObjectMeta: v1.ObjectMeta{
			Name:      "my-pod",
			Namespace: "default",
			Labels:    map[string]string{"app": "web"},
		},
	}
	handler.OnAdd(obj, false)

	assert.Equal(t, 1, len(received))
	assert.Equal(t, EventAdd, received[0].Type)
	assert.Equal(t, "my-pod", received[0].Name)
	assert.Equal(t, "default", received[0].Namespace)
	assert.Equal(t, map[string]string{"app": "web"}, received[0].Labels)
}

func TestEventHandlerFuncs_NonMetaObject(t *testing.T) {
	gvr := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"}

	var received []Event
	wm := NewWatchManager(
		fake.NewSimpleMetadataClient(runtime.NewScheme()),
		1*time.Hour,
		func(e Event) {},
		noopLogger(),
	)

	w := wm.newWatch(gvr)
	handler := w.eventHandlerFuncs(func(e Event) { received = append(received, e) })

	// Pass a non-meta object (plain string) — toEvent should return nil and
	// no event should be emitted.
	handler.OnAdd("not-a-meta-object", false)
	assert.Equal(t, 0, len(received))

	handler.OnUpdate("bad-old", "bad-new")
	assert.Equal(t, 0, len(received))

	handler.OnDelete("bad-obj")
	assert.Equal(t, 0, len(received))
}

func TestDeleteFunc_DirectObject(t *testing.T) {
	gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}

	var received []Event
	wm := NewWatchManager(
		fake.NewSimpleMetadataClient(runtime.NewScheme()),
		1*time.Hour,
		func(e Event) {},
		noopLogger(),
	)

	w := wm.newWatch(gvr)
	handler := w.eventHandlerFuncs(func(e Event) { received = append(received, e) })

	// Direct delete (no tombstone wrapper).
	obj := &v1.PartialObjectMetadata{
		ObjectMeta: v1.ObjectMeta{
			Name:      "direct-del",
			Namespace: "ns",
		},
	}
	handler.OnDelete(obj)

	assert.Equal(t, 1, len(received))
	assert.Equal(t, EventDelete, received[0].Type)
	assert.Equal(t, "direct-del", received[0].Name)
}

func TestNewWatch_WatchErrorHandler(t *testing.T) {
	gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}

	scheme := runtime.NewScheme()
	_ = v1.AddMetaToScheme(scheme)
	failClient := fake.NewSimpleMetadataClient(scheme)
	failClient.PrependWatchReactor("*", func(action clienttesting.Action) (bool, watch.Interface, error) {
		// Return a valid watcher that immediately stops, which triggers
		// the watch error handler on the next retry.
		w := watch.NewFake()
		w.Stop()
		return true, w, nil
	})
	failClient.PrependReactor("list", "*", func(action clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("simulated list error")
	})

	wm := NewWatchManager(failClient, 1*time.Hour, func(e Event) {}, noopLogger())
	wm.SyncTimeout = 500 * time.Millisecond
	_, _ = wm.Acquire(gvr, "test")

	// Give the informer goroutine time to hit the error handler.
	time.Sleep(200 * time.Millisecond)
}

func TestNewWatch_AddEventHandlerError(t *testing.T) {
	gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}

	scheme := runtime.NewScheme()
	_ = v1.AddMetaToScheme(scheme)

	wm := NewWatchManager(
		fake.NewSimpleMetadataClient(scheme),
		1*time.Hour,
		func(e Event) {},
		noopLogger(),
	)

	// Override createInformer to return an informer that's already stopped,
	// which causes AddEventHandler to return an error.
	wm.createInformer = func(gvr schema.GroupVersionResource) cache.SharedIndexInformer {
		// Create a real informer via the metadata client, start and stop it.
		inf := wm.defaultCreateInformer(gvr)
		stopCh := make(chan struct{})
		go inf.Run(stopCh)
		time.Sleep(50 * time.Millisecond)
		close(stopCh)
		// Wait for it to fully stop.
		time.Sleep(100 * time.Millisecond)
		return inf
	}

	// newWatch should log the error but not panic.
	w := wm.newWatch(gvr)
	assert.NotNil(t, w)
}

func TestUpdateFunc_OldLabels(t *testing.T) {
	gvr := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}

	var received []Event
	wm := NewWatchManager(
		fake.NewSimpleMetadataClient(runtime.NewScheme()),
		1*time.Hour,
		func(e Event) { received = append(received, e) },
		noopLogger(),
	)

	w := wm.newWatch(gvr)

	oldObj := &v1.PartialObjectMetadata{
		ObjectMeta: v1.ObjectMeta{
			Name:      "my-cm",
			Namespace: "default",
			Labels:    map[string]string{"team": "alpha"},
		},
	}
	newObj := &v1.PartialObjectMetadata{
		ObjectMeta: v1.ObjectMeta{
			Name:      "my-cm",
			Namespace: "default",
			Labels:    map[string]string{"team": "beta"},
		},
	}

	handler := w.eventHandlerFuncs(func(e Event) { received = append(received, e) })
	handler.OnUpdate(oldObj, newObj)

	assert.Equal(t, 1, len(received))
	assert.Equal(t, EventUpdate, received[0].Type)
	assert.Equal(t, map[string]string{"team": "beta"}, received[0].Labels)
	assert.Equal(t, map[string]string{"team": "alpha"}, received[0].OldLabels)
}

func TestAcquire_SyncTimeout(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = v1.AddMetaToScheme(scheme)
	client := fake.NewSimpleMetadataClient(scheme)
	// Fail all list calls so the informer cannot sync.
	client.PrependReactor("list", "*", func(action clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("simulated list error")
	})

	wm := NewWatchManager(client, 1*time.Hour, func(Event) {}, noopLogger())
	wm.SyncTimeout = 200 * time.Millisecond

	gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	_, err := wm.Acquire(gvr, "test")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "cache sync timeout")

	// Broken watch should be cleaned up so a future Acquire can retry.
	assert.Equal(t, 0, wm.ActiveWatchCount())
}

func TestAcquire_SyncTimeout_RetrySucceeds(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = v1.AddMetaToScheme(scheme)
	client := fake.NewSimpleMetadataClient(scheme)

	// First call: fail all lists → sync timeout.
	var failList atomic.Bool
	failList.Store(true)
	client.PrependReactor("list", "*", func(action clienttesting.Action) (bool, runtime.Object, error) {
		if failList.Load() {
			return true, nil, fmt.Errorf("simulated list error")
		}
		return false, nil, nil
	})

	wm := NewWatchManager(client, 1*time.Hour, func(Event) {}, noopLogger())
	wm.SyncTimeout = 200 * time.Millisecond

	gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}

	_, err := wm.Acquire(gvr, "test")
	assert.Error(t, err)
	assert.Equal(t, 0, wm.ActiveWatchCount(), "broken watch should be removed")

	// Second call: lists succeed → should create fresh informer and sync.
	failList.Store(false)
	wm.SyncTimeout = 5 * time.Second
	_, err = wm.Acquire(gvr, "test")
	assert.NoError(t, err)
	assert.Equal(t, 1, wm.ActiveWatchCount(), "retry should succeed with fresh informer")
	wm.Shutdown()
}

func TestAcquire_SyncSuccess(t *testing.T) {
	wm := newTestWatchManager(t)
	gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	wm.SyncTimeout = 5 * time.Second

	_, err := wm.Acquire(gvr, "test")
	assert.NoError(t, err)
	assert.Equal(t, 1, wm.ActiveWatchCount())
	wm.Shutdown()
}

func TestAcquire_ConcurrentCalls(t *testing.T) {
	wm := newTestWatchManager(t)
	gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}

	// Launch multiple concurrent Acquire calls with distinct keys.
	errs := make(chan error, 10)
	for i := 0; i < 10; i++ {
		go func(idx int) {
			_, err := wm.Acquire(gvr, fmt.Sprintf("caller-%d", idx))
			errs <- err
		}(i)
	}
	for i := 0; i < 10; i++ {
		assert.NoError(t, <-errs)
	}

	// Only one informer should exist.
	assert.Equal(t, 1, wm.ActiveWatchCount())
	wm.Shutdown()
}

func TestConcurrentAcquire_Release(t *testing.T) {
	wm := newTestWatchManager(t)
	wm.SyncTimeout = 500 * time.Millisecond
	gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}

	// Start and stop concurrently with key "a".
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 20; i++ {
			_, _ = wm.Acquire(gvr, "a")
			wm.Release(gvr, "a")
		}
	}()

	// Concurrent Acquire calls with key "b".
	for i := 0; i < 20; i++ {
		_, _ = wm.Acquire(gvr, "b")
	}
	<-done

	// Should not panic. Final state is deterministic: either 0 or 1 watches.
	count := wm.ActiveWatchCount()
	assert.True(t, count == 0 || count == 1)
	wm.Shutdown()
}

func TestAcquire_SyncTimeout_ConcurrentRef(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = v1.AddMetaToScheme(scheme)
	client := fake.NewSimpleMetadataClient(scheme)

	// Fail all list calls so the first Acquire's cache sync times out.
	client.PrependReactor("list", "*", func(action clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("simulated list error")
	})

	wm := NewWatchManager(client, 1*time.Hour, func(Event) {}, noopLogger())
	wm.SyncTimeout = 200 * time.Millisecond

	gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}

	// G1: Acquire creates the watch (owner "g1") and starts waiting for sync.
	// While G1 blocks, G2 Acquires the same GVR (owner "g2").
	// When G1's sync timeout fires, it should remove only its own key
	// instead of force-removing the watch, so G2's ownership stays valid.
	var g2err error
	g2done := make(chan struct{})

	// Start G1.
	g1done := make(chan struct{})
	go func() {
		defer close(g1done)
		_, _ = wm.Acquire(gvr, "g1") // will timeout
	}()

	// Give G1 time to create the watch and start waiting for sync.
	time.Sleep(50 * time.Millisecond)

	// G2: Acquire same GVR with different key.
	go func() {
		defer close(g2done)
		_, g2err = wm.Acquire(gvr, "g2")
	}()
	<-g2done

	// G2 should succeed (it adds its key to the existing watch).
	assert.NoError(t, g2err)

	// Wait for G1 timeout.
	<-g1done

	// The watch should still exist because G2 holds ownership.
	assert.Equal(t, 1, wm.ActiveWatchCount(),
		"watch should survive G1's timeout because G2 still owns it")

	// G2 releases — now the watch should stop.
	wm.Release(gvr, "g2")
	assert.Equal(t, 0, wm.ActiveWatchCount(),
		"watch should stop after G2 releases")
}

func TestSyncTimeout_DefaultValue(t *testing.T) {
	wm := newTestWatchManager(t)
	assert.Equal(t, defaultSyncTimeout, wm.syncTimeout())

	wm.SyncTimeout = 5 * time.Second
	assert.Equal(t, 5*time.Second, wm.syncTimeout())
}
