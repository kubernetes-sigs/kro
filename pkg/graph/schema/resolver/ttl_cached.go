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
	"time"

	lru "github.com/hashicorp/golang-lru/v2/expirable"
	"golang.org/x/sync/singleflight"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/cel/openapi/resolver"
	"k8s.io/kube-openapi/pkg/validation/spec"
)

// TTLCachedSchemaResolver caches schemas with LRU eviction and time-based expiration
type TTLCachedSchemaResolver struct {
	// delegate is the underlying resolver to fetch schemas from when not in cache
	delegate resolver.SchemaResolver

	// cache with LRU + TTL
	cache *lru.LRU[schema.GroupVersionKind, *spec.Schema]

	// Deduplicate concurrent requests for the same GVK
	sf singleflight.Group
}

// NewTTLCachedSchemaResolver creates a new TTLCachedSchemaResolver with LRU+TTL caching
func NewTTLCachedSchemaResolver(
	delegate resolver.SchemaResolver,
	maxSize int,
	ttl time.Duration,
) *TTLCachedSchemaResolver {
	// Create LRU cache with TTL and eviction callback for metrics
	cache := lru.NewLRU(
		maxSize,
		func(key schema.GroupVersionKind, value *spec.Schema) {
			cacheEvictionsTotal.Inc()
		},
		ttl,
	)

	return &TTLCachedSchemaResolver{
		delegate: delegate,
		cache:    cache,
	}
}

// ResolveSchema resolves the schema for the given GroupVersionKind, using the cache if possible.
// If multiple concurrent requests for the same GVK are made, they will be deduplicated.
func (c *TTLCachedSchemaResolver) ResolveSchema(gvk schema.GroupVersionKind) (*spec.Schema, error) {
	// Check cache first
	if schema, ok := c.cache.Get(gvk); ok {
		cacheHitsTotal.Inc()
		return schema, nil
	}

	cacheMissesTotal.Inc()

	// Use singleflight to ensure only one API call per GVK
	key := gvk.String()
	result, err, shared := c.sf.Do(key, func() (interface{}, error) {
		// Double-check cache inside singleflight (another goroutine might have populated it)
		if schema, ok := c.cache.Get(gvk); ok {
			return schema, nil
		}

		// Actually fetch from delegate
		start := time.Now()
		schema, err := c.delegate.ResolveSchema(gvk)
		apiCallDuration.Observe(time.Since(start).Seconds())
		if err != nil {
			schemaResolutionErrorsTotal.Inc()
			return nil, err
		}

		// Store in cache (LRU handles eviction automatically)
		c.cache.Add(gvk, schema)
		cacheSize.Set(float64(c.cache.Len()))

		return schema, nil
	})

	if shared {
		singleflightDeduplicatedTotal.Inc()
	}

	if err != nil {
		return nil, err
	}

	return result.(*spec.Schema), nil
}
