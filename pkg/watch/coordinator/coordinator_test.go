// Copyright 2025 The Kube Resource Orchestrator Authors
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

package coordinator

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/cache"

	kwatch "github.com/kubernetes-sigs/kro/pkg/watch"
)

var (
	gvrA = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}
	gvrB = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
)

// --- fakes -----------------------------------------------------------------

// enqueueRecorder captures the keys the coordinator asks to be requeued.
// Thread-safe so concurrent RouteEvent goroutines don't race.
type enqueueRecorder struct {
	mu   sync.Mutex
	keys []string
}

func (r *enqueueRecorder) fn() EnqueueFunc[string] {
	return func(k string) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.keys = append(r.keys, k)
	}
}

func (r *enqueueRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.keys...)
}

// recordingObserver counts the structural lifecycle callbacks so tests can
// assert the coordinator drives the Observer contract as documented.
type recordingObserver struct {
	mu             sync.Mutex
	addOwner       int
	removeOwner    int
	addRequest     int
	removeRequest  int
	removeRequestN int
	routeMatched   int
	routeUnmatched int
}

func (o *recordingObserver) OnAddOwner(string)    { o.mu.Lock(); o.addOwner++; o.mu.Unlock() }
func (o *recordingObserver) OnRemoveOwner(string) { o.mu.Lock(); o.removeOwner++; o.mu.Unlock() }
func (o *recordingObserver) OnAddRequest(schema.GroupVersionResource, bool) {
	o.mu.Lock()
	o.addRequest++
	o.mu.Unlock()
}
func (o *recordingObserver) OnRemoveRequest(_ schema.GroupVersionResource, _ bool, n int) {
	o.mu.Lock()
	o.removeRequest++
	o.removeRequestN += n
	o.mu.Unlock()
}
func (o *recordingObserver) OnRoute(_ schema.GroupVersionResource, matched bool) {
	o.mu.Lock()
	if matched {
		o.routeMatched++
	} else {
		o.routeUnmatched++
	}
	o.mu.Unlock()
}

// fakeInformer is a minimal cache.SharedIndexInformer whose HasSynced flag can
// be forced to never sync, which drives the Manager's EnsureWatch sync-timeout
// path (and thus the coordinator's rollback path).
type fakeInformer struct {
	mu        sync.Mutex
	handlers  []cache.ResourceEventHandler
	stopped   bool
	stopCh    chan struct{}
	neverSync bool
	synced    bool
}

func newFakeInformer(neverSync bool) *fakeInformer {
	return &fakeInformer{stopCh: make(chan struct{}), neverSync: neverSync}
}

func (f *fakeInformer) AddEventHandler(h cache.ResourceEventHandler) (cache.ResourceEventHandlerRegistration, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers = append(f.handlers, h)
	return &fakeReg{}, nil
}

func (f *fakeInformer) AddEventHandlerWithResyncPeriod(h cache.ResourceEventHandler, _ time.Duration) (cache.ResourceEventHandlerRegistration, error) {
	return f.AddEventHandler(h)
}

func (f *fakeInformer) AddEventHandlerWithOptions(h cache.ResourceEventHandler, _ cache.HandlerOptions) (cache.ResourceEventHandlerRegistration, error) {
	return f.AddEventHandler(h)
}

func (f *fakeInformer) RemoveEventHandler(_ cache.ResourceEventHandlerRegistration) error { return nil }
func (f *fakeInformer) GetStore() cache.Store                                             { return nil }
func (f *fakeInformer) GetController() cache.Controller                                   { return nil }

func (f *fakeInformer) Run(stopCh <-chan struct{}) {
	f.mu.Lock()
	if !f.neverSync {
		f.synced = true
	}
	f.mu.Unlock()
	<-stopCh
	f.mu.Lock()
	f.stopped = true
	close(f.stopCh)
	f.mu.Unlock()
}

func (f *fakeInformer) RunWithContext(ctx context.Context) { f.Run(ctx.Done()) }

func (f *fakeInformer) HasSynced() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.synced
}

func (f *fakeInformer) HasSyncedChecker() cache.DoneChecker {
	return fakeDoneChecker{synced: f.HasSynced()}
}

func (f *fakeInformer) LastSyncResourceVersion() string                      { return "" }
func (f *fakeInformer) SetWatchErrorHandler(_ cache.WatchErrorHandler) error { return nil }
func (f *fakeInformer) SetWatchErrorHandlerWithContext(_ cache.WatchErrorHandlerWithContext) error {
	return nil
}
func (f *fakeInformer) SetTransform(_ cache.TransformFunc) error { return nil }
func (f *fakeInformer) IsStopped() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stopped
}
func (f *fakeInformer) AddIndexers(_ cache.Indexers) error { return nil }
func (f *fakeInformer) GetIndexer() cache.Indexer          { return nil }

