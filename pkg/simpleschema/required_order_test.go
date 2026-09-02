// Copyright 2026 The Kubernetes Authors.
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
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The required list is collected by ranging over a map, so without an explicit
// sort its order varies between processes. A synthesized CRD that differs only
// in required[] ordering still produces a server-side-apply diff on every
// reconcile, so the order has to be deterministic rather than merely
// self-consistent.
//
// Asserting sortedness rather than comparing against a sorted copy is the point
// here: the existing table tests sort both sides before comparing, so they pass
// whether or not the implementation sorts.
func TestToOpenAPISpecSortsRequiredFields(t *testing.T) {
	// Field names deliberately not in alphabetical order in the source map, and
	// enough of them that map iteration is unlikely to produce sorted output by
	// chance.
	obj := map[string]any{
		"zebra":    "string | required=true",
		"alpha":    "string | required=true",
		"mike":     "integer | required=true",
		"bravo":    "boolean | required=true",
		"yankee":   "string | required=true",
		"charlie":  "integer | required=true",
		"optional": "string",
	}

	schema, err := ToOpenAPISpec(obj, nil)
	require.NoError(t, err)
	require.NotNil(t, schema)

	want := []string{"alpha", "bravo", "charlie", "mike", "yankee", "zebra"}
	assert.Equal(t, want, schema.Required,
		"required[] must be sorted so the synthesized CRD is byte-stable across builds")
	assert.True(t, sort.StringsAreSorted(schema.Required))
	assert.NotContains(t, schema.Required, "optional",
		"a field without required=true must not appear in required[]")
}

// Nested objects synthesize their own required list, so the guarantee has to
// hold at every level rather than only at the root.
func TestToOpenAPISpecSortsNestedRequiredFields(t *testing.T) {
	obj := map[string]any{
		"config": map[string]any{
			"zulu":     "string | required=true",
			"alpha":    "string | required=true",
			"november": "integer | required=true",
		},
	}

	schema, err := ToOpenAPISpec(obj, nil)
	require.NoError(t, err)

	nested, ok := schema.Properties["config"]
	require.True(t, ok, "nested object must be present")
	assert.Equal(t, []string{"alpha", "november", "zulu"}, nested.Required,
		"a nested object's required[] must be sorted too")
}

// Repeated builds of the same input must agree. This is the property the e2e
// suite depends on, and it is what fails in the field when the sort is missing,
// since map iteration order varies per process rather than per call.
func TestToOpenAPISpecRequiredIsStableAcrossBuilds(t *testing.T) {
	obj := map[string]any{
		"delta":   "string | required=true",
		"echo":    "string | required=true",
		"foxtrot": "string | required=true",
		"golf":    "string | required=true",
		"hotel":   "string | required=true",
	}

	first, err := ToOpenAPISpec(obj, nil)
	require.NoError(t, err)

	for i := range 20 {
		again, err := ToOpenAPISpec(obj, nil)
		require.NoError(t, err)
		require.Equal(t, first.Required, again.Required,
			"build %d produced a different required[] ordering", i)
	}
}
