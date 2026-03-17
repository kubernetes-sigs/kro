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

package library

import (
	"testing"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types/ref"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newListsEnv creates a CEL environment with only the Lists library registered.
func newListsEnv(t *testing.T) *cel.Env {
	t.Helper()
	env, err := cel.NewEnv(Lists())
	require.NoError(t, err)
	return env
}

// evalList compiles and evaluates a CEL expression, returning the result as
// a []interface{} for uniform element comparison.
func evalList(t *testing.T, env *cel.Env, expr string) []interface{} {
	t.Helper()
	ast, issues := env.Compile(expr)
	require.NoError(t, issues.Err(), "compile %q", expr)
	prg, err := env.Program(ast)
	require.NoError(t, err)
	out, _, err := prg.Eval(map[string]interface{}{})
	require.NoError(t, err)
	return toIfaceSlice(t, out.Value())
}

// evalErr compiles and evaluates a CEL expression and returns the error string.
// The test fails if no error is produced.
func evalErr(t *testing.T, env *cel.Env, expr string) string {
	t.Helper()
	ast, issues := env.Compile(expr)
	if issues != nil && issues.Err() != nil {
		return issues.Err().Error()
	}
	prg, err := env.Program(ast)
	require.NoError(t, err)
	_, _, err = prg.Eval(map[string]interface{}{})
	require.Error(t, err, "expected runtime error for %q", expr)
	return err.Error()
}

func toIfaceSlice(t *testing.T, v interface{}) []interface{} {
	t.Helper()
	switch s := v.(type) {
	case []int64:
		out := make([]interface{}, len(s))
		for i, e := range s {
			out[i] = e
		}
		return out
	case []string:
		out := make([]interface{}, len(s))
		for i, e := range s {
			out[i] = e
		}
		return out
	case []ref.Val:
		out := make([]interface{}, len(s))
		for i, e := range s {
			out[i] = e.Value()
		}
		return out
	case []interface{}:
		return s
	default:
		t.Fatalf("toIfaceSlice: unhandled type %T", v)
		return nil
	}
}

// ── lists.setAtIndex ─────────────────────────────────────────────────────────

func TestListsSetAtIndex(t *testing.T) {
	env := newListsEnv(t)
	tests := []struct {
		name    string
		expr    string
		want    []interface{}
		wantErr string
	}{
		{
			name: "replace middle int",
			expr: "lists.setAtIndex([1, 2, 3], 1, 99)",
			want: []interface{}{int64(1), int64(99), int64(3)},
		},
		{
			name: "replace first int",
			expr: "lists.setAtIndex([1, 2, 3], 0, 0)",
			want: []interface{}{int64(0), int64(2), int64(3)},
		},
		{
			name: "replace last int",
			expr: "lists.setAtIndex([1, 2, 3], 2, 7)",
			want: []interface{}{int64(1), int64(2), int64(7)},
		},
		{
			name: "replace string element",
			expr: `lists.setAtIndex(["a", "b", "c"], 1, "z")`,
			want: []interface{}{"a", "z", "c"},
		},
		{
			name: "replace first string",
			expr: `lists.setAtIndex(["x", "y"], 0, "replaced")`,
			want: []interface{}{"replaced", "y"},
		},
		{
			name: "single-element list",
			expr: "lists.setAtIndex([42], 0, 0)",
			want: []interface{}{int64(0)},
		},
		{
			name:    "index out of bounds high",
			expr:    "lists.setAtIndex([1, 2], 5, 9)",
			wantErr: "out of bounds",
		},
		{
			name:    "index out of bounds negative",
			expr:    "lists.setAtIndex([1, 2], -1, 9)",
			wantErr: "out of bounds",
		},
		{
			name:    "index equal to size is out of bounds",
			expr:    "lists.setAtIndex([1, 2], 2, 9)",
			wantErr: "out of bounds",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.wantErr != "" {
				assert.Contains(t, evalErr(t, env, tt.expr), tt.wantErr)
				return
			}
			assert.Equal(t, tt.want, evalList(t, env, tt.expr))
		})
	}
}

// ── lists.insertAtIndex ──────────────────────────────────────────────────────

func TestListsInsertAtIndex(t *testing.T) {
	env := newListsEnv(t)
	tests := []struct {
		name    string
		expr    string
		want    []interface{}
		wantErr string
	}{
		{
			name: "insert in middle",
			expr: "lists.insertAtIndex([1, 2, 3], 1, 99)",
			want: []interface{}{int64(1), int64(99), int64(2), int64(3)},
		},
		{
			name: "insert at front",
			expr: "lists.insertAtIndex([1, 2, 3], 0, 99)",
			want: []interface{}{int64(99), int64(1), int64(2), int64(3)},
		},
		{
			name: "insert at end (append)",
			expr: "lists.insertAtIndex([1, 2, 3], 3, 99)",
			want: []interface{}{int64(1), int64(2), int64(3), int64(99)},
		},
		{
			name: "insert into empty list",
			expr: "lists.insertAtIndex([], 0, 42)",
			want: []interface{}{int64(42)},
		},
		{
			name: "insert string element",
			expr: `lists.insertAtIndex(["a", "c"], 1, "b")`,
			want: []interface{}{"a", "b", "c"},
		},
		{
			name: "insert into single-element list at front",
			expr: "lists.insertAtIndex([2], 0, 1)",
			want: []interface{}{int64(1), int64(2)},
		},
		{
			name:    "index out of bounds high",
			expr:    "lists.insertAtIndex([1, 2], 5, 9)",
			wantErr: "out of bounds",
		},
		{
			name:    "index out of bounds negative",
			expr:    "lists.insertAtIndex([1, 2], -1, 9)",
			wantErr: "out of bounds",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.wantErr != "" {
				assert.Contains(t, evalErr(t, env, tt.expr), tt.wantErr)
				return
			}
			assert.Equal(t, tt.want, evalList(t, env, tt.expr))
		})
	}
}

