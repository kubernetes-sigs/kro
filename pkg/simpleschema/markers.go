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

package simpleschema

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/utils/ptr"
)

// ErrUnknownMarker is returned when an unrecognized marker is encountered.
var ErrUnknownMarker = errors.New("unknown marker")

// This package is used find extra markers in the NeoCRD schema that maps to
// something in OpenAPI schema. For example, the `required` marker in the NeoCRD
// schema maps to the `required` field in the OpenAPI schema, and the `description`
// marker in the NeoCRD schema maps to the `description` field in the OpenAPI schema.
//
// NeoCRDs typically expect the markers to be in the format `marker=value`. For
// example, `required=true` or `description="The name of the resource"`. The `Marker`
// struct is used to represent these markers.
//
// Example:
//
// variables:
//
//	spec:
//	  name: string | required=true description="The name of the resource"
//	  count: int | default=10 description="Some random number"

// MarkerType represents the type of marker that is found in the NeoCRD schema.
type MarkerType string

// OpenAPI schema type constants.
const (
	schemaTypeString  = "string"
	schemaTypeInteger = "integer"
	schemaTypeNumber  = "number"
	schemaTypeBoolean = "boolean"
	schemaTypeArray   = "array"
)

const (
	// MarkerTypeRequired represents the `required` marker.
	MarkerTypeRequired MarkerType = "required"
	// MarkerTypeDefault represents the `default` marker.
	MarkerTypeDefault MarkerType = "default"
	// MarkerTypeDescription represents the `description` marker.
	MarkerTypeDescription MarkerType = "description"
	// MarkerTypeMinimum represents the `minimum` marker.
	MarkerTypeMinimum MarkerType = "minimum"
	// MarkerTypeMaximum represents the `maximum` marker.
	MarkerTypeMaximum MarkerType = "maximum"
	// MarkerTypeValidation represents the `validation` marker.
	MarkerTypeValidation MarkerType = "validation"
	// MarkerTypeEnum represents the `enum` marker.
	MarkerTypeEnum MarkerType = "enum"
	// MarkerTypeImmutable represents the `immutable` marker.
	MarkerTypeImmutable MarkerType = "immutable"
	// MarkerTypePattern represents the `pattern` marker.
	MarkerTypePattern MarkerType = "pattern"
	// MarkerTypeUniqueItems represents the `uniqueItems` marker.
	MarkerTypeUniqueItems MarkerType = "uniqueItems"
	// MarkerTypeMinLength represents the `minLength` marker.
	MarkerTypeMinLength MarkerType = "minLength"
	// MarkerTypeMaxLength represents the `maxLength` marker.
	MarkerTypeMaxLength MarkerType = "maxLength"
	// MarkerTypeMinItems represents the `minItems` marker.
	MarkerTypeMinItems MarkerType = "minItems"
	// MarkerTypeMaxItems represents the `maxItems` marker.
	MarkerTypeMaxItems MarkerType = "maxItems"
)

func markerTypeFromString(s string) (MarkerType, error) {
	switch MarkerType(s) {
	case MarkerTypeRequired, MarkerTypeDefault, MarkerTypeDescription,
		MarkerTypeMinimum, MarkerTypeMaximum, MarkerTypeValidation, MarkerTypeEnum, MarkerTypeImmutable,
		MarkerTypePattern, MarkerTypeUniqueItems, MarkerTypeMinLength, MarkerTypeMaxLength, MarkerTypeMinItems,
		MarkerTypeMaxItems:
		return MarkerType(s), nil
	default:
		return "", fmt.Errorf("%w: %s", ErrUnknownMarker, s)
	}
}

// Marker represents a marker found in the NeoCRD schema.
type Marker struct {
	MarkerType MarkerType
	Key        string
	Value      string
}

// ParseMarkers parses a string of markers and returns a slice of Marker structs.
// The marker string should be in the format `marker=value`.
func ParseMarkers(markers string) ([]*Marker, error) {
	var result []*Marker
	var currentMarker *Marker
	var inQuotes bool
	var bracketCount int
	var buffer strings.Builder
	var escaped bool

	for _, char := range markers {
		switch {
		case char == '=' && currentMarker == nil && !inQuotes && bracketCount == 0:
			key := strings.TrimSpace(buffer.String())
			if key == "" {
				return nil, fmt.Errorf("empty marker key")
			}
			markerType, err := markerTypeFromString(key)
			if err != nil {
				return nil, fmt.Errorf("invalid marker key %q: %w", key, err)
			}
			currentMarker = &Marker{MarkerType: markerType, Key: key}
			buffer.Reset()
		case char == '"' && !escaped:
			inQuotes = !inQuotes
			buffer.WriteRune(char)
		case char == '\\' && inQuotes && !escaped:
			escaped = true
			buffer.WriteRune(char)
		case (char == '{' || char == '[') && !inQuotes:
			bracketCount++
			buffer.WriteRune(char)
		case (char == '}' || char == ']') && !inQuotes:
			bracketCount--
			buffer.WriteRune(char)
			if bracketCount < 0 {
				return nil, fmt.Errorf("unmatched closing bracket/brace")
			}
		case unicode.IsSpace(char) && !inQuotes && bracketCount == 0:
			if currentMarker != nil {
				currentMarker.Value = processValue(buffer.String())
				result = append(result, currentMarker)
				currentMarker = nil
				buffer.Reset()
			}
		default:
			if escaped && inQuotes {
				escaped = false
			}
			buffer.WriteRune(char)
		}
	}

	if currentMarker != nil {
		currentMarker.Value = processValue(buffer.String())
		result = append(result, currentMarker)
	}

	if inQuotes {
		return nil, fmt.Errorf("unclosed quote")
	}
	if bracketCount > 0 {
		return nil, fmt.Errorf("unclosed bracket/brace")
	}

	return result, nil
}
func processValue(value string) string {
	if strings.HasPrefix(value, "\"") && strings.HasSuffix(value, "\"") {
		// remove surrounding quotes and unescape the string
		unquoted := value[1 : len(value)-1]
		return unescapeString(unquoted)
	}
	return strings.TrimSpace(value)
}

