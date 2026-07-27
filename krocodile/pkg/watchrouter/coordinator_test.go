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
	"sync"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// enqueueRecorder captures the keys the coordinator asks to be requeued.
// Thread-safe so concurrent RouteEvent goroutines don't race.
type enqueueRecorder struct {
	mu   sync.Mutex
	keys []client.ObjectKey
}

func (r *enqueueRecorder) fn() EnqueueFunc {
	return func(k client.ObjectKey) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.keys = append(r.keys, k)
	}
}

func (r *enqueueRecorder) snapshot() []client.ObjectKey {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]client.ObjectKey(nil), r.keys...)
}

// newTestCoordinator returns a coordinator wired to an in-memory
// Manager backed by fake informers. Cleanup is registered so each
// test starts clean.
func newTestCoordinator(t *testing.T) (*Coordinator, *enqueueRecorder) {
	t.Helper()
	rec := &enqueueRecorder{}
	wm, _ := newTestManager(t, nil)
	return NewCoordinator(wm, rec.fn(), logr.Discard()), rec
}

func scalarReq(nodeID string, gvr schema.GroupVersionResource, name, ns string) WatchRequest {
	return WatchRequest{NodeID: nodeID, GVR: gvr, Name: name, Namespace: ns}
}

func collectionReq(nodeID string, gvr schema.GroupVersionResource, ns string, sel labels.Selector) WatchRequest {
	return WatchRequest{NodeID: nodeID, GVR: gvr, Namespace: ns, Selector: sel}
}

// TestCoordinatorWatchAndDone covers the Watch/Done(commit) cycle:
// committing a fresh set, removing stale entries when a node drops out,
// and aborting an in-flight cycle without disturbing the previous one.
func TestCoordinatorWatchAndDone(t *testing.T) {
	graphA := client.ObjectKey{Namespace: "ns", Name: "graph-a"}

	t.Run("commit-installs-scalar-watch", func(t *testing.T) {
		c, _ := newTestCoordinator(t)
		w := c.ForGraph(graphA)
		require.NoError(t, w.Watch(scalarReq("n1", gvrA, "cm-1", "ns")))
		w.Done(true)
		scalar, collection := c.WatchRequestCount()
		assert.Equal(t, 1, scalar)
		assert.Equal(t, 0, collection)
		assert.Equal(t, 1, c.GraphCount())
	})

	t.Run("commit-removes-stale", func(t *testing.T) {
		c, _ := newTestCoordinator(t)
		w := c.ForGraph(graphA)

		// Cycle 1: watch two resources.
		require.NoError(t, w.Watch(scalarReq("n1", gvrA, "cm-1", "ns")))
		require.NoError(t, w.Watch(scalarReq("n2", gvrA, "cm-2", "ns")))
		w.Done(true)
		s, _ := c.WatchRequestCount()
		assert.Equal(t, 2, s)

		// Cycle 2: drop n2 entirely. After Done(true) it must be gone.
		w2 := c.ForGraph(graphA)
		require.NoError(t, w2.Watch(scalarReq("n1", gvrA, "cm-1", "ns")))
		w2.Done(true)
		s, _ = c.WatchRequestCount()
		assert.Equal(t, 1, s)
	})

	t.Run("abort-discards-current-keeps-previous", func(t *testing.T) {
		c, _ := newTestCoordinator(t)
		w := c.ForGraph(graphA)

		// Establish baseline.
		require.NoError(t, w.Watch(scalarReq("n1", gvrA, "cm-1", "ns")))
		w.Done(true)

		// Aborted cycle adds a brand-new node — must be rolled back.
		w2 := c.ForGraph(graphA)
		require.NoError(t, w2.Watch(scalarReq("n1", gvrA, "cm-1", "ns")))
		require.NoError(t, w2.Watch(scalarReq("nNew", gvrB, "dep-1", "ns")))
		w2.Done(false)

		s, _ := c.WatchRequestCount()
		assert.Equal(t, 1, s, "rollback should remove the new node's index entry")
	})

	// Pins a bug found in audit: within a single reconcile cycle,
	// Watch(A) → Watch(B) → Watch(A) where previous-committed=A used
	// to leave the index empty for that nodeID — the second call
	// evicted A from the index, the third call short-circuited the
	// re-add because previous still matched A. After Done(true) the
	// node had no index entry and drift events were silently dropped.
	t.Run("retarget-roundtrip-restores-index", func(t *testing.T) {
		c, rec := newTestCoordinator(t)

		// Cycle 1: commit A.
		w := c.ForGraph(graphA)
		require.NoError(t, w.Watch(scalarReq("n1", gvrA, "cm-A", "ns")))
		w.Done(true)

		// Cycle 2: A → B → A. After the third call the index must
		// still route events to graphA for cm-A.
		w2 := c.ForGraph(graphA)
		require.NoError(t, w2.Watch(scalarReq("n1", gvrA, "cm-A", "ns")))
		require.NoError(t, w2.Watch(scalarReq("n1", gvrA, "cm-B", "ns")))
		require.NoError(t, w2.Watch(scalarReq("n1", gvrA, "cm-A", "ns")))
		w2.Done(true)

		c.RouteEvent(Event{Type: EventUpdate, GVR: gvrA, Name: "cm-A", Namespace: "ns"})
		assert.ElementsMatch(t, []client.ObjectKey{graphA}, rec.snapshot(),
			"retarget round-trip should leave A in the routing index")
	})

	t.Run("retarget-same-nodeID-replaces-entry", func(t *testing.T) {
		c, _ := newTestCoordinator(t)
		w := c.ForGraph(graphA)

		require.NoError(t, w.Watch(scalarReq("n1", gvrA, "cm-1", "ns")))
		w.Done(true)

		// Same nodeID, different name → stale "cm-1" entry must go.
		w2 := c.ForGraph(graphA)
		require.NoError(t, w2.Watch(scalarReq("n1", gvrA, "cm-renamed", "ns")))
		w2.Done(true)

		s, _ := c.WatchRequestCount()
		assert.Equal(t, 1, s)

		// Routing on the old name should match nothing now.
		c.RouteEvent(Event{Type: EventUpdate, GVR: gvrA, Name: "cm-1", Namespace: "ns"})
	})
}

