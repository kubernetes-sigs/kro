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
	"fmt"
	"sync"
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"
	"golang.org/x/sync/singleflight"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/cel/openapi/resolver"
	"k8s.io/kube-openapi/pkg/validation/spec"

	"github.com/kubernetes-sigs/kro/pkg/metrics"
)

// CachedSchemaResolver wraps an underlying schema resolver with a
// bounded LRU cache and push-driven invalidation. Entries live until
// either the LRU evicts them under capacity pressure or
// InvalidateGroupKind is called (typically by the schema watcher when
// a CRD content change is observed), or when their TTL expires.
//
// Entries have a default time-based TTL (DefaultCachedSchemaResolverTTL) as a
// fallback so stale definitions expire even if push invalidation is not wired.
// Push invalidation via InvalidateGroupKind immediately drops matching entries.
//
// Concurrent fetches for the same GVK are deduplicated through
// singleflight so the underlying delegate sees one call per unique
// key, even under high concurrency.
type CachedSchemaResolver struct {
	delegate resolver.SchemaResolver

	cache  *expirable.LRU[schema.GroupVersionKind, *spec.Schema]
	sf     singleflight.Group
	mu     sync.RWMutex
	epochs map[schema.GroupKind]uint64
}

// DefaultCachedSchemaResolverTTL is the default entry lifetime for CachedSchemaResolver.
const DefaultCachedSchemaResolverTTL = 5 * time.Minute

// NewCachedSchemaResolver builds a cached resolver around the given
// delegate with DefaultCachedSchemaResolverTTL and maxSize bounding the LRU.
// A reasonable default for production: ~500 for installations with up to a few hundred CRDs.
func NewCachedSchemaResolver(delegate resolver.SchemaResolver, maxSize int) (*CachedSchemaResolver, error) {
	return NewCachedSchemaResolverWithTTL(delegate, maxSize, DefaultCachedSchemaResolverTTL)
}

// NewCachedSchemaResolverWithTTL builds a cached resolver around the given
// delegate with a custom TTL (0 disables time-based expiration).
func NewCachedSchemaResolverWithTTL(delegate resolver.SchemaResolver, maxSize int, ttl time.Duration) (*CachedSchemaResolver, error) {
	if maxSize <= 0 {
		return nil, fmt.Errorf("must provide a positive size")
	}
	// onEvict fires for both capacity-pressure evictions and TTL expirations,
	// so LRU/TTL-driven drops are counted alongside the explicit removals in
	// InvalidateGroupKind/Clear.
	onEvict := func(_ schema.GroupVersionKind, _ *spec.Schema) {
		metrics.SchemaResolverCacheEvictionsTotal.Inc()
	}
	cache := expirable.NewLRU[schema.GroupVersionKind, *spec.Schema](maxSize, onEvict, ttl)
	return &CachedSchemaResolver{
		delegate: delegate,
		cache:    cache,
		epochs:   make(map[schema.GroupKind]uint64),
	}, nil
}

// Clear drops all cached entries and increments all epochs so in-flight fetches are invalidated.
func (c *CachedSchemaResolver) Clear() {
	c.mu.Lock()
	for gk := range c.epochs {
		c.epochs[gk]++
	}
	c.cache.Purge()
	c.mu.Unlock()
}

// InvalidateGroupKind drops every cached entry whose GroupKind matches
// the supplied value. The schema watcher calls this on CRD content
// changes so subsequent ResolveSchema calls re-fetch the new shape.
// Idempotent: calling for an unknown GK is a no-op.
//
// NOTE on epoch-map growth (finding #6): the epoch is bumped
// unconditionally, which is REQUIRED for correctness — an in-flight
// ResolveSchema leader for a not-yet-cached GK is scoped to the pre-bump
// epoch, and only bumping fences it from repopulating a stale schema after
// this invalidation (see TestCachedSchemaResolver_InFlightSingleflightRace).
// Because singleflight does not expose whether such a leader exists, we
// cannot safely skip the bump for an "uncached" GK, nor delete an entry
// afterward (resetting the counter toward 0 lets a stale leader holding the
// old epoch re-match under the write-lock re-check — the exact TOCTOU the
// epoch guard prevents). A per-GK uint64 counter is O(1) memory; growth is
// bounded by the number of DISTINCT GroupKinds the watcher ever invalidates,
// which is itself bounded by the CRDs installed in the cluster. Deleting
// entries safely would require tracking in-flight singleflight leaders per
// GK, which is a larger design change deferred as a separate decision.
func (c *CachedSchemaResolver) InvalidateGroupKind(gk schema.GroupKind) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.epochs[gk]++
	removed := 0
	for _, k := range c.cache.Keys() {
		if k.GroupKind() != gk {
			continue
		}
		if c.cache.Remove(k) {
			removed++
		}
	}
	if removed > 0 {
		metrics.SchemaResolverCacheSize.Set(float64(c.cache.Len()))
	}
}

// ResolveSchema returns the schema for gvk, hitting the cache when
// possible. Concurrent misses for the same gvk collapse to one
// delegate call via singleflight.
func (c *CachedSchemaResolver) ResolveSchema(gvk schema.GroupVersionKind) (*spec.Schema, error) {
	if sch, ok := c.cache.Get(gvk); ok {
		metrics.SchemaResolverCacheHitsTotal.Inc()
		return sch, nil
	}
	metrics.SchemaResolverCacheMissesTotal.Inc()
	gk := gvk.GroupKind()
	c.mu.RLock()
	epoch := c.epochs[gk]
	c.mu.RUnlock()

	// Scope the singleflight key by epoch so that callers arriving after
	// InvalidateGroupKind do not join an already-in-flight leader computing a
	// stale, pre-invalidation schema.
	key := fmt.Sprintf("%s@%d", gvk.String(), epoch)
	result, err, shared := c.sf.Do(key, func() (any, error) {
		if sch, ok := c.cache.Get(gvk); ok {
			return sch, nil
		}
		start := time.Now()
		sch, err := c.delegate.ResolveSchema(gvk)
		metrics.SchemaResolverAPICallDuration.Observe(time.Since(start).Seconds())
		if err != nil {
			metrics.SchemaResolutionErrorsTotal.Inc()
			return nil, err
		}
		// Re-check the epoch and insert atomically under the write lock.
		// InvalidateGroupKind/Clear both bump the epoch AND Remove/Purge under
		// c.mu.Lock(); holding the same lock across the compare and the Add means
		// an invalidation cannot slip between them and let this (now stale) fetch
		// repopulate the cache. A separate RLock+read then unlock-before-Add would
		// leave exactly that window open.
		c.mu.Lock()
		if c.epochs[gk] == epoch {
			c.cache.Add(gvk, sch)
		}
		metrics.SchemaResolverCacheSize.Set(float64(c.cache.Len()))
		c.mu.Unlock()
		return sch, nil
	})
	if shared {
		// This caller's request was served by another goroutine's in-flight
		// delegate call rather than issuing its own.
		metrics.SchemaResolverSingleflightDeduplicatedTotal.Inc()
	}
	if err != nil {
		return nil, err
	}
	return result.(*spec.Schema), nil
}
