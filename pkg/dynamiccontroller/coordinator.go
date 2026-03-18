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
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// InstanceWatcher is the interface the instance reconciler uses to request
// watches. It is scoped to a single instance and obtained via
// WatchCoordinator.ForInstance().
type InstanceWatcher interface {
	// Watch requests that the instance be re-reconciled when the specified
	// resource changes. Call this for every resource (managed or external)
	// the instance cares about.
	//
	// For scalar resources: set Name + Namespace.
	// For collections: set Selector + Namespace.
	Watch(req WatchRequest) error

	// Done signals that all Watch() calls for this reconciliation cycle
	// are complete. Any watch requests from the previous cycle that were
	// NOT re-requested are automatically cleaned up. If commit is false, the
	// current cycle is discarded and the previously committed watch set stays
	// active.
	Done(commit bool)
}

// WatchRequest describes a resource the instance reconciler wants to watch.
// For scalar resources, set Name + Namespace.
// For collections, set Selector + Namespace. Selector supports both
// matchLabels and matchExpressions (the full metav1.LabelSelector spec).
type WatchRequest struct {
	// NodeID is the graph node ID (for debugging/metrics).
	NodeID string
	// GVR is the GroupVersionResource to watch.
	GVR schema.GroupVersionResource
	// Name is the specific resource name (scalar watches).
	Name string
	// Namespace is the resource namespace. Empty for cluster-scoped resources.
	Namespace string
	// Selector is a label selector for collection watches. nil means scalar watch.
	Selector labels.Selector
}

// isCollection returns true if this is a selector-based collection watch.
func (r *WatchRequest) isCollection() bool {
	return r.Selector != nil
}

// EnqueueFunc is called by the coordinator to enqueue an instance for
// re-reconciliation when one of its watched resources changes.
type EnqueueFunc func(parentGVR schema.GroupVersionResource, instance types.NamespacedName)

// instanceKey uniquely identifies an instance across all RGDs.
type instanceKey struct {
	parentGVR schema.GroupVersionResource
	instance  types.NamespacedName
}

// watchIdentity identifies a watch by its content (GVR + target).
// Used for diffing between reconciliation cycles instead of nodeID-based identity.
type watchIdentity struct {
	gvr       schema.GroupVersionResource
	name      string
	namespace string
	selector  string // Selector.String() for collections, "" for scalar
}

func identityOf(r WatchRequest) watchIdentity {
	sel := ""
	if r.Selector != nil {
		sel = r.Selector.String()
	}
	return watchIdentity{gvr: r.GVR, name: r.Name, namespace: r.Namespace, selector: sel}
}

// scalarRouteKey identifies a specific scalar resource in the reverse index.
type scalarRouteKey struct {
	gvr       schema.GroupVersionResource
	name      string
	namespace string
}

// collectionRoute is a single collection watch in the reverse index.
type collectionRoute struct {
	selector  labels.Selector
	namespace string
	key       instanceKey
}

// WatchCoordinator aggregates watch requests from all instances, manages
// shared watches via WatchManager, and routes events back to the correct
// instances.
//
// Watch requests are collected locally by each instanceWatcher and committed
// atomically when Done(true) is called. This avoids locking the coordinator
// during the reconciliation loop and eliminates per-cycle current/previous
// tracking.
type WatchCoordinator struct {
	mu sync.RWMutex

	watches *WatchManager
	enqueue EnqueueFunc
	log     logr.Logger

	// Per-instance: the last committed watch set.
	committed map[instanceKey][]WatchRequest

	// Reverse indexes for event routing.
	scalarRoutes     map[scalarRouteKey]map[instanceKey]struct{}
	collectionRoutes map[schema.GroupVersionResource][]collectionRoute

	// Reference count per GVR across both indexes, for orphan detection.
	gvrRefCount map[schema.GroupVersionResource]int
}

// NewWatchCoordinator creates a new WatchCoordinator.
func NewWatchCoordinator(watches *WatchManager, enqueue EnqueueFunc, log logr.Logger) *WatchCoordinator {
	return &WatchCoordinator{
		watches:          watches,
		enqueue:          enqueue,
		log:              log.WithName("watch-coordinator"),
		committed:        make(map[instanceKey][]WatchRequest),
		scalarRoutes:     make(map[scalarRouteKey]map[instanceKey]struct{}),
		collectionRoutes: make(map[schema.GroupVersionResource][]collectionRoute),
		gvrRefCount:      make(map[schema.GroupVersionResource]int),
	}
}