// TestCoordinatorRouteScalar covers the scalar reverse index path: an
// event for the watched GVR+Name+Namespace must enqueue exactly the
// watching Graph(s), and nothing else.
func TestCoordinatorRouteScalar(t *testing.T) {
	graphA := client.ObjectKey{Namespace: "ns", Name: "a"}
	graphB := client.ObjectKey{Namespace: "ns", Name: "b"}

	tests := []struct {
		name  string
		setup func(c *Coordinator)
		event Event
		want  []client.ObjectKey
	}{
		{
			name: "single-graph-matches",
			setup: func(c *Coordinator) {
				w := c.ForGraph(graphA)
				_ = w.Watch(scalarReq("n", gvrA, "cm-1", "ns"))
				w.Done(true)
			},
			event: Event{Type: EventUpdate, GVR: gvrA, Name: "cm-1", Namespace: "ns"},
			want:  []client.ObjectKey{graphA},
		},
		{
			name: "two-graphs-on-same-resource-both-enqueue",
			setup: func(c *Coordinator) {
				wa := c.ForGraph(graphA)
				_ = wa.Watch(scalarReq("na", gvrA, "cm-1", "ns"))
				wa.Done(true)
				wb := c.ForGraph(graphB)
				_ = wb.Watch(scalarReq("nb", gvrA, "cm-1", "ns"))
				wb.Done(true)
			},
			event: Event{Type: EventUpdate, GVR: gvrA, Name: "cm-1", Namespace: "ns"},
			want:  []client.ObjectKey{graphA, graphB},
		},
		{
			name: "non-matching-name-is-ignored",
			setup: func(c *Coordinator) {
				w := c.ForGraph(graphA)
				_ = w.Watch(scalarReq("n", gvrA, "cm-1", "ns"))
				w.Done(true)
			},
			event: Event{Type: EventUpdate, GVR: gvrA, Name: "other", Namespace: "ns"},
			want:  nil,
		},
		{
			name: "non-matching-namespace-is-ignored",
			setup: func(c *Coordinator) {
				w := c.ForGraph(graphA)
				_ = w.Watch(scalarReq("n", gvrA, "cm-1", "ns"))
				w.Done(true)
			},
			event: Event{Type: EventUpdate, GVR: gvrA, Name: "cm-1", Namespace: "other"},
			want:  nil,
		},
		{
			name: "non-matching-gvr-is-ignored",
			setup: func(c *Coordinator) {
				w := c.ForGraph(graphA)
				_ = w.Watch(scalarReq("n", gvrA, "cm-1", "ns"))
				w.Done(true)
			},
			event: Event{Type: EventUpdate, GVR: gvrB, Name: "cm-1", Namespace: "ns"},
			want:  nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, rec := newTestCoordinator(t)
			tc.setup(c)
			c.RouteEvent(tc.event)
			assert.ElementsMatch(t, tc.want, rec.snapshot())
		})
	}
}

