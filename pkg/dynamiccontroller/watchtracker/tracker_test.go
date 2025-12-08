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

package watchtracker

import (
	"errors"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
	testingclock "k8s.io/utils/clock/testing"
)

var testGVR = schema.GroupVersionResource{
	Group:    "test.kro.run",
	Version:  "v1",
	Resource: "widgets",
}

func TestRecordError_TransitionsToDegraded(t *testing.T) {
	fakeClock := testingclock.NewFakeClock(time.Now())
	tracker := NewTrackerWithClock(30*time.Second, 10, fakeClock)

	tracker.InitState(testGVR)
	tracker.MarkSynced(testGVR)

	state, found := tracker.GetState(testGVR)
	if !found {
		t.Fatal("expected state to be found")
	}
	if state.Status != StatusSynced {
		t.Errorf("expected status %s, got %s", StatusSynced, state.Status)
	}

	testErr := errors.New("watch error")
	tracker.RecordError(testGVR, testErr)

	state, _ = tracker.GetState(testGVR)
	if state.Status != StatusDegraded {
		t.Errorf("expected status %s, got %s", StatusDegraded, state.Status)
	}
	if !state.HasError {
		t.Error("expected HasError to be true")
	}
	if state.ErrorCount != 1 {
		t.Errorf("expected ErrorCount 1, got %d", state.ErrorCount)
	}
	if state.LastError != testErr {
		t.Errorf("expected LastError %v, got %v", testErr, state.LastError)
	}

	// Step past recovery timeout and wait for timer to fire
	fakeClock.Step(45 * time.Second)

	state, _ = tracker.GetState(testGVR)
	if state.HasError {
		t.Error("expected HasError to be false after recovery")
	}
	if state.ErrorCount != 0 {
		t.Errorf("expected ErrorCount 0 after recovery, got %d", state.ErrorCount)
	}
	if state.Status != StatusSynced {
		t.Errorf("expected status %s after recovery, got %s", StatusSynced, state.Status)
	}
}

func TestRecordError_RecoveryAfterTimeout(t *testing.T) {
	fakeClock := testingclock.NewFakeClock(time.Now())
	recoveryTimeout := 30 * time.Second
	tracker := NewTrackerWithClock(recoveryTimeout, 10, fakeClock)

	tracker.InitState(testGVR)
	tracker.MarkSynced(testGVR)

	tracker.RecordError(testGVR, errors.New("watch error"))

	state, _ := tracker.GetState(testGVR)
	if state.Status != StatusDegraded {
		t.Errorf("expected status %s, got %s", StatusDegraded, state.Status)
	}

	// Step past recovery timeout and wait for timer to fire
	fakeClock.Step(recoveryTimeout + time.Second)
	time.Sleep(10 * time.Millisecond)

	state, _ = tracker.GetState(testGVR)
	if state.Status != StatusSynced {
		t.Errorf("expected status %s after recovery, got %s", StatusSynced, state.Status)
	}
	if state.HasError {
		t.Error("expected HasError to be false after recovery")
	}
	if state.ErrorCount != 0 {
		t.Errorf("expected ErrorCount 0 after recovery, got %d", state.ErrorCount)
	}
}

func TestRecordError_RepeatedErrorsResetTimer(t *testing.T) {
	fakeClock := testingclock.NewFakeClock(time.Now())
	recoveryTimeout := 30 * time.Second
	tracker := NewTrackerWithClock(recoveryTimeout, 10, fakeClock)

	tracker.InitState(testGVR)
	tracker.MarkSynced(testGVR)

	tracker.RecordError(testGVR, errors.New("error 1"))

	// Advance 20s - not enough for recovery
	fakeClock.Step(20 * time.Second)
	time.Sleep(10 * time.Millisecond)

	// Record another error - this resets the timer
	tracker.RecordError(testGVR, errors.New("error 2"))

	// Advance 15s - would be 35s from first error, but only 15s from second
	fakeClock.Step(15 * time.Second)
	time.Sleep(10 * time.Millisecond)

	state, _ := tracker.GetState(testGVR)
	if state.Status != StatusDegraded {
		t.Errorf("expected status %s (timer should have reset), got %s", StatusDegraded, state.Status)
	}
	if state.ErrorCount != 2 {
		t.Errorf("expected ErrorCount 2, got %d", state.ErrorCount)
	}

	// Advance another 20s - now 35s from second error, should recover
	fakeClock.Step(20 * time.Second)
	time.Sleep(10 * time.Millisecond)

	state, _ = tracker.GetState(testGVR)
	if state.Status != StatusSynced {
		t.Errorf("expected status %s after recovery, got %s", StatusSynced, state.Status)
	}
}

