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

// NOTE(a-hilaly): move to internal package ?
package watchtracker

import (
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/clock"
)

// Tracker tracks the state of watches for GVRs, including sync status and errors.
// It emits state change events that consumers can use to react to watch health changes.
//
// Optimized for high scale (10K+ RGDs, high error rates). Uses sync.Map for the
// states so operations on different GVRs don't block each other. Each entry has
// its own mutex because multiple goroutines can access the same entry concurrently
// (watchEventHandler, recovery timers, deregistration, RGD reconciliation).
type Tracker struct {
	// states maps each GroupVersionResource to its current state entry.
	// Using sync.Map for lock free concurrent access to different GVRs.
	states sync.Map // map[schema.GroupVersionResource]*stateEntry

	// stateEventsCh is the channel where state change events are sent.
	// Events are sent non blocking; if the buffer is full, events are dropped.
	// Consumers should process events promptly to avoid drops.
	stateEventsCh chan StateChangeEvent

	// recoveryTimeout is the duration without errors before a watch is considered
	// recovered from a degraded state.
	recoveryTimeout time.Duration

	// clock is used for time operations, allowing injection of fake clocks for testing.
	clock clock.WithDelayedExecution
}

// stateEntry holds the state and recovery timer for a single GVR.
// Each entry has its own mutex for fine grained locking.
type stateEntry struct {
	mu            sync.Mutex
	state         State
	recoveryTimer clock.Timer
}

// NewTracker creates a new watch state tracker.
//
// recoveryTimeout is the duration without errors before a watch is considered recovered.
func NewTracker(recoveryTimeout time.Duration, bufferSize int) *Tracker {
	return &Tracker{
		stateEventsCh:   make(chan StateChangeEvent, bufferSize),
		recoveryTimeout: recoveryTimeout,
		clock:           clock.RealClock{},
	}
}

// NewTrackerWithClock creates a tracker with a custom clock (for testing).
func NewTrackerWithClock(recoveryTimeout time.Duration, bufferSize int, clk clock.WithDelayedExecution) *Tracker {
	return &Tracker{
		stateEventsCh:   make(chan StateChangeEvent, bufferSize),
		recoveryTimeout: recoveryTimeout,
		clock:           clk,
	}
}

// GetState returns the current state of a watch for a GVR.
// Returns false if the GVR has never been tracked.
//
// This operation is lock free for the map lookup and only briefly locks
// the specific entry to copy the state.
func (t *Tracker) GetState(gvr schema.GroupVersionResource) (State, bool) {
	value, exists := t.states.Load(gvr)
	if !exists {
		return State{}, false
	}

	entry := value.(*stateEntry)
	entry.mu.Lock()
	state := entry.state
	entry.mu.Unlock()

	return state, true
}

// StateEvents returns a channel that receives state change events.
func (t *Tracker) StateEvents() <-chan StateChangeEvent {
	return t.stateEventsCh
}

// InitState initializes tracking for a GVR in Syncing state.
// If the GVR is already tracked, this is a no op.
func (t *Tracker) InitState(gvr schema.GroupVersionResource) {
	// LoadOrStore is atomic - only creates entry if not exists
	_, loaded := t.states.LoadOrStore(gvr, &stateEntry{
		state: State{Status: StatusSyncing},
	})
	if !loaded {
		watchTrackerGVRsTotal.Inc()
		watchTrackerGVRsByStatus.WithLabelValues(string(StatusSyncing)).Inc()
	}
}

// MarkSynced marks a GVR as synced.
// If the GVR was not initialized, this is a no-op.
func (t *Tracker) MarkSynced(gvr schema.GroupVersionResource) {
	value, exists := t.states.Load(gvr)
	if !exists {
		return
	}
	entry := value.(*stateEntry)

	entry.mu.Lock()
	defer entry.mu.Unlock()

	oldState := entry.state
	entry.state.Synced = true

	if entry.state.HasError {
		entry.state.Status = StatusDegraded
	} else {
		entry.state.Status = StatusSynced
	}

	if oldState.Status != entry.state.Status {
		watchTrackerGVRsByStatus.WithLabelValues(string(oldState.Status)).Dec()
		watchTrackerGVRsByStatus.WithLabelValues(string(entry.state.Status)).Inc()
		t.notifyStateChange(gvr, oldState, entry.state)
	}
}

