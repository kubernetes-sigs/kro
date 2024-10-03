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
	"reflect"
	"testing"
)

func TestParsePath(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		want    []pathPart
		wantErr bool
	}{
		{
			name: "simple access",
			path: "metadata.name",
			want: []pathPart{
				{name: "metadata", isArray: false},
				{name: "name", isArray: false},
			},
			wantErr: false,
		},
		{
			name: "path with array",
			path: "spec.containers[0].name",
			want: []pathPart{
				{name: "spec", isArray: false},
				{name: "containers", isArray: false},
				{name: "", isArray: true, index: 0},
				{name: "name", isArray: false},
			},
			wantErr: false,
		},
		{
			name: "path with multiple arrays",
			path: "spec.containers[0].ports[1].containerPort",
			want: []pathPart{
				{name: "spec", isArray: false},
				{name: "containers", isArray: false},
				{name: "", isArray: true, index: 0},
				{name: "ports", isArray: false},
				{name: "", isArray: true, index: 1},
				{name: "containerPort", isArray: false},
			},
			wantErr: false,
		},
		{
			name:    "Empty path",
			path:    "",
			want:    nil,
			wantErr: true, // Do we want an error here?
		},
		// TODO(a-hilaly): Need this test but it requires a change in the parser
		/* {
			name:    "Path starting with dot",
			path:    ".metadata.name",
			want:    nil,
			wantErr: true,
		}, */
		{
			name:    "Path ending with dot",
			path:    "metadata.name.",
			want:    nil,
			wantErr: true,
		},
		{
			name:    "Path with consecutive dots",
			path:    "metadata..name",
			want:    nil,
			wantErr: true,
		},
		{
			name:    "Path with unclosed bracket",
			path:    "spec.containers[0.name",
			want:    nil,
			wantErr: true,
		},
		{
			name:    "Path with invalid array index",
			path:    "spec.containers[a].name",
			want:    nil,
			wantErr: true,
		},
		{
			name:    "Path with negative array index",
			path:    "spec.containers[-15].name",
			want:    nil,
			wantErr: true,
		},
		{
			name: "path with large array index",
			path: "spec.containers[20200406].name",
			want: []pathPart{
				{name: "spec", isArray: false},
				{name: "containers", isArray: false},
				{name: "", isArray: true, index: 20200406},
				{name: "name", isArray: false},
			},
			wantErr: false,
		},
		{
			name:    "Path with empty array brackets",
			path:    "spec.containers[].name",
			want:    nil,
			wantErr: true,
		},
		{
			name:    "Path with string array brackets",
			path:    "spec.containers[holla].name",
			want:    nil,
			wantErr: true,
		},
		{
			name: "path with underscore and camel case",
			path: "metadata.labels.my_label.myNestedField",
			want: []pathPart{
				{name: "metadata", isArray: false},
				{name: "labels", isArray: false},
				{name: "my_label", isArray: false},
				{name: "myNestedField", isArray: false},
			},
			wantErr: false,
		},
		{
			name: "Path with numbers in field names",
			path: "spec.container1.port2",
			want: []pathPart{
				{name: "spec", isArray: false},
				{name: "container1", isArray: false},
				{name: "port2", isArray: false},
			},
			wantErr: false,
		},
		{
			name:    "path with only brackets",
			path:    "[]",
			want:    nil,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parsePath(tt.path)
			if (err != nil) != tt.wantErr {
				t.Errorf("parsePath() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parsePath() = %v, want %v", got, tt.want)
			}
		})
	}
}
