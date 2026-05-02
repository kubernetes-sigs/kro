// instance.go holds per-Graph mutable reconcile-time state.
//
// Each Graph CR gets its own instanceState even when sharing a
// compiledGraph with other instances. This is correct because the mutable
// state tracks per-instance Kubernetes resources with different observed
// states.
package graphcontroller

import (
	"sync"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ellistarn/kro/experimental/controller/compiler"
	dagpkg "github.com/ellistarn/kro/experimental/controller/dag"
	graphpkg "github.com/ellistarn/kro/experimental/controller/graph"
)

// instanceState holds the mutable reconcile-time state for a single Graph
// instance. Each Graph CR gets its own instanceState even when sharing a
// compiledGraph with other instances.
type instanceState struct {
	// --- Compilation artifacts ---
	compiled *compiler.CompiledGraph
	spec     *graphpkg.GraphSpec
	dag      *dagpkg.DAG

	// --- Walk carry-forward (preserved across reconciles for propagateWhen,
	// dependency gating, and forEach state) ---
	previousScope      map[string]any
	previousKeys       map[string][]Applied
	previousPlanStates *dagpkg.PlanState
	forEach            *forEachCarryForward // nil until first forEach evaluation

	// --- Prune and finalization ---
	previousAppliedKeys map[string]Applied
	deferredPruneKeys   []Applied
	activeFinalization  map[string]*finalizationEntry

	// --- Deferred typing (dynamic GVK resolution) ---
	resolvedDynamicGVKs map[string]schema.GroupVersionKind
}

// forEachCarryForward holds forEach collection state retained across reconciles.
type forEachCarryForward struct {
	items     map[string][]any                // nodeID/varName → collection items
	itemScope map[string]map[string]any       // nodeID → itemID → scope data
	itemKeys  map[string]map[string][]Applied // nodeID → itemID → applied keys
}

// newInstanceState creates a fresh instanceState for a compiledGraph.
func newInstanceState(compiled *compiler.CompiledGraph) *instanceState {
	return &instanceState{
		compiled: compiled,
		previousScope: make(map[string]any),
		previousKeys:  make(map[string][]Applied),
		forEach: &forEachCarryForward{
			items:     make(map[string][]any),
			itemScope: make(map[string]map[string]any),
			itemKeys:  make(map[string]map[string][]Applied),
		},
	}
}

// updateAppliedKeys stores the current key set as the comparison baseline.
// Call this after prune completes successfully.
func (s *instanceState) updateAppliedKeys(keys []Applied) {
	s.previousAppliedKeys = make(map[string]Applied, len(keys))
	for _, a := range keys {
		s.previousAppliedKeys[a.Key] = a
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
// Used by the SetOnNewType callback to requeue all cached graphs
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