// TestCoordinatorRouteCollection exercises selector-based routing
// including the label-loss case where an object losing a matching label
// still triggers re-reconcile via OldLabels.
func TestCoordinatorRouteCollection(t *testing.T) {
	graphA := client.ObjectKey{Namespace: "ns", Name: "a"}

	matchApp := func() labels.Selector {
		sel, err := labels.Parse("app=svc")
		require.NoError(t, err)
		return sel
	}

	tests := []struct {
		name  string
		setup func(c *Coordinator)
		event Event
		want  []client.ObjectKey
	}{
		{
			name: "current-labels-match",
			setup: func(c *Coordinator) {
				w := c.ForGraph(graphA)
				_ = w.Watch(collectionReq("n", gvrA, "ns", matchApp()))
				w.Done(true)
			},
			event: Event{
				Type: EventUpdate, GVR: gvrA, Name: "p-1", Namespace: "ns",
				Labels: map[string]string{"app": "svc"},
			},
			want: []client.ObjectKey{graphA},
		},
		{
			name: "old-labels-match-routes-the-label-loss",
			setup: func(c *Coordinator) {
				w := c.ForGraph(graphA)
				_ = w.Watch(collectionReq("n", gvrA, "ns", matchApp()))
				w.Done(true)
			},
			event: Event{
				Type: EventUpdate, GVR: gvrA, Name: "p-1", Namespace: "ns",
				Labels:    map[string]string{"app": "other"},
				OldLabels: map[string]string{"app": "svc"},
			},
			want: []client.ObjectKey{graphA},
		},
		{
			name: "no-match-no-enqueue",
			setup: func(c *Coordinator) {
				w := c.ForGraph(graphA)
				_ = w.Watch(collectionReq("n", gvrA, "ns", matchApp()))
				w.Done(true)
			},
			event: Event{
				Type: EventUpdate, GVR: gvrA, Name: "p-1", Namespace: "ns",
				Labels: map[string]string{"app": "other"},
			},
			want: nil,
		},
		{
			name: "namespace-scope-respected",
			setup: func(c *Coordinator) {
				w := c.ForGraph(graphA)
				_ = w.Watch(collectionReq("n", gvrA, "ns", matchApp()))
				w.Done(true)
			},
			event: Event{
				Type: EventUpdate, GVR: gvrA, Name: "p-1", Namespace: "other",
				Labels: map[string]string{"app": "svc"},
			},
			want: nil,
		},
		{
			name: "cluster-wide-watch-matches-any-namespace",
			setup: func(c *Coordinator) {
				w := c.ForGraph(graphA)
				_ = w.Watch(collectionReq("n", gvrA, "", matchApp()))
				w.Done(true)
			},
			event: Event{
				Type: EventUpdate, GVR: gvrA, Name: "p-1", Namespace: "anywhere",
				Labels: map[string]string{"app": "svc"},
			},
			want: []client.ObjectKey{graphA},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, rec := newTestCoordinator(t)
			tc.setup(c)
			c.RouteEvent(tc.event)
			assert.ElementsMatch(t, tc.want, rec.snapshot())
		})
	}
}

