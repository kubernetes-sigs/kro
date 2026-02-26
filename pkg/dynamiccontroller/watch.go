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
// Informers start lazily on first use and stay alive until Shutdown().
// This avoids all start/stop races and lock-while-blocking issues.
type WatchManager struct {
	mu      sync.Mutex
	watches map[schema.GroupVersionResource]*gvrWatch
	client  metadata.Interface
	resync  time.Duration
	log     logr.Logger

	// onEvent is the single callback invoked for every informer event.
	// Set at construction time; never nil.
	onEvent EventHandler
}

// gvrWatch wraps a single SharedIndexInformer for one GVR.
// Once started, the informer runs until Shutdown().
type gvrWatch struct {
	gvr      schema.GroupVersionResource
	informer cache.SharedIndexInformer
	stopCh   chan struct{}
	log      logr.Logger
}

// NewWatchManager creates a new WatchManager. The onEvent callback is invoked
// for every informer event across all GVRs.
func NewWatchManager(client metadata.Interface, resync time.Duration, onEvent EventHandler, log logr.Logger) *WatchManager {
	return &WatchManager{
		watches: make(map[schema.GroupVersionResource]*gvrWatch),
		client:  client,
		resync:  resync,
		onEvent: onEvent,
		log:     log.WithName("watch-manager"),
	}
}

// EnsureWatch idempotently ensures an informer is running for the given GVR.
// If the informer is already running, this is a no-op. The informer is started
// in a background goroutine and does not block on cache sync.
func (m *WatchManager) EnsureWatch(gvr schema.GroupVersionResource) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.watches[gvr]; ok {
		return
	}

	w := m.newWatch(gvr)
	m.watches[gvr] = w

	go w.informer.Run(w.stopCh)
	m.log.V(1).Info("Informer started", "gvr", gvr)
}

// StopWatch stops the informer for the given GVR and removes it from the
// watches map. It is non-blocking and idempotent. A subsequent EnsureWatch
// for the same GVR will create a fresh informer.
func (m *WatchManager) StopWatch(gvr schema.GroupVersionResource) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, ok := m.watches[gvr]
	if !ok {
		return
	}
	close(w.stopCh)
	delete(m.watches, gvr)
	m.log.V(1).Info("Watch stopped", "gvr", gvr)
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

// WatchCount returns the number of active watches. Alias for metrics.
func (m *WatchManager) WatchCount() int {
	return m.ActiveWatchCount()
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

func (m *WatchManager) newWatch(gvr schema.GroupVersionResource) *gvrWatch {
	inf := metadatainformer.NewFilteredMetadataInformer(
		m.client, gvr, metav1.NamespaceAll, m.resync,
		cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
		nil,
	).Informer()

	_ = inf.SetWatchErrorHandler(func(_ *cache.Reflector, err error) {
		m.log.V(1).Error(err, "Watch error", "gvr", gvr)
	})

	w := &gvrWatch{
		gvr:      gvr,
		informer: inf,
		stopCh:   make(chan struct{}),
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
		UpdateFunc: func(_, newObj interface{}) {
			if e := toEvent(newObj, EventUpdate); e != nil {
				onEvent(*e)
			}
		},
		DeleteFunc: func(obj interface{}) {
			if e := toEvent(obj, EventDelete); e != nil {
				onEvent(*e)
			}
		},
	}
}

// SetWatchForTesting replaces the internal watch for a GVR. Test only.
func (m *WatchManager) SetWatchForTesting(gvr schema.GroupVersionResource, inf cache.SharedIndexInformer) {
	m.mu.Lock()
	defer m.mu.Unlock()

	w := &gvrWatch{
		gvr:      gvr,
		informer: inf,
		stopCh:   make(chan struct{}),
		log:      m.log.WithValues("gvr", gvr.String()),
	}
	m.watches[gvr] = w
}
