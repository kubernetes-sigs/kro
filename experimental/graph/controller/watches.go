// watches.go implements dynamic watch management for the Graph controller.
//
// The architecture mirrors pkg/dynamiccontroller/{watch_manager,coordinator}.go
// but simplified for the Graph controller's needs:
//
//   - WatchManager: one metadata-only informer per GVR, ref-counted.
//   - WatchCoordinator: routes events from informers to the correct Graph(s)
//     using scalar (name-based) and collection (selector-based) reverse indexes.
//   - Reconcile-time registration: watches are declared during each reconcile.
//     The double-buffer (current/previous) pattern auto-cleans stale watches.
package graphcontroller

import (
	"context"
	"fmt"
	"maps"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/metadata/metadatainformer"
	"k8s.io/client-go/tools/cache"
)

// ---------- WatchManager ----------

// WatchManager maintains one metadata-only informer per GVR, ref-counted by owners.
type WatchManager struct {
	mu      sync.Mutex
	watches map[schema.GroupVersionResource]*gvrWatch
	owners  map[schema.GroupVersionResource]map[string]struct{}
	client  metadata.Interface
	resync  time.Duration
	onEvent func(watchEvent)
	log     logr.Logger

	// createInformer is overridable for tests.
	createInformer func(schema.GroupVersionResource) cache.SharedIndexInformer
}

type gvrWatch struct {
	informer   cache.SharedIndexInformer
	handlerReg cache.ResourceEventHandlerRegistration
	cancel     context.CancelFunc
}

// WatchEventType classifies the Kubernetes event that triggered a watch callback.
type WatchEventType string

const (
	WatchEventAdd    WatchEventType = "add"
	WatchEventUpdate WatchEventType = "update"
	WatchEventDelete WatchEventType = "delete"
)

type watchEvent struct {
	eventType WatchEventType
	gvr       schema.GroupVersionResource
	name      string
	namespace string
	labels    map[string]string
	oldLabels map[string]string
}

func newWatchManager(client metadata.Interface, resync time.Duration, onEvent func(watchEvent), log logr.Logger) *WatchManager {
	wm := &WatchManager{
		watches: make(map[schema.GroupVersionResource]*gvrWatch),
		owners:  make(map[schema.GroupVersionResource]map[string]struct{}),
		client:  client,
		resync:  resync,
		onEvent: onEvent,
		log:     log.WithName("watch-manager"),
	}
	wm.createInformer = wm.defaultCreateInformer
	return wm
}

func (m *WatchManager) ensureWatch(gvr schema.GroupVersionResource, ownerID string) error {
	m.mu.Lock()
	if m.owners[gvr] == nil {
		m.owners[gvr] = make(map[string]struct{})
	}
	m.owners[gvr][ownerID] = struct{}{}

	if _, ok := m.watches[gvr]; ok {
		m.mu.Unlock()
		return nil
	}

	// Create and start
	inf := m.createInformer(gvr)
	ctx, cancel := context.WithCancel(context.Background())
	w := &gvrWatch{informer: inf, cancel: cancel}

	reg, err := inf.AddEventHandler(m.eventHandlers(gvr))
	if err != nil {
		cancel()
		m.mu.Unlock()
		return fmt.Errorf("adding event handler for %s: %w", gvr, err)
	}
	w.handlerReg = reg
	m.watches[gvr] = w

	go inf.RunWithContext(ctx)
	m.log.V(1).Info("informer started", "gvr", gvr)
	m.mu.Unlock()

	// Wait for sync outside the lock
	syncCtx, syncCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer syncCancel()
	if !cache.WaitForCacheSync(syncCtx.Done(), inf.HasSynced) {
		m.forceStop(gvr)
		return fmt.Errorf("cache sync timeout for %s", gvr)
	}

	return nil
}

func (m *WatchManager) releaseWatch(gvr schema.GroupVersionResource, ownerID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if owners := m.owners[gvr]; owners != nil {
		delete(owners, ownerID)
		if len(owners) == 0 {
			delete(m.owners, gvr)
		}
	}
	m.stopLocked(gvr, false)
}

func (m *WatchManager) forceStop(gvr schema.GroupVersionResource) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopLocked(gvr, true)
}

