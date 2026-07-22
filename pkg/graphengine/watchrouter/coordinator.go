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

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Watcher is the per-Graph handle a reconciler uses to declare which
// resources should re-enqueue the Graph on change. Obtain one with
// Router.ForGraph at the top of a reconcile, call Watch for
// every managed/observed resource, then commit with Done(true). On
// Done(true) any watches from the previous cycle that were not re-declared
// are torn down automatically; on Done(false) the in-flight set is
// discarded and the previously committed set stays active (useful when
// reconcile failed before all watches were registered and you want the
// last-good set to remain).
type Watcher interface {
	Watch(req WatchRequest) error
	Done(commit bool)
}

// WatchRequest describes one resource the Graph wants tracked. For scalar
// watches (a specific named resource) set Name + Namespace and leave
// Selector nil. For collection watches set Selector (and optionally
// Namespace to restrict the scope) and leave Name empty.
type WatchRequest struct {
	// NodeID is the graph node ID, used for dedup and observability. Two
	// requests with the same NodeID from the same Graph are treated as
	// updates of one entry rather than two separate watches.
	NodeID string
	// GVR is the resource being watched.
	GVR schema.GroupVersionResource
	// Name is the resource name (scalar watches only).
	Name string
	// Namespace scopes the watch. Empty for cluster-scoped resources.
	Namespace string
	// Selector matches a collection of resources by label. Non-nil
	// flips this request into a collection watch.
	Selector labels.Selector
}

func (r *WatchRequest) isCollection() bool { return r.Selector != nil }

// EnqueueFunc is what the coordinator calls to request a Graph reconcile.
// Implementations push the key onto the controller's work queue or an
// upstream source.Channel.
type EnqueueFunc func(key client.ObjectKey)

// graphState tracks the in-flight (current) and last-committed (previous)
// watch sets for one Graph. The diff between previous and current on Done
// is what drives stale-watch cleanup.
type graphState struct {
	current  map[string]*WatchRequest // by NodeID
	previous map[string]*WatchRequest
}

type scalarEntry struct {
	nodeID string
	key    client.ObjectKey
}

type collectionEntry struct {
	nodeID    string
	selector  labels.Selector
	namespace string
	key       client.ObjectKey
}

// Coordinator aggregates watch requests across all Graphs, asks the
// Manager to retain informers as needed, and routes informer events
// back to the matching Graphs via the EnqueueFunc.
type Coordinator struct {
	mu sync.RWMutex

	watches *Manager
	enqueue EnqueueFunc
	log     logr.Logger

	graphs map[client.ObjectKey]*graphState

	// Reverse indexes for routing.
	scalarIndex     map[schema.GroupVersionResource]map[types.NamespacedName][]scalarEntry
	collectionIndex map[schema.GroupVersionResource][]collectionEntry
}

// NewCoordinator wires a coordinator to a Manager and an
// enqueue callback.
func NewCoordinator(watches *Manager, enqueue EnqueueFunc, log logr.Logger) *Coordinator {
	return &Coordinator{
		watches:         watches,
		enqueue:         enqueue,
		log:             log.WithName("watch-coordinator"),
		graphs:          make(map[client.ObjectKey]*graphState),
		scalarIndex:     make(map[schema.GroupVersionResource]map[types.NamespacedName][]scalarEntry),
		collectionIndex: make(map[schema.GroupVersionResource][]collectionEntry),
	}
}

// ForGraph returns a per-Graph Watcher. Call this at the top of every
// reconcile and Done(commit) at the end.
func (c *Coordinator) ForGraph(key client.ObjectKey) Watcher {
	return &graphWatcher{coordinator: c, key: key}
}

// RouteEvent dispatches an event from the Manager to every Graph
// whose declared watch set covers it. Both current labels and old labels
// are considered for collection watches so that an object losing its
// label match still triggers reconciliation.
func (c *Coordinator) RouteEvent(event Event) {
	c.mu.RLock()
	matched := make(map[client.ObjectKey]struct{})

	if byName, ok := c.scalarIndex[event.GVR]; ok {
		nn := types.NamespacedName{Name: event.Name, Namespace: event.Namespace}
		for _, entry := range byName[nn] {
			matched[entry.key] = struct{}{}
		}
	}

	for _, entry := range c.collectionIndex[event.GVR] {
		if entry.namespace != "" && event.Namespace != entry.namespace {
			continue
		}
		if entry.selector.Matches(labels.Set(event.Labels)) {
			matched[entry.key] = struct{}{}
			continue
		}
		if len(event.OldLabels) > 0 && entry.selector.Matches(labels.Set(event.OldLabels)) {
			matched[entry.key] = struct{}{}
		}
	}
	c.mu.RUnlock()

	for key := range matched {
		c.enqueue(key)
	}
}

// GraphCount returns the number of Graphs the coordinator currently
// tracks (tests + metrics).
func (c *Coordinator) GraphCount() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.graphs)
}

