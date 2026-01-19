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

// Resource states track the lifecycle of individual resources during reconciliation.
const (
	// ResourceStateInProgress is the initial state when a resource starts being processed.
	// Set at the beginning of prepareResource before any operations.
	ResourceStateInProgress = "IN_PROGRESS"
	// ResourceStateDeleting means delete was called but the resource still exists.
	// Set when deletion is initiated but not yet confirmed.
	ResourceStateDeleting = "DELETING"
	// ResourceStateSkipped means the resource was not applied.
	// Set when includeWhen=false, dependency is skipped, or external ref during deletion.
	ResourceStateSkipped = "SKIPPED"
	// ResourceStateError means something failed.
	// Set when resolution, apply, or delete fails.
	ResourceStateError = "ERROR"
	// ResourceStateSynced means the resource was applied and is ready.
	// Set when apply succeeds and readyWhen is satisfied.
	ResourceStateSynced = "SYNCED"
	// ResourceStateDeleted means the resource no longer exists.
	// Set when deletion is confirmed or resource was not found.
	ResourceStateDeleted = "DELETED"
	// ResourceStateWaitingForReadiness means apply succeeded but readyWhen is not satisfied.
	// Set when apply succeeds but readyWhen evaluates to false.
	ResourceStateWaitingForReadiness = "WAITING_FOR_READINESS"

	// FieldManagerForLabeler is the field manager name used when applying labels.
	FieldManagerForLabeler = "kro.run/labeller"
)
