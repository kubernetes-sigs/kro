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

package schema

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/kube-openapi/pkg/validation/spec"
)

func objectSchemaWith(props map[string]spec.Schema) *spec.Schema {
	return &spec.Schema{
		SchemaProps: spec.SchemaProps{
			Type:       []string{"object"},
			Properties: props,
		},
	}
}

// TestCacheLookupField pins the pointer-stability contract: a (parent, field)
// pair always returns the same pointer, missing fields return nil, and a nil
// parent or absent Properties is handled gracefully.
func TestCacheLookupField(t *testing.T) {
	tests := []struct {
		name    string
		parent  *spec.Schema
		field   string
		wantNil bool
	}{
		{
			name:    "nil parent",
			parent:  nil,
			field:   "x",
			wantNil: true,
		},
		{
			name:    "nil properties",
			parent:  &spec.Schema{SchemaProps: spec.SchemaProps{Type: []string{"object"}}},
			field:   "x",
			wantNil: true,
		},
		{
			name: "missing field",
			parent: objectSchemaWith(map[string]spec.Schema{
				"a": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
			}),
			field:   "b",
			wantNil: true,
		},
		{
			name: "existing field",
			parent: objectSchemaWith(map[string]spec.Schema{
				"a": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
			}),
			field:   "a",
			wantNil: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := NewCache()
			got := c.LookupField(tc.parent, tc.field)
			if tc.wantNil {
				assert.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			// Pointer-stability: the same lookup returns the identical pointer.
			again := c.LookupField(tc.parent, tc.field)
			assert.Same(t, got, again)
		})
	}
}

// TestCacheLookupAdditionalProperties covers the three additionalProperties
// shapes: an explicit schema (returned directly), Allows=true (a stable empty
// schema), and the nil / Allows=false cases that return nil.
func TestCacheLookupAdditionalProperties(t *testing.T) {
	explicit := &spec.Schema{SchemaProps: spec.SchemaProps{Type: []string{"string"}}}

	tests := []struct {
		name      string
		parent    *spec.Schema
		wantNil   bool
		wantSame  *spec.Schema // if set, the result must be this exact pointer
		wantEmpty bool         // if true, result is a fresh stable empty schema
	}{
		{
			name:    "nil parent",
			parent:  nil,
			wantNil: true,
		},
		{
			name:    "nil additionalProperties",
			parent:  &spec.Schema{SchemaProps: spec.SchemaProps{Type: []string{"object"}}},
			wantNil: true,
		},
		{
			name: "explicit schema returned directly",
			parent: &spec.Schema{SchemaProps: spec.SchemaProps{
				AdditionalProperties: &spec.SchemaOrBool{Schema: explicit},
			}},
			wantSame: explicit,
		},
		{
			name: "allows false returns nil",
			parent: &spec.Schema{SchemaProps: spec.SchemaProps{
				AdditionalProperties: &spec.SchemaOrBool{Allows: false},
			}},
			wantNil: true,
		},
		{
			name: "allows true returns stable empty schema",
			parent: &spec.Schema{SchemaProps: spec.SchemaProps{
				AdditionalProperties: &spec.SchemaOrBool{Allows: true},
			}},
			wantEmpty: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := NewCache()
			got := c.LookupAdditionalProperties(tc.parent)
			switch {
			case tc.wantNil:
				assert.Nil(t, got)
			case tc.wantSame != nil:
				assert.Same(t, tc.wantSame, got)
			case tc.wantEmpty:
				require.NotNil(t, got)
				assert.Equal(t, &spec.Schema{}, got)
				// Stable across repeated lookups for the same parent.
				assert.Same(t, got, c.LookupAdditionalProperties(tc.parent))
			}
		})
	}
}

// TestCacheWrapAsList checks the wrapper structure and that the same item
// pointer always yields the same wrapper pointer.
func TestCacheWrapAsList(t *testing.T) {
	c := NewCache()
	item := &spec.Schema{SchemaProps: spec.SchemaProps{Type: []string{"string"}}}

	wrapped := c.WrapAsList(item)
	require.NotNil(t, wrapped)
	assert.Equal(t, spec.StringOrArray{"array"}, wrapped.Type)
	require.NotNil(t, wrapped.Items)
	assert.Same(t, item, wrapped.Items.Schema)

	// Same item pointer returns the same wrapper.
	assert.Same(t, wrapped, c.WrapAsList(item))

	// Distinct item pointer gets a distinct wrapper.
	other := &spec.Schema{SchemaProps: spec.SchemaProps{Type: []string{"integer"}}}
	assert.NotSame(t, wrapped, c.WrapAsList(other))
}
