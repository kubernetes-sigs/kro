// graphcache.go manages the two-level cache for compiled graphs and per-instance state.
//
// The compiled cache is content-addressed by compilation key: N identical child
// graphs share one compiledGraph instead of each independently compiling
// identical CEL programs and topologies.
//
// The instance cache is keyed by namespace/revision-name: each Graph CR
// gets its own mutable state even when sharing a compiledGraph.
package graphcontroller

import (
	"strings"
	"sync"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ellistarn/kro/experimental/controller/compiler"
	"github.com/ellistarn/kro/experimental/controller/watches"
)

// graphCaches manages two cache layers:
//   - compiled: content-addressed compiledGraph instances shared across all
//     Graph instances with identical structural inputs. Keyed by compilation key.
//   - instances: per-Graph-instance mutable state. Keyed by namespace/revision-name.
//
// This separation means N identical child graphs (common in nested graph
// patterns with forEach) share one compiledGraph instead of each independently
// compiling identical CEL programs and topologies.
//
// When evictUnresolved fires (CRD discovery), affected instanceStates have
// their compiled pointer set to nil rather than being moved to a separate map.
// compileRevision detects this (compiled == nil) and recompiles in-place,
// preserving the per-node mutable state that is valid across type-resolution
// recompilation (hashes, scopes, references, resync timers, applied keys).
type graphCaches struct {
	// RWMutex: get and getCompiled are read-only and take RLock; set and remove
	// mutate both maps and take Lock. The read path (one get per reconcile per
	// graph) is the hot path under multi-worker controllers.
	mu        sync.RWMutex
	compiled  map[string]*compiler.CompiledGraph // compilation key → shared compiled graph
	instances map[string]*instanceState // namespace/revision-name → per-instance state
}

func newGraphCaches() *graphCaches {
	return &graphCaches{
		compiled:  make(map[string]*compiler.CompiledGraph),
		instances: make(map[string]*instanceState),
	}
}

func (gc *graphCaches) get(key string) *instanceState {
	gc.mu.RLock()
	defer gc.mu.RUnlock()
	return gc.instances[key]
}

func (gc *graphCaches) set(key string, s *instanceState) {
	gc.mu.Lock()
	defer gc.mu.Unlock()
	gc.instances[key] = s
	// Ensure the compiledGraph is also tracked.
	if s.compiled != nil {
		gc.compiled[s.compiled.CompilationKey] = s.compiled
	}
}

func (gc *graphCaches) remove(key string) {
	gc.mu.Lock()
	defer gc.mu.Unlock()
	inst := gc.instances[key]
	delete(gc.instances, key)

	// Sweep: if no other instance references this compiledGraph, remove it.
	// O(instances) per removal — acceptable for typical graph counts (<1000).
	// If this becomes hot (e.g., 10K+ forEach items tearing down), replace
	// with a reference count or reverse index from compilationKey → instance keys.
	if inst != nil && inst.compiled != nil {
		hash := inst.compiled.CompilationKey
		referenced := false
		for _, other := range gc.instances {
			if other.compiled != nil && other.compiled.CompilationKey == hash {
				referenced = true
				break
			}
		}
		if !referenced {
			delete(gc.compiled, hash)
		}
	}
}

// getCompiled returns a shared compiledGraph by compilation key, or nil if not cached.
func (gc *graphCaches) getCompiled(compilationKey string) *compiler.CompiledGraph {
	gc.mu.RLock()
	defer gc.mu.RUnlock()
	return gc.compiled[compilationKey]
}

// CacheSizes returns the number of compiled graphs and instance states.
func (gc *graphCaches) CacheSizes() (compiledCount, instanceCount int) {
	gc.mu.RLock()
	defer gc.mu.RUnlock()
	return len(gc.compiled), len(gc.instances)
}

// evictUnresolved nils out compiled pointers for instance states whose
// compiledGraph had the specified GVR as an unresolved GVK, and removes those
// compiledGraphs from the compiled cache. Returns the affected Graph keys so
// callers can enqueue them for recompilation. Called when a new type becomes
// watchable (CRD install, aggregated API registration) — the previously-
// unresolvable schema may now be available.
//
// Filtering by GVR prevents thundering-herd recompilation when one new type
// appears: only Graphs that reference THAT type are recompiled. Graphs still
// waiting on other unresolved GVKs are left alone — their compiled cache stays
// valid until the GVK they actually need resolves.
//
// The instanceState stays in the instances map with compiled == nil. On the
// next reconcile, compileRevision detects this and recompiles in-place,
// preserving per-node mutable state (hashes, scopes, resync timers, applied
// keys) that is valid across type-resolution recompilation.
func (gc *graphCaches) evictUnresolved(gvr schema.GroupVersionResource) []watches.GraphKey {
	gc.mu.Lock()
	defer gc.mu.Unlock()

	// Find compiled graphs whose unresolved GVK set matches this GVR.
	// GVR comparison: group + version must match, resource must match the
	// pluralized kind. Use gvkToGVR for a single source of truth.
	evictHashes := make(map[string]bool)
	for hash, compiled := range gc.compiled {
		for _, unresolved := range compiled.UnresolvedGVKs {
			if gvkToGVR(unresolved) == gvr {
				evictHashes[hash] = true
				delete(gc.compiled, hash)
				break
			}
		}
	}
	if len(evictHashes) == 0 {
		return nil
	}

	// Nil out compiled pointers on affected instance states. The instance
	// stays in the map — compileRevision will recompile in-place.
	var affected []watches.GraphKey
	for key, state := range gc.instances {
		if state.compiled != nil && evictHashes[state.compiled.CompilationKey] {
			state.compiled = nil
			// Instance key is "namespace/revision-name". The Graph name
			// is the revision name minus the "-gNNNNN" suffix.
			if gk, ok := instanceKeyToGraphKey(key); ok {
				affected = append(affected, gk)
			}
		}
	}
	return affected
}

// instanceKeyToGraphKey extracts a watches.GraphKey from an instance cache key.
// Instance keys are "namespace/graphname-gNNNNN". Returns false if the
// key doesn't match the expected format.
func instanceKeyToGraphKey(instanceKey string) (watches.GraphKey, bool) {
	// Split "namespace/revision-name"
	slash := strings.Index(instanceKey, "/")
	if slash < 0 {
		return watches.GraphKey{}, false
	}
	ns := instanceKey[:slash]
	revName := instanceKey[slash+1:]

	// Revision name format: "graphname-gNNNNN" — find the last "-g" followed by digits.
	lastDash := strings.LastIndex(revName, "-g")
	if lastDash < 0 || lastDash+2 >= len(revName) {
		return watches.GraphKey{}, false
	}
	// Verify the suffix after "-g" is all digits.
	suffix := revName[lastDash+2:]
	for _, c := range suffix {
		if c < '0' || c > '9' {
			return watches.GraphKey{}, false
		}
	}
	return watches.GraphKey{Name: revName[:lastDash], Namespace: ns}, true
}
