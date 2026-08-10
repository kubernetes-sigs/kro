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

import (
	"errors"

	v1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
)

// NodeState holds the current reconciliation state for a node.
type NodeState struct {
	State v1alpha1.NodeState
	Err   error
}

// Value constructors for NodeState — prefer these over mutating pointers.

func inProgressState() NodeState {
	return NodeState{State: v1alpha1.NodeStateInProgress}
}

func errorState(err error) NodeState {
	return NodeState{State: v1alpha1.NodeStateError, Err: err}
}

func skippedState() NodeState {
	return NodeState{State: v1alpha1.NodeStateSkipped}
}

func readyState() NodeState {
	return NodeState{State: v1alpha1.NodeStateSynced}
}

func deletingState() NodeState {
	return NodeState{State: v1alpha1.NodeStateDeleting}
}

func waitingForReadinessState(err error) NodeState {
	return NodeState{State: v1alpha1.NodeStateWaitingForReadiness, Err: err}
}

// Pointer mutation methods — retained for processApplyResults and deletion paths
// that update state on existing registered nodes.

// SetInProgress marks the node as in progress and clears any error.
func (st *NodeState) SetInProgress() {
	st.State = v1alpha1.NodeStateInProgress
	st.Err = nil
}

// SetSkipped marks the node as intentionally skipped and clears any error.
func (st *NodeState) SetSkipped() {
	st.State = v1alpha1.NodeStateSkipped
	st.Err = nil
}

// SetError marks the node as failed and records err.
func (st *NodeState) SetError(err error) {
	st.State = v1alpha1.NodeStateError
	st.Err = err
}

// SetReady marks the node as ready/synced and clears any error.
func (st *NodeState) SetReady() {
	st.State = v1alpha1.NodeStateSynced
	st.Err = nil
}

// SetDeleted marks the node as deleted and clears any error.
func (st *NodeState) SetDeleted() {
	st.State = v1alpha1.NodeStateDeleted
	st.Err = nil
}

// SetDeleting marks the node as deletion-in-progress and clears any error.
func (st *NodeState) SetDeleting() {
	st.State = v1alpha1.NodeStateDeleting
	st.Err = nil
}

// SetWaitingForReadiness marks the node as waiting for readiness, optionally
// recording err.
func (st *NodeState) SetWaitingForReadiness(err error) {
	st.State = v1alpha1.NodeStateWaitingForReadiness
	st.Err = err
}

// StateManager tracks instance and node states during reconciliation.
// It is not safe for concurrent use; reconciliation processes nodes sequentially.
type StateManager struct {
	State        v1alpha1.InstanceState
	NodeStates   map[string]*NodeState
	ReconcileErr error
}

// newStateManager constructs a StateManager with initialized fields.
func newStateManager() *StateManager {
	return &StateManager{
		State:      v1alpha1.InstanceStateInProgress,
		NodeStates: make(map[string]*NodeState),
	}
}

// NewNodeState initializes and registers node state as InProgress.
// Returns a pointer for processApplyResults and deletion paths that
// update state on already-registered nodes.
func (s *StateManager) NewNodeState(id string) *NodeState {
	st := &NodeState{State: v1alpha1.NodeStateInProgress}
	s.NodeStates[id] = st
	return st
}

// SetNodeState registers a node state by value. This is the preferred
// registration method for processNode — it makes the caller the single
// point where state is written.
func (s *StateManager) SetNodeState(id string, state NodeState) {
	s.NodeStates[id] = &state
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
	if s.ReconcileErr != nil {
		s.State = v1alpha1.InstanceStateError
		return
	}

	if s.State == v1alpha1.InstanceStateDeleting {
		return
	}

	allSynced := true
	hasError := false
	for _, st := range s.NodeStates {
		switch st.State {
		case v1alpha1.NodeStateError:
			hasError = true
		case v1alpha1.NodeStateSynced, v1alpha1.NodeStateSkipped, v1alpha1.NodeStateDeleted:
			// terminal/success states
		default:
			allSynced = false
		}
		if hasError {
			break
		}
	}

	if hasError {
		s.State = v1alpha1.InstanceStateError
	} else if allSynced {
		s.State = v1alpha1.InstanceStateActive
	} else {
		s.State = v1alpha1.InstanceStateInProgress
	}
}