// unescapeString unescapes a string that is surrounded by quotes.
// For example `\"foo\"` becomes `foo`
func unescapeString(s string) string {
	// i heard a few of people say strings.Builder isn't the best choice for this
	// but i don't know what is a better choice :shrung:
	var result strings.Builder
	escaped := false
	for _, char := range s {
		// If the character is escaped, write it to the buffer and reset the escaped
		// flag. If the character is a backslash, set the escaped flag to true. Otherwise,
		// write the character to the buffer.
		if escaped {
			if char != '"' && char != '\\' {
				result.WriteRune('\\')
			}
			result.WriteRune(char)
			escaped = false
		} else if char == '\\' {
			escaped = true
		} else {
			// If the character is not escaped, write it to the buffer
			result.WriteRune(char)
		}
	}
	return result.String()
}

// applyMarkers applies markers to a schema.
func applyMarkers(schema *extv1.JSONSchemaProps, markers []*Marker, key string, parentSchema *extv1.JSONSchemaProps) error {
	for _, marker := range markers {
		if err := applyMarker(schema, marker, key, parentSchema); err != nil {
			return err
		}
	}
	return nil
}

// applyMarker applies a single marker to a schema.
//
//nolint:gocyclo // switch over marker types is inherently complex but straightforward
func applyMarker(schema *extv1.JSONSchemaProps, marker *Marker, key string, parentSchema *extv1.JSONSchemaProps) error {
	switch marker.MarkerType {
	case MarkerTypeRequired:
		return applyRequiredMarker(marker, key, parentSchema)
	case MarkerTypeDefault:
		applyDefaultMarker(schema, marker)
	case MarkerTypeDescription:
		schema.Description = marker.Value
	case MarkerTypeMinimum:
		return applyMinimumMarker(schema, marker)
	case MarkerTypeMaximum:
		return applyMaximumMarker(schema, marker)
	case MarkerTypeValidation:
		return applyValidationMarker(schema, marker)
	case MarkerTypeImmutable:
		return applyImmutableMarker(schema, marker)
	case MarkerTypeEnum:
		return applyEnumMarker(schema, marker)
	case MarkerTypeMinLength:
		return applyMinLengthMarker(schema, marker)
	case MarkerTypeMaxLength:
		return applyMaxLengthMarker(schema, marker)
	case MarkerTypePattern:
		return applyPatternMarker(schema, marker)
	case MarkerTypeUniqueItems:
		return applyUniqueItemsMarker(schema, marker)
	case MarkerTypeMinItems:
		return applyMinItemsMarker(schema, marker)
	case MarkerTypeMaxItems:
		return applyMaxItemsMarker(schema, marker)
	default:
		return fmt.Errorf("%w: %s", ErrUnknownMarker, marker.MarkerType)
	}
	return nil
}

func applyRequiredMarker(marker *Marker, key string, parentSchema *extv1.JSONSchemaProps) error {
	isRequired, err := strconv.ParseBool(marker.Value)
	if err != nil {
		return fmt.Errorf("failed to parse required marker value: %w", err)
	}
	if isRequired {
		parentSchema.Required = append(parentSchema.Required, key)
	}
	return nil
}

func applyDefaultMarker(schema *extv1.JSONSchemaProps, marker *Marker) {
	var defaultValue []byte
	switch schema.Type {
	case schemaTypeString:
		defaultValue = []byte(fmt.Sprintf("\"%s\"", marker.Value))
	case schemaTypeInteger, schemaTypeNumber, schemaTypeBoolean:
		defaultValue = []byte(marker.Value)
	default:
		defaultValue = []byte(marker.Value)
	}
	schema.Default = &extv1.JSON{Raw: defaultValue}
}

func applyMinimumMarker(schema *extv1.JSONSchemaProps, marker *Marker) error {
	val, err := strconv.ParseFloat(marker.Value, 64)
	if err != nil {
		return fmt.Errorf("failed to parse minimum enum value: %w", err)
	}
	schema.Minimum = &val
	return nil
}

