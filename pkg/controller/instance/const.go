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
	"github.com/kubernetes-sigs/kro/pkg/applyset"
	"sigs.k8s.io/release-utils/version"
)

const (
	ResourceStatePending             = "PENDING"
	ResourceStateInProgress          = "IN_PROGRESS"
	ResourceStateDeleting            = "DELETING"
	ResourceStateSkipped             = "SKIPPED"
	ResourceStateError               = "ERROR"
	ResourceStateSynced              = "SYNCED"
	ResourceStateCreated             = "CREATED"
	ResourceStateDeleted             = "DELETED"
	ResourceStatePendingDeletion     = "PENDING_DELETION"
	ResourceStateWaitingForReadiness = "WAITING_FOR_READINESS"
	ResourceStateUpdating            = "UPDATING"

	FieldManagerForApplyset = "kro.run/applyset"
	FieldManagerForLabeler  = "kro.run/labeller"
)

var (
	KROTooling = applyset.ToolingID{Name: "kro", Version: version.GetVersionInfo().GitVersion}
)
