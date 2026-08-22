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
	cache := expirable.NewLRU[schema.GroupVersionKind, *spec.Schema](maxSize, nil, ttl)
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
func (c *CachedSchemaResolver) InvalidateGroupKind(gk schema.GroupKind) {
	c.mu.Lock()
	c.epochs[gk]++
	for _, k := range c.cache.Keys() {
		if k.GroupKind() == gk {
			c.cache.Remove(k)
		}
	}
	c.mu.Unlock()
}

// ResolveSchema returns the schema for gvk, hitting the cache when
// possible. Concurrent misses for the same gvk collapse to one
// delegate call via singleflight.
func (c *CachedSchemaResolver) ResolveSchema(gvk schema.GroupVersionKind) (*spec.Schema, error) {
	if sch, ok := c.cache.Get(gvk); ok {
		return sch, nil
	}
	gk := gvk.GroupKind()
	c.mu.RLock()
	epoch := c.epochs[gk]
	c.mu.RUnlock()

	// Scope the singleflight key by epoch so that callers arriving after
	// InvalidateGroupKind do not join an already-in-flight leader computing a
	// stale, pre-invalidation schema.
	key := fmt.Sprintf("%s@%d", gvk.String(), epoch)
	result, err, _ := c.sf.Do(key, func() (any, error) {
		if sch, ok := c.cache.Get(gvk); ok {
			return sch, nil
		}
		sch, err := c.delegate.ResolveSchema(gvk)
		if err != nil {
			return nil, err
		}
		c.mu.RLock()
		curEpoch := c.epochs[gk]
		c.mu.RUnlock()
		if curEpoch == epoch {
			c.cache.Add(gvk, sch)
		}
		return sch, nil
	})
	if err != nil {
		return nil, err
	}
	return result.(*spec.Schema), nil
}
