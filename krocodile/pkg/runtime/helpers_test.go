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

package runtime

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"sigs.k8s.io/krocodile/pkg/compiler"
	"sigs.k8s.io/krocodile/pkg/testutil/generator"
)

// cm builds an unstructured ConfigMap with the given namespace/name so the
// identityKey-driven helpers have a stable, complete identity to key on.
func cm(ns, name string) *unstructured.Unstructured {
	o := &unstructured.Unstructured{}
	o.SetAPIVersion("v1")
	o.SetKind("ConfigMap")
	o.SetNamespace(ns)
	o.SetName(name)
	return o
}

// TestOrderedIntersection pins the observed→desired alignment used so
// collection readyWhen sees a deterministic order regardless of the
// LIST result order.
func TestOrderedIntersection(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		observed []*unstructured.Unstructured
		desired  []*unstructured.Unstructured
		wantName []string
	}{
		{
			name:     "empty observed short-circuits to observed",
			observed: nil,
			desired:  []*unstructured.Unstructured{cm("d", "a")},
			wantName: nil,
		},
		{
			name:     "empty desired short-circuits to observed",
			observed: []*unstructured.Unstructured{cm("d", "a")},
			desired:  nil,
			wantName: []string{"a"},
		},
		{
			name: "observed reordered to match desired sequence",
			observed: []*unstructured.Unstructured{
				cm("d", "c"), cm("d", "a"), cm("d", "b"),
			},
			desired: []*unstructured.Unstructured{
				cm("d", "a"), cm("d", "b"), cm("d", "c"),
			},
			wantName: []string{"a", "b", "c"},
		},
		{
			name: "desired entries absent from observed are dropped",
			observed: []*unstructured.Unstructured{
				cm("d", "a"), cm("d", "c"),
			},
			desired: []*unstructured.Unstructured{
				cm("d", "a"), cm("d", "b"), cm("d", "c"),
			},
			wantName: []string{"a", "c"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := orderedIntersection(tc.observed, tc.desired)
			var names []string
			for _, o := range got {
				names = append(names, o.GetName())
			}
			assert.Equal(t, tc.wantName, names)
		})
	}
}

// TestValidateUniqueIdentities pins the duplicate-name guard run after
// collection expansion.
func TestValidateUniqueIdentities(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		objs    []*unstructured.Unstructured
		wantErr string
	}{
		{
			name: "all distinct identities pass",
			objs: []*unstructured.Unstructured{cm("d", "a"), cm("d", "b")},
		},
		{
			name:    "duplicate identity is rejected",
			objs:    []*unstructured.Unstructured{cm("d", "a"), cm("d", "a")},
			wantErr: "duplicate identity",
		},
		{
			name: "same name in different namespaces is distinct",
			objs: []*unstructured.Unstructured{cm("ns1", "a"), cm("ns2", "a")},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateUniqueIdentities(tc.objs)
			if tc.wantErr != "" {
				assert.ErrorContains(t, err, tc.wantErr)
				return
			}
			assert.NoError(t, err)
		})
	}
}

// TestIsDataPending and the CEL classifier pin the error-sentinel helpers
// the executor branches on.
func TestErrorClassifiers(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name           string
		err            error
		wantDataPend   bool
		wantCELPending bool
	}{
		{name: "nil", err: nil, wantDataPend: false, wantCELPending: false},
		{
			name:         "wrapped ErrDataPending is data-pending",
			err:          fmt.Errorf("ctx: %w", ErrDataPending),
			wantDataPend: true,
		},
		{
			name:           "no such key is a CEL data-pending",
			err:            errors.New("no such key: status"),
			wantCELPending: true,
		},
		{
			name:           "no such field is a CEL data-pending",
			err:            errors.New("no such field 'ready'"),
			wantCELPending: true,
		},
		{
			name:           "index out of bounds is a CEL data-pending",
			err:            errors.New("index out of bounds: 3"),
			wantCELPending: true,
		},
		{
			name:           "type error is not CEL data-pending",
			err:            errors.New("no matching overload for '_+_'"),
			wantCELPending: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.wantDataPend, IsDataPending(tc.err))
			assert.Equal(t, tc.wantCELPending, isCELDataPending(tc.err))
		})
	}
}

