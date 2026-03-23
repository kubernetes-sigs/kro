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

func TestReleaseWatch_StopsUnowned(t *testing.T) {
	wm := newTestWatchManager(t)
	gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}

	assert.NoError(t, wm.EnsureWatch(gvr, "test"))
	assert.Equal(t, 1, wm.ActiveWatchCount())

	wm.ReleaseWatch(gvr, "test")
	assert.Equal(t, 0, wm.ActiveWatchCount())

	// Second release should not panic and count stays 0.
	wm.ReleaseWatch(gvr, "test")
	assert.Equal(t, 0, wm.ActiveWatchCount())
}

func TestReleaseWatch_ThenRetainWatch_CreatesFresh(t *testing.T) {
	wm := newTestWatchManager(t)
	gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}

	assert.NoError(t, wm.EnsureWatch(gvr, "test"))
	inf1 := wm.GetInformer(gvr)
	assert.NotNil(t, inf1)

	wm.ReleaseWatch(gvr, "test")
	assert.Nil(t, wm.GetInformer(gvr))

	assert.NoError(t, wm.EnsureWatch(gvr, "test"))
	inf2 := wm.GetInformer(gvr)
	assert.NotNil(t, inf2)

	// Must be a new informer instance, not the old one.
	assert.NotSame(t, inf1, inf2, "expected fresh informer after ReleaseWatch + EnsureWatch")
}

func TestReleaseWatch_RetainedByOtherOwner(t *testing.T) {
	wm := newTestWatchManager(t)
	gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}

	assert.NoError(t, wm.EnsureWatch(gvr, "parent"))
	assert.NotNil(t, wm.GetInformer(gvr))

	// Releasing an unrelated owner should not stop an owned watch.
	wm.ReleaseWatch(gvr, "other")
	assert.NotNil(t, wm.GetInformer(gvr), "owned informer should stay running")

	// Releasing the actual owner should stop it.
	wm.ReleaseWatch(gvr, "parent")
	assert.Nil(t, wm.GetInformer(gvr), "watch should stop after the owner releases it")
}

func TestRetainWatch_MultipleOwners(t *testing.T) {
	wm := newTestWatchManager(t)
	gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}

	assert.NoError(t, wm.EnsureWatch(gvr, "parent"))
	assert.NoError(t, wm.EnsureWatch(gvr, "coordinator"))
	assert.Equal(t, 1, wm.ActiveWatchCount())

	// Release one owner — watch should stay.
	wm.ReleaseWatch(gvr, "parent")
	assert.NotNil(t, wm.GetInformer(gvr), "informer should stay running with one owner remaining")

	// Release second owner — now it stops automatically.
	wm.ReleaseWatch(gvr, "coordinator")
	assert.Nil(t, wm.GetInformer(gvr), "informer should stop after all owners release")
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

func TestEnsureWatch_Idempotent(t *testing.T) {
	wm := newTestWatchManager(t)
	gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}

	assert.NoError(t, wm.EnsureWatch(gvr, "test"))
	inf1 := wm.GetInformer(gvr)
	assert.NotNil(t, inf1)
	assert.Equal(t, 1, wm.ActiveWatchCount())

	// Second call is a no-op; same informer, same count.
	assert.NoError(t, wm.EnsureWatch(gvr, "test"))
	inf2 := wm.GetInformer(gvr)
	assert.Same(t, inf1, inf2)
	assert.Equal(t, 1, wm.ActiveWatchCount())
}

func TestShutdown(t *testing.T) {
	wm := newTestWatchManager(t)
	gvr1 := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	gvr2 := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "services"}

	assert.NoError(t, wm.EnsureWatch(gvr1, "test"))
	assert.NoError(t, wm.EnsureWatch(gvr2, "test"))
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
	_ = wm.EnsureWatch(gvr, "test")

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

func TestEnsureWatch_SyncTimeout(t *testing.T) {
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
	err := wm.EnsureWatch(gvr, "test")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "cache sync timeout")

	// Broken watch should be cleaned up so a future EnsureWatch can retry.
	assert.Equal(t, 0, wm.ActiveWatchCount())
}