type fakeReg struct{}

func (fakeReg) HasSynced() bool { return true }
func (fakeReg) HasSyncedChecker() cache.DoneChecker {
	return fakeDoneChecker{synced: true}
}

type fakeDoneChecker struct{ synced bool }

func (fakeDoneChecker) Name() string { return "fake" }
func (c fakeDoneChecker) Done() <-chan struct{} {
	ch := make(chan struct{})
	if c.synced {
		close(ch)
	}
	return ch
}

// fakeInformerRegistry hands the Manager fake informers and remembers them so
// tests can assert on their stopped state. failGVRs forces a given GVR's
// informer to never sync, exercising EnsureWatch failure.
type fakeInformerRegistry struct {
	mu        sync.Mutex
	informers map[schema.GroupVersionResource]*fakeInformer
	failGVRs  map[schema.GroupVersionResource]bool
}

func (r *fakeInformerRegistry) create(gvr schema.GroupVersionResource) cache.SharedIndexInformer {
	r.mu.Lock()
	defer r.mu.Unlock()
	f := newFakeInformer(r.failGVRs[gvr])
	r.informers[gvr] = f
	return f
}

func (r *fakeInformerRegistry) get(gvr schema.GroupVersionResource) *fakeInformer {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.informers[gvr]
}

// newTestCoordinator wires a coordinator to a real Manager backed by fake
// informers with a short sync timeout. failGVRs marks GVRs whose informer
// never syncs, so EnsureWatch for them fails.
func newTestCoordinator(t *testing.T, obs Observer[string], failGVRs ...schema.GroupVersionResource) (*Coordinator[string], *enqueueRecorder, *fakeInformerRegistry) {
	t.Helper()
	reg := &fakeInformerRegistry{
		informers: make(map[schema.GroupVersionResource]*fakeInformer),
		failGVRs:  make(map[schema.GroupVersionResource]bool),
	}
	for _, gvr := range failGVRs {
		reg.failGVRs[gvr] = true
	}
	wm := kwatch.NewManager(nil, 0, func(kwatch.Event) {}, logr.Discard())
	wm.SyncTimeout = 200 * time.Millisecond
	wm.SetInformerFactory(reg.create)
	t.Cleanup(wm.Shutdown)

	rec := &enqueueRecorder{}
	c := New(wm, rec.fn(), obs, logr.Discard())
	return c, rec, reg
}

func scalarReq(nodeID string, gvr schema.GroupVersionResource, name, ns string) WatchRequest {
	return WatchRequest{NodeID: nodeID, GVR: gvr, Name: name, Namespace: ns}
}

func collectionReq(nodeID string, gvr schema.GroupVersionResource, ns string, sel labels.Selector) WatchRequest {
	return WatchRequest{NodeID: nodeID, GVR: gvr, Namespace: ns, Selector: sel}
}

func mustSelector(t *testing.T, s string) labels.Selector {
	t.Helper()
	sel, err := labels.Parse(s)
	require.NoError(t, err)
	return sel
}

// --- New / constructor -----------------------------------------------------

func TestNew_NilObserverInstallsNop(t *testing.T) {
	wm := kwatch.NewManager(nil, 0, func(kwatch.Event) {}, logr.Discard())
	c := New[string](wm, func(string) {}, nil, logr.Discard())
	require.NotNil(t, c)
	assert.IsType(t, NopObserver[string]{}, c.obs)
}

// --- registration / dedup / commit -----------------------------------------

func TestWatchAndDone_CommitInstallsScalar(t *testing.T) {
	c, _, reg := newTestCoordinator(t, nil)
	w := c.For("owner-a")
	require.NoError(t, w.Watch(scalarReq("n1", gvrA, "cm-1", "ns")))
	w.Done(true)

	scalar, collection := c.WatchRequestCount()
	assert.Equal(t, 1, scalar)
	assert.Equal(t, 0, collection)
	assert.Equal(t, 1, c.OwnerCount())
	// A successful EnsureWatch must have retained a running informer.
	assert.NotNil(t, reg.get(gvrA))
}

func TestWatchAndDone_CommitInstallsCollection(t *testing.T) {
	c, _, _ := newTestCoordinator(t, nil)
	w := c.For("owner-a")
	require.NoError(t, w.Watch(collectionReq("n1", gvrA, "ns", mustSelector(t, "app=svc"))))
	w.Done(true)

	scalar, collection := c.WatchRequestCount()
	assert.Equal(t, 0, scalar)
	assert.Equal(t, 1, collection)
}

