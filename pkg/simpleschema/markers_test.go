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
		{
			name:  "immutable marker with other markers",
			input: "immutable=true required=false",
			want: []*Marker{
				{MarkerType: MarkerTypeImmutable, Key: "immutable", Value: "true"},
				{MarkerType: MarkerTypeRequired, Key: "required", Value: "false"},
			},
			wantErr: false,
		},
		{
			name:  "pattern marker",
			input: "pattern=\"^[a-zA-Z0-9]+$\"",
			want: []*Marker{
				{MarkerType: MarkerTypePattern, Key: "pattern", Value: "^[a-zA-Z0-9]+$"},
			},
			wantErr: false,
		},
		{
			name:  "minLength and maxLength markers",
			input: "minLength=5 maxLength=20",
			want: []*Marker{
				{MarkerType: MarkerTypeMinLength, Key: "minLength", Value: "5"},
				{MarkerType: MarkerTypeMaxLength, Key: "maxLength", Value: "20"},
			},
			wantErr: false,
		},
		{
			name:  "all string validation markers",
			input: "pattern=\"[a-z]+\" minLength=3 maxLength=15 description=\"String field with validation\"",
			want: []*Marker{
				{MarkerType: MarkerTypePattern, Key: "pattern", Value: "[a-z]+"},
				{MarkerType: MarkerTypeMinLength, Key: "minLength", Value: "3"},
				{MarkerType: MarkerTypeMaxLength, Key: "maxLength", Value: "15"},
				{MarkerType: MarkerTypeDescription, Key: "description", Value: "String field with validation"},
			},
			wantErr: false,
		},
		{
			name:  "pattern with special regex characters",
			input: "pattern=\"^(foo|bar)\\d{2,4}$\"",
			want: []*Marker{
				{MarkerType: MarkerTypePattern, Key: "pattern", Value: "^(foo|bar)\\d{2,4}$"},
			},
			wantErr: false,
		},
		{
			name:  "zero minLength",
			input: "minLength=0",
			want: []*Marker{
				{MarkerType: MarkerTypeMinLength, Key: "minLength", Value: "0"},
			},
			wantErr: false,
		},
		{
			name:  "uniqueItems marker true",
			input: "uniqueItems=true",
			want: []*Marker{
				{MarkerType: MarkerTypeUniqueItems, Key: "uniqueItems", Value: "true"},
			},
			wantErr: false,
		},
		{
			name:  "uniqueItems marker false",
			input: "uniqueItems=false",
			want: []*Marker{
				{MarkerType: MarkerTypeUniqueItems, Key: "uniqueItems", Value: "false"},
			},
			wantErr: false,
		},
		{
			name:  "array field with uniqueItems and validation",
			input: "uniqueItems=true description=\"Array with unique elements\"",
			want: []*Marker{
				{MarkerType: MarkerTypeUniqueItems, Key: "uniqueItems", Value: "true"},
				{MarkerType: MarkerTypeDescription, Key: "description", Value: "Array with unique elements"},
			},
			wantErr: false,
		},
		{
			name:  "minItems marker",
			input: "minItems=2",
			want: []*Marker{
				{MarkerType: MarkerTypeMinItems, Key: "minItems", Value: "2"},
			},
			wantErr: false,
		},
		{
			name:  "maxItems marker",
			input: "maxItems=10",
			want: []*Marker{
				{MarkerType: MarkerTypeMaxItems, Key: "maxItems", Value: "10"},
			},
			wantErr: false,
		},
		{
			name:  "minItems and maxItems markers",
			input: "minItems=1 maxItems=5",
			want: []*Marker{
				{MarkerType: MarkerTypeMinItems, Key: "minItems", Value: "1"},
				{MarkerType: MarkerTypeMaxItems, Key: "maxItems", Value: "5"},
			},
			wantErr: false,
		},
		{
			name:  "array field with all validation markers",
			input: "uniqueItems=true minItems=2 maxItems=8 description=\"Array with comprehensive validation\"",
			want: []*Marker{
				{MarkerType: MarkerTypeUniqueItems, Key: "uniqueItems", Value: "true"},
				{MarkerType: MarkerTypeMinItems, Key: "minItems", Value: "2"},
				{MarkerType: MarkerTypeMaxItems, Key: "maxItems", Value: "8"},
				{MarkerType: MarkerTypeDescription, Key: "description", Value: "Array with comprehensive validation"},
			},
			wantErr: false,
		},
		{
			name:  "zero minItems",
			input: "minItems=0",
			want: []*Marker{
				{MarkerType: MarkerTypeMinItems, Key: "minItems", Value: "0"},
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
