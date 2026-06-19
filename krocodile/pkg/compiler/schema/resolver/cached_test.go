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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/kube-openapi/pkg/validation/spec"
)

// mockResolver counts ResolveSchema invocations and hands back a
// deterministic empty schema. Counts are atomic-safe for the
// concurrent test row.
type mockResolver struct {
	calls int32
}

func (m *mockResolver) ResolveSchema(_ schema.GroupVersionKind) (*spec.Schema, error) {
	atomic.AddInt32(&m.calls, 1)
	return &spec.Schema{SchemaProps: spec.SchemaProps{Type: []string{"object"}}}, nil
}

func (m *mockResolver) count() int { return int(atomic.LoadInt32(&m.calls)) }

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
		fetches   func(t *testing.T, c *CachedSchemaResolver, m *mockResolver)
		wantCalls int
	}{
		{
			name: "single-gvk-repeated-fetch-hits-once",
			size: 100,
			fetches: func(t *testing.T, c *CachedSchemaResolver, _ *mockResolver) {
				k := gvk("apps", "v1", "Deployment")
				for i := 0; i < 10; i++ {
					_, err := c.ResolveSchema(k)
					require.NoError(t, err)
				}
			},
			wantCalls: 1,
		},
		{
			name: "distinct-gvks-each-fetch-once",
			size: 100,
			fetches: func(t *testing.T, c *CachedSchemaResolver, _ *mockResolver) {
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
			fetches: func(t *testing.T, c *CachedSchemaResolver, _ *mockResolver) {
				k := gvk("apps", "v1", "Deployment")
				var wg sync.WaitGroup
				for i := 0; i < 50; i++ {
					wg.Add(1)
					go func() {
						defer wg.Done()
						_, err := c.ResolveSchema(k)
						assert.NoError(t, err)
					}()
				}
				wg.Wait()
			},
			wantCalls: 1,
		},
		{
			name: "lru-evicts-oldest-then-refetches",
			size: 3,
			fetches: func(t *testing.T, c *CachedSchemaResolver, m *mockResolver) {
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
			mock := &mockResolver{}
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
	mock := &mockResolver{}
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
			mock := &mockResolver{}
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
