// coordinator.go implements WatchCoordinator — event routing from informers to
// the correct Graph(s) using scalar (name-based) and collection (selector-based)
// reverse indexes. The double-buffer (current/previous) pattern auto-cleans
// stale watches between reconcile cycles.
package watches

import (
	"sync"

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

	Watches *WatchManager
	enqueue func(GraphKey) // enqueues a Graph for reconciliation
	log     logr.Logger

	graphs          map[GraphKey]*graphState
	scalarIndex     map[schema.GroupVersionResource]map[types.NamespacedName][]scalarEntry
	collectionIndex map[schema.GroupVersionResource][]collectionEntry

	// pendingTriggers records which node IDs triggered each Graph's enqueue.
	// Populated by routeEvent, drained by DrainTriggers at reconcile start.
	// Uses a separate mutex for clean atomic drain without blocking event routing.
	triggerMu       sync.Mutex
	pendingTriggers map[GraphKey]map[string]bool // graph → set of triggering node IDs

	// collectionChanges buffers changed resource keys for Watch nodes.
	// When a collection-matched watch event fires, the specific resource
	// (namespace/name + event type) is recorded alongside the node ID trigger.
	// This enables reconcileWatch to GET only changed items instead of
	// re-listing the entire collection — O(changed) per reconcile, not
	// O(matching). Per 005-reconciliation.md § Propagation: "When a single
	// resource changes, update the cached list incrementally rather than
	// re-listing — O(1) per event, not O(matching)."
	// Protected by triggerMu (same lock as pendingTriggers — deposited and
	// drained together).
	collectionChanges map[GraphKey]map[string][]CollectionChange // graph → nodeID → changes
}

// CollectionChange records a specific resource change within a Watch collection.
type CollectionChange struct {
	Namespace string
	Name      string
	EventType WatchEventType
}