// WatchRequestCount returns the number of active scalar and collection
// watch requests across all Graphs.
func (c *Coordinator) WatchRequestCount() (scalar, collection int) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, byName := range c.scalarIndex {
		for _, entries := range byName {
			scalar += len(entries)
		}
	}
	for _, entries := range c.collectionIndex {
		collection += len(entries)
	}
	return
}

// RemoveGraph drops every watch owned by the Graph. Called on Graph
// deletion. Idempotent — calling for an unknown key is a no-op.
func (c *Coordinator) RemoveGraph(key client.ObjectKey) {
	c.mu.Lock()
	state, ok := c.graphs[key]
	if !ok {
		c.mu.Unlock()
		return
	}

	var affectedGVRs []schema.GroupVersionResource
	for _, req := range state.current {
		c.removeRequestFromIndexesLocked(key, req)
		affectedGVRs = append(affectedGVRs, req.GVR)
	}
	for _, req := range state.previous {
		c.removeRequestFromIndexesLocked(key, req)
		affectedGVRs = append(affectedGVRs, req.GVR)
	}
	delete(c.graphs, key)

	orphaned := c.findOrphanedGVRsLocked(affectedGVRs)
	c.mu.Unlock()

	c.stopWatches(orphaned)
}

// addWatch enrolls a request under key. Called from graphWatcher.Watch.
// EnsureWatch is invoked outside the coordinator lock to avoid holding
// two locks simultaneously.
func (c *Coordinator) addWatch(key client.ObjectKey, req WatchRequest) error {
	c.mu.Lock()

	state, ok := c.graphs[key]
	if !ok {
		state = &graphState{
			current:  make(map[string]*WatchRequest),
			previous: make(map[string]*WatchRequest),
		}
		c.graphs[key] = state
	}

	// If this nodeID is being re-declared with a different target than
	// the in-flight current entry, evict the stale current entry from
	// the indexes before installing the new one — unless the stale
	// entry is shared with the previous committed set (in which case
	// the index still needs it).
	if old, exists := state.current[req.NodeID]; exists {
		if !sameWatchTarget(old, &req) {
			if prev, shared := state.previous[req.NodeID]; !shared || !sameWatchTarget(prev, old) {
				c.removeRequestFromIndexesLocked(key, old)
			}
		}
	}

	state.current[req.NodeID] = &req

	if prev, shared := state.previous[req.NodeID]; !shared || !sameWatchTarget(prev, &req) {
		if req.isCollection() {
			c.addCollectionIndexLocked(key, req)
		} else {
			c.addScalarIndexLocked(key, req)
		}
	}

	gvr := req.GVR
	c.mu.Unlock()

	if err := c.watches.EnsureWatch(gvr, ownerCoordinator); err != nil {
		c.log.Error(err, "EnsureWatch failed", "gvr", gvr, "graph", key)
		return err
	}
	return nil
}

// doneGraph commits the in-flight cycle for key. Any watches from the
// previous cycle that were not re-declared (or whose target changed)
// are removed.
func (c *Coordinator) doneGraph(key client.ObjectKey) {
	c.mu.Lock()
	state, ok := c.graphs[key]
	if !ok {
		c.mu.Unlock()
		return
	}

	var affectedGVRs []schema.GroupVersionResource
	for nodeID, oldReq := range state.previous {
		if newReq, stillActive := state.current[nodeID]; stillActive && sameWatchTarget(newReq, oldReq) {
			continue
		}
		c.removeRequestFromIndexesLocked(key, oldReq)
		affectedGVRs = append(affectedGVRs, oldReq.GVR)
	}

	state.previous = state.current
	state.current = make(map[string]*WatchRequest)

	orphaned := c.findOrphanedGVRsLocked(affectedGVRs)
	c.mu.Unlock()

	c.stopWatches(orphaned)
}

// abortGraph discards the in-flight cycle without touching the previous
// committed set. Used when reconcile fails partway through declaring
// watches and the prior set should remain authoritative.
func (c *Coordinator) abortGraph(key client.ObjectKey) {
	c.mu.Lock()
	state, ok := c.graphs[key]
	if !ok {
		c.mu.Unlock()
		return
	}

	var affectedGVRs []schema.GroupVersionResource
	for nodeID, req := range state.current {
		if prev, shared := state.previous[nodeID]; shared && sameWatchTarget(prev, req) {
			continue
		}
		c.removeRequestFromIndexesLocked(key, req)
		affectedGVRs = append(affectedGVRs, req.GVR)
	}
	state.current = make(map[string]*WatchRequest)

	orphaned := c.findOrphanedGVRsLocked(affectedGVRs)
	c.mu.Unlock()

	c.stopWatches(orphaned)
}

// --- internal helpers ---------------------------------------------------

const ownerCoordinator = "coordinator"

