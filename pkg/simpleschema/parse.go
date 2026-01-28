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

// ParseField parses a field string like "[]string | required=true" into a Type and Markers.
func ParseField(s string) (types.Type, []*Marker, error) {
	parts := strings.SplitN(s, "|", 2)
	typeStr := strings.TrimSpace(parts[0])
	if typeStr == "" {
		return nil, nil, fmt.Errorf("empty type")
	}

	typ, err := parseType(typeStr)
	if err != nil {
		return nil, nil, err
	}

	var markers []*Marker
	if len(parts) > 1 {
		markers, err = parseMarkers(parts[1])
		if err != nil {
			return nil, nil, err
		}
	}

	return typ, markers, nil
}

// parseSpec parses a spec into a Type.
// Handles both type strings and nested object maps.
// Note: Markers from string specs are discarded; they're handled separately in buildSchema.
func parseSpec(spec interface{}) (types.Type, error) {
	switch val := spec.(type) {
	case string:
		typ, _, err := ParseField(val)
		return typ, err
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

// parseType parses a type string like "[]map[string]Person" into a Type.
func parseType(s string) (types.Type, error) {
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
		elemType, err := parseType(elem)
		if err != nil {
			return nil, err
		}
		return types.Slice{Elem: elemType}, nil
	}
	if val, ok := parseMapString(s); ok {
		valType, err := parseType(val)
		if err != nil {
			return nil, err
		}
		return types.Map{Value: valType}, nil
	}
	if strings.HasPrefix(s, "map[") {
		return nil, fmt.Errorf("map key must be string")
	}
	return types.Custom(s), nil
}

func parseMapString(s string) (val string, ok bool) {
	rest, found := strings.CutPrefix(s, "map[string]")
	if !found {
		return "", false
	}
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return "", false
	}
	return rest, true
}
