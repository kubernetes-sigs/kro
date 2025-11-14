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

func TestStripIndices(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		want    string
		wantErr bool
	}{
		{
			name: "simple path without indices",
			path: "spec.containers.ports",
			want: "spec.containers.ports",
		},
		{
			name: "path with single array index",
			path: "spec.containers[0]",
			want: "spec.containers",
		},
		{
			name: "path with multiple array indices",
			path: "routes[0].middlewares[1]",
			want: "routes.middlewares",
		},
		{
			name: "path with consecutive indices",
			path: "spec[0][1].field",
			want: "spec.field",
		},
		{
			name: "path with quoted field names",
			path: `spec["my.field"][0].value`,
			want: `spec.["my.field"]value`,
		},
		{
			name: "complex path with mixed indices and fields",
			path: "spec.containers[0].env[2].value",
			want: "spec.containers.env.value",
		},
		{
			name: "path with only indices",
			path: "[0][1][2]",
			want: "",
		},
		{
			name: "empty path",
			path: "",
			want: "",
		},
		{
			name: "path with trailing index",
			path: "routes.middlewares[99]",
			want: "routes.middlewares",
		},
		{
			name:    "invalid path - unterminated bracket",
			path:    "spec.containers[0",
			wantErr: true,
		},
		{
			name:    "invalid path - unterminated quote",
			path:    `spec["field`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := StripIndices(tt.path)
			if (err != nil) != tt.wantErr {
				t.Errorf("StripIndices() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("StripIndices() = %q, want %q", got, tt.want)
			}
		})
	}
}
