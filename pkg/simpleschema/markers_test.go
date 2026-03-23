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
	"errors"
	"reflect"
	"testing"

	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/utils/ptr"
)

func TestApplyMarkers(t *testing.T) {
	tests := []struct {
		name       string
		schemaType string
		markers    string
		want       *extv1.JSONSchemaProps
		wantParent *extv1.JSONSchemaProps
		wantErr    bool
	}{
		// Required marker
		{
			name:       "required=true adds to parent",
			schemaType: "string",
			markers:    "required=true",
			want:       &extv1.JSONSchemaProps{Type: "string"},
			wantParent: &extv1.JSONSchemaProps{Required: []string{"field"}},
		},
		{
			name:       "required=false does not add to parent",
			schemaType: "string",
			markers:    "required=false",
			want:       &extv1.JSONSchemaProps{Type: "string"},
			wantParent: &extv1.JSONSchemaProps{},
		},
		{
			name:       "invalid required value",
			schemaType: "string",
			markers:    "required=invalid",
			wantErr:    true,
		},
		// Default marker
		{
			name:       "string default",
			schemaType: "string",
			markers:    `default="hello"`,
			want:       &extv1.JSONSchemaProps{Type: "string", Default: &extv1.JSON{Raw: []byte(`"hello"`)}},
		},
		{
			name:       "integer default",
			schemaType: "integer",
			markers:    "default=42",
			want:       &extv1.JSONSchemaProps{Type: "integer", Default: &extv1.JSON{Raw: []byte("42")}},
		},
		{
			name:       "boolean default",
			schemaType: "boolean",
			markers:    "default=true",
			want:       &extv1.JSONSchemaProps{Type: "boolean", Default: &extv1.JSON{Raw: []byte("true")}},
		},
		// Description marker
		{
			name:       "description",
			schemaType: "string",
			markers:    `description="A helpful description"`,
			want:       &extv1.JSONSchemaProps{Type: "string", Description: "A helpful description"},
		},
		// Minimum/Maximum markers
		{
			name:       "minimum",
			schemaType: "integer",
			markers:    "minimum=0",
			want:       &extv1.JSONSchemaProps{Type: "integer", Minimum: ptr.To(0.0)},
		},
		{
			name:       "maximum",
			schemaType: "integer",
			markers:    "maximum=100",
			want:       &extv1.JSONSchemaProps{Type: "integer", Maximum: ptr.To(100.0)},
		},
		{
			name:       "minimum and maximum",
			schemaType: "number",
			markers:    "minimum=0.5 maximum=99.5",
			want:       &extv1.JSONSchemaProps{Type: "number", Minimum: ptr.To(0.5), Maximum: ptr.To(99.5)},
		},
		{
			name:       "invalid minimum",
			schemaType: "integer",
			markers:    "minimum=notanumber",
			wantErr:    true,
		},
		{
			name:       "invalid maximum",
			schemaType: "integer",
			markers:    "maximum=notanumber",
			wantErr:    true,
		},
		// Validation marker
		{
			name:       "validation rule",
			schemaType: "string",
			markers:    `validation="self.size() > 0"`,
			want: &extv1.JSONSchemaProps{
				Type: "string",
				XValidations: []extv1.ValidationRule{
					{Rule: "self.size() > 0", Message: "validation failed"},
				},
			},
		},
		{
			name:       "empty validation",
			schemaType: "string",
			markers:    `validation=""`,
			wantErr:    true,
		},
		// Immutable marker
		{
			name:       "immutable=true",
			schemaType: "string",
			markers:    "immutable=true",
			want: &extv1.JSONSchemaProps{
				Type: "string",
				XValidations: []extv1.ValidationRule{
					{Rule: "self == oldSelf", Message: "field is immutable"},
				},
			},
		},
		{
			name:       "immutable=false",
			schemaType: "string",
			markers:    "immutable=false",
			want:       &extv1.JSONSchemaProps{Type: "string"},
		},
		{
			name:       "invalid immutable",
			schemaType: "string",
			markers:    "immutable=invalid",
			wantErr:    true,
		},
		// Enum marker
		{
			name:       "string enum",
			schemaType: "string",
			markers:    `enum="a,b,c"`,
			want: &extv1.JSONSchemaProps{
				Type: "string",
				Enum: []extv1.JSON{
					{Raw: []byte(`"a"`)},
					{Raw: []byte(`"b"`)},
					{Raw: []byte(`"c"`)},
				},
			},
		},
		{
			name:       "integer enum",
			schemaType: "integer",
			markers:    `enum="1,2,3"`,
			want: &extv1.JSONSchemaProps{
				Type: "integer",
				Enum: []extv1.JSON{
					{Raw: []byte("1")},
					{Raw: []byte("2")},
					{Raw: []byte("3")},
				},
			},
		},
		{
			name:       "invalid integer enum",
			schemaType: "integer",
			markers:    `enum="1,2,abc"`,
			wantErr:    true,
		},
		{
			name:       "empty enum value",
			schemaType: "string",
			markers:    `enum="a,,c"`,
			wantErr:    true,
		},
		{
			name:       "enum on unsupported type",
			schemaType: "boolean",
			markers:    `enum="true,false"`,
			wantErr:    true,
		},
		// Pattern marker
		{
			name:       "pattern",
			schemaType: "string",
			markers:    `pattern="^[a-z]+$"`,
			want:       &extv1.JSONSchemaProps{Type: "string", Pattern: "^[a-z]+$"},
		},
		{
			name:       "invalid pattern regex",
			schemaType: "string",
			markers:    `pattern="[unclosed"`,
			wantErr:    true,
		},
		{
			name:       "empty pattern",
			schemaType: "string",
			markers:    `pattern=""`,
			wantErr:    true,
		},
		{
			name:       "pattern on non-string",
			schemaType: "integer",
			markers:    `pattern="[0-9]+"`,
			wantErr:    true,
		},
		// MinLength/MaxLength markers
		{
			name:       "minLength and maxLength",
			schemaType: "string",
			markers:    "minLength=3 maxLength=20",
			want:       &extv1.JSONSchemaProps{Type: "string", MinLength: ptr.To[int64](3), MaxLength: ptr.To[int64](20)},
		},
		{
			name:       "invalid minLength",
			schemaType: "string",
			markers:    "minLength=abc",
			wantErr:    true,
		},
		{
			name:       "invalid maxLength",
			schemaType: "string",
			markers:    "maxLength=xyz",
			wantErr:    true,
		},
		{
			name:       "minLength on non-string",
			schemaType: "integer",
			markers:    "minLength=5",
			wantErr:    true,
		},
		{
			name:       "maxLength on non-string",
			schemaType: "boolean",
			markers:    "maxLength=10",
			wantErr:    true,
		},
		// UniqueItems marker
		{
			name:       "uniqueItems=true sets list type",
			schemaType: "array",
			markers:    "uniqueItems=true",
			want:       &extv1.JSONSchemaProps{Type: "array", XListType: ptr.To("set")},
		},
		{
			name:       "uniqueItems=false",
			schemaType: "array",
			markers:    "uniqueItems=false",
			want:       &extv1.JSONSchemaProps{Type: "array"},
		},
		{
			name:       "invalid uniqueItems",
			schemaType: "array",
			markers:    "uniqueItems=invalid",
			wantErr:    true,
		},
		{
			name:       "uniqueItems on non-array",
			schemaType: "string",
			markers:    "uniqueItems=true",
			wantErr:    true,
		},
		// MinItems/MaxItems markers
		{
			name:       "minItems and maxItems",
			schemaType: "array",
			markers:    "minItems=2 maxItems=10",
			want:       &extv1.JSONSchemaProps{Type: "array", MinItems: ptr.To[int64](2), MaxItems: ptr.To[int64](10)},
		},
		{
			name:       "invalid minItems",
			schemaType: "array",
			markers:    "minItems=abc",
			wantErr:    true,
		},
		{
			name:       "invalid maxItems",
			schemaType: "array",
			markers:    "maxItems=xyz",
			wantErr:    true,
		},
		{
			name:       "minItems on non-array",
			schemaType: "string",
			markers:    "minItems=5",
			wantErr:    true,
		},
		{
			name:       "maxItems on non-array",
			schemaType: "integer",
			markers:    "maxItems=10",
			wantErr:    true,
		},
		// Combined markers
		{
			name:       "multiple markers",
			schemaType: "string",
			markers:    `required=true default="test" description="A test field"`,
			want: &extv1.JSONSchemaProps{
				Type:        "string",
				Default:     &extv1.JSON{Raw: []byte(`"test"`)},
				Description: "A test field",
			},
			wantParent: &extv1.JSONSchemaProps{Required: []string{"field"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			markers, err := ParseMarkers(tt.markers)
			if err != nil {
				t.Fatalf("ParseMarkers() error = %v", err)
			}

			schema := &extv1.JSONSchemaProps{Type: tt.schemaType}
			parent := &extv1.JSONSchemaProps{}

			err = applyMarkers(schema, markers, "field", parent)
			if (err != nil) != tt.wantErr {
				t.Errorf("applyMarkers() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.wantErr {
				return
			}

			if tt.want != nil && !reflect.DeepEqual(schema, tt.want) {
				t.Errorf("schema mismatch:\ngot:  %+v\nwant: %+v", schema, tt.want)
			}
			if tt.wantParent != nil && !reflect.DeepEqual(parent, tt.wantParent) {
				t.Errorf("parent mismatch:\ngot:  %+v\nwant: %+v", parent, tt.wantParent)
			}
		})
	}
}

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
			name:    "unmatched closing bracket",
			input:   "default=}",
			want:    nil,
			wantErr: true,
		},
		{
			name:    "should not skip markers without an '='",
			input:   "optional",
			want:    nil,
			wantErr: true,
		},
		{
			name:    "should reject markers without an '=' at the end of the marker list",
			input:   "description=\"some description\" optional",
			want:    nil,
			wantErr: true,
		},
		{
			name:    "should reject markers without an '=' at the beginning of the marker list",
			input:   "optional description=\"some description\"",
			want:    nil,
			wantErr: true,
		},
		{
			name:    "should reject markers without an '=' in the middle of the marker list",
			input:   "default=5 optional description=\"some description\"",
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
			name:  "complex markers with json default value",
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
			got, err := ParseMarkers(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseMarkers() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ParseMarkers() = %v, want %v", got, tt.want)
			}
		})
	}
}

// unreachable code but 100% coverage is better than 99.7%.
func TestApplyMarkerUnknownType(t *testing.T) {
	schema := &extv1.JSONSchemaProps{Type: "string"}
	parent := &extv1.JSONSchemaProps{}
	marker := &Marker{MarkerType: MarkerType("unknown"), Key: "unknown", Value: "foo"}

	err := applyMarker(schema, marker, "field", parent)
	if err == nil {
		t.Error("expected error for unknown marker type")
	}
	if !errors.Is(err, ErrUnknownMarker) {
		t.Errorf("expected ErrUnknownMarker, got %v", err)
	}
}