func TestWatch_DuplicateNodeIDDedups(t *testing.T) {
	obs := &recordingObserver{}
	c, _, _ := newTestCoordinator(t, obs)
	w := c.For("owner-a")

	// Same nodeID + same target declared twice in one cycle: single index entry.
	require.NoError(t, w.Watch(scalarReq("n1", gvrA, "cm-1", "ns")))
	require.NoError(t, w.Watch(scalarReq("n1", gvrA, "cm-1", "ns")))
	w.Done(true)

	scalar, _ := c.WatchRequestCount()
	assert.Equal(t, 1, scalar, "duplicate declaration should dedup to one entry")

	obs.mu.Lock()
	defer obs.mu.Unlock()
	assert.Equal(t, 1, obs.addOwner)
	assert.Equal(t, 1, obs.addRequest, "dedup means OnAddRequest fires once")
}

func TestWatch_DistinctNodeIDsSameTargetBothIndexed(t *testing.T) {
	c, rec, _ := newTestCoordinator(t, nil)
	w := c.For("owner-a")

	require.NoError(t, w.Watch(scalarReq("n1", gvrA, "cm-1", "ns")))
	require.NoError(t, w.Watch(scalarReq("n2", gvrA, "cm-1", "ns")))
	w.Done(true)

	scalar, _ := c.WatchRequestCount()
	assert.Equal(t, 2, scalar, "two nodeIDs on the same target are two entries")

	// But routing still enqueues the single owner once (dedup by key).
	c.RouteEvent(kwatch.Event{Type: kwatch.EventUpdate, GVR: gvrA, Name: "cm-1", Namespace: "ns"})
	assert.ElementsMatch(t, []string{"owner-a"}, rec.snapshot())
}

func TestWatchAndDone_CommitRemovesStale(t *testing.T) {
	c, _, _ := newTestCoordinator(t, nil)

	w := c.For("owner-a")
	require.NoError(t, w.Watch(scalarReq("n1", gvrA, "cm-1", "ns")))
	require.NoError(t, w.Watch(scalarReq("n2", gvrA, "cm-2", "ns")))
	w.Done(true)
	s, _ := c.WatchRequestCount()
	assert.Equal(t, 2, s)

	// Second cycle drops n2. After Done(true) only n1 remains.
	w2 := c.For("owner-a")
	require.NoError(t, w2.Watch(scalarReq("n1", gvrA, "cm-1", "ns")))
	w2.Done(true)
	s, _ = c.WatchRequestCount()
	assert.Equal(t, 1, s)
}

func TestWatchAndDone_RetargetSameNodeIDReplacesEntry(t *testing.T) {
	c, rec, _ := newTestCoordinator(t, nil)

	w := c.For("owner-a")
	require.NoError(t, w.Watch(scalarReq("n1", gvrA, "cm-1", "ns")))
	w.Done(true)

	// Same nodeID, different name → stale "cm-1" entry must be evicted.
	w2 := c.For("owner-a")
	require.NoError(t, w2.Watch(scalarReq("n1", gvrA, "cm-renamed", "ns")))
	w2.Done(true)

	s, _ := c.WatchRequestCount()
	assert.Equal(t, 1, s)

	// Old name no longer routes.
	c.RouteEvent(kwatch.Event{Type: kwatch.EventUpdate, GVR: gvrA, Name: "cm-1", Namespace: "ns"})
	assert.Empty(t, rec.snapshot())

	// New name routes.
	c.RouteEvent(kwatch.Event{Type: kwatch.EventUpdate, GVR: gvrA, Name: "cm-renamed", Namespace: "ns"})
	assert.ElementsMatch(t, []string{"owner-a"}, rec.snapshot())
}

// Regression (KREP-024 / PR #1355): a single graph node that renders items of
// MULTIPLE distinct resource types (GVRs) — e.g. a dynamic collection producing
// a mix of ConfigMaps and Deployments — must keep a live watch per resource
// type. State keyed by NodeID alone collapsed these into one entry, so the
// second Watch call overwrote the first and only the last GVR retained a watch.
// Keying by (NodeID, GVR) keeps both alive.
func TestWatch_SameNodeIDDistinctGVRsBothSurvive(t *testing.T) {
	c, rec, reg := newTestCoordinator(t, nil)

	w := c.For("owner-a")
	// Same NodeID, two distinct resource types.
	require.NoError(t, w.Watch(scalarReq("n1", gvrA, "cm-1", "ns")))
	require.NoError(t, w.Watch(scalarReq("n1", gvrB, "dep-1", "ns")))
	w.Done(true)

	scalar, _ := c.WatchRequestCount()
	assert.Equal(t, 2, scalar, "one node with two GVRs must yield two watch entries")

	// Both informers must be retained — not just the last-registered GVR.
	assert.NotNil(t, reg.get(gvrA), "watch for the first GVR must survive")
	assert.NotNil(t, reg.get(gvrB), "watch for the second GVR must survive")

	// Changes to the FIRST resource type must still re-enqueue the owner.
	c.RouteEvent(kwatch.Event{Type: kwatch.EventUpdate, GVR: gvrA, Name: "cm-1", Namespace: "ns"})
	assert.Len(t, rec.snapshot(), 1, "first GVR must route")
	assert.Equal(t, "owner-a", rec.snapshot()[0])

	// And so must changes to the second (recorder accumulates across routes).
	c.RouteEvent(kwatch.Event{Type: kwatch.EventUpdate, GVR: gvrB, Name: "dep-1", Namespace: "ns"})
	assert.ElementsMatch(t, []string{"owner-a", "owner-a"}, rec.snapshot(), "second GVR must also route")
}