func applyMaximumMarker(schema *extv1.JSONSchemaProps, marker *Marker) error {
	val, err := strconv.ParseFloat(marker.Value, 64)
	if err != nil {
		return fmt.Errorf("failed to parse maximum enum value: %w", err)
	}
	schema.Maximum = &val
	return nil
}

func applyValidationMarker(schema *extv1.JSONSchemaProps, marker *Marker) error {
	if strings.TrimSpace(marker.Value) == "" {
		return fmt.Errorf("validation failed")
	}
	schema.XValidations = []extv1.ValidationRule{
		{Rule: marker.Value, Message: "validation failed"},
	}
	return nil
}

func applyImmutableMarker(schema *extv1.JSONSchemaProps, marker *Marker) error {
	isImmutable, err := strconv.ParseBool(marker.Value)
	if err != nil {
		return fmt.Errorf("failed to parse immutable marker value: %w", err)
	}
	if isImmutable {
		schema.XValidations = append(schema.XValidations, extv1.ValidationRule{
			Rule:    "self == oldSelf",
			Message: "field is immutable",
		})
	}
	return nil
}

func applyEnumMarker(schema *extv1.JSONSchemaProps, marker *Marker) error {
	enumValues := strings.Split(marker.Value, ",")
	enumJSONValues := make([]extv1.JSON, 0, len(enumValues))
	for _, val := range enumValues {
		val = strings.TrimSpace(val)
		if val == "" {
			return fmt.Errorf("empty enum values are not allowed")
		}
		var rawValue []byte
		switch schema.Type {
		case schemaTypeString:
			rawValue = []byte(fmt.Sprintf("%q", val))
		case schemaTypeInteger:
			if _, err := strconv.ParseInt(val, 10, 64); err != nil {
				return fmt.Errorf("failed to parse integer enum value: %w", err)
			}
			rawValue = []byte(val)
		default:
			return fmt.Errorf("enum values only supported for string and integer types, got type: %s", schema.Type)
		}
		enumJSONValues = append(enumJSONValues, extv1.JSON{Raw: rawValue})
	}
	if len(enumJSONValues) > 0 {
		schema.Enum = enumJSONValues
	}
	return nil
}

func applyMinLengthMarker(schema *extv1.JSONSchemaProps, marker *Marker) error {
	if schema.Type != schemaTypeString {
		return fmt.Errorf("minLength marker is only valid for string types, got type: %s", schema.Type)
	}
	val, err := strconv.ParseInt(marker.Value, 10, 64)
	if err != nil {
		return fmt.Errorf("failed to parse minLength value: %w", err)
	}
	schema.MinLength = &val
	return nil
}

func applyMaxLengthMarker(schema *extv1.JSONSchemaProps, marker *Marker) error {
	if schema.Type != schemaTypeString {
		return fmt.Errorf("maxLength marker is only valid for string types, got type: %s", schema.Type)
	}
	val, err := strconv.ParseInt(marker.Value, 10, 64)
	if err != nil {
		return fmt.Errorf("failed to parse maxLength value: %w", err)
	}
	schema.MaxLength = &val
	return nil
}

func applyPatternMarker(schema *extv1.JSONSchemaProps, marker *Marker) error {
	if marker.Value == "" {
		return fmt.Errorf("pattern marker value cannot be empty")
	}
	if schema.Type != schemaTypeString {
		return fmt.Errorf("pattern marker is only valid for string types, got type: %s", schema.Type)
	}
	if _, err := regexp.Compile(marker.Value); err != nil {
		return fmt.Errorf("invalid pattern regex: %w", err)
	}
	schema.Pattern = marker.Value
	return nil
}

func applyUniqueItemsMarker(schema *extv1.JSONSchemaProps, marker *Marker) error {
	isUnique, err := strconv.ParseBool(marker.Value)
	if err != nil {
		return fmt.Errorf("failed to parse uniqueItems marker value: %w", err)
	}
	if schema.Type != schemaTypeArray {
		return fmt.Errorf("uniqueItems marker is only valid for array types, got type: %s", schema.Type)
	}
	if isUnique {
		// Always set x-kubernetes-list-type to "set" when uniqueItems is true
		// https://kubernetes.io/docs/tasks/extend-kubernetes/custom-resources/custom-resource-definitions
		schema.XListType = ptr.To("set")
	}
	return nil
}

func applyMinItemsMarker(schema *extv1.JSONSchemaProps, marker *Marker) error {
	if schema.Type != schemaTypeArray {
		return fmt.Errorf("minItems marker is only valid for array types, got type: %s", schema.Type)
	}
	val, err := strconv.ParseInt(marker.Value, 10, 64)
	if err != nil {
		return fmt.Errorf("failed to parse minItems value: %w", err)
	}
	schema.MinItems = &val
	return nil
}

func applyMaxItemsMarker(schema *extv1.JSONSchemaProps, marker *Marker) error {
	if schema.Type != schemaTypeArray {
		return fmt.Errorf("maxItems marker is only valid for array types, got type: %s", schema.Type)
	}
	val, err := strconv.ParseInt(marker.Value, 10, 64)
	if err != nil {
		return fmt.Errorf("failed to parse maxItems value: %w", err)
	}
	schema.MaxItems = &val
	return nil
}
