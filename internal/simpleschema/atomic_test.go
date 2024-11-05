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
	"testing"
)

func TestIsAtomicType(t *testing.T) {
	tests := []struct {
		name     string
		typeName string
		want     bool
	}{
		{"Boolean", "boolean", true},
		{"Integer", "integer", true},
		{"Float", "float", true},
		{"String", "string", true},
		{"Invalid", "invalid", false},
		{"Empty", "", false},
		{"", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isAtomicType(tt.typeName); got != tt.want {
				t.Errorf("isAtomicType() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsCollectionType(t *testing.T) {
	tests := []struct {
		name     string
		typeName string
		want     bool
	}{
		{"array type", "[]string", true},
		{"map map", "map[string]int", true},
		{"not a collection", "string", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isCollectionType(tt.typeName); got != tt.want {
				t.Errorf("isCollectionType() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsMapType(t *testing.T) {
	tests := []struct {
		name     string
		typeName string
		want     bool
	}{
		{"valid Map", "map[string]integer", true},
		{"Not Map", "[]string", false},
		{"Not Map", "string", false},
		{"customMap", "map[string]custom", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isMapType(tt.typeName); got != tt.want {
				t.Errorf("isMapType() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsSliceType(t *testing.T) {
	tests := []struct {
		name     string
		typeName string
		want     bool
	}{
		{"valid Slice", "[]string", true},
		{"not Slice", "map[string]int", false},
		{"not Slice", "string", false},
		{"customSlice", "[]something", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isSliceType(tt.typeName); got != tt.want {
				t.Errorf("isSliceType() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseMapType(t *testing.T) {
	tests := []struct {
		name          string
		typeName      string
		wantKeyType   string
		wantValueType string
		wantErr       bool
	}{
		{"valid map", "map[string]integer", "string", "integer", false},
		{"Valid Complex Map", "map[string]map[int]bool", "string", "map[int]bool", false},
		{"Nested Map", "map[string]map[string]map[string]integer", "string", "map[string]map[string]integer", false},
		{"invalid map", "map[]", "", "", true},
		{"invalid map", "map[string]", "", "", true},
		{"not a map", "something", "", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotKeyType, gotValueType, err := parseMapType(tt.typeName)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseMapType() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if gotKeyType != tt.wantKeyType {
				t.Errorf("parseMapType() gotKeyType = %v, want %v", gotKeyType, tt.wantKeyType)
			}
			if gotValueType != tt.wantValueType {
				t.Errorf("parseMapType() gotValueType = %v, want %v", gotValueType, tt.wantValueType)
			}
		})
	}
}

func TestParseSliceType(t *testing.T) {
	tests := []struct {
		name         string
		typeName     string
		wantElemType string
		wantErr      bool
	}{
		{"valid slice", "[]string", "string", false},
		{"Valid Complex Slice", "[]map[string]int", "map[string]int", false},
		{"Nested Slice", "[][][]int", "[][]int", false},
		{"invalid slice", "[]", "", true},
		{"Not a slice", "string", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotElemType, err := parseSliceType(tt.typeName)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseSliceType() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if gotElemType != tt.wantElemType {
				t.Errorf("parseSliceType() = %v, want %v", gotElemType, tt.wantElemType)
			}
		})
	}
}
