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

package applyset

import (
	"errors"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
)

// ApplyResult contains outcomes for all resources.
type ApplyResult struct {
	Applied []ApplyResultItem
}

// ApplyResultItem is the outcome of applying a single resource.
type ApplyResultItem struct {
	ID       string                     // same as input Resource.ID
	Desired  *unstructured.Unstructured // what we sent
	Observed *unstructured.Unstructured // cluster state after apply (nil if error)
	Changed  bool                       // resourceVersion changed
	Error    error
}

// PruneResultItem is a successfully pruned resource.
type PruneResultItem struct {
	Object *unstructured.Unstructured
}

// OrphanCandidate is a resource discovered during orphan listing that is
// a member of the applyset but not in the keep set. Callers use this to
// apply ordering (e.g. reverse-topological) before issuing deletes.
type OrphanCandidate struct {
	Object *unstructured.Unstructured
	GVR    schema.GroupVersionResource
}

// DeleteOrphanResult describes the outcome of a single orphan deletion.
type DeleteOrphanResult struct {
	// Pruned is non-nil when the delete was accepted by the API server.
	Pruned *PruneResultItem
	// Conflict is true when the UID precondition failed (resource was recreated).
	Conflict bool
}

// ByID returns a map of results keyed by resource ID for easy lookup.
func (r *ApplyResult) ByID() map[string]ApplyResultItem {
	m := make(map[string]ApplyResultItem, len(r.Applied))
	for _, item := range r.Applied {
		m[item.ID] = item
	}
	return m
}

// ObservedUIDs returns the UIDs of all successfully applied resources.
func (r *ApplyResult) ObservedUIDs() sets.Set[types.UID] {
	uids := sets.New[types.UID]()
	for _, item := range r.Applied {
		if item.Error == nil && item.Observed != nil {
			uids.Insert(item.Observed.GetUID())
		}
	}
	return uids
}

// Errors returns combined errors from apply operations, or nil if none.
// Use this to check if it's safe to prune (don't prune if applies failed).
func (r *ApplyResult) Errors() error {
	var errs []error
	for _, item := range r.Applied {
		if item.Error != nil {
			errs = append(errs, item.Error)
		}
	}
	return errors.Join(errs...)
}

// HasClusterMutation returns true if any apply operation changed the cluster.
func (r *ApplyResult) HasClusterMutation() bool {
	for _, item := range r.Applied {
		if item.Changed {
			return true
		}
	}
	return false
}
