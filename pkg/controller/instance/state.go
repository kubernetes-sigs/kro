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

import "errors"

// InstanceState represents high-level reconciliation state for an instance.
type InstanceState string

const (
	// InstanceStateInProgress means reconciliation is ongoing.
	InstanceStateInProgress InstanceState = "IN_PROGRESS"
	// InstanceStateFailed is a legacy state kept for compatibility.
	InstanceStateFailed InstanceState = "FAILED"
	// InstanceStateActive means all nodes reached terminal success states.
	InstanceStateActive InstanceState = "ACTIVE"
	// InstanceStateDeleting means deletion workflow is running.
	InstanceStateDeleting InstanceState = "DELETING"
	// InstanceStateError means reconciliation hit an error.
	InstanceStateError InstanceState = "ERROR"
)

// NodeState values track the lifecycle of individual nodes during reconciliation.
const (
	// NodeStateInProgress is the initial state when a node starts being processed.
	// Set at the beginning of processNode before any operations.
	NodeStateInProgress = "IN_PROGRESS"
	// NodeStateDeleting means delete was called but the node still exists.
	// Set when deletion is initiated but not yet confirmed.
	NodeStateDeleting = "DELETING"
	// NodeStateSkipped means the node was not applied.
	// Set when includeWhen=false, dependency is skipped, or external ref during deletion.
	NodeStateSkipped = "SKIPPED"
	// NodeStateError means something failed.
	// Set when resolution, apply, or delete fails.
	NodeStateError = "ERROR"
	// NodeStateSynced means the node was applied and is ready.
	// Set when apply succeeds and readyWhen is satisfied.
	NodeStateSynced = "SYNCED"
	// NodeStateDeleted means the node no longer exists.
	// Set when deletion is confirmed or resource was not found.
	NodeStateDeleted = "DELETED"
	// NodeStateWaitingForReadiness means apply succeeded but readyWhen is not satisfied.
	// Set when apply succeeds but readyWhen evaluates to false.
	NodeStateWaitingForReadiness = "WAITING_FOR_READINESS"
)

// NodeState holds the current reconciliation state for a node.
// Prefer mutating this struct via its helper methods.
type NodeState struct {
	State string
	Err   error
}

// SetInProgress marks the node as in progress and clears any error.
func (st *NodeState) SetInProgress() {
	st.State = NodeStateInProgress
	st.Err = nil
}

// SetError marks the node as failed and records err.
func (st *NodeState) SetError(err error) {
	st.State = NodeStateError
	st.Err = err
}

// SetSkipped marks the node as intentionally skipped and clears any error.
func (st *NodeState) SetSkipped() {
	st.State = NodeStateSkipped
	st.Err = nil
}

// SetReady marks the node as ready/synced and clears any error.
func (st *NodeState) SetReady() {
	st.State = NodeStateSynced
	st.Err = nil
}

// SetDeleted marks the node as deleted and clears any error.
func (st *NodeState) SetDeleted() {
	st.State = NodeStateDeleted
	st.Err = nil
}

// SetDeleting marks the node as deletion-in-progress and clears any error.
func (st *NodeState) SetDeleting() {
	st.State = NodeStateDeleting
	st.Err = nil
}

// SetWaitingForReadiness marks the node as waiting for readiness, optionally
// recording err.
func (st *NodeState) SetWaitingForReadiness(err error) {
	st.State = NodeStateWaitingForReadiness
	st.Err = err
}

// StateManager tracks instance and node states during reconciliation.

type StateManager struct {
	State              InstanceState
	NodeStates         map[string]*NodeState
	ReconcileErr       error
	ObservedGeneration int64 // Fix: Track the generation to avoid being stuck "IN_PROGRESS"
}

// newStateManager constructs a StateManager with initialized fields.
// Brackets are kept empty () to prevent errors in context.go and status.go.
func newStateManager(generation int64) *StateManager {
    return &StateManager{
        State:              InstanceStateInProgress,
        NodeStates:         make(map[string]*NodeState),
        ObservedGeneration: generation, 
    }
}
// SetObservedGeneration allows setting the version without breaking other files.
func (s *StateManager) SetObservedGenration(gen int64) {
	s.ObservedGeneration = gen
}

// NewNodeState initializes and registers node state.
// Callers should prefer this over allocating NodeState directly.
func (s *StateManager) NewNodeState(id string) *NodeState {
	st := &NodeState{}
	st.SetInProgress()
	s.NodeStates[id] = st
	return st
}

// NodeErrors aggregates errors across all node states.
func (s *StateManager) NodeErrors() error {
	var errs []error
	for _, st := range s.NodeStates {
		if st.Err != nil {
			errs = append(errs, st.Err)
		}
	}
	return errors.Join(errs...)
}

// Update recomputes the instance state from node states.
func (s *StateManager) Update() {
	// If reconciliation hit an error, mark instance as ERROR
	if s.ReconcileErr != nil {
		s.State = InstanceStateError
		return
	}

	// If instance is deleting, keep state
	if s.State == InstanceStateDeleting {
		return
	}

	// Check node states
	allSynced := true
	hasError := false
	for _, st := range s.NodeStates {
		switch st.State {
		case NodeStateError:
			hasError = true
		case NodeStateSynced, NodeStateSkipped, NodeStateDeleted:
			// terminal/success states
		default:
			allSynced = false
		}
		if hasError {
			break
		}
	}

	// Transition based on node states
	if hasError {
		s.State = InstanceStateError
	} else if allSynced {
		s.State = InstanceStateActive
	} else {
		s.State = InstanceStateInProgress
	}
}