// Retargeting a scalar watch under a node keeps the SAME GVR (only Name
// changes), so it must still collide on the (NodeID, GVR) key and replace in
// place — the multi-GVR keying must not turn a rename into a duplicate.
func TestWatch_SameNodeIDSameGVRRetargetStillReplaces(t *testing.T) {
	c, rec, _ := newTestCoordinator(t, nil)

	w := c.For("owner-a")
	require.NoError(t, w.Watch(scalarReq("n1", gvrA, "cm-1", "ns")))
	require.NoError(t, w.Watch(scalarReq("n1", gvrA, "cm-renamed", "ns")))
	w.Done(true)

	s, _ := c.WatchRequestCount()
	assert.Equal(t, 1, s, "same (node, GVR) retarget replaces in place")

	c.RouteEvent(kwatch.Event{Type: kwatch.EventUpdate, GVR: gvrA, Name: "cm-1", Namespace: "ns"})
	assert.Empty(t, rec.snapshot(), "stale name must not route")
	c.RouteEvent(kwatch.Event{Type: kwatch.EventUpdate, GVR: gvrA, Name: "cm-renamed", Namespace: "ns"})
	assert.ElementsMatch(t, []string{"owner-a"}, rec.snapshot())
}

// Regression: within one cycle Watch(A)→Watch(B)→Watch(A) with previous==A
// used to leave the index empty for that nodeID. After Done(true) A must
// still route.
func TestWatch_RetargetRoundTripRestoresIndex(t *testing.T) {
	c, rec, _ := newTestCoordinator(t, nil)

	w := c.For("owner-a")
	require.NoError(t, w.Watch(scalarReq("n1", gvrA, "cm-A", "ns")))
	w.Done(true)

	w2 := c.For("owner-a")
	require.NoError(t, w2.Watch(scalarReq("n1", gvrA, "cm-A", "ns")))
	require.NoError(t, w2.Watch(scalarReq("n1", gvrA, "cm-B", "ns")))
	require.NoError(t, w2.Watch(scalarReq("n1", gvrA, "cm-A", "ns")))
	w2.Done(true)

	c.RouteEvent(kwatch.Event{Type: kwatch.EventUpdate, GVR: gvrA, Name: "cm-A", Namespace: "ns"})
	assert.ElementsMatch(t, []string{"owner-a"}, rec.snapshot())
}

// --- abort / rollback of in-flight cycle ------------------------------------

func TestDone_AbortDiscardsCurrentKeepsPrevious(t *testing.T) {
	c, _, _ := newTestCoordinator(t, nil)

	w := c.For("owner-a")
	require.NoError(t, w.Watch(scalarReq("n1", gvrA, "cm-1", "ns")))
	w.Done(true)

	// Aborted cycle adds a brand-new node — must be rolled back.
	w2 := c.For("owner-a")
	require.NoError(t, w2.Watch(scalarReq("n1", gvrA, "cm-1", "ns")))
	require.NoError(t, w2.Watch(scalarReq("nNew", gvrB, "dep-1", "ns")))
	w2.Done(false)

	s, _ := c.WatchRequestCount()
	assert.Equal(t, 1, s, "abort should remove the new node's index entry")
}

func TestDone_AbortWithNoPreviousDropsOwner(t *testing.T) {
	obs := &recordingObserver{}
	c, _, _ := newTestCoordinator(t, obs)

	// First-ever cycle for this owner, then abort: owner is dropped entirely.
	w := c.For("owner-a")
	require.NoError(t, w.Watch(scalarReq("n1", gvrA, "cm-1", "ns")))
	w.Done(false)

	assert.Equal(t, 0, c.OwnerCount())
	s, col := c.WatchRequestCount()
	assert.Equal(t, 0, s)
	assert.Equal(t, 0, col)

	obs.mu.Lock()
	defer obs.mu.Unlock()
	assert.Equal(t, 1, obs.addOwner)
	assert.Equal(t, 1, obs.removeOwner, "aborting a never-committed owner removes it")
}

// --- EnsureWatch failure / rollback (the reviewer's focus) ------------------

