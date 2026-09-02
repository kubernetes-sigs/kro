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

package resolver

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/kube-openapi/pkg/validation/spec"
)

// mockResolver counts ResolveSchema invocations and hands back a
// deterministic empty schema. Counts are atomic-safe for the
// concurrent test row.
type pushMockResolver struct {
	calls atomic.Int32
}

func (m *pushMockResolver) ResolveSchema(_ schema.GroupVersionKind) (*spec.Schema, error) {
	m.calls.Add(1)
	return &spec.Schema{SchemaProps: spec.SchemaProps{Type: []string{"object"}}}, nil
}

func (m *pushMockResolver) count() int { return int(m.calls.Load()) }

func gvk(group, version, kind string) schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: group, Version: version, Kind: kind}
}

// TestCachedSchemaResolver_Caching covers the cache-hit / cache-miss
// mechanics in a single table — dedup on concurrent fetch, separate
// GVKs each take a slot, repeated lookups don't pay a delegate call.
// LRU eviction has its own row because the steps run in a specific
// order with a small cache size.
func TestCachedSchemaResolver_Caching(t *testing.T) {
	tests := []struct {
		name      string
		size      int
		fetches   func(t *testing.T, c *CachedSchemaResolver, m *pushMockResolver)
		wantCalls int
	}{
		{
			name: "single-gvk-repeated-fetch-hits-once",
			size: 100,
			fetches: func(t *testing.T, c *CachedSchemaResolver, _ *pushMockResolver) {
				k := gvk("apps", "v1", "Deployment")
				for range 10 {
					_, err := c.ResolveSchema(k)
					require.NoError(t, err)
				}
			},
			wantCalls: 1,
		},
		{
			name: "distinct-gvks-each-fetch-once",
			size: 100,
			fetches: func(t *testing.T, c *CachedSchemaResolver, _ *pushMockResolver) {
				for _, k := range []schema.GroupVersionKind{
					gvk("apps", "v1", "Deployment"),
					gvk("apps", "v1", "StatefulSet"),
					gvk("batch", "v1", "Job"),
				} {
					_, err := c.ResolveSchema(k)
					require.NoError(t, err)
					_, err = c.ResolveSchema(k) // second fetch is a hit
					require.NoError(t, err)
				}
			},
			wantCalls: 3,
		},
		{
			name: "concurrent-fetches-for-same-gvk-collapse-to-one",
			size: 100,
			fetches: func(t *testing.T, c *CachedSchemaResolver, _ *pushMockResolver) {
				k := gvk("apps", "v1", "Deployment")
				var wg sync.WaitGroup
				for range 50 {
					wg.Go(func() {
						_, err := c.ResolveSchema(k)
						assert.NoError(t, err)
					})
				}
				wg.Wait()
			},
			wantCalls: 1,
		},
		{
			name: "lru-evicts-oldest-then-refetches",
			size: 3,
			fetches: func(t *testing.T, c *CachedSchemaResolver, m *pushMockResolver) {
				gvks := []schema.GroupVersionKind{
					gvk("apps", "v1", "Deployment"), // oldest
					gvk("apps", "v1", "StatefulSet"),
					gvk("batch", "v1", "Job"),
					gvk("batch", "v1", "CronJob"), // forces eviction of Deployment
				}
				for _, k := range gvks {
					_, err := c.ResolveSchema(k)
					require.NoError(t, err)
				}
				// Re-fetch the evicted GVK: should hit the delegate again.
				preCount := m.count()
				_, err := c.ResolveSchema(gvks[0])
				require.NoError(t, err)
				assert.Equal(t, preCount+1, m.count(), "evicted GVK should re-call delegate")
				// CronJob is still in the cache.
				preCount = m.count()
				_, err = c.ResolveSchema(gvks[3])
				require.NoError(t, err)
				assert.Equal(t, preCount, m.count(), "cached GVK should not re-call delegate")
			},
			wantCalls: 5, // 4 initial + 1 refetch
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mock := &pushMockResolver{}
			cached, err := NewCachedSchemaResolver(mock, tc.size)
			require.NoError(t, err)
			tc.fetches(t, cached, mock)
			assert.Equal(t, tc.wantCalls, mock.count())
		})
	}
}

