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

// WatchManager manages informer lifecycle per GVR.
// Informers start lazily on first use and stay alive until all owners release
// them or Shutdown() is called.
type WatchManager struct {
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

	// SyncTimeout is the maximum time to wait for cache sync in ensureWatch.
	// Zero means use the default (30s).
	SyncTimeout time.Duration

	// createInformer builds a SharedIndexInformer for a GVR. Defaults to
	// metadatainformer.NewFilteredMetadataInformer. Override in tests only.
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

// NewWatchManager creates a new WatchManager. The onEvent callback is invoked
// for every informer event across all GVRs.
func NewWatchManager(client metadata.Interface, resync time.Duration, onEvent EventHandler, log logr.Logger) *WatchManager {
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

// EnsureWatch ensures a watch on the given GVR registered under a given owner.
// If the informer fails to start, the owner is removed.
func (m *WatchManager) EnsureWatch(gvr schema.GroupVersionResource, ownerID string) error {
	m.mu.Lock()
	if m.owners[gvr] == nil {
		m.owners[gvr] = make(map[string]struct{})
	}
	m.owners[gvr][ownerID] = struct{}{}
	m.mu.Unlock()

	if err := m.ensureWatch(gvr); err != nil {
		m.ReleaseWatch(gvr, ownerID)
		return err
	}
	return nil
}

// ReleaseWatch removes an owner from the GVR. If no owners remain, the
// informer is stopped automatically.
func (m *WatchManager) ReleaseWatch(gvr schema.GroupVersionResource, ownerID string) {
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

// ensureWatch idempotently ensures an informer is running for the given GVR.
// If the informer is already running, this is a no-op. After starting the
// informer it waits for the initial cache sync with a timeout and returns an
// error if the sync does not complete.
func (m *WatchManager) ensureWatch(gvr schema.GroupVersionResource) error {
	m.mu.Lock()

	if _, ok := m.watches[gvr]; ok {
		m.mu.Unlock()
		return nil
	}

	w := m.newWatch(gvr)
	m.watches[gvr] = w

	ctx, cancel := context.WithCancel(context.Background())
	w.cancel = cancel
	go w.informer.RunWithContext(ctx)
	m.log.V(1).Info("Informer started", "gvr", gvr)

	// Release the lock before blocking on cache sync.
	m.mu.Unlock()

	// Wait for initial list/sync with a timeout so callers get a usable cache.
	syncStart := time.Now()
	syncCtx, cancel := context.WithTimeout(context.Background(), m.syncTimeout())
	defer cancel()

	if !cache.WaitForCacheSync(syncCtx.Done(), w.informer.HasSynced) {
		informerSyncDuration.WithLabelValues(gvr.String()).Observe(time.Since(syncStart).Seconds())
		// Stop and remove the broken watch so a future ensureWatch can retry
		// with a fresh informer instead of returning early for a registered
		// but non-functional watch.
		m.forceStopWatch(gvr)
		return fmt.Errorf("cache sync timeout for %s", gvr)
	}
	informerSyncDuration.WithLabelValues(gvr.String()).Observe(time.Since(syncStart).Seconds())
	return nil
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

	for gvr := range m.watches {
		m.stopWatchLocked(gvr, true)
	}
	m.watches = make(map[schema.GroupVersionResource]*gvrWatch)
	m.owners = make(map[schema.GroupVersionResource]map[string]struct{})
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

func (m *WatchManager) forceStopWatch(gvr schema.GroupVersionResource) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopWatchLocked(gvr, true)
}

func (m *WatchManager) stopWatchLocked(gvr schema.GroupVersionResource, force bool) {
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
	m.log.V(1).Info("Watch stopped", "gvr", gvr)
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
