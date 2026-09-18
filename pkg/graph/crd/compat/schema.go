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
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"

	v1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/equality"

	"github.com/kubernetes-sigs/kro/pkg/features"
)

// Compare compares two OpenAPIV3Schema objects and returns a compatibility report.
// It identifies breaking and non-breaking changes between the schemas.
func Compare(oldSchema, newSchema *v1.JSONSchemaProps) *Report {
	strictChecks := features.FeatureGate.Enabled(features.StrictCRDCompatibilityChecks)
	return compare("", oldSchema, newSchema, strictChecks)
}

// compare is the internal recursive implementation
func compare(path string, oldSchema, newSchema *v1.JSONSchemaProps, strictChecks bool) *Report {
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
			oldSchema.Description,
			newSchema.Description,
		)
	}

	// default value changes are non-breaking
	if !defaultsEqual(oldSchema.Default, newSchema.Default) {
		result.AddNonBreakingChange(
			path+".default",
			DefaultChanged,
			getDefaultValue(oldSchema.Default),
			getDefaultValue(newSchema.Default),
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

	// Compare numeric/length/items constraints
	compareConstraints(path, oldSchema, newSchema, result)

	// Compare properties
	compareProperties(path, oldSchema, newSchema, result, strictChecks)

	// Check required fields
	compareRequiredFields(path, oldSchema, newSchema, result)

	// Check enum values
	compareEnumValues(path, oldSchema, newSchema, result, strictChecks)

	// For arrays, check items schema
	compareArrayItems(path, oldSchema, newSchema, result, strictChecks)

	if strictChecks {
		// For maps, check the value schema.
		compareAdditionalProperties(path, oldSchema, newSchema, result, strictChecks)

		compareFormat(path, oldSchema, newSchema, result)
		compareNullable(path, oldSchema, newSchema, result)
		comparePreserveUnknownFields(path, oldSchema, newSchema, result)
		compareTopology(path, oldSchema, newSchema, result)
		compareValidationRules(path, oldSchema, newSchema, result)

		// Reject changes to schema facets that are not explicitly classified above.
		// This keeps newly added or currently unsupported validation keywords from
		// silently passing as compatible.
		compareUnclassifiedFields(path, oldSchema, newSchema, result)
	}

	return result
}

func appendReport(result, nested *Report) {
	result.BreakingChanges = append(result.BreakingChanges, nested.BreakingChanges...)
	result.NonBreakingChanges = append(result.NonBreakingChanges, nested.NonBreakingChanges...)
}

func getDefaultValue(val *v1.JSON) string {
	if val == nil {
		return ""
	}
	return string(val.Raw)
}

// compareProperties checks for added, removed, or changed properties
func compareProperties(
	path string,
	oldSchema, newSchema *v1.JSONSchemaProps,
	result *Report,
	strictChecks bool,
) {
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
		appendReport(result, compare(propPath, &oldProp, &newProp, strictChecks))
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

	// Check for required fields with default value removed.
	// If a field is required in both old and new schemas but its default value
	// was removed, new instances can no longer omit the field and rely on the
	// default being populated automatically.
	for req := range newRequiredSet {
		if !existingProps[req] || !oldRequiredSet[req] {
			continue
		}
		oldProp := oldSchema.Properties[req]
		newProp := newSchema.Properties[req]
		oldHasDefault := oldProp.Default != nil && len(oldProp.Default.Raw) > 0
		newHasDefault := newProp.Default != nil && len(newProp.Default.Raw) > 0
		if oldHasDefault && !newHasDefault {
			result.AddBreakingChange(path+".required", RequiredDefaultRemoved, req, "")
		}
	}
}

// compareEnumValues checks for changes to enum values
func compareEnumValues(
	path string,
	oldSchema, newSchema *v1.JSONSchemaProps,
	result *Report,
	strictChecks bool,
) {
	if len(oldSchema.Enum) == 0 && len(newSchema.Enum) == 0 {
		return
	}
	if len(oldSchema.Enum) == 0 {
		if !strictChecks {
			return
		}
		result.AddBreakingChange(
			path+".enum",
			EnumConstraintAdded,
			"",
			formatSchemaFieldValue(newSchema.Enum),
		)
		return
	}
	if len(newSchema.Enum) == 0 {
		if !strictChecks {
			return
		}
		result.AddNonBreakingChange(
			path+".enum",
			EnumConstraintRemoved,
			formatSchemaFieldValue(oldSchema.Enum),
			"",
		)
		return
	}

	oldEnumSet := toJSONValueSet(oldSchema.Enum)
	newEnumSet := toJSONValueSet(newSchema.Enum)

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
func compareArrayItems(
	path string,
	oldSchema, newSchema *v1.JSONSchemaProps,
	result *Report,
	strictChecks bool,
) {
	if oldSchema.Type == "array" && newSchema.Type == "array" {
		// Use safer existence checks
		oldHasItems := oldSchema.Items != nil && oldSchema.Items.Schema != nil
		newHasItems := newSchema.Items != nil && newSchema.Items.Schema != nil

		if oldHasItems && newHasItems {
			appendReport(result, compare(path+".items", oldSchema.Items.Schema, newSchema.Items.Schema, strictChecks))
		} else if oldHasItems && !newHasItems {
			// Items schema was removed - breaking
			result.AddBreakingChange(path+".items", PropertyRemoved, "", "")
		} else if !oldHasItems && newHasItems {
			// Items schema was added - non-breaking
			result.AddNonBreakingChange(path+".items", PropertyAdded, "", "")
		}
	}
}

// compareAdditionalProperties checks map value schemas recursively. We cannot
// safely classify changes between absent, boolean, and schema forms because
// their compatibility depends on both validation and structural-schema pruning.
// Fail closed for those changes instead of claiming they are always breaking.
func compareAdditionalProperties(
	path string,
	oldSchema, newSchema *v1.JSONSchemaProps,
	result *Report,
	strictChecks bool,
) {
	oldAdditional := oldSchema.AdditionalProperties
	newAdditional := newSchema.AdditionalProperties

	if equality.Semantic.DeepEqual(oldAdditional, newAdditional) {
		return
	}

	fieldPath := path + ".additionalProperties"
	oldHasSchema := oldAdditional != nil && oldAdditional.Schema != nil
	newHasSchema := newAdditional != nil && newAdditional.Schema != nil
	if oldHasSchema && newHasSchema {
		appendReport(result, compare(fieldPath, oldAdditional.Schema, newAdditional.Schema, strictChecks))
		return
	}

	result.AddBreakingChange(
		fieldPath,
		AdditionalPropertiesChanged,
		formatSchemaFieldValue(oldAdditional),
		formatSchemaFieldValue(newAdditional),
	)
}

func compareFormat(path string, oldSchema, newSchema *v1.JSONSchemaProps, result *Report) {
	if oldSchema.Format == newSchema.Format {
		return
	}

	fieldPath := path + ".format"
	if oldSchema.Format != "" && newSchema.Format == "" {
		result.AddNonBreakingChange(fieldPath, FormatRemoved, oldSchema.Format, "")
		return
	}

	result.AddBreakingChange(fieldPath, FormatChanged, oldSchema.Format, newSchema.Format)
}

func compareNullable(path string, oldSchema, newSchema *v1.JSONSchemaProps, result *Report) {
	if oldSchema.Nullable == newSchema.Nullable {
		return
	}

	fieldPath := path + ".nullable"
	if newSchema.Nullable {
		result.AddNonBreakingChange(fieldPath, NullableAdded, "false", "true")
		return
	}

	result.AddBreakingChange(fieldPath, NullableRemoved, "true", "false")
}

func comparePreserveUnknownFields(path string, oldSchema, newSchema *v1.JSONSchemaProps, result *Report) {
	oldPreserves := boolPointerValue(oldSchema.XPreserveUnknownFields)
	newPreserves := boolPointerValue(newSchema.XPreserveUnknownFields)
	if oldPreserves == newPreserves {
		return
	}

	fieldPath := path + ".x-kubernetes-preserve-unknown-fields"
	if newPreserves {
		result.AddNonBreakingChange(fieldPath, PreserveUnknownFieldsAdded, "false", "true")
		return
	}

	result.AddBreakingChange(fieldPath, PreserveUnknownFieldsRemoved, "true", "false")
}

func boolPointerValue(value *bool) bool {
	return value != nil && *value
}

func compareTopology(path string, oldSchema, newSchema *v1.JSONSchemaProps, result *Report) {
	fields := []struct {
		name string
		old  any
		new  any
	}{
		{"x-kubernetes-list-map-keys", oldSchema.XListMapKeys, newSchema.XListMapKeys},
		{"x-kubernetes-list-type", oldSchema.XListType, newSchema.XListType},
		{"x-kubernetes-map-type", oldSchema.XMapType, newSchema.XMapType},
	}

	for _, field := range fields {
		if equality.Semantic.DeepEqual(field.old, field.new) {
			continue
		}
		result.AddBreakingChange(
			path+"."+field.name,
			TopologyChanged,
			formatSchemaFieldValue(field.old),
			formatSchemaFieldValue(field.new),
		)
	}
}

func compareValidationRules(path string, oldSchema, newSchema *v1.JSONSchemaProps, result *Report) {
	if len(oldSchema.XValidations) == 0 && len(newSchema.XValidations) == 0 {
		return
	}
	if equality.Semantic.DeepEqual(oldSchema.XValidations, newSchema.XValidations) {
		return
	}

	result.AddBreakingChange(
		path+".x-kubernetes-validations",
		ValidationRulesChanged,
		formatSchemaFieldValue(oldSchema.XValidations),
		formatSchemaFieldValue(newSchema.XValidations),
	)
}

var classifiedSchemaFields = map[string]bool{
	"Description":            true,
	"Default":                true,
	"Type":                   true,
	"Format":                 true,
	"Pattern":                true,
	"Minimum":                true,
	"Maximum":                true,
	"MinLength":              true,
	"MaxLength":              true,
	"MinItems":               true,
	"MaxItems":               true,
	"Enum":                   true,
	"Properties":             true,
	"Required":               true,
	"Items":                  true,
	"AdditionalProperties":   true,
	"Nullable":               true,
	"XPreserveUnknownFields": true,
	"XListMapKeys":           true,
	"XListType":              true,
	"XMapType":               true,
	"XValidations":           true,
}

func compareUnclassifiedFields(path string, oldSchema, newSchema *v1.JSONSchemaProps, result *Report) {
	schemaType := reflect.TypeOf(*oldSchema)
	oldValue := reflect.ValueOf(*oldSchema)
	newValue := reflect.ValueOf(*newSchema)

	for i := range schemaType.NumField() {
		field := schemaType.Field(i)
		if classifiedSchemaFields[field.Name] {
			continue
		}

		oldField := oldValue.Field(i).Interface()
		newField := newValue.Field(i).Interface()
		if equality.Semantic.DeepEqual(oldField, newField) {
			continue
		}

		jsonName := strings.Split(field.Tag.Get("json"), ",")[0]
		if jsonName == "" || jsonName == "-" {
			jsonName = field.Name
		}
		result.AddBreakingChange(
			path+"."+jsonName,
			UnclassifiedSchemaChange,
			formatSchemaFieldValue(oldField),
			formatSchemaFieldValue(newField),
		)
	}
}

func formatSchemaFieldValue(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprint(value)
	}
	return string(encoded)
}

// defaultsEqual compares two JSON default values for equality.
// Two defaults are equal if they are both nil, or both non-nil with
// identical Raw byte content.
func defaultsEqual(a, b *v1.JSON) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return bytes.Equal(a.Raw, b.Raw)
}

