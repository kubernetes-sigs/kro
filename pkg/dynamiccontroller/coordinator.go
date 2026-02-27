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
	//
	// Call Watch() BEFORE operating on the resource to avoid event gaps.
	Watch(req WatchRequest) error

	// Done signals that all Watch() calls for this reconciliation cycle
	// are complete. Any watch requests from the previous cycle that were
	// NOT re-requested are automatically cleaned up.
	//
	// If reconciliation fails before Done(), previous requests stay active.
	Done()
}

// WatchRequest describes a resource the instance reconciler wants to watch.
// For scalar resources, set Name + Namespace.
// For collections, set Selector + Namespace.
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

// instanceState tracks watch requests for a single instance across
// reconciliation cycles. The coordinator uses current vs previous to
// detect and clean up stale requests.
type instanceState struct {
	current  map[string]*WatchRequest // keyed by nodeID
	previous map[string]*WatchRequest // from last Done() cycle
}

// collectionEntry is a single collection watch in the reverse index.
type collectionEntry struct {
	selector  labels.Selector
	namespace string
	key       instanceKey
}

// WatchCoordinator aggregates watch requests from all instances, manages
// shared watches via WatchManager, and routes events back to the correct
// instances.
type WatchCoordinator struct {
	mu sync.RWMutex

	watches *WatchManager
	enqueue EnqueueFunc
	log     logr.Logger

	// Per-instance state: tracks what each instance is watching.
	instances map[instanceKey]*instanceState

	// Reverse indexes for event routing.
	// GVR -> namespace/name -> set of instanceKeys
	scalarIndex map[schema.GroupVersionResource]map[types.NamespacedName]map[instanceKey]struct{}

	// GVR -> list of collection watchers.
	collectionIndex map[schema.GroupVersionResource][]collectionEntry
}

// NewWatchCoordinator creates a new WatchCoordinator.
func NewWatchCoordinator(watches *WatchManager, enqueue EnqueueFunc, log logr.Logger) *WatchCoordinator {
	return &WatchCoordinator{
		watches:         watches,
		enqueue:         enqueue,
		log:             log.WithName("watch-coordinator"),
		instances:       make(map[instanceKey]*instanceState),
		scalarIndex:     make(map[schema.GroupVersionResource]map[types.NamespacedName]map[instanceKey]struct{}),
		collectionIndex: make(map[schema.GroupVersionResource][]collectionEntry),
	}
}

// ForInstance returns a scoped InstanceWatcher handle for the given instance.
func (c *WatchCoordinator) ForInstance(parentGVR schema.GroupVersionResource, instance types.NamespacedName) InstanceWatcher {
	return &instanceWatcher{
		coordinator: c,
		parentGVR:   parentGVR,
		instance:    instance,
	}
}

// addWatch registers a watch request for the given instance.
func (c *WatchCoordinator) addWatch(key instanceKey, req WatchRequest) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Ensure instance state exists.
	state, ok := c.instances[key]
	if !ok {
		state = &instanceState{
			current:  make(map[string]*WatchRequest),
			previous: make(map[string]*WatchRequest),
		}
		c.instances[key] = state
	}

	// Fix reverse index orphaning on nodeID reuse: if an existing entry
	// for this nodeID has a different target, remove it from indexes first.
	if old, exists := state.current[req.NodeID]; exists {
		if old.GVR != req.GVR || old.Name != req.Name || old.Namespace != req.Namespace {
			c.removeRequestFromIndexesLocked(key, old)
		}
	}

	// Add to current cycle.
	state.current[req.NodeID] = &req

	// Ensure an informer is running for this GVR (non-blocking).
	c.watches.EnsureWatch(req.GVR)

	// Add to reverse index.
	if req.isCollection() {
		c.addCollectionIndexLocked(key, req)
	} else {
		c.addScalarIndexLocked(key, req)
	}

	return nil
}

