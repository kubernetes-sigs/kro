// Copyright Amazon.com Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//	http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package simpleschema

import (
	"fmt"
	"strings"
	"unicode"
)

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

const (
	// MarkerTypeRequired represents the `required` marker.
	MarkerTypeRequired MarkerType = "required"
	// MarkerTypeDefault represents the `default` marker.
	MarkerTypeDefault MarkerType = "default"
	// MarkerTypeDescription represents the `description` marker.
	MarkerTypeDescription MarkerType = "description"
)

func markerTypeFromString(s string) (MarkerType, error) {
	switch MarkerType(s) {
	case MarkerTypeRequired, MarkerTypeDefault, MarkerTypeDescription:
		return MarkerType(s), nil
	default:
		return "", fmt.Errorf("unknown marker type: %s", s)
	}
}

// Marker represents a marker found in the NeoCRD schema.
type Marker struct {
	MarkerType MarkerType
	Key        string
	Value      string
}

// parseMarker parses a marker string and returns a `Marker` struct.
// The marker string should be in the format `marker=value`.
// parseMarkers parses a string of markers and returns a slice of Marker structs
func parseMarkers(markers string) ([]*Marker, error) {
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
				return nil, fmt.Errorf("invalid marker key '%s': %v", key, err)
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
