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

import "testing"

func TestBuild(t *testing.T) {
	tests := []struct {
		name     string
		segments []Segment
		want     string
	}{
		{
			name: "simple field",
			segments: []Segment{
				NewNamedSegment("spec"),
			},
			want: "spec",
		},
		{
			name: "two simple fields",
			segments: []Segment{
				NewNamedSegment("spec"),
				NewNamedSegment("containers"),
			},
			want: "spec.containers",
		},
		{
			name: "array index",
			segments: []Segment{
				NewNamedSegment("containers"),
				NewIndexedSegment(0),
			},
			want: "containers[0]",
		},
		{
			name: "dotted field name",
			segments: []Segment{
				NewNamedSegment("aws.eks.cluster"),
			},
			want: `["aws.eks.cluster"]`,
		},
		{
			name: "mixed names and indices",
			segments: []Segment{
				NewNamedSegment("spec"),
				NewIndexedSegment(0),
				NewNamedSegment("env"),
			},
			want: "spec[0].env",
		},
		{
			name: "dotted names and indices",
			segments: []Segment{
				NewNamedSegment("somefield"),
				NewNamedSegment("labels.kubernetes.io/name"),
				NewIndexedSegment(0),
				NewNamedSegment("value"),
			},
			want: `somefield["labels.kubernetes.io/name"][0].value`,
		},
		{
			name: "consecutive indices",
			segments: []Segment{
				NewNamedSegment("spec"),
				NewIndexedSegment(0),
				NewIndexedSegment(2),
			},
			want: "spec[0][2]",
		},
		{
			name:     "empty segments",
			segments: []Segment{},
			want:     "",
		},
		{
			name: "mix of everything",
			segments: []Segment{
				NewNamedSegment("field"),
				NewNamedSegment("subfield"),
				NewIndexedSegment(0),
				NewNamedSegment("kubernetes.io/config"),
				NewNamedSegment(""),
				NewNamedSegment("field"),
				NewIndexedSegment(1),
			},
			want: `field.subfield[0]["kubernetes.io/config"][""].field[1]`,
		},
	}

	for _, tt := range tests[0:1] {
		t.Run(tt.name, func(t *testing.T) {
			got := Build(tt.segments)
			if got != tt.want {
				t.Errorf("Build() = %v, want %v", got, tt.want)
				return
			}
		})
	}
}
