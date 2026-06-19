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
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	expv1alpha1 "sigs.k8s.io/krocodile/api/v1alpha1"
	"sigs.k8s.io/krocodile/pkg/compiler"
)

func key(name string) types.NamespacedName {
	return types.NamespacedName{Namespace: "default", Name: name}
}

func newGraph(def string) *expv1alpha1.Graph {
	return &expv1alpha1.Graph{
		ObjectMeta: metav1.ObjectMeta{Name: "g", Namespace: "default"},
		Spec: expv1alpha1.GraphSpec{Nodes: []expv1alpha1.Node{
			{ID: "n", Def: rawJSON(def)},
		}},
	}
}

// TestRegistry covers the low-level Lookup / Store / Delete surface.
func TestRegistry(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		run  func(t *testing.T, r *Registry)
	}{
		{
			name: "lookup miss on empty registry",
			run: func(t *testing.T, r *Registry) {
				_, hit := r.Lookup(key("g"), "h")
				assert.False(t, hit)
			},
		},
		{
			name: "store then lookup hit",
			run: func(t *testing.T, r *Registry) {
				prog := &compiler.Program{}
				r.Store(key("g"), "h", prog)
				got, hit := r.Lookup(key("g"), "h")
				assert.True(t, hit)
				assert.Same(t, prog, got)
			},
		},
		{
			name: "lookup with stale hash is a miss",
			run: func(t *testing.T, r *Registry) {
				r.Store(key("g"), "h1", &compiler.Program{})
				_, hit := r.Lookup(key("g"), "h2")
				assert.False(t, hit)
			},
		},
		{
			name: "store replaces prior entry",
			run: func(t *testing.T, r *Registry) {
				old := &compiler.Program{}
				r.Store(key("g"), "h", old)
				newP := &compiler.Program{}
				r.Store(key("g"), "h2", newP)
				_, hit := r.Lookup(key("g"), "h")
				assert.False(t, hit, "old hash should miss after replace")
				got, hit := r.Lookup(key("g"), "h2")
				assert.True(t, hit)
				assert.Same(t, newP, got)
			},
		},
		{
			name: "delete drops entry",
			run: func(t *testing.T, r *Registry) {
				r.Store(key("g"), "h", &compiler.Program{})
				r.Delete(key("g"))
				_, hit := r.Lookup(key("g"), "h")
				assert.False(t, hit)
			},
		},
		{
			name: "delete on missing key is a no-op",
			run: func(t *testing.T, r *Registry) {
				r.Delete(key("missing"))
			},
		},
		{
			name: "len reflects size",
			run: func(t *testing.T, r *Registry) {
				assert.Equal(t, 0, r.Len())
				r.Store(key("a"), "h", &compiler.Program{})
				r.Store(key("b"), "h", &compiler.Program{})
				assert.Equal(t, 2, r.Len())
				r.Delete(key("a"))
				assert.Equal(t, 1, r.Len())
			},
		},
		{
			name: "entries are isolated by key",
			run: func(t *testing.T, r *Registry) {
				pa := &compiler.Program{}
				pb := &compiler.Program{}
				r.Store(key("a"), "h", pa)
				r.Store(key("b"), "h", pb)
				got, _ := r.Lookup(key("a"), "h")
				assert.Same(t, pa, got)
				got, _ = r.Lookup(key("b"), "h")
				assert.Same(t, pb, got)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.run(t, New())
		})
	}
}

// TestRegistry_Compile covers the high-level Compile flow: cache hit
// short-circuits compile, miss invokes it, errors propagate without
// poisoning the cache, and a different graph spec under the same key is
// detected as stale.
func TestRegistry_Compile(t *testing.T) {
	t.Parallel()
	type call struct {
		graph     *expv1alpha1.Graph
		compileFn CompileFunc
		wantHit   bool
		wantErr   bool
	}
	cases := []struct {
		name  string
		calls []call
		// post asserts on the Registry after the sequence completes.
		post func(t *testing.T, r *Registry, calls *int)
	}{
		{
			name: "first call misses and stores",
			calls: []call{
				{graph: newGraph(`{"a":1}`), wantHit: false},
			},
			post: func(t *testing.T, r *Registry, calls *int) {
				assert.Equal(t, 1, *calls)
				assert.Equal(t, 1, r.Len())
			},
		},
		{
			name: "second call with identical spec hits",
			calls: []call{
				{graph: newGraph(`{"a":1}`), wantHit: false},
				{graph: newGraph(`{"a":1}`), wantHit: true},
			},
			post: func(t *testing.T, r *Registry, calls *int) {
				assert.Equal(t, 1, *calls, "compile should run only once")
			},
		},
		{
			name: "spec change invalidates and recompiles",
			calls: []call{
				{graph: newGraph(`{"a":1}`), wantHit: false},
				{graph: newGraph(`{"a":2}`), wantHit: false},
			},
			post: func(t *testing.T, r *Registry, calls *int) {
				assert.Equal(t, 2, *calls)
				assert.Equal(t, 1, r.Len())
			},
		},
		{
			name: "compile error does not poison cache",
			calls: []call{
				{graph: newGraph(`{"a":1}`), compileFn: func(*expv1alpha1.Graph) (*compiler.Program, error) {
					return nil, errors.New("boom")
				}, wantErr: true},
				{graph: newGraph(`{"a":1}`), wantHit: false},
			},
			post: func(t *testing.T, r *Registry, _ *int) {
				assert.Equal(t, 1, r.Len(), "successful second compile should be cached")
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := New()
			var compiles int
			defaultCompile := func(*expv1alpha1.Graph) (*compiler.Program, error) {
				compiles++
				return &compiler.Program{}, nil
			}
			for i, c := range tc.calls {
				fn := c.compileFn
				if fn == nil {
					fn = defaultCompile
				}
				prog, hit, err := r.Compile(key(c.graph.Name), c.graph, fn)
				if c.wantErr {
					require.Error(t, err, "call %d", i)
					continue
				}
				require.NoError(t, err, "call %d", i)
				assert.NotNil(t, prog, "call %d", i)
				assert.Equal(t, c.wantHit, hit, "call %d", i)
			}
			if tc.post != nil {
				tc.post(t, r, &compiles)
			}
		})
	}
}
