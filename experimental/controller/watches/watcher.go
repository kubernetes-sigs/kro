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

// GetLabels returns the labels from the metadata informer cache for a specific
// object. Returns nil, false if not found or no watch exists.
func (gw *GraphWatcher) GetLabels(gvr schema.GroupVersionResource, namespace, name string) (map[string]string, bool) {
	return gw.coord.GetLabels(gvr, namespace, name)
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
	// Lock protects against concurrent WatchScalar/WatchCollection calls from
	// DAG node goroutines during the parallel walk. Required, not defense-in-depth.
	gw.mu.Lock()
	pending := gw.pending
	gw.pending = nil
	gw.mu.Unlock()

	if commit {
		gw.coord.doneGraph(gw.graph, pending)
	}
}
