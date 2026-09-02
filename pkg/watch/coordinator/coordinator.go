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

// Package coordinator holds the shared, key-generic watch-coordination core
// used by both the dynamic (instance) controller and the graph engine's watch
// router. It aggregates per-owner watch requests, retains one shared informer
// per GVR via pkg/watch.Manager, and routes informer events back to the
// matching owners.
//
// The package is a leaf: it depends only on pkg/watch and apimachinery. All
// metric/observability concerns are pushed to the caller through the Observer
// interface so this package never imports pkg/metrics or any controller stack.
package coordinator

import (
	"fmt"
	"sync"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	kwatch "github.com/kubernetes-sigs/kro/pkg/watch"
)

// ownerCoordinator is the owner ID the coordinator uses to retain informers
// in the shared Manager.
const ownerCoordinator = "coordinator"

// WatchRequest describes one resource an owner wants tracked. For scalar
// watches (a specific named resource) set Name + Namespace and leave Selector
// nil. For collection watches set Selector (and optionally Namespace to
// restrict the scope) and leave Name empty.
type WatchRequest struct {
	// NodeID is the graph node ID, used for dedup and observability. Two
	// requests with the same NodeID from the same owner are treated as
	// updates of one entry rather than two separate watches.
	NodeID string
	// GVR is the resource being watched.
	GVR schema.GroupVersionResource
	// Name is the resource name (scalar watches only).
	Name string
	// Namespace scopes the watch. Empty for cluster-scoped resources.
	Namespace string
	// Selector matches a collection of resources by label. Non-nil flips this
	// request into a collection watch.
	Selector labels.Selector
}

// isCollection reports whether this is a selector-based collection watch.
func (r *WatchRequest) isCollection() bool { return r.Selector != nil }

// Watcher is the per-owner handle a reconciler uses to declare which resources
// should re-enqueue the owner on change. Obtain one with Coordinator.For at
// the top of a reconcile, call Watch for every managed/observed resource, then
// commit with Done(true). On Done(true) any watches from the previous cycle
// that were not re-declared are torn down automatically; on Done(false) the
// in-flight set is discarded and the previously committed set stays active.
type Watcher interface {
	Watch(req WatchRequest) error
	Done(commit bool)
}

// NoopWatcher discards every Watch/Done call. Use in tests or when no
// coordinator is wired (e.g. CLI / dry-run paths).
type NoopWatcher struct{}

func (NoopWatcher) Watch(_ WatchRequest) error { return nil }
func (NoopWatcher) Done(_ bool)                {}

var (
	_ Watcher = (*NoopWatcher)(nil)
	_ Watcher = NoopWatcher{}
)

// EnqueueFunc is what the coordinator calls to request an owner reconcile.
type EnqueueFunc[K comparable] func(K)

// Observer receives structural lifecycle notifications so the facade packages
// can maintain metrics without the core depending on any metrics package. All
// callbacks except OnRoute are invoked while the coordinator holds its own
// lock; implementations must not call back into the coordinator. OnRoute is
// invoked without the coordinator lock and may run concurrently, so an
// implementation must treat it as a concurrent call.
type Observer[K comparable] interface {
	OnAddOwner(k K)
	OnRemoveOwner(k K)
	OnAddRequest(gvr schema.GroupVersionResource, collection bool)
	OnRemoveRequest(gvr schema.GroupVersionResource, collection bool, n int)
	OnRoute(gvr schema.GroupVersionResource, matched bool)
}

// NopObserver is an Observer that ignores every notification.
type NopObserver[K comparable] struct{}

func (NopObserver[K]) OnAddOwner(K)                                           {}
func (NopObserver[K]) OnRemoveOwner(K)                                        {}
func (NopObserver[K]) OnAddRequest(schema.GroupVersionResource, bool)         {}
func (NopObserver[K]) OnRemoveRequest(schema.GroupVersionResource, bool, int) {}
func (NopObserver[K]) OnRoute(schema.GroupVersionResource, bool)              {}