// toStringSet converts a string slice to a map for O(1) lookups
func toStringSet(slice []string) map[string]bool {
	set := make(map[string]bool, len(slice))
	for _, item := range slice {
		set[item] = true
	}
	return set
}

// toJSONValueSet converts JSON values to strings for comparison
func toJSONValueSet(values []v1.JSON) map[string]bool {
	set := make(map[string]bool, len(values))
	for _, val := range values {
		set[string(val.Raw)] = true
	}
	return set
}

// compareConstraints checks for changes to numeric, length, and items constraints.
func compareConstraints(path string, oldSchema, newSchema *v1.JSONSchemaProps, result *Report) {
	compareFloatConstraint(path, "minimum", oldSchema.Minimum, newSchema.Minimum, true, result)
	compareFloatConstraint(path, "maximum", oldSchema.Maximum, newSchema.Maximum, false, result)
	compareIntConstraint(path, "minLength", oldSchema.MinLength, newSchema.MinLength, true, result)
	compareIntConstraint(path, "maxLength", oldSchema.MaxLength, newSchema.MaxLength, false, result)
	compareIntConstraint(path, "minItems", oldSchema.MinItems, newSchema.MinItems, true, result)
	compareIntConstraint(path, "maxItems", oldSchema.MaxItems, newSchema.MaxItems, false, result)
}

