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

package registry

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	expv1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
)

// rawJSON builds a RawExtension from a JSON string.
func rawJSON(s string) *runtime.RawExtension {
	return &runtime.RawExtension{Raw: []byte(s)}
}

// TestHashSpec covers stability and sensitivity:
//   - same spec → same hash (twice)
//   - reordered nodes / RawExtension keys → same hash
//   - nil-vs-empty collection → same hash
//   - genuinely different specs → different hash
//
// Each row supplies two specs and an expectation on whether they hash equal.
func TestHashSpec(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		a         expv1alpha1.GraphSpec
		b         expv1alpha1.GraphSpec
		wantEqual bool
	}{
		{
			name: "identical specs",
			a: expv1alpha1.GraphSpec{Nodes: []expv1alpha1.Node{
				{ID: "n", Def: rawJSON(`{"a":1}`)},
			}},
			b: expv1alpha1.GraphSpec{Nodes: []expv1alpha1.Node{
				{ID: "n", Def: rawJSON(`{"a":1}`)},
			}},
			wantEqual: true,
		},
		{
			name: "node order is normalized",
			a: expv1alpha1.GraphSpec{Nodes: []expv1alpha1.Node{
				{ID: "a", Def: rawJSON(`{}`)},
				{ID: "b", Def: rawJSON(`{}`)},
			}},
			b: expv1alpha1.GraphSpec{Nodes: []expv1alpha1.Node{
				{ID: "b", Def: rawJSON(`{}`)},
				{ID: "a", Def: rawJSON(`{}`)},
			}},
			wantEqual: true,
		},
		{
			name: "RawExtension key order is normalized",
			a: expv1alpha1.GraphSpec{Nodes: []expv1alpha1.Node{
				{ID: "n", Def: rawJSON(`{"a":1,"b":2}`)},
			}},
			b: expv1alpha1.GraphSpec{Nodes: []expv1alpha1.Node{
				{ID: "n", Def: rawJSON(`{"b":2,"a":1}`)},
			}},
			wantEqual: true,
		},
		{
			name:      "nil vs empty nodes hash equally",
			a:         expv1alpha1.GraphSpec{Nodes: nil},
			b:         expv1alpha1.GraphSpec{Nodes: []expv1alpha1.Node{}},
			wantEqual: true,
		},
		{
			name: "nil vs empty forEach hash equally",
			a: expv1alpha1.GraphSpec{Nodes: []expv1alpha1.Node{
				{ID: "n", Def: rawJSON(`{}`)},
			}},
			b: expv1alpha1.GraphSpec{Nodes: []expv1alpha1.Node{
				{ID: "n", Def: rawJSON(`{}`), ForEach: []expv1alpha1.ForEachDimension{}},
			}},
			wantEqual: true,
		},
		{
			name: "different node id → different hash",
			a: expv1alpha1.GraphSpec{Nodes: []expv1alpha1.Node{
				{ID: "a", Def: rawJSON(`{}`)},
			}},
			b: expv1alpha1.GraphSpec{Nodes: []expv1alpha1.Node{
				{ID: "b", Def: rawJSON(`{}`)},
			}},
			wantEqual: false,
		},
		{
			name: "different RawExtension value → different hash",
			a: expv1alpha1.GraphSpec{Nodes: []expv1alpha1.Node{
				{ID: "n", Def: rawJSON(`{"a":1}`)},
			}},
			b: expv1alpha1.GraphSpec{Nodes: []expv1alpha1.Node{
				{ID: "n", Def: rawJSON(`{"a":2}`)},
			}},
			wantEqual: false,
		},
		{
			name: "different node kind (template vs def) → different hash",
			a: expv1alpha1.GraphSpec{Nodes: []expv1alpha1.Node{
				{ID: "n", Template: rawJSON(`{}`)},
			}},
			b: expv1alpha1.GraphSpec{Nodes: []expv1alpha1.Node{
				{ID: "n", Def: rawJSON(`{}`)},
			}},
			wantEqual: false,
		},
		{
			name: "forEach order is preserved (semantic)",
			a: expv1alpha1.GraphSpec{Nodes: []expv1alpha1.Node{{
				ID:      "n",
				Def:     rawJSON(`{}`),
				ForEach: []expv1alpha1.ForEachDimension{{"r": "${a}"}, {"t": "${b}"}},
			}}},
			b: expv1alpha1.GraphSpec{Nodes: []expv1alpha1.Node{{
				ID:      "n",
				Def:     rawJSON(`{}`),
				ForEach: []expv1alpha1.ForEachDimension{{"t": "${b}"}, {"r": "${a}"}},
			}}},
			wantEqual: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h1, err := HashSpec(tc.a)
			require.NoError(t, err)
			h2, err := HashSpec(tc.b)
			require.NoError(t, err)
			if tc.wantEqual {
				assert.Equal(t, h1, h2)
			} else {
				assert.NotEqual(t, h1, h2)
			}
		})
	}
}

