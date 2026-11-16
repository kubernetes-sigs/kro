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
	"errors"
	"fmt"
	"maps"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/tools/cache"
)

// MetricsRecorder receives observability signals from a [Manager]. It is
// optional: leave [Manager.Metrics] nil to disable instrumentation. The
// recorder is called while the manager may hold internal locks, so
// implementations must not call back into the manager and must return quickly.
type MetricsRecorder interface {
	// SetActiveWatches reports the current number of running informers.
	SetActiveWatches(active int)
	// ObserveInformerSync records how long a newly started informer took to
	// complete its initial cache sync. Not recorded for informers stopped
	// before they synced.
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

	// SyncTimeout is the maximum time WaitForSync waits for cache sync.
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
	blockedErr atomic.Pointer[error]
}

// WatchState is a non-blocking snapshot of one GVR's informer.
type WatchState struct {
	Exists     bool
	Synced     bool
	BlockedErr error
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
// none is running yet. It never blocks on the initial list; callers that read
// the store must call WaitForSync first. Idempotent for a given ownerID.
func (m *Manager) EnsureWatch(gvr schema.GroupVersionResource, ownerID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.owners[gvr] == nil {
		m.owners[gvr] = make(map[string]struct{})
	}
	m.owners[gvr][ownerID] = struct{}{}

	if _, alreadyExists := m.watches[gvr]; alreadyExists {
		return
	}
	// Create and start the informer while still holding the lock, so no
	// ReleaseWatch can remove our owner before the watch exists.
	w := m.newWatch(gvr)
	m.watches[gvr] = w
	ctx, cancel := context.WithCancel(context.Background())
	w.cancel = cancel
	go w.informer.RunWithContext(ctx)
	go m.announceSynced(ctx, w)
	m.recordActiveWatchesLocked()
	m.log.V(1).Info("Informer started", "gvr", gvr)
}

// announceSynced emits EventSynced once the informer's initial list completes.
func (m *Manager) announceSynced(ctx context.Context, w *gvrWatch) {
	start := time.Now()
	if !cache.WaitForCacheSync(ctx.Done(), w.informer.HasSynced) {
		return
	}
	m.observeInformerSync(w.gvr, time.Since(start))
	w.log.V(1).Info("Informer synced")
	m.onEvent(Event{Type: EventSynced, GVR: w.gvr})
}

// WatchState reports the informer state for gvr without blocking.
func (m *Manager) WatchState(gvr schema.GroupVersionResource) WatchState {
	return m.WatchStates([]schema.GroupVersionResource{gvr})[gvr]
}

// WatchStates reports the informer state for each gvr under one lock acquisition.
func (m *Manager) WatchStates(gvrs []schema.GroupVersionResource) map[schema.GroupVersionResource]WatchState {
	out := make(map[schema.GroupVersionResource]WatchState, len(gvrs))
	m.mu.Lock()
	for _, gvr := range gvrs {
		w, ok := m.watches[gvr]
		if !ok {
			out[gvr] = WatchState{}
			continue
		}
		st := WatchState{Exists: true, Synced: w.informer.HasSynced()}
		if p := w.blockedErr.Load(); p != nil {
			st.BlockedErr = *p
		}
		out[gvr] = st
	}
	m.mu.Unlock()
	return out
}

// Health is an owner's view of its declared watches: Blocked holds GVRs whose
// informer hit a blocking error, Pending those still completing their initial
// list. Both empty means every watch is delivering events.
type Health struct {
	Blocked map[schema.GroupVersionResource]error
	Pending []schema.GroupVersionResource
}

// Describe renders the health as a stable condition message: sorted GVRs with
// the API status reason for blocked ones. Empty when healthy.
func (h Health) Describe() string {
	if len(h.Blocked) > 0 {
		parts := make([]string, 0, len(h.Blocked))
		for gvr, err := range h.Blocked {
			reason := "error"
			var st apierrors.APIStatus
			if errors.As(err, &st) && st.Status().Reason != "" {
				reason = string(st.Status().Reason)
			}
			parts = append(parts, fmt.Sprintf("%s (%s)", gvr.String(), reason))
		}
		sort.Strings(parts)
		return "changes to these kinds are not detected until the next reconcile: " + strings.Join(parts, ", ")
	}
	if len(h.Pending) > 0 {
		parts := make([]string, 0, len(h.Pending))
		for _, gvr := range h.Pending {
			parts = append(parts, gvr.String())
		}
		sort.Strings(parts)
		return "watches still syncing: " + strings.Join(parts, ", ")
	}
	return ""
}

// isBlockingWatchError reports whether a list/watch error will recur identically
// on retry (RBAC denied, kind not served) rather than clear on its own.
func isBlockingWatchError(err error) bool {
	return apierrors.IsForbidden(err) ||
		apierrors.IsUnauthorized(err) ||
		apierrors.IsNotFound(err) ||
		apierrors.IsMethodNotSupported(err) ||
		apierrors.IsNotAcceptable(err) ||
		apierrors.IsUnsupportedMediaType(err)
}

// WaitForSync blocks until the informer for gvr has synced, its reflector hits a
// blocking error, SyncTimeout elapses, or ctx is done. It does not release.
func (m *Manager) WaitForSync(ctx context.Context, gvr schema.GroupVersionResource) error {
	m.mu.Lock()
	w, ok := m.watches[gvr]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("no watch for %s", gvr)
	}
	syncCtx, cancel := context.WithTimeout(ctx, m.syncTimeout())
	defer cancel()
	ticker := time.NewTicker(syncPollInterval)
	defer ticker.Stop()
	for {
		if w.informer.HasSynced() {
			return nil
		}
		// Fail fast on authorization errors only. A NotFound right after a CRD
		// is Established can be a stale replica; let it ride out the timeout.
		if p := w.blockedErr.Load(); p != nil && !apierrors.IsNotFound(*p) {
			return fmt.Errorf("watch for %s failed: %w", gvr, *p)
		}
		select {
		case <-syncCtx.Done():
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("cache sync timeout for %s", gvr)
		case <-ticker.C:
		}
	}
}

