// manager.go implements WatchManager — one metadata-only informer per GVR,
// ref-counted by per-graph owner IDs. Provides the informer lifecycle:
// create, ensure, release, shutdown, and applied-set derivation from cache.
package watches

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/ellistarn/kro/experimental/controller/graph"

	"github.com/go-logr/logr"
	"github.com/gobuffalo/flect"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/metadata/metadatainformer"
	"k8s.io/client-go/tools/cache"
)

// WatchManager maintains one metadata-only informer per GVR, ref-counted by owners.
type WatchManager struct {
	mu        sync.RWMutex
	watches   map[schema.GroupVersionResource]*gvrWatch
	owners    map[schema.GroupVersionResource]map[string]struct{}
	gvrKinds  map[schema.GroupVersionResource]string // GVR → canonical CamelCase Kind (append-only)
	client    metadata.Interface
	resync    time.Duration
	onEvent   func(watchEvent)
	onNewType func(schema.GroupVersionResource) // called when a new type is first watched
	log       logr.Logger

	// parentCtx is the root context for all informer goroutines. Cancelling
	// it stops every informer regardless of ref-count. shutdown() cancels it.
	parentCtx    context.Context
	parentCancel context.CancelFunc

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
	eventType WatchEventType // Observability only — routing is type-agnostic.
	gvr       schema.GroupVersionResource
	name      string
	namespace string
	labels    map[string]string
	oldLabels map[string]string
}

func NewWatchManager(client metadata.Interface, resync time.Duration, onEvent func(watchEvent), log logr.Logger) *WatchManager {
	ctx, cancel := context.WithCancel(context.Background())
	wm := &WatchManager{
		watches:      make(map[schema.GroupVersionResource]*gvrWatch),
		owners:       make(map[schema.GroupVersionResource]map[string]struct{}),
		gvrKinds:     make(map[schema.GroupVersionResource]string),
		client:       client,
		resync:       resync,
		onEvent:      onEvent,
		log:          log.WithName("watch-manager"),
		parentCtx:    ctx,
		parentCancel: cancel,
	}
	wm.createInformer = wm.defaultCreateInformer
	return wm
}

func (m *WatchManager) SetOnEvent(fn func(watchEvent)) {
	m.onEvent = fn
}

func (m *WatchManager) SetOnNewType(fn func(schema.GroupVersionResource)) {
	m.onNewType = fn
}

func (m *WatchManager) EnsureWatch(gvr schema.GroupVersionResource, kind string, ownerID string) error {
	m.mu.Lock()
	if m.owners[gvr] == nil {
		m.owners[gvr] = make(map[string]struct{})
	}
	m.owners[gvr][ownerID] = struct{}{}
	if kind != "" {
		m.gvrKinds[gvr] = kind
	}

	if _, ok := m.watches[gvr]; ok {
		ownerCount := len(m.owners[gvr])
		m.mu.Unlock()
		m.log.V(2).Info("watch already active — reusing informer",
			"gvr", gvr, "owner", ownerID, "ownerCount", ownerCount)
		return nil
	}

	// Create and start
	inf := m.createInformer(gvr)
	ctx, cancel := context.WithCancel(m.parentCtx)
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
	m.log.V(1).Info("informer started", "gvr", gvr, "owner", ownerID)
	m.mu.Unlock()

	// Wait for sync outside the lock. Firing onNewType BEFORE the cache is
	// synced races the recompile: a Graph that evicts and recompiles on the
	// notification may hit the schema resolver before the informer has
	// populated the discovery cache, and compile silently falls back to dyn
	// again. Sync first, notify second — the informer is authoritative by
	// the time the callback runs.
	syncStart := time.Now()
	m.log.V(1).Info("waiting for cache sync", "gvr", gvr)
	syncCtx, syncCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer syncCancel()
	if !cache.WaitForCacheSync(syncCtx.Done(), inf.HasSynced) {
		m.log.Error(nil, "cache sync timeout", "gvr", gvr, "duration", time.Since(syncStart))
		m.forceStop(gvr)
		return fmt.Errorf("cache sync timeout for %s", gvr)
	}
	m.log.V(1).Info("cache synced", "gvr", gvr, "duration", time.Since(syncStart))

	// Notify that a new type is being watched. This triggers recompilation
	// for Graphs that had this type unresolved — the schema may now be available.
	if m.onNewType != nil {
		m.onNewType(gvr)
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
		m.log.V(2).Info("informer retained — other owners remain",
			"gvr", gvr, "ownerCount", len(m.owners[gvr]))
		return
	}
	_ = w.informer.RemoveEventHandler(w.handlerReg) // best-effort; informer is being cancelled anyway
	w.cancel()
	delete(m.watches, gvr)
	m.log.V(1).Info("informer stopped", "gvr", gvr)
}

func (m *WatchManager) Shutdown() {
	m.log.V(1).Info("shutting down watch manager")
	m.parentCancel() // stop all informers via parent context
	m.mu.Lock()
	defer m.mu.Unlock()
	for gvr := range m.watches {
		m.stopLocked(gvr, true)
	}
	m.log.V(1).Info("watch manager shutdown complete")
}

