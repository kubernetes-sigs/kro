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
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/cache"
)

// fakeInformer satisfies cache.SharedIndexInformer for Manager tests.
// Only the methods Manager actually exercises are non-trivial; the
// rest return zero values so the type still compiles against the full
// interface. SyncedAt controls how long HasSynced reports false — set to
// 0 for "already synced", anything else delays the sync flag for that
// long after Run starts.
type fakeInformer struct {
	mu       sync.Mutex
	handlers []cache.ResourceEventHandler
	started  bool
	stopped  bool
	stopCh   chan struct{}
	syncedAt time.Time

	// Static flag flipped to true after syncedAt has elapsed. Tests can
	// also force this via setSynced.
	synced bool
}

func newFakeInformer() *fakeInformer { return &fakeInformer{stopCh: make(chan struct{})} }

func (f *fakeInformer) setSynced(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.synced = v
}

func (f *fakeInformer) AddEventHandler(handler cache.ResourceEventHandler) (cache.ResourceEventHandlerRegistration, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers = append(f.handlers, handler)
	return &fakeReg{handler: handler}, nil
}

func (f *fakeInformer) AddEventHandlerWithResyncPeriod(handler cache.ResourceEventHandler, _ time.Duration) (cache.ResourceEventHandlerRegistration, error) {
	return f.AddEventHandler(handler)
}

func (f *fakeInformer) AddEventHandlerWithOptions(handler cache.ResourceEventHandler, _ cache.HandlerOptions) (cache.ResourceEventHandlerRegistration, error) {
	return f.AddEventHandler(handler)
}

func (f *fakeInformer) RemoveEventHandler(_ cache.ResourceEventHandlerRegistration) error {
	return nil
}

func (f *fakeInformer) GetStore() cache.Store           { return nil }
func (f *fakeInformer) GetController() cache.Controller { return nil }

func (f *fakeInformer) Run(stopCh <-chan struct{}) {
	f.mu.Lock()
	f.started = true
	syncedAt := f.syncedAt
	f.mu.Unlock()

	if !syncedAt.IsZero() {
		time.Sleep(time.Until(syncedAt))
	}
	f.mu.Lock()
	f.synced = true
	f.mu.Unlock()

	<-stopCh
	f.mu.Lock()
	f.stopped = true
	close(f.stopCh)
	f.mu.Unlock()
}

func (f *fakeInformer) RunWithContext(ctx context.Context) { f.Run(ctx.Done()) }

func (f *fakeInformer) HasSynced() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.synced
}

func (f *fakeInformer) LastSyncResourceVersion() string                      { return "" }
func (f *fakeInformer) SetWatchErrorHandler(_ cache.WatchErrorHandler) error { return nil }
func (f *fakeInformer) SetWatchErrorHandlerWithContext(_ cache.WatchErrorHandlerWithContext) error {
	return nil
}
func (f *fakeInformer) SetTransform(_ cache.TransformFunc) error { return nil }
func (f *fakeInformer) IsStopped() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stopped
}
func (f *fakeInformer) AddIndexers(_ cache.Indexers) error { return nil }
func (f *fakeInformer) GetIndexer() cache.Indexer          { return nil }

// fire delivers an event synchronously through every registered handler.
func (f *fakeInformer) fireAdd(obj any) {
	f.mu.Lock()
	hs := append([]cache.ResourceEventHandler(nil), f.handlers...)
	f.mu.Unlock()
	for _, h := range hs {
		h.OnAdd(obj, false)
	}
}

func (f *fakeInformer) fireUpdate(oldObj, newObj any) {
	f.mu.Lock()
	hs := append([]cache.ResourceEventHandler(nil), f.handlers...)
	f.mu.Unlock()
	for _, h := range hs {
		h.OnUpdate(oldObj, newObj)
	}
}

func (f *fakeInformer) fireDelete(obj any) {
	f.mu.Lock()
	hs := append([]cache.ResourceEventHandler(nil), f.handlers...)
	f.mu.Unlock()
	for _, h := range hs {
		h.OnDelete(obj)
	}
}

type fakeReg struct{ handler cache.ResourceEventHandler }

func (f *fakeReg) HasSynced() bool { return true }

// fakeObj is the minimum metadata-bearing object the Manager event
// path needs. meta.Accessor accepts anything implementing
// metav1.ObjectMetaAccessor or metav1.Object — we just embed ObjectMeta.
type fakeObj struct {
	metav1.ObjectMeta
}

func newFakeObj(namespace, name string, labels map[string]string) *fakeObj {
	return &fakeObj{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    labels,
		},
	}
}

// GetObjectKind / DeepCopyObject — meta.Accessor doesn't require them,
// but a few code paths brush against runtime.Object. Keep these tiny.
func (f *fakeObj) GetObjectKind() schema.ObjectKind { return schema.EmptyObjectKind }
