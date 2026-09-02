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
	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubernetes-sigs/kro/pkg/metrics"
	"github.com/kubernetes-sigs/kro/pkg/watch/coordinator"
)

// WatchRequest describes one resource a Graph wants tracked. It is an alias of
// the shared coordinator type so both controller stacks operate on one
// concrete request type.
type WatchRequest = coordinator.WatchRequest

// Watcher is the per-Graph handle a reconciler uses to declare which resources
// should re-enqueue the Graph on change. It is an alias of the shared
// coordinator Watcher interface.
type Watcher = coordinator.Watcher

// NoopWatcher discards every Watch/Done call. Use in tests or when no
// coordinator is wired (e.g. CLI / dry-run paths).
type NoopWatcher = coordinator.NoopWatcher

// EnqueueFunc is what the coordinator calls to request a Graph reconcile.
// Implementations push the key onto the controller's work queue or an upstream
// source.Channel.
type EnqueueFunc = coordinator.EnqueueFunc[client.ObjectKey]

// Coordinator aggregates watch requests across all Graphs, asks the Manager to
// retain informers as needed, and routes informer events back to the matching
// Graphs via the EnqueueFunc. It is a thin facade over the shared generic
// coordinator core, keyed by client.ObjectKey, plus a metrics observer.
type Coordinator struct {
	core *coordinator.Coordinator[client.ObjectKey]
}

// NewCoordinator wires a coordinator to a Manager and an enqueue callback.
func NewCoordinator(watches *Manager, enqueue EnqueueFunc, log logr.Logger) *Coordinator {
	core := coordinator.New(watches, enqueue, coordinator.Observer[client.ObjectKey](graphObserver{}), log)
	return &Coordinator{core: core}
}

// ForGraph returns a per-Graph Watcher. Call this at the top of every
// reconcile and Done(commit) at the end.
func (c *Coordinator) ForGraph(key client.ObjectKey) Watcher { return c.core.For(key) }

// RemoveGraph drops every watch owned by the Graph. Called on Graph deletion.
// Idempotent — calling for an unknown key is a no-op.
func (c *Coordinator) RemoveGraph(key client.ObjectKey) { c.core.Remove(key) }

// RouteEvent dispatches an event from the Manager to every Graph whose
// declared watch set covers it.
func (c *Coordinator) RouteEvent(event Event) { c.core.RouteEvent(event) }

// GraphCount returns the number of Graphs the coordinator currently tracks.
func (c *Coordinator) GraphCount() int { return c.core.OwnerCount() }

// WatchRequestCount returns the number of active scalar and collection watch
// requests across all Graphs.
func (c *Coordinator) WatchRequestCount() (scalar, collection int) {
	return c.core.WatchRequestCount()
}

// graphObserver implements coordinator.Observer[client.ObjectKey] and maps the
// coordinator lifecycle onto the graph-engine watch-router metrics. It ignores
// the owner key (graph keys are high-cardinality, so the owner gauge is a
// plain unlabeled count).
type graphObserver struct{}

func (graphObserver) OnAddOwner(client.ObjectKey)    { metrics.GraphWatchOwnerCount.Inc() }
func (graphObserver) OnRemoveOwner(client.ObjectKey) { metrics.GraphWatchOwnerCount.Dec() }

func (graphObserver) OnAddRequest(gvr schema.GroupVersionResource, collection bool) {
	metrics.GraphWatchRequestCount.WithLabelValues(gvr.String(), kindLabel(collection)).Inc()
}

func (graphObserver) OnRemoveRequest(gvr schema.GroupVersionResource, collection bool, n int) {
	metrics.GraphWatchRequestCount.WithLabelValues(gvr.String(), kindLabel(collection)).Sub(float64(n))
}

func (graphObserver) OnRoute(gvr schema.GroupVersionResource, matched bool) {
	metrics.GraphRouteTotal.WithLabelValues(gvr.String()).Inc()
	if matched {
		metrics.GraphRouteMatchTotal.WithLabelValues(gvr.String()).Inc()
	}
}

func kindLabel(collection bool) string {
	if collection {
		return "collection"
	}
	return "scalar"
}
