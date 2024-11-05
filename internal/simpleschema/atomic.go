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
)

// AtomicType represents the type of an atomic value that can be used
// to define CRD fields.
//
// These atomic types can be used to define more complex types like maps,
// arrays, and structs.
type AtomicType string

const (
	// AtomicTypeBool represents a boolean value.
	AtomicTypeBool AtomicType = "boolean"
	// AtomicTypeInteger represents an integer value (64-bit signed integer)
	AtomicTypeInteger AtomicType = "integer"
	// AtomicTypeFloat represents a floating point value (64-bit float)
	AtomicTypeFloat AtomicType = "float"
	// AtomicTypeString represents a string value.
	AtomicTypeString AtomicType = "string"
)

func isAtomicType(s string) bool {
	switch AtomicType(s) {
	case AtomicTypeBool, AtomicTypeInteger, AtomicTypeFloat, AtomicTypeString:
		return true
	default:
		return false
	}
}

// CollectionType represents the type of a collection value that can be used
// to define CRD fields.
type CollectionType string

const (
	// CollectionTypeArray represents an array of values.
	CollectionTypeArray CollectionType = "array"
	// CollectionTypeMap represents a map of values.
	CollectionTypeMap CollectionType = "map"
)

// isCollectionType returns true if the given type is a collection type.
// NOTE(a-hilaly): we probably need a smarter way to detect collection types
// as this is a very naive implementation. For example, we could use a regex
// to detect the type. I wonder how does the Go compiler ast parses types...
func isCollectionType(s string) bool {
	if strings.HasPrefix(s, "[]") {
		return true
	}
	if strings.HasPrefix(s, "map") {
		return true
	}
	return false
}

func isMapType(s string) bool {
	return strings.HasPrefix(s, "map")
}

func isSliceType(s string) bool {
	return strings.HasPrefix(s, "[]")
}

// parseMapType parses a map type string and returns the key and value types.
func parseMapType(s string) (string, string, error) {
	if !strings.HasPrefix(s, "map[") {
		return "", "", fmt.Errorf("invalid map type: %s", s)
	}

	// remove the "map[" prefix
	s = s[4:]

	keyEndIndex := findMatchingBracket(s)
	if keyEndIndex == -1 {
		return "", "", fmt.Errorf("invalid map key type: %s", s)
	}

	keyType := s[:keyEndIndex]
	valueType := s[keyEndIndex+1:]

	valueType = strings.TrimSuffix(valueType, "]")
	if keyType == "" {
		return "", "", fmt.Errorf("empty map key type")
	}
	if valueType == "" {
		return "", "", fmt.Errorf("empty map value type")
	}

	return keyType, valueType, nil
}
func findMatchingBracket(s string) int {
	depth := 1
	for i, char := range s {
		switch char {
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	// no matching bracket found
	return -1
}

// parseSliceType parses a slice type string and returns the element type.
func parseSliceType(s string) (string, error) {
	if !strings.HasPrefix(s, "[]") {
		return "", fmt.Errorf("invalid slice type: %s", s)
	}

	// Remove the "[]" prefix.
	s = strings.TrimPrefix(s, "[]")
	if s == "" {
		return "", fmt.Errorf("empty slice type")
	}
	return s, nil
}
