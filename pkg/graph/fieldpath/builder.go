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

package fieldpath

import (
	"fmt"
	"strings"
)

// Build constructs a field path string from a slice of segments.
//
// Examples:
//   - [{Field: "spec"}, {Field: "containers", ArrayIdx: 0}] -> spec.containers[0]
//   - [{Field: "spec"}, {Field: "my.field"}] -> spec["my.field"]
func Build(segments []Segment) string {
	var b strings.Builder

	for i, segment := range segments {
		if segment.Index != -1 {
			b.WriteString(fmt.Sprintf("[%d]", segment.Index))
			continue
		}

		// Use bracket notation for field names with dots or empty names
		if strings.Contains(segment.Name, ".") || segment.Name == "" {
			b.WriteString(fmt.Sprintf(`[%q]`, segment.Name))
		} else {
			// Add a dot before regular field names if this isn't the first segment
			if i > 0 {
				b.WriteByte('.')
			}
			b.WriteString(segment.Name)
		}
	}

	return b.String()
}