// KindFor returns the canonical CamelCase Kind for a GVR, or "" if unknown.
func (m *WatchManager) KindFor(gvr schema.GroupVersionResource) string {
	m.mu.RLock()
	kind := m.gvrKinds[gvr]
	m.mu.RUnlock()
	return kind
}

// DeriveAppliedSet scans all active informer caches for resources carrying
// the specified graph's identity labels. Returns the applied set — all
// resources this graph has written to the cluster, keyed by resource key.
//
// Per 005-reconciliation.md: "The applied set is derived from the watch
// cache — all resources where the Graph's identity label exists in the
// controller's informer stores. Not persisted."
func (m *WatchManager) DeriveAppliedSet(graphName, namespace string) map[string]graph.AppliedEntry {
	m.mu.RLock()
	// Snapshot watches to avoid holding the lock during cache iteration.
	watches := make(map[schema.GroupVersionResource]*gvrWatch, len(m.watches))
	for gvr, w := range m.watches {
		watches[gvr] = w
	}
	// Snapshot gvrKinds for canonical Kind lookup during iteration.
	gvrKinds := make(map[schema.GroupVersionResource]string, len(m.gvrKinds))
	for gvr, kind := range m.gvrKinds {
		gvrKinds[gvr] = kind
	}
	m.mu.RUnlock()

	suffix := graph.GraphLabelSuffix(graphName, namespace)
	result := make(map[string]graph.AppliedEntry)

	for gvr, w := range watches {
		items := w.informer.GetStore().List()
		for _, item := range items {
			accessor, err := meta.Accessor(item)
			if err != nil {
				continue
			}
			for labelKey, labelValue := range accessor.GetLabels() {
				if !strings.HasSuffix(labelKey, suffix) {
					continue
				}
				// Extract node ID from the label key.
				nodeID, ok := graph.ParseNodeIDFromLabel(labelKey)
				if !ok {
					continue
				}
				// Build resource key from GVR + object metadata.
				// Prefer the gvrKinds cache for canonical CamelCase Kind.
				// The cache is populated from compile-time GVK data, which is
				// always correct. gvrKindFromInformer is the fallback when the
				// cache hasn't been populated for this GVR (e.g., startup race).
				kindForGVR := gvrKinds[gvr]
				if kindForGVR == "" {
					kindForGVR = GVRKindFromInformer(gvr, item)
				}
				gvk := schema.GroupVersionKind{
					Group:   gvr.Group,
					Version: gvr.Version,
					Kind:    kindForGVR,
				}
				nodeType, ok := graph.NodeTypeFromLabelValue(labelValue)
				if !ok {
					m.log.V(1).Info("skipping resource with unrecognized reference label value",
						"resource", accessor.GetNamespace()+"/"+accessor.GetName(),
						"value", labelValue)
					break
				}
				key := fmt.Sprintf("%s/%s/%s/%s/%s",
					gvk.Group, gvk.Version, gvk.Kind,
					accessor.GetNamespace(), accessor.GetName())
				result[key] = graph.AppliedEntry{
					NodeID:   nodeID,
					NodeType: nodeType,
					Key:      key,
				}
				break // one identity label match per resource is sufficient
			}
		}
	}
	return result
}

// GVRKindFromInformer infers the Kind from a GVR and a raw informer item.
// Metadata informers store PartialObjectMetadata which has the Kind set.
func GVRKindFromInformer(gvr schema.GroupVersionResource, item interface{}) string {
	// PartialObjectMetadata carries TypeMeta. Try to read it directly.
	if typed, ok := item.(*metav1.PartialObjectMetadata); ok && typed.Kind != "" {
		return typed.Kind
	}
	// Fallback: best-effort Kind derivation when PartialObjectMetadata.Kind is
	// empty. This path is not expected to fire in normal operation — metadata
	// informers always populate TypeMeta. If it does, the singular form may
	// not match the canonical CamelCase Kind (e.g., "Configmap" vs "ConfigMap").
	// Prefer investigating why TypeMeta is empty over relying on this path.
	singular := flect.Singularize(gvr.Resource)
	if len(singular) > 0 {
		return strings.ToUpper(singular[:1]) + singular[1:]
	}
	return gvr.Resource
}

// GetLabels returns the labels of an object from the metadata informer cache.
// Returns nil, false if the object is not found or no watch exists for the GVR.
func (m *WatchManager) GetLabels(gvr schema.GroupVersionResource, namespace, name string) (map[string]string, bool) {
	m.mu.RLock()
	w, ok := m.watches[gvr]
	m.mu.RUnlock()
	if !ok {
		return nil, false
	}

	key := name
	if namespace != "" {
		key = namespace + "/" + name
	}

	item, exists, err := w.informer.GetStore().GetByKey(key)
	if err != nil || !exists {
		return nil, false
	}

	accessor, err := meta.Accessor(item)
	if err != nil {
		return nil, false
	}
	return maps.Clone(accessor.GetLabels()), true
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
