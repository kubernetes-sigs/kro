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
	"testing"

	"github.com/kubernetes-sigs/kro/pkg/simpleschema/types"
)

func TestParseField(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		wantType    types.Type
		wantMarkers int
		wantErr     bool
	}{
		{name: "string", input: "string", wantType: types.String},
		{name: "integer", input: "integer", wantType: types.Integer},
		{name: "boolean", input: "boolean", wantType: types.Boolean},
		{name: "float", input: "float", wantType: types.Float},
		{name: "object", input: "object", wantType: types.Object{}},
		{name: "slice of string", input: "[]string", wantType: types.Slice{Elem: types.String}},
		{name: "slice of integer", input: "[]integer", wantType: types.Slice{Elem: types.Integer}},
		{name: "nested slice", input: "[][]string", wantType: types.Slice{Elem: types.Slice{Elem: types.String}}},
		{name: "triple nested slice", input: "[][][]integer", wantType: types.Slice{Elem: types.Slice{Elem: types.Slice{Elem: types.Integer}}}},
		{name: "map string to string", input: "map[string]string", wantType: types.Map{Value: types.String}},
		{name: "map string to integer", input: "map[string]integer", wantType: types.Map{Value: types.Integer}},
		{name: "nested map", input: "map[string]map[string]string", wantType: types.Map{Value: types.Map{Value: types.String}}},
		{name: "slice of map", input: "[]map[string]string", wantType: types.Slice{Elem: types.Map{Value: types.String}}},
		{name: "map of slice", input: "map[string][]integer", wantType: types.Map{Value: types.Slice{Elem: types.Integer}}},
		{name: "custom type", input: "Person", wantType: types.Custom("Person")},
		{name: "slice of custom", input: "[]Person", wantType: types.Slice{Elem: types.Custom("Person")}},
		{name: "map of custom", input: "map[string]Person", wantType: types.Map{Value: types.Custom("Person")}},
		{name: "with one marker", input: "string | required=true", wantType: types.String, wantMarkers: 1},
		{name: "with two markers", input: "string | required=true default=foo", wantType: types.String, wantMarkers: 2},
		{name: "empty slice elem", input: "[]", wantErr: true},
		{name: "non-string map key", input: "map[integer]string", wantErr: true},
		{name: "invalid marker syntax", input: "string | =invalid", wantErr: true},
		{name: "invalid nested slice elem", input: "[][]", wantErr: true},
		{name: "invalid map value type", input: "map[string][]", wantErr: true},
		{name: "empty type", input: "", wantErr: true},
		{name: "whitespace only type", input: "   ", wantErr: true},
		{name: "markers only no type", input: "| required=true", wantErr: true},
		{name: "map with trailing space no value", input: "map[string] ", wantErr: true},
		{name: "incomplete map type", input: "map[string]", wantErr: true},
		{name: "map missing closing bracket", input: "map[stringPerson", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotType, gotMarkers, err := ParseField(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}
			if !typeEqual(gotType, tt.wantType) {
				t.Errorf("type: got %#v, want %#v", gotType, tt.wantType)
			}
			if len(gotMarkers) != tt.wantMarkers {
				t.Errorf("markers: got %d, want %d", len(gotMarkers), tt.wantMarkers)
			}
		})
	}
}

func TestParseSpec(t *testing.T) {
	tests := []struct {
		name    string
		input   interface{}
		want    types.Type
		wantErr bool
	}{
		{
			name: "struct",
			input: map[string]interface{}{
				"name": "string",
				"age":  "integer",
			},
			want: types.Struct{Fields: map[string]types.Type{
				"name": types.String,
				"age":  types.Integer,
			}},
		},
		{
			name: "nested struct",
			input: map[string]interface{}{
				"person": map[string]interface{}{
					"name": "string",
				},
			},
			want: types.Struct{Fields: map[string]types.Type{
				"person": types.Struct{Fields: map[string]types.Type{
					"name": types.String,
				}},
			}},
		},
		{
			name:    "invalid type",
			input:   123,
			wantErr: true,
		},
		{
			name: "invalid field in struct",
			input: map[string]interface{}{
				"name": "string",
				"bad":  123,
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseSpec(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}
			if !typeEqual(got, tt.want) {
				t.Errorf("got %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestParseMapString(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantVal string
		wantOk  bool
	}{
		{name: "simple", input: "map[string]integer", wantVal: "integer", wantOk: true},
		{name: "nested value", input: "map[string][]string", wantVal: "[]string", wantOk: true},
		{name: "nested map", input: "map[string]map[string]int", wantVal: "map[string]int", wantOk: true},
		{name: "non-string key", input: "map[integer]string", wantOk: false},
		{name: "not a map", input: "string", wantOk: false},
		{name: "empty value", input: "map[string]", wantOk: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			val, ok := parseMapString(tt.input)
			if ok != tt.wantOk {
				t.Errorf("ok = %v, want %v", ok, tt.wantOk)
				return
			}
			if !ok {
				return
			}
			if val != tt.wantVal {
				t.Errorf("val = %q, want %q", val, tt.wantVal)
			}
		})
	}
}

func typeEqual(a, b types.Type) bool {
	switch at := a.(type) {
	case types.Atomic:
		bt, ok := b.(types.Atomic)
		return ok && at == bt
	case types.Object:
		_, ok := b.(types.Object)
		return ok
	case types.Slice:
		bt, ok := b.(types.Slice)
		return ok && typeEqual(at.Elem, bt.Elem)
	case types.Map:
		bt, ok := b.(types.Map)
		return ok && typeEqual(at.Value, bt.Value)
	case types.Custom:
		bt, ok := b.(types.Custom)
		return ok && at == bt
	case types.Struct:
		bt, ok := b.(types.Struct)
		if !ok || len(at.Fields) != len(bt.Fields) {
			return false
		}
		for k, v := range at.Fields {
			if !typeEqual(v, bt.Fields[k]) {
				return false
			}
		}
		return true
	default:
		return false
	}
}
