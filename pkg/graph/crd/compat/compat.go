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

package compat

import (
	"fmt"
	"strconv"

	v1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/equality"
)

// VersionsReport is the compatibility analysis of two CRD version lists.
type VersionsReport struct {
	// Versions holds one report per version name present on both sides.
	Versions map[string]*Report
	// Added lists version names only present in the desired CRD.
	Added []string
	// Removed lists version names only present in the existing CRD.
	Removed []string
}

// HasChanges reports whether the two version lists differ in any way.
func (r *VersionsReport) HasChanges() bool {
	if len(r.Added) > 0 || len(r.Removed) > 0 {
		return true
	}
	for _, report := range r.Versions {
		if report.HasChanges() {
			return true
		}
	}
	return false
}

// CompareVersions compares two CRD version lists name by name. Versions
// present on only one side are reported as added or removed; versions present
// on both are compared with CompareAcrossVersions' per-version logic.
//
// Comparison options, including WithConservativeComparison, are passed to
// Compare.
func CompareVersions(oldVersions, newVersions []v1.CustomResourceDefinitionVersion, options ...CompareOption) (*VersionsReport, error) {
	existing := make(map[string]v1.CustomResourceDefinitionVersion, len(oldVersions))
	for _, version := range oldVersions {
		existing[version.Name] = version
	}

	result := &VersionsReport{Versions: map[string]*Report{}}
	seen := make(map[string]struct{}, len(newVersions))
	for _, desired := range newVersions {
		seen[desired.Name] = struct{}{}
		old, ok := existing[desired.Name]
		if !ok {
			result.Added = append(result.Added, desired.Name)
			continue
		}
		report, err := compareSameVersion(old, desired, options...)
		if err != nil {
			return nil, err
		}
		result.Versions[desired.Name] = report
	}
	for _, old := range oldVersions {
		if _, ok := seen[old.Name]; !ok {
			result.Removed = append(result.Removed, old.Name)
		}
	}
	return result, nil
}

// compareSameVersion compares the two definitions of one version name.
//
// served true->false is recorded as a metadata change rather than a breaking
// one: whether retiring a version is allowed depends on whether anything still
// stores or uses it, which only the caller knows.
func compareSameVersion(oldVersion, newVersion v1.CustomResourceDefinitionVersion, options ...CompareOption) (*Report, error) {
	if oldVersion.Schema == nil || oldVersion.Schema.OpenAPIV3Schema == nil {
		return nil, fmt.Errorf("old version %s has no schema", oldVersion.Name)
	}
	if newVersion.Schema == nil || newVersion.Schema.OpenAPIV3Schema == nil {
		return nil, fmt.Errorf("new version %s has no schema", newVersion.Name)
	}

	report := Compare(oldVersion.Schema.OpenAPIV3Schema, newVersion.Schema.OpenAPIV3Schema, options...)

	// Status subresource removal would break status updates.
	oldHasStatus := oldVersion.Subresources != nil && oldVersion.Subresources.Status != nil
	newHasStatus := newVersion.Subresources != nil && newVersion.Subresources.Status != nil
	if oldHasStatus && !newHasStatus {
		report.AddBreakingChange("version.subresources.status", PropertyRemoved, "", "")
	}

	// Serving/storage/deprecation flags are not part of the schema. Record any
	// change so the CRD still gets patched.
	if oldVersion.Served != newVersion.Served {
		report.AddNonBreakingChange("version.served", VersionMetadataChanged,
			strconv.FormatBool(oldVersion.Served), strconv.FormatBool(newVersion.Served))
	}
	if oldVersion.Storage != newVersion.Storage {
		report.AddNonBreakingChange("version.storage", VersionMetadataChanged,
			strconv.FormatBool(oldVersion.Storage), strconv.FormatBool(newVersion.Storage))
	}
	if oldVersion.Deprecated != newVersion.Deprecated {
		report.AddNonBreakingChange("version.deprecated", VersionMetadataChanged,
			strconv.FormatBool(oldVersion.Deprecated), strconv.FormatBool(newVersion.Deprecated))
	}
	if !equality.Semantic.DeepEqual(oldVersion.DeprecationWarning, newVersion.DeprecationWarning) {
		report.AddNonBreakingChange("version.deprecationWarning", VersionMetadataChanged, "", "")
	}

	// Additional printer columns are not part of the schema, so they wouldn't
	// otherwise be caught above. Record any change so the CRD still gets updated.
	if !equality.Semantic.DeepEqual(oldVersion.AdditionalPrinterColumns, newVersion.AdditionalPrinterColumns) {
		report.AddNonBreakingChange("version.additionalPrinterColumns", AdditionalPrinterColumnsChanged, "", "")
	}

	return report, nil
}

// CompareAcrossVersions compares the schemas of two different versions of the
// same CRD. With conversion strategy None the API server serves one stored
// object under every version verbatim, so a spoke schema that rejects what the
// hub schema accepts breaks reads of existing objects.
func CompareAcrossVersions(from, to v1.CustomResourceDefinitionVersion, options ...CompareOption) (*Report, error) {
	if from.Schema == nil || from.Schema.OpenAPIV3Schema == nil {
		return nil, fmt.Errorf("version %s has no schema", from.Name)
	}
	if to.Schema == nil || to.Schema.OpenAPIV3Schema == nil {
		return nil, fmt.Errorf("version %s has no schema", to.Name)
	}
	return Compare(from.Schema.OpenAPIV3Schema, to.Schema.OpenAPIV3Schema, options...), nil
}
