// graphcache.go manages the per-instance state cache for compiled graphs.
//
// Each Graph CR gets its own mutable instanceState keyed by
// namespace/revision-name.
package graphcontroller

import (
	"sync"
)

// graphCaches is a concurrent-safe wrapper around per-instance state.
// Keyed by namespace/revision-name — each Graph CR gets its own mutable
// state.
type graphCaches struct {
	mu        sync.RWMutex
	instances map[string]*instanceState
}

func newGraphCaches() *graphCaches {
	return &graphCaches{
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
}

func (gc *graphCaches) remove(key string) {
	gc.mu.Lock()
	defer gc.mu.Unlock()
	delete(gc.instances, key)
}

// CacheSizes returns the number of cached instance states.
func (gc *graphCaches) CacheSizes() int {
	gc.mu.RLock()
	defer gc.mu.RUnlock()
	return len(gc.instances)
}

// instanceKeys returns a snapshot of all instance cache keys.
// Used by the SetOnNewType callback to requeue all cached graphs
// when a new type becomes watchable.
func (gc *graphCaches) instanceKeys() []string {
	gc.mu.RLock()
	defer gc.mu.RUnlock()
	keys := make([]string, 0, len(gc.instances))
	for k := range gc.instances {
		keys = append(keys, k)
	}
	return keys
}