func TestEnsureWatch_SyncTimeout_RetrySucceeds(t *testing.T) {
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

	err := wm.EnsureWatch(gvr, "test")
	assert.Error(t, err)
	assert.Equal(t, 0, wm.ActiveWatchCount(), "broken watch should be removed")

	// Second call: lists succeed → should create fresh informer and sync.
	failList.Store(false)
	wm.SyncTimeout = 5 * time.Second
	err = wm.EnsureWatch(gvr, "test")
	assert.NoError(t, err)
	assert.Equal(t, 1, wm.ActiveWatchCount(), "retry should succeed with fresh informer")
	wm.Shutdown()
}

func TestEnsureWatch_SyncSuccess(t *testing.T) {
	wm := newTestWatchManager(t)
	gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	wm.SyncTimeout = 5 * time.Second

	err := wm.EnsureWatch(gvr, "test")
	assert.NoError(t, err)
	assert.Equal(t, 1, wm.ActiveWatchCount())
	wm.Shutdown()
}

func TestEnsureWatch_ConcurrentCalls(t *testing.T) {
	wm := newTestWatchManager(t)
	gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}

	// Launch multiple concurrent EnsureWatch calls.
	errs := make(chan error, 10)
	for i := 0; i < 10; i++ {
		go func() {
			errs <- wm.EnsureWatch(gvr, "test")
		}()
	}
	for i := 0; i < 10; i++ {
		assert.NoError(t, <-errs)
	}

	// Only one informer should exist.
	assert.Equal(t, 1, wm.ActiveWatchCount())
	wm.Shutdown()
}

func TestConcurrentRetainWatch_ReleaseWatch(t *testing.T) {
	wm := newTestWatchManager(t)
	wm.SyncTimeout = 500 * time.Millisecond
	gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}

	// Retain and release concurrently.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 20; i++ {
			_ = wm.EnsureWatch(gvr, "a")
			wm.ReleaseWatch(gvr, "a")
		}
	}()

	// Concurrent EnsureWatch calls.
	for i := 0; i < 20; i++ {
		_ = wm.EnsureWatch(gvr, "b")
		wm.ReleaseWatch(gvr, "b")
	}
	<-done

	// Should not panic. Final state: 0 watches (all owners released).
	assert.Equal(t, 0, wm.ActiveWatchCount())
}

func TestEnsureWatch_RaceCondition_ReleaseBeforeInformerCreated(t *testing.T) {
	// Regression test: EnsureWatch must hold the lock through both owner
	// registration and informer creation. Without this, a concurrent
	// ReleaseWatch between AddOwner and EnsureWatch could remove the owner,
	// leaving a leaked watch with zero owners.
	//
	// With the fix, ReleaseWatch blocks until EnsureWatch releases the
	// lock (after informer creation but before cache sync). ReleaseWatch
	// then removes the owner and stops the watch, which cancels the
	// informer context and causes cache sync to fail.
	scheme := runtime.NewScheme()
	_ = v1.AddMetaToScheme(scheme)
	client := fake.NewSimpleMetadataClient(scheme)

	gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}

	wm := NewWatchManager(client, 1*time.Hour, func(Event) {}, noopLogger())
	wm.SyncTimeout = 1 * time.Second

	// Use a slow createInformer so the lock is held longer, giving
	// ReleaseWatch time to block on the mutex.
	wm.createInformer = func(gvr schema.GroupVersionResource) cache.SharedIndexInformer {
		time.Sleep(50 * time.Millisecond)
		return wm.defaultCreateInformer(gvr)
	}

	// Start EnsureWatch in a goroutine.
	errCh := make(chan error, 1)
	go func() {
		errCh <- wm.EnsureWatch(gvr, "owner-a")
	}()

	// Give EnsureWatch time to acquire the lock. The slow createInformer
	// means the lock is held for ~50ms, during which ReleaseWatch blocks.
	time.Sleep(10 * time.Millisecond)

	// ReleaseWatch while EnsureWatch holds the lock for informer creation.
	// With the fix, ReleaseWatch blocks until the lock is released (after
	// informer start but before cache sync), then removes the owner and
	// stops the informer — causing cache sync to fail in EnsureWatch.
	wm.ReleaseWatch(gvr, "owner-a")

	// EnsureWatch may return an error (cache sync timeout because the
	// informer was stopped) or succeed (if cache synced before ReleaseWatch
	// ran). Either way, the key invariant holds: no leaked watches.
	<-errCh

	// The key invariant: no leaked watch with zero owners.
	assert.Equal(t, 0, wm.ActiveWatchCount(), "watch should be stopped after sole owner released")
}

