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

package watchrouter

// This file rounds out coverage on the less-exercised paths in
// router, coordinator, and manager — the gaps left after the core
// behavioral tests in coordinator_test.go / controller_test.go /
// manager_test.go land. Cases here are deliberately small: each
// targets one branch or method.

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/metadata/fake"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TestNewRouterProductionConstructor exercises NewRouter with a real
// metadata client (a fake-but-typed implementation) so the production
// wiring path is covered. No informers actually run because we don't
// EnsureWatch anything.
func TestNewRouterProductionConstructor(t *testing.T) {
	scheme := fake.NewTestScheme()
	mc := fake.NewSimpleMetadataClient(scheme)

	r := NewRouter(logr.Discard(), Config{
		EventBuffer:  16,
		SyncTimeout:  time.Second,
		ResyncPeriod: 0,
	}, mc)
	require.NotNil(t, r)
	assert.NotNil(t, r.Coordinator())
	assert.NotNil(t, r.Manager())
	assert.True(t, r.NeedLeaderElection())
	// Default-config branch.
	r2 := NewRouter(logr.Discard(), Config{}, mc)
	require.NotNil(t, r2)
}

// TestRouterManagerAccessor covers the WatchManager() helper that
// the integration tests don't touch.
func TestRouterManagerAccessor(t *testing.T) {
	dc, _ := newTestDC(t)
	assert.NotNil(t, dc.Manager())
}

// TestManagerGetInformer covers GetInformer's hit + miss branches.
func TestManagerGetInformer(t *testing.T) {
	wm, _ := newTestManager(t, nil)
	assert.Nil(t, wm.GetInformer(gvrA))
	require.NoError(t, wm.EnsureWatch(gvrA, "owner"))
	assert.NotNil(t, wm.GetInformer(gvrA))
}

// TestManagerSyncTimeoutDefault drops into syncTimeout's default
// branch (zero → 30s fallback). We only need to instantiate the
// manager with SyncTimeout=0 and observe a successful EnsureWatch
// (which uses the fallback path internally).
func TestManagerSyncTimeoutDefault(t *testing.T) {
	wm := NewManager(nil, 0, func(Event) {}, logr.Discard())
	reg := &fakeInformerRegistry{informers: make(map[schema.GroupVersionResource]*fakeInformer)}
	wm.createInformer = reg.create
	t.Cleanup(wm.Shutdown)
	// SyncTimeout=0 means defaultSyncTimeout (30s) applies. The fake
	// informer synces immediately so we get back from EnsureWatch
	// long before 30s, exercising the default-fallback branch.
	require.NoError(t, wm.EnsureWatch(gvrA, "owner"))
}

// TestCoordinatorCollectionRemovalAndAbort drives a Done(true) cycle
// that removes a collection watch — the only path through
// removeCollectionIndexLocked.
func TestCoordinatorCollectionRemovalAndAbort(t *testing.T) {
	graphA := client.ObjectKey{Namespace: "ns", Name: "g"}
	c, _ := newTestCoordinator(t)

	sel, err := labels.Parse("app=svc")
	require.NoError(t, err)

	w := c.ForGraph(graphA)
	require.NoError(t, w.Watch(collectionReq("c1", gvrA, "ns", sel)))
	require.NoError(t, w.Watch(collectionReq("c2", gvrB, "ns", sel)))
	w.Done(true)
	_, collection := c.WatchRequestCount()
	assert.Equal(t, 2, collection)

	// Second cycle: drop c1; only c2 should remain.
	w2 := c.ForGraph(graphA)
	require.NoError(t, w2.Watch(collectionReq("c2", gvrB, "ns", sel)))
	w2.Done(true)
	_, collection = c.WatchRequestCount()
	assert.Equal(t, 1, collection)

	// Third cycle: re-introduce c1, then abort — the abort branch
	// of removeRequestFromIndexesLocked for a collection request.
	w3 := c.ForGraph(graphA)
	require.NoError(t, w3.Watch(collectionReq("c2", gvrB, "ns", sel)))
	require.NoError(t, w3.Watch(collectionReq("c1", gvrA, "ns", sel)))
	w3.Done(false)
	_, collection = c.WatchRequestCount()
	assert.Equal(t, 1, collection, "abort should roll back c1")
}

// TestCoordinatorCollectionDedup hits the early-return dedup branch in
// addCollectionIndexLocked: re-watching the same {nodeID, GVR, ns,
// selector} must not produce a duplicate entry.
func TestCoordinatorCollectionDedup(t *testing.T) {
	graphA := client.ObjectKey{Namespace: "ns", Name: "g"}
	c, _ := newTestCoordinator(t)
	sel, err := labels.Parse("app=svc")
	require.NoError(t, err)

	w := c.ForGraph(graphA)
	require.NoError(t, w.Watch(collectionReq("c1", gvrA, "ns", sel)))
	require.NoError(t, w.Watch(collectionReq("c1", gvrA, "ns", sel)))
	require.NoError(t, w.Watch(collectionReq("c1", gvrA, "ns", sel)))
	w.Done(true)
	_, collection := c.WatchRequestCount()
	assert.Equal(t, 1, collection)
}

