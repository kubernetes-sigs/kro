// coordinator.go implements WatchCoordinator — event routing from informers to
// the correct Graph(s) using scalar (name-based) and collection (selector-based)
// reverse indexes. The double-buffer (current/previous) pattern auto-cleans
// stale watches between reconcile cycles.
package watches

import (
	"sync"

	"github.com/ellistarn/kro/experimental/controller/graph"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// GraphKey identifies a Graph object. Type alias keeps call sites clean;
// a distinct type would add conversion noise for marginal safety.
type GraphKey = types.NamespacedName

// watchRequest describes a resource a Graph wants to watch.
type watchRequest struct {
	nodeID    string
	gvr       schema.GroupVersionResource
	kind      string
	name      string
	namespace string
	selector  labels.Selector // non-nil for Watch
}

func (r *watchRequest) isCollection() bool { return r.selector != nil }

// bufferKey returns a unique key for this watch request within a graph's
// watch buffer. For scalar watches, the key includes the target identity
// (name+namespace) because a single forEach node can watch multiple
// distinct resources. For collection watches, nodeID alone suffices
// because each watch node has exactly one selector.
func (r *watchRequest) bufferKey() string {
	if r.isCollection() {
		return r.nodeID
	}
	return r.nodeID + "\x00" + r.namespace + "\x00" + r.name
}

type scalarEntry struct {
	nodeID string
	graph  GraphKey
}

type collectionEntry struct {
	nodeID    string
	selector  labels.Selector
	namespace string
	graph     GraphKey
}

type graphState struct {
	current  map[string]*watchRequest
	previous map[string]*watchRequest
}

// WatchCoordinator routes watch events to the correct Graph(s).
type WatchCoordinator struct {
	mu sync.RWMutex

	watches *WatchManager
	enqueue func(GraphKey) // enqueues a Graph for reconciliation
	log     logr.Logger

	graphs          map[GraphKey]*graphState
	scalarIndex     map[schema.GroupVersionResource]map[types.NamespacedName][]scalarEntry
	collectionIndex map[schema.GroupVersionResource][]collectionEntry
}

func NewWatchCoordinator(watches *WatchManager, enqueue func(GraphKey), log logr.Logger) *WatchCoordinator {
	c := &WatchCoordinator{
		watches:         watches,
		enqueue:         enqueue,
		log:             log.WithName("watch-coordinator"),
		graphs:          make(map[GraphKey]*graphState),
		scalarIndex:     make(map[schema.GroupVersionResource]map[types.NamespacedName][]scalarEntry),
		collectionIndex: make(map[schema.GroupVersionResource][]collectionEntry),
	}
	watches.onEvent = c.RouteEvent
	return c
}

func (c *WatchCoordinator) doneGraph(graph GraphKey, pending []watchRequest) {
	c.mu.Lock()

	state, ok := c.graphs[graph]
	if !ok {
		state = &graphState{
			current:  make(map[string]*watchRequest),
			previous: make(map[string]*watchRequest),
		}
		c.graphs[graph] = state
	}

	// Flush all buffered watch requests into state.current and indexes.
	// This is the sole write-lock acquisition for the entire reconcile cycle.
	//
	// Lifetime note: state.current stores pointers into the pending slice.
	// The backing array is pinned by these pointers until the next doneGraph
	// swaps current→previous and reallocates current. This is correct but
	// means the entire pending array stays live, not individual elements.
	newGVRs := make(map[schema.GroupVersionResource]string)
	newWatchCount := 0
	hasNewIndexEntries := false
	for i := range pending {
		req := &pending[i]
		bk := req.bufferKey()

		// Handle buffer-key reuse with different target. For scalar
		// watches the target identity is embedded in the key, so
		// collisions only happen for collection watches where the
		// selector changed.
		if old, exists := state.current[bk]; exists {
			if !sameTarget(old, req) {
				if prev, shared := state.previous[bk]; !shared || !sameTarget(prev, old) {
					c.removeFromIndexesLocked(graph, old)
				}
			}
		}

		state.current[bk] = req

		// Only add to index if not already covered by previous cycle
		if prev, shared := state.previous[bk]; !shared || !sameTarget(prev, req) {
			if req.isCollection() {
				c.addCollectionLocked(graph, *req)
			} else {
				c.addScalarLocked(graph, *req)
			}
			newWatchCount++
			hasNewIndexEntries = true
		}
		newGVRs[req.gvr] = req.kind
	}

	// Clean stale entries from previous cycle.
	var affectedGVRs []schema.GroupVersionResource
	for bk, oldReq := range state.previous {
		if newReq, active := state.current[bk]; active && sameTarget(newReq, oldReq) {
			continue
		}
		c.removeFromIndexesLocked(graph, oldReq)
		affectedGVRs = append(affectedGVRs, oldReq.gvr)
	}

	state.previous = state.current
	state.current = make(map[string]*watchRequest)

	// Compute GVRs this graph still actively watches (now in previous
	// after the swap). Release per-graph ownership only for GVRs the
	// graph no longer needs — the WatchManager ref-counts across graphs.
	toRelease := c.gvrsToReleaseLocked(state.previous, affectedGVRs)
	c.mu.Unlock()

	c.log.V(1).Info("watch cycle flushed", "graph", graph,
		"newWatches", newWatchCount, "staleWatches", len(affectedGVRs),
		"toRelease", len(toRelease))

	// Ensure informers running for watched GVRs (outside lock).
	// ensureWatch is idempotent and ref-counted — calling it for
	// GVRs already running just bumps the owner set.
	//
	// Timing: ensureWatch runs here (post-reconcile) rather than
	// during WatchScalar/WatchCollection. For a brand-new GVR, the
	// informer won't be running during its first reconcile cycle.
	// This is safe: applySSA falls back to a direct GET when the
	// informer hasn't observed the resource yet. On subsequent
	// reconciles the informer is already running from this call.
	ownerID := GraphOwnerID(graph)
	for gvr, kind := range newGVRs {
		if err := c.watches.EnsureWatch(gvr, kind, ownerID); err != nil {
			c.log.Error(err, "failed to ensure watch", "gvr", gvr)
		}
	}
	for _, gvr := range toRelease {
		c.watches.releaseWatch(gvr, ownerID)
	}

	if hasNewIndexEntries {
		c.enqueue(graph)
	}
}

// RemoveGraph removes all watch state for a deleted Graph.
func (c *WatchCoordinator) RemoveGraph(graph GraphKey) {
	c.log.V(1).Info("removing graph watch state", "graph", graph)
	c.mu.Lock()

	state, ok := c.graphs[graph]
	if !ok {
		c.mu.Unlock()
		return
	}

	// Collect all GVRs this graph watches and remove all index entries.
	// After doneGraph's double-buffer swap, state.current is always empty —
	// only state.previous holds the active watch set.
	gvrSet := make(map[schema.GroupVersionResource]bool)
	for _, req := range state.previous {
		c.removeFromIndexesLocked(graph, req)
		gvrSet[req.gvr] = true
	}
	delete(c.graphs, graph)

	c.mu.Unlock()

	ownerID := GraphOwnerID(graph)
	for gvr := range gvrSet {
		c.watches.releaseWatch(gvr, ownerID)
	}
}

// DeriveAppliedSet delegates to the underlying WatchManager.
func (c *WatchCoordinator) DeriveAppliedSet(graphName, namespace string) map[string]graph.AppliedEntry {
	return c.watches.DeriveAppliedSet(graphName, namespace)
}

// GetLabels delegates to the underlying WatchManager.
func (c *WatchCoordinator) GetLabels(gvr schema.GroupVersionResource, namespace, name string) (map[string]string, bool) {
	return c.watches.GetLabels(gvr, namespace, name)
}

// RouteEvent routes an informer event to all matching Graphs and enqueues
// them for reconciliation.
func (c *WatchCoordinator) RouteEvent(event watchEvent) {
	c.mu.RLock()
	matched := make(map[GraphKey]bool)

	// Scalar matches: O(1) per GVR+name
	if byName, ok := c.scalarIndex[event.gvr]; ok {
		key := types.NamespacedName{Name: event.name, Namespace: event.namespace}
		for _, entry := range byName[key] {
			matched[entry.graph] = true
		}
	}

	// Collection matches: selector scan
	for _, entry := range c.collectionIndex[event.gvr] {
		// Skip if both sides are namespace-scoped and don't match.
		// Cluster-scoped events (namespace "") match any entry.
		if entry.namespace != "" && event.namespace != "" && event.namespace != entry.namespace {
			continue
		}
		if entry.selector.Matches(labels.Set(event.labels)) {
			matched[entry.graph] = true
		} else if len(event.oldLabels) > 0 && entry.selector.Matches(labels.Set(event.oldLabels)) {
			matched[entry.graph] = true
		}
	}
	c.mu.RUnlock()

	if len(matched) > 0 {
		for graph := range matched {
			c.enqueue(graph)
		}

		c.log.V(2).Info("routed event", "gvr", event.gvr, "name", event.name,
			"namespace", event.namespace, "type", event.eventType,
			"matchCount", len(matched))
	}
}

// --- index helpers ---

func (c *WatchCoordinator) addScalarLocked(graph GraphKey, req watchRequest) {
	byName, ok := c.scalarIndex[req.gvr]
	if !ok {
		byName = make(map[types.NamespacedName][]scalarEntry)
		c.scalarIndex[req.gvr] = byName
	}
	nn := types.NamespacedName{Name: req.name, Namespace: req.namespace}
	for _, e := range byName[nn] {
		if e.graph == graph && e.nodeID == req.nodeID {
			return // dedup
		}
	}
	byName[nn] = append(byName[nn], scalarEntry{nodeID: req.nodeID, graph: graph})
}

func (c *WatchCoordinator) addCollectionLocked(graph GraphKey, req watchRequest) {
	entries := c.collectionIndex[req.gvr]
	for _, e := range entries {
		if e.graph == graph && e.nodeID == req.nodeID && e.namespace == req.namespace && e.selector.String() == req.selector.String() {
			return // dedup
		}
	}
	c.collectionIndex[req.gvr] = append(entries, collectionEntry{
		nodeID:    req.nodeID,
		selector:  req.selector,
		namespace: req.namespace,
		graph:     graph,
	})
}

func (c *WatchCoordinator) removeFromIndexesLocked(graph GraphKey, req *watchRequest) {
	if req.isCollection() {
		c.removeCollectionLocked(graph, req)
	} else {
		c.removeScalarLocked(graph, req)
	}
}

func (c *WatchCoordinator) removeScalarLocked(graph GraphKey, req *watchRequest) {
	byName, ok := c.scalarIndex[req.gvr]
	if !ok {
		return
	}
	nn := types.NamespacedName{Name: req.name, Namespace: req.namespace}
	entries := byName[nn]
	filtered := entries[:0]
	for _, e := range entries {
		if e.graph == graph && e.nodeID == req.nodeID {
			continue
		}
		filtered = append(filtered, e)
	}
	if len(filtered) == 0 {
		delete(byName, nn)
	} else {
		byName[nn] = filtered
	}
	if len(byName) == 0 {
		delete(c.scalarIndex, req.gvr)
	}
}

func (c *WatchCoordinator) removeCollectionLocked(graph GraphKey, req *watchRequest) {
	entries := c.collectionIndex[req.gvr]
	filtered := entries[:0]
	for _, e := range entries {
		if e.graph == graph && e.nodeID == req.nodeID && e.namespace == req.namespace && e.selector.String() == req.selector.String() {
			continue
		}
		filtered = append(filtered, e)
	}
	if len(filtered) == 0 {
		delete(c.collectionIndex, req.gvr)
	} else {
		c.collectionIndex[req.gvr] = filtered
	}
}

// gvrsToReleaseLocked returns the subset of affectedGVRs that the graph no
// longer actively watches. activeWatches is the graph's committed watch set
// (state.previous after doneGraph's buffer-swap).
// Must be called with c.mu held.
func (c *WatchCoordinator) gvrsToReleaseLocked(activeWatches map[string]*watchRequest, affectedGVRs []schema.GroupVersionResource) []schema.GroupVersionResource {
	activeGVRs := make(map[schema.GroupVersionResource]bool, len(activeWatches))
	for _, req := range activeWatches {
		activeGVRs[req.gvr] = true
	}
	var toRelease []schema.GroupVersionResource
	seen := make(map[schema.GroupVersionResource]bool)
	for _, gvr := range affectedGVRs {
		if seen[gvr] {
			continue
		}
		seen[gvr] = true
		if !activeGVRs[gvr] {
			toRelease = append(toRelease, gvr)
		}
	}
	return toRelease
}

// GraphOwnerID returns a stable owner identifier for a Graph in the
// WatchManager. Using per-graph IDs (instead of a shared constant) enables
// correct ref-counting: releasing one graph's ownership of a GVR cannot
// kill an informer that another graph still needs.
func GraphOwnerID(graph GraphKey) string {
	return "graph/" + graph.Namespace + "/" + graph.Name
}

func sameTarget(a, b *watchRequest) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.gvr != b.gvr || a.name != b.name || a.namespace != b.namespace {
		return false
	}
	if a.isCollection() != b.isCollection() {
		return false
	}
	if !a.isCollection() {
		return true
	}
	return a.selector.String() == b.selector.String()
}
