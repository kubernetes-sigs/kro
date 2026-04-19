// typecache.go implements the schema generation counter described in
// 004-compilation.md § Type Cache.
//
// A single global generation counter tracks schema freshness. Any schema change
// (CRD installed, updated, removed) advances it. Staleness is one integer
// comparison: current generation exceeds the artifact's recorded generation.
//
// This over-invalidates — a schema change to an unrelated CRD marks all
// artifacts stale — but CRD changes are rare and recompilation is cheap
// relative to the alternative (per-entry versioning with dependency tracking).
// The hot path pays one comparison.
//
// Schema resolution itself is handled by the SchemaResolver on the reconciler
// (resolveNodeTypes calls it directly). SchemaGeneration does not cache schemas —
// the underlying CombinedResolver handles that via the discovery client cache.
// The generation counter is the staleness *signal*, not a cache.
package graphcontroller

import "sync/atomic"

// SchemaGeneration tracks the global schema epoch for compilation staleness.
// Per 004-compilation.md § Type Cache: "The cache tracks a single global
// generation counter."
//
// Thread-safe: the generation counter is atomic. No locking required for the
// hot-path staleness check (one Load per reconcile per graph).
type SchemaGeneration struct {
	// generation is the global counter. Any schema change advances it.
	// Artifacts record the generation at compile time. Staleness is
	// artifact.typeCacheGen < schemaGen.Generation().
	generation atomic.Int64
}

// NewSchemaGeneration creates a schema generation tracker.
func NewSchemaGeneration() *SchemaGeneration {
	return &SchemaGeneration{}
}

// Generation returns the current schema generation. Artifacts compare their
// recorded generation against this to determine staleness.
func (sg *SchemaGeneration) Generation() int64 {
	return sg.generation.Load()
}

// AdvanceGeneration increments the generation counter. Called when a schema
// change is detected (CRD install, update, or removal). All artifacts with
// a recorded generation less than the new value are stale.
//
// Per 004-compilation.md: "Any schema change (CRD installed, updated, removed)
// advances it."
func (sg *SchemaGeneration) AdvanceGeneration() {
	sg.generation.Add(1)
}