// doneInstance finalizes the reconciliation cycle for an instance.
func (c *WatchCoordinator) doneInstance(key instanceKey) {
	c.mu.Lock()
	defer c.mu.Unlock()

	state, ok := c.instances[key]
	if !ok {
		return
	}

	// Remove requests that were in previous but NOT in current, or whose
	// target changed (same nodeID but different GVR/name/namespace).
	// Collect affected GVRs for orphan cleanup.
	var affectedGVRs []schema.GroupVersionResource
	for nodeID, oldReq := range state.previous {
		if newReq, stillActive := state.current[nodeID]; stillActive {
			if newReq.GVR == oldReq.GVR && newReq.Name == oldReq.Name && newReq.Namespace == oldReq.Namespace {
				continue
			}
			// Target changed — remove old index entry.
		}
		c.removeRequestFromIndexesLocked(key, oldReq)
		affectedGVRs = append(affectedGVRs, oldReq.GVR)
	}

	// Swap: previous = current, current = new empty map.
	state.previous = state.current
	state.current = make(map[string]*WatchRequest)

	c.stopOrphanedWatchesLocked(affectedGVRs)
}

// RemoveInstance removes all watch requests for a specific instance.
// Called when an instance is deleted.
func (c *WatchCoordinator) RemoveInstance(parentGVR schema.GroupVersionResource, instance types.NamespacedName) {
	key := instanceKey{parentGVR: parentGVR, instance: instance}

	c.mu.Lock()
	defer c.mu.Unlock()

	state, ok := c.instances[key]
	if !ok {
		return
	}

	// Remove all current and previous requests from indexes.
	// Collect affected GVRs for orphan cleanup.
	var affectedGVRs []schema.GroupVersionResource
	for _, req := range state.current {
		c.removeRequestFromIndexesLocked(key, req)
		affectedGVRs = append(affectedGVRs, req.GVR)
	}
	for _, req := range state.previous {
		c.removeRequestFromIndexesLocked(key, req)
		affectedGVRs = append(affectedGVRs, req.GVR)
	}

	delete(c.instances, key)

	c.stopOrphanedWatchesLocked(affectedGVRs)
}

// RemoveParentGVR removes all instances for a given parent GVR.
// Called when an RGD is deregistered.
func (c *WatchCoordinator) RemoveParentGVR(parentGVR schema.GroupVersionResource) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Collect instance keys to remove.
	var toRemove []instanceKey
	for key := range c.instances {
		if key.parentGVR == parentGVR {
			toRemove = append(toRemove, key)
		}
	}

	capacity := 0
	for _, key := range toRemove {
		state := c.instances[key]
		capacity += len(state.current) + len(state.previous)
	}
	affectedGVRs := make([]schema.GroupVersionResource, 0, capacity)
	for _, key := range toRemove {
		state := c.instances[key]
		for _, req := range state.current {
			c.removeRequestFromIndexesLocked(key, req)
			affectedGVRs = append(affectedGVRs, req.GVR)
		}
		for _, req := range state.previous {
			c.removeRequestFromIndexesLocked(key, req)
			affectedGVRs = append(affectedGVRs, req.GVR)
		}
		delete(c.instances, key)
	}

	c.stopOrphanedWatchesLocked(affectedGVRs)
}

// RouteEvent routes a watch event to all matching instances.
// Called by the watch handler for every event.
func (c *WatchCoordinator) RouteEvent(event Event) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	matched := false

	// Scalar matches (O(1) per GVR+name).
	if byName, ok := c.scalarIndex[event.GVR]; ok {
		key := types.NamespacedName{Name: event.Name, Namespace: event.Namespace}
		for instKey := range byName[key] {
			c.enqueue(instKey.parentGVR, instKey.instance)
			matched = true
		}
	}

	// Collection matches (selector scan).
	for _, entry := range c.collectionIndex[event.GVR] {
		if entry.namespace != "" && event.Namespace != entry.namespace {
			continue
		}
		if entry.selector.Matches(labels.Set(event.Labels)) {
			c.enqueue(entry.key.parentGVR, entry.key.instance)
			matched = true
		}
	}

	if matched {
		c.log.V(2).Info("Routed event", "gvr", event.GVR, "name", event.Name, "namespace", event.Namespace, "type", event.Type)
	}
}

