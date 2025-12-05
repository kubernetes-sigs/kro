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

func TestNewInstanceState(t *testing.T) {
	state := newInstanceState()

	if state.State != InstanceStateInProgress {
		t.Errorf("expected State to be %q, got %q", InstanceStateInProgress, state.State)
	}

	if state.ResourceStates == nil {
		t.Error("expected ResourceStates to be initialized, got nil")
	}

	if len(state.ResourceStates) != 0 {
		t.Errorf("expected ResourceStates to be empty, got %d items", len(state.ResourceStates))
	}

	if state.ReconcileErr != nil {
		t.Errorf("expected ReconcileErr to be nil, got %v", state.ReconcileErr)
	}
}

func TestResourceErrors(t *testing.T) {
	err1 := errors.New("error 1")
	err2 := errors.New("error 2")
	singleErr := errors.New("resource failed")

	tests := map[string]struct {
		resourceStates map[string]*ResourceState
		expectError    bool
		expectedErrors []error
	}{
		"no errors": {
			resourceStates: map[string]*ResourceState{
				"resource1": {State: "ACTIVE", Err: nil},
				"resource2": {State: "ACTIVE", Err: nil},
			},
			expectError:    false,
			expectedErrors: nil,
		},
		"single error": {
			resourceStates: map[string]*ResourceState{
				"resource1": {State: "FAILED", Err: singleErr},
				"resource2": {State: "ACTIVE", Err: nil},
			},
			expectError:    true,
			expectedErrors: []error{singleErr},
		},
		"multiple errors": {
			resourceStates: map[string]*ResourceState{
				"resource1": {State: "FAILED", Err: err1},
				"resource2": {State: "FAILED", Err: err2},
			},
			expectError:    true,
			expectedErrors: []error{err1, err2},
		},
		"empty resource states": {
			resourceStates: map[string]*ResourceState{},
			expectError:    false,
			expectedErrors: nil,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			state := &InstanceState{
				ResourceStates: tt.resourceStates,
			}

			err := state.ResourceErrors()

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