// nodeGVR keys a watch entry within an owner. A single graph node can render
// items of several distinct resource types (GVRs) — e.g. a dynamic collection
// that produces a mix of Deployments and Services. Keying watch state by NodeID
// alone would collapse those into one entry, so each new GVR registered under
// the node would overwrite the prior one and only the last resource type would
// keep a live watch. Keying by (NodeID, GVR) lets every resource type under a
// node retain its own watch. A scalar retarget that keeps the same GVR (only
// Name/Namespace changes) still collides on this key, preserving the intended
// replace-in-place behavior for a given (node, GVR).
type nodeGVR struct {
	nodeID string
	gvr    schema.GroupVersionResource
}

func keyOf(req *WatchRequest) nodeGVR {
	return nodeGVR{nodeID: req.NodeID, gvr: req.GVR}
}

// ownerState tracks the in-flight (current) and last-committed (previous)
// watch sets for one owner. The diff between previous and current on Done is
// what drives stale-watch cleanup.
type ownerState struct {
	current  map[nodeGVR]*WatchRequest // by (NodeID, GVR)
	previous map[nodeGVR]*WatchRequest
}

type scalarEntry[K comparable] struct {
	nodeID string
	key    K
}

type collectionEntry[K comparable] struct {
	nodeID    string
	selector  labels.Selector
	namespace string
	key       K
}

// Coordinator aggregates watch requests across all owners, asks the Manager to
// retain informers as needed, and routes informer events back to the matching
// owners via the EnqueueFunc. K is the owner key type (e.g. an instance key or
// a client.ObjectKey).
type Coordinator[K comparable] struct {
	mu sync.RWMutex

	watches *kwatch.Manager
	enqueue EnqueueFunc[K]
	obs     Observer[K]
	log     logr.Logger

	owners map[K]*ownerState

	// Reverse indexes for routing.
	scalarIndex     map[schema.GroupVersionResource]map[types.NamespacedName][]scalarEntry[K]
	collectionIndex map[schema.GroupVersionResource][]collectionEntry[K]
}

// New wires a coordinator to a Manager, an enqueue callback, and an observer.
// If obs is nil a no-op observer is installed.
func New[K comparable](watches *kwatch.Manager, enqueue EnqueueFunc[K], obs Observer[K], log logr.Logger) *Coordinator[K] {
	if obs == nil {
		obs = NopObserver[K]{}
	}
	return &Coordinator[K]{
		watches:         watches,
		enqueue:         enqueue,
		obs:             obs,
		log:             log.WithName("watch-coordinator"),
		owners:          make(map[K]*ownerState),
		scalarIndex:     make(map[schema.GroupVersionResource]map[types.NamespacedName][]scalarEntry[K]),
		collectionIndex: make(map[schema.GroupVersionResource][]collectionEntry[K]),
	}
}

// For returns a per-owner Watcher. Call this at the top of every reconcile and
// Done(commit) at the end.
func (c *Coordinator[K]) For(key K) Watcher {
	return &watcher[K]{c: c, key: key}
}

// RouteEvent dispatches an event from the Manager to every owner whose
// declared watch set covers it. Both current labels and old labels are
// considered for collection watches so that an object losing its label match
// still triggers reconciliation.
func (c *Coordinator[K]) RouteEvent(event kwatch.Event) {
	c.mu.RLock()
	matched := make(map[K]struct{})

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
		// On an update, always evaluate the old labels (nil treated as the empty
		// set). An object that transitions FROM matching to not-matching —
		// including a selector like `key DoesNotExist`, which the empty set
		// satisfies — must still re-enqueue the previously-matched owner so it
		// observes the object leaving its collection. A len>0 guard would skip
		// exactly the empty-old-set case that DoesNotExist selectors need.
		if event.Type == kwatch.EventUpdate && entry.selector.Matches(labels.Set(event.OldLabels)) {
			matched[entry.key] = struct{}{}
		}
	}
	c.mu.RUnlock()

	for key := range matched {
		c.enqueue(key)
	}
	// OnRoute is fired outside the lock and may run concurrently with other
	// RouteEvent calls; the observer must treat it as a concurrent call.
	c.obs.OnRoute(event.GVR, len(matched) > 0)
}

