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

package watch

import (
	"context"
	"fmt"
	"maps"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/metadata/metadatainformer"
	"k8s.io/client-go/tools/cache"
)

// MetricsRecorder receives observability signals from a [Manager]. It is
// optional: leave [Manager.Metrics] nil to disable instrumentation. The
// recorder is called while the manager may hold internal locks, so
// implementations must not call back into the manager and must return quickly.
type MetricsRecorder interface {
	// SetActiveWatches reports the current number of running informers.
	SetActiveWatches(active int)
	// ObserveInformerSync records how long EnsureWatch waited for an
	// informer's initial cache sync, whether it succeeded or timed out.
	ObserveInformerSync(gvr schema.GroupVersionResource, seconds float64)
}

// Manager owns the lifecycle of one shared informer per GVR. Informers start
// lazily on first [Manager.EnsureWatch] and run until the last owner releases
// them (or [Manager.Shutdown] is called). Owners are arbitrary string IDs; the
// manager makes no distinction between them, which lets several independent
// consumers share a single informer for the same GVR.
type Manager struct {
	mu      sync.Mutex
	watches map[schema.GroupVersionResource]*gvrWatch
	// owners tracks who retains each GVR. An informer is eligible for
	// shutdown only when its owner set is empty.
	owners map[schema.GroupVersionResource]map[string]struct{}
	client metadata.Interface
	resync time.Duration
	log    logr.Logger

	// onEvent is the single callback invoked for every informer event.
	// Set at construction time; never nil.
	onEvent EventHandler

	// SyncTimeout is the maximum time to wait for cache sync in EnsureWatch.
	// Zero means use the default (30s).
	SyncTimeout time.Duration

	// Metrics receives observability signals. Nil disables instrumentation.
	Metrics MetricsRecorder

	// createInformer builds a SharedIndexInformer for a GVR. Defaults to a
	// metadatainformer-backed factory. Override via SetInformerFactory in
	// tests only.
	createInformer func(schema.GroupVersionResource) cache.SharedIndexInformer
}

// gvrWatch wraps a single SharedIndexInformer for one GVR.
// Once started, the informer runs until all owners release it or Shutdown().
type gvrWatch struct {
	gvr        schema.GroupVersionResource
	informer   cache.SharedIndexInformer
	handlerReg cache.ResourceEventHandlerRegistration
	cancel     context.CancelFunc
	log        logr.Logger
}

// NewManager creates a Manager. The onEvent callback is invoked for every
// informer event across all GVRs. The metadata client backs the informers, so
// only object metadata is retrieved -- memory stays bounded as the set of
// watched GVRs grows.
func NewManager(client metadata.Interface, resync time.Duration, onEvent EventHandler, log logr.Logger) *Manager {
	m := &Manager{
		watches: make(map[schema.GroupVersionResource]*gvrWatch),
		owners:  make(map[schema.GroupVersionResource]map[string]struct{}),
		client:  client,
		resync:  resync,
		onEvent: onEvent,
		log:     log.WithName("watch-manager"),
	}
	m.createInformer = m.defaultCreateInformer
	return m
}

// SetInformerFactory overrides the informer constructor. Intended for tests
// that need to inject a fake SharedIndexInformer.
func (m *Manager) SetInformerFactory(f func(schema.GroupVersionResource) cache.SharedIndexInformer) {
	m.createInformer = f
}

// EnsureWatch retains the informer for gvr under ownerID, starting one if
// none is running yet. It then blocks until the informer reports HasSynced
// (up to SyncTimeout) so callers can rely on a usable cache on return.
// Idempotent for a given ownerID.
//
// On sync timeout only ownerID's retention is dropped -- the informer is never
// force-stopped. If another owner attached while we were waiting, the informer
// keeps running under that owner (and it gets its own chance to wait for
// sync); if we were the sole owner, releasing empties the owner set and the
// informer stops naturally.
func (m *Manager) EnsureWatch(gvr schema.GroupVersionResource, ownerID string) error {
	m.mu.Lock()
	if m.owners[gvr] == nil {
		m.owners[gvr] = make(map[string]struct{})
	}
	m.owners[gvr][ownerID] = struct{}{}

	// Check if watch already exists while still holding the lock.
	w, alreadyExists := m.watches[gvr]
	if !alreadyExists {
		// Create and start the informer while still holding the lock, so no
		// ReleaseWatch can remove our owner before the watch exists.
		w = m.newWatch(gvr)
		m.watches[gvr] = w
		ctx, cancel := context.WithCancel(context.Background())
		w.cancel = cancel
		go w.informer.RunWithContext(ctx)
		m.recordActiveWatchesLocked()
		m.log.V(1).Info("Informer started", "gvr", gvr)
	}

	// Release the lock before blocking on cache sync.
	m.mu.Unlock()

	// Wait for initial list/sync with a timeout so callers get a usable cache.
	syncStart := time.Now()
	syncCtx, syncCancel := context.WithTimeout(context.Background(), m.syncTimeout())
	defer syncCancel()

	if !cache.WaitForCacheSync(syncCtx.Done(), w.informer.HasSynced) {
		m.observeInformerSync(gvr, time.Since(syncStart))
		// Drop our retention only. See the method comment for why we never
		// force-stop here.
		m.ReleaseWatch(gvr, ownerID)
		return fmt.Errorf("cache sync timeout for %s", gvr)
	}
	m.observeInformerSync(gvr, time.Since(syncStart))
	return nil
}

