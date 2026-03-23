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

package v1alpha1

// InstanceState represents high-level reconciliation state for an instance.
type InstanceState string

const (
	// InstanceStateInProgress means reconciliation is ongoing.
	InstanceStateInProgress InstanceState = "IN_PROGRESS"
	// Deprecated: InstanceStateFailed is a legacy state kept for compatibility.
	InstanceStateFailed InstanceState = "FAILED"
	// InstanceStateActive means all nodes reached terminal success states.
	InstanceStateActive InstanceState = "ACTIVE"
	// InstanceStateDeleting means deletion workflow is running.
	InstanceStateDeleting InstanceState = "DELETING"
	// InstanceStateError means reconciliation hit an error.
	InstanceStateError InstanceState = "ERROR"
)

// NodeState tracks the lifecycle of individual nodes during reconciliation.
type NodeState string

const (
	NodeStateInProgress          NodeState = "IN_PROGRESS"
	NodeStateDeleting            NodeState = "DELETING"
	NodeStateSkipped             NodeState = "SKIPPED"
	NodeStateError               NodeState = "ERROR"
	NodeStateSynced              NodeState = "SYNCED"
	NodeStateDeleted             NodeState = "DELETED"
	NodeStateWaitingForReadiness NodeState = "WAITING_FOR_READINESS"
)