// OwnerCount returns the number of owners the coordinator currently tracks.
func (c *Coordinator[K]) OwnerCount() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.owners)
}

// WatchRequestCount returns the number of active scalar and collection watch
// requests across all owners.
func (c *Coordinator[K]) WatchRequestCount() (scalar, collection int) {
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
	return scalar, collection
}

// Remove drops every watch owned by key. Idempotent — calling for an unknown
// key is a no-op.
func (c *Coordinator[K]) Remove(key K) {
	c.mu.Lock()
	state, ok := c.owners[key]
	if !ok {
		c.mu.Unlock()
		return
	}

	affectedGVRs := c.unindexOwnerStateLocked(key, state, nil)
	delete(c.owners, key)
	c.obs.OnRemoveOwner(key)

	orphaned := c.findOrphanedGVRsLocked(affectedGVRs)
	c.mu.Unlock()

	c.stopWatches(orphaned)
}

// RemoveWhere drops every watch owned by a key for which pred returns true.
// OnRemoveOwner fires once per owner actually removed so per-owner accounting
// stays correct.
func (c *Coordinator[K]) RemoveWhere(pred func(K) bool) {
	c.mu.Lock()

	var toRemove []K
	for key := range c.owners {
		if pred(key) {
			toRemove = append(toRemove, key)
		}
	}

	capacity := 0
	for _, key := range toRemove {
		state := c.owners[key]
		capacity += len(state.current) + len(state.previous)
	}
	affectedGVRs := make([]schema.GroupVersionResource, 0, capacity)
	for _, key := range toRemove {
		affectedGVRs = c.unindexOwnerStateLocked(key, c.owners[key], affectedGVRs)
		delete(c.owners, key)
		c.obs.OnRemoveOwner(key)
	}

	orphaned := c.findOrphanedGVRsLocked(affectedGVRs)
	c.mu.Unlock()

	c.stopWatches(orphaned)
}

// addWatch enrolls a request under key. Called from watcher.Watch. EnsureWatch
// is invoked outside the coordinator lock to avoid holding two locks
// simultaneously. On EnsureWatch failure the added entry is rolled back and
// the wrapped error is returned.
func (c *Coordinator[K]) addWatch(key K, req WatchRequest) error {
	c.mu.Lock()

	state, ok := c.owners[key]
	if !ok {
		state = &ownerState{
			current:  make(map[nodeGVR]*WatchRequest),
			previous: make(map[nodeGVR]*WatchRequest),
		}
		c.owners[key] = state
		c.obs.OnAddOwner(key)
	}

	// If this (nodeID, GVR) is being re-declared with a different target than
	// the in-flight current entry (e.g. a scalar watch retargeted to a new
	// Name), evict the stale current entry from the indexes before installing
	// the new one — unless the stale entry is shared with the previous
	// committed set (in which case the index still needs it). Keying by
	// (nodeID, GVR) means a second GVR under the same node is a distinct entry
	// and does NOT evict the first.
	k := keyOf(&req)
	var orphaned []schema.GroupVersionResource
	if old, exists := state.current[k]; exists {
		if !SameWatchTarget(old, &req) {
			if prev, shared := state.previous[k]; !shared || !SameWatchTarget(prev, old) {
				c.removeRequestFromIndexesLocked(key, old)
				orphaned = c.findOrphanedGVRsLocked([]schema.GroupVersionResource{old.GVR})
			}
		}
	}

	state.current[k] = &req

	if prev, shared := state.previous[k]; !shared || !SameWatchTarget(prev, &req) {
		if req.isCollection() {
			c.addCollectionIndexLocked(key, req)
		} else {
			c.addScalarIndexLocked(key, req)
		}
	}

	gvr := req.GVR
	c.mu.Unlock()

	c.stopWatches(orphaned)

	if err := c.watches.EnsureWatch(gvr, ownerCoordinator); err != nil {
		c.mu.Lock()
		if state, ok := c.owners[key]; ok {
			if cur, exists := state.current[k]; exists && SameWatchTarget(cur, &req) {
				delete(state.current, k)
				if prev, shared := state.previous[k]; !shared || !SameWatchTarget(prev, cur) {
					c.removeRequestFromIndexesLocked(key, cur)
				}
			}
		}
		c.mu.Unlock()
		return fmt.Errorf("ensure watch for %s: %w", gvr, err)
	}
	return nil
}