// TestNodeAccessors_DynamicAndObserved covers the accessors not exercised
// by the existing TestNodeAccessors table.
func TestNodeAccessors_DynamicAndObserved(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		run  func(t *testing.T)
	}{
		{
			name: "DynamicGVK reflects spec.DynamicGVK",
			run: func(t *testing.T) {
				n := &Node{spec: &compiler.Node{DynamicGVK: true}}
				assert.True(t, n.DynamicGVK())
				assert.False(t, (&Node{spec: &compiler.Node{}}).DynamicGVK())
			},
		},
		{
			name: "Observed is nil before SetObserved and returns the recorded slice after",
			run: func(t *testing.T) {
				n := &Node{spec: &compiler.Node{ID: "x"}}
				assert.Nil(t, n.Observed())
				obs := []*unstructured.Unstructured{cm("d", "a")}
				n.SetObserved(obs, obs)
				assert.Equal(t, obs, n.Observed())
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.run(t)
		})
	}
}

// TestCartesianProduct_EmptyDimensions pins the SQL outer-join
// short-circuit: any zero-length axis collapses the whole product to nil.
func TestCartesianProduct_EmptyDimensions(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		dims    []evaluatedDimension
		wantLen int
		wantNil bool
	}{
		{
			name:    "no dimensions yields a single empty row",
			dims:    nil,
			wantLen: 1,
		},
		{
			name: "a zero-length axis collapses the product to nil",
			dims: []evaluatedDimension{
				{name: "a", values: []any{"x", "y"}},
				{name: "b", values: []any{}},
			},
			wantNil: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := cartesianProduct(tc.dims, DefaultMaxCollectionSize)
			assert.NoError(t, err)
			if tc.wantNil {
				assert.Nil(t, got)
				return
			}
			assert.Len(t, got, tc.wantLen)
		})
	}
}

// TestSetObserved_Collection pins the collection branch of SetObserved:
// observed items are realigned to desired order via orderedIntersection,
// while a singleton node stores observed verbatim.
func TestSetObserved_Collection(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		collected bool
		observed  []*unstructured.Unstructured
		desired   []*unstructured.Unstructured
		wantOrder []string
	}{
		{
			name:      "collection realigns observed to desired order",
			collected: true,
			observed:  []*unstructured.Unstructured{cm("d", "b"), cm("d", "a")},
			desired:   []*unstructured.Unstructured{cm("d", "a"), cm("d", "b")},
			wantOrder: []string{"a", "b"},
		},
		{
			name:      "singleton stores observed verbatim",
			collected: false,
			observed:  []*unstructured.Unstructured{cm("d", "b"), cm("d", "a")},
			desired:   []*unstructured.Unstructured{cm("d", "a"), cm("d", "b")},
			wantOrder: []string{"b", "a"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			spec := &compiler.Node{ID: "x"}
			if tc.collected {
				// A non-empty ForEach makes IsCollection() report true.
				spec.ForEach = []compiler.ForEachDimension{{Name: "n"}}
			}
			n := &Node{spec: spec}
			n.SetObserved(tc.observed, tc.desired)
			got := make([]string, 0, len(n.Observed()))
			for _, o := range n.Observed() {
				got = append(got, o.GetName())
			}
			assert.Equal(t, tc.wantOrder, got)
		})
	}
}

// TestIsIgnored_Memoized pins the cache: the second IsIgnored call returns
// the stashed result (and cause) without re-walking deps or re-evaluating.
func TestIsIgnored_Memoized(t *testing.T) {
	t.Parallel()
	g := generator.NewGraph("g",
		generator.WithDef("flag", map[string]any{"enabled": false}),
		generator.WithDef("guarded", map[string]any{"x": "y"}),
		generator.WithIncludeWhen("${flag.enabled}"),
	)
	prog := compileGraph(t, g)
	rt := New(prog, g)
	setFirst(rt, "flag")

	n := rt.Node("guarded")
	first, err := n.IsIgnored()
	assert.NoError(t, err)
	assert.True(t, first)

	// Second call must hit the memo branch and return the same answer.
	second, err := n.IsIgnored()
	assert.NoError(t, err)
	assert.Equal(t, first, second)
}

