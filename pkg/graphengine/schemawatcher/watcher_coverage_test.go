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

package schemawatcher

import (
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TestNewDefaultsEventBuffer pins the EventBuffer default branch in New:
// a zero/unset buffer falls back to the 1024 default rather than a
// zero-capacity channel that would block the informer goroutine.
func TestNewDefaultsEventBuffer(t *testing.T) {
	cases := []struct {
		name    string
		buffer  int
		wantCap int
	}{
		{name: "unset-uses-default", buffer: 0, wantCap: 1024},
		{name: "negative-uses-default", buffer: -5, wantCap: 1024},
		{name: "explicit-buffer-honored", buffer: 8, wantCap: 8},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sw := New(logr.Discard(), Config{EventBuffer: tc.buffer})
			assert.Equal(t, tc.wantCap, cap(sw.events))
		})
	}
}

// TestToCRD covers every branch of the obj->CRD coercion: the pointer
// case, the value case, a DeletedFinalStateUnknown unwrap (via onDelete),
// and the non-CRD fallbacks that return nil.
func TestToCRD(t *testing.T) {
	cases := []struct {
		name   string
		obj    any
		wantOK bool
	}{
		{
			name:   "pointer-CRD",
			obj:    makeCRD("example.com", "Widget", "v0"),
			wantOK: true,
		},
		{
			name:   "value-CRD",
			obj:    *makeCRD("example.com", "Widget", "v0"),
			wantOK: true,
		},
		{
			name:   "non-CRD-object-with-metadata-returns-nil",
			obj:    &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cm"}},
			wantOK: false,
		},
		{
			name:   "non-meta-type-returns-nil",
			obj:    "not even a k8s object",
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := toCRD(tc.obj)
			if tc.wantOK {
				assert.NotNil(t, got)
			} else {
				assert.Nil(t, got)
			}
		})
	}
}

// TestEventHandlersIgnoreNonCRD ensures onAdd/onUpdate/onDelete short
// circuit when toCRD yields nil — no panic, no enqueue.
func TestEventHandlersIgnoreNonCRD(t *testing.T) {
	junk := "not a CRD"
	cases := []struct {
		name string
		fire func(sw *SchemaWatcher)
	}{
		{name: "onAdd-ignores", fire: func(sw *SchemaWatcher) { sw.onAdd(junk) }},
		{name: "onUpdate-ignores", fire: func(sw *SchemaWatcher) { sw.onUpdate(junk, junk) }},
		{name: "onDelete-ignores", fire: func(sw *SchemaWatcher) { sw.onDelete(junk) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sw, _, _ := newTestWatcher(t)
			tc.fire(sw)
			assert.Equal(t, 0, len(sw.events))
		})
	}
}

// TestOnDeleteUnwrapsTombstone covers the DeletedFinalStateUnknown
// branch in onDelete: the informer wraps the last-known CRD in a
// tombstone when a delete is observed after a relist gap.
func TestOnDeleteUnwrapsTombstone(t *testing.T) {
	sw, _, _ := newTestWatcher(t)
	gk := schema.GroupKind{Group: "example.com", Kind: "Widget"}
	g := client.ObjectKey{Namespace: "ns", Name: "g"}

	s := sw.ForGraph(g)
	s.Track(gk)
	s.Done(true)

	sw.onAdd(makeCRD("example.com", "Widget", "seed"))
	drainEvents(sw, 50*time.Millisecond)

	tombstone := cache.DeletedFinalStateUnknown{
		Key: "widgets.example.com",
		Obj: makeCRD("example.com", "Widget", "seed"),
	}
	sw.onDelete(tombstone)

	assert.Empty(t, sw.SchemaHash(gk), "tombstoned delete must still evict the hash")
	got := drainEvents(sw, 100*time.Millisecond)
	assert.ElementsMatch(t, []client.ObjectKey{g}, got)
}

// TestNotifyWithNoSubscribers covers the len(keys)==0 logging branch of
// notify: a CRD changes that no Graph tracks. No enqueue, no invalidator
// call for any graph, but the schema invalidator still fires.
func TestNotifyWithNoSubscribers(t *testing.T) {
	sw, gi, si := newTestWatcher(t)

	// No subscriptions at all. An Update for an unknown GK should hit
	// the empty-keys branch.
	sw.onAdd(makeCRD("lonely.io", "Nobody", "v0"))
	drainEvents(sw, 50*time.Millisecond)
	si.calls = nil
	gi.calls = nil

	sw.onUpdate(
		makeCRD("lonely.io", "Nobody", "v0"),
		makeCRD("lonely.io", "Nobody", "v1"),
	)
	got := drainEvents(sw, 50*time.Millisecond)
	assert.Empty(t, got, "no graph subscribed → nothing enqueued")
	assert.Empty(t, gi.snapshot(), "no graph invalidations with zero subscribers")
	// Schema invalidator still notified — the GK content changed.
	assert.ElementsMatch(t, []schema.GroupKind{{Group: "lonely.io", Kind: "Nobody"}}, si.snapshot())
}