// InstanceWatchCount returns the number of tracked instances.
func (c *WatchCoordinator) InstanceWatchCount() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.instances)
}

// WatchRequestCount returns the total number of active watch requests.
func (c *WatchCoordinator) WatchRequestCount() (scalar, collection int) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	for _, byName := range c.scalarIndex {
		for _, instSet := range byName {
			scalar += len(instSet)
		}
	}
	for _, entries := range c.collectionIndex {
		collection += len(entries)
	}
	return
}

// --- internal helpers ---

func (c *WatchCoordinator) addScalarIndexLocked(key instanceKey, req WatchRequest) {
	byName, ok := c.scalarIndex[req.GVR]
	if !ok {
		byName = make(map[types.NamespacedName]map[instanceKey]struct{})
		c.scalarIndex[req.GVR] = byName
	}
	nn := types.NamespacedName{Name: req.Name, Namespace: req.Namespace}
	if _, ok := byName[nn]; !ok {
		byName[nn] = make(map[instanceKey]struct{})
	}
	byName[nn][key] = struct{}{}
}

func (c *WatchCoordinator) addCollectionIndexLocked(key instanceKey, req WatchRequest) {
	c.collectionIndex[req.GVR] = append(c.collectionIndex[req.GVR], collectionEntry{
		selector:  req.Selector,
		namespace: req.Namespace,
		key:       key,
	})
}

func (c *WatchCoordinator) removeRequestFromIndexesLocked(key instanceKey, req *WatchRequest) {
	if req.isCollection() {
		c.removeCollectionIndexLocked(key, req)
	} else {
		c.removeScalarIndexLocked(key, req)
	}
}

func (c *WatchCoordinator) removeScalarIndexLocked(key instanceKey, req *WatchRequest) {
	byName, ok := c.scalarIndex[req.GVR]
	if !ok {
		return
	}
	nn := types.NamespacedName{Name: req.Name, Namespace: req.Namespace}
	instSet, ok := byName[nn]
	if !ok {
		return
	}
	delete(instSet, key)
	if len(instSet) == 0 {
		delete(byName, nn)
	}
	if len(byName) == 0 {
		delete(c.scalarIndex, req.GVR)
	}
}

// stopOrphanedWatchesLocked stops informers for GVRs that have zero entries
// in both the scalar and collection indexes. Must be called with c.mu held.
func (c *WatchCoordinator) stopOrphanedWatchesLocked(gvrs []schema.GroupVersionResource) {
	for _, gvr := range gvrs {
		if len(c.scalarIndex[gvr]) == 0 && len(c.collectionIndex[gvr]) == 0 {
			c.watches.StopWatch(gvr)
			c.log.V(1).Info("Stopped orphaned child watch", "gvr", gvr)
		}
	}
}

func (c *WatchCoordinator) removeCollectionIndexLocked(key instanceKey, req *WatchRequest) {
	entries := c.collectionIndex[req.GVR]
	filtered := entries[:0]
	for _, e := range entries {
		if e.key == key && e.selector.String() == req.Selector.String() && e.namespace == req.Namespace {
			continue
		}
		filtered = append(filtered, e)
	}
	if len(filtered) == 0 {
		delete(c.collectionIndex, req.GVR)
	} else {
		c.collectionIndex[req.GVR] = filtered
	}
}

// instanceWatcher is the concrete implementation of InstanceWatcher.
type instanceWatcher struct {
	coordinator *WatchCoordinator
	parentGVR   schema.GroupVersionResource
	instance    types.NamespacedName
}

// Watch registers a watch request for this instance.
func (w *instanceWatcher) Watch(req WatchRequest) error {
	return w.coordinator.addWatch(instanceKey{
		parentGVR: w.parentGVR,
		instance:  w.instance,
	}, req)
}

// Done finalizes the current reconciliation cycle, cleaning up stale requests.
func (w *instanceWatcher) Done() {
	w.coordinator.doneInstance(instanceKey{
		parentGVR: w.parentGVR,
		instance:  w.instance,
	})
}