// TestCachedSchemaResolver_InvalidateGroupKind pins push-driven
// eviction. The schema watcher calls InvalidateGroupKind when a CRD's
// schema content changes; this test confirms entries for that GK are
// dropped (and entries for other GKs untouched).
func TestCachedSchemaResolver_InvalidateGroupKind(t *testing.T) {
	mock := &pushMockResolver{}
	cached, err := NewCachedSchemaResolver(mock, 100)
	require.NoError(t, err)

	tests := []struct {
		name        string
		seed        []schema.GroupVersionKind
		invalidate  schema.GroupKind
		refetch     schema.GroupVersionKind
		wantNewCall bool
	}{
		{
			name: "matching-gk-evicts-entry",
			seed: []schema.GroupVersionKind{
				gvk("apps", "v1", "Deployment"),
				gvk("apps", "v1beta1", "Deployment"), // same GK, different version
			},
			invalidate:  schema.GroupKind{Group: "apps", Kind: "Deployment"},
			refetch:     gvk("apps", "v1", "Deployment"),
			wantNewCall: true,
		},
		{
			name: "non-matching-gk-leaves-entry",
			seed: []schema.GroupVersionKind{
				gvk("apps", "v1", "Deployment"),
				gvk("batch", "v1", "Job"),
			},
			invalidate:  schema.GroupKind{Group: "batch", Kind: "Job"},
			refetch:     gvk("apps", "v1", "Deployment"),
			wantNewCall: false,
		},
		{
			name:        "invalidate-unknown-gk-is-noop",
			seed:        []schema.GroupVersionKind{gvk("apps", "v1", "Deployment")},
			invalidate:  schema.GroupKind{Group: "fictional", Kind: "Widget"},
			refetch:     gvk("apps", "v1", "Deployment"),
			wantNewCall: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Fresh resolver per row so seed counts don't leak across.
			mock := &pushMockResolver{}
			cached, err := NewCachedSchemaResolver(mock, 100)
			require.NoError(t, err)
			for _, k := range tc.seed {
				_, err := cached.ResolveSchema(k)
				require.NoError(t, err)
			}
			seedCalls := mock.count()
			cached.InvalidateGroupKind(tc.invalidate)
			_, err = cached.ResolveSchema(tc.refetch)
			require.NoError(t, err)
			postCalls := mock.count()
			if tc.wantNewCall {
				assert.Equal(t, seedCalls+1, postCalls, "expected delegate to be called again post-invalidate")
			} else {
				assert.Equal(t, seedCalls, postCalls, "expected cache to still cover refetch")
			}
		})
	}

	_ = cached // keep linter happy if the outer cached is unused
}

type blockableMockResolver struct {
	firstCallStarted  chan struct{}
	secondCallStarted chan struct{}
	unblockFirstCall  chan struct{}
	calls             atomic.Int32
}

func (m *blockableMockResolver) ResolveSchema(_ schema.GroupVersionKind) (*spec.Schema, error) {
	callNum := m.calls.Add(1)
	if callNum == 1 {
		close(m.firstCallStarted)
		<-m.unblockFirstCall
		return &spec.Schema{SchemaProps: spec.SchemaProps{Description: "v1-pre-invalidation"}}, nil
	}
	if callNum == 2 && m.secondCallStarted != nil {
		close(m.secondCallStarted)
	}
	return &spec.Schema{SchemaProps: spec.SchemaProps{Description: "v2-post-invalidation"}}, nil
}

