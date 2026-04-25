// watcher.go implements GraphWatcher — a scoped handle for a single Graph's
// reconcile cycle. Watch requests are buffered locally during the reconcile
// and flushed to the coordinator's indexes in a single lock acquisition at
// Done(true). This keeps lock acquisitions at 1 per reconcile regardless of
// node count.
package watches

import (
	"sync"

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GraphWatcher is a scoped handle for a single Graph's reconcile cycle.
//
// Watch requests are buffered locally during the reconcile. The coordinator's
// write lock (mu.Lock) is taken once at Done(true) to flush the buffer,
// instead of once per addWatch call. This reduces Lock acquisitions per
// reconcile from N (node count) to 1, eliminating write-preferring RWMutex
// starvation of routeEvent's RLock under high concurrency.
//
// The per-GraphWatcher mutex protects concurrent writes from DAG node
// goroutines. It's per-graph (not global), so contention is minimal.
type GraphWatcher struct {
	coord   *WatchCoordinator
	graph   GraphKey
	mu      sync.Mutex
	pending []watchRequest
}

func (c *WatchCoordinator) ForGraph(graph GraphKey) *GraphWatcher {
	return &GraphWatcher{coord: c, graph: graph}
}

// WatchScalar buffers a scalar watch request (specific name).
// The request is flushed to the coordinator's indexes in Done(true).
func (gw *GraphWatcher) WatchScalar(nodeID string, gvr schema.GroupVersionResource, kind string, name, namespace string) {
	gw.mu.Lock()
	gw.pending = append(gw.pending, watchRequest{
		nodeID:    nodeID,
		gvr:       gvr,
		kind:      kind,
		name:      name,
		namespace: namespace,
	})
	gw.mu.Unlock()
}

// WatchCollection buffers a selector-scoped kind-level watch request
// for a watch: node. The request is flushed to the coordinator's
// indexes in Done(true).
func (gw *GraphWatcher) WatchCollection(nodeID string, gvr schema.GroupVersionResource, kind string, namespace string, sel labels.Selector) {
	gw.mu.Lock()
	gw.pending = append(gw.pending, watchRequest{
		nodeID:    nodeID,
		gvr:       gvr,
		kind:      kind,
		namespace: namespace,
		selector:  sel,
	})
	gw.mu.Unlock()
}

// RetainWatches carries forward watch requests from the previous cycle
// for a node that was skipped in the current trigger-scoped walk. Without
// this, doneGraph would see skipped nodes' watches as stale and remove
// them from the index, breaking event routing for those nodes.
func (gw *GraphWatcher) RetainWatches(nodeID string) {
	gw.coord.mu.RLock()
	state, ok := gw.coord.graphs[gw.graph]
	if !ok {
		gw.coord.mu.RUnlock()
		return
	}
	// Copy previous cycle's request for this node into pending.
	if req, exists := state.previous[nodeID]; exists {
		gw.mu.Lock()
		gw.pending = append(gw.pending, *req)
		gw.mu.Unlock()
	}
	gw.coord.mu.RUnlock()
}

// GetResourceVersion returns the resourceVersion from the metadata informer
// cache for a specific object. Returns "" if not found or no watch exists.
func (gw *GraphWatcher) GetResourceVersion(gvr schema.GroupVersionResource, namespace, name string) string {
	return gw.coord.Watches.GetResourceVersion(gvr, namespace, name)
}

// GetLabels returns the labels from the metadata informer cache for a specific
// object. Returns nil, false if not found or no watch exists.
func (gw *GraphWatcher) GetLabels(gvr schema.GroupVersionResource, namespace, name string) (map[string]string, bool) {
	return gw.coord.Watches.GetLabels(gvr, namespace, name)
}

// DrainTriggers atomically drains and returns the set of node IDs that
// triggered this Graph's enqueue. An empty set means the reconcile was
// triggered by a resync, error retry, or manual trigger — the reconciler
// should do a full walk. Triggers deposited after drain belong to the
// next reconcile.
func (gw *GraphWatcher) DrainTriggers() map[string]bool {
	gw.coord.triggerMu.Lock()
	defer gw.coord.triggerMu.Unlock()
	triggers := gw.coord.pendingTriggers[gw.graph]
	delete(gw.coord.pendingTriggers, gw.graph)
	return triggers
}

// DepositTrigger adds a node ID to the pending triggers for the next
// reconcile. Used by the walk to schedule re-evaluation of nodes that
// were dispatched with stale readiness data (concurrent workers raced).
func (gw *GraphWatcher) DepositTrigger(nodeID string) {
	gw.coord.triggerMu.Lock()
	defer gw.coord.triggerMu.Unlock()
	if gw.coord.pendingTriggers[gw.graph] == nil {
		gw.coord.pendingTriggers[gw.graph] = make(map[string]bool)
	}
	gw.coord.pendingTriggers[gw.graph][nodeID] = true
}

// DrainCollectionChanges atomically drains and returns the buffered
// collection changes for this Graph's Watch nodes. Returns nil if
// no collection changes were buffered. Used by reconcileWatch to
// GET only changed items instead of re-listing the entire collection.
func (gw *GraphWatcher) DrainCollectionChanges() map[string][]CollectionChange {
	gw.coord.triggerMu.Lock()
	defer gw.coord.triggerMu.Unlock()
	changes := gw.coord.collectionChanges[gw.graph]
	delete(gw.coord.collectionChanges, gw.graph)
	return changes
}

// Done finalizes the watch set for this reconcile cycle.
//
// On commit, the buffered requests are flushed to the coordinator's
// indexes under a single Lock acquisition, then ensureWatch is called
// for each GVR outside the lock.
//
// On abort (reconcile error), the buffer is discarded. This is safe
// because buffered requests were never applied to the coordinator's
// indexes — the previous cycle's index entries remain intact and
// continue routing events correctly. Failed reconciles don't start
// new informers; the retry will call doneGraph on success.
func (gw *GraphWatcher) Done(commit bool) {
	// Lock is defense-in-depth: all DAG node goroutines have returned
	// by the time Done is called (the coordinator drains the results
	// channel before returning). No concurrent appends are possible.
	gw.mu.Lock()
	pending := gw.pending
	gw.pending = nil
	gw.mu.Unlock()

	if commit {
		gw.coord.doneGraph(gw.graph, pending)
	}
}
