// Copyright 2026 The Kubernetes Authors.
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

package watchrouter

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

// Manager owns the lifecycle of one shared informer per GVR. Informers
// start lazily on first EnsureWatch and run until the last owner releases
// them (or Shutdown is called). Owners are arbitrary string IDs; the watch
// manager makes no distinction between them. The same GVR can be retained
// by many owners — kro's coordinator owns child watches; in the graph engine the
// only owner is the coordinator itself, but the multi-owner shape is kept
// to allow extension.
type Manager struct {
	mu      sync.Mutex
	watches map[schema.GroupVersionResource]*gvrWatch
	owners  map[schema.GroupVersionResource]map[string]struct{}
	client  metadata.Interface
	resync  time.Duration
	log     logr.Logger

	// onEvent is invoked synchronously for every informer event. Set at
	// construction time and never nil.
	onEvent EventHandler

	// SyncTimeout bounds how long EnsureWatch waits for the initial list
	// to populate the cache. Zero falls back to defaultSyncTimeout.
	SyncTimeout time.Duration

	// createInformer is overridable for tests. Production uses
	// defaultCreateInformer (metadatainformer-backed).
	createInformer func(schema.GroupVersionResource) cache.SharedIndexInformer
}

type gvrWatch struct {
	gvr        schema.GroupVersionResource
	informer   cache.SharedIndexInformer
	handlerReg cache.ResourceEventHandlerRegistration
	cancel     context.CancelFunc
	log        logr.Logger
}

// NewManager constructs a manager that fans informer events into
// onEvent. The metadata client is used for the underlying informers — full
// object bodies are not retrieved, which keeps memory bounded as the set
// of watched GVRs grows.
func NewManager(client metadata.Interface, resync time.Duration, onEvent EventHandler, log logr.Logger) *Manager {
	wm := &Manager{
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

// EnsureWatch retains the informer for gvr under ownerID. If no informer
// is running yet, one is started. EnsureWatch then blocks until the
// informer reports HasSynced (up to SyncTimeout) so callers can rely on
// the cache being usable on return. Idempotent for a given ownerID.
//
// Sync is awaited even when the informer was already running, because
// it may still be in its initial list. A timeout drops ownerID's
// retention only — never force-stops the informer — so other owners
// who attached while we were waiting don't get a zombie watch under
// their feet.
func (m *Manager) EnsureWatch(gvr schema.GroupVersionResource, ownerID string) error {
	m.mu.Lock()
	if m.owners[gvr] == nil {
		m.owners[gvr] = make(map[string]struct{})
	}
	m.owners[gvr][ownerID] = struct{}{}

	w, alreadyExists := m.watches[gvr]
	if !alreadyExists {
		w = m.newWatch(gvr)
		m.watches[gvr] = w
		ctx, cancel := context.WithCancel(context.Background())
		w.cancel = cancel
		go w.informer.RunWithContext(ctx)
		m.log.V(1).Info("Informer started", "gvr", gvr)
	}
	m.mu.Unlock()

	syncCtx, syncCancel := context.WithTimeout(context.Background(), m.syncTimeout())
	defer syncCancel()
	if !cache.WaitForCacheSync(syncCtx.Done(), w.informer.HasSynced) {
		// Release our retention only. If we created the informer and
		// no other owner attached during the wait, stopWatchLocked
		// inside ReleaseWatch shuts it down naturally. If another
		// owner did attach, the informer keeps running and that
		// owner gets its own chance to wait for sync.
		m.ReleaseWatch(gvr, ownerID)
		return fmt.Errorf("cache sync timeout for %s", gvr)
	}
	return nil
}

// ReleaseWatch drops ownerID's retention. When the owner set empties the
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

// GetInformer returns the running informer for gvr, or nil if none.
func (m *Manager) GetInformer(gvr schema.GroupVersionResource) cache.SharedIndexInformer {
	m.mu.Lock()
	defer m.mu.Unlock()
	if w, ok := m.watches[gvr]; ok {
		return w.informer
	}
	return nil
}

// ActiveWatchCount returns the number of running informers (for metrics
// and tests).
func (m *Manager) ActiveWatchCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.watches)
}

// Shutdown stops every informer and clears all owner state. Safe to call
// from a manager Stop hook.
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
			return
		}
	}
	_ = w.informer.RemoveEventHandler(w.handlerReg)
	w.cancel()
	delete(m.watches, gvr)
	m.log.V(1).Info("Watch stopped", "gvr", gvr)
}

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
			if oldMeta, err := meta.Accessor(oldObj); err == nil {
				e.OldLabels = maps.Clone(oldMeta.GetLabels())
			}
			onEvent(*e)
		},
		DeleteFunc: func(obj any) {
			if d, ok := obj.(cache.DeletedFinalStateUnknown); ok {
				obj = d.Obj
			}
			if e := toEvent(obj, EventDelete); e != nil {
				onEvent(*e)
			}
		},
	}
}