// TestResolve_DuplicateIdentities pins the post-expansion uniqueness guard:
// a collection whose identity-field expressions collapse to the same name
// across instances is rejected rather than handed to SSA.
func TestResolve_DuplicateIdentities(t *testing.T) {
	t.Parallel()
	g := generator.NewGraph("g",
		// Two identical iterator values render the same metadata.name, so
		// the rendered objects share an identity.
		generator.WithDef("src", map[string]any{"names": []any{"dup", "dup"}}),
		generator.WithTemplate("p", map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "${'cm-' + n}"},
			"data":     map[string]any{"k": "v"},
		}, generator.ForEachDim("n", "${src.names}")),
	)
	prog := compileGraph(t, g)
	rt := New(prog, g)
	setFirst(rt, "src")

	_, err := rt.Node("p").Resolve()
	assert.ErrorContains(t, err, "duplicate identity")
}

// TestComputeIgnored_DepErrorPropagates pins the contagious-error branch:
// when an upstream dep's includeWhen fails, the failure surfaces from the
// downstream node's IsIgnored prefixed with the dep id, and CheckReadiness
// surfaces the same error through its own IsIgnored call.
func TestComputeIgnored_DepErrorPropagates(t *testing.T) {
	t.Parallel()
	g := generator.NewGraph("g",
		generator.WithDef("seed", map[string]any{"k": "v"}),
		// dyn field resolves to a string; the includeWhen result is not a
		// bool, so the upstream "mid" node's IsIgnored returns a hard error.
		generator.WithDef("cfg", map[string]any{"flag": "${'literal'}"}),
		generator.WithDef("mid", map[string]any{"x": "y"}),
		generator.WithIncludeWhen("${cfg.flag}"),
		generator.WithDef("leaf", map[string]any{"y": "${mid.x}"}),
	)
	prog := compileGraph(t, g)
	rt := New(prog, g)
	setFirst(rt, "cfg")

	_, ignErr := rt.Node("leaf").IsIgnored()
	require.Error(t, ignErr)
	assert.Contains(t, ignErr.Error(), `dep "mid"`)

	// CheckReadiness on the same node bubbles the IsIgnored error up.
	rdErr := rt.Node("leaf").CheckReadiness()
	require.Error(t, rdErr)
	assert.Contains(t, rdErr.Error(), `dep "mid"`)
}

// TestEvalList_Nil pins the evalList branch that rejects a nil forEach
// source (distinct from the wrong-type branch covered elsewhere).
func TestEvalList_Nil(t *testing.T) {
	t.Parallel()
	g := generator.NewGraph("g",
		generator.WithDef("seed", map[string]any{"k": "v"}),
		// dyn field that resolves to null at runtime; the static forEach
		// list check can't reject it, so evalList hits the nil case.
		generator.WithDef("src", map[string]any{"value": "${dyn(null)}"}),
		generator.WithTemplate("p", map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "${'cm-' + n}"},
			"data":     map[string]any{"k": "v"},
		}, generator.ForEachDim("n", "${src.value}")),
	)
	prog := compileGraph(t, g)
	rt := New(prog, g)
	setFirst(rt, "src")

	_, err := rt.Node("p").Resolve()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "expected list, got nil")
}

// TestWithSeedScope pins the child-scope seeding option: seeded values are
// readable, the copy is independent of the source map, and a later New()
// scope write does not leak back to the seed.
func TestWithSeedScope(t *testing.T) {
	t.Parallel()
	g := generator.NewGraph("g",
		generator.WithNamespace("default"),
		generator.WithDef("seed", map[string]any{"k": "v"}),
	)
	prog := compileGraph(t, g)

	parentScope := map[string]any{"captured": map[string]any{"name": "from-parent"}}
	rt := New(prog, g, WithSeedScope(parentScope))

	assert.Equal(t, map[string]any{"name": "from-parent"}, rt.Scope()["captured"])

	// Mutating the child scope must not reach back into the source map.
	rt.Set("captured", "overwritten")
	assert.Equal(t, map[string]any{"name": "from-parent"}, parentScope["captured"],
		"seed is copied; child mutation must not leak back to the source")
}