// ForInstance returns a scoped InstanceWatcher handle for the given instance.
// Watch() calls on the returned handle accumulate locally; nothing is written
// to the coordinator until Done(true) is called.
func (c *WatchCoordinator) ForInstance(parentGVR schema.GroupVersionResource, instance types.NamespacedName) InstanceWatcher {
	return &instanceWatcher{
		coordinator: c,
		key:         instanceKey{parentGVR: parentGVR, instance: instance},
	}
}

// commitWatches atomically updates the watch set for an instance by diffing
// the new requests against the previously committed set.
func (c *WatchCoordinator) commitWatches(key instanceKey, newReqs []WatchRequest) {
	newReqs = dedupWatchRequests(newReqs)

	c.mu.Lock()

	oldReqs, existed := c.committed[key]

	// Nothing to do if there were no watches before and none now.
	if !existed && len(newReqs) == 0 {
		c.mu.Unlock()
		return
	}

	// Build identity sets for diffing.
	oldSet := make(map[watchIdentity]WatchRequest, len(oldReqs))
	for _, r := range oldReqs {
		oldSet[identityOf(r)] = r
	}
	newSet := make(map[watchIdentity]WatchRequest, len(newReqs))
	for _, r := range newReqs {
		newSet[identityOf(r)] = r
	}

	// Remove routes no longer needed.
	var removedGVRs []schema.GroupVersionResource
	for id, old := range oldSet {
		if _, ok := newSet[id]; !ok {
			c.removeRouteLocked(key, old)
			removedGVRs = append(removedGVRs, old.GVR)
		}
	}

	// Add routes that are new.
	ensureGVRs := make(map[schema.GroupVersionResource]struct{})
	for id, req := range newSet {
		if _, ok := oldSet[id]; !ok {
			c.addRouteLocked(key, req)
			ensureGVRs[req.GVR] = struct{}{}
		}
	}

	// Update committed state.
	if len(newReqs) > 0 {
		c.committed[key] = newReqs
		if !existed {
			instanceWatchCount.WithLabelValues(key.parentGVR.String()).Inc()
		}
	} else if existed {
		delete(c.committed, key)
		c.decInstanceWatchCount(key.parentGVR)
	}

	orphanedGVRs := c.findOrphanedGVRsLocked(removedGVRs)
	c.mu.Unlock()

	// Ensure informers for new GVRs (outside lock).
	for gvr := range ensureGVRs {
		if err := c.watches.EnsureWatch(gvr, "coordinator"); err != nil {
			c.log.Error(err, "Failed to ensure watch", "gvr", gvr)
		}
	}
	c.stopWatches(orphanedGVRs)
}

// RemoveInstance removes all watch requests for a specific instance.
// Called when an instance is deleted.
func (c *WatchCoordinator) RemoveInstance(parentGVR schema.GroupVersionResource, instance types.NamespacedName) {
	key := instanceKey{parentGVR: parentGVR, instance: instance}

	c.mu.Lock()

	reqs, ok := c.committed[key]
	if !ok {
		c.mu.Unlock()
		return
	}

	var affectedGVRs []schema.GroupVersionResource
	for _, req := range reqs {
		c.removeRouteLocked(key, req)
		affectedGVRs = append(affectedGVRs, req.GVR)
	}

	delete(c.committed, key)
	c.decInstanceWatchCount(parentGVR)

	orphanedGVRs := c.findOrphanedGVRsLocked(affectedGVRs)
	c.mu.Unlock()

	c.stopWatches(orphanedGVRs)
}

