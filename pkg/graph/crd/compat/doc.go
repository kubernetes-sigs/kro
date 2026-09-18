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
//   - Pattern changes
//
// Non-breaking changes detected include:
//   - Adding new properties that are not required
//   - Expanding enum values
//   - Changing descriptions
//   - Changing default values
//   - Removing optional fields from 'required' list
//
// When the strict-crd-compatibility-checks feature gate is enabled, the
// comparator also checks map value schemas, CEL validation rules, nullable and
// format changes, unknown-field preservation, and Kubernetes list or map
// topology. It classifies safe relaxations such as adding nullable or
// unknown-field preservation and removing format constraints as non-breaking.
// Changes to schema facets without an explicit compatibility classification
// fail closed while the gate is enabled.
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