// TestCoordinatorRemoveGraph removes every entry owned by a Graph and
// is safe to call for an unknown key.
func TestCoordinatorRemoveGraph(t *testing.T) {
	c, rec := newTestCoordinator(t)
	graphA := client.ObjectKey{Namespace: "ns", Name: "a"}
	graphB := client.ObjectKey{Namespace: "ns", Name: "b"}

	wa := c.ForGraph(graphA)
	require.NoError(t, wa.Watch(scalarReq("n1", gvrA, "cm-1", "ns")))
	require.NoError(t, wa.Watch(scalarReq("n2", gvrB, "dep-1", "ns")))
	wa.Done(true)

	wb := c.ForGraph(graphB)
	require.NoError(t, wb.Watch(scalarReq("n", gvrA, "cm-1", "ns")))
	wb.Done(true)

	c.RemoveGraph(graphA)
	scalar, _ := c.WatchRequestCount()
	assert.Equal(t, 1, scalar, "graphA's entries gone, graphB's stays")

	// Event on graphA's old resource should only enqueue graphB now.
	c.RouteEvent(Event{Type: EventUpdate, GVR: gvrA, Name: "cm-1", Namespace: "ns"})
	assert.ElementsMatch(t, []client.ObjectKey{graphB}, rec.snapshot())

	// Idempotent for unknown.
	c.RemoveGraph(client.ObjectKey{Namespace: "x", Name: "ghost"})
}

// TestSameWatchTarget pins the equivalence used to detect "is this
// request a no-op vs. a retarget?" Edge cases: nil, scalar vs. scalar,
// scalar vs. collection (never equal), collection selector equality by
// canonical string.
func TestSameWatchTarget(t *testing.T) {
	mkSel := func(s string) labels.Selector {
		sel, err := labels.Parse(s)
		require.NoError(t, err)
		return sel
	}

	tests := []struct {
		name string
		a, b *WatchRequest
		want bool
	}{
		{name: "both-nil", a: nil, b: nil, want: true},
		{name: "a-nil-b-not", a: nil, b: &WatchRequest{}, want: false},
		{name: "scalar-equal", a: &WatchRequest{GVR: gvrA, Name: "x", Namespace: "ns"}, b: &WatchRequest{GVR: gvrA, Name: "x", Namespace: "ns"}, want: true},
		{name: "scalar-diff-name", a: &WatchRequest{GVR: gvrA, Name: "x"}, b: &WatchRequest{GVR: gvrA, Name: "y"}, want: false},
		{name: "scalar-diff-namespace", a: &WatchRequest{GVR: gvrA, Name: "x", Namespace: "a"}, b: &WatchRequest{GVR: gvrA, Name: "x", Namespace: "b"}, want: false},
		{name: "scalar-diff-gvr", a: &WatchRequest{GVR: gvrA, Name: "x"}, b: &WatchRequest{GVR: gvrB, Name: "x"}, want: false},
		{name: "scalar-vs-collection", a: &WatchRequest{GVR: gvrA, Name: "x"}, b: &WatchRequest{GVR: gvrA, Selector: mkSel("app=svc")}, want: false},
		{name: "collection-equal", a: &WatchRequest{GVR: gvrA, Selector: mkSel("app=svc")}, b: &WatchRequest{GVR: gvrA, Selector: mkSel("app=svc")}, want: true},
		{name: "collection-diff-selector", a: &WatchRequest{GVR: gvrA, Selector: mkSel("app=svc")}, b: &WatchRequest{GVR: gvrA, Selector: mkSel("app=other")}, want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, sameWatchTarget(tc.a, tc.b))
		})
	}
}

// TestNoopWatcher locks in that NoopWatcher does nothing observable —
// the executor uses it when no DC is wired (CLI/tests).
func TestNoopWatcher(t *testing.T) {
	var w Watcher = NoopWatcher{}
	require.NoError(t, w.Watch(WatchRequest{GVR: gvrA, Name: "x"}))
	w.Done(true)
	w.Done(false)
}
