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

package dynamiccontroller

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

// WatchManager manages informer lifecycle per GVR using keyed ownership.
// Callers Acquire a GVR with a unique key to start (or reuse) an informer
// and Release it when done. Acquire is idempotent per key — calling it
// twice with the same key is a no-op. The informer stops when all owners
// have Released it. Shutdown() force-stops all informers regardless of
// outstanding owners.
type WatchManager struct {
	mu      sync.Mutex
	watches map[schema.GroupVersionResource]*gvrWatch
	client  metadata.Interface
	resync  time.Duration
	log     logr.Logger

	// onEvent is the single callback invoked for every informer event.
	// Set at construction time; never nil.
	onEvent EventHandler

	// SyncTimeout is the maximum time to wait for cache sync in Acquire.
	// Zero means use the default (30s).
	SyncTimeout time.Duration

	// createInformer builds a SharedIndexInformer for a GVR. Defaults to
	// metadatainformer.NewFilteredMetadataInformer. Override in tests only.
	createInformer func(schema.GroupVersionResource) cache.SharedIndexInformer
}

// gvrWatch wraps a single SharedIndexInformer for one GVR.
// The informer runs until all owners have Released it or Shutdown() is called.
type gvrWatch struct {
	gvr      schema.GroupVersionResource
	informer cache.SharedIndexInformer
	stopCh   chan struct{}
	owners   map[string]struct{} // keyed ownership set
	log      logr.Logger
}

// NewWatchManager creates a new WatchManager. The onEvent callback is invoked
// for every informer event across all GVRs.
func NewWatchManager(client metadata.Interface, resync time.Duration, onEvent EventHandler, log logr.Logger) *WatchManager {
	wm := &WatchManager{
		watches: make(map[schema.GroupVersionResource]*gvrWatch),
		client:  client,
		resync:  resync,
		onEvent: onEvent,
		log:     log.WithName("watch-manager"),
	}
	wm.createInformer = wm.defaultCreateInformer
	return wm
}

// Acquire adds the given key to the ownership set for the GVR and ensures
// the informer is running. Acquire is idempotent: calling it again with the
// same key is a no-op and returns (false, nil). Returns (true, nil) when the
// key was newly added. The informer stops only when all owners Release it.
func (m *WatchManager) Acquire(gvr schema.GroupVersionResource, key string) (bool, error) {
	m.mu.Lock()

	if w, ok := m.watches[gvr]; ok {
		if _, held := w.owners[key]; held {
			m.mu.Unlock()
			return false, nil
		}
		w.owners[key] = struct{}{}
		m.mu.Unlock()
		return true, nil
	}

	w := m.newWatch(gvr)
	w.owners[key] = struct{}{}
	m.watches[gvr] = w

	go w.informer.Run(w.stopCh)
	m.log.V(1).Info("Informer started", "gvr", gvr)

	// Release the lock before blocking on cache sync.
	m.mu.Unlock()

	// Wait for initial list/sync with a timeout so callers get a usable cache.
	syncCtx, cancel := context.WithTimeout(context.Background(), m.syncTimeout())
	defer cancel()

	if !cache.WaitForCacheSync(syncCtx.Done(), w.informer.HasSynced) {
		// Remove our key rather than force-removing. A concurrent
		// Acquire may have added another key while we were waiting for
		// sync, so force-removing would kill their watch.
		m.mu.Lock()
		delete(w.owners, key)
		if len(w.owners) == 0 {
			close(w.stopCh)
			delete(m.watches, gvr)
		}
		m.mu.Unlock()
		return false, fmt.Errorf("cache sync timeout for %s", gvr)
	}
	return true, nil
}

// Release removes the given key from the ownership set for the GVR.
// If the key is not present, this is a no-op. When the ownership set
// becomes empty, the informer is stopped and removed.
func (m *WatchManager) Release(gvr schema.GroupVersionResource, key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, ok := m.watches[gvr]
	if !ok {
		return
	}
	delete(w.owners, key)
	if len(w.owners) == 0 {
		close(w.stopCh)
		delete(m.watches, gvr)
		m.log.V(1).Info("Watch stopped", "gvr", gvr)
	}
}

// GetInformer returns the SharedIndexInformer for the given GVR, or nil
// if no watch exists.
func (m *WatchManager) GetInformer(gvr schema.GroupVersionResource) cache.SharedIndexInformer {
	m.mu.Lock()
	defer m.mu.Unlock()
	if w, ok := m.watches[gvr]; ok {
		return w.informer
	}
	return nil
}

// ActiveWatchCount returns the number of active watches.
func (m *WatchManager) ActiveWatchCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.watches)
}

// Shutdown stops all informers and clears state.
func (m *WatchManager) Shutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for gvr, w := range m.watches {
		m.log.V(1).Info("Shutting down watch", "gvr", gvr)
		close(w.stopCh)
	}
	m.watches = make(map[schema.GroupVersionResource]*gvrWatch)
}

const defaultSyncTimeout = 30 * time.Second

func (m *WatchManager) syncTimeout() time.Duration {
	if m.SyncTimeout > 0 {
		return m.SyncTimeout
	}
	return defaultSyncTimeout
}

func (m *WatchManager) defaultCreateInformer(gvr schema.GroupVersionResource) cache.SharedIndexInformer {
	return metadatainformer.NewFilteredMetadataInformer(
		m.client, gvr, metav1.NamespaceAll, m.resync,
		cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
		nil,
	).Informer()
}

func (m *WatchManager) newWatch(gvr schema.GroupVersionResource) *gvrWatch {
	inf := m.createInformer(gvr)

	_ = inf.SetWatchErrorHandler(func(_ *cache.Reflector, err error) {
		m.log.V(1).Error(err, "Watch error", "gvr", gvr)
	})

	w := &gvrWatch{
		gvr:      gvr,
		informer: inf,
		stopCh:   make(chan struct{}),
		owners:   make(map[string]struct{}),
		log:      m.log.WithValues("gvr", gvr.String()),
	}

	// Register a single event handler that converts informer callbacks
	// into normalized Event structs and dispatches via onEvent.
	if _, err := inf.AddEventHandler(w.eventHandlerFuncs(m.onEvent)); err != nil {
		m.log.Error(err, "Failed to add event handler to informer", "gvr", gvr)
	}

	return w
}

// eventHandlerFuncs returns cache.ResourceEventHandlerFuncs that convert
// informer callbacks into normalized Event structs.
func (w *gvrWatch) eventHandlerFuncs(onEvent EventHandler) cache.ResourceEventHandlerFuncs {
	toEvent := func(obj interface{}, eventType EventType) *Event {
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
		AddFunc: func(obj interface{}) {
			if e := toEvent(obj, EventAdd); e != nil {
				onEvent(*e)
			}
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
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
		DeleteFunc: func(obj interface{}) {
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