// TestRemoveGraphWithInFlightSubscription covers the currentGKs and
// currentDyn loops in RemoveGraph: a Graph removed mid-cycle (Track
// called, Done not yet) must still have its in-flight index entries
// cleaned up.
func TestRemoveGraphWithInFlightSubscription(t *testing.T) {
	sw, _, _ := newTestWatcher(t)
	g := client.ObjectKey{Namespace: "ns", Name: "g"}
	gkCommitted := schema.GroupKind{Group: "apps", Kind: "Deployment"}
	gkInFlight := schema.GroupKind{Group: "batch", Kind: "Job"}

	// Cycle 1: commit Deployment + dynamic.
	s := sw.ForGraph(g)
	s.Track(gkCommitted)
	s.TrackDynamic()
	s.Done(true)

	// Cycle 2: start tracking Job (in-flight, no Done), keep dynamic.
	s2 := sw.ForGraph(g)
	s2.Track(gkInFlight)
	s2.TrackDynamic()
	// no Done — both committed (prevGKs) and in-flight (currentGKs) live.

	sw.RemoveGraph(g)

	assert.Empty(t, sw.GraphsForGroupKind(gkCommitted), "committed GK must be removed")
	assert.Empty(t, sw.GraphsForGroupKind(gkInFlight), "in-flight GK must be removed")
	assert.Empty(t, sw.DynamicGraphs(), "dynamic entry must be removed")
	assert.Equal(t, 0, sw.SubscribedGraphs())
}

// TestAbortDynamicOnlyCycle covers the abort path where a Graph claims
// dynamic in-flight without a previous dynamic commit: abort must drop
// it from the dynamic set. Also exercises commit/abort on an absent
// subscription (the sub == nil early returns).
func TestAbortDynamicOnlyCycle(t *testing.T) {
	t.Run("abort-rolls-back-fresh-dynamic", func(t *testing.T) {
		sw, _, _ := newTestWatcher(t)
		g := client.ObjectKey{Namespace: "ns", Name: "g"}

		s := sw.ForGraph(g)
		s.TrackDynamic() // in-flight, never previously committed
		s.Done(false)    // abort

		assert.Empty(t, sw.DynamicGraphs(), "fresh dynamic claim must roll back on abort")
	})

	t.Run("commit-on-untracked-graph-is-noop", func(t *testing.T) {
		sw, _, _ := newTestWatcher(t)
		// ForGraph then immediately Done(true) without any Track: the
		// sub map has no entry, so commit returns early.
		sw.ForGraph(client.ObjectKey{Namespace: "ns", Name: "never-tracked"}).Done(true)
		assert.Equal(t, 0, sw.SubscribedGraphs())
	})

	t.Run("abort-on-untracked-graph-is-noop", func(t *testing.T) {
		sw, _, _ := newTestWatcher(t)
		sw.ForGraph(client.ObjectKey{Namespace: "ns", Name: "never-tracked"}).Done(false)
		assert.Equal(t, 0, sw.SubscribedGraphs())
	})
}

// TestTrackDynamicIdempotent covers the early-return in TrackDynamic
// when the flag is already set within the same cycle.
func TestTrackDynamicIdempotent(t *testing.T) {
	sw, _, _ := newTestWatcher(t)
	g := client.ObjectKey{Namespace: "ns", Name: "g"}

	s := sw.ForGraph(g)
	s.TrackDynamic()
	s.TrackDynamic() // second call hits the already-set early return
	s.Done(true)

	assert.ElementsMatch(t, []client.ObjectKey{g}, sw.DynamicGraphs())
}

// TestHashCRDSchemaNil pins the nil-crd guard: a nil CRD hashes to the
// empty string rather than panicking.
func TestHashCRDSchemaNil(t *testing.T) {
	assert.Equal(t, "", hashCRDSchema(nil))
}

// TestNeedLeaderElection pins the leader-election contract: the watcher
// only runs on the active leader since its index is process-local.
func TestNeedLeaderElection(t *testing.T) {
	sw := New(logr.Discard(), Config{})
	assert.True(t, sw.NeedLeaderElection())
}

// TestSourceReturnsChannel confirms Source wires a non-nil source around
// the events channel. (Wiring it into a manager needs a cluster; this
// just guards against a nil return.)
func TestSourceReturnsChannel(t *testing.T) {
	sw := New(logr.Discard(), Config{})
	assert.NotNil(t, sw.Source())
}

// keep apiextensionsv1 import referenced for the value-CRD case above.
var _ = apiextensionsv1.CustomResourceDefinition{}
