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

// Package watchrouter manages a fleet of metadata-only informers
// for arbitrary GVRs and routes their events back to the Graph that
// declared a watch.
//
// The shape is intentionally simpler than kro's equivalent: there is no
// parent CRD generated per RGD — the Graph is the only owner the
// coordinator knows about. controller-runtime watches the Graph itself;
// this package only handles the child/observed resources.
//
// Lifecycle:
//
//  1. NewRouter builds the manager + coordinator.
//  2. Adding the controller to a manager (mgr.Add) gives it a Start hook
//     that simply waits for ctx cancellation and then drains informers.
//  3. WatchesRawSource(dc.Source()) on the Graph builder wires the
//     coordinator's enqueue callback into the controller-runtime queue.
//  4. The Graph reconciler calls ForGraph(key) at the top of every
//     reconcile, Watch() per node, and Done(true)/Done(false) at the end.
//  5. On Graph deletion the reconciler calls RemoveGraph(key) so the
//     coordinator drops every retention.
package watchrouter

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/metadata"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

// Config tunes the Router. Defaults are sensible for typical
// loads; bump EventBuffer if you expect bursts of informer events to
// outpace reconciles.
type Config struct {
	// ResyncPeriod is the informer relist interval. Zero disables resync
	// (events only); enable it (e.g. 10m) if you want belt-and-braces
	// drift detection on top of watch events.
	ResyncPeriod time.Duration
	// SyncTimeout caps how long EnsureWatch waits for a new informer's
	// initial list to populate the cache. Zero falls back to 30s.
	SyncTimeout time.Duration
	// EventBuffer is the size of the channel between coordinator and the
	// controller-runtime source. A full buffer makes informer callbacks
	// block until reconciles drain — preferred over dropping events.
	EventBuffer int
}

// Router is the integration point between the per-GVR
// Manager + Coordinator and controller-runtime's Reconciler.
// It exposes a source.Source that the Graph builder consumes via
// WatchesRawSource.
type Router struct {
	log logr.Logger

	watches     *Manager
	coordinator *Coordinator

	events chan event.GenericEvent
	closed atomic.Bool
}

// NewRouter constructs a controller wired around the given
// metadata client. The returned controller is not running yet — add it
// to a manager so its Start hook executes.
func NewRouter(log logr.Logger, cfg Config, metaClient metadata.Interface) *Router {
	bufSize := cfg.EventBuffer
	if bufSize <= 0 {
		bufSize = 4096
	}

	dc := &Router{
		log:    log.WithName("dynamic-controller"),
		events: make(chan event.GenericEvent, bufSize),
	}

	dc.watches = NewManager(metaClient, cfg.ResyncPeriod, dc.routeEvent, dc.log)
	if cfg.SyncTimeout > 0 {
		dc.watches.SyncTimeout = cfg.SyncTimeout
	}
	dc.coordinator = NewCoordinator(dc.watches, dc.enqueue, dc.log)

	return dc
}

// Coordinator exposes the underlying Coordinator. Most callers
// should prefer ForGraph; Coordinator is here for tests and metrics.
func (dc *Router) Coordinator() *Coordinator { return dc.coordinator }

// ForGraph returns the per-Graph Watcher handle. Forwards to the
// coordinator; lifted here so callers don't need to know about the
// coordinator at all.
func (dc *Router) ForGraph(key client.ObjectKey) Watcher {
	return dc.coordinator.ForGraph(key)
}

// RemoveGraph tears down every watch owned by the given Graph. Call
// from the reconciler when the Graph is being finalized.
func (dc *Router) RemoveGraph(key client.ObjectKey) {
	dc.coordinator.RemoveGraph(key)
}

// Source returns the source.Source to register with the Graph builder
// via WatchesRawSource. Internally a buffered channel pumps events from
// the coordinator into controller-runtime's work queue.
func (dc *Router) Source() source.Source {
	return source.Channel(dc.events, &handler.EnqueueRequestForObject{})
}

// Start implements manager.Runnable. It blocks until ctx is canceled,
// then drains and shuts down the Manager. The event channel stays
// open so concurrent enqueue calls don't panic — only the underlying
// informers stop.
func (dc *Router) Start(ctx context.Context) error {
	dc.log.Info("Starting dynamic controller")
	<-ctx.Done()
	dc.log.Info("Shutting down dynamic controller")
	dc.watches.Shutdown()
	dc.closed.Store(true)
	return nil
}

// NeedLeaderElection implements manager.LeaderElectionRunnable. The
// dynamic controller participates in leader election with the rest of
// the manager — keeping watches and enqueues authoritative on a single
// replica.
func (dc *Router) NeedLeaderElection() bool { return true }

var _ manager.Runnable = (*Router)(nil)
var _ manager.LeaderElectionRunnable = (*Router)(nil)

// routeEvent is the EventHandler given to Manager. It forwards to
// the coordinator's RouteEvent for indexing + enqueue.
func (dc *Router) routeEvent(e Event) {
	dc.coordinator.RouteEvent(e)
}

// enqueue is the EnqueueFunc given to the coordinator. It synthesizes a
// minimal Graph object (just Name + Namespace) and pushes it down the
// event channel. controller-runtime's EnqueueRequestForObject reads the
// metadata back out as a reconcile.Request.
func (dc *Router) enqueue(key client.ObjectKey) {
	if dc.closed.Load() {
		return
	}
	u := &unstructured.Unstructured{}
	u.SetName(key.Name)
	u.SetNamespace(key.Namespace)
	select {
	case dc.events <- event.GenericEvent{Object: u}:
	default:
		// Buffer overflow shouldn't happen with the default 4k buffer
		// unless reconciles wedge. Log so we know if it does — drift
		// would silently stop converging otherwise.
		dc.log.V(0).Info("event buffer full; dropping enqueue", "graph", key)
	}
}

// Manager returns the underlying Manager (tests + metrics).
func (dc *Router) Manager() *Manager { return dc.watches }

// String reports the count of active watches and tracked graphs, useful
// in logs.
func (dc *Router) String() string {
	scalar, collection := dc.coordinator.WatchRequestCount()
	return fmt.Sprintf("dynamic-controller{graphs=%d, gvrs=%d, scalar=%d, collection=%d}",
		dc.coordinator.GraphCount(), dc.watches.ActiveWatchCount(), scalar, collection)
}