func (m *WatchManager) stopLocked(gvr schema.GroupVersionResource, force bool) {
	w, ok := m.watches[gvr]
	if !ok {
		return
	}
	if !force && len(m.owners[gvr]) > 0 {
		return
	}
	_ = w.informer.RemoveEventHandler(w.handlerReg) // best-effort; informer is being cancelled anyway
	w.cancel()
	delete(m.watches, gvr)
	m.log.V(1).Info("informer stopped", "gvr", gvr)
}

func (m *WatchManager) shutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for gvr := range m.watches {
		m.stopLocked(gvr, true)
	}
}

// getResourceVersion returns the resourceVersion of an object from the
// metadata informer cache, if a watch is active for the GVR. Returns ""
// if the object is not found or no watch exists.
func (m *WatchManager) getResourceVersion(gvr schema.GroupVersionResource, namespace, name string) string {
	m.mu.Lock()
	w, ok := m.watches[gvr]
	m.mu.Unlock()
	if !ok {
		return ""
	}

	key := name
	if namespace != "" {
		key = namespace + "/" + name
	}

	item, exists, err := w.informer.GetStore().GetByKey(key)
	if err != nil || !exists {
		return ""
	}

	accessor, err := meta.Accessor(item)
	if err != nil {
		return ""
	}
	return accessor.GetResourceVersion()
}

func (m *WatchManager) defaultCreateInformer(gvr schema.GroupVersionResource) cache.SharedIndexInformer {
	return metadatainformer.NewFilteredMetadataInformer(
		m.client, gvr, metav1.NamespaceAll, m.resync,
		cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
		nil,
	).Informer()
}

func (m *WatchManager) eventHandlers(gvr schema.GroupVersionResource) cache.ResourceEventHandlerFuncs {
	toEvent := func(obj interface{}, et WatchEventType) *watchEvent {
		mobj, err := meta.Accessor(obj)
		if err != nil {
			return nil
		}
		return &watchEvent{
			eventType: et,
			gvr:       gvr,
			name:      mobj.GetName(),
			namespace: mobj.GetNamespace(),
			labels:    maps.Clone(mobj.GetLabels()),
		}
	}

	return cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			if e := toEvent(obj, WatchEventAdd); e != nil {
				m.onEvent(*e)
			}
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			e := toEvent(newObj, WatchEventUpdate)
			if e == nil {
				return
			}
			if oldMeta, err := meta.Accessor(oldObj); err == nil {
				e.oldLabels = maps.Clone(oldMeta.GetLabels())
			}
			m.onEvent(*e)
		},
		DeleteFunc: func(obj interface{}) {
			if d, ok := obj.(cache.DeletedFinalStateUnknown); ok {
				obj = d.Obj
			}
			if e := toEvent(obj, WatchEventDelete); e != nil {
				m.onEvent(*e)
			}
		},
	}
}

// ---------- WatchCoordinator ----------

// graphKey identifies a Graph object. Type alias keeps call sites clean;
// a distinct type would add conversion noise for marginal safety.
type graphKey = types.NamespacedName

// watchRequest describes a resource a Graph wants to watch.
type watchRequest struct {
	nodeID    string
	gvr       schema.GroupVersionResource
	name      string
	namespace string
	selector  labels.Selector // non-nil for collection watches
}

func (r *watchRequest) isCollection() bool { return r.selector != nil }

type scalarEntry struct {
	nodeID string
	graph  graphKey
}

type collectionEntry struct {
	nodeID    string
	selector  labels.Selector
	namespace string
	graph     graphKey
}

type graphState struct {
	current  map[string]*watchRequest
	previous map[string]*watchRequest
}

// WatchCoordinator routes watch events to the correct Graph(s).
type WatchCoordinator struct {
	mu sync.RWMutex

	watches *WatchManager
	enqueue func(graphKey) // enqueues a Graph for reconciliation
	log     logr.Logger

	graphs          map[graphKey]*graphState
	scalarIndex     map[schema.GroupVersionResource]map[types.NamespacedName][]scalarEntry
	collectionIndex map[schema.GroupVersionResource][]collectionEntry

	// pendingTriggers records which node IDs triggered each Graph's enqueue.
	// Populated by routeEvent, drained by drainTriggers at reconcile start.
	// Uses a separate mutex for clean atomic drain without blocking event routing.
	triggerMu       sync.Mutex
	pendingTriggers map[graphKey]map[string]bool // graph → set of triggering node IDs
}