func TestWatch_EnsureWatchFailureRollsBack(t *testing.T) {
	obs := &recordingObserver{}
	// gvrA's informer never syncs → EnsureWatch times out and returns an error.
	c, rec, reg := newTestCoordinator(t, obs, gvrA)

	w := c.For("owner-a")
	err := w.Watch(scalarReq("n1", gvrA, "cm-1", "ns"))
	require.Error(t, err, "a failed informer sync must surface as an error")
	assert.Contains(t, err.Error(), "ensure watch")

	// Rollback: the entry that was optimistically added must be gone.
	s, col := c.WatchRequestCount()
	assert.Equal(t, 0, s, "failed EnsureWatch must roll back the scalar index entry")
	assert.Equal(t, 0, col)

	// And an event for the rolled-back target routes to nobody.
	c.RouteEvent(kwatch.Event{Type: kwatch.EventUpdate, GVR: gvrA, Name: "cm-1", Namespace: "ns"})
	assert.Empty(t, rec.snapshot())

	// The Manager should not retain a broken informer for gvrA.
	if inf := reg.get(gvrA); inf != nil {
		assert.Eventually(t, inf.IsStopped, time.Second, 5*time.Millisecond,
			"broken informer should be released after sync failure")
	}

	// Observer saw the add then the compensating remove.
	obs.mu.Lock()
	defer obs.mu.Unlock()
	assert.Equal(t, 1, obs.addRequest)
	assert.Equal(t, 1, obs.removeRequest)
	assert.Equal(t, 1, obs.removeRequestN)
}

func TestWatch_EnsureWatchFailureKeepsPriorCommittedEntry(t *testing.T) {
	// gvrB's informer never syncs. gvrA is healthy.
	c, rec, _ := newTestCoordinator(t, nil, gvrB)

	// Commit a healthy scalar watch on gvrA.
	w := c.For("owner-a")
	require.NoError(t, w.Watch(scalarReq("n1", gvrA, "cm-1", "ns")))
	w.Done(true)

	// Next cycle re-declares n1 (still fine) and adds a failing gvrB watch.
	w2 := c.For("owner-a")
	require.NoError(t, w2.Watch(scalarReq("n1", gvrA, "cm-1", "ns")))
	require.Error(t, w2.Watch(scalarReq("n2", gvrB, "dep-1", "ns")))

	// The failing add rolled itself back; the committed gvrA entry is intact.
	c.RouteEvent(kwatch.Event{Type: kwatch.EventUpdate, GVR: gvrA, Name: "cm-1", Namespace: "ns"})
	assert.ElementsMatch(t, []string{"owner-a"}, rec.snapshot())

	// gvrB never made it into the index.
	scalar, _ := c.WatchRequestCount()
	assert.Equal(t, 1, scalar)
}

// --- routing ----------------------------------------------------------------

func TestRouteEvent_Scalar(t *testing.T) {
	tests := []struct {
		name  string
		setup func(c *Coordinator[string])
		event kwatch.Event
		want  []string
	}{
		{
			name: "single-owner-matches",
			setup: func(c *Coordinator[string]) {
				w := c.For("a")
				_ = w.Watch(scalarReq("n", gvrA, "cm-1", "ns"))
				w.Done(true)
			},
			event: kwatch.Event{Type: kwatch.EventUpdate, GVR: gvrA, Name: "cm-1", Namespace: "ns"},
			want:  []string{"a"},
		},
		{
			name: "two-owners-same-resource-both-enqueue",
			setup: func(c *Coordinator[string]) {
				wa := c.For("a")
				_ = wa.Watch(scalarReq("na", gvrA, "cm-1", "ns"))
				wa.Done(true)
				wb := c.For("b")
				_ = wb.Watch(scalarReq("nb", gvrA, "cm-1", "ns"))
				wb.Done(true)
			},
			event: kwatch.Event{Type: kwatch.EventUpdate, GVR: gvrA, Name: "cm-1", Namespace: "ns"},
			want:  []string{"a", "b"},
		},
		{
			name: "non-matching-name-ignored",
			setup: func(c *Coordinator[string]) {
				w := c.For("a")
				_ = w.Watch(scalarReq("n", gvrA, "cm-1", "ns"))
				w.Done(true)
			},
			event: kwatch.Event{Type: kwatch.EventUpdate, GVR: gvrA, Name: "other", Namespace: "ns"},
			want:  nil,
		},
		{
			name: "non-matching-namespace-ignored",
			setup: func(c *Coordinator[string]) {
				w := c.For("a")
				_ = w.Watch(scalarReq("n", gvrA, "cm-1", "ns"))
				w.Done(true)
			},
			event: kwatch.Event{Type: kwatch.EventUpdate, GVR: gvrA, Name: "cm-1", Namespace: "other"},
			want:  nil,
		},
		{
			name: "non-matching-gvr-ignored",
			setup: func(c *Coordinator[string]) {
				w := c.For("a")
				_ = w.Watch(scalarReq("n", gvrA, "cm-1", "ns"))
				w.Done(true)
			},
			event: kwatch.Event{Type: kwatch.EventUpdate, GVR: gvrB, Name: "cm-1", Namespace: "ns"},
			want:  nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, rec, _ := newTestCoordinator(t, nil)
			tc.setup(c)
			c.RouteEvent(tc.event)
			assert.ElementsMatch(t, tc.want, rec.snapshot())
		})
	}
}