// compareFloatConstraint compares a float64 pointer constraint field.
// isLowerBound=true means this is a minimum-style constraint (increase = breaking).
// isLowerBound=false means this is a maximum-style constraint (decrease = breaking).
func compareFloatConstraint(path, name string, oldVal, newVal *float64, isLowerBound bool, result *Report) {
	if oldVal == nil && newVal == nil {
		return
	}

	fieldPath := path + "." + name
	addedType, removedType, tightenedType, relaxedType := constraintChangeTypes(name)

	if oldVal == nil && newVal != nil {
		result.AddBreakingChange(fieldPath, addedType, "", formatFloat(*newVal))
		return
	}
	if oldVal != nil && newVal == nil {
		result.AddNonBreakingChange(fieldPath, removedType, formatFloat(*oldVal), "")
		return
	}

	if *oldVal == *newVal {
		return
	}

	oldStr := formatFloat(*oldVal)
	newStr := formatFloat(*newVal)

	if isLowerBound {
		if *newVal > *oldVal {
			result.AddBreakingChange(fieldPath, tightenedType, oldStr, newStr)
		} else {
			result.AddNonBreakingChange(fieldPath, relaxedType, oldStr, newStr)
		}
	} else {
		if *newVal < *oldVal {
			result.AddBreakingChange(fieldPath, tightenedType, oldStr, newStr)
		} else {
			result.AddNonBreakingChange(fieldPath, relaxedType, oldStr, newStr)
		}
	}
}

