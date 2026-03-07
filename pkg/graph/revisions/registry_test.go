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

package revisions

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/kubernetes-sigs/kro/pkg/graph"
)

func TestRegistryCases(t *testing.T) {
	one := &graph.Graph{TopologicalOrder: []string{"one"}}
	two := &graph.Graph{TopologicalOrder: []string{"two"}}
	three := &graph.Graph{TopologicalOrder: []string{"three"}}

	tests := []struct {
		name string
		run  func(*testing.T, *Registry)
	}{
		{
			name: "put and get preserve entry fields",
			run: func(t *testing.T, reg *Registry) {
				reg.Put(Entry{RGDName: "demo-rgd", Revision: 1, SpecHash: "hash-1", State: RevisionStateActive, CompiledGraph: one})

				entry, ok := reg.Get("demo-rgd", 1)
				require.True(t, ok)
				assert.Equal(t, "demo-rgd", entry.RGDName)
				assert.Equal(t, int64(1), entry.Revision)
				assert.Equal(t, "hash-1", entry.SpecHash)
				assert.Equal(t, RevisionStateActive, entry.State)
				assert.Same(t, one, entry.CompiledGraph)
			},
		},
		{
			name: "put overwrites an existing revision",
			run: func(t *testing.T, reg *Registry) {
				reg.Put(Entry{RGDName: "demo-rgd", Revision: 1, SpecHash: "hash-1", State: RevisionStateActive, CompiledGraph: one})
				reg.Put(Entry{RGDName: "demo-rgd", Revision: 1, SpecHash: "hash-2", State: RevisionStateActive, CompiledGraph: two})

				entry, ok := reg.Get("demo-rgd", 1)
				require.True(t, ok)
				assert.Equal(t, "hash-2", entry.SpecHash)
				assert.Same(t, two, entry.CompiledGraph)
			},
		},
		{
			name: "get returns misses for unknown rgd or revision",
			run: func(t *testing.T, reg *Registry) {
				reg.Put(Entry{RGDName: "demo-rgd", Revision: 1, State: RevisionStateActive, CompiledGraph: one})

				_, ok := reg.Get("demo-rgd", 2)
				assert.False(t, ok)
				_, ok = reg.Get("other-rgd", 1)
				assert.False(t, ok)
			},
		},
		{
			name: "hasAll only checks revision membership",
			run: func(t *testing.T, reg *Registry) {
				reg.Put(Entry{RGDName: "demo-rgd", Revision: 1, State: RevisionStateActive, CompiledGraph: one})
				reg.Put(Entry{RGDName: "demo-rgd", Revision: 3, State: RevisionStateActive, CompiledGraph: three})

				assert.True(t, reg.HasAll("demo-rgd", nil))
				assert.True(t, reg.HasAll("demo-rgd", []int64{1, 3}))
				assert.False(t, reg.HasAll("demo-rgd", []int64{1, 2, 3}))
				assert.False(t, reg.HasAll("other-rgd", []int64{1}))
			},
		},
		{
			name: "latest tracks the highest revision and tolerates corrupt markers",
			run: func(t *testing.T, reg *Registry) {
				reg.Put(Entry{RGDName: "demo-rgd", Revision: 1, SpecHash: "hash-1", State: RevisionStateActive, CompiledGraph: one})
				reg.Put(Entry{RGDName: "demo-rgd", Revision: 3, SpecHash: "hash-3", State: RevisionStateActive, CompiledGraph: three})
				reg.Put(Entry{RGDName: "demo-rgd", Revision: 2, SpecHash: "hash-2", State: RevisionStateActive, CompiledGraph: two})

				latest, ok := reg.Latest("demo-rgd")
				require.True(t, ok)
				assert.Equal(t, int64(3), latest.Revision)
				assert.Equal(t, "hash-3", latest.SpecHash)
				assert.Same(t, three, latest.CompiledGraph)

				_, ok = reg.Latest("other-rgd")
				assert.False(t, ok)

				reg.byRGD["corrupt-rgd"] = &rgdBucket{entries: map[int64]Entry{}, latestRevision: 42, hasLatest: true}
				_, ok = reg.Latest("corrupt-rgd")
				assert.False(t, ok)
			},
		},
		{
			name: "delete removes revisions and recomputes the latest marker",
			run: func(t *testing.T, reg *Registry) {
				putEntries(reg,
					Entry{RGDName: "demo-rgd", Revision: 1, State: RevisionStateActive, CompiledGraph: one},
					Entry{RGDName: "demo-rgd", Revision: 2, State: RevisionStateActive, CompiledGraph: two},
					Entry{RGDName: "demo-rgd", Revision: 3, State: RevisionStateActive, CompiledGraph: three},
				)

				reg.Delete("demo-rgd", 2)
				_, ok := reg.Get("demo-rgd", 2)
				assert.False(t, ok)
				assert.Equal(t, int64(3), mustLatest(t, reg, "demo-rgd").Revision)

				reg.Delete("demo-rgd", 3)
				assert.Equal(t, int64(1), mustLatest(t, reg, "demo-rgd").Revision)

				reg.Delete("demo-rgd", 1)
				_, ok = reg.Latest("demo-rgd")
				assert.False(t, ok)
			},
		},
		{
			name: "delete is idempotent for absent buckets and revisions",
			run: func(t *testing.T, reg *Registry) {
				reg.Delete("demo-rgd", 1)
				reg.Put(Entry{RGDName: "demo-rgd", Revision: 1, State: RevisionStateActive, CompiledGraph: one})
				reg.Delete("demo-rgd", 2)

				entry, ok := reg.Get("demo-rgd", 1)
				require.True(t, ok)
				assert.Equal(t, int64(1), entry.Revision)
			},
		},
		{
			name: "deleteBelow prunes old revisions and preserves the retention floor",
			run: func(t *testing.T, reg *Registry) {
				putEntries(reg,
					Entry{RGDName: "demo-rgd", Revision: 1, State: RevisionStateActive, CompiledGraph: one},
					Entry{RGDName: "demo-rgd", Revision: 2, State: RevisionStateActive, CompiledGraph: two},
					Entry{RGDName: "demo-rgd", Revision: 3, State: RevisionStateActive, CompiledGraph: three},
				)

				reg.DeleteBelow("demo-rgd", 3)

				_, ok := reg.Get("demo-rgd", 1)
				assert.False(t, ok)
				_, ok = reg.Get("demo-rgd", 2)
				assert.False(t, ok)
				entry, ok := reg.Get("demo-rgd", 3)
				require.True(t, ok)
				assert.Same(t, three, entry.CompiledGraph)
				assert.Equal(t, int64(3), mustLatest(t, reg, "demo-rgd").Revision)
			},
		},
		{
			name: "deleteBelow removes empty buckets and ignores unknown rgds",
			run: func(t *testing.T, reg *Registry) {
				reg.DeleteBelow("demo-rgd", 3)
				_, ok := reg.Latest("demo-rgd")
				assert.False(t, ok)

				reg.Put(Entry{RGDName: "demo-rgd", Revision: 1, State: RevisionStateActive, CompiledGraph: one})
				reg.DeleteBelow("demo-rgd", 2)
				_, ok = reg.Latest("demo-rgd")
				assert.False(t, ok)
			},
		},
		{
			name: "deleteBelow recomputes latest when the cached marker is stale",
			run: func(t *testing.T, reg *Registry) {
				reg.byRGD["demo-rgd"] = &rgdBucket{
					entries: map[int64]Entry{
						2: {RGDName: "demo-rgd", Revision: 2, State: RevisionStateActive, CompiledGraph: two},
						3: {RGDName: "demo-rgd", Revision: 3, State: RevisionStateActive, CompiledGraph: three},
					},
					latestRevision: 1,
					hasLatest:      true,
				}

				reg.DeleteBelow("demo-rgd", 3)
				assert.Equal(t, int64(3), mustLatest(t, reg, "demo-rgd").Revision)
			},
		},
		{
			name: "resolver scopes lookups to a single rgd",
			run: func(t *testing.T, reg *Registry) {
				putEntries(reg,
					Entry{RGDName: "demo-rgd", Revision: 1, State: RevisionStateActive, CompiledGraph: one},
					Entry{RGDName: "demo-rgd", Revision: 2, State: RevisionStateActive, CompiledGraph: two},
				)

				resolver := reg.ResolverForRGD("demo-rgd")
				latest, ok := resolver.GetLatestRevision()
				require.True(t, ok)
				assert.Equal(t, RevisionStateActive, latest.State)
				assert.Same(t, two, latest.CompiledGraph)

				revOne, ok := resolver.GetGraphRevision(1)
				require.True(t, ok)
				assert.Same(t, one, revOne.CompiledGraph)

				_, ok = resolver.GetGraphRevision(99)
				assert.False(t, ok)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.run(t, NewRegistry())
		})
	}
}

func putEntries(reg *Registry, entries ...Entry) {
	for _, entry := range entries {
		reg.Put(entry)
	}
}

func mustLatest(t *testing.T, reg *Registry, rgdName string) Entry {
	t.Helper()

	latest, ok := reg.Latest(rgdName)
	require.True(t, ok)
	return latest
}
