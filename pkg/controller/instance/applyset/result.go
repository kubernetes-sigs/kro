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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
)

// ApplyResult contains outcomes for all resources.
type ApplyResult struct {
	Applied []ApplyResultItem
}

// PruneResult contains outcomes for prune operations.
// All items in Pruned are successful deletes (errors return from Prune directly).
type PruneResult struct {
	Pruned []PruneResultItem
}

// HasPruned returns true if any resources were pruned.
func (r *PruneResult) HasPruned() bool {
	return len(r.Pruned) > 0
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
// Note: Prune mutations are tracked separately via PruneResult.HasPruned().
func (r *ApplyResult) HasClusterMutation() bool {
	for _, item := range r.Applied {
		if item.Changed {
			return true
		}
	}
	return false
}
