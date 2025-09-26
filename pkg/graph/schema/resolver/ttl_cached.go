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
	"time"

	"golang.org/x/sync/singleflight"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/cel/openapi/resolver"
	"k8s.io/kube-openapi/pkg/validation/spec"
	"k8s.io/utils/clock"
)

// TTLCachedSchemaResolver caches schemas with time-based expiration and deduplicates concurrent requests
type TTLCachedSchemaResolver struct {
	mu sync.RWMutex

	// maxSize is the maximum number of entries in the cache. 0 means no limit.
	// When the cache exceeds this size, the oldest entry will be removed.
	// This is a simple eviction policy and can be improved with more complex ones
	// like LRU if needed.
	maxSize int
	// ttl is the time-to-live for cached schemas.
	ttl time.Duration

	// delegate is the underlying resolver to fetch schemas from when not in cache. a.k.a the actual
	// culprit that does the API calls to fetch OpenAPI schemas.
	delegate resolver.SchemaResolver

	// cache is the in memory cache of resolved schemas.
	cache map[schema.GroupVersionKind]*cacheEntry

	// Deduplicate concurrent requests for the same GVK - if multiple RGD reconciliations are
	// trying to resolve the same schema at the same time, only one will do the work
	// and the others will wait for the result.
	sf singleflight.Group

	// clock for testing purposes
	clock clock.Clock
}

// cacheEntry represents a cached schema entry with its registration timestamp.
type cacheEntry struct {
	schema    *spec.Schema
	timestamp time.Time
}

// NewTTLCachedSchemaResolver creates a new TTLCachedSchemaResolver with the given delegate resolver,
func NewTTLCachedSchemaResolver(
	delegate resolver.SchemaResolver,
	maxSize int,
	ttl time.Duration,
) *TTLCachedSchemaResolver {
	return &TTLCachedSchemaResolver{
		delegate: delegate,
		cache:    make(map[schema.GroupVersionKind]*cacheEntry),
		maxSize:  maxSize,
		ttl:      ttl,
		clock:    clock.RealClock{},
	}
}

// ResolveSchema resolves the schema for the given GroupVersionKind, using the cache if possible.
// If multiple concurrent requests for the same GVK are made, they will be deduplicated.
func (c *TTLCachedSchemaResolver) ResolveSchema(gvk schema.GroupVersionKind) (*spec.Schema, error) {
	// Check cache first
	c.mu.RLock()
	if entry, ok := c.cache[gvk]; ok {
		if c.clock.Since(entry.timestamp) < c.ttl {
			c.mu.RUnlock()
			cacheHitsTotal.Inc()
			return entry.schema, nil
		}
		// Entry expired, will refetch
	}
	c.mu.RUnlock()

	cacheMissesTotal.Inc()

	// Use singleflight to ensure only one API call per GVK
	key := gvk.String()
	result, err, shared := c.sf.Do(key, func() (interface{}, error) {
		// Double-check cache inside singleflight (another goroutine might have populated it)
		c.mu.RLock()
		if entry, ok := c.cache[gvk]; ok {
			if c.clock.Since(entry.timestamp) < c.ttl {
				c.mu.RUnlock()
				return entry.schema, nil
			}
		}
		c.mu.RUnlock()

		// Actually fetch from delegate
		start := c.clock.Now()
		schemaResolver, err := c.delegate.ResolveSchema(gvk)
		apiCallDuration.Observe(c.clock.Since(start).Seconds())

		if err != nil {
			return nil, err
		}

		// Store in cache
		c.mu.Lock()
		defer c.mu.Unlock()

		// Evict expired entries first
		evicted := 0
		for k, v := range c.cache {
			if c.clock.Since(v.timestamp) >= c.ttl {
				delete(c.cache, k)
				evicted++
			}
		}

		// If still over limit, remove oldest
		if len(c.cache) >= c.maxSize && c.maxSize > 0 {
			var oldestKey schema.GroupVersionKind
			var oldestTime time.Time
			first := true
			for k, v := range c.cache {
				if first || v.timestamp.Before(oldestTime) {
					oldestKey = k
					oldestTime = v.timestamp
					first = false
				}
			}
			delete(c.cache, oldestKey)
			evicted++
		}

		if evicted > 0 {
			cacheEvictionsTotal.Add(float64(evicted))
		}

		c.cache[gvk] = &cacheEntry{
			schema:    schemaResolver,
			timestamp: c.clock.Now(),
		}

		cacheSize.Set(float64(len(c.cache)))

		return schemaResolver, nil
	})

	if shared {
		singleflightDeduplicatedTotal.Inc()
	}

	if err != nil {
		return nil, err
	}

	return result.(*spec.Schema), nil
}
