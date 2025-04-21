// Copyright 2025 The Kube Resource Orchestrator Authors
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

package instance

const (
	InstanceStateInProgress = "IN_PROGRESS"
	InstanceStateFailed     = "FAILED"
	InstanceStateActive     = "ACTIVE"
	InstanceStateDeleting   = "DELETING"
	InstanceStateError      = "ERROR"
)

// newInstanceState creates a new InstanceState with initialized fields
func newInstanceState() *InstanceState {
	return &InstanceState{
		State:          "IN_PROGRESS",
		ResourceStates: make(map[string]*ResourceState),
	}
}

// ResourceState represents the state and any associated error of a resource
// being managed by the controller.
type ResourceState struct {
	// State represents the current state of the resource
	State string
	// Err captures any error associated with the current state
	Err error
}

// InstanceState tracks the overall state of resources being managed
type InstanceState struct {
	// Current state of the instance
	State string
	// Map of resource IDs to their current states
	ResourceStates map[string]*ResourceState
	// Any error encountered during reconciliation
	ReconcileErr error
}
