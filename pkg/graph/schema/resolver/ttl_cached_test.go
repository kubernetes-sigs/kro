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
	fake "k8s.io/utils/clock/testing"
)

// mockResolver tracks call counts calls, mainly trying to catch redundant calls
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

	// Return a dummy schema
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

	// launch 50 concurrent reads, only one should win, the rest should wait
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

	// should have only called mock once due to singleflight
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

	// call each GVK twice
	for _, gvk := range gvks {
		for i := 0; i < 2; i++ {
			_, err := cachedResolver.ResolveSchema(gvk)
			if err != nil {
				t.Fatalf("unexpected error for %v: %v", gvk, err)
			}
		}
	}

	// Should have called mock once per unique GVK
	if calls := mock.getCallCount(); calls != len(gvks) {
		t.Errorf("expected %d calls for %d unique GVKs, got %d", len(gvks), len(gvks), calls)
	}
}

func TestTTLCachedSchemaResolver_ExpiryCallCounting(t *testing.T) {
	mock := newMockResolver()
	// Short TTL for testing

	cachedResolver := NewTTLCachedSchemaResolver(mock, 100, 50*time.Millisecond)

	// Use fake clock for time travel
	fakeClock := fake.NewFakeClock(time.Now())
	cachedResolver.clock = fakeClock

	gvk := schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}

	// First call
	_, _ = cachedResolver.ResolveSchema(gvk)

	if calls := mock.getCallCount(); calls != 1 {
		t.Errorf("expected 1 initial call, got %d", calls)
	}

	// Advance clock beyond TTL
	fakeClock.Step(500 * time.Millisecond)

	// Call again after expiry
	_, _ = cachedResolver.ResolveSchema(gvk)

	// Should have called mock twice now
	if calls := mock.getCallCount(); calls != 2 {
		t.Errorf("expected 2 calls after TTL expiry, got %d", calls)
	}
}