func NewWatchCoordinator(watches *WatchManager, enqueue func(GraphKey), log logr.Logger) *WatchCoordinator {
	return &WatchCoordinator{
		Watches:           watches,
		enqueue:           enqueue,
		log:               log.WithName("watch-coordinator"),
		graphs:            make(map[GraphKey]*graphState),
		scalarIndex:       make(map[schema.GroupVersionResource]map[types.NamespacedName][]scalarEntry),
		collectionIndex:   make(map[schema.GroupVersionResource][]collectionEntry),
		pendingTriggers:   make(map[GraphKey]map[string]bool),
		collectionChanges: make(map[GraphKey]map[string][]CollectionChange),
	}
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
	var newNodeIDs []string
	hasNewIndexEntries := false
	for i := range pending {
		req := &pending[i]

		// Handle nodeID reuse with different target
		if old, exists := state.current[req.nodeID]; exists {
			if !sameTarget(old, req) {
				if prev, shared := state.previous[req.nodeID]; !shared || !sameTarget(prev, old) {
					c.removeFromIndexesLocked(graph, old)
				}
			}
		}

		state.current[req.nodeID] = req

		// Only add to index if not already covered by previous cycle
		if prev, shared := state.previous[req.nodeID]; !shared || !sameTarget(prev, req) {
			if req.isCollection() {
				c.addCollectionLocked(graph, *req)
			} else {
				c.addScalarLocked(graph, *req)
			}
			newNodeIDs = append(newNodeIDs, req.nodeID)
			hasNewIndexEntries = true
		}
		newGVRs[req.gvr] = req.kind
	}

	// Clean stale entries from previous cycle.
	var affectedGVRs []schema.GroupVersionResource
	for nodeID, oldReq := range state.previous {
		if newReq, active := state.current[nodeID]; active && sameTarget(newReq, oldReq) {
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
		"newWatches", len(newNodeIDs), "staleWatches", len(affectedGVRs),
		"toRelease", len(toRelease))

	// Ensure informers running for watched GVRs (outside lock).
	// ensureWatch is idempotent and ref-counted — calling it for
	// GVRs already running just bumps the owner set.
	//
	// Timing: ensureWatch runs here (post-reconcile) rather than
	// during WatchScalar/WatchCollection. For a brand-new GVR, the
	// informer won't be running during its first reconcile cycle.
	// This is safe: GetResourceVersion returns "" which triggers a
	// fallback GET in applySSA. On subsequent
	// reconciles the informer is already running from this call.
	ownerID := GraphOwnerID(graph)
	for gvr, kind := range newGVRs {
		if err := c.Watches.EnsureWatch(gvr, kind, ownerID); err != nil {
			c.log.Error(err, "failed to ensure watch", "gvr", gvr)
		}
	}
	for _, gvr := range toRelease {
		c.Watches.releaseWatch(gvr, ownerID)
	}

	if hasNewIndexEntries {
		c.triggerMu.Lock()
		if c.pendingTriggers[graph] == nil {
			c.pendingTriggers[graph] = make(map[string]bool)
		}
		for _, nodeID := range newNodeIDs {
			c.pendingTriggers[graph][nodeID] = true
		}
		c.triggerMu.Unlock()
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

	// Collect all GVRs this graph watches (from both buffers) and
	// remove all index entries.
	gvrSet := make(map[schema.GroupVersionResource]bool)
	for _, req := range state.current {
		c.removeFromIndexesLocked(graph, req)
		gvrSet[req.gvr] = true
	}
	for _, req := range state.previous {
		c.removeFromIndexesLocked(graph, req)
		gvrSet[req.gvr] = true
	}
	delete(c.graphs, graph)

	c.mu.Unlock()

	ownerID := GraphOwnerID(graph)
	for gvr := range gvrSet {
		c.Watches.releaseWatch(gvr, ownerID)
	}
}

// RouteEvent routes an informer event to all matching Graphs and records
// the triggering node IDs for scoped walks.
func (c *WatchCoordinator) RouteEvent(event watchEvent) {
	c.mu.RLock()
	// matched maps graph → set of triggering node IDs
	matched := make(map[GraphKey]map[string]bool)
	// collectionMatched tracks which matches came from collection (Watch)
	// entries, so the changed resource key can be buffered for incremental cache.
	collectionMatched := make(map[GraphKey]map[string]bool)

	// Scalar matches: O(1) per GVR+name
	if byName, ok := c.scalarIndex[event.gvr]; ok {
		key := types.NamespacedName{Name: event.name, Namespace: event.namespace}
		for _, entry := range byName[key] {
			if matched[entry.graph] == nil {
				matched[entry.graph] = map[string]bool{}
			}
			matched[entry.graph][entry.nodeID] = true
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
			if matched[entry.graph] == nil {
				matched[entry.graph] = map[string]bool{}
			}
			matched[entry.graph][entry.nodeID] = true
			if collectionMatched[entry.graph] == nil {
				collectionMatched[entry.graph] = map[string]bool{}
			}
			collectionMatched[entry.graph][entry.nodeID] = true
		} else if len(event.oldLabels) > 0 && entry.selector.Matches(labels.Set(event.oldLabels)) {
			if matched[entry.graph] == nil {
				matched[entry.graph] = map[string]bool{}
			}
			matched[entry.graph][entry.nodeID] = true
			if collectionMatched[entry.graph] == nil {
				collectionMatched[entry.graph] = map[string]bool{}
			}
			collectionMatched[entry.graph][entry.nodeID] = true
		}
	}
	c.mu.RUnlock()

	// Deposit triggers and enqueue. Multiple events between reconciles
	// naturally union into a larger trigger set.
	if len(matched) > 0 {
		c.triggerMu.Lock()
		for graph, nodeIDs := range matched {
			if c.pendingTriggers[graph] == nil {
				c.pendingTriggers[graph] = map[string]bool{}
			}
			for nodeID := range nodeIDs {
				c.pendingTriggers[graph][nodeID] = true
			}
		}
		// Buffer collection changes for Watch incremental cache.
		// The changed resource key is recorded per node so reconcileWatch
		// can GET only the changed items instead of re-listing.
		for graph, nodeIDs := range collectionMatched {
			if c.collectionChanges[graph] == nil {
				c.collectionChanges[graph] = map[string][]CollectionChange{}
			}
			for nodeID := range nodeIDs {
				c.collectionChanges[graph][nodeID] = append(
					c.collectionChanges[graph][nodeID],
					CollectionChange{
						Namespace: event.namespace,
						Name:      event.name,
						EventType: event.eventType,
					},
				)
			}
		}
		c.triggerMu.Unlock()

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