func TestSyncingToSynced(t *testing.T) {
	fakeClock := testingclock.NewFakeClock(time.Now())
	tracker := NewTrackerWithClock(30*time.Second, 10, fakeClock)

	tracker.InitState(testGVR)

	state, found := tracker.GetState(testGVR)
	if !found {
		t.Fatal("expected state to be found")
	}
	if state.Status != StatusSyncing {
		t.Errorf("expected status %s, got %s", StatusSyncing, state.Status)
	}
	if state.Synced {
		t.Error("expected Synced to be false")
	}

	tracker.MarkSynced(testGVR)

	state, _ = tracker.GetState(testGVR)
	if state.Status != StatusSynced {
		t.Errorf("expected status %s, got %s", StatusSynced, state.Status)
	}
	if !state.Synced {
		t.Error("expected Synced to be true")
	}
}

func TestSyncingToSyncingError(t *testing.T) {
	fakeClock := testingclock.NewFakeClock(time.Now())
	tracker := NewTrackerWithClock(30*time.Second, 10, fakeClock)

	tracker.InitState(testGVR)

	state, _ := tracker.GetState(testGVR)
	if state.Status != StatusSyncing {
		t.Errorf("expected status %s, got %s", StatusSyncing, state.Status)
	}

	// Record error before sync completes
	testErr := errors.New("watch error")
	tracker.RecordError(testGVR, testErr)

	state, _ = tracker.GetState(testGVR)
	if state.Status != StatusSyncingError {
		t.Errorf("expected status %s, got %s", StatusSyncingError, state.Status)
	}
	if !state.HasError {
		t.Error("expected HasError to be true")
	}
	if state.Synced {
		t.Error("expected Synced to be false")
	}
}

func TestSyncingErrorToSynced(t *testing.T) {
	fakeClock := testingclock.NewFakeClock(time.Now())
	recoveryTimeout := 30 * time.Second
	tracker := NewTrackerWithClock(recoveryTimeout, 10, fakeClock)

	tracker.InitState(testGVR)

	// Record error before sync
	tracker.RecordError(testGVR, errors.New("watch error"))

	state, _ := tracker.GetState(testGVR)
	if state.Status != StatusSyncingError {
		t.Errorf("expected status %s, got %s", StatusSyncingError, state.Status)
	}

	// Now sync completes - should transition to Degraded (synced but has error)
	tracker.MarkSynced(testGVR)

	state, _ = tracker.GetState(testGVR)
	if state.Status != StatusDegraded {
		t.Errorf("expected status %s after sync with error, got %s", StatusDegraded, state.Status)
	}
	if !state.Synced {
		t.Error("expected Synced to be true")
	}
	if !state.HasError {
		t.Error("expected HasError to be true")
	}

	// Wait for recovery
	fakeClock.Step(recoveryTimeout + time.Second)

	state, _ = tracker.GetState(testGVR)
	if state.Status != StatusSynced {
		t.Errorf("expected status %s after recovery, got %s", StatusSynced, state.Status)
	}
	if state.HasError {
		t.Error("expected HasError to be false after recovery")
	}
}

func TestGetState_NonExistent(t *testing.T) {
	fakeClock := testingclock.NewFakeClock(time.Now())
	tracker := NewTrackerWithClock(30*time.Second, 10, fakeClock)

	state, found := tracker.GetState(testGVR)
	if found {
		t.Error("expected state to not be found")
	}
	if state.Status != "" {
		t.Errorf("expected empty status, got %s", state.Status)
	}
}