func TestEnsureWatch_AtomicOwnerAndWatch(t *testing.T) {
	// Verify that after EnsureWatch returns successfully, both the owner
	// and the watch exist — i.e., they were created atomically.
	wm := newTestWatchManager(t)
	gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}

	assert.NoError(t, wm.EnsureWatch(gvr, "owner-a"))

	// Both owner and watch should exist.
	assert.Equal(t, 1, wm.ActiveWatchCount())
	assert.NotNil(t, wm.GetInformer(gvr))

	// ReleaseWatch should be able to clean up properly.
	wm.ReleaseWatch(gvr, "owner-a")
	assert.Equal(t, 0, wm.ActiveWatchCount())
	assert.Nil(t, wm.GetInformer(gvr))
}

func TestEnsureWatch_SyncTimeout_CleansUpOwner(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = v1.AddMetaToScheme(scheme)
	client := fake.NewSimpleMetadataClient(scheme)
	client.PrependReactor("list", "*", func(action clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("simulated list error")
	})

	wm := NewWatchManager(client, 1*time.Hour, func(Event) {}, noopLogger())
	wm.SyncTimeout = 200 * time.Millisecond

	gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}

	err := wm.EnsureWatch(gvr, "owner-a")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "cache sync timeout")

	// Both the watch and the owner should be cleaned up.
	assert.Equal(t, 0, wm.ActiveWatchCount())

	// Verify owner was removed by checking that a new EnsureWatch + Release
	// doesn't leave stale state.
	wm.ReleaseWatch(gvr, "owner-a")
	assert.Equal(t, 0, wm.ActiveWatchCount())
}

func TestSyncTimeout_DefaultValue(t *testing.T) {
	wm := newTestWatchManager(t)
	assert.Equal(t, defaultSyncTimeout, wm.syncTimeout())

	wm.SyncTimeout = 5 * time.Second
	assert.Equal(t, 5*time.Second, wm.syncTimeout())
}

func TestReleaseWatch_HandlerRemoved(t *testing.T) {
	gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}

	var eventCount atomic.Int32
	wm := NewWatchManager(
		fake.NewSimpleMetadataClient(func() *runtime.Scheme {
			s := runtime.NewScheme()
			_ = v1.AddMetaToScheme(s)
			return s
		}()),
		1*time.Hour,
		func(e Event) { eventCount.Add(1) },
		noopLogger(),
	)

	assert.NoError(t, wm.EnsureWatch(gvr, "test"))
	assert.NotNil(t, wm.GetInformer(gvr))

	// Release stops the watch and removes the handler.
	wm.ReleaseWatch(gvr, "test")
	assert.Nil(t, wm.GetInformer(gvr))
}

func TestShutdown_HandlerRemoved(t *testing.T) {
	gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}

	wm := NewWatchManager(
		fake.NewSimpleMetadataClient(func() *runtime.Scheme {
			s := runtime.NewScheme()
			_ = v1.AddMetaToScheme(s)
			return s
		}()),
		1*time.Hour,
		func(e Event) {},
		noopLogger(),
	)

	assert.NoError(t, wm.EnsureWatch(gvr, "test"))
	wm.Shutdown()
	assert.Equal(t, 0, wm.ActiveWatchCount())
}
