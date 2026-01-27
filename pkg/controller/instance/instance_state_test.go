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

package instance

import (
	"errors"
	"testing"
)

func TestNewStateManager(t *testing.T) {
	state := newStateManager()

	if state.State != InstanceStateInProgress {
		t.Errorf("expected State to be %q, got %q", InstanceStateInProgress, state.State)
	}

	if state.NodeStates == nil {
		t.Error("expected NodeStates to be initialized, got nil")
	}

	if len(state.NodeStates) != 0 {
		t.Errorf("expected NodeStates to be empty, got %d items", len(state.NodeStates))
	}

	if state.ReconcileErr != nil {
		t.Errorf("expected ReconcileErr to be nil, got %v", state.ReconcileErr)
	}
}

func TestNodeErrors(t *testing.T) {
	err1 := errors.New("error 1")
	err2 := errors.New("error 2")
	singleErr := errors.New("node failed")

	tests := map[string]struct {
		nodeStates     map[string]*NodeState
		expectError    bool
		expectedErrors []error
	}{
		"no errors": {
			nodeStates: map[string]*NodeState{
				"resource1": {State: "ACTIVE", Err: nil},
				"resource2": {State: "ACTIVE", Err: nil},
			},
			expectError:    false,
			expectedErrors: nil,
		},
		"single error": {
			nodeStates: map[string]*NodeState{
				"resource1": {State: "FAILED", Err: singleErr},
				"resource2": {State: "ACTIVE", Err: nil},
			},
			expectError:    true,
			expectedErrors: []error{singleErr},
		},
		"multiple errors": {
			nodeStates: map[string]*NodeState{
				"resource1": {State: "FAILED", Err: err1},
				"resource2": {State: "FAILED", Err: err2},
			},
			expectError:    true,
			expectedErrors: []error{err1, err2},
		},
		"empty node states": {
			nodeStates:     map[string]*NodeState{},
			expectError:    false,
			expectedErrors: nil,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			state := &StateManager{
				NodeStates: tt.nodeStates,
			}

			err := state.NodeErrors()

			if tt.expectError {
				if err == nil {
					t.Fatal("expected error, got nil")
				}

				// Check that all expected errors are contained in the result
				for _, expectedErr := range tt.expectedErrors {
					if !errors.Is(err, expectedErr) {
						t.Errorf("expected error to contain %v, got %v", expectedErr, err)
					}
				}
			} else {
				if err != nil {
					t.Errorf("expected no error, got %v", err)
				}
			}
		})
	}
}
