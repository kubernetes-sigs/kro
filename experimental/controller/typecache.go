// typecache.go implements the type cache described in 004-compilation.md § Type Cache.
//
// All resolved types live in the type cache. Resource nodes populate it from
// the API server's OpenAPI schemas. The cache tracks a single global generation
// counter. Any schema change (CRD installed, updated, removed) advances it.
// Staleness is one integer comparison: current generation exceeds the artifact's
// recorded generation.
//
// This over-invalidates — a schema change to an unrelated CRD marks all artifacts
// stale — but CRD changes are rare and recompilation is cheap relative to the
// alternative (per-entry versioning with dependency tracking). The hot path pays
// one comparison.
package graphcontroller

import (
	"sync"
	"sync/atomic"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/cel/openapi/resolver"
	"k8s.io/kube-openapi/pkg/validation/spec"
)

// TypeCache provides schema resolution with generation-based staleness tracking.
// Per 004-compilation.md § Type Cache: "All resolved types live in a type cache.
// The cache tracks a single global generation counter."
//
// Thread-safe: concurrent reads (Resolve) via RLock, generation advances via Lock.
// The generation counter is atomic for reads without lock contention on the
// staleness check hot path.
type TypeCache struct {
	resolver resolver.SchemaResolver // underlying API server schema resolver

	mu      sync.RWMutex
	schemas map[schema.GroupVersionKind]*spec.Schema // resolved schemas

	// generation is the global counter. Any schema change advances it.
	// Artifacts record the generation at compile time. Staleness is
	// artifact.generation < cache.Generation().
	generation atomic.Int64
}

// NewTypeCache creates a type cache backed by the given schema resolver.
// The resolver is called on cache misses to populate the cache from the
// API server's OpenAPI endpoint.
func NewTypeCache(r resolver.SchemaResolver) *TypeCache {
	return &TypeCache{
		resolver: r,
		schemas:  make(map[schema.GroupVersionKind]*spec.Schema),
	}
}

// Resolve returns the OpenAPI schema for a GVK, resolving from the API server
// on cache miss. Returns nil if the GVK cannot be resolved (CRD not installed,
// aggregated API not registered). Nil is not cached — the next call will retry.
func (tc *TypeCache) Resolve(gvk schema.GroupVersionKind) *spec.Schema {
	// Fast path: check cache.
	tc.mu.RLock()
	if s, ok := tc.schemas[gvk]; ok {
		tc.mu.RUnlock()
		return s
	}
	tc.mu.RUnlock()

	// Slow path: resolve from API server.
	if tc.resolver == nil {
		return nil
	}
	s, err := tc.resolver.ResolveSchema(gvk)
	if err != nil || s == nil {
		return nil
	}

	// Cache the resolved schema.
	tc.mu.Lock()
	tc.schemas[gvk] = s
	tc.mu.Unlock()
	return s
}

// Generation returns the current cache generation. Artifacts compare their
// recorded generation against this to determine staleness.
func (tc *TypeCache) Generation() int64 {
	return tc.generation.Load()
}

// AdvanceGeneration increments the generation counter. Called when a schema
// change is detected (CRD install, update, or removal). All artifacts with
// a recorded generation less than the new value are stale.
//
// Per 004-compilation.md: "Any schema change (CRD installed, updated, removed)
// advances it."
func (tc *TypeCache) AdvanceGeneration() {
	tc.generation.Add(1)
}

// Invalidate removes a specific GVK from the cache (e.g., when a CRD is
// deleted or updated). The next Resolve for this GVK will re-query the
// API server. Does NOT advance generation — call AdvanceGeneration separately
// to mark artifacts stale.
func (tc *TypeCache) Invalidate(gvk schema.GroupVersionKind) {
	tc.mu.Lock()
	delete(tc.schemas, gvk)
	tc.mu.Unlock()
}

// InvalidateAndAdvance removes a GVK from the cache and advances the
// generation counter. This is the common pattern when a CRD changes:
// the cached schema is stale and all artifacts need recompilation.
func (tc *TypeCache) InvalidateAndAdvance(gvk schema.GroupVersionKind) {
	tc.mu.Lock()
	delete(tc.schemas, gvk)
	tc.mu.Unlock()
	tc.generation.Add(1)
}