// RecordError records a watch error for a GVR.
// This transitions the watch to a degraded state and starts a recovery timer.
// If the GVR was not initialized, this is a no-op.
func (t *Tracker) RecordError(gvr schema.GroupVersionResource, err error) {
	value, exists := t.states.Load(gvr)
	if !exists {
		return
	}
	entry := value.(*stateEntry)

	entry.mu.Lock()
	defer entry.mu.Unlock()

	oldState := entry.state

	entry.state.HasError = true
	entry.state.LastError = err
	entry.state.LastErrorTime = t.clock.Now()
	entry.state.ErrorCount++

	if entry.state.Synced {
		entry.state.Status = StatusDegraded
	} else {
		entry.state.Status = StatusSyncingError
	}

	// Reset recovery timer
	if entry.recoveryTimer != nil {
		entry.recoveryTimer.Stop()
	}
	entry.recoveryTimer = t.clock.AfterFunc(t.recoveryTimeout, func() {
		t.checkRecovery(gvr)
	})

	watchTrackerErrorsTotal.Inc()

	if oldState.Status != entry.state.Status {
		watchTrackerGVRsByStatus.WithLabelValues(string(oldState.Status)).Dec()
		watchTrackerGVRsByStatus.WithLabelValues(string(entry.state.Status)).Inc()
		t.notifyStateChange(gvr, oldState, entry.state)
	}
}

// checkRecovery is called by the recovery timer to check if a GVR has recovered.
func (t *Tracker) checkRecovery(gvr schema.GroupVersionResource) {
	value, exists := t.states.Load(gvr)
	if !exists {
		return
	}

	entry := value.(*stateEntry)
	entry.mu.Lock()
	defer entry.mu.Unlock()

	// If the timer was replaced (new error came in), this callback is stale
	if entry.recoveryTimer == nil {
		return
	}

	oldState := entry.state

	entry.state.HasError = false
	entry.state.ErrorCount = 0

	if entry.state.Synced {
		entry.state.Status = StatusSynced
	} else {
		entry.state.Status = StatusSyncing
	}

	entry.recoveryTimer = nil

	if oldState.Status != entry.state.Status {
		watchTrackerRecoveriesTotal.Inc()
		watchTrackerGVRsByStatus.WithLabelValues(string(oldState.Status)).Dec()
		watchTrackerGVRsByStatus.WithLabelValues(string(entry.state.Status)).Inc()
		t.notifyStateChange(gvr, oldState, entry.state)
	}
}

// MarkStopped marks a GVR as stopped and removes it from tracking.
func (t *Tracker) MarkStopped(gvr schema.GroupVersionResource) {
	value, exists := t.states.LoadAndDelete(gvr)
	if !exists {
		return
	}

	entry := value.(*stateEntry)
	entry.mu.Lock()

	if entry.recoveryTimer != nil {
		entry.recoveryTimer.Stop()
	}

	oldState := entry.state
	entry.mu.Unlock()

	watchTrackerGVRsTotal.Dec()
	watchTrackerGVRsByStatus.WithLabelValues(string(oldState.Status)).Dec()

	t.notifyStateChange(gvr, oldState, State{Status: StatusStopped})
}

// RemoveState removes a GVR from tracking without emitting events.
// Use this for cleanup when the watch is removed but no notification is needed.
func (t *Tracker) RemoveState(gvr schema.GroupVersionResource) {
	value, exists := t.states.LoadAndDelete(gvr)
	if !exists {
		return
	}

	entry := value.(*stateEntry)
	entry.mu.Lock()
	oldStatus := entry.state.Status
	if entry.recoveryTimer != nil {
		entry.recoveryTimer.Stop()
	}
	entry.mu.Unlock()

	watchTrackerGVRsTotal.Dec()
	watchTrackerGVRsByStatus.WithLabelValues(string(oldStatus)).Dec()
}

// notifyStateChange sends a state change event to listeners.
// This is non-blocking; if the channel buffer is full, the event is dropped.
func (t *Tracker) notifyStateChange(gvr schema.GroupVersionResource, oldState, newState State) {
	select {
	case t.stateEventsCh <- StateChangeEvent{GVR: gvr, OldState: oldState, NewState: newState}:
	default:
		// Channel full - drop event
		watchTrackerEventsDroppedTotal.Inc()
	}
}

// Shutdown stops all recovery timers.
// After shutdown, the tracker should not be used.
func (t *Tracker) Shutdown() {
	t.states.Range(func(key, value any) bool {
		entry := value.(*stateEntry)
		entry.mu.Lock()
		if entry.recoveryTimer != nil {
			entry.recoveryTimer.Stop()
		}
		entry.mu.Unlock()
		return true
	})
}
