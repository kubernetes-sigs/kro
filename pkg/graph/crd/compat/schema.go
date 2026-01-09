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
	v1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// Compare compares two OpenAPIV3Schema objects and returns a compatibility report.
// It identifies breaking and non-breaking changes between the schemas.
func Compare(oldSchema, newSchema *v1.JSONSchemaProps) *Report {
	return compare("", oldSchema, newSchema)
}

// compare is the internal recursive implementation
func compare(path string, oldSchema, newSchema *v1.JSONSchemaProps) *Report {
	result := &Report{
		BreakingChanges:    []Change{},
		NonBreakingChanges: []Change{},
	}

	// Guard against nil schemas
	if oldSchema == nil && newSchema == nil {
		return result
	}
	if oldSchema == nil {
		// Schema added where none existed - non-breaking
		result.AddNonBreakingChange(path, PropertyAdded, "", "")
		return result
	}
	if newSchema == nil {
		// Schema removed - breaking
		result.AddBreakingChange(path, PropertyRemoved, "", "")
		return result
	}

	// description changes are non-breaking
	if oldSchema.Description != newSchema.Description {
		result.AddNonBreakingChange(
			path+".description",
			DescriptionChanged,
			"",
			"",
		)
	}

	// type changes are breaking
	if oldSchema.Type != newSchema.Type {
		result.AddBreakingChange(
			path+".type",
			TypeChanged,
			oldSchema.Type,
			newSchema.Type,
		)
		// return early here to avoid further comparisons...
		return result
	}

	// pattern changes
	if oldSchema.Pattern != newSchema.Pattern {
		switch {
		case oldSchema.Pattern == "" && newSchema.Pattern != "":
			// Adding a pattern where none existed = breaking (restricts values)
			result.AddBreakingChange(path+".pattern", PatternAdded, "", newSchema.Pattern)
		case oldSchema.Pattern != "" && newSchema.Pattern == "":
			// Removing a pattern = non-breaking (relaxes constraint)
			result.AddNonBreakingChange(path+".pattern", PatternRemoved, oldSchema.Pattern, "")
		default:
			// Changing a pattern = breaking
			result.AddBreakingChange(path+".pattern", PatternChanged, oldSchema.Pattern, newSchema.Pattern)
		}
	}

	// Compare properties
	compareProperties(path, oldSchema, newSchema, result)

	// Check required fields
	compareRequiredFields(path, oldSchema, newSchema, result)

	// Check enum values
	compareEnumValues(path, oldSchema, newSchema, result)

	// For arrays, check items schema
	compareArrayItems(path, oldSchema, newSchema, result)

	return result
}

// compareProperties checks for added, removed, or changed properties
func compareProperties(path string, oldSchema, newSchema *v1.JSONSchemaProps, result *Report) {
	// First, check for removed properties (breaking changes)
	for propName, oldProp := range oldSchema.Properties {
		propPath := path + ".properties." + propName

		// check if property still exists
		newProp, exists := newSchema.Properties[propName]
		if !exists {
			// property was removed - breaking change
			result.AddBreakingChange(propPath, PropertyRemoved, "", "")
			continue
		}

		// property exists in both schemas - compare them recursively
		propResult := compare(propPath, &oldProp, &newProp)
		result.BreakingChanges = append(result.BreakingChanges, propResult.BreakingChanges...)
		result.NonBreakingChanges = append(result.NonBreakingChanges, propResult.NonBreakingChanges...)
	}

	// Then check for added properties. Now things get a bit more spicy.
	// A new property can be required or optional, and can have a default value.
	// Depending on these factors, it can be a breaking or non-breaking change.
	//
	// In general the rules are:
	// - Adding a required property without a default value is a breaking change
	// - Adding a required property with a default value is a non-breaking change
	// - Adding an optional property is a non-breaking change

	newRequiredSet := toStringSet(newSchema.Required)

	for propName, newProp := range newSchema.Properties {
		if _, exists := oldSchema.Properties[propName]; !exists {
			propPath := path + ".properties." + propName

			// check if property is required but has a default (non-breaking)
			// or required without default (breaking)
			hasDefault := newProp.Default != nil && len(newProp.Default.Raw) > 0

			if newRequiredSet[propName] && !hasDefault {
				// property is required and has no default - breaking change
				result.AddBreakingChange(propPath, PropertyAdded, "required=false", "required=true")
			} else {
				// property is optional or has default - non-breaking change
				result.AddNonBreakingChange(propPath, PropertyAdded, "", "")
			}
		}
	}
}

