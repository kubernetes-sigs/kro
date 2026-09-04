// Copyright 2026 The Kubernetes Authors.
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

package registry

import (
	"fmt"
	"sync"

	"k8s.io/apimachinery/pkg/types"

	expv1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/compiler"
)

// CompileFunc is the compilation surface the Registry uses on cache misses.
// Kept as a function value rather than an interface so callers can inject a
// bound method like `compiler.Compile` directly.
type CompileFunc func(*expv1alpha1.Graph) (*compiler.Program, error)

// Registry is an in-memory cache of compiled Programs keyed by Graph
// identity. Entries are invalidated when the Graph's normalized spec hash
// changes; Delete drops the entry entirely. The zero value is unusable —
// always construct via New().
type Registry struct {
	mu      sync.RWMutex
	entries map[types.NamespacedName]entry
	// epochs tracks, per live key, the version of the last mutation, used as a
	// store-back guard for Compile: a concurrent Compile snapshots a key's
	// epoch and commits only if it is unchanged afterwards. Values come from
	// the strictly-monotonic nextEpoch counter (never a per-key reset), so a
	// deleted-and-recreated key gets a fresh, larger epoch — an in-flight
	// Compile from before the delete can never collide. Deleted keys are pruned
	// (see Delete) so the map stays bounded to live keys.
	epochs    map[types.NamespacedName]uint64
	nextEpoch uint64
}

type entry struct {
	hash    string
	program *compiler.Program
}

// New returns an empty Registry ready for concurrent use.
func New() *Registry {
	return &Registry{
		entries: map[types.NamespacedName]entry{},
		epochs:  map[types.NamespacedName]uint64{},
	}
}

// Lookup returns the cached Program for key when the stored hash matches
// the supplied hash. Returns (nil, false) on miss or stale hash.
func (r *Registry) Lookup(key types.NamespacedName, hash string) (*compiler.Program, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.entries[key]
	if !ok || e.hash != hash {
		return nil, false
	}
	return e.program, true
}

// Store records the (hash, program) entry for key, replacing any prior
// entry under the same key.
func (r *Registry) Store(key types.NamespacedName, hash string, program *compiler.Program) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextEpoch++
	r.epochs[key] = r.nextEpoch
	r.entries[key] = entry{hash: hash, program: program}
}

// Delete drops the entry for key. Safe to call when no entry exists.
//
// The epoch is advanced before the entry is removed so a concurrent Compile
// that snapshotted the pre-delete epoch fails its store-back guard and does not
// resurrect a deleted Graph's program; the epoch is then pruned so the map
// stays bounded. Because epochs are strictly monotonic, a later re-created
// Graph gets a fresh, larger epoch, so pruning can never match a stale compile.
func (r *Registry) Delete(key types.NamespacedName) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextEpoch++
	delete(r.entries, key)
	delete(r.epochs, key)
}

// Compile returns the cached Program if g's spec hash matches the cached
// entry under key; otherwise it invokes compile and stores the new pair.
// The bool reports whether the result came from the cache (true = hit).
// A compile error is propagated and does not poison the cache.
func (r *Registry) Compile(
	key types.NamespacedName,
	g *expv1alpha1.Graph,
	compile CompileFunc,
) (*compiler.Program, bool, error) {
	if g == nil {
		return nil, false, fmt.Errorf("graph is required")
	}
	hash, err := HashSpec(g.Spec)
	if err != nil {
		return nil, false, err
	}
	r.mu.RLock()
	// Snapshot the epoch and whether the key currently has an entry; the
	// store-back commits only if BOTH are unchanged. Tracking presence (not
	// just value) is what makes pruning in Delete safe: after a delete the
	// pruned value reads back as zero, but ok != hadEpoch still trips the
	// guard, preventing a stale compile from resurrecting a deleted key.
	epoch, hadEpoch := r.epochs[key]
	if e, ok := r.entries[key]; ok && e.hash == hash {
		r.mu.RUnlock()
		return e.program, true, nil
	}
	r.mu.RUnlock()

	prog, err := compile(g)
	if err != nil {
		return nil, false, err
	}

	r.mu.Lock()
	if cur, ok := r.epochs[key]; ok == hadEpoch && cur == epoch {
		r.nextEpoch++
		r.epochs[key] = r.nextEpoch
		r.entries[key] = entry{hash: hash, program: prog}
	}
	r.mu.Unlock()
	return prog, false, nil
}

// Len returns the number of cached entries. Useful for observability.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.entries)
}
