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
	"strings"
	"testing"
)

// TestOpenAPISchemaMisuseError verifies that a field written as a nested
// OpenAPI/structural schema (instead of the inline SimpleSchema string form)
// produces a clear, actionable error rather than the confusing "unknown type"
// message that leaks from two levels deeper. See kro issue #1314.
func TestOpenAPISchemaMisuseError(t *testing.T) {
	obj := map[string]interface{}{
		"awsTags": map[string]interface{}{
			"type": "object",
			"additionalProperties": map[string]interface{}{
				"type": "string",
			},
			"default": map[string]interface{}{
				"auto-delete": "no",
			},
		},
	}

	_, err := ToOpenAPISpec(obj, nil)
	if err == nil {
		t.Fatalf("expected an error for OpenAPI-shaped field, got nil")
	}

	msg := err.Error()
	// It must be wrapped with the offending field name...
	if !strings.Contains(msg, "awsTags") {
		t.Errorf("error should name the field %q, got: %s", "awsTags", msg)
	}
	// ...point at the real problem (nested OpenAPI schema)...
	if !strings.Contains(msg, "nested OpenAPI schema") {
		t.Errorf("error should mention the nested OpenAPI schema, got: %s", msg)
	}
	// ...and suggest the inline form.
	if !strings.Contains(msg, "map[string]string | default=") {
		t.Errorf("error should suggest the inline form, got: %s", msg)
	}
	// It must NOT surface the misleading leaked type error.
	if strings.Contains(msg, "unknown type: no") {
		t.Errorf("error should not leak the misleading 'unknown type: no', got: %s", msg)
	}
}

func TestLooksLikeOpenAPISchema(t *testing.T) {
	tests := []struct {
		name string
		spec map[string]interface{}
		want bool
	}{
		{
			name: "full openapi object schema",
			spec: map[string]interface{}{
				"type":                 "object",
				"additionalProperties": map[string]interface{}{"type": "string"},
				"default":              map[string]interface{}{"a": "b"},
			},
			want: true,
		},
		{
			name: "type plus enum",
			spec: map[string]interface{}{"type": "string", "enum": []interface{}{"a", "b"}},
			want: true,
		},
		{
			name: "type plus items",
			spec: map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}},
			want: true,
		},
		{
			name: "no type key is not flagged",
			spec: map[string]interface{}{"default": "x", "format": "date"},
			want: false,
		},
		{
			name: "genuine inline struct with a field named type",
			spec: map[string]interface{}{"type": "string", "name": "string"},
			want: false,
		},
		{
			name: "genuine inline struct with a field named default",
			spec: map[string]interface{}{"default": "string | default=foo", "value": "string"},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := looksLikeOpenAPISchema(tt.spec); got != tt.want {
				t.Errorf("looksLikeOpenAPISchema() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestInlineStructWithKeywordFieldNamesStillParses guards against false
// positives: a legitimate inline struct whose field names happen to include
// OpenAPI keywords must still build a normal object schema.
func TestInlineStructWithKeywordFieldNamesStillParses(t *testing.T) {
	obj := map[string]interface{}{
		"config": map[string]interface{}{
			"type": "string",
			"name": "string | required=true",
		},
	}

	schema, err := ToOpenAPISpec(obj, nil)
	if err != nil {
		t.Fatalf("unexpected error for genuine inline struct: %v", err)
	}
	cfg, ok := schema.Properties["config"]
	if !ok {
		t.Fatalf("expected 'config' property in schema")
	}
	if cfg.Type != "object" {
		t.Errorf("expected 'config' to be an object, got %q", cfg.Type)
	}
	for _, want := range []string{"type", "name"} {
		if _, ok := cfg.Properties[want]; !ok {
			t.Errorf("expected 'config' to have property %q", want)
		}
	}
}