// RemoveParentGVR removes all instances for a given parent GVR.
// Called when an RGD is deregistered.
func (c *WatchCoordinator) RemoveParentGVR(parentGVR schema.GroupVersionResource) {
	c.mu.Lock()

	var toRemove []instanceKey
	for key := range c.committed {
		if key.parentGVR == parentGVR {
			toRemove = append(toRemove, key)
		}
	}

	var affectedGVRs []schema.GroupVersionResource
	for _, key := range toRemove {
		for _, req := range c.committed[key] {
			c.removeRouteLocked(key, req)
			affectedGVRs = append(affectedGVRs, req.GVR)
		}
		delete(c.committed, key)
	}
	c.decInstanceWatchCount(parentGVR)

	orphanedGVRs := c.findOrphanedGVRsLocked(affectedGVRs)
	c.mu.Unlock()

	c.stopWatches(orphanedGVRs)
}

// RouteEvent routes a watch event to all matching instances.
// Called by the watch handler for every event.
func (c *WatchCoordinator) RouteEvent(event Event) {
	routeTotal.WithLabelValues(event.GVR.String()).Inc()

	c.mu.RLock()
	matched := make(map[instanceKey]struct{})

	// Scalar matches (O(1) lookup).
	rk := scalarRouteKey{gvr: event.GVR, name: event.Name, namespace: event.Namespace}
	for key := range c.scalarRoutes[rk] {
		matched[key] = struct{}{}
	}

	// Collection matches (selector scan).
	// Match against both current and old labels so that an object losing
	// matching labels still triggers re-reconciliation.
	for _, entry := range c.collectionRoutes[event.GVR] {
		if entry.namespace != "" && event.Namespace != entry.namespace {
			continue
		}
		if entry.selector.Matches(labels.Set(event.Labels)) {
			matched[entry.key] = struct{}{}
		} else if len(event.OldLabels) > 0 && entry.selector.Matches(labels.Set(event.OldLabels)) {
			matched[entry.key] = struct{}{}
		}
	}
	c.mu.RUnlock()

	for key := range matched {
		c.enqueue(key.parentGVR, key.instance)
	}
	if len(matched) > 0 {
		routeMatchTotal.WithLabelValues(event.GVR.String()).Inc()
		c.log.V(2).Info("Routed event", "gvr", event.GVR, "name", event.Name, "namespace", event.Namespace, "type", event.Type)
	}
}

// InstanceWatchCount returns the number of tracked instances.
func (c *WatchCoordinator) InstanceWatchCount() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.committed)
}

// WatchRequestCount returns the total number of active watch requests.
func (c *WatchCoordinator) WatchRequestCount() (scalar, collection int) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	for _, keys := range c.scalarRoutes {
		scalar += len(keys)
	}
	for _, entries := range c.collectionRoutes {
		collection += len(entries)
	}
	return
}

// --- internal helpers ---

func (c *WatchCoordinator) addRouteLocked(key instanceKey, req WatchRequest) {
	if req.isCollection() {
		c.collectionRoutes[req.GVR] = append(c.collectionRoutes[req.GVR], collectionRoute{
			selector:  req.Selector,
			namespace: req.Namespace,
			key:       key,
		})
		watchRequestCount.WithLabelValues(req.GVR.String(), "collection").Inc()
	} else {
		rk := scalarRouteKey{gvr: req.GVR, name: req.Name, namespace: req.Namespace}
		if c.scalarRoutes[rk] == nil {
			c.scalarRoutes[rk] = make(map[instanceKey]struct{})
		}
		c.scalarRoutes[rk][key] = struct{}{}
		watchRequestCount.WithLabelValues(req.GVR.String(), "scalar").Inc()
	}
	c.gvrRefCount[req.GVR]++
}

