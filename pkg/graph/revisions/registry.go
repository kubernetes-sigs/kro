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
	"sync"

	"github.com/kubernetes-sigs/kro/pkg/graph"
)

// RevisionState is the runtime status of a revision in the in-memory cache.
//
// These values are internal scheduling signals between controllers. They are
// intentionally separate from GraphRevision API status, which is conditions-only.
type RevisionState string

const (
	// RevisionStatePending means the GraphRevision object exists but compilation
	// has not completed yet.
	RevisionStatePending RevisionState = "Pending"
	// RevisionStateActive means compilation succeeded and CompiledGraph is ready
	// for direct consumption by reconcilers.
	RevisionStateActive RevisionState = "Active"
	// RevisionStateFailed means compilation failed for this revision.
	RevisionStateFailed RevisionState = "Failed"
)

// Entry is a single cached revision record for an RGD name.
type Entry struct {
	// RGDName is the logical ownership key. Revisions are grouped by name so a
	// re-created RGD can still reuse existing GraphRevisions.
	RGDName string
	// Revision is the monotonic revision number under RGDName.
	Revision int64
	// SpecHash is copied from GraphRevision spec and used by callers for
	// issuance decisions.
	SpecHash string
	// State is the runtime readiness signal for this entry.
	State RevisionState
	// CompiledGraph is populated for active revisions.
	// Registry writes enforce that Active never exists without a graph.
	CompiledGraph *graph.Graph
}

// rgdBucket holds all revisions for a single RGD name.
type rgdBucket struct {
	// entries is keyed by revision number.
	entries map[int64]Entry
	// latestRevision/hasLatest keep Latest() O(1) without scanning entries on
	// read paths.
	latestRevision int64
	hasLatest      bool
}

// Registry is an in-memory cache shared by reconcilers.
//
// The API server remains the source of truth. This cache exists to avoid
// recompiling graphs and to coordinate revision readiness between controllers.
type Registry struct {
	// mu protects byRGD and all nested bucket state.
	mu    sync.RWMutex
	byRGD map[string]*rgdBucket
}

// NewRegistry creates an empty registry for compiled graph revisions.
func NewRegistry() *Registry {
	return &Registry{
		byRGD: make(map[string]*rgdBucket),
	}
}

// Put inserts or replaces a revision entry.
//
// Behavior:
// - idempotent overwrite for the same (RGDName, Revision)
// - advances latest pointer only when a strictly newer revision is written
//
// Contract:
// - Callers always provide explicit State
func (r *Registry) Put(entry Entry) {
	r.mu.Lock()
	defer r.mu.Unlock()

	bucket, ok := r.byRGD[entry.RGDName]
	if !ok {
		bucket = &rgdBucket{
			entries: make(map[int64]Entry),
		}
		r.byRGD[entry.RGDName] = bucket
	}

	bucket.entries[entry.Revision] = entry
	if !bucket.hasLatest || entry.Revision > bucket.latestRevision {
		bucket.latestRevision = entry.Revision
		bucket.hasLatest = true
	}
}

// Get returns the entry for a specific (RGDName, Revision) pair.
func (r *Registry) Get(rgdName string, revision int64) (Entry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	bucket, ok := r.byRGD[rgdName]
	if !ok {
		return Entry{}, false
	}

	entry, ok := bucket.entries[revision]
	if !ok {
		return Entry{}, false
	}

	return entry, true
}

// HasAll returns true when all listed revision numbers are present for rgdName.
//
// This is a membership check only. Callers handle entry state separately.
func (r *Registry) HasAll(rgdName string, revisions []int64) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if len(revisions) == 0 {
		return true
	}

	bucket, ok := r.byRGD[rgdName]
	if !ok {
		return false
	}

	for _, revision := range revisions {
		if _, ok := bucket.entries[revision]; !ok {
			return false
		}
	}

	return true
}

// Latest returns the highest revision entry for rgdName.
//
// It returns false when there is no bucket, no latest marker, or if bucket
// metadata points to a missing entry.
func (r *Registry) Latest(rgdName string) (Entry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	bucket, ok := r.byRGD[rgdName]
	if !ok || !bucket.hasLatest {
		return Entry{}, false
	}

	entry, ok := bucket.entries[bucket.latestRevision]
	if !ok {
		return Entry{}, false
	}

	return entry, true
}

// Delete removes one revision entry for rgdName.
//
// The method is idempotent and safe to call for absent buckets/revisions.
func (r *Registry) Delete(rgdName string, revision int64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	bucket, ok := r.byRGD[rgdName]
	if !ok {
		return
	}

	if _, ok := bucket.entries[revision]; !ok {
		return
	}

	delete(bucket.entries, revision)
	if len(bucket.entries) == 0 {
		delete(r.byRGD, rgdName)
		return
	}

	if bucket.latestRevision != revision {
		// Fast path: deleting a non-latest revision cannot affect Latest().
		return
	}

	recomputeLatest(bucket)
}

// DeleteBelow removes all entries with revision strictly lower than minRevision.
//
// This is used by retention GC to keep only recent revisions per RGD.
func (r *Registry) DeleteBelow(rgdName string, minRevision int64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	bucket, ok := r.byRGD[rgdName]
	if !ok {
		return
	}

	for revision := range bucket.entries {
		if revision < minRevision {
			delete(bucket.entries, revision)
		}
	}

	if len(bucket.entries) == 0 {
		delete(r.byRGD, rgdName)
		return
	}

	if bucket.latestRevision >= minRevision {
		// Fast path: latest is still retained.
		return
	}

	recomputeLatest(bucket)
}

func recomputeLatest(bucket *rgdBucket) {
	// Rebuild latest pointer from current entries after a deletion path invalidates
	// the previous marker.
	var (
		latestRevision int64
		hasLatest      bool
	)
	for revision := range bucket.entries {
		if !hasLatest || revision > latestRevision {
			latestRevision = revision
			hasLatest = true
		}
	}

	bucket.latestRevision = latestRevision
	bucket.hasLatest = hasLatest
}
