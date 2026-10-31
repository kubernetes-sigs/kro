// Copyright 2026 The Kubernetes Authors.
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

package compiler

import "fmt"

// FieldPathSegment is one step of a parsed externalFields field path: a map
// key (Key), optionally followed by a [field=value] selector that picks one
// item out of an associative list by matching one of its fields (Match,
// Value). A plain segment has Match == "".
//
// Only a single field=value pair per selector is supported. A composite
// merge key (for example a container port keyed by both containerPort and
// protocol together) cannot be expressed in v1.
type FieldPathSegment struct {
	Key   string
	Match string
	Value string
}

// ParseFieldPath parses a dotted field path where any segment may carry a
// bracketed [field=value] selector, for example
// "containers[name=nginx].env[name=FOO].value". It mirrors the targeting
// syntax kustomize uses to address associative list items by their merge
// key, since that is the same problem Kubernetes' own structured merge diff
// field tracking (metadata.managedFields) solves for list items.
func ParseFieldPath(path string) ([]FieldPathSegment, error) {
	if path == "" {
		return nil, fmt.Errorf("empty field path")
	}
	var segments []FieldPathSegment
	i, n := 0, len(path)
	for i < n {
		start := i
		for i < n && path[i] != '.' && path[i] != '[' {
			i++
		}
		if i == start {
			return nil, fmt.Errorf("field path %q: expected a field name at position %d", path, start)
		}
		seg := FieldPathSegment{Key: path[start:i]}
		if i < n && path[i] == '[' {
			end := i + 1
			for end < n && path[end] != ']' {
				end++
			}
			if end >= n {
				return nil, fmt.Errorf("field path %q: unterminated '[' at position %d", path, i)
			}
			inner := path[i+1 : end]
			i = end + 1
			eq := -1
			for j := 0; j < len(inner); j++ {
				if inner[j] == '=' {
					eq = j
					break
				}
			}
			if eq == -1 {
				return nil, fmt.Errorf("field path %q: expected [field=value] inside brackets, got %q", path, inner)
			}
			seg.Match, seg.Value = inner[:eq], inner[eq+1:]
			if seg.Match == "" || seg.Value == "" {
				return nil, fmt.Errorf("field path %q: empty field or value inside brackets %q", path, inner)
			}
		}
		segments = append(segments, seg)
		if i < n {
			if path[i] != '.' {
				return nil, fmt.Errorf("field path %q: expected '.' at position %d, found %q", path, i, string(path[i]))
			}
			i++
			if i == n {
				return nil, fmt.Errorf("field path %q: trailing '.'", path)
			}
		}
	}
	return segments, nil
}

// MatchListItem returns the first element of list that is an object whose
// field equals value, formatted with fmt's default %v so a templated
// numeric or boolean merge key value still compares as a string, and
// whether a match was found.
func MatchListItem(list []any, field, value string) (map[string]any, bool) {
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		v, ok := m[field]
		if !ok {
			continue
		}
		if fmt.Sprintf("%v", v) == value {
			return m, true
		}
	}
	return nil, false
}
