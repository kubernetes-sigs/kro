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

package compiler

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseFieldPath(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		want    []FieldPathSegment
		wantErr string
	}{
		{
			name: "single field",
			path: "replicas",
			want: []FieldPathSegment{{Key: "replicas"}},
		},
		{
			name: "dotted fields",
			path: "spec.replicas",
			want: []FieldPathSegment{{Key: "spec"}, {Key: "replicas"}},
		},
		{
			name: "single selector",
			path: "containers[name=nginx]",
			want: []FieldPathSegment{{Key: "containers", Match: "name", Value: "nginx"}},
		},
		{
			name: "selector followed by a field",
			path: "containers[name=nginx].image",
			want: []FieldPathSegment{{Key: "containers", Match: "name", Value: "nginx"}, {Key: "image"}},
		},
		{
			name: "two selectors",
			path: "spec.template.spec.containers[name=nginx].env[name=FOO].value",
			want: []FieldPathSegment{
				{Key: "spec"}, {Key: "template"}, {Key: "spec"},
				{Key: "containers", Match: "name", Value: "nginx"},
				{Key: "env", Match: "name", Value: "FOO"},
				{Key: "value"},
			},
		},
		{
			name:    "empty path",
			path:    "",
			wantErr: "empty field path",
		},
		{
			name:    "trailing dot",
			path:    "spec.",
			wantErr: "trailing '.'",
		},
		{
			name:    "leading dot",
			path:    ".spec",
			wantErr: "expected a field name",
		},
		{
			name:    "double dot",
			path:    "spec..replicas",
			wantErr: "expected a field name",
		},
		{
			name:    "unterminated bracket",
			path:    "containers[name=nginx",
			wantErr: "unterminated '['",
		},
		{
			name:    "missing equals",
			path:    "containers[name]",
			wantErr: "expected [field=value]",
		},
		{
			name:    "empty selector field",
			path:    "containers[=nginx]",
			wantErr: "empty field or value",
		},
		{
			name:    "empty selector value",
			path:    "containers[name=]",
			wantErr: "empty field or value",
		},
		{
			name:    "missing dot after selector",
			path:    "containers[name=nginx]image",
			wantErr: "expected '.'",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseFieldPath(tt.path)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestMatchListItem(t *testing.T) {
	list := []any{
		map[string]any{"name": "a", "value": "1"},
		map[string]any{"name": "b", "value": "2"},
		"not-an-object",
	}

	item, found := MatchListItem(list, "name", "b")
	require.True(t, found)
	assert.Equal(t, "2", item["value"])

	_, found = MatchListItem(list, "name", "missing")
	assert.False(t, found)

	_, found = MatchListItem(list, "missing-field", "b")
	assert.False(t, found)
}
