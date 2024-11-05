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
		if i > 0 && !strings.HasSuffix(b.String(), "]") {
			b.WriteByte('.')
		}

		if segment.Index != -1 {
			b.WriteString(fmt.Sprintf("[%d]", segment.Index))
			continue
		}

		if strings.Contains(segment.Name, ".") {
			b.WriteString(fmt.Sprintf(`[%q]`, segment.Name))
		} else {
			b.WriteString(segment.Name)
		}
	}

	return b.String()
}
