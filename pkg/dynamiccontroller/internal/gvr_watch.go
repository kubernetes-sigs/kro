// Copyright 2025 The Kube Resource Orchestrator Authors
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

package internal

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/metadata/metadatainformer"
	"k8s.io/client-go/tools/cache"
)

// LazyInformer manages a SharedIndexInformer per GVR with multiple handlers.
// It lazily starts when the first handler is added and stops when the last is removed.
// It can restart again after a full shutdown.
type LazyInformer struct {
	gvr    schema.GroupVersionResource
	client metadata.Interface
	resync time.Duration
	tweak  metadatainformer.TweakListOptionsFunc

	mu       sync.Mutex
	informer cache.SharedIndexInformer
	handlers map[string]cache.ResourceEventHandlerRegistration

	done   <-chan struct{}
	cancel context.CancelFunc

	log logr.Logger
}

func NewLazyInformer(
	client metadata.Interface,
	gvr schema.GroupVersionResource,
	resync time.Duration,
	tweak metadatainformer.TweakListOptionsFunc,
	logger logr.Logger,
) *LazyInformer {
	li := &LazyInformer{
		gvr:      gvr,
		client:   client,
		resync:   resync,
		tweak:    tweak,
		handlers: make(map[string]cache.ResourceEventHandlerRegistration),
		log:      logger.WithValues("gvr", gvr.String()),
	}
	return li
}

func (w *LazyInformer) resetContext(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	w.done = ctx.Done()
	w.cancel = cancel
}

// ensureInformer initializes informer if missing or after shutdown.
func (w *LazyInformer) ensureInformer() {
	if w.informer != nil {
		return
	}
	inf := metadatainformer.NewFilteredMetadataInformer(
		w.client, w.gvr, metav1.NamespaceAll, w.resync,
		cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
		w.tweak,
	).Informer()

	_ = inf.SetWatchErrorHandler(func(_ *cache.Reflector, err error) {
		w.log.V(1).Error(err, "watch error for lazy informer", "gvr", w.gvr)
	})

	w.informer = inf
}

// AddHandler registers a new event handler and starts the informer if needed.
func (w *LazyInformer) AddHandler(ctx context.Context, id string, h cache.ResourceEventHandler) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	// If informer was fully stopped, reset context before re-creating it.
	if w.cancel == nil {
		// recreate context
		w.resetContext(ctx)
	}

	w.ensureInformer()

	reg, err := w.informer.AddEventHandler(h)
	if err != nil {
		return err
	}
	w.handlers[id] = reg

	// Start informer if first handler
	if len(w.handlers) == 1 {
		go func(done <-chan struct{}, inf cache.SharedIndexInformer) {
			inf.Run(done)
		}(w.done, w.informer)

		if !cache.WaitForCacheSync(w.done, w.informer.HasSynced) {
			w.log.Error(fmt.Errorf("cache sync failed"), "lazy informer sync failure", "gvr", w.gvr)
			w.cancel()
			w.informer = nil
			w.handlers = make(map[string]cache.ResourceEventHandlerRegistration)
			return fmt.Errorf("failed to sync informer for %s", w.gvr)
		}
	}
	return nil
}

// RemoveHandler unregisters a handler. Returns true if informer was stopped.
func (w *LazyInformer) RemoveHandler(id string) (bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	reg, ok := w.handlers[id]
	if !ok {
		return false, nil
	}
	if w.informer != nil {
		if err := w.informer.RemoveEventHandler(reg); err != nil {
			return false, err
		}
		delete(w.handlers, id)
	}

	if len(w.handlers) == 0 {
		w.cancel()
		w.informer = nil
		return true, nil
	}
	return false, nil
}

// Informer returns the underlying SharedIndexInformer (may be nil).
func (w *LazyInformer) Informer() cache.SharedIndexInformer {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.informer
}

// Shutdown stops the informer and clears state.
func (w *LazyInformer) Shutdown() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.cancel()
	w.informer = nil
	w.handlers = make(map[string]cache.ResourceEventHandlerRegistration)
}
