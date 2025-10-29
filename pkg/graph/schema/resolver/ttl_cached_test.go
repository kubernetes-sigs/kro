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

package resolver

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/kube-openapi/pkg/validation/spec"
)

type mockResolver struct {
	callCount int32
	schemas   map[schema.GroupVersionKind]*spec.Schema
}

func newMockResolver() *mockResolver {
	return &mockResolver{
		schemas: make(map[schema.GroupVersionKind]*spec.Schema),
	}
}

func (m *mockResolver) ResolveSchema(gvk schema.GroupVersionKind) (*spec.Schema, error) {
	atomic.AddInt32(&m.callCount, 1)

	return &spec.Schema{
		SchemaProps: spec.SchemaProps{
			Type: []string{"object"},
		},
	}, nil
}

func (m *mockResolver) getCallCount() int {
	return int(atomic.LoadInt32(&m.callCount))
}

func TestTTLCachedSchemaResolver_ConcurrentCallCounting(t *testing.T) {
	mock := newMockResolver()
	cachedResolver := NewTTLCachedSchemaResolver(mock, 100, 1*time.Hour)

	gvk := schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := cachedResolver.ResolveSchema(gvk)
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()

	if calls := mock.getCallCount(); calls != 1 {
		t.Errorf("expected 1 call with concurrent access, got %d", calls)
	}
}

func TestTTLCachedSchemaResolver_DifferentGVKCallCounting(t *testing.T) {
	mock := newMockResolver()
	cachedResolver := NewTTLCachedSchemaResolver(mock, 100, 1*time.Hour)

	gvks := []schema.GroupVersionKind{
		{Group: "apps", Version: "v1", Kind: "Deployment"},
		{Group: "apps", Version: "v1", Kind: "StatefulSet"},
		{Group: "batch", Version: "v1", Kind: "Job"},
	}

	for _, gvk := range gvks {
		for i := 0; i < 2; i++ {
			_, err := cachedResolver.ResolveSchema(gvk)
			if err != nil {
				t.Fatalf("unexpected error for %v: %v", gvk, err)
			}
		}
	}

	if calls := mock.getCallCount(); calls != len(gvks) {
		t.Errorf("expected %d calls for %d unique GVKs, got %d", len(gvks), len(gvks), calls)
	}
}

func TestTTLCachedSchemaResolver_TTLExpiry(t *testing.T) {
	mock := newMockResolver()

	ttl := 100 * time.Millisecond
	cachedResolver := NewTTLCachedSchemaResolver(mock, 100, ttl)

	gvk := schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}

	_, err := cachedResolver.ResolveSchema(gvk)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if calls := mock.getCallCount(); calls != 1 {
		t.Errorf("expected 1 initial call, got %d", calls)
	}

	time.Sleep(ttl + 50*time.Millisecond)

	_, err = cachedResolver.ResolveSchema(gvk)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if calls := mock.getCallCount(); calls != 2 {
		t.Errorf("expected 2 calls after TTL expiry, got %d", calls)
	}
}

func TestTTLCachedSchemaResolver_LRUEviction(t *testing.T) {
	mock := newMockResolver()

	cacheSize := 3
	cachedResolver := NewTTLCachedSchemaResolver(mock, cacheSize, 1*time.Hour)

	gvks := []schema.GroupVersionKind{
		{Group: "apps", Version: "v1", Kind: "Deployment"},
		{Group: "apps", Version: "v1", Kind: "StatefulSet"},
		{Group: "batch", Version: "v1", Kind: "Job"},
		{Group: "batch", Version: "v1", Kind: "CronJob"},
	}

	for _, gvk := range gvks {
		_, err := cachedResolver.ResolveSchema(gvk)
		if err != nil {
			t.Fatalf("unexpected error for %v: %v", gvk, err)
		}
	}

	if calls := mock.getCallCount(); calls != len(gvks) {
		t.Errorf("expected %d calls for initial population, got %d", len(gvks), calls)
	}

	atomic.StoreInt32(&mock.callCount, 0)

	_, err := cachedResolver.ResolveSchema(gvks[0])
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if calls := mock.getCallCount(); calls != 1 {
		t.Errorf("expected 1 call after LRU eviction, got %d", calls)
	}

	atomic.StoreInt32(&mock.callCount, 0)
	_, err = cachedResolver.ResolveSchema(gvks[len(gvks)-1])
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if calls := mock.getCallCount(); calls != 0 {
		t.Errorf("expected 0 calls for cache hit, got %d", calls)
	}
}

func TestTTLCachedSchemaResolver_CacheHitAfterInitial(t *testing.T) {
	mock := newMockResolver()
	cachedResolver := NewTTLCachedSchemaResolver(mock, 100, 1*time.Hour)

	gvk := schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}

	_, err := cachedResolver.ResolveSchema(gvk)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if calls := mock.getCallCount(); calls != 1 {
		t.Errorf("expected 1 call for initial fetch, got %d", calls)
	}

	for i := 0; i < 10; i++ {
		_, err := cachedResolver.ResolveSchema(gvk)
		if err != nil {
			t.Fatalf("unexpected error on call %d: %v", i, err)
		}
	}

	if calls := mock.getCallCount(); calls != 1 {
		t.Errorf("expected 1 total call with cache hits, got %d", calls)
	}
}
