// instance.go holds per-Graph mutable reconcile-time state.
//
// In the stateless reconciler model, instanceState is ephemeral —
// created fresh every reconcile from compilation output. No cross-cycle
// state is preserved. The InstanceMap exists solely to support the
// onNewType callback (requeue all graphs when a new CRD is observed).
package graphcontroller

import (
	"sync"

	"github.com/ellistarn/kro/experimental/controller/compiler"
	dagpkg "github.com/ellistarn/kro/experimental/controller/dag"
)

// instanceState holds the compilation artifacts for a single reconcile cycle.
// Created fresh every compileRevision call — no cross-cycle state.
type instanceState struct {
	compilation compiledArtifacts
}

// compiledArtifacts holds the output of a single compilation.
type compiledArtifacts struct {
	compiled *compiler.CompiledGraph
	dag      *dagpkg.DAG
}

// newInstanceState creates a fresh instanceState.
func newInstanceState(compiled *compiler.CompiledGraph, dag *dagpkg.DAG) *instanceState {
	return &instanceState{
		compilation: compiledArtifacts{
			compiled: compiled,
			dag:      dag,
		},
	}
}

// ---------------------------------------------------------------------------
// instanceMap — concurrent-safe wrapper around per-instance state.
// Exists to support the onNewType callback (requeue all cached graphs).
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
