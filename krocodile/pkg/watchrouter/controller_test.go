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
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

// newTestDC builds a Router whose Manager uses a fake
// informer factory. We bypass NewRouter because we need to
// inject the fake before any owners attach.
func newTestDC(t *testing.T) (*Router, *fakeInformerRegistry) {
	t.Helper()
	reg := &fakeInformerRegistry{informers: make(map[schema.GroupVersionResource]*fakeInformer)}
	dc := &Router{
		log:    logr.Discard(),
		events: make(chan event.GenericEvent, 16),
	}
	dc.watches = NewManager(nil, 0, dc.routeEvent, logr.Discard())
	dc.watches.SyncTimeout = 500 * time.Millisecond
	dc.watches.createInformer = func(gvr schema.GroupVersionResource) cache.SharedIndexInformer {
		return reg.create(gvr)
	}
	dc.coordinator = NewCoordinator(dc.watches, dc.enqueue, logr.Discard())
	t.Cleanup(dc.watches.Shutdown)
	return dc, reg
}

// TestDCForGraphAndRemove plus the integration with Source: a Watch
// registered through ForGraph must cause an event on Source's channel
// when the underlying informer fires the matching resource.
func TestDCForGraphAndRemove(t *testing.T) {
	dc, reg := newTestDC(t)
	graphA := client.ObjectKey{Namespace: "ns", Name: "g-a"}

	w := dc.ForGraph(graphA)
	require.NoError(t, w.Watch(scalarReq("n", gvrA, "cm-1", "ns")))
	w.Done(true)

	require.Equal(t, 1, dc.Coordinator().GraphCount())

	// Drive an event through the informer and assert it surfaces via
	// the controller-runtime event channel as a GenericEvent on graphA.
	reg.get(gvrA).fireUpdate(
		newFakeObj("ns", "cm-1", nil),
		newFakeObj("ns", "cm-1", nil),
	)

	select {
	case ev := <-dc.events:
		assert.Equal(t, "g-a", ev.Object.GetName())
		assert.Equal(t, "ns", ev.Object.GetNamespace())
	case <-time.After(time.Second):
		t.Fatal("expected event for graphA, none arrived")
	}

	// RemoveGraph drops the watch — subsequent informer events must
	// produce nothing on the channel.
	dc.RemoveGraph(graphA)
	assert.Equal(t, 0, dc.Coordinator().GraphCount())
	reg.get(gvrA).fireUpdate(
		newFakeObj("ns", "cm-1", nil),
		newFakeObj("ns", "cm-1", nil),
	)
	select {
	case ev := <-dc.events:
		t.Fatalf("unexpected event after RemoveGraph: %v", ev.Object)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestDCSourceWiresGraphEvents covers the source.Channel handshake by
// reading directly from the events chan — the production handler is
// supplied at Source() time and ultimately exercised in integration
// tests, but the upstream end is owned here.
func TestDCSourceWiresGraphEvents(t *testing.T) {
	dc, reg := newTestDC(t)
	graphA := client.ObjectKey{Namespace: "ns", Name: "g-a"}

	w := dc.ForGraph(graphA)
	require.NoError(t, w.Watch(scalarReq("n", gvrA, "cm-1", "ns")))
	w.Done(true)

	src := dc.Source()
	require.NotNil(t, src)

	// Trigger an informer event and consume from the events chan: the
	// source wraps this same channel, so confirming the producer side
	// is sufficient for the unit-test layer.
	reg.get(gvrA).fireAdd(newFakeObj("ns", "cm-1", nil))
	select {
	case ev := <-dc.events:
		assert.Equal(t, graphA.Name, ev.Object.GetName())
	case <-time.After(time.Second):
		t.Fatal("no event observed")
	}
}

// TestDCStartShutsDownInformers verifies the manager.Runnable Start
// hook waits on ctx then drains the Manager. Informers running
// before Start must stop before Start returns.
func TestDCStartShutsDownInformers(t *testing.T) {
	dc, reg := newTestDC(t)
	w := dc.ForGraph(client.ObjectKey{Namespace: "ns", Name: "g"})
	require.NoError(t, w.Watch(scalarReq("n", gvrA, "cm-1", "ns")))
	w.Done(true)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- dc.Start(ctx) }()

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("Start did not return after ctx cancel")
	}

	assert.Eventually(t, reg.get(gvrA).IsStopped, time.Second, 5*time.Millisecond)
	assert.True(t, dc.closed.Load())
	assert.True(t, dc.NeedLeaderElection())
}

// TestDCEnqueueDropOnClosed covers the edge case where the DC has been
// shut down but the coordinator still has live state — enqueue must
// short-circuit silently rather than racing into a half-closed channel.
func TestDCEnqueueDropOnClosed(t *testing.T) {
	dc, _ := newTestDC(t)
	dc.closed.Store(true)
	dc.enqueue(client.ObjectKey{Namespace: "ns", Name: "g"})
	select {
	case ev := <-dc.events:
		t.Fatalf("unexpected event after shutdown: %v", ev.Object)
	case <-time.After(50 * time.Millisecond):
	}
}

// TestDCString returns a non-empty summary for logging.
func TestDCString(t *testing.T) {
	dc, _ := newTestDC(t)
	w := dc.ForGraph(client.ObjectKey{Namespace: "ns", Name: "g"})
	require.NoError(t, w.Watch(scalarReq("n", gvrA, "cm-1", "ns")))
	w.Done(true)
	got := dc.String()
	assert.Contains(t, got, "dynamic-controller")
	assert.Contains(t, got, "graphs=1")
}
