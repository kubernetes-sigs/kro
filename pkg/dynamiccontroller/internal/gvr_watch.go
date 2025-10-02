package internal

import (
	"context"
	"fmt"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/metadata/metadatainformer"
	"k8s.io/client-go/tools/cache"
)

// LazyInformer manages a SharedIndexInformer per GVR with multiple handlers.
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
}

func NewLazyInformer(
	ctx context.Context,
	client metadata.Interface,
	gvr schema.GroupVersionResource,
	resync time.Duration,
	tweak metadatainformer.TweakListOptionsFunc,
) *LazyInformer {
	ctx, cancel := context.WithCancel(ctx)
	return &LazyInformer{
		gvr:      gvr,
		client:   client,
		resync:   resync,
		tweak:    tweak,
		handlers: make(map[string]cache.ResourceEventHandlerRegistration),
		done:     ctx.Done(),
		cancel:   cancel,
	}
}

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
		fmt.Printf("Watch error for %s: %v\n", w.gvr, err)
	})
	w.informer = inf
}

// AddHandler registers a new event handler and starts the informer if needed.
func (w *LazyInformer) AddHandler(id string, h cache.ResourceEventHandler) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.ensureInformer()
	reg, err := w.informer.AddEventHandler(h)
	if err != nil {
		return err
	}
	w.handlers[id] = reg

	if len(w.handlers) == 1 { // first handler triggers informer run
		go w.informer.Run(w.done)
		if !cache.WaitForCacheSync(w.done, w.informer.HasSynced) {
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
	delete(w.handlers, id)
	if err := w.informer.RemoveEventHandler(reg); err != nil {
		return false, err
	}

	if len(w.handlers) == 0 {
		w.cancel()
		w.informer = nil
		return true, nil
	}
	return false, nil
}

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