// compareRequiredFields checks for changes to required fields, it only considers
// existing properties, since new properties are handled in compareProperties.
func compareRequiredFields(path string, oldSchema, newSchema *v1.JSONSchemaProps, result *Report) {
	// Use length checks instead of nil checks
	if len(oldSchema.Required) == 0 && len(newSchema.Required) == 0 {
		return
	}

	// Convert to sets for efficient comparison
	oldRequiredSet := toStringSet(oldSchema.Required)
	newRequiredSet := toStringSet(newSchema.Required)

	// Make a set of all existing property names (not newly added ones)
	existingProps := make(map[string]bool)
	for propName := range oldSchema.Properties {
		existingProps[propName] = true
	}

	// Check for newly required fields ONLY for existing properties (breaking)
	for req := range newRequiredSet {
		// Only consider requirements for properties that already existed
		if existingProps[req] && !oldRequiredSet[req] {
			result.AddBreakingChange(path+".required", RequiredAdded, "", req)
		}
	}

	// Check for removed required fields (non-breaking)
	for req := range oldRequiredSet {
		if !newRequiredSet[req] {
			result.AddNonBreakingChange(path+".required", RequiredRemoved, req, "")
		}
	}
}

// compareEnumValues checks for changes to enum values
func compareEnumValues(path string, oldSchema, newSchema *v1.JSONSchemaProps, result *Report) {
	// Use length checks instead of nil checks
	if len(oldSchema.Enum) == 0 || len(newSchema.Enum) == 0 {
		return
	}

	oldEnumSet := toJsonValueSet(oldSchema.Enum)
	newEnumSet := toJsonValueSet(newSchema.Enum)

	// Check for removed enum values (breaking)
	for val := range oldEnumSet {
		if !newEnumSet[val] {
			result.AddBreakingChange(path+".enum", EnumRestricted, val, "")
		}
	}

	// Check for added enum values (non-breaking)
	for val := range newEnumSet {
		if !oldEnumSet[val] {
			result.AddNonBreakingChange(path+".enum", EnumExpanded, "", val)
		}
	}
}

// compareArrayItems checks array item schemas recursively
func compareArrayItems(path string, oldSchema, newSchema *v1.JSONSchemaProps, result *Report) {
	if oldSchema.Type == "array" && newSchema.Type == "array" {
		// Use safer existence checks
		oldHasItems := oldSchema.Items != nil && oldSchema.Items.Schema != nil
		newHasItems := newSchema.Items != nil && newSchema.Items.Schema != nil

		if oldHasItems && newHasItems {
			itemsResult := compare(path+".items", oldSchema.Items.Schema, newSchema.Items.Schema)
			result.BreakingChanges = append(result.BreakingChanges, itemsResult.BreakingChanges...)
			result.NonBreakingChanges = append(result.NonBreakingChanges, itemsResult.NonBreakingChanges...)
		} else if oldHasItems && !newHasItems {
			// Items schema was removed - breaking
			result.AddBreakingChange(path+".items", PropertyRemoved, "", "")
		} else if !oldHasItems && newHasItems {
			// Items schema was added - non-breaking
			result.AddNonBreakingChange(path+".items", PropertyAdded, "", "")
		}
	}
}

// toStringSet converts a string slice to a map for O(1) lookups
func toStringSet(slice []string) map[string]bool {
	set := make(map[string]bool, len(slice))
	for _, item := range slice {
		set[item] = true
	}
	return set
}

// toJsonValueSet converts JSON values to strings for comparison
func toJsonValueSet(values []v1.JSON) map[string]bool {
	set := make(map[string]bool, len(values))
	for _, val := range values {
		set[string(val.Raw)] = true
	}
	return set
}
