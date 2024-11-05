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
