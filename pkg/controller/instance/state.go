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

	// Determine whether all resources are synced
	allSynced := true
	for _, st := range s.ResourceStates {
		if st.State != ResourceStateSynced {
			allSynced = false
			break
		}
	}

	// Transition to ACTIVE if everything is synced
	if allSynced {
		s.State = InstanceStateActive
	} else {
		s.State = InstanceStateInProgress
	}
}