func (c *Coordinator) addScalarIndexLocked(key client.ObjectKey, req WatchRequest) {
	byName, ok := c.scalarIndex[req.GVR]
	if !ok {
		byName = make(map[types.NamespacedName][]scalarEntry)
		c.scalarIndex[req.GVR] = byName
	}
	nn := types.NamespacedName{Name: req.Name, Namespace: req.Namespace}
	for _, entry := range byName[nn] {
		if entry.key == key && entry.nodeID == req.NodeID {
			return
		}
	}
	byName[nn] = append(byName[nn], scalarEntry{nodeID: req.NodeID, key: key})
}

func (c *Coordinator) addCollectionIndexLocked(key client.ObjectKey, req WatchRequest) {
	entries := c.collectionIndex[req.GVR]
	for _, e := range entries {
		if e.key == key && e.nodeID == req.NodeID && e.namespace == req.Namespace &&
			e.selector.String() == req.Selector.String() {
			return
		}
	}
	c.collectionIndex[req.GVR] = append(entries, collectionEntry{
		nodeID:    req.NodeID,
		selector:  req.Selector,
		namespace: req.Namespace,
		key:       key,
	})
}

func (c *Coordinator) removeRequestFromIndexesLocked(key client.ObjectKey, req *WatchRequest) {
	if req.isCollection() {
		c.removeCollectionIndexLocked(key, req)
	} else {
		c.removeScalarIndexLocked(key, req)
	}
}

func (c *Coordinator) removeScalarIndexLocked(key client.ObjectKey, req *WatchRequest) {
	byName, ok := c.scalarIndex[req.GVR]
	if !ok {
		return
	}
	nn := types.NamespacedName{Name: req.Name, Namespace: req.Namespace}
	entries, ok := byName[nn]
	if !ok {
		return
	}
	filtered := entries[:0]
	for _, entry := range entries {
		if entry.key == key && entry.nodeID == req.NodeID {
			continue
		}
		filtered = append(filtered, entry)
	}
	if len(filtered) == 0 {
		delete(byName, nn)
	} else {
		byName[nn] = filtered
	}
	if len(byName) == 0 {
		delete(c.scalarIndex, req.GVR)
	}
}

func (c *Coordinator) removeCollectionIndexLocked(key client.ObjectKey, req *WatchRequest) {
	entries := c.collectionIndex[req.GVR]
	filtered := entries[:0]
	for _, e := range entries {
		if e.key == key && e.nodeID == req.NodeID && e.namespace == req.Namespace &&
			e.selector.String() == req.Selector.String() {
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

// findOrphanedGVRsLocked returns GVRs with no remaining entries in either
// index. Must hold c.mu.
func (c *Coordinator) findOrphanedGVRsLocked(gvrs []schema.GroupVersionResource) []schema.GroupVersionResource {
	if len(gvrs) == 0 {
		return nil
	}
	seen := make(map[schema.GroupVersionResource]struct{}, len(gvrs))
	var orphaned []schema.GroupVersionResource
	for _, gvr := range gvrs {
		if _, dupe := seen[gvr]; dupe {
			continue
		}
		seen[gvr] = struct{}{}
		if len(c.scalarIndex[gvr]) == 0 && len(c.collectionIndex[gvr]) == 0 {
			orphaned = append(orphaned, gvr)
		}
	}
	return orphaned
}

// stopWatches releases the coordinator's retention on the given GVRs.
// Must NOT hold c.mu — ReleaseWatch acquires Manager's lock and
// nesting them would invite deadlocks if Manager ever calls back
// into the coordinator.
func (c *Coordinator) stopWatches(gvrs []schema.GroupVersionResource) {
	for _, gvr := range gvrs {
		c.watches.ReleaseWatch(gvr, ownerCoordinator)
	}
}

// sameWatchTarget compares two requests for behavioral equivalence
// (ignoring NodeID, which is the *key*, not the target).
func sameWatchTarget(a, b *WatchRequest) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.GVR != b.GVR || a.Name != b.Name || a.Namespace != b.Namespace {
		return false
	}
	if a.isCollection() != b.isCollection() {
		return false
	}
	if !a.isCollection() {
		return true
	}
	return a.Selector.String() == b.Selector.String()
}

// NoopWatcher discards every Watch/Done call. Use in tests or when no
// coordinator is wired (e.g. CLI / dry-run paths).
type NoopWatcher struct{}

func (NoopWatcher) Watch(_ WatchRequest) error { return nil }
func (NoopWatcher) Done(_ bool)                {}

type graphWatcher struct {
	coordinator *Coordinator
	key         client.ObjectKey
}

func (w *graphWatcher) Watch(req WatchRequest) error {
	return w.coordinator.addWatch(w.key, req)
}

func (w *graphWatcher) Done(commit bool) {
	if commit {
		w.coordinator.doneGraph(w.key)
		return
	}
	w.coordinator.abortGraph(w.key)
}
