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
	"reflect"
	"testing"
)

func TestParseFieldSchema(t *testing.T) {
	tests := []struct {
		name        string
		fieldSchema string
		wantType    string
		wantMarkers []*Marker
		wantErr     bool
	}{
		{
			name:        "valid field with markers",
			fieldSchema: "string | required=true description=\"A-test-field\" default=\"kubernetes-is-very-nice!\"",
			wantType:    "string",
			wantMarkers: []*Marker{
				{MarkerType: MarkerTypeRequired, Key: "required", Value: "true"},
				{MarkerType: MarkerTypeDescription, Key: "description", Value: "A-test-field"},
				{MarkerType: MarkerTypeDefault, Key: "default", Value: "kubernetes-is-very-nice!"},
			},
			wantErr: false,
		},
		{
			name:        "Valid field without markers",
			fieldSchema: "integer",
			wantType:    "integer",
			wantMarkers: nil,
			wantErr:     false,
		},
		{
			name:        "Invalid field schema",
			fieldSchema: "| invalid",
			wantType:    "",
			wantMarkers: nil,
			wantErr:     true,
		},
		{
			name:        "integer field with min and max",
			fieldSchema: "integer | minimum=0 maximum=100",
			wantType:    "integer",
			wantMarkers: []*Marker{
				{MarkerType: MarkerTypeMinimum, Key: "minimum", Value: "0"},
				{MarkerType: MarkerTypeMaximum, Key: "maximum", Value: "100"},
			},
			wantErr: false,
		},
		{
			name:        "number field with decimal constraints",
			fieldSchema: "float | minimum=0.1 maximum=1.0",
			wantType:    "float",
			wantMarkers: []*Marker{
				{MarkerType: MarkerTypeMinimum, Key: "minimum", Value: "0.1"},
				{MarkerType: MarkerTypeMaximum, Key: "maximum", Value: "1.0"},
			},
			wantErr: false,
		},
		{
			name:        "array field with complex validation",
			fieldSchema: "[]string | validation=\"self.all(x, startsWith(x, 'foo') || startsWith(x, 'bar'))\"",
			wantType:    "[]string",
			wantMarkers: []*Marker{
				{MarkerType: MarkerTypeValidation, Key: "validation", Value: "self.all(x, startsWith(x, 'foo') || startsWith(x, 'bar'))"},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotType, gotMarkers, err := parseFieldSchema(tt.fieldSchema)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseFieldSchema() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if gotType != tt.wantType {
				t.Errorf("parseFieldSchema() gotType = %v, want %v", gotType, tt.wantType)
			}
			if !reflect.DeepEqual(gotMarkers, tt.wantMarkers) {
				t.Errorf("parseFieldSchema() gotMarkers = %+v, want %+v", gotMarkers, tt.wantMarkers)
			}
		})
	}
}