func TestRouteEvent_Collection(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, c *Coordinator[string])
		event kwatch.Event
		want  []string
	}{
		{
			name: "current-labels-match",
			setup: func(t *testing.T, c *Coordinator[string]) {
				w := c.For("a")
				_ = w.Watch(collectionReq("n", gvrA, "ns", mustSelector(t, "app=svc")))
				w.Done(true)
			},
			event: kwatch.Event{
				Type: kwatch.EventUpdate, GVR: gvrA, Name: "p-1", Namespace: "ns",
				Labels: map[string]string{"app": "svc"},
			},
			want: []string{"a"},
		},
		{
			name: "old-labels-match-routes-label-loss",
			setup: func(t *testing.T, c *Coordinator[string]) {
				w := c.For("a")
				_ = w.Watch(collectionReq("n", gvrA, "ns", mustSelector(t, "app=svc")))
				w.Done(true)
			},
			event: kwatch.Event{
				Type: kwatch.EventUpdate, GVR: gvrA, Name: "p-1", Namespace: "ns",
				Labels:    map[string]string{"app": "other"},
				OldLabels: map[string]string{"app": "svc"},
			},
			want: []string{"a"},
		},
		{
			name: "no-match-no-enqueue",
			setup: func(t *testing.T, c *Coordinator[string]) {
				w := c.For("a")
				_ = w.Watch(collectionReq("n", gvrA, "ns", mustSelector(t, "app=svc")))
				w.Done(true)
			},
			event: kwatch.Event{
				Type: kwatch.EventUpdate, GVR: gvrA, Name: "p-1", Namespace: "ns",
				Labels: map[string]string{"app": "other"},
			},
			want: nil,
		},
		{
			name: "namespace-scope-respected",
			setup: func(t *testing.T, c *Coordinator[string]) {
				w := c.For("a")
				_ = w.Watch(collectionReq("n", gvrA, "ns", mustSelector(t, "app=svc")))
				w.Done(true)
			},
			event: kwatch.Event{
				Type: kwatch.EventUpdate, GVR: gvrA, Name: "p-1", Namespace: "other",
				Labels: map[string]string{"app": "svc"},
			},
			want: nil,
		},
		{
			name: "cluster-wide-matches-any-namespace",
			setup: func(t *testing.T, c *Coordinator[string]) {
				w := c.For("a")
				_ = w.Watch(collectionReq("n", gvrA, "", mustSelector(t, "app=svc")))
				w.Done(true)
			},
			event: kwatch.Event{
				Type: kwatch.EventUpdate, GVR: gvrA, Name: "p-1", Namespace: "anywhere",
				Labels: map[string]string{"app": "svc"},
			},
			want: []string{"a"},
		},
		{
			// A DoesNotExist selector matches the EMPTY label set. An object that
			// gains the label on update (old={} -> new={app:svc}) leaves the
			// selector, so the previously-matched owner must still be enqueued
			// via the empty OldLabels set. A len(OldLabels)>0 guard would drop it.
			name: "empty-old-labels-match-doesnotexist-on-update",
			setup: func(t *testing.T, c *Coordinator[string]) {
				w := c.For("a")
				_ = w.Watch(collectionReq("n", gvrA, "ns", mustSelector(t, "!app")))
				w.Done(true)
			},
			event: kwatch.Event{
				Type: kwatch.EventUpdate, GVR: gvrA, Name: "p-1", Namespace: "ns",
				Labels:    map[string]string{"app": "svc"},
				OldLabels: nil,
			},
			want: []string{"a"},
		},
		{
			// An Add event carries no OldLabels; the empty-set old-label branch
			// only fires on updates, so a non-matching Add must NOT enqueue.
			name: "add-with-nonmatching-labels-no-enqueue",
			setup: func(t *testing.T, c *Coordinator[string]) {
				w := c.For("a")
				_ = w.Watch(collectionReq("n", gvrA, "ns", mustSelector(t, "!app")))
				w.Done(true)
			},
			event: kwatch.Event{
				Type: kwatch.EventAdd, GVR: gvrA, Name: "p-1", Namespace: "ns",
				Labels: map[string]string{"app": "svc"},
			},
			want: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, rec, _ := newTestCoordinator(t, nil)
			tc.setup(t, c)
			c.RouteEvent(tc.event)
			assert.ElementsMatch(t, tc.want, rec.snapshot())
		})
	}
}