// done commits the in-flight cycle for key. Any watches from the previous
// cycle that were not re-declared (or whose target changed) are removed.
func (c *Coordinator[K]) done(key K) {
	c.mu.Lock()
	state, ok := c.owners[key]
	if !ok {
		c.mu.Unlock()
		return
	}

	var affectedGVRs []schema.GroupVersionResource
	for k, oldReq := range state.previous {
		if newReq, stillActive := state.current[k]; stillActive && SameWatchTarget(newReq, oldReq) {
			continue
		}
		c.removeRequestFromIndexesLocked(key, oldReq)
		affectedGVRs = append(affectedGVRs, oldReq.GVR)
	}

	state.previous = state.current
	state.current = make(map[nodeGVR]*WatchRequest)

	orphaned := c.findOrphanedGVRsLocked(affectedGVRs)
	c.mu.Unlock()

	c.stopWatches(orphaned)
}

// abort discards the in-flight cycle without touching the previous committed
// set. Used when reconcile fails partway through declaring watches and the
// prior set should remain authoritative. If no committed set exists, the owner
// is dropped entirely.
func (c *Coordinator[K]) abort(key K) {
	c.mu.Lock()
	state, ok := c.owners[key]
	if !ok {
		c.mu.Unlock()
		return
	}

	var affectedGVRs []schema.GroupVersionResource
	for k, req := range state.current {
		if prev, shared := state.previous[k]; shared && SameWatchTarget(prev, req) {
			continue
		}
		c.removeRequestFromIndexesLocked(key, req)
		affectedGVRs = append(affectedGVRs, req.GVR)
	}
	state.current = make(map[nodeGVR]*WatchRequest)

	if len(state.previous) == 0 {
		delete(c.owners, key)
		c.obs.OnRemoveOwner(key)
	}

	orphaned := c.findOrphanedGVRsLocked(affectedGVRs)
	c.mu.Unlock()

	c.stopWatches(orphaned)
}

// --- internal helpers ---------------------------------------------------

func (c *Coordinator[K]) addScalarIndexLocked(key K, req WatchRequest) {
	byName, ok := c.scalarIndex[req.GVR]
	if !ok {
		byName = make(map[types.NamespacedName][]scalarEntry[K])
		c.scalarIndex[req.GVR] = byName
	}
	nn := types.NamespacedName{Name: req.Name, Namespace: req.Namespace}
	for _, entry := range byName[nn] {
		if entry.key == key && entry.nodeID == req.NodeID {
			return
		}
	}
	byName[nn] = append(byName[nn], scalarEntry[K]{nodeID: req.NodeID, key: key})
	c.obs.OnAddRequest(req.GVR, false)
}

func (c *Coordinator[K]) addCollectionIndexLocked(key K, req WatchRequest) {
	entries := c.collectionIndex[req.GVR]
	for _, e := range entries {
		if e.key == key && e.nodeID == req.NodeID && e.namespace == req.Namespace &&
			e.selector.String() == req.Selector.String() {
			return
		}
	}
	c.collectionIndex[req.GVR] = append(entries, collectionEntry[K]{
		nodeID:    req.NodeID,
		selector:  req.Selector,
		namespace: req.Namespace,
		key:       key,
	})
	c.obs.OnAddRequest(req.GVR, true)
}

