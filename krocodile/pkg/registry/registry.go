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
	"sync"

	"k8s.io/apimachinery/pkg/types"

	expv1alpha1 "sigs.k8s.io/krocodile/api/v1alpha1"
	"sigs.k8s.io/krocodile/pkg/compiler"
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
}

type entry struct {
	hash    string
	program *compiler.Program
}

// New returns an empty Registry ready for concurrent use.
func New() *Registry {
	return &Registry{entries: map[types.NamespacedName]entry{}}
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
	r.entries[key] = entry{hash: hash, program: program}
}

// Delete drops the entry for key. Safe to call when no entry exists.
func (r *Registry) Delete(key types.NamespacedName) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.entries, key)
}

// Compile returns the cached Program if g's spec hash matches the cached
// entry under key; otherwise it invokes compile and stores the new pair.
// The bool reports whether the result came from the cache (true = hit).
// A compile error is propagated and does not poison the cache.
func (r *Registry) Compile(key types.NamespacedName, g *expv1alpha1.Graph, compile CompileFunc) (*compiler.Program, bool, error) {
	hash, err := HashSpec(g.Spec)
	if err != nil {
		return nil, false, err
	}
	if prog, hit := r.Lookup(key, hash); hit {
		return prog, true, nil
	}
	prog, err := compile(g)
	if err != nil {
		return nil, false, err
	}
	r.Store(key, hash, prog)
	return prog, false, nil
}

// Len returns the number of cached entries. Useful for observability.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.entries)
}