func TestMarkSynced_UninitializedGVR(t *testing.T) {
	fakeClock := testingclock.NewFakeClock(time.Now())
	tracker := NewTrackerWithClock(30*time.Second, 10, fakeClock)

	// Should not panic
	tracker.MarkSynced(testGVR)

	_, found := tracker.GetState(testGVR)
	if found {
		t.Error("expected state to not be found after no-op MarkSynced")
	}
}

func TestRecordError_UninitializedGVR(t *testing.T) {
	fakeClock := testingclock.NewFakeClock(time.Now())
	tracker := NewTrackerWithClock(30*time.Second, 10, fakeClock)

	// Should not panic
	tracker.RecordError(testGVR, errors.New("test error"))

	_, found := tracker.GetState(testGVR)
	if found {
		t.Error("expected state to not be found after no-op RecordError")
	}
}

func TestMarkStopped(t *testing.T) {
	fakeClock := testingclock.NewFakeClock(time.Now())
	tracker := NewTrackerWithClock(30*time.Second, 10, fakeClock)

	tracker.InitState(testGVR)
	tracker.MarkSynced(testGVR)

	// Drain the Synced event
	<-tracker.StateEvents()

	state, found := tracker.GetState(testGVR)
	if !found {
		t.Fatal("expected state to be found")
	}
	if state.Status != StatusSynced {
		t.Errorf("expected status %s, got %s", StatusSynced, state.Status)
	}

	tracker.MarkStopped(testGVR)

	_, found = tracker.GetState(testGVR)
	if found {
		t.Error("expected state to not be found after MarkStopped")
	}

	// Check that Stopped event was emitted
	select {
	case event := <-tracker.StateEvents():
		if event.NewState.Status != StatusStopped {
			t.Errorf("expected event with status %s, got %s", StatusStopped, event.NewState.Status)
		}
	default:
		t.Error("expected state change event to be emitted")
	}
}

func TestMarkStopped_NonExistent(t *testing.T) {
	fakeClock := testingclock.NewFakeClock(time.Now())
	tracker := NewTrackerWithClock(30*time.Second, 10, fakeClock)

	// Should not panic
	tracker.MarkStopped(testGVR)
}

func TestRemoveState(t *testing.T) {
	fakeClock := testingclock.NewFakeClock(time.Now())
	tracker := NewTrackerWithClock(30*time.Second, 10, fakeClock)

	tracker.InitState(testGVR)
	tracker.MarkSynced(testGVR)

	// Drain any events from InitState/MarkSynced
	for len(tracker.StateEvents()) > 0 {
		<-tracker.StateEvents()
	}

	tracker.RemoveState(testGVR)

	_, found := tracker.GetState(testGVR)
	if found {
		t.Error("expected state to not be found after RemoveState")
	}

	// Verify no event was emitted
	select {
	case <-tracker.StateEvents():
		t.Error("expected no event to be emitted for RemoveState")
	default:
		// Good - no event
	}
}

func TestStateEvents_ReceivesTransitions(t *testing.T) {
	fakeClock := testingclock.NewFakeClock(time.Now())
	tracker := NewTrackerWithClock(30*time.Second, 10, fakeClock)

	tracker.InitState(testGVR)

	// Syncing -> Synced
	tracker.MarkSynced(testGVR)

	event := <-tracker.StateEvents()
	if event.OldState.Status != StatusSyncing {
		t.Errorf("expected old status %s, got %s", StatusSyncing, event.OldState.Status)
	}
	if event.NewState.Status != StatusSynced {
		t.Errorf("expected new status %s, got %s", StatusSynced, event.NewState.Status)
	}

	// Synced -> Degraded
	tracker.RecordError(testGVR, errors.New("error"))

	event = <-tracker.StateEvents()
	if event.OldState.Status != StatusSynced {
		t.Errorf("expected old status %s, got %s", StatusSynced, event.OldState.Status)
	}
	if event.NewState.Status != StatusDegraded {
		t.Errorf("expected new status %s, got %s", StatusDegraded, event.NewState.Status)
	}
}

