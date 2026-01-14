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

const (
	InstanceStateInProgress = "IN_PROGRESS"
	InstanceStateFailed     = "FAILED"
	InstanceStateActive     = "ACTIVE"
	InstanceStateDeleting   = "DELETING"
	InstanceStateError      = "ERROR"
)

type ResourceState struct {
	State string
	Err   error
}

type StateManager struct {
	State          string
	ResourceStates map[string]*ResourceState
	ReconcileErr   error
}

func newStateManager() *StateManager {
	return &StateManager{
		State:          InstanceStateInProgress,
		ResourceStates: make(map[string]*ResourceState),
	}
}

func (s *StateManager) ResourceErrors() error {
	var errs []error
	for _, st := range s.ResourceStates {
		if st.Err != nil {
			errs = append(errs, st.Err)
		}
	}
	return errors.Join(errs...)
}

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

	// Check resource states
	allSynced := true
	hasError := false
	for _, st := range s.ResourceStates {
		switch st.State {
		case ResourceStateError:
			hasError = true
		case ResourceStateSynced, ResourceStateSkipped, ResourceStateDeleted:
			// terminal/success states
		default:
			allSynced = false
		}
		if hasError {
			break
		}
	}

	// Transition based on resource states
	if hasError {
		s.State = InstanceStateError
	} else if allSynced {
		s.State = InstanceStateActive
	} else {
		s.State = InstanceStateInProgress
	}
}
