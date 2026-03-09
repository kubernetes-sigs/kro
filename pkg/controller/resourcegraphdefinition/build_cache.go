// Copyright 2025 The Kubernetes Authors.
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

package resourcegraphdefinition

import (
	"sync"

	"github.com/kubernetes-sigs/kro/pkg/graph"
)

// buildCache caches the last successful graph build for each RGD, keyed by
// name. It is safe for concurrent use.
type buildCache interface {
	// Get returns the cached graph for the given RGD name and generation.
	// Returns nil if there is no entry or the generation does not match.
	Get(name string, generation int64) *graph.Graph
	// Store records a successful build for the given RGD.
	Store(name string, generation int64, g *graph.Graph)
	// Delete removes the cached entry for the given RGD.
	Delete(name string)
}

type buildCacheEntry struct {
	generation int64
	graph      *graph.Graph
}

// syncMapBuildCache implements buildCache using a sync.Map.
type syncMapBuildCache struct {
	m sync.Map // map[string]*buildCacheEntry
}

func newBuildCache() buildCache {
	return &syncMapBuildCache{}
}

func (c *syncMapBuildCache) Get(name string, generation int64) *graph.Graph {
	v, ok := c.m.Load(name)
	if !ok {
		return nil
	}
	entry := v.(*buildCacheEntry)
	if entry.generation != generation {
		return nil
	}
	return entry.graph
}

func (c *syncMapBuildCache) Store(name string, generation int64, g *graph.Graph) {
	c.m.Store(name, &buildCacheEntry{generation: generation, graph: g})
}

func (c *syncMapBuildCache) Delete(name string) {
	c.m.Delete(name)
}

// noopBuildCache is a buildCache that never caches. Used as the zero-value
// fallback so callers don't need nil checks.
type noopBuildCache struct{}

func (noopBuildCache) Get(string, int64) *graph.Graph    { return nil }
func (noopBuildCache) Store(string, int64, *graph.Graph) {}
func (noopBuildCache) Delete(string)                     {}