func newWatchCoordinator(watches *WatchManager, enqueue func(graphKey), log logr.Logger) *WatchCoordinator {
	return &WatchCoordinator{
		watches:         watches,
		enqueue:         enqueue,
		log:             log.WithName("watch-coordinator"),
		graphs:          make(map[graphKey]*graphState),
		scalarIndex:     make(map[schema.GroupVersionResource]map[types.NamespacedName][]scalarEntry),
		collectionIndex: make(map[schema.GroupVersionResource][]collectionEntry),
		pendingTriggers: make(map[graphKey]map[string]bool),
	}
}

// graphWatcher is a scoped handle for a single Graph's reconcile cycle.
type graphWatcher struct {
	coord *WatchCoordinator
	graph graphKey
}

func (c *WatchCoordinator) forGraph(graph graphKey) *graphWatcher {
	return &graphWatcher{coord: c, graph: graph}
}

// watchScalar registers a scalar watch (specific name).
func (gw *graphWatcher) watchScalar(nodeID string, gvr schema.GroupVersionResource, name, namespace string) {
	gw.coord.addWatch(gw.graph, watchRequest{
		nodeID:    nodeID,
		gvr:       gvr,
		name:      name,
		namespace: namespace,
	})
}

// watchCollection registers a collection watch (label selector).
func (gw *graphWatcher) watchCollection(nodeID string, gvr schema.GroupVersionResource, namespace string, sel labels.Selector) {
	gw.coord.addWatch(gw.graph, watchRequest{
		nodeID:    nodeID,
		gvr:       gvr,
		namespace: namespace,
		selector:  sel,
	})
}

// getResourceVersion returns the resourceVersion from the metadata informer
// cache for a specific object. Returns "" if not found or no watch exists.
func (gw *graphWatcher) getResourceVersion(gvr schema.GroupVersionResource, namespace, name string) string {
	return gw.coord.watches.getResourceVersion(gvr, namespace, name)
}

// drainTriggers atomically drains and returns the set of node IDs that
// triggered this Graph's enqueue. An empty set means the reconcile was
// triggered by a resync, error retry, or manual trigger — the reconciler
// should do a full walk. Triggers deposited after drain belong to the
// next reconcile.
func (gw *graphWatcher) drainTriggers() map[string]bool {
	gw.coord.triggerMu.Lock()
	defer gw.coord.triggerMu.Unlock()
	triggers := gw.coord.pendingTriggers[gw.graph]
	delete(gw.coord.pendingTriggers, gw.graph)
	return triggers
}

// done finalizes the watch set for this reconcile cycle.
func (gw *graphWatcher) done(commit bool) {
	if commit {
		gw.coord.doneGraph(gw.graph)
	} else {
		gw.coord.abortGraph(gw.graph)
	}
}

func (c *WatchCoordinator) addWatch(graph graphKey, req watchRequest) {
	c.mu.Lock()

	state, ok := c.graphs[graph]
	if !ok {
		state = &graphState{
			current:  make(map[string]*watchRequest),
			previous: make(map[string]*watchRequest),
		}
		c.graphs[graph] = state
	}

	// Handle nodeID reuse with different target
	if old, exists := state.current[req.nodeID]; exists {
		if !sameTarget(old, &req) {
			if prev, shared := state.previous[req.nodeID]; !shared || !sameTarget(prev, old) {
				c.removeFromIndexesLocked(graph, old)
			}
		}
	}

	state.current[req.nodeID] = &req

	// Only add to index if not already covered by previous cycle
	if prev, shared := state.previous[req.nodeID]; !shared || !sameTarget(prev, &req) {
		if req.isCollection() {
			c.addCollectionLocked(graph, req)
		} else {
			c.addScalarLocked(graph, req)
		}
	}

	gvr := req.gvr
	c.mu.Unlock()

	// Ensure informer running (outside lock)
	if err := c.watches.ensureWatch(gvr, "coordinator"); err != nil {
		c.log.Error(err, "failed to ensure watch", "gvr", gvr)
	}
}