// unindexOwnerStateLocked removes every current and previous request owned by
// key from the indexes, appending each request's GVR to affectedGVRs (which may
// be nil) and returning the extended slice. Caller must hold c.mu.
func (c *Coordinator[K]) unindexOwnerStateLocked(key K, state *ownerState, affectedGVRs []schema.GroupVersionResource) []schema.GroupVersionResource {
	for _, req := range state.current {
		c.removeRequestFromIndexesLocked(key, req)
		affectedGVRs = append(affectedGVRs, req.GVR)
	}
	for _, req := range state.previous {
		c.removeRequestFromIndexesLocked(key, req)
		affectedGVRs = append(affectedGVRs, req.GVR)
	}
	return affectedGVRs
}

func (c *Coordinator[K]) removeRequestFromIndexesLocked(key K, req *WatchRequest) {
	if req.isCollection() {
		c.removeCollectionIndexLocked(key, req)
	} else {
		c.removeScalarIndexLocked(key, req)
	}
}

func (c *Coordinator[K]) removeScalarIndexLocked(key K, req *WatchRequest) {
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
	removed := len(entries) - len(filtered)
	if len(filtered) == 0 {
		delete(byName, nn)
	} else {
		clear(entries[len(filtered):])
		byName[nn] = filtered
	}
	if len(byName) == 0 {
		delete(c.scalarIndex, req.GVR)
	}
	if removed > 0 {
		c.obs.OnRemoveRequest(req.GVR, false, removed)
	}
}

func (c *Coordinator[K]) removeCollectionIndexLocked(key K, req *WatchRequest) {
	entries := c.collectionIndex[req.GVR]
	filtered := entries[:0]
	for _, e := range entries {
		if e.key == key && e.nodeID == req.NodeID && e.namespace == req.Namespace &&
			e.selector.String() == req.Selector.String() {
			continue
		}
		filtered = append(filtered, e)
	}
	removed := len(entries) - len(filtered)
	if len(filtered) == 0 {
		delete(c.collectionIndex, req.GVR)
	} else {
		clear(entries[len(filtered):])
		c.collectionIndex[req.GVR] = filtered
	}
	if removed > 0 {
		c.obs.OnRemoveRequest(req.GVR, true, removed)
	}
}

// findOrphanedGVRsLocked returns GVRs with no remaining entries in either
// index. Must hold c.mu.
func (c *Coordinator[K]) findOrphanedGVRsLocked(gvrs []schema.GroupVersionResource) []schema.GroupVersionResource {
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

// stopWatches releases the coordinator's retention on the given GVRs. Must NOT
// hold c.mu — ReleaseWatch acquires the Manager's lock and nesting them would
// invite deadlocks if the Manager ever calls back into the coordinator.
func (c *Coordinator[K]) stopWatches(gvrs []schema.GroupVersionResource) {
	for _, gvr := range gvrs {
		c.watches.ReleaseWatch(gvr, ownerCoordinator)
		c.log.V(1).Info("Stopped orphaned child watch", "gvr", gvr)
	}
}

// SameWatchTarget compares two requests for behavioral equivalence (ignoring
// NodeID, which is the key, not the target).
func SameWatchTarget(a, b *WatchRequest) bool {
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

// watcher is the concrete per-owner Watcher handle.
type watcher[K comparable] struct {
	c   *Coordinator[K]
	key K
}

func (w *watcher[K]) Watch(req WatchRequest) error { return w.c.addWatch(w.key, req) }

func (w *watcher[K]) Done(commit bool) {
	if commit {
		w.c.done(w.key)
		return
	}
	w.c.abort(w.key)
}
