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

// Package applyset holds pure ApplySet-specification contract types shared
// across kro subsystems. It is a neutral leaf package with no dependencies on
// other kro packages, so both the controller and graph-engine subsystems can
// depend on it without creating a layering back-edge.
package applyset

import (
	"errors"
	"fmt"
)

// ApplysetPartOfLabel is the key of the label which indicates that the object is a member of an ApplySet.
// The value of the label MUST match the value of ApplySetParentIDLabel on the parent object.
const ApplysetPartOfLabel = "applyset.kubernetes.io/part-of"

// ErrApplySetConflict is returned when a resource already belongs to a different ApplySet.
// This indicates the resource is managed by another controller/instance and should not
// be overwritten without explicit action.
var ErrApplySetConflict = errors.New("resource belongs to a different ApplySet")

// ApplySetConflictError provides details about an ApplySet membership conflict.
type ApplySetConflictError struct {
	ResourceName      string
	ResourceNamespace string
	ResourceGVK       string
	CurrentApplySetID string
	DesiredApplySetID string
}

func (e *ApplySetConflictError) Error() string {
	if e.ResourceNamespace != "" {
		return fmt.Sprintf("%s: %s/%s (%s) belongs to ApplySet %q, cannot reassign to %q",
			ErrApplySetConflict, e.ResourceNamespace, e.ResourceName, e.ResourceGVK,
			e.CurrentApplySetID, e.DesiredApplySetID)
	}
	return fmt.Sprintf("%s: %s (%s) belongs to ApplySet %q, cannot reassign to %q",
		ErrApplySetConflict, e.ResourceName, e.ResourceGVK,
		e.CurrentApplySetID, e.DesiredApplySetID)
}

func (e *ApplySetConflictError) Unwrap() error {
	return ErrApplySetConflict
}