// TestHashSpec_ErrorPaths exercises the failure branches of hashSpecWith
// and its helpers — the marshalFunc seam, the empty-RawExtension
// short-circuit, the bad-JSON parse path, and the marshal-error-after-
// normalize path. Driven through hashSpecWith / normalizeRawExtension so
// each row can inject a custom marshaler.
func TestHashSpec_ErrorPaths(t *testing.T) {
	t.Parallel()
	// firstNthenFail returns a marshaler that delegates to encoding/json
	// for the first n calls and then errors. Lets a row trigger a
	// failure deep in the normalize pipeline rather than at the top.
	firstNthenFail := func(n int) func(any) ([]byte, error) {
		calls := 0
		return func(v any) ([]byte, error) {
			calls++
			if calls > n {
				return nil, errors.New("forced marshal failure")
			}
			return json.Marshal(v)
		}
	}

	cases := []struct {
		name    string
		spec    expv1alpha1.GraphSpec
		marshal func(any) ([]byte, error)
		wantErr string
	}{
		{
			name:    "top-level marshal failure",
			spec:    expv1alpha1.GraphSpec{Nodes: []expv1alpha1.Node{{ID: "n"}}},
			marshal: func(any) ([]byte, error) { return nil, errors.New("forced") },
			wantErr: "forced",
		},
		{
			name: "invalid JSON in Raw surfaces parse error",
			spec: expv1alpha1.GraphSpec{Nodes: []expv1alpha1.Node{
				{ID: "n", Def: &runtime.RawExtension{Raw: []byte("not json")}},
			}},
			marshal: json.Marshal,
			wantErr: "parse raw",
		},
		{
			name: "RawExtension canonical-marshal failure inside normalize",
			spec: expv1alpha1.GraphSpec{Nodes: []expv1alpha1.Node{
				{ID: "n", Def: &runtime.RawExtension{Object: &unstructuredLike{A: 1}}},
			}},
			// First marshal: Object → Raw fallback succeeds. Second
			// marshal: canonical-JSON write fails.
			marshal: firstNthenFail(1),
			wantErr: "forced marshal failure",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := hashSpecWith(tc.spec, tc.marshal)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// TestNormalizeRawExtension covers the success branches of
// normalizeRawExtension that don't surface through HashSpec's main table.
func TestNormalizeRawExtension(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		ext       runtime.RawExtension
		wantEmpty bool
	}{
		{name: "empty Raw and nil Object produces empty extension", ext: runtime.RawExtension{}, wantEmpty: true},
		{name: "whitespace-only Raw collapses to empty", ext: runtime.RawExtension{Raw: []byte("   ")}, wantEmpty: true},
		{name: "well-formed Raw is preserved canonically", ext: runtime.RawExtension{Raw: []byte(`{"a":1}`)}, wantEmpty: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, err := normalizeRawExtension(tc.ext, json.Marshal)
			require.NoError(t, err)
			if tc.wantEmpty {
				assert.Empty(t, out.Raw)
				return
			}
			assert.NotEmpty(t, out.Raw)
		})
	}
}

// TestHashSpec_RawExtensionFromObject covers the Object → bytes fallback
// used when Raw is empty but the API object form is present. Asserts the
// Raw and Object forms hash to the same value.
func TestHashSpec_RawExtensionFromObject(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		a, b expv1alpha1.GraphSpec
	}{
		{
			name: "Object-only RawExtension hashes identically to Raw-only equivalent",
			a: expv1alpha1.GraphSpec{Nodes: []expv1alpha1.Node{
				{ID: "n", Def: &runtime.RawExtension{Raw: []byte(`{"a":1}`)}},
			}},
			b: expv1alpha1.GraphSpec{Nodes: []expv1alpha1.Node{
				{ID: "n", Def: &runtime.RawExtension{Object: &unstructuredLike{A: 1}}},
			}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h1, err := HashSpec(tc.a)
			require.NoError(t, err)
			h2, err := HashSpec(tc.b)
			require.NoError(t, err)
			assert.Equal(t, h1, h2)
		})
	}
}

// unstructuredLike is a tiny runtime.Object stand-in whose JSON marshal
// matches `{"a":<value>}`. It exists only to populate RawExtension.Object
// without dragging in an unstructured.Unstructured allocation.
type unstructuredLike struct {
	A int `json:"a"`
}

func (*unstructuredLike) GetObjectKind() schema.ObjectKind { return schema.EmptyObjectKind }
func (u *unstructuredLike) DeepCopyObject() runtime.Object {
	cp := *u
	return &cp
}
