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
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Status represents the current status of a watch.
type Status string

const (
	// StatusSyncing indicates the watch is starting up and syncing.
	StatusSyncing Status = "Syncing"
	// StatusSyncingError indicates the watch is syncing but has errors.
	StatusSyncingError Status = "SyncingError"
	// StatusSynced indicates the watch has synced and is healthy.
	StatusSynced Status = "Synced"
	// StatusDegraded indicates the watch is synced but experiencing errors.
	StatusDegraded Status = "Degraded"
	// StatusStopped indicates the watch has been stopped.
	StatusStopped Status = "Stopped"
)

// State represents the current state of a watch for a GVR.
type State struct {
	Status        Status
	Synced        bool
	HasError      bool
	LastError     error
	LastErrorTime time.Time
	ErrorCount    int
}

// StateChangeEvent represents a change in watch state for a GVR.
type StateChangeEvent struct {
	GVR      schema.GroupVersionResource
	OldState State
	NewState State
}
