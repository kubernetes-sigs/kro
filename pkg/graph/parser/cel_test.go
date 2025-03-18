// Copyright 2025 The Kube Resource Orchestrator Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package parser

import (
	"testing"
)

func TestExtractExpressions(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    []string
		wantErr bool
	}{
		{
			name:    "Simple expression",
			input:   "${resource.field}",
			want:    []string{"resource.field"},
			wantErr: false,
		},
		{
			name:    "Expression with function",
			input:   "${length(resource.list)}",
			want:    []string{"length(resource.list)"},
			wantErr: false,
		},
		{
			name:    "Expression with prefix",
			input:   "prefix-${resource.field}",
			want:    []string{"resource.field"},
			wantErr: false,
		},
		{
			name:    "Expression with suffix",
			input:   "${resource.field}-suffix",
			want:    []string{"resource.field"},
			wantErr: false,
		},
		{
			name:    "Multiple expressions",
			input:   "${resource1.field}-middle-${resource2.field}",
			want:    []string{"resource1.field", "resource2.field"},
			wantErr: false,
		},
		{
			name:    "Expression with map",
			input:   "${resource.map['key']}",
			want:    []string{"resource.map['key']"},
			wantErr: false,
		},
		{
			name:    "Expression with list index",
			input:   "${resource.list[0]}",
			want:    []string{"resource.list[0]"},
			wantErr: false,
		},
		{
			name:    "Complex expression",
			input:   "${resource.field == 'value' && resource.number > 5}",
			want:    []string{"resource.field == 'value' && resource.number > 5"},
			wantErr: false,
		},
		{
			name:    "No expressions",
			input:   "plain string",
			want:    []string{},
			wantErr: false,
		},
		{
			name:    "Empty string",
			input:   "",
			want:    []string{},
			wantErr: false,
		},
		{
			name:    "Incomplete expression",
			input:   "${incomplete",
			want:    []string{},
			wantErr: false,
		},
		{
			name:    "Expression with escaped quotes",
			input:   "${resource.field == \"escaped\\\"quote\"}",
			want:    []string{"resource.field == \"escaped\\\"quote\""},
			wantErr: false,
		},
		{
			name:    "Multiple expressions with whitespace",
			input:   "  ${resource1.field}  ${resource2.field}  ",
			want:    []string{"resource1.field", "resource2.field"},
			wantErr: false,
		},
		{
			name:    "Expression with newlines",
			input:   "${resource.list.map(\n  x,\n  x * 2\n)}",
			want:    []string{"resource.list.map(\n  x,\n  x * 2\n)"},
			wantErr: false,
		},
		{
			name:    "Nested expression (should error)",
			input:   "${outer(${inner})} ${outer}",
			want:    nil,
			wantErr: true,
		},
		{
			name:    "Nested expression but with quotes",
			input:   "${outer(\"${inner}\")}",
			want:    []string{"outer(\"${inner}\")"},
			wantErr: false,
		},
		{
			name:    "Nested closing brace without opening one",
			input:   "${\"text with }} inside\"}",
			want:    []string{"\"text with }} inside\""},
			wantErr: false,
		},
		{
			name:    "Nested open brace without closing one",
			input:   "${\"text with { inside\"}",
			want:    []string{"\"text with { inside\""},
			wantErr: false,
		},
		{
			name:    "Expressions with dictionary building",
			input:   "${true ? {'key': 'value'} : {'key': 'value2'}}",
			want:    []string{"true ? {'key': 'value'} : {'key': 'value2'}"},
			wantErr: false,
		},
		{
			name:  "Multiple expressions with dictionary building",
			input: "${true ? {'key': 'value'} : {'key': 'value2'}} somewhat ${resource.field} then ${false ? {'key': {'nestedKey':'value'}} : {'key': 'value2'}}",
			want: []string{
				"true ? {'key': 'value'} : {'key': 'value2'}",
				"resource.field",
				"false ? {'key': {'nestedKey':'value'}} : {'key': 'value2'}",
			},
			wantErr: false,
		},
		{
			name:    "Multiple incomplete expressions",
			input:   "${incomplete1 ${incomplete2",
			want:    []string{},
			wantErr: true,
		},
		{
			name:    "Mixed complete and incomplete",
			input:   "${complete} ${complete2} ${incomplete",
			want:    []string{"complete", "complete2"},
			wantErr: false,
		},
		{
			name:    "Mixed incomplete and complete",
			input:   "${incomplete ${complete}",
			want:    []string{},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := extractExpressions(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("extractExpressions() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !equalStrings(got, tt.want) {
				t.Errorf("extractExpressions() = %v, want %v", got, tt.want)
			}
		})
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i, s := range a {
		if s != b[i] {
			return false
		}
	}
	return true
}

func TestIsOneShotExpression(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    bool
		wantErr bool
	}{
		{"Simple one-shot", "${resource.field}", true, false},
		{"One-shot with function", "${length(resource.list)}", true, false},
		{"Not one-shot prefix", "prefix-${resource.field}", false, false},
		{"Not one-shot suffix", "${resource.field}-suffix", false, false},
		{"Not one-shot multiple", "${resource1.field}${resource2.field}", false, false},
		{"Not expression", "plain string", false, false},
		{"Empty string", "", false, false},
		{"Incomplete expression", "${incomplete", false, false},
		{"With map access", "${resource.map['key']}", true, false},
		{"With list index", "${resource.list[0]}", true, false},
		{"With escaped quotes", "${resource.field == \"escaped\\\"quote\"}", true, false},
		{"With newlines", "${resource.list.map(\n  x,\n  x * 2\n)}", true, false},
		{"Complex expression", "${resource.list.map(x, x.field).filter(y, y > 5)}", true, false},
		{"Nested expression (should error)", "${outer(${inner})}", false, true},
		{"Nested expression but with quotes", "${outer(\"${inner}\")}", true, false},
		{"Nested closing brace without opening one", "${\"text with }} inside\"}", true, false},
		{"Nested open brace without closing one", "${\"text with { inside\"}", true, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := isStandaloneExpression(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("isOneShotExpression() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("isOneShotExpression() = %v, want %v", got, tt.want)
			}
		})
	}
}
