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

package schema

import (
	"fmt"
	"strconv"
	"strings"
)

// pathPart represents a part of a field path.
type pathPart struct {
	// name is the name of the field.
	name string
	// isArray is true if the field is an array type (a.ka access
	// via integer index).
	//
	// NOTE(a-hilaly) You might wonder why we don't have a isMap field
	// here. This is because map fields are accessed via dot notation
	// (e.g "metadata.labels.foo"). This is mainly a CEL feature, or
	// limitation, depends on how you see it :)
	isArray bool
	// index is the index of the field in the array.
	index int
}

func parsePath(path string) ([]pathPart, error) {
	var parts []pathPart
	currentPart := ""

	if path == "" {
		return nil, fmt.Errorf("empty path")
	}

	/* if strings.HasPrefix(path, ".") {
		return nil, fmt.Errorf("path cannot start with a dot")
	} */
	// TODO(a-hilaly): Do not allow paths to start with a dot
	path = strings.TrimPrefix(path, ".")

	if strings.HasSuffix(path, ".") {
		return nil, fmt.Errorf("path cannot end with a dot")
	}

	for i := 0; i < len(path); i++ {
		switch path[i] {
		case '.':
			if currentPart == "" {
				// do not allow trailing dots like ".." in the path
				return nil, fmt.Errorf("empty field name at position %d", i)
			}
			parts = append(parts, pathPart{name: currentPart})
			currentPart = ""
		case '[':
			if currentPart != "" {
				parts = append(parts, pathPart{name: currentPart})
				currentPart = ""
			}
			closeBracket := strings.IndexByte(path[i:], ']')
			if closeBracket == -1 {
				return nil, fmt.Errorf("unclosed bracket at position %d", i)
			}
			indexStr := path[i+1 : i+closeBracket]
			index, err := strconv.Atoi(indexStr)
			if err != nil {
				return nil, fmt.Errorf("invalid array index at position %d: %v", i, err)
			}
			if index < 0 {
				return nil, fmt.Errorf("negative array index at position %d", i)
			}

			parts = append(parts, pathPart{name: "", isArray: true, index: index})
			i += closeBracket
			// Skip the next dot if it exists
			if i+1 < len(path) && path[i+1] == '.' {
				i++
			}
		default:
			currentPart += string(path[i])
		}
	}

	if currentPart != "" {
		parts = append(parts, pathPart{name: currentPart})
	}
	return parts, nil
}
