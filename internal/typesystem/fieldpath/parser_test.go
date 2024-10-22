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
	"reflect"
	"testing"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		want    []Segment
		wantErr bool
	}{
		{
			name: "simple path",
			path: "spec.containers",
			want: []Segment{
				{Name: "spec", Index: -1},
				{Name: "containers", Index: -1},
			},
		},
		{
			name: "path with array",
			path: "spec.containers[0]",
			want: []Segment{
				{Name: "spec", Index: -1},
				{Name: "containers", Index: -1},
				{Name: "", Index: 0},
			},
		},
		{
			name: "path with quoted field",
			path: `spec["my.dotted.field"]`,
			want: []Segment{
				{Name: "spec", Index: -1},
				{Name: "my.dotted.field", Index: -1},
			},
		},
		{
			name: "complex path",
			path: `spec["my.field"].items[0]["other.field"]`,
			want: []Segment{
				{Name: "spec", Index: -1},
				{Name: "my.field", Index: -1},
				{Name: "items", Index: -1},
				{Name: "", Index: 0},
				{Name: "other.field", Index: -1},
			},
		},
		{
			name: "path with multiple arrays",
			path: `spec.items[0].containers[1]["my.field"][42][""][""]`,
			want: []Segment{
				{Name: "spec", Index: -1},
				{Name: "items", Index: -1},
				{Name: "", Index: 0},
				{Name: "containers", Index: -1},
				{Name: "", Index: 1},
				{Name: "my.field", Index: -1},
				{Name: "", Index: 42},
				{Name: "", Index: -1},
				{Name: "", Index: -1},
			},
		},
		{
			name: "nested arrays",
			path: "3dmatrix[0][1][2]",
			want: []Segment{
				{Name: "3dmatrix", Index: -1},
				{Name: "", Index: 0},
				{Name: "", Index: 1},
				{Name: "", Index: 2},
			},
		},
		{
			name: "starting with key lookup",
			path: `["my.field"].salut`,
			want: []Segment{
				{Name: "my.field", Index: -1},
				{Name: "salut", Index: -1},
			},
		},
		{
			name:    "unterminated quote",
			path:    `spec["unterminated`,
			wantErr: true,
		},
		{
			name:    "invalid array index",
			path:    "items[abc]",
			wantErr: true,
		},
		{
			name:    "missing closing bracket",
			path:    `spec["field"`,
			wantErr: true,
		},
		{
			name:    "multiple non close brackets",
			path:    `spec[[[["`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse(tt.path)
			if (err != nil) != tt.wantErr {
				t.Errorf("Parse() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Parse() = %v, want %v", got, tt.want)
			}
		})
	}
}
