// typecache.go tracks the global schema epoch for compilation staleness.
// Per 004-compilation.md § Type Cache: "The cache tracks a single global
// generation counter." Any schema change (CRD installed, updated, removed)
// advances it. Staleness is one integer comparison: current generation
// exceeds the artifact's recorded generation.
package compiler

import "sync/atomic"

// SchemaGeneration tracks the global schema epoch for compilation staleness.
// Per 004-compilation.md § Type Cache: "The cache tracks a single global
// generation counter." Any schema change (CRD installed, updated, removed)
// advances it. Staleness is one integer comparison: current generation
// exceeds the artifact's recorded generation.
//
// Thread-safe: the generation counter is atomic. No locking required for the
// hot-path staleness check (one Load per reconcile per graph).
type SchemaGeneration struct {
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
func (sg *SchemaGeneration) AdvanceGeneration() {
	sg.generation.Add(1)
}