func (c *WatchCoordinator) removeRouteLocked(key instanceKey, req WatchRequest) {
	if req.isCollection() {
		sel := req.Selector.String()
		entries := c.collectionRoutes[req.GVR]
		filtered := entries[:0]
		for _, e := range entries {
			if e.key == key && e.namespace == req.Namespace && e.selector.String() == sel {
				continue
			}
			filtered = append(filtered, e)
		}
		// Zero trailing elements so removed collectionRoute values (which hold
		// labels.Selector interface references) can be garbage-collected.
		for i := len(filtered); i < len(entries); i++ {
			entries[i] = collectionRoute{}
		}
		if len(filtered) == 0 {
			delete(c.collectionRoutes, req.GVR)
		} else {
			c.collectionRoutes[req.GVR] = filtered
		}
	} else {
		rk := scalarRouteKey{gvr: req.GVR, name: req.Name, namespace: req.Namespace}
		if keys := c.scalarRoutes[rk]; keys != nil {
			delete(keys, key)
			if len(keys) == 0 {
				delete(c.scalarRoutes, rk)
			}
		}
	}

	c.gvrRefCount[req.GVR]--
	if c.gvrRefCount[req.GVR] <= 0 {
		delete(c.gvrRefCount, req.GVR)
		gvrStr := req.GVR.String()
		watchRequestCount.DeleteLabelValues(gvrStr, "scalar")
		watchRequestCount.DeleteLabelValues(gvrStr, "collection")
	} else {
		typ := "scalar"
		if req.isCollection() {
			typ = "collection"
		}
		watchRequestCount.WithLabelValues(req.GVR.String(), typ).Dec()
	}
}

// findOrphanedGVRsLocked returns GVRs from the candidate list that have zero
// route entries. Must be called with c.mu held.
func (c *WatchCoordinator) findOrphanedGVRsLocked(gvrs []schema.GroupVersionResource) []schema.GroupVersionResource {
	var orphaned []schema.GroupVersionResource
	for _, gvr := range gvrs {
		if c.gvrRefCount[gvr] == 0 {
			orphaned = append(orphaned, gvr)
		}
	}
	return orphaned
}

// stopWatches releases the coordinator's ownership of the given GVRs and
// stops informers that have no remaining owners. Must be called without
// c.mu held to avoid holding the coordinator lock while the WatchManager
// acquires its own lock.
func (c *WatchCoordinator) stopWatches(gvrs []schema.GroupVersionResource) {
	for _, gvr := range gvrs {
		c.watches.ReleaseWatch(gvr, "coordinator")
		c.log.V(1).Info("Stopped orphaned child watch", "gvr", gvr)
	}
}

// decInstanceWatchCount decrements the instanceWatchCount gauge for the given
// parent GVR, deleting the label set entirely when no instances remain.
// Must be called with c.mu held, after the instance has been deleted.
func (c *WatchCoordinator) decInstanceWatchCount(parentGVR schema.GroupVersionResource) {
	for key := range c.committed {
		if key.parentGVR == parentGVR {
			instanceWatchCount.WithLabelValues(parentGVR.String()).Dec()
			return
		}
	}
	instanceWatchCount.DeleteLabelValues(parentGVR.String())
}

// dedupWatchRequests removes duplicate watch requests by content identity.
func dedupWatchRequests(reqs []WatchRequest) []WatchRequest {
	if len(reqs) <= 1 {
		return reqs
	}
	seen := make(map[watchIdentity]struct{}, len(reqs))
	result := make([]WatchRequest, 0, len(reqs))
	for _, r := range reqs {
		id := identityOf(r)
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		result = append(result, r)
	}
	return result
}

// NoopInstanceWatcher is a no-op implementation of InstanceWatcher for use
// in tests or when no coordinator is available.
type NoopInstanceWatcher struct{}

func (NoopInstanceWatcher) Watch(_ WatchRequest) error { return nil }
func (NoopInstanceWatcher) Done(bool)                  {}

// instanceWatcher is the concrete implementation of InstanceWatcher.
// Watch() calls append to a local pending slice with no coordinator
// interaction. Done(true) commits the pending set atomically.
//
// Not safe for concurrent use — callers must serialize Watch/Done calls.
// This is guaranteed by controller-runtime which processes one reconcile
// per instance at a time.
type instanceWatcher struct {
	coordinator *WatchCoordinator
	key         instanceKey
	pending     []WatchRequest
}

// Watch appends a watch request to the local pending set.
// No locks are acquired and the call never fails.
func (w *instanceWatcher) Watch(req WatchRequest) error {
	w.pending = append(w.pending, req)
	return nil
}

// Done finalizes the current reconciliation cycle. If commit is true, the
// pending watch set replaces the previously committed set and indexes are
// updated atomically. If commit is false, the pending set is discarded and
// the previously committed watches remain active.
func (w *instanceWatcher) Done(commit bool) {
	if !commit {
		return
	}
	w.coordinator.commitWatches(w.key, w.pending)
}
