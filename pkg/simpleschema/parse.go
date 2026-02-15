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
	"fmt"
	"strings"

	"github.com/kubernetes-sigs/kro/pkg/simpleschema/types"
)

// ParseTypeString parses the type portion of a field string (before any "|").
func ParseTypeString(s string) (types.Type, error) {
	typeStr := strings.SplitN(s, "|", 2)[0]
	typeStr = strings.TrimSpace(typeStr)
	if typeStr == "" {
		return nil, fmt.Errorf("empty type")
	}
	return parseType(typeStr)
}

// ParseField parses a field string like "[]string | required=true" into a Type and Markers.
func ParseField(s string) (types.Type, []*Marker, error) {
	typ, err := ParseTypeString(s)
	if err != nil {
		return nil, nil, err
	}

	var markers []*Marker
	if parts := strings.SplitN(s, "|", 2); len(parts) > 1 {
		markers, err = ParseMarkers(parts[1])
		if err != nil {
			return nil, nil, err
		}
	}

	return typ, markers, nil
}

// parseSpec parses a spec into a Type.
// Handles both type strings and nested object maps.
// Note: Markers are not parsed here; they're handled separately in buildSchema.
func parseSpec(spec interface{}) (types.Type, error) {
	switch val := spec.(type) {
	case string:
		return ParseTypeString(val)
	case map[string]interface{}:
		fields := make(map[string]types.Type)
		for name, field := range val {
			t, err := parseSpec(field)
			if err != nil {
				return nil, fmt.Errorf("field %s: %w", name, err)
			}
			fields[name] = t
		}
		return types.Struct{Fields: fields}, nil
	default:
		return nil, fmt.Errorf("unexpected type: %T", spec)
	}
}

// maxTypeNestingDepth limits recursion depth in parseType to prevent stack
// overflow from pathological input like "[][][][]...string".
const maxTypeNestingDepth = 16

// parseType parses a type string like "[]map[string]Person" into a Type.
func parseType(s string) (types.Type, error) {
	return parseTypeWithDepth(s, 0)
}

func parseTypeWithDepth(s string, depth int) (types.Type, error) {
	if depth > maxTypeNestingDepth {
		return nil, fmt.Errorf("type nesting too deep (max %d): %q", maxTypeNestingDepth, s)
	}

	s = strings.TrimSpace(s)

	if types.IsAtomic(s) {
		return types.Atomic(s), nil
	}
	if s == "object" {
		return types.Object{}, nil
	}
	if elem, ok := strings.CutPrefix(s, "[]"); ok {
		if elem == "" {
			return nil, fmt.Errorf("empty slice element type")
		}
		elemType, err := parseTypeWithDepth(elem, depth+1)
		if err != nil {
			return nil, err
		}
		return types.Slice{Elem: elemType}, nil
	}
	if val, err := parseMapString(s); err != nil {
		return nil, err
	} else if val != "" {
		valType, err := parseTypeWithDepth(val, depth+1)
		if err != nil {
			return nil, err
		}
		return types.Map{Value: valType}, nil
	}
	// Unrecognized type names are treated as references to custom types.
	// Validation happens later when Schema() is called with a Resolver.
	return types.Custom(s), nil
}

func parseMapString(s string) (string, error) {
	if !strings.HasPrefix(s, "map[") {
		return "", nil // not a map
	}
	rest, found := strings.CutPrefix(s, "map[string]")
	if !found {
		return "", fmt.Errorf("map key must be string")
	}
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return "", fmt.Errorf("map value type is required")
	}
	return rest, nil
}
