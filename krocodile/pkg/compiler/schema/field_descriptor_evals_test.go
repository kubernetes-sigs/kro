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

	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// TestAreValidExpressionEvals pins the validity rule used when a field is
// built from a multi-segment template: no values is invalid, a single value
// is always valid, and multi-value runs are only valid when every value is a
// string (string concatenation being the only meaningful multi-expression
// combination).
func TestAreValidExpressionEvals(t *testing.T) {
	tests := []struct {
		name  string
		evals []ref.Val
		want  bool
	}{
		{name: "empty is invalid", evals: nil, want: false},
		{name: "single value is valid", evals: []ref.Val{types.Int(1)}, want: true},
		{
			name:  "multi strings valid",
			evals: []ref.Val{types.String("a"), types.String("b")},
			want:  true,
		},
		{
			name:  "multi first not string invalid",
			evals: []ref.Val{types.Int(1), types.Int(2)},
			want:  false,
		},
		{
			name:  "multi mixed types invalid",
			evals: []ref.Val{types.String("a"), types.Int(2)},
			want:  false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, areValidExpressionEvals(tc.evals))
		})
	}
}

// TestGenerateSchemaFromEvals_ErrorPaths covers the failure surface of the
// evals-to-schema entry point: invalid eval sets and unsupported value types
// both bubble up as errors.
func TestGenerateSchemaFromEvals_ErrorPaths(t *testing.T) {
	tests := []struct {
		name    string
		evals   map[string][]ref.Val
		wantErr bool
	}{
		{
			name:    "invalid eval set (empty)",
			evals:   map[string][]ref.Val{"status.x": {}},
			wantErr: true,
		},
		{
			name:    "multi-value mixed types",
			evals:   map[string][]ref.Val{"status.x": {types.Int(1), types.Int(2)}},
			wantErr: true,
		},
		{
			name:    "unsupported value type errors",
			evals:   map[string][]ref.Val{"status.x": {types.NullValue}},
			wantErr: true,
		},
		{
			name:    "valid single value",
			evals:   map[string][]ref.Val{"status.x": {types.String("v")}},
			wantErr: false,
		},
		{
			name:    "unparsable path errors",
			evals:   map[string][]ref.Val{"status..x": {types.String("v")}},
			wantErr: true,
		},
		{
			name:    "array leaf path",
			evals:   map[string][]ref.Val{"status.items[0]": {types.String("v")}},
			wantErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := GenerateSchemaFromEvals(tc.evals)
			if tc.wantErr {
				require.Error(t, err)
				assert.Nil(t, got)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, got)
		})
	}
}

// TestAddFieldToSchema_NilLeafDefaultsToString verifies that a field
// descriptor with a nil schema defaults its leaf to a string type, and that
// an unparsable path surfaces an error.
func TestAddFieldToSchema_NilLeafDefaultsToString(t *testing.T) {
	tests := []struct {
		name      string
		fd        fieldDescriptor
		wantErr   bool
		wantField bool
	}{
		{
			name:      "nil schema defaults to string",
			fd:        fieldDescriptor{Path: "status.x", Schema: nil},
			wantField: true,
		},
		{
			name:      "empty path is a no-op",
			fd:        fieldDescriptor{Path: "", Schema: nil},
			wantField: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := &extv1.JSONSchemaProps{
				Type:       "object",
				Properties: make(map[string]extv1.JSONSchemaProps),
			}
			err := addFieldToSchema(tc.fd, root)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			if !tc.wantField {
				assert.Empty(t, root.Properties)
				return
			}
			status, ok := root.Properties["status"]
			require.True(t, ok)
			x, ok := status.Properties["x"]
			require.True(t, ok)
			assert.Equal(t, "string", x.Type)
		})
	}
}
