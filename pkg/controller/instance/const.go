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
