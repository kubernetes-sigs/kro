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

package parser

import (
	"testing"
)

func TestExtractExpressions(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    []exprMatch
		wantErr bool
	}{
		{
			name:  "Simple expression",
			input: "${resource.field}",
			want:  []exprMatch{{expr: "resource.field", start: 0, end: 17}},
		},
		{
			name:  "Expression with function",
			input: "${length(resource.list)}",
			want:  []exprMatch{{expr: "length(resource.list)", start: 0, end: 24}},
		},
		{
			name:  "Expression with prefix",
			input: "prefix-${resource.field}",
			want:  []exprMatch{{expr: "resource.field", start: 7, end: 24}},
		},
		{
			name:  "Expression with suffix",
			input: "${resource.field}-suffix",
			want:  []exprMatch{{expr: "resource.field", start: 0, end: 17}},
		},
		{
			name:  "Multiple expressions",
			input: "${resource1.field}-middle-${resource2.field}",
			want: []exprMatch{
				{expr: "resource1.field", start: 0, end: 18},
				{expr: "resource2.field", start: 26, end: 44},
			},
		},
		{
			name:  "Expression with map",
			input: "${resource.map['key']}",
			want:  []exprMatch{{expr: "resource.map['key']", start: 0, end: 22}},
		},
		{
			name:  "Expression with list index",
			input: "${resource.list[0]}",
			want:  []exprMatch{{expr: "resource.list[0]", start: 0, end: 19}},
		},
		{
			name:  "Complex expression",
			input: "${resource.field == 'value' && resource.number > 5}",
			want:  []exprMatch{{expr: "resource.field == 'value' && resource.number > 5", start: 0, end: 51}},
		},
		{
			name:  "No expressions",
			input: "plain string",
			want:  nil,
		},
		{
			name:  "Empty string",
			input: "",
			want:  nil,
		},
		{
			name:  "Incomplete expression",
			input: "${incomplete",
			want:  nil,
		},
		{
			name:  "Expression with escaped quotes",
			input: "${resource.field == \"escaped\\\"quote\"}",
			want:  []exprMatch{{expr: "resource.field == \"escaped\\\"quote\"", start: 0, end: 37}},
		},
		{
			name:  "Multiple expressions with whitespace",
			input: "  ${resource1.field}  ${resource2.field}  ",
			want: []exprMatch{
				{expr: "resource1.field", start: 2, end: 20},
				{expr: "resource2.field", start: 22, end: 40},
			},
		},
		{
			name:  "Expression with newlines",
			input: "${resource.list.map(\n  x,\n  x * 2\n)}",
			want:  []exprMatch{{expr: "resource.list.map(\n  x,\n  x * 2\n)", start: 0, end: 36}},
		},
		{
			name:    "Nested expression (should error)",
			input:   "${outer(${inner})} ${outer}",
			want:    nil,
			wantErr: true,
		},
		{
			name:  "Nested expression but with quotes",
			input: "${outer(\"${inner}\")}",
			want:  []exprMatch{{expr: "outer(\"${inner}\")", start: 0, end: 20}},
		},
		{
			name:  "Nested closing brace without opening one",
			input: "${\"text with }} inside\"}",
			want:  []exprMatch{{expr: "\"text with }} inside\"", start: 0, end: 24}},
		},
		{
			name:  "Nested open brace without closing one",
			input: "${\"text with { inside\"}",
			want:  []exprMatch{{expr: "\"text with { inside\"", start: 0, end: 23}},
		},
		{
			name:  "Expressions with dictionary building",
			input: "${true ? {'key': 'value'} : {'key': 'value2'}}",
			want:  []exprMatch{{expr: "true ? {'key': 'value'} : {'key': 'value2'}", start: 0, end: 46}},
		},
		{
			name:  "Multiple expressions with dictionary building",
			input: "${true ? {'key': 'value'} : {'key': 'value2'}} somewhat ${resource.field} then ${false ? {'key': {'nestedKey':'value'}} : {'key': 'value2'}}",
			want: []exprMatch{
				{expr: "true ? {'key': 'value'} : {'key': 'value2'}", start: 0, end: 46},
				{expr: "resource.field", start: 56, end: 73},
				{expr: "false ? {'key': {'nestedKey':'value'}} : {'key': 'value2'}", start: 79, end: 140},
			},
		},
		{
			name:    "Multiple incomplete expressions",
			input:   "${incomplete1 ${incomplete2",
			want:    nil,
			wantErr: true,
		},
		{
			name:  "Mixed complete and incomplete",
			input: "${complete} ${complete2} ${incomplete",
			want: []exprMatch{
				{expr: "complete", start: 0, end: 11},
				{expr: "complete2", start: 12, end: 24},
			},
		},
		{
			name:    "Mixed incomplete and complete",
			input:   "${incomplete ${complete}",
			want:    nil,
			wantErr: true,
		},
		{
			name:  "Ternary with empty map literal",
			input: "${condition ? value : {}}",
			want:  []exprMatch{{expr: "condition ? value : {}", start: 0, end: 25}},
		},
		{
			name:  "Ternary with empty map on both sides",
			input: "${condition ? {} : {}}",
			want:  []exprMatch{{expr: "condition ? {} : {}", start: 0, end: 22}},
		},
		{
			name:  "Complex ternary with empty map (real world example)",
			input: "${schema.spec.deployment.includeAnnotations ? schema.spec.deployment.annotations : {}}",
			want:  []exprMatch{{expr: "schema.spec.deployment.includeAnnotations ? schema.spec.deployment.annotations : {}", start: 0, end: 86}},
		},
		{
			name:  "Ternary with has() and empty map",
			input: "${has(schema.annotations) && includeAnnotations ? schema.annotations : {}}",
			want:  []exprMatch{{expr: "has(schema.annotations) && includeAnnotations ? schema.annotations : {}", start: 0, end: 74}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := extractExpressions(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("extractExpressions() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if len(got) != len(tt.want) {
				t.Errorf("extractExpressions() returned %d matches, want %d", len(got), len(tt.want))
				return
			}
			for i, g := range got {
				w := tt.want[i]
				if g.expr != w.expr || g.start != w.start || g.end != w.end {
					t.Errorf("match[%d] = {expr:%q, start:%d, end:%d}, want {expr:%q, start:%d, end:%d}",
						i, g.expr, g.start, g.end, w.expr, w.start, w.end)
				}
			}
		})
	}
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