// TestCachedSchemaResolver_InFlightSingleflightRace verifies that a caller
// arriving after InvalidateGroupKind does not join an in-flight singleflight
// leader fetching a pre-invalidation schema.
func TestCachedSchemaResolver_InFlightSingleflightRace(t *testing.T) {
	mock := &blockableMockResolver{
		firstCallStarted:  make(chan struct{}),
		secondCallStarted: make(chan struct{}),
		unblockFirstCall:  make(chan struct{}),
	}
	cached, err := NewCachedSchemaResolver(mock, 100)
	require.NoError(t, err)

	targetGVK := gvk("example.com", "v1", "Widget")

	var (
		sch1, sch2 *spec.Schema
		err1, err2 error
		wg         sync.WaitGroup
	)

	// Caller 1 starts in epoch 0 and blocks in the delegate.
	wg.Go(func() {
		sch1, err1 = cached.ResolveSchema(targetGVK)
	})

	// Wait until caller 1 is actively resolving in the delegate.
	<-mock.firstCallStarted

	// Invalidate the GroupKind while caller 1 is in-flight.
	cached.InvalidateGroupKind(targetGVK.GroupKind())

	// Caller 2 starts after invalidation. It must NOT join caller 1's stale leader.
	caller2Done := make(chan struct{})
	go func() {
		sch2, err2 = cached.ResolveSchema(targetGVK)
		close(caller2Done)
	}()

	// Wait for caller 2 to enter delegate (call 2) or complete.
	select {
	case <-mock.secondCallStarted:
		// Caller 2 bypassed the stale in-flight leader and started call 2.
	case <-caller2Done:
		// Caller 2 completed.
	case <-time.After(100 * time.Millisecond):
		// Caller 2 joined caller 1 and is stuck waiting for unblockFirstCall.
	}

	// Unblock caller 1 so both goroutines can complete.
	close(mock.unblockFirstCall)
	wg.Wait()
	<-caller2Done

	require.NoError(t, err1)
	require.NoError(t, err2)
	assert.Equal(t, "v1-pre-invalidation", sch1.Description, "caller 1 should receive pre-invalidation schema")
	assert.Equal(t, "v2-post-invalidation", sch2.Description, "caller 2 arriving post-invalidation must receive fresh schema")
	assert.Equal(t, int32(2), mock.calls.Load(), "delegate should have been called twice")

	// Subsequent cache lookup should return the post-invalidation schema.
	sch3, err := cached.ResolveSchema(targetGVK)
	require.NoError(t, err)
	assert.Equal(t, "v2-post-invalidation", sch3.Description)
	assert.Equal(t, int32(2), mock.calls.Load(), "cached lookup should not invoke delegate again")
}

func TestCachedSchemaResolver_TTL(t *testing.T) {
	t.Parallel()
	mock := &pushMockResolver{}
	cached, err := NewCachedSchemaResolverWithTTL(mock, 100, 20*time.Millisecond)
	require.NoError(t, err)

	k := gvk("apps", "v1", "Deployment")
	_, err = cached.ResolveSchema(k)
	require.NoError(t, err)
	assert.Equal(t, 1, mock.count())

	// Hit cache before TTL expiration.
	_, err = cached.ResolveSchema(k)
	require.NoError(t, err)
	assert.Equal(t, 1, mock.count())

	// Wait for TTL expiration.
	time.Sleep(30 * time.Millisecond)

	// Fetch after TTL expiration should call delegate again.
	_, err = cached.ResolveSchema(k)
	require.NoError(t, err)
	assert.Equal(t, 2, mock.count())
}

func TestCachedSchemaResolver_Clear(t *testing.T) {
	t.Parallel()
	mock := &pushMockResolver{}
	cached, err := NewCachedSchemaResolver(mock, 100)
	require.NoError(t, err)

	k1 := gvk("apps", "v1", "Deployment")
	k2 := gvk("batch", "v1", "Job")

	_, err = cached.ResolveSchema(k1)
	require.NoError(t, err)
	_, err = cached.ResolveSchema(k2)
	require.NoError(t, err)
	assert.Equal(t, 2, mock.count())

	cached.Clear()

	_, err = cached.ResolveSchema(k1)
	require.NoError(t, err)
	assert.Equal(t, 3, mock.count())

	_, err = cached.ResolveSchema(k2)
	require.NoError(t, err)
	assert.Equal(t, 4, mock.count())
}
