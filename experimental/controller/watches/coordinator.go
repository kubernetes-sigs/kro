// coordinator.go implements WatchCoordinator — event routing from informers to
// the correct Graph(s) using scalar (name-based) and collection (selector-based)
// reverse indexes. Watches are persistent per-graph: registered immediately and
// held until the graph is deleted.
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

// nodeWatch describes a resource a Graph node is watching.
type nodeWatch struct {
	gvr       schema.GroupVersionResource
	kind      string
	name      string          // empty for collections
	namespace string
	selector  labels.Selector // nil for scalars
}

func (w *nodeWatch) isCollection() bool { return w.selector != nil }

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

// WatchCoordinator routes watch events to the correct Graph(s).
type WatchCoordinator struct {
	mu sync.RWMutex

	watches *WatchManager
	enqueue func(GraphKey) // enqueues a Graph for reconciliation
	log     logr.Logger

	// Persistent watch state per graph, per node
	graphWatches    map[GraphKey]map[string]*nodeWatch // graphKey → nodeID → watch entry
	scalarIndex     map[schema.GroupVersionResource]map[types.NamespacedName][]scalarEntry
	collectionIndex map[schema.GroupVersionResource][]collectionEntry
}

func NewWatchCoordinator(watches *WatchManager, enqueue func(GraphKey), log logr.Logger) *WatchCoordinator {
	c := &WatchCoordinator{
		watches:         watches,
		enqueue:         enqueue,
		log:             log.WithName("watch-coordinator"),
		graphWatches:    make(map[GraphKey]map[string]*nodeWatch),
		scalarIndex:     make(map[schema.GroupVersionResource]map[types.NamespacedName][]scalarEntry),
		collectionIndex: make(map[schema.GroupVersionResource][]collectionEntry),
	}
	watches.onEvent = c.RouteEvent
	return c
}

// ensureNodeWatch is the core operation. Idempotent: same params = no-op,
// different params = swap old entry for new, new node = add.
func (c *WatchCoordinator) ensureNodeWatch(graphKey GraphKey, nodeID string, gvr schema.GroupVersionResource, kind, name, namespace string, selector labels.Selector) {
	c.mu.Lock()

	nodes, ok := c.graphWatches[graphKey]
	if !ok {
		nodes = make(map[string]*nodeWatch)
		c.graphWatches[graphKey] = nodes
	}

	newWatch := &nodeWatch{
		gvr:       gvr,
		kind:      kind,
		name:      name,
		namespace: namespace,
		selector:  selector,
	}

	if existing, exists := nodes[nodeID]; exists {
		if sameNodeWatch(existing, newWatch) {
			c.mu.Unlock()
			return // idempotent no-op
		}
		// Different params — remove old routing entry before adding new one
		c.removeNodeFromIndexesLocked(graphKey, nodeID, existing)
	}

	nodes[nodeID] = newWatch
	if newWatch.isCollection() {
		c.addCollectionLocked(graphKey, nodeID, newWatch)
	} else {
		c.addScalarLocked(graphKey, nodeID, newWatch)
	}
	c.mu.Unlock()

	// Ensure informer running (outside lock). Idempotent and ref-counted.
	ownerID := GraphOwnerID(graphKey)
	if err := c.watches.EnsureWatch(gvr, kind, ownerID); err != nil {
		c.log.Error(err, "failed to ensure watch", "gvr", gvr)
	}
}

// RemoveGraph removes all watch state for a deleted Graph.
func (c *WatchCoordinator) RemoveGraph(graphKey GraphKey) {
	c.log.V(1).Info("removing graph watch state", "graph", graphKey)
	c.mu.Lock()

	nodes, ok := c.graphWatches[graphKey]
	if !ok {
		c.mu.Unlock()
		return
	}

	// Collect GVRs and remove all index entries.
	gvrSet := make(map[schema.GroupVersionResource]bool)
	for nodeID, w := range nodes {
		c.removeNodeFromIndexesLocked(graphKey, nodeID, w)
		gvrSet[w.gvr] = true
	}
	delete(c.graphWatches, graphKey)

	c.mu.Unlock()

	ownerID := GraphOwnerID(graphKey)
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

func (c *WatchCoordinator) addScalarLocked(graphKey GraphKey, nodeID string, w *nodeWatch) {
	byName, ok := c.scalarIndex[w.gvr]
	if !ok {
		byName = make(map[types.NamespacedName][]scalarEntry)
		c.scalarIndex[w.gvr] = byName
	}
	nn := types.NamespacedName{Name: w.name, Namespace: w.namespace}
	for _, e := range byName[nn] {
		if e.graph == graphKey && e.nodeID == nodeID {
			return // dedup
		}
	}
	byName[nn] = append(byName[nn], scalarEntry{nodeID: nodeID, graph: graphKey})
}

func (c *WatchCoordinator) addCollectionLocked(graphKey GraphKey, nodeID string, w *nodeWatch) {
	entries := c.collectionIndex[w.gvr]
	for _, e := range entries {
		if e.graph == graphKey && e.nodeID == nodeID && e.namespace == w.namespace && e.selector.String() == w.selector.String() {
			return // dedup
		}
	}
	c.collectionIndex[w.gvr] = append(entries, collectionEntry{
		nodeID:    nodeID,
		selector:  w.selector,
		namespace: w.namespace,
		graph:     graphKey,
	})
}

func (c *WatchCoordinator) removeNodeFromIndexesLocked(graphKey GraphKey, nodeID string, w *nodeWatch) {
	if w.isCollection() {
		c.removeCollectionLocked(graphKey, nodeID, w)
	} else {
		c.removeScalarLocked(graphKey, nodeID, w)
	}
}

func (c *WatchCoordinator) removeScalarLocked(graphKey GraphKey, nodeID string, w *nodeWatch) {
	byName, ok := c.scalarIndex[w.gvr]
	if !ok {
		return
	}
	nn := types.NamespacedName{Name: w.name, Namespace: w.namespace}
	entries := byName[nn]
	filtered := entries[:0]
	for _, e := range entries {
		if e.graph == graphKey && e.nodeID == nodeID {
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
		delete(c.scalarIndex, w.gvr)
	}
}

func (c *WatchCoordinator) removeCollectionLocked(graphKey GraphKey, nodeID string, w *nodeWatch) {
	entries := c.collectionIndex[w.gvr]
	filtered := entries[:0]
	for _, e := range entries {
		if e.graph == graphKey && e.nodeID == nodeID && e.namespace == w.namespace && e.selector.String() == w.selector.String() {
			continue
		}
		filtered = append(filtered, e)
	}
	if len(filtered) == 0 {
		delete(c.collectionIndex, w.gvr)
	} else {
		c.collectionIndex[w.gvr] = filtered
	}
}

// GraphOwnerID returns a stable owner identifier for a Graph in the
// WatchManager. Using per-graph IDs (instead of a shared constant) enables
// correct ref-counting: releasing one graph's ownership of a GVR cannot
// kill an informer that another graph still needs.
func GraphOwnerID(graph GraphKey) string {
	return "graph/" + graph.Namespace + "/" + graph.Name
}

func sameNodeWatch(a, b *nodeWatch) bool {
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