func (c *WatchCoordinator) doneGraph(graph graphKey) {
	c.mu.Lock()

	state, ok := c.graphs[graph]
	if !ok {
		c.mu.Unlock()
		return
	}

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

	orphaned := c.findOrphanedLocked(affectedGVRs)
	c.mu.Unlock()

	for _, gvr := range orphaned {
		c.watches.releaseWatch(gvr, "coordinator")
	}
}

func (c *WatchCoordinator) abortGraph(graph graphKey) {
	c.mu.Lock()

	state, ok := c.graphs[graph]
	if !ok {
		c.mu.Unlock()
		return
	}

	var affectedGVRs []schema.GroupVersionResource
	for nodeID, req := range state.current {
		if prev, shared := state.previous[nodeID]; shared && sameTarget(prev, req) {
			continue
		}
		c.removeFromIndexesLocked(graph, req)
		affectedGVRs = append(affectedGVRs, req.gvr)
	}
	state.current = make(map[string]*watchRequest)

	orphaned := c.findOrphanedLocked(affectedGVRs)
	c.mu.Unlock()

	for _, gvr := range orphaned {
		c.watches.releaseWatch(gvr, "coordinator")
	}
}

// removeGraph removes all watch state for a deleted Graph.
func (c *WatchCoordinator) removeGraph(graph graphKey) {
	c.mu.Lock()

	state, ok := c.graphs[graph]
	if !ok {
		c.mu.Unlock()
		return
	}

	var affectedGVRs []schema.GroupVersionResource
	for _, req := range state.current {
		c.removeFromIndexesLocked(graph, req)
		affectedGVRs = append(affectedGVRs, req.gvr)
	}
	for _, req := range state.previous {
		c.removeFromIndexesLocked(graph, req)
		affectedGVRs = append(affectedGVRs, req.gvr)
	}
	delete(c.graphs, graph)

	orphaned := c.findOrphanedLocked(affectedGVRs)
	c.mu.Unlock()

	for _, gvr := range orphaned {
		c.watches.releaseWatch(gvr, "coordinator")
	}
}

// routeEvent routes an informer event to all matching Graphs and records
// the triggering node IDs for scoped walks.
func (c *WatchCoordinator) routeEvent(event watchEvent) {
	c.mu.RLock()
	// matched maps graph → set of triggering node IDs
	matched := make(map[graphKey]map[string]bool)

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
		if entry.namespace != "" && event.namespace != entry.namespace {
			continue
		}
		if entry.selector.Matches(labels.Set(event.labels)) {
			if matched[entry.graph] == nil {
				matched[entry.graph] = map[string]bool{}
			}
			matched[entry.graph][entry.nodeID] = true
		} else if len(event.oldLabels) > 0 && entry.selector.Matches(labels.Set(event.oldLabels)) {
			if matched[entry.graph] == nil {
				matched[entry.graph] = map[string]bool{}
			}
			matched[entry.graph][entry.nodeID] = true
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
		c.triggerMu.Unlock()

		for graph := range matched {
			c.enqueue(graph)
		}

		c.log.V(2).Info("routed event", "gvr", event.gvr, "name", event.name, "namespace", event.namespace, "type", event.eventType, "matchCount", len(matched))
	}
}

// --- index helpers ---

func (c *WatchCoordinator) addScalarLocked(graph graphKey, req watchRequest) {
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

func (c *WatchCoordinator) addCollectionLocked(graph graphKey, req watchRequest) {
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

func (c *WatchCoordinator) removeFromIndexesLocked(graph graphKey, req *watchRequest) {
	if req.isCollection() {
		c.removeCollectionLocked(graph, req)
	} else {
		c.removeScalarLocked(graph, req)
	}
}

func (c *WatchCoordinator) removeScalarLocked(graph graphKey, req *watchRequest) {
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

func (c *WatchCoordinator) removeCollectionLocked(graph graphKey, req *watchRequest) {
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

func (c *WatchCoordinator) findOrphanedLocked(gvrs []schema.GroupVersionResource) []schema.GroupVersionResource {
	var orphaned []schema.GroupVersionResource
	for _, gvr := range gvrs {
		if len(c.scalarIndex[gvr]) == 0 && len(c.collectionIndex[gvr]) == 0 {
			orphaned = append(orphaned, gvr)
		}
	}
	return orphaned
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