// compareIntConstraint compares an int64 pointer constraint field.
// isLowerBound=true means this is a minimum-style constraint (increase = breaking).
// isLowerBound=false means this is a maximum-style constraint (decrease = breaking).
func compareIntConstraint(path, name string, oldVal, newVal *int64, isLowerBound bool, result *Report) {
	if oldVal == nil && newVal == nil {
		return
	}

	fieldPath := path + "." + name
	addedType, removedType, tightenedType, relaxedType := constraintChangeTypes(name)

	if oldVal == nil && newVal != nil {
		result.AddBreakingChange(fieldPath, addedType, "", strconv.FormatInt(*newVal, 10))
		return
	}
	if oldVal != nil && newVal == nil {
		result.AddNonBreakingChange(fieldPath, removedType, strconv.FormatInt(*oldVal, 10), "")
		return
	}

	if *oldVal == *newVal {
		return
	}

	oldStr := strconv.FormatInt(*oldVal, 10)
	newStr := strconv.FormatInt(*newVal, 10)

	if isLowerBound {
		if *newVal > *oldVal {
			result.AddBreakingChange(fieldPath, tightenedType, oldStr, newStr)
		} else {
			result.AddNonBreakingChange(fieldPath, relaxedType, oldStr, newStr)
		}
	} else {
		if *newVal < *oldVal {
			result.AddBreakingChange(fieldPath, tightenedType, oldStr, newStr)
		} else {
			result.AddNonBreakingChange(fieldPath, relaxedType, oldStr, newStr)
		}
	}
}

// constraintChangeTypes returns the (added, removed, tightened, relaxed) ChangeTypes for a named constraint.
func constraintChangeTypes(name string) (added, removed, tightened, relaxed ChangeType) {
	switch name {
	case "minimum":
		return MinimumAdded, MinimumRemoved, MinimumIncreased, MinimumDecreased
	case "maximum":
		return MaximumAdded, MaximumRemoved, MaximumDecreased, MaximumIncreased
	case "minLength":
		return MinLengthAdded, MinLengthRemoved, MinLengthIncreased, MinLengthDecreased
	case "maxLength":
		return MaxLengthAdded, MaxLengthRemoved, MaxLengthDecreased, MaxLengthIncreased
	case "minItems":
		return MinItemsAdded, MinItemsRemoved, MinItemsIncreased, MinItemsDecreased
	case "maxItems":
		return MaxItemsAdded, MaxItemsRemoved, MaxItemsDecreased, MaxItemsIncreased
	default:
		return ChangeType(name + "_ADDED"), ChangeType(name + "_REMOVED"), ChangeType(name + "_TIGHTENED"), ChangeType(name + "_RELAXED")
	}
}

func formatFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}