func TestRouteEvent_ObserverMatchAndMiss(t *testing.T) {
	obs := &recordingObserver{}
	c, _, _ := newTestCoordinator(t, obs)

	w := c.For("a")
	require.NoError(t, w.Watch(scalarReq("n", gvrA, "cm-1", "ns")))
	w.Done(true)

	c.RouteEvent(kwatch.Event{Type: kwatch.EventUpdate, GVR: gvrA, Name: "cm-1", Namespace: "ns"})
	c.RouteEvent(kwatch.Event{Type: kwatch.EventUpdate, GVR: gvrA, Name: "miss", Namespace: "ns"})

	obs.mu.Lock()
	defer obs.mu.Unlock()
	assert.Equal(t, 1, obs.routeMatched)
	assert.Equal(t, 1, obs.routeUnmatched)
}

// --- teardown / removal / refcount teardown ---------------------------------

func TestRemove_DropsOwnerAndTearsDownOrphanedWatch(t *testing.T) {
	c, rec, reg := newTestCoordinator(t, nil)

	wa := c.For("a")
	require.NoError(t, wa.Watch(scalarReq("n1", gvrA, "cm-1", "ns")))
	require.NoError(t, wa.Watch(scalarReq("n2", gvrB, "dep-1", "ns")))
	wa.Done(true)

	wb := c.For("b")
	require.NoError(t, wb.Watch(scalarReq("n", gvrA, "cm-1", "ns")))
	wb.Done(true)

	// gvrA is retained by both a and b; gvrB only by a.
	infB := reg.get(gvrB)
	require.NotNil(t, infB)

	c.Remove("a")
	scalar, _ := c.WatchRequestCount()
	assert.Equal(t, 1, scalar, "only b's cm-1 entry survives")
	assert.Equal(t, 1, c.OwnerCount())

	// gvrB had only owner a → it must be torn down (refcount hit zero).
	assert.Eventually(t, infB.IsStopped, time.Second, 5*time.Millisecond,
		"orphaned gvrB informer should stop after its last owner is removed")

	// gvrA still routes to b.
	c.RouteEvent(kwatch.Event{Type: kwatch.EventUpdate, GVR: gvrA, Name: "cm-1", Namespace: "ns"})
	assert.ElementsMatch(t, []string{"b"}, rec.snapshot())
}

func TestRemove_CollectionWatchTornDown(t *testing.T) {
	c, rec, reg := newTestCoordinator(t, nil)

	w := c.For("a")
	require.NoError(t, w.Watch(collectionReq("n", gvrA, "ns", mustSelector(t, "app=svc"))))
	w.Done(true)
	_, col := c.WatchRequestCount()
	assert.Equal(t, 1, col)

	inf := reg.get(gvrA)
	require.NotNil(t, inf)

	// Remove exercises removeCollectionIndexLocked and orphaned teardown.
	c.Remove("a")
	_, col = c.WatchRequestCount()
	assert.Equal(t, 0, col, "collection index entry must be removed")
	assert.Eventually(t, inf.IsStopped, time.Second, 5*time.Millisecond,
		"orphaned collection informer should stop")

	c.RouteEvent(kwatch.Event{
		Type: kwatch.EventUpdate, GVR: gvrA, Name: "p-1", Namespace: "ns",
		Labels: map[string]string{"app": "svc"},
	})
	assert.Empty(t, rec.snapshot())
}

// TestDone_CommitRemovesStaleCollection drives removeCollectionIndexLocked via
// the commit-diff path: a collection node dropped in the next cycle is torn
// down on Done(true).
func TestDone_CommitRemovesStaleCollection(t *testing.T) {
	c, _, _ := newTestCoordinator(t, nil)

	w := c.For("a")
	require.NoError(t, w.Watch(collectionReq("n1", gvrA, "ns", mustSelector(t, "app=svc"))))
	require.NoError(t, w.Watch(collectionReq("n2", gvrB, "ns", mustSelector(t, "app=web"))))
	w.Done(true)
	_, col := c.WatchRequestCount()
	assert.Equal(t, 2, col)

	// Drop n2 next cycle.
	w2 := c.For("a")
	require.NoError(t, w2.Watch(collectionReq("n1", gvrA, "ns", mustSelector(t, "app=svc"))))
	w2.Done(true)
	_, col = c.WatchRequestCount()
	assert.Equal(t, 1, col)
}

func TestRemove_UnknownKeyIsNoop(t *testing.T) {
	c, _, _ := newTestCoordinator(t, nil)
	// Should not panic and leaves state empty.
	c.Remove("ghost")
	assert.Equal(t, 0, c.OwnerCount())
}

