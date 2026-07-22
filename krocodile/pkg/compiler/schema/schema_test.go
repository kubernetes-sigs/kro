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

// TestBuildNamespacelessObjectMetaSchema checks that the namespace property
// and required entry are dropped, while other fields are preserved. The
// nil-Properties / nil-Required rows exercise the early-skip branches.
func TestBuildNamespacelessObjectMetaSchema(t *testing.T) {
	tests := []struct {
		name         string
		input        spec.Schema
		wantProps    []string
		wantNotProps []string
		wantRequired []string
	}{
		{
			name: "drops namespace property and required entry",
			input: spec.Schema{
				SchemaProps: spec.SchemaProps{
					Properties: map[string]spec.Schema{
						"name":      {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
						"namespace": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
					},
					Required: []string{"name", "namespace"},
				},
			},
			wantProps:    []string{"name"},
			wantNotProps: []string{"namespace"},
			wantRequired: []string{"name"},
		},
		{
			name:  "nil properties and required is a no-op",
			input: spec.Schema{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := buildNamespacelessObjectMetaSchema(tc.input)

			for _, p := range tc.wantProps {
				_, ok := got.Properties[p]
				assert.True(t, ok, "expected property %q", p)
			}
			for _, p := range tc.wantNotProps {
				_, ok := got.Properties[p]
				assert.False(t, ok, "did not expect property %q", p)
			}
			if tc.wantRequired != nil {
				assert.Equal(t, tc.wantRequired, got.Required)
			}
		})
	}
}

// TestObjectMetaSchemasPopulated confirms the package-level schemas were
// populated by init, and that the namespaceless variant has no namespace.
func TestObjectMetaSchemasPopulated(t *testing.T) {
	require.NotNil(t, ObjectMetaSchema.Properties)
	_, hasNamespace := ObjectMetaSchema.Properties["namespace"]
	assert.True(t, hasNamespace, "ObjectMeta should carry namespace")

	require.NotNil(t, NamespacelessObjectMetaSchema.Properties)
	_, hasNamespaceless := NamespacelessObjectMetaSchema.Properties["namespace"]
	assert.False(t, hasNamespaceless, "namespaceless variant must omit namespace")
}