// ── lists.removeAtIndex ──────────────────────────────────────────────────────

func TestListsRemoveAtIndex(t *testing.T) {
	env := newListsEnv(t)
	tests := []struct {
		name    string
		expr    string
		want    []interface{}
		wantErr string
	}{
		{
			name: "remove middle element",
			expr: "lists.removeAtIndex([1, 2, 3], 1)",
			want: []interface{}{int64(1), int64(3)},
		},
		{
			name: "remove first element",
			expr: "lists.removeAtIndex([1, 2, 3], 0)",
			want: []interface{}{int64(2), int64(3)},
		},
		{
			name: "remove last element",
			expr: "lists.removeAtIndex([1, 2, 3], 2)",
			want: []interface{}{int64(1), int64(2)},
		},
		{
			name: "remove from two-element list",
			expr: "lists.removeAtIndex([10, 20], 0)",
			want: []interface{}{int64(20)},
		},
		{
			name: "remove only element yields empty list",
			expr: "lists.removeAtIndex([42], 0)",
			want: []interface{}{},
		},
		{
			name: "remove string element",
			expr: `lists.removeAtIndex(["a", "b", "c"], 1)`,
			want: []interface{}{"a", "c"},
		},
		{
			name:    "index out of bounds high",
			expr:    "lists.removeAtIndex([1, 2], 5)",
			wantErr: "out of bounds",
		},
		{
			name:    "index out of bounds negative",
			expr:    "lists.removeAtIndex([1, 2], -1)",
			wantErr: "out of bounds",
		},
		{
			name:    "index equal to size is out of bounds",
			expr:    "lists.removeAtIndex([1, 2], 2)",
			wantErr: "out of bounds",
		},
		{
			name:    "empty list",
			expr:    "lists.removeAtIndex([], 0)",
			wantErr: "out of bounds",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.wantErr != "" {
				assert.Contains(t, evalErr(t, env, tt.expr), tt.wantErr)
				return
			}
			assert.Equal(t, tt.want, evalList(t, env, tt.expr))
		})
	}
}

// ── composition ──────────────────────────────────────────────────────────────

func TestListsComposition(t *testing.T) {
	env := newListsEnv(t)
	tests := []struct {
		name string
		expr string
		want []interface{}
	}{
		{
			name: "insertAtIndex then removeAtIndex round-trips",
			expr: "lists.removeAtIndex(lists.insertAtIndex([1, 2, 3], 1, 99), 1)",
			want: []interface{}{int64(1), int64(2), int64(3)},
		},
		{
			name: "setAtIndex then setAtIndex",
			expr: "lists.setAtIndex(lists.setAtIndex([1, 2, 3], 0, 9), 2, 7)",
			want: []interface{}{int64(9), int64(2), int64(7)},
		},
		{
			name: "chain insertAtIndex twice builds ordered list",
			expr: "lists.insertAtIndex(lists.insertAtIndex([2, 3], 0, 1), 3, 4)",
			want: []interface{}{int64(1), int64(2), int64(3), int64(4)},
		},
		{
			name: "removeAtIndex front then setAtIndex",
			expr: "lists.setAtIndex(lists.removeAtIndex([0, 1, 2, 3], 0), 0, 99)",
			want: []interface{}{int64(99), int64(2), int64(3)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, evalList(t, env, tt.expr))
		})
	}
}

// ── type inference ───────────────────────────────────────────────────────────

// TestListsTypeInference verifies that the compiler infers the concrete element
// type rather than collapsing to dyn. This catches regressions where TypeParamType
// is accidentally replaced with DynType in CompileOptions.
func TestListsTypeInference(t *testing.T) {
	env := newListsEnv(t)
	tests := []struct {
		name     string
		expr     string
		wantType string
	}{
		{
			name:     "setAtIndex on int list returns list(int)",
			expr:     "lists.setAtIndex([1, 2, 3], 0, 9)",
			wantType: "list(int)",
		},
		{
			name:     "setAtIndex on string list returns list(string)",
			expr:     `lists.setAtIndex(["a", "b"], 0, "z")`,
			wantType: "list(string)",
		},
		{
			name:     "insertAtIndex on int list returns list(int)",
			expr:     "lists.insertAtIndex([1, 2, 3], 0, 9)",
			wantType: "list(int)",
		},
		{
			name:     "insertAtIndex on string list returns list(string)",
			expr:     `lists.insertAtIndex(["a", "b"], 0, "z")`,
			wantType: "list(string)",
		},
		{
			name:     "removeAtIndex on int list returns list(int)",
			expr:     "lists.removeAtIndex([1, 2, 3], 0)",
			wantType: "list(int)",
		},
		{
			name:     "removeAtIndex on string list returns list(string)",
			expr:     `lists.removeAtIndex(["a", "b", "c"], 0)`,
			wantType: "list(string)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ast, iss := env.Compile(tt.expr)
			require.NoError(t, iss.Err())
			assert.Equal(t, tt.wantType, ast.OutputType().String())
		})
	}
}
