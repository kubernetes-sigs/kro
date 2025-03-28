// Copyright 2025 The Kube Resource Orchestrator Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package simpleschema

import (
	"reflect"
	"testing"
)

func TestParseMarkers(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    []*Marker
		wantErr bool
	}{
		{
			name:    "Invalid marker key",
			input:   "invalid=true",
			want:    nil,
			wantErr: true,
		},
		{
			name:    "unclosed quote in value",
			input:   "description=\"Unclosed quote",
			want:    nil,
			wantErr: true,
		},
		{
			name:    "unclosed brace (incomplete json)",
			input:   "default={\"unclosed\": \"brace\"",
			want:    nil,
			wantErr: true,
		},
		{
			name:    "empty marker key",
			input:   "=value",
			want:    nil,
			wantErr: true,
		},
		{
			name:  "Simple markers",
			input: "required=true description=\"This is a description\"",
			want: []*Marker{
				{MarkerType: MarkerTypeRequired, Key: "required", Value: "true"},
				{MarkerType: MarkerTypeDescription, Key: "description", Value: "This is a description"},
			},
			wantErr: false,
		},
		{
			name:  "all markers",
			input: `required=true default=5 description="This is a description"`,
			want: []*Marker{
				{MarkerType: MarkerTypeRequired, Key: "required", Value: "true"},
				{MarkerType: MarkerTypeDefault, Key: "default", Value: "5"},
				{MarkerType: MarkerTypeDescription, Key: "description", Value: "This is a description"},
			},
		},
		{
			name:  "complex markers with array as default value",
			input: "default=[\"key\": \"value\"] required=true",
			want: []*Marker{
				{MarkerType: MarkerTypeDefault, Key: "default", Value: "[\"key\": \"value\"]"},
				{MarkerType: MarkerTypeRequired, Key: "required", Value: "true"},
			},
			wantErr: false,
		},
		{
			name:  "complex markers with json defaul value",
			input: "default={\"key\": \"value\"} description=\"A complex \\\"description\\\"\" required=true",
			want: []*Marker{
				{MarkerType: MarkerTypeDefault, Key: "default", Value: "{\"key\": \"value\"}"},
				{MarkerType: MarkerTypeDescription, Key: "description", Value: "A complex \"description\""},
				{MarkerType: MarkerTypeRequired, Key: "required", Value: "true"},
			},
			wantErr: false,
		},
		{
			name:  "minimum and maximum markers",
			input: "minimum=0 maximum=100",
			want: []*Marker{
				{MarkerType: MarkerTypeMinimum, Key: "minimum", Value: "0"},
				{MarkerType: MarkerTypeMaximum, Key: "maximum", Value: "100"},
			},
			wantErr: false,
		},
		{
			name:  "decimal minimum and maximum",
			input: "minimum=0.1 maximum=1.5",
			want: []*Marker{
				{MarkerType: MarkerTypeMinimum, Key: "minimum", Value: "0.1"},
				{MarkerType: MarkerTypeMaximum, Key: "maximum", Value: "1.5"},
			},
			wantErr: false,
		},
		{
			name:  "Markers with spaces in values",
			input: "description=\"This has spaces\" default=5 required=true",
			want: []*Marker{
				{MarkerType: MarkerTypeDescription, Key: "description", Value: "This has spaces"},
				{MarkerType: MarkerTypeDefault, Key: "default", Value: "5"},
				{MarkerType: MarkerTypeRequired, Key: "required", Value: "true"},
			},
			wantErr: false,
		},
		{
			name: "Markers with escaped characters",
			// my eyes... i hope nobody will ever have to use this.
			input: "description=\"This has \\\"quotes\\\" and a \\n newline\" default=\"\\\"quoted\\\"\"",
			want: []*Marker{
				{MarkerType: MarkerTypeDescription, Key: "description", Value: "This has \"quotes\" and a \\n newline"},
				{MarkerType: MarkerTypeDefault, Key: "default", Value: "\"quoted\""},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseMarkers(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseMarkers() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseMarkers() = %v, want %v", got, tt.want)
			}
		})
	}
}
