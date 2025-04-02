// Copyright 2025 The Kube Resource Orchestrator Authors.
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

// Inspired by https://github.com/knative/pkg/tree/97c7258e3a98b81459936bc7a29dc6a9540fa357/apis,
// but we chose to diverge due to the unacceptably large dependency closure of knative/pkg.

package apis

import "golang.org/x/exp/slices"

// NewReadyConditions returns a ConditionTypes to hold the conditions for the
// resource. ConditionReady is used as the root condition.
// The set of condition types provided are those of the terminal sub-conditions.
func NewReadyConditions(d ...string) ConditionTypes {
	return newConditionTypes(ConditionReady, d...)
}

// NewSucceededConditions returns a ConditionTypes to hold the conditions for the
// batch resource. ConditionSucceeded is used as the root condition.
// The set of condition types provided are those of the terminal sub-conditions.
func NewSucceededConditions(d ...string) ConditionTypes {
	return newConditionTypes(ConditionSucceeded, d...)
}

// ConditionTypes is an abstract collection of the possible ConditionType values
// that a particular resource might expose.  It also holds the "root condition"
// for that resource, which we define to be one of Ready or Succeeded depending
// on whether it is a Living or Batch process respectively.
type ConditionTypes struct {
	root       string
	dependents []string
}

// For creates a ConditionSet from an object using the original
// ConditionTypes as a reference. Status must be a pointer to a struct.
func (ct ConditionTypes) For(object Object) ConditionSet {
	cs := ConditionSet{object: object, ConditionTypes: ct}
	// Set known conditions Unknown if not set.
	// Set the root condition first to get consistent timing for LastTransitionTime
	for _, t := range append([]string{ct.root}, ct.dependents...) {
		if cs.Get(t) == nil {
			cs.SetUnknown(t)
		}
	}
	return cs
}

// DependsOn is a helper function to determine if deps contains the provided condition type.
func (ct ConditionTypes) DependsOn(d string) bool {
	for i := range ct.dependents {
		if ct.dependents[i] == d {
			return true
		}
	}
	return false
}

func newConditionTypes(root string, dependents ...string) ConditionTypes {
	deps := make([]string, 0, len(dependents))
	for _, d := range dependents {
		// Skip duplicates
		if d == root || slices.Contains(deps, d) {
			continue
		}
		deps = append(deps, d)
	}
	return ConditionTypes{
		root:       root,
		dependents: deps,
	}
}
