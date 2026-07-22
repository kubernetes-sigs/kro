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

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/cache"
)

var (
	gvrA = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}
	gvrB = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
)

// newTestManager returns a manager whose createInformer hands out a
// pre-baked fake. The map of informers is exposed so tests can drive
// events. SyncTimeout is short so failing syncs don't stall the suite.
func newTestManager(t *testing.T, onEvent EventHandler) (*Manager, *fakeInformerRegistry) {
	t.Helper()
	if onEvent == nil {
		onEvent = func(Event) {}
	}
	reg := &fakeInformerRegistry{informers: make(map[schema.GroupVersionResource]*fakeInformer)}
	wm := NewManager(nil, 0, onEvent, logr.Discard())
	wm.SyncTimeout = 500 * time.Millisecond
	wm.createInformer = reg.create
	t.Cleanup(wm.Shutdown)
	return wm, reg
}

type fakeInformerRegistry struct {
	mu        sync.Mutex
	informers map[schema.GroupVersionResource]*fakeInformer
}

func (r *fakeInformerRegistry) create(gvr schema.GroupVersionResource) cache.SharedIndexInformer {
	r.mu.Lock()
	defer r.mu.Unlock()
	f := newFakeInformer()
	r.informers[gvr] = f
	return f
}

func (r *fakeInformerRegistry) get(gvr schema.GroupVersionResource) *fakeInformer {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.informers[gvr]
}

// TestManagerLifecycle exercises EnsureWatch + ReleaseWatch +
// Shutdown across single-owner, multi-owner, and idempotent cases. The
// fake informer transitions to HasSynced=true inside its Run goroutine
// so EnsureWatch unblocks naturally.
func TestManagerLifecycle(t *testing.T) {
	tests := []struct {
		name string
		fn   func(t *testing.T, wm *Manager, reg *fakeInformerRegistry)
	}{
		{
			name: "ensure-once-and-release-stops",
			fn: func(t *testing.T, wm *Manager, reg *fakeInformerRegistry) {
				require.NoError(t, wm.EnsureWatch(gvrA, "owner-1"))
				inf := reg.get(gvrA)
				require.NotNil(t, inf)
				assert.Equal(t, 1, wm.ActiveWatchCount())
				wm.ReleaseWatch(gvrA, "owner-1")
				assert.Equal(t, 0, wm.ActiveWatchCount())
				assert.Eventually(t, inf.IsStopped, time.Second, 5*time.Millisecond)
			},
		},
		{
			name: "two-owners-only-stops-on-last-release",
			fn: func(t *testing.T, wm *Manager, reg *fakeInformerRegistry) {
				require.NoError(t, wm.EnsureWatch(gvrA, "owner-1"))
				require.NoError(t, wm.EnsureWatch(gvrA, "owner-2"))
				assert.Equal(t, 1, wm.ActiveWatchCount())
				wm.ReleaseWatch(gvrA, "owner-1")
				assert.Equal(t, 1, wm.ActiveWatchCount(), "second owner keeps watch alive")
				wm.ReleaseWatch(gvrA, "owner-2")
				assert.Equal(t, 0, wm.ActiveWatchCount())
				inf := reg.get(gvrA)
				assert.Eventually(t, inf.IsStopped, time.Second, 5*time.Millisecond)
			},
		},
		{
			name: "duplicate-ensure-is-idempotent",
			fn: func(t *testing.T, wm *Manager, _ *fakeInformerRegistry) {
				require.NoError(t, wm.EnsureWatch(gvrA, "owner-1"))
				require.NoError(t, wm.EnsureWatch(gvrA, "owner-1"))
				require.NoError(t, wm.EnsureWatch(gvrA, "owner-1"))
				assert.Equal(t, 1, wm.ActiveWatchCount())
				wm.ReleaseWatch(gvrA, "owner-1")
				assert.Equal(t, 0, wm.ActiveWatchCount())
			},
		},
		{
			name: "release-unknown-owner-is-noop",
			fn: func(t *testing.T, wm *Manager, _ *fakeInformerRegistry) {
				wm.ReleaseWatch(gvrA, "ghost")
				assert.Equal(t, 0, wm.ActiveWatchCount())
			},
		},
		{
			name: "shutdown-stops-all",
			fn: func(t *testing.T, wm *Manager, reg *fakeInformerRegistry) {
				require.NoError(t, wm.EnsureWatch(gvrA, "x"))
				require.NoError(t, wm.EnsureWatch(gvrB, "x"))
				assert.Equal(t, 2, wm.ActiveWatchCount())
				wm.Shutdown()
				assert.Equal(t, 0, wm.ActiveWatchCount())
				assert.Eventually(t, reg.get(gvrA).IsStopped, time.Second, 5*time.Millisecond)
				assert.Eventually(t, reg.get(gvrB).IsStopped, time.Second, 5*time.Millisecond)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			wm, reg := newTestManager(t, nil)
			tc.fn(t, wm, reg)
		})
	}
}