func TestStateEvents_DropsWhenFull(t *testing.T) {
	fakeClock := testingclock.NewFakeClock(time.Now())
	// Buffer size of 1
	tracker := NewTrackerWithClock(30*time.Second, 1, fakeClock)

	tracker.InitState(testGVR)
	tracker.MarkSynced(testGVR) // First event - fills buffer

	// Record multiple errors - these should be dropped since buffer is full
	tracker.RecordError(testGVR, errors.New("error 1"))
	tracker.RecordError(testGVR, errors.New("error 2"))
	tracker.RecordError(testGVR, errors.New("error 3"))

	// Should only get the first event
	event := <-tracker.StateEvents()
	if event.NewState.Status != StatusSynced {
		t.Errorf("expected first event status %s, got %s", StatusSynced, event.NewState.Status)
	}

	// Channel should be empty now (other events were dropped)
	select {
	case <-tracker.StateEvents():
		t.Error("expected no more events (should have been dropped)")
	default:
		// Good
	}
}

func TestShutdown(t *testing.T) {
	fakeClock := testingclock.NewFakeClock(time.Now())
	tracker := NewTrackerWithClock(30*time.Second, 10, fakeClock)

	// Create multiple GVRs with active recovery timers
	gvr1 := schema.GroupVersionResource{Group: "test", Version: "v1", Resource: "one"}
	gvr2 := schema.GroupVersionResource{Group: "test", Version: "v1", Resource: "two"}

	tracker.InitState(gvr1)
	tracker.InitState(gvr2)
	tracker.MarkSynced(gvr1)
	tracker.MarkSynced(gvr2)
	tracker.RecordError(gvr1, errors.New("error"))
	tracker.RecordError(gvr2, errors.New("error"))

	// Should not panic
	tracker.Shutdown()

	// States should still be readable
	state1, _ := tracker.GetState(gvr1)
	state2, _ := tracker.GetState(gvr2)
	if state1.Status != StatusDegraded {
		t.Errorf("expected status %s, got %s", StatusDegraded, state1.Status)
	}
	if state2.Status != StatusDegraded {
		t.Errorf("expected status %s, got %s", StatusDegraded, state2.Status)
	}
}

func TestMultipleGVRs(t *testing.T) {
	fakeClock := testingclock.NewFakeClock(time.Now())
	tracker := NewTrackerWithClock(30*time.Second, 10, fakeClock)

	gvr1 := schema.GroupVersionResource{Group: "test", Version: "v1", Resource: "one"}
	gvr2 := schema.GroupVersionResource{Group: "test", Version: "v1", Resource: "two"}

	tracker.InitState(gvr1)
	tracker.InitState(gvr2)

	tracker.MarkSynced(gvr1)
	// gvr2 stays in Syncing

	tracker.RecordError(gvr2, errors.New("error"))
	// gvr2 goes to SyncingError

	state1, _ := tracker.GetState(gvr1)
	state2, _ := tracker.GetState(gvr2)

	if state1.Status != StatusSynced {
		t.Errorf("gvr1: expected status %s, got %s", StatusSynced, state1.Status)
	}
	if state2.Status != StatusSyncingError {
		t.Errorf("gvr2: expected status %s, got %s", StatusSyncingError, state2.Status)
	}
}

func TestInitState_Idempotent(t *testing.T) {
	fakeClock := testingclock.NewFakeClock(time.Now())
	tracker := NewTrackerWithClock(30*time.Second, 10, fakeClock)

	tracker.InitState(testGVR)
	tracker.MarkSynced(testGVR)

	// InitState again should not reset the state
	tracker.InitState(testGVR)

	state, _ := tracker.GetState(testGVR)
	if state.Status != StatusSynced {
		t.Errorf("expected status %s after second InitState, got %s", StatusSynced, state.Status)
	}
}
