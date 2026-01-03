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

// Package crdcompat provides functionality for comparing Kubernetes CustomResourceDefinition
// schemas and identifying breaking and non-breaking changes.
//
// The package analyzes OpenAPI v3 schemas from CRDs and generates detailed reports about
// compatibility issues. It's designed to prevent accidental schema changes that would break
// existing CRD instances.
//
// Breaking changes detected include:
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
// Usage:
//
//	// Get existing and new CRD objects
//	oldCRD, newCRD := getCRDs()
//
//	// Compare schemas
//	diffResult, err := crdcompat.DiffSchema(oldCRD.Spec.Versions, newCRD.Spec.Versions)
//	if err != nil {
//	    // Handle error
//	}
//
//	// Check for breaking changes
//	if diffResult.HasBreakingChanges() {
//	    summary := diffResult.Summary()
//	    log.Fatalf("Breaking changes detected: %s", summary)
//	}
package compat