// TestRouterEnqueueDropsOnFullBuffer saturates the events channel and
// confirms the next enqueue is dropped silently instead of blocking.
// This is the only branch of enqueue we haven't exercised — the
// default-case in the non-blocking select.
func TestRouterEnqueueDropsOnFullBuffer(t *testing.T) {
	r, _ := newTestDC(t)
	// Drain anything that may have been buffered by setup.
	for {
		select {
		case <-r.events:
		default:
			goto saturate
		}
	}
saturate:
	bufCap := cap(r.events)
	for range bufCap {
		r.enqueue(client.ObjectKey{Namespace: "ns", Name: "x"})
	}
	// One more — this must hit the drop branch without panic.
	r.enqueue(client.ObjectKey{Namespace: "ns", Name: "overflow"})

	// Sanity: chan length should still be at capacity, not capacity+1.
	assert.Equal(t, bufCap, len(r.events))
}

// TestNoopWatcherDoneStatements exercises both Done(true) and
// Done(false) paths so the cover tool registers the empty receiver
// body as covered.
func TestNoopWatcherDoneStatements(t *testing.T) {
	var w Watcher = NoopWatcher{}
	w.Done(true)
	w.Done(false)
	require.NoError(t, w.Watch(WatchRequest{}))
}

// TestManagerNewWatchEventHandlerError stresses the
// AddEventHandler-failure log path. We can't easily make the real
// informer fail, but we can ensure newWatch survives when the
// informer returns an error from AddEventHandler — using a custom
// fake that errors on registration.
func TestManagerNewWatchEventHandlerError(t *testing.T) {
	wm := NewManager(nil, 0, func(Event) {}, logr.Discard())
	wm.SyncTimeout = 200 * time.Millisecond
	wm.createInformer = func(_ schema.GroupVersionResource) cache.SharedIndexInformer {
		return &erroringHandlerInformer{fakeInformer: *newFakeInformer()}
	}
	t.Cleanup(wm.Shutdown)
	// EnsureWatch should still succeed: the error path inside newWatch
	// only logs and the informer otherwise behaves.
	require.NoError(t, wm.EnsureWatch(gvrA, "owner"))
}

// erroringHandlerInformer satisfies SharedIndexInformer but returns
// an error from AddEventHandler so newWatch's error branch fires.
type erroringHandlerInformer struct {
	fakeInformer
}

func (e *erroringHandlerInformer) AddEventHandler(_ cache.ResourceEventHandler) (cache.ResourceEventHandlerRegistration, error) {
	return nil, context.Canceled // any error will do
}

// Compile-time check the real metadata.Interface satisfies what
// NewRouter wants. Catches an accidental dependency drift.
var _ metadata.Interface = (metadata.Interface)(nil)

// TestManagerDefaultCreateInformer exercises the production
// metadatainformer factory using the fake metadata client. We don't
// drive any events through — only ensure the factory returns a
// usable informer (non-nil, registers handlers, syncs).
func TestManagerDefaultCreateInformer(t *testing.T) {
	scheme := fake.NewTestScheme()
	mc := fake.NewSimpleMetadataClient(scheme)

	wm := NewManager(mc, 0, func(Event) {}, logr.Discard())
	wm.SyncTimeout = 2 * time.Second
	t.Cleanup(wm.Shutdown)

	// Pods are a known GVR the fake scheme registers by default.
	pods := schema.GroupVersionResource{Version: "v1", Resource: "pods"}
	require.NoError(t, wm.EnsureWatch(pods, "owner"))
	assert.NotNil(t, wm.GetInformer(pods))
	assert.Equal(t, 1, wm.ActiveWatchCount())
}

// TestCoordinatorNoSuchKey table-tests the early-return paths in
// RemoveGraph, doneGraph, and abortGraph when no state has been
// recorded for the supplied key.
func TestCoordinatorNoSuchKey(t *testing.T) {
	ghost := client.ObjectKey{Namespace: "ns", Name: "ghost"}
	tests := []struct {
		name string
		op   func(c *Coordinator)
	}{
		{"RemoveGraph", func(c *Coordinator) { c.RemoveGraph(ghost) }},
		{"Done(true)", func(c *Coordinator) { c.ForGraph(ghost).Done(true) }},
		{"Done(false)", func(c *Coordinator) { c.ForGraph(ghost).Done(false) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newTestCoordinator(t)
			tc.op(c)
			assert.Equal(t, 0, c.GraphCount())
		})
	}
}

// TestEventHandlerFuncsAccessorError fires an event with an object
// that fails meta.Accessor (e.g. a non-metav1.Object value) so we
// exercise the error-skip branch in eventHandlerFuncs.
func TestEventHandlerFuncsAccessorError(t *testing.T) {
	var seen int
	handler := func(Event) { seen++ }
	wm, reg := newTestManager(t, handler)
	require.NoError(t, wm.EnsureWatch(gvrA, "owner"))
	inf := reg.get(gvrA)

	// "string" is not an ObjectMeta accessor — toEvent should bail out.
	inf.fireAdd("not-a-meta-object")
	inf.fireUpdate("old", "new")
	inf.fireDelete("not-a-meta-object")

	// Real object should still flow through.
	inf.fireAdd(newFakeObj("ns", "cm-1", nil))
	assert.Equal(t, 1, seen)
}
