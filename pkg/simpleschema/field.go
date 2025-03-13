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

package simpleschema

import (
	"fmt"
	"strings"
)

// parseFieldType parses the type from a typed value. The type can be in the
// format `type | marker1=value1 marker2=value2`
//
// For example, `string | required=true description="something" default="foo"`
// note that the type is always required, but the markers are optional. If no
// markers are present, the function will return an empty slice.
//
// The type can either be an atomic type, a collection type, or a custom type.
// It is up to the caller to determine what to do with the parsed type.
func parseFieldSchema(fieldSchema string) (string, []*Marker, error) {
	// we need to parse the type and its markers
	// type can be in the format `type | marker1=value1 marker2=value2`
	if fieldSchema == "" {
		return "", nil, fmt.Errorf("empty type")
	}

	// split the type and markers if possible
	parts := strings.Split(fieldSchema, "|")
	if len(parts) > 2 {
		return "", nil, fmt.Errorf("invalid type: %s", fieldSchema)
	}

	// trim spaces from the type
	typ := strings.TrimSpace(parts[0])
	if typ == "" {
		return "", nil, fmt.Errorf("empty type")
	}

	if len(parts) == 1 {
		// no markers
		return typ, nil, nil
	}

	// trim spaces from the markers
	markers, err := parseMarkers(strings.TrimSpace(parts[1]))
	if err != nil {
		return "", nil, err
	}

	return typ, markers, nil
}
