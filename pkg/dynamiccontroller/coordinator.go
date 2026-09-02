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
	"sync"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/kubernetes-sigs/kro/pkg/metrics"
	kwatch "github.com/kubernetes-sigs/kro/pkg/watch"
	"github.com/kubernetes-sigs/kro/pkg/watch/coordinator"
)

// WatchRequest describes a resource the instance reconciler wants to watch. It
// is an alias of the shared coordinator type so both controller stacks operate
// on one concrete request type.
type WatchRequest = coordinator.WatchRequest

// InstanceWatcher is the interface the instance reconciler uses to request
// watches. It is scoped to a single instance and obtained via
// WatchCoordinator.ForInstance(). It is an alias of the shared coordinator
// Watcher interface.
type InstanceWatcher = coordinator.Watcher

// NoopInstanceWatcher is a no-op implementation of InstanceWatcher for use in
// tests or when no coordinator is available.
type NoopInstanceWatcher = coordinator.NoopWatcher

// EnqueueFunc is called by the coordinator to enqueue an instance for
// re-reconciliation when one of its watched resources changes. It remains the
// public facade signature (parentGVR + instance) that existing callers use.
type EnqueueFunc func(parentGVR schema.GroupVersionResource, instance types.NamespacedName)

// instanceKey uniquely identifies an instance across all RGDs.
type instanceKey struct {
	parentGVR schema.GroupVersionResource
	instance  types.NamespacedName
}

// WatchCoordinator aggregates watch requests from all instances, manages
// shared watches via the WatchManager, and routes events back to the correct
// instances. It is a thin facade over the shared generic coordinator core,
// keyed by instanceKey, plus a metrics observer that reproduces the dynamic
// controller's Prometheus semantics.
type WatchCoordinator struct {
	core    *coordinator.Coordinator[instanceKey]
	watches *kwatch.Manager
}

// NewWatchCoordinator creates a new WatchCoordinator.
func NewWatchCoordinator(watches *kwatch.Manager, enqueue EnqueueFunc, log logr.Logger) *WatchCoordinator {
	core := coordinator.New(
		watches,
		func(k instanceKey) { enqueue(k.parentGVR, k.instance) },
		coordinator.Observer[instanceKey](newDynObserver()),
		log,
	)
	return &WatchCoordinator{core: core, watches: watches}
}

// ForInstance returns a scoped InstanceWatcher handle for the given instance.
func (c *WatchCoordinator) ForInstance(parentGVR schema.GroupVersionResource, instance types.NamespacedName) InstanceWatcher {
	return c.core.For(instanceKey{parentGVR: parentGVR, instance: instance})
}

// RemoveInstance removes all watch requests for a specific instance. Called
// when an instance is deleted.
func (c *WatchCoordinator) RemoveInstance(parentGVR schema.GroupVersionResource, instance types.NamespacedName) {
	c.core.Remove(instanceKey{parentGVR: parentGVR, instance: instance})
}

// RemoveParentGVR removes all instances for a given parent GVR. Called when an
// RGD is deregistered.
func (c *WatchCoordinator) RemoveParentGVR(parentGVR schema.GroupVersionResource) {
	c.core.RemoveWhere(func(k instanceKey) bool { return k.parentGVR == parentGVR })
}

// RouteEvent routes a watch event to all matching instances. Called by the
// watch handler for every event.
func (c *WatchCoordinator) RouteEvent(event kwatch.Event) {
	c.core.RouteEvent(event)
}

// InstanceWatchCount returns the number of tracked instances.
func (c *WatchCoordinator) InstanceWatchCount() int {
	return c.core.OwnerCount()
}

// WatchRequestCount returns the total number of active watch requests.
func (c *WatchCoordinator) WatchRequestCount() (scalar, collection int) {
	return c.core.WatchRequestCount()
}

// dynObserver implements coordinator.Observer[instanceKey] and reproduces the
// exact metrics.Dyn* label lifecycle the standalone coordinator used to
// maintain. It keeps its own mutex-guarded counters so the delete-when-last
// semantics are preserved without depending on core internals.
type dynObserver struct {
	mu      sync.Mutex
	owners  map[string]int // parentGVR.String() -> live instance count
	scalarN map[string]int // gvr.String() -> live scalar request count
	collN   map[string]int // gvr.String() -> live collection request count
}

func newDynObserver() *dynObserver {
	return &dynObserver{
		owners:  make(map[string]int),
		scalarN: make(map[string]int),
		collN:   make(map[string]int),
	}
}

func (o *dynObserver) OnAddOwner(k instanceKey) {
	p := k.parentGVR.String()
	o.mu.Lock()
	o.owners[p]++
	o.mu.Unlock()
	metrics.DynInstanceWatchCount.WithLabelValues(p).Inc()
}

func (o *dynObserver) OnRemoveOwner(k instanceKey) {
	p := k.parentGVR.String()
	o.mu.Lock()
	o.owners[p]--
	remaining := o.owners[p]
	if remaining <= 0 {
		delete(o.owners, p)
	}
	o.mu.Unlock()
	// Preserve the old decInstanceWatchCount / RemoveParentGVR behavior:
	// decrement while other instances remain, otherwise delete the label set
	// entirely so a re-registration starts clean.
	if remaining > 0 {
		metrics.DynInstanceWatchCount.WithLabelValues(p).Dec()
	} else {
		metrics.DynInstanceWatchCount.DeleteLabelValues(p)
	}
}

func (o *dynObserver) OnAddRequest(gvr schema.GroupVersionResource, collection bool) {
	g := gvr.String()
	kind := "scalar"
	o.mu.Lock()
	if collection {
		kind = "collection"
		o.collN[g]++
	} else {
		o.scalarN[g]++
	}
	o.mu.Unlock()
	metrics.DynWatchRequestCount.WithLabelValues(g, kind).Inc()
}

func (o *dynObserver) OnRemoveRequest(gvr schema.GroupVersionResource, collection bool, n int) {
	g := gvr.String()
	kind := "scalar"
	if collection {
		kind = "collection"
	}
	metrics.DynWatchRequestCount.WithLabelValues(g, kind).Sub(float64(n))

	o.mu.Lock()
	if collection {
		o.collN[g] -= n
	} else {
		o.scalarN[g] -= n
	}
	if o.scalarN[g] <= 0 {
		delete(o.scalarN, g)
	}
	if o.collN[g] <= 0 {
		delete(o.collN, g)
	}
	_, hasScalar := o.scalarN[g]
	_, hasColl := o.collN[g]
	bothZero := !hasScalar && !hasColl
	o.mu.Unlock()

	// When a GVR has no remaining scalar AND collection requests, delete both
	// label sets — matching the old index-emptiness cleanup.
	if bothZero {
		metrics.DynWatchRequestCount.DeleteLabelValues(g, "scalar")
		metrics.DynWatchRequestCount.DeleteLabelValues(g, "collection")
	}
}

func (o *dynObserver) OnRoute(gvr schema.GroupVersionResource, matched bool) {
	metrics.DynRouteTotal.WithLabelValues(gvr.String()).Inc()
	if matched {
		metrics.DynRouteMatchTotal.WithLabelValues(gvr.String()).Inc()
	}
}