func TestRemove_SharedGVRNotTornDownUntilLastOwner(t *testing.T) {
	c, _, reg := newTestCoordinator(t, nil)

	wa := c.For("a")
	require.NoError(t, wa.Watch(scalarReq("na", gvrA, "cm-1", "ns")))
	wa.Done(true)
	wb := c.For("b")
	require.NoError(t, wb.Watch(scalarReq("nb", gvrA, "cm-2", "ns")))
	wb.Done(true)

	inf := reg.get(gvrA)
	require.NotNil(t, inf)

	// Removing a leaves b still referencing gvrA → informer stays up.
	c.Remove("a")
	assert.False(t, inf.IsStopped(), "shared informer must survive while another owner holds it")

	// Removing b drops the last reference → informer tears down.
	c.Remove("b")
	assert.Eventually(t, inf.IsStopped, time.Second, 5*time.Millisecond,
		"informer should stop only after the last owner is removed")
}

func TestRemoveWhere(t *testing.T) {
	obs := &recordingObserver{}
	c, rec, _ := newTestCoordinator(t, obs)

	for _, name := range []string{"keep-1", "drop-1", "drop-2"} {
		w := c.For(name)
		require.NoError(t, w.Watch(scalarReq("n", gvrA, "cm-"+name, "ns")))
		w.Done(true)
	}
	assert.Equal(t, 3, c.OwnerCount())

	c.RemoveWhere(func(k string) bool { return k == "drop-1" || k == "drop-2" })
	assert.Equal(t, 1, c.OwnerCount())

	// Only keep-1 routes now.
	c.RouteEvent(kwatch.Event{Type: kwatch.EventUpdate, GVR: gvrA, Name: "cm-keep-1", Namespace: "ns"})
	c.RouteEvent(kwatch.Event{Type: kwatch.EventUpdate, GVR: gvrA, Name: "cm-drop-1", Namespace: "ns"})
	assert.ElementsMatch(t, []string{"keep-1"}, rec.snapshot())

	obs.mu.Lock()
	defer obs.mu.Unlock()
	assert.Equal(t, 2, obs.removeOwner, "OnRemoveOwner fires once per removed owner")
}

func TestRemoveWhere_NoMatchIsNoop(t *testing.T) {
	c, _, _ := newTestCoordinator(t, nil)
	w := c.For("a")
	require.NoError(t, w.Watch(scalarReq("n", gvrA, "cm-1", "ns")))
	w.Done(true)

	c.RemoveWhere(func(string) bool { return false })
	assert.Equal(t, 1, c.OwnerCount())
}

// --- SameWatchTarget --------------------------------------------------------

func TestSameWatchTarget(t *testing.T) {
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
		{name: "scalar-vs-collection", a: &WatchRequest{GVR: gvrA, Name: "x"}, b: &WatchRequest{GVR: gvrA, Selector: mustSelector(t, "app=svc")}, want: false},
		{name: "collection-equal", a: &WatchRequest{GVR: gvrA, Selector: mustSelector(t, "app=svc")}, b: &WatchRequest{GVR: gvrA, Selector: mustSelector(t, "app=svc")}, want: true},
		{name: "collection-diff-selector", a: &WatchRequest{GVR: gvrA, Selector: mustSelector(t, "app=svc")}, b: &WatchRequest{GVR: gvrA, Selector: mustSelector(t, "app=other")}, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, SameWatchTarget(tc.a, tc.b))
		})
	}
}

// --- NoopWatcher ------------------------------------------------------------

func TestNoopWatcher(t *testing.T) {
	var w Watcher = NoopWatcher{}
	require.NoError(t, w.Watch(WatchRequest{GVR: gvrA, Name: "x"}))
	w.Done(true)
	w.Done(false)
}

// --- concurrency safety -----------------------------------------------------

func TestConcurrentWatchRouteRemove(t *testing.T) {
	c, _, _ := newTestCoordinator(t, nil)

	var wg sync.WaitGroup
	var routes atomic.Int64

	// Writers: repeatedly declare + commit watches for several owners.
	for i := range 8 {
		key := "owner-" + string(rune('a'+i))
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 25 {
				w := c.For(key)
				_ = w.Watch(scalarReq("n", gvrA, "cm-1", "ns"))
				w.Done(true)
				c.Remove(key)
			}
		}()
	}

	// Readers: route events concurrently.
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				c.RouteEvent(kwatch.Event{Type: kwatch.EventUpdate, GVR: gvrA, Name: "cm-1", Namespace: "ns"})
				routes.Add(1)
			}
		}()
	}

	wg.Wait()
	// No assertion on exact counts; the test passes if the race detector and
	// the coordinator's locks kept everything consistent (no panic/deadlock).
	assert.Equal(t, int64(200), routes.Load())
}
