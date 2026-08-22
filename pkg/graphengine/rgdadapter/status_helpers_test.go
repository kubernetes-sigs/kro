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

package rgdadapter

import (
	"errors"
	"fmt"
	"testing"

	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/kubernetes-sigs/kro/pkg/cel/library"
)

// isDataPendingCEL decides whether a failed status expression means "the
// cluster has not produced this value yet" (requeue) or "the expression is
// wrong" (fail the reconcile). Collapsing that distinction either wedges an
// instance that would have converged or hides a real authoring bug.
func TestIsDataPendingCEL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil is not data-pending", err: nil, want: false},
		{
			name: "a missing map key is data-pending",
			err:  errors.New(`no such key: arn`),
			want: true,
		},
		{
			name: "a missing field is data-pending",
			err:  errors.New(`no such field 'status'`),
			want: true,
		},
		{
			name: "a missing attribute is data-pending",
			err:  errors.New(`no such attribute(s): bucket.status`),
			want: true,
		},
		{
			name: "an out-of-bounds index is data-pending",
			err:  errors.New(`index out of bounds: 3`),
			want: true,
		},
		{
			name: "the pattern is found through error wrapping",
			err:  fmt.Errorf("eval %q: %w", "bucket.status.arn", errors.New("no such key: arn")),
			want: true,
		},
		{
			name: "a type mismatch is a hard error, not data-pending",
			err:  errors.New("no such overload: string + int"),
			want: false,
		},
		{
			name: "division by zero is a hard error",
			err:  errors.New("division by zero"),
			want: false,
		},
		{
			name: "an unrelated failure is a hard error",
			err:  errors.New("connection refused"),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, isDataPendingCEL(tt.err))
		})
	}
}

// setAtPath and getAtPath are the dotted-path accessors used to assemble the
// projected status map. They handle map levels only; anything else is a
// not-found rather than a panic.
func TestSetAndGetAtPath(t *testing.T) {
	t.Parallel()

	t.Run("a top-level key round-trips", func(t *testing.T) {
		t.Parallel()
		m := map[string]any{}
		require.NoError(t, setAtPath(m, "state", "ACTIVE"))
		got, found := getAtPath(m, "state")
		assert.True(t, found)
		assert.Equal(t, "ACTIVE", got)
	})

	t.Run("a nested path creates intermediate maps", func(t *testing.T) {
		t.Parallel()
		m := map[string]any{}
		require.NoError(t, setAtPath(m, "bucket.status.arn", "arn:aws:s3:::b"))
		got, found := getAtPath(m, "bucket.status.arn")
		assert.True(t, found)
		assert.Equal(t, "arn:aws:s3:::b", got)
	})

	t.Run("an existing intermediate map is reused, not replaced", func(t *testing.T) {
		t.Parallel()
		m := map[string]any{}
		require.NoError(t, setAtPath(m, "net.vpcID", "vpc-1"))
		require.NoError(t, setAtPath(m, "net.subnetID", "subnet-1"))

		vpc, found := getAtPath(m, "net.vpcID")
		require.True(t, found, "the first write must survive the second")
		assert.Equal(t, "vpc-1", vpc)
		subnet, found := getAtPath(m, "net.subnetID")
		require.True(t, found)
		assert.Equal(t, "subnet-1", subnet)
	})

	t.Run("a scalar blocking an intermediate path is an error", func(t *testing.T) {
		t.Parallel()
		m := map[string]any{"net": "not-a-map"}
		err := setAtPath(m, "net.vpcID", "vpc-1")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "net")
		assert.Contains(t, err.Error(), "string", "the error should report the conflicting type")
	})

	t.Run("a missing path is not found", func(t *testing.T) {
		t.Parallel()
		_, found := getAtPath(map[string]any{"a": "1"}, "b")
		assert.False(t, found)
	})

	t.Run("a scalar intermediate is not found rather than a panic", func(t *testing.T) {
		t.Parallel()
		_, found := getAtPath(map[string]any{"a": "scalar"}, "a.b.c")
		assert.False(t, found)
	})
}