const syncPollInterval = 100 * time.Millisecond

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
	// Same informer metadatainformer.NewFilteredMetadataInformer builds, with
	// Watch wrapped so an accepted watch can clear a recorded blocking error
	// (the reflector has a hook for failures, none for recovery). Only Watch
	// counts: the reflector re-lists before every re-watch, so a List hook
	// would flap blocked/recovered when list is permitted but watch is not.
	res := m.client.Resource(gvr).Namespace(metav1.NamespaceAll)
	lw := &cache.ListWatch{
		ListWithContextFunc: func(ctx context.Context, options metav1.ListOptions) (runtime.Object, error) {
			return res.List(ctx, options)
		},
		WatchFuncWithContext: func(ctx context.Context, options metav1.ListOptions) (watch.Interface, error) {
			wi, err := res.Watch(ctx, options)
			if err == nil {
				m.recordRecovery(gvr)
			}
			return wi, err
		},
	}
	return cache.NewSharedIndexInformer(
		cache.ToListWatcherWithWatchListSemantics(lw, m.client),
		&metav1.PartialObjectMetadata{},
		m.resync,
		cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
	)
}

// recordRecovery clears a recorded blocking error after an accepted Watch and,
// if the informer had already synced, re-emits EventSynced.
func (m *Manager) recordRecovery(gvr schema.GroupVersionResource) {
	m.mu.Lock()
	w, ok := m.watches[gvr]
	m.mu.Unlock()
	if !ok {
		return
	}
	if w.blockedErr.Swap(nil) != nil && w.informer.HasSynced() {
		m.onEvent(Event{Type: EventSynced, GVR: gvr})
	}
}

func (m *Manager) newWatch(gvr schema.GroupVersionResource) *gvrWatch {
	inf := m.createInformer(gvr)

	w := &gvrWatch{
		gvr:      gvr,
		informer: inf,
		log:      m.log.WithValues("gvr", gvr.String()),
	}

	_ = inf.SetWatchErrorHandler(func(_ *cache.Reflector, err error) {
		m.log.V(1).Error(err, "Watch error", "gvr", gvr)
		if !isBlockingWatchError(err) {
			return
		}
		e := err
		if w.blockedErr.Swap(&e) == nil {
			m.onEvent(Event{Type: EventWatchBlocked, GVR: gvr})
		}
	})

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
