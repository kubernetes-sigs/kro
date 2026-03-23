// Copyright 2025 The Kube Resource Orchestrator Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package compat

import (
	"fmt"

	v1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// CompareVersions compares CRD versions and returns a compatibility report.
// This is a convenience wrapper that extracts schemas from version slices.
// It expects exactly one version in each slice.
func CompareVersions(oldVersions, newVersions []v1.CustomResourceDefinitionVersion) (*Report, error) {
	if len(oldVersions) != 1 || len(newVersions) != 1 {
		return nil, fmt.Errorf("expected exactly one version in each CRD, got %d old and %d new versions",
			len(oldVersions), len(newVersions))
	}

	oldVersion := oldVersions[0]
	newVersion := newVersions[0]

	// Check version names match
	if oldVersion.Name != newVersion.Name {
		return &Report{
			BreakingChanges: []Change{
				{
					Path:       "version",
					ChangeType: TypeChanged,
					OldValue:   oldVersion.Name,
					NewValue:   newVersion.Name,
				},
			},
		}, nil
	}

	// Verify schemas exist
	if oldVersion.Schema == nil || oldVersion.Schema.OpenAPIV3Schema == nil {
		return nil, fmt.Errorf("old version %s has no schema", oldVersion.Name)
	}

	if newVersion.Schema == nil || newVersion.Schema.OpenAPIV3Schema == nil {
		return nil, fmt.Errorf("new version %s has no schema", newVersion.Name)
	}

	// Compare schemas first
	report := Compare(oldVersion.Schema.OpenAPIV3Schema, newVersion.Schema.OpenAPIV3Schema)

	// Check version metadata changes that could break kro
	// Served going from true to false would break API access
	if oldVersion.Served && !newVersion.Served {
		report.AddBreakingChange("version.served", TypeChanged, "true", "false")
	}

	// Status subresource removal would break status updates
	oldHasStatus := oldVersion.Subresources != nil && oldVersion.Subresources.Status != nil
	newHasStatus := newVersion.Subresources != nil && newVersion.Subresources.Status != nil
	if oldHasStatus && !newHasStatus {
		report.AddBreakingChange("version.subresources.status", PropertyRemoved, "", "")
	}

	return report, nil
}