// ReleaseWatch removes an owner from the GVR. If no owners remain, the
// informer is stopped automatically.
func (m *Manager) ReleaseWatch(gvr schema.GroupVersionResource, ownerID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if owners := m.owners[gvr]; owners != nil {
		delete(owners, ownerID)
		if len(owners) == 0 {
			delete(m.owners, gvr)
		}
	}
	m.stopWatchLocked(gvr, false)
}

// GetInformer returns the SharedIndexInformer for the given GVR, or nil
// if no watch exists.
func (m *Manager) GetInformer(gvr schema.GroupVersionResource) cache.SharedIndexInformer {
	m.mu.Lock()
	defer m.mu.Unlock()
	if w, ok := m.watches[gvr]; ok {
		return w.informer
	}
	return nil
}

// ActiveWatchCount returns the number of active watches.
func (m *Manager) ActiveWatchCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.watches)
}

// Shutdown stops all informers and clears state.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for gvr := range m.watches {
		m.stopWatchLocked(gvr, true)
	}
	m.watches = make(map[schema.GroupVersionResource]*gvrWatch)
	m.owners = make(map[schema.GroupVersionResource]map[string]struct{})
}

const defaultSyncTimeout = 30 * time.Second

func (m *Manager) syncTimeout() time.Duration {
	if m.SyncTimeout > 0 {
		return m.SyncTimeout
	}
	return defaultSyncTimeout
}

func (m *Manager) recordActiveWatchesLocked() {
	if m.Metrics != nil {
		m.Metrics.SetActiveWatches(len(m.watches))
	}
}

func (m *Manager) observeInformerSync(gvr schema.GroupVersionResource, d time.Duration) {
	if m.Metrics != nil {
		m.Metrics.ObserveInformerSync(gvr, d.Seconds())
	}
}

func (m *Manager) defaultCreateInformer(gvr schema.GroupVersionResource) cache.SharedIndexInformer {
	return metadatainformer.NewFilteredMetadataInformer(
		m.client, gvr, metav1.NamespaceAll, m.resync,
		cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
		nil,
	).Informer()
}

func (m *Manager) newWatch(gvr schema.GroupVersionResource) *gvrWatch {
	inf := m.createInformer(gvr)

	_ = inf.SetWatchErrorHandler(func(_ *cache.Reflector, err error) {
		m.log.V(1).Error(err, "Watch error", "gvr", gvr)
	})

	w := &gvrWatch{
		gvr:      gvr,
		informer: inf,
		log:      m.log.WithValues("gvr", gvr.String()),
	}

	// Register a single event handler that converts informer callbacks
	// into normalized Event structs and dispatches via onEvent.
	reg, err := inf.AddEventHandler(w.eventHandlerFuncs(m.onEvent))
	if err != nil {
		m.log.Error(err, "Failed to add event handler to informer", "gvr", gvr)
	}
	w.handlerReg = reg

	return w
}

func (m *Manager) stopWatchLocked(gvr schema.GroupVersionResource, force bool) {
	w, ok := m.watches[gvr]
	if !ok {
		return
	}
	if !force {
		if owners := m.owners[gvr]; len(owners) > 0 {
			m.log.V(1).Info("Watch retained by owners", "gvr", gvr, "owners", len(owners))
			return
		}
	}
	// event handler registration can only fail if the handler type is
	// mismatched, which can never happen since we own all handlers
	_ = w.informer.RemoveEventHandler(w.handlerReg)
	w.cancel()
	delete(m.watches, gvr)
	m.recordActiveWatchesLocked()
	m.log.V(1).Info("Watch stopped", "gvr", gvr)
}

// eventHandlerFuncs returns cache.ResourceEventHandlerFuncs that convert
// informer callbacks into normalized Event structs.
func (w *gvrWatch) eventHandlerFuncs(onEvent EventHandler) cache.ResourceEventHandlerFuncs {
	toEvent := func(obj any, eventType EventType) *Event {
		mobj, err := meta.Accessor(obj)
		if err != nil {
			w.log.Error(err, "Failed to get meta for watched object")
			return nil
		}
		return &Event{
			Type:      eventType,
			GVR:       w.gvr,
			Name:      mobj.GetName(),
			Namespace: mobj.GetNamespace(),
			Labels:    maps.Clone(mobj.GetLabels()),
		}
	}

	return cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			if e := toEvent(obj, EventAdd); e != nil {
				onEvent(*e)
			}
		},
		UpdateFunc: func(oldObj, newObj any) {
			e := toEvent(newObj, EventUpdate)
			if e == nil {
				return
			}
			// Capture old labels for collection watches to detect label-loss.
			if oldMeta, err := meta.Accessor(oldObj); err == nil {
				e.OldLabels = maps.Clone(oldMeta.GetLabels())
			}
			onEvent(*e)
		},
		DeleteFunc: func(obj any) {
			// Unwrap tombstones: when the informer's watch expires and
			// re-lists, deleted objects may arrive wrapped in
			// DeletedFinalStateUnknown.
			if d, ok := obj.(cache.DeletedFinalStateUnknown); ok {
				obj = d.Obj
			}
			if e := toEvent(obj, EventDelete); e != nil {
				onEvent(*e)
			}
		},
	}
}
