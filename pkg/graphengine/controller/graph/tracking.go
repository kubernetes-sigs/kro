// Copyright 2026 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package graph

import (
	expv1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/executor"
)

// resourceKey is the identity tuple used to dedup ManagedResource
// entries. UID is excluded because pre-apply entries (write-ahead)
// and post-apply entries (with UID) describe the same resource.
type resourceKey struct {
	APIVersion string
	Kind       string
	Namespace  string
	Name       string
}

func keyOf(r expv1alpha1.ManagedResource) resourceKey {
	return resourceKey{
		APIVersion: r.APIVersion,
		Kind:       r.Kind,
		Namespace:  r.Namespace,
		Name:       r.Name,
	}
}

// diffManagedResources compares the previously-tracked set against the
// just-applied set, accounting for nodes whose identities couldn't be
// resolved this cycle.
//
// Returned newSet is the set the controller wants to advertise after a
// fully-successful Apply (Applied entries + entries preserved from
// previous because their NodeID hit data-pending). pruneCandidates are
// previous entries we're confident are no longer wanted — node dropped
// from spec, forEach shrunk, rename, or includeWhen flipped to false.
//
// Order: newSet preserves the executor's topological apply order from
// result.Applied; preserved entries are appended after, in their
// previous-cycle order. Reverse iteration over newSet still gives a
// reasonable reverse-apply order for delete.
func diffManagedResources(previous []expv1alpha1.ManagedResource, result executor.ApplyResult) (newSet, pruneCandidates []expv1alpha1.ManagedResource) {
	unresolved := make(map[string]struct{}, len(result.Unresolved))
	for _, nodeID := range result.Unresolved {
		unresolved[nodeID] = struct{}{}
	}

	applied := make(map[resourceKey]expv1alpha1.ManagedResource, len(result.Applied))
	for _, r := range result.Applied {
		applied[keyOf(r)] = r
	}

	newSet = make([]expv1alpha1.ManagedResource, 0, len(result.Applied)+len(previous))
	newSet = append(newSet, result.Applied...)

	for _, prev := range previous {
		if _, alreadyApplied := applied[keyOf(prev)]; alreadyApplied {
			continue
		}
		if _, isUnresolved := unresolved[prev.NodeID]; isUnresolved {
			newSet = append(newSet, prev)
			continue
		}
		pruneCandidates = append(pruneCandidates, prev)
	}
	return newSet, pruneCandidates
}

// unionManagedResources concatenates previous and applied, deduping on
// identity. Used when an Apply hit a soft or hard error — we don't
// trust the diff result enough to prune, so we widen status to cover
// everything we know about. Order preserves previous first, then
// newly-applied entries that previous didn't already have.
func unionManagedResources(previous []expv1alpha1.ManagedResource, applied []expv1alpha1.ManagedResource) []expv1alpha1.ManagedResource {
	seen := make(map[resourceKey]struct{}, len(previous)+len(applied))
	out := make([]expv1alpha1.ManagedResource, 0, len(previous)+len(applied))
	for _, r := range previous {
		k := keyOf(r)
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, r)
	}
	for _, r := range applied {
		k := keyOf(r)
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, r)
	}
	return out
}
