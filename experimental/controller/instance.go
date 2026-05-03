// instance.go holds per-Graph mutable reconcile-time state.
//
// Each Graph CR gets its own instanceState even when sharing a
// compiledGraph with other instances. This is correct because the mutable
// state tracks per-instance Kubernetes resources with different observed
// states.
//
// Fields are grouped by lifetime into sub-structs so that recompilation
// replaces compilation artifacts atomically rather than resetting
// individual fields.
package graphcontroller

import (
	"sync"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ellistarn/kro/experimental/controller/compiler"
	dagpkg "github.com/ellistarn/kro/experimental/controller/dag"
)

// instanceState holds the mutable reconcile-time state for a single Graph
// instance. Each Graph CR gets its own instanceState even when sharing a
// compiledGraph with other instances.
//
// Fields are separated by lifetime:
//   - compilation: replaced atomically on every compile
//   - walk: carry-forward state preserved across reconciles
//   - prune: prune/finalization state preserved across reconciles
//   - resolvedDynamicGVKs: cross-reconcile hints for deferred typing
type instanceState struct {
	compilation compiledArtifacts
	walk        walkCarryForward
	prune       pruneCarryForward

	// resolvedDynamicGVKs persists across everything including recompile.
	// Used as hints for schema resolution on subsequent compilations.
	resolvedDynamicGVKs map[string]schema.GroupVersionKind
}

// compiledArtifacts holds the output of a single compilation. Replaced
// atomically on every compileRevision call.
type compiledArtifacts struct {
	compiled *compiler.CompiledGraph
	dag      *dagpkg.DAG
}

// walkCarryForward holds state carried forward across reconcile cycles by
// the DAG walk. Includes previous scope for propagateWhen and lazy deps,
// previous keys for carry-forward on blocked/pending nodes, previous plan
// states, and forEach collection state.
type walkCarryForward struct {
	previousScope      map[string]any
	previousKeys       map[string][]Applied
	previousPlanStates *dagpkg.PlanState
	forEach            *forEachCarryForward // nil until first forEach evaluation
}

// resetForRecompile clears walk state that is structurally incompatible
// with a new compilation. previousScope IS cleared because after
// recompilation node IDs may change — stale scope entries from the old
// compilation would cause propagateWhen to evaluate against data from a
// different node topology. The one-cycle transient Pending is correct —
// it's the same as a fresh instance.
//
// previousKeys and previousPlanStates are preserved because they use
// stable resource keys for carry-forward gating; stale entries for
// removed nodes are harmless (never read).
func (w *walkCarryForward) resetForRecompile() {
	w.previousScope = make(map[string]any)
	w.forEach = &forEachCarryForward{
		itemScope: make(map[string]map[string]any),
		itemKeys:  make(map[string]map[string][]Applied),
	}
}

// pruneCarryForward holds state carried forward across reconcile cycles by
// the prune and finalization phases.
type pruneCarryForward struct {
	previousAppliedKeys map[string]Applied
	deferredPruneKeys   []Applied
	activeFinalization  map[string]*finalizationEntry
}

// resetForRecompile clears prune state that should not survive recompilation.
// activeFinalization and previousAppliedKeys are preserved: they track
// resource keys and phases (stable across compilations), not node
// definitions. Orphaned entries for removed nodes are harmless — they're
// never matched as prune candidates.
func (p *pruneCarryForward) resetForRecompile() {
	p.deferredPruneKeys = nil
}

// forEachCarryForward holds forEach collection state retained across reconciles.
type forEachCarryForward struct {
	itemScope map[string]map[string]any       // nodeID → itemID → scope data
	itemKeys  map[string]map[string][]Applied // nodeID → itemID → applied keys
}

// newInstanceState creates a fresh instanceState for a compiledGraph.
func newInstanceState(compiled *compiler.CompiledGraph, dag *dagpkg.DAG) *instanceState {
	return &instanceState{
		compilation: compiledArtifacts{
			compiled: compiled,
			dag:      dag,
		},
		walk: walkCarryForward{
			previousScope: make(map[string]any),
			previousKeys:  make(map[string][]Applied),
			forEach: &forEachCarryForward{
				itemScope: make(map[string]map[string]any),
				itemKeys:  make(map[string]map[string][]Applied),
			},
		},
	}
}

// recompile replaces compilation artifacts and resets state that is
// structurally incompatible with the new compilation.
func (s *instanceState) recompile(compiled *compiler.CompiledGraph, dag *dagpkg.DAG) {
	s.compilation = compiledArtifacts{compiled: compiled, dag: dag}
	s.walk.resetForRecompile()
	s.prune.resetForRecompile()
}

// updateAppliedKeys stores the current key set as the comparison baseline.
// Call this after prune completes successfully.
func (p *pruneCarryForward) updateAppliedKeys(keys []Applied) {
	p.previousAppliedKeys = make(map[string]Applied, len(keys))
	for _, a := range keys {
		p.previousAppliedKeys[a.Key] = a
	}
}

// mergeDynamicGVK records a resolved dynamic GVK for a node and reports
// whether the compilation key has changed (first resolution or GVK change).
// When stale=true, the caller should requeue: the next reconcile will compute
// a new compilation key that includes the resolved GVK, causing a cache miss
// and recompilation with the schema available for type checking.
func (s *instanceState) mergeDynamicGVK(nodeID string, resolved schema.GroupVersionKind) (stale bool) {
	if s.resolvedDynamicGVKs == nil {
		s.resolvedDynamicGVKs = make(map[string]schema.GroupVersionKind)
	}
	prevGVK, hadPrev := s.resolvedDynamicGVKs[nodeID]
	stale = !hadPrev || prevGVK != resolved
	s.resolvedDynamicGVKs[nodeID] = resolved
	return stale
}

// ---------------------------------------------------------------------------
// instanceMap — concurrent-safe wrapper around per-instance state.
// Replaces the former graphCaches type.
// ---------------------------------------------------------------------------

// InstanceMap is a concurrent-safe map of per-instance state keyed by
// namespace/revision-name.
type InstanceMap struct {
	mu        sync.RWMutex
	instances map[string]*instanceState
}

func newInstanceMap() *InstanceMap {
	return &InstanceMap{
		instances: make(map[string]*instanceState),
	}
}

func (m *InstanceMap) get(key string) *instanceState {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.instances[key]
}

func (m *InstanceMap) set(key string, s *instanceState) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.instances[key] = s
}

func (m *InstanceMap) remove(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.instances, key)
}

// CacheSizes returns the number of cached instance states.
func (m *InstanceMap) CacheSizes() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.instances)
}

// instanceKeys returns a snapshot of all instance cache keys.
// Used by the onNewType callback to requeue all cached graphs
// when a new type becomes watchable.
func (m *InstanceMap) instanceKeys() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	keys := make([]string, 0, len(m.instances))
	for k := range m.instances {
		keys = append(keys, k)
	}
	return keys
}