// TestManagerEventRouting drives Add/Update/Delete through the fake
// informer and asserts the events surface via onEvent in the expected
// shape (labels cloned, OldLabels populated only on update).
func TestManagerEventRouting(t *testing.T) {
	tests := []struct {
		name string
		fire func(inf *fakeInformer)
		want Event
	}{
		{
			name: "add",
			fire: func(inf *fakeInformer) {
				inf.fireAdd(newFakeObj("ns1", "cm-1", map[string]string{"app": "x"}))
			},
			want: Event{
				Type: EventAdd, GVR: gvrA, Name: "cm-1", Namespace: "ns1",
				Labels: map[string]string{"app": "x"},
			},
		},
		{
			name: "update-carries-old-labels",
			fire: func(inf *fakeInformer) {
				inf.fireUpdate(
					newFakeObj("ns1", "cm-1", map[string]string{"app": "old"}),
					newFakeObj("ns1", "cm-1", map[string]string{"app": "new"}),
				)
			},
			want: Event{
				Type: EventUpdate, GVR: gvrA, Name: "cm-1", Namespace: "ns1",
				Labels:    map[string]string{"app": "new"},
				OldLabels: map[string]string{"app": "old"},
			},
		},
		{
			name: "delete",
			fire: func(inf *fakeInformer) {
				inf.fireDelete(newFakeObj("ns1", "cm-1", nil))
			},
			want: Event{Type: EventDelete, GVR: gvrA, Name: "cm-1", Namespace: "ns1"},
		},
		{
			name: "delete-unwraps-tombstone",
			fire: func(inf *fakeInformer) {
				inf.fireDelete(cache.DeletedFinalStateUnknown{
					Key: "ns1/cm-1",
					Obj: newFakeObj("ns1", "cm-1", nil),
				})
			},
			want: Event{Type: EventDelete, GVR: gvrA, Name: "cm-1", Namespace: "ns1"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var (
				mu  sync.Mutex
				got []Event
			)
			handler := func(e Event) {
				mu.Lock()
				defer mu.Unlock()
				got = append(got, e)
			}
			wm, reg := newTestManager(t, handler)
			require.NoError(t, wm.EnsureWatch(gvrA, "owner"))
			tc.fire(reg.get(gvrA))

			mu.Lock()
			defer mu.Unlock()
			require.Len(t, got, 1)
			assert.Equal(t, tc.want, got[0])
		})
	}
}

// TestManagerSyncTimeout asserts EnsureWatch fails when the fake
// informer never reports HasSynced. The watch is torn down on timeout so
// active count stays zero — no zombie watches.
func TestManagerSyncTimeout(t *testing.T) {
	wm := NewManager(nil, 0, func(Event) {}, logr.Discard())
	wm.SyncTimeout = 50 * time.Millisecond

	never := &neverSyncInformer{}
	wm.createInformer = func(_ schema.GroupVersionResource) cache.SharedIndexInformer {
		return never
	}
	t.Cleanup(wm.Shutdown)

	err := wm.EnsureWatch(gvrA, "owner")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cache sync timeout")
	assert.Equal(t, 0, wm.ActiveWatchCount())
	// We were the only owner, so ReleaseWatch (called from the
	// timeout path inside EnsureWatch) tore the informer down.
	assert.Eventually(t, func() bool {
		return never.stopped.Load()
	}, time.Second, 5*time.Millisecond)
}

// neverSyncInformer reports HasSynced=false forever — even after Run is
// invoked — so cache.WaitForCacheSync hits its deadline.
type neverSyncInformer struct {
	fakeInformer
	stopped atomic.Bool
}

func (n *neverSyncInformer) HasSynced() bool { return false }
func (n *neverSyncInformer) RunWithContext(ctx context.Context) {
	<-ctx.Done()
	n.stopped.Store(true)
}