// unwrapExpr strips a standalone ${...} wrapper so the expression can be handed
// to CEL directly.
func TestUnwrapExpr(t *testing.T) {
	t.Parallel()
	tests := []struct{ in, want string }{
		{"${bucket.status.arn}", "bucket.status.arn"},
		{"bucket.status.arn", "bucket.status.arn"},
		{"  ${bucket.status.arn}  ", "bucket.status.arn"},
		{"${a} and ${b}", "a} and ${b"},
		{"${unterminated", "${unterminated"},
		{"", ""},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, unwrapExpr(tt.in))
		})
	}
}

// dedupConditionTypes drops EVERY occurrence of a duplicated condition type
// rather than keeping the first. A duplicate means the author's expressions
// disagree about one condition, and picking a winner arbitrarily would make
// the instance's status depend on evaluation order.
func TestDedupConditionTypes(t *testing.T) {
	t.Parallel()

	cond := func(condType, status string) library.Condition {
		return library.Condition{ConditionType: condType, Status: status}
	}

	t.Run("distinct types are all kept", func(t *testing.T) {
		t.Parallel()
		kept, dups := dedupConditionTypes([]library.Condition{
			cond("Ready", "True"), cond("Synced", "False"),
		})
		assert.Len(t, kept, 2)
		assert.Empty(t, dups)
	})

	t.Run("a duplicated type is dropped entirely, not deduplicated", func(t *testing.T) {
		t.Parallel()
		kept, dups := dedupConditionTypes([]library.Condition{
			cond("Ready", "True"), cond("Ready", "False"), cond("Synced", "True"),
		})
		require.Len(t, kept, 1, "both Ready entries must be dropped")
		assert.Equal(t, "Synced", kept[0].ConditionType)
		assert.Equal(t, []string{"Ready"}, dups)
	})

	t.Run("multiple duplicated types are reported sorted", func(t *testing.T) {
		t.Parallel()
		kept, dups := dedupConditionTypes([]library.Condition{
			cond("Synced", "True"), cond("Synced", "False"),
			cond("Ready", "True"), cond("Ready", "False"),
		})
		assert.Empty(t, kept)
		assert.Equal(t, []string{"Ready", "Synced"}, dups,
			"duplicate types must be reported in a stable order")
	})

	t.Run("an empty input yields no conditions and no duplicates", func(t *testing.T) {
		t.Parallel()
		kept, dups := dedupConditionTypes(nil)
		assert.Empty(t, kept)
		assert.Empty(t, dups)
	})
}

// flattenConditionValue accepts either a single Condition or a list of them
// (collection expansion), and rejects anything else with an error naming the
// expression so the author can find it.
func TestFlattenConditionValue(t *testing.T) {
	t.Parallel()

	t.Run("a single Condition is wrapped in a one-element slice", func(t *testing.T) {
		t.Parallel()
		out, err := flattenConditionValue(
			&library.Condition{ConditionType: "Ready", Status: "True"}, "expr")
		require.NoError(t, err)
		require.Len(t, out, 1)
		assert.Equal(t, "Ready", out[0].ConditionType)
	})

	t.Run("a list of Conditions is flattened in order", func(t *testing.T) {
		t.Parallel()
		list := types.NewRefValList(types.DefaultTypeAdapter, []ref.Val{
			&library.Condition{ConditionType: "Ready", Status: "True"},
			&library.Condition{ConditionType: "Synced", Status: "False"},
		})
		out, err := flattenConditionValue(list, "expr")
		require.NoError(t, err)
		require.Len(t, out, 2)
		assert.Equal(t, "Ready", out[0].ConditionType)
		assert.Equal(t, "Synced", out[1].ConditionType)
	})

	t.Run("a nil value is an error naming the expression", func(t *testing.T) {
		t.Parallel()
		_, err := flattenConditionValue(nil, "myExpr")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "myExpr")
	})

	t.Run("a non-Condition value is an error naming the expression", func(t *testing.T) {
		t.Parallel()
		_, err := flattenConditionValue(types.String("nope"), "myExpr")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "myExpr")
	})

	t.Run("a list containing a non-Condition element is an error", func(t *testing.T) {
		t.Parallel()
		list := types.NewRefValList(types.DefaultTypeAdapter, []ref.Val{
			&library.Condition{ConditionType: "Ready", Status: "True"},
			types.String("nope"),
		})
		_, err := flattenConditionValue(list, "myExpr")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not a Condition")
	})
}
