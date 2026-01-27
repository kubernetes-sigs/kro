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
	"strings"
)

// ChangeType represents the type of schema change
type ChangeType string

const (
	// Breaking change types
	PropertyRemoved ChangeType = "PROPERTY_REMOVED"
	TypeChanged     ChangeType = "TYPE_CHANGED"
	RequiredAdded   ChangeType = "REQUIRED_ADDED"
	EnumRestricted  ChangeType = "ENUM_RESTRICTED"
	PatternChanged  ChangeType = "PATTERN_CHANGED"
	PatternAdded    ChangeType = "PATTERN_ADDED"

	// Non-breaking change types
	PropertyAdded      ChangeType = "PROPERTY_ADDED"
	DescriptionChanged ChangeType = "DESCRIPTION_CHANGED"
	DefaultChanged     ChangeType = "DEFAULT_CHANGED"
	RequiredRemoved    ChangeType = "REQUIRED_REMOVED"
	EnumExpanded       ChangeType = "ENUM_EXPANDED"
	PatternRemoved     ChangeType = "PATTERN_REMOVED"
)

// Change represents a single schema change
type Change struct {
	// Path is the JSON path to the changed property
	Path string
	// ChangeType is the type of change
	ChangeType ChangeType
	// OldValue is the string representation of the old value (if applicable)
	OldValue string
	// NewValue is the string representation of the new value (if applicable)
	NewValue string
}

// Report contains the full analysis of schema differences
type Report struct {
	// BreakingChanges are changes that break backward compatibility
	BreakingChanges []Change
	// NonBreakingChanges are changes that maintain backward compatibility
	NonBreakingChanges []Change
}

// IsCompatible returns true if no breaking changes were detected
func (r *Report) IsCompatible() bool {
	return len(r.BreakingChanges) == 0
}

// HasBreakingChanges returns true if breaking changes were detected
func (r *Report) HasBreakingChanges() bool {
	return len(r.BreakingChanges) > 0
}

// HasChanges returns true if any changes were detected
func (r *Report) HasChanges() bool {
	return len(r.BreakingChanges) > 0 || len(r.NonBreakingChanges) > 0
}

const maxBreakingChangesSummary = 3

// SummarizeBreakingChanges returns a user-friendly summary of breaking changes
func (r *Report) String() string {
	if !r.HasBreakingChanges() {
		return "no breaking changes"
	}

	changeDescs := make([]string, 0, maxBreakingChangesSummary)

	for i, change := range r.BreakingChanges {
		// Cut off the summary if there are too many breaking changes
		if i >= maxBreakingChangesSummary {
			remaining := len(r.BreakingChanges) - i
			if remaining > 0 {
				changeDescs = append(changeDescs, fmt.Sprintf("and %d more changes", remaining))
			}
			break
		}
		changeDescs = append(changeDescs, change.Description())
	}

	return strings.Join(changeDescs, "; ")
}

// AddBreakingChange adds a breaking change to the result with automatically generated description
func (r *Report) AddBreakingChange(path string, changeType ChangeType, oldValue, newValue string) {
	r.BreakingChanges = append(r.BreakingChanges, Change{
		Path:       path,
		ChangeType: changeType,
		OldValue:   oldValue,
		NewValue:   newValue,
	})
}

// AddNonBreakingChange adds a non-breaking change to the result with automatically generated description
func (r *Report) AddNonBreakingChange(path string, changeType ChangeType, oldValue, newValue string) {
	r.NonBreakingChanges = append(r.NonBreakingChanges, Change{
		Path:       path,
		ChangeType: changeType,
		OldValue:   oldValue,
		NewValue:   newValue,
	})
}

// lastPathComponent extracts the last component from a JSON path
func lastPathComponent(path string) string {
	parts := strings.Split(path, ".")
	if len(parts) == 0 {
		return path
	}
	return parts[len(parts)-1]
}

// Description generates a human-readable description based on the change type
func (c Change) Description() string {
	propName := lastPathComponent(c.Path)

	switch c.ChangeType {
	case PropertyRemoved:
		return fmt.Sprintf("Property %s was removed", propName)
	case PropertyAdded:
		if c.NewValue == "required" {
			return fmt.Sprintf("Required property %s was added", propName)
		}
		return fmt.Sprintf("Optional property %s was added", propName)
	case TypeChanged:
		return fmt.Sprintf("Type changed from %s to %s", c.OldValue, c.NewValue)
	case RequiredAdded:
		return fmt.Sprintf("Field %s is newly required", c.NewValue)
	case RequiredRemoved:
		return fmt.Sprintf("Field %s is no longer required", c.OldValue)
	case EnumRestricted:
		return fmt.Sprintf("Enum value %s was removed", c.OldValue)
	case EnumExpanded:
		return fmt.Sprintf("Enum value %s was added", c.NewValue)
	case PatternChanged:
		return fmt.Sprintf("Validation pattern changed from %s to %s", c.OldValue, c.NewValue)
	case PatternAdded:
		return fmt.Sprintf("Validation pattern %s was added", c.NewValue)
	case PatternRemoved:
		return fmt.Sprintf("Validation pattern %s was removed", c.OldValue)
	case DescriptionChanged:
		return "Description field was changed"
	case DefaultChanged:
		return "Default value was changed"
	default:
		return fmt.Sprintf("Unknown change to %s", c.Path)
	}
}
