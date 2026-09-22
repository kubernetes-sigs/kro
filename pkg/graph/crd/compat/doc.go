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

// Package compat provides functionality for comparing Kubernetes CustomResourceDefinition
// schemas and identifying breaking and non-breaking changes.
//
// The package analyzes OpenAPI v3 schemas from CRDs and generates detailed reports about
// compatibility issues. It's designed to prevent accidental schema changes that would break
// existing CRD instances.
//
// Breaking changes detected by default include:
//   - Property removal
//   - Type changes
//   - Adding required fields
//   - Restricting enum values
//   - Adding an enum constraint
//   - Pattern changes
//   - Removing nullable support
//   - Removing unknown-field preservation
//
// Non-breaking changes detected include:
//   - Adding new properties that are not required
//   - Expanding enum values
//   - Removing an enum constraint
//   - Changing descriptions
//   - Changing default values
//   - Removing optional fields from 'required' list
//   - Adding nullable support
//   - Adding unknown-field preservation
//
// ConservativeCRDComparison compares map value schemas recursively and
// treats unclassified schema changes as breaking. It also detects these
// additional breaking changes:
//   - Disabling additional properties
//   - Constraining additional properties
//   - Adding a format constraint
//   - Changing a format constraint
//   - Changing Kubernetes list or map topology
//   - Adding CEL validation rules
//   - Changing CEL validation rules
//
// Additional non-breaking changes detected when
// ConservativeCRDComparison is enabled:
//   - Enabling additional properties
//   - Relaxing additional properties
//   - Removing a format constraint
//   - Removing CEL validation rules
//
// Usage:
//
//	// Get existing and new CRD objects
//	oldCRD, newCRD := getCRDs()
//
//	// Compare schemas
//	report, err := crdcompat.CompareVersions(oldCRD.Spec.Versions, newCRD.Spec.Versions)
//	if err != nil {
//	    // Handle error
//	}
//
//	// Check for breaking changes
//	if report.HasBreakingChanges() {
//	    log.Fatalf("Breaking changes detected: %s", report)
//	}
package compat
