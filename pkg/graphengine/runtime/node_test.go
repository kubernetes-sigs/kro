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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	memory "k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/restmapper"

	expv1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/compiler"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/testutil/generator"
	testk8s "github.com/kubernetes-sigs/kro/pkg/graphengine/testutil/k8s"
)

// mustCompiler builds a Compiler bound to the FakeResolver + a
// memory-cached discovery REST mapper. Same shape used by pkg/compiler's
// own tests; the exported NewCompilerWithDependencies makes that wiring
// reusable from other packages.
func mustCompiler(t *testing.T) *compiler.Compiler {
	t.Helper()
	r, disco := testk8s.NewFakeResolver()
	rm := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(disco))
	return compiler.NewCompilerWithDependencies(r, rm)
}

// compileGraph compiles g via a fresh test compiler and returns the
// produced Program. Fatal on error so callers don't have to thread the
// error through table rows.
func compileGraph(t *testing.T, g *expv1alpha1.Graph) *compiler.Program {
	t.Helper()
	p, err := mustCompiler(t).Compile(g)
	require.NoError(t, err)
	return p
}

// setFirst is a helper used by populate funcs to take the first (and
// only) resolved output and publish it under the node id. Used by
// non-collection nodes where Resolve returns a single-element slice.
func setFirst(rt *Runtime, id string) {
	objs, _ := rt.Node(id).Resolve()
	rt.Set(id, objs[0].Object)
}

// TestNode_IsIgnored exercises the contagious-ignore semantics: a node
// is ignored when any of its upstream deps is ignored or when its own
// includeWhen folds to false.
func TestNode_IsIgnored(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		graph    *expv1alpha1.Graph
		populate func(rt *Runtime)
		assertID string // node ID to call IsIgnored on; "" → last node
		want     bool
		wantErr  string
	}{
		{
			name: "no includeWhen and no ignored deps → not ignored",
			graph: generator.NewGraph("g",
				generator.WithDef("v", map[string]any{"x": "y"}),
			),
			want: false,
		},
		{
			name: "all includeWhen true → not ignored",
			graph: generator.NewGraph("g",
				generator.WithDef("v", map[string]any{"a": int64(1), "b": int64(2)}),
				generator.WithDef("guarded", map[string]any{"x": "y"}),
				generator.WithIncludeWhen("${v.a == 1}", "${v.b == 2}"),
			),
			populate: func(rt *Runtime) { setFirst(rt, "v") },
			want:     false,
		},
		{
			name: "includeWhen false → ignored",
			graph: generator.NewGraph("g",
				generator.WithDef("v", map[string]any{"a": int64(1)}),
				generator.WithDef("guarded", map[string]any{"x": "y"}),
				generator.WithIncludeWhen("${v.a == 999}"),
			),
			populate: func(rt *Runtime) { setFirst(rt, "v") },
			want:     true,
		},
		{
			name: "ignored dep propagates contagiously to downstream",
			graph: generator.NewGraph("g",
				generator.WithDef("flag", map[string]any{"enabled": false}),
				generator.WithDef("middle", map[string]any{"x": "y"}),
				generator.WithIncludeWhen("${flag.enabled}"),
				generator.WithDef("leaf", map[string]any{"y": "${middle.x}"}),
			),
			populate: func(rt *Runtime) { setFirst(rt, "flag") },
			// leaf is downstream of middle which is ignored; leaf should
			// also be ignored.
			assertID: "leaf",
			want:     true,
		},
		{
			name: "includeWhen returning a non-bool is a hard error",
			graph: generator.NewGraph("g",
				generator.WithDef("seed", map[string]any{"k": "v"}),
				// dyn-typed field passes the bool typecheck but resolves
				// to a string at runtime, hitting the non-bool branch.
				generator.WithDef("cfg", map[string]any{"flag": "${'literal'}"}),
				generator.WithDef("guarded", map[string]any{"x": "y"}),
				generator.WithIncludeWhen("${cfg.flag}"),
			),
			populate: func(rt *Runtime) { setFirst(rt, "cfg") },
			assertID: "guarded",
			wantErr:  "want bool",
		},
		{
			name: "includeWhen referencing a missing sub-field surfaces data-pending",
			graph: generator.NewGraph("g",
				generator.WithDef("seed", map[string]any{"k": "v"}),
				generator.WithDef("cfg", map[string]any{"flag": "${'literal'}"}),
				generator.WithDef("guarded", map[string]any{"x": "y"}),
				generator.WithIncludeWhen("${cfg.flag.bogus}"),
			),
			populate: func(rt *Runtime) { setFirst(rt, "cfg") },
			assertID: "guarded",
			wantErr:  ErrDataPending.Error(),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			prog := compileGraph(t, tc.graph)
			rt := New(prog, tc.graph)
			if tc.populate != nil {
				tc.populate(rt)
			}
			id := tc.assertID
			if id == "" {
				id = rt.Nodes()[len(rt.Nodes())-1].ID()
			}
			got, err := rt.Node(id).IsIgnored()
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestNode_CheckReadiness exercises the observed-state-driven readiness
// gate. Empty readyWhen short-circuits to ready; an unsatisfied
// readyWhen returns ErrWaitingForReadiness; eval-time pending data also
// surfaces as ErrWaitingForReadiness.
func TestNode_CheckReadiness(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		graph       *expv1alpha1.Graph
		populate    func(rt *Runtime)
		assertID    string
		wantNil     bool // expect CheckReadiness to return nil
		wantWaiting bool // expect ErrWaitingForReadiness
		wantErr     string
	}{
		{
			name: "no readyWhen → ready regardless of observed",
			graph: generator.NewGraph("g",
				generator.WithDef("v", map[string]any{"x": "y"}),
			),
			wantNil: true,
		},
		{
			name: "readyWhen with no observed → waiting",
			graph: generator.NewGraph("g",
				generator.WithDef("svc", map[string]any{"x": "y"}),
				generator.WithReadyWhen("${svc.x == 'y'}"),
			),
			// Deliberately do NOT call SetObserved.
			wantWaiting: true,
		},
		{
			name: "readyWhen all true with observed set → ready",
			graph: generator.NewGraph("g",
				generator.WithDef("svc", map[string]any{"phase": "Active"}),
				generator.WithReadyWhen("${svc.phase == 'Active'}"),
			),
			populate: func(rt *Runtime) {
				objs, _ := rt.Node("svc").Resolve()
				rt.Set("svc", objs[0].Object)
				rt.Node("svc").SetObserved(objs, objs)
			},
			wantNil: true,
		},
		{
			name: "readyWhen false with observed set → waiting",
			graph: generator.NewGraph("g",
				generator.WithDef("svc", map[string]any{"phase": "Pending"}),
				generator.WithReadyWhen("${svc.phase == 'Active'}"),
			),
			populate: func(rt *Runtime) {
				objs, _ := rt.Node("svc").Resolve()
				rt.Set("svc", objs[0].Object)
				rt.Node("svc").SetObserved(objs, objs)
			},
			wantWaiting: true,
		},
		{
			name: "readyWhen referencing a missing sub-field on a dyn def → waiting (data-pending)",
			graph: generator.NewGraph("g",
				generator.WithDef("seed", map[string]any{"k": "v"}),
				// The CEL fragment keeps `dyn` at compile time so the
				// readyWhen typecheck doesn't reject. At runtime the
				// resolved value is a string, so `.bogus` triggers the
				// data-pending classification.
				generator.WithDef("svc", map[string]any{"x": "${'literal'}"}),
				generator.WithReadyWhen("${svc.x.bogus != ''}"),
			),
			populate: func(rt *Runtime) {
				objs, _ := rt.Node("svc").Resolve()
				rt.Set("svc", objs[0].Object)
				rt.Node("svc").SetObserved(objs, objs)
			},
			wantWaiting: true,
		},
		{
			name: "ignored node short-circuits to ready",
			graph: generator.NewGraph("g",
				generator.WithDef("flag", map[string]any{"enabled": false}),
				// guarded has a dyn field so the readyWhen sub-field
				// access compiles. If the ignore short-circuit doesn't
				// fire, eval would error at runtime — the test passes
				// only when ignored=true skips the eval entirely.
				generator.WithDef("guarded", map[string]any{"x": "${'literal'}"}),
				generator.WithIncludeWhen("${flag.enabled}"),
				generator.WithReadyWhen("${guarded.x.bogus != ''}"),
			),
			populate: func(rt *Runtime) { setFirst(rt, "flag") },
			wantNil:  true,
		},
		{
			name: "readyWhen returning a non-bool is a hard error",
			graph: generator.NewGraph("g",
				generator.WithDef("seed", map[string]any{"k": "v"}),
				// dyn field resolves to a string, so the readyWhen result
				// isn't a bool and hits the non-bool branch (hard error).
				generator.WithDef("svc", map[string]any{"phase": "${'Active'}"}),
				generator.WithReadyWhen("${svc.phase}"),
			),
			populate: func(rt *Runtime) {
				objs, _ := rt.Node("svc").Resolve()
				rt.Set("svc", objs[0].Object)
				rt.Node("svc").SetObserved(objs, objs)
			},
			assertID: "svc",
			wantErr:  "want bool",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			prog := compileGraph(t, tc.graph)
			rt := New(prog, tc.graph)
			if tc.populate != nil {
				tc.populate(rt)
			}
			id := tc.assertID
			if id == "" {
				id = rt.Nodes()[len(rt.Nodes())-1].ID()
			}
			err := rt.Node(id).CheckReadiness()
			switch {
			case tc.wantNil:
				assert.NoError(t, err)
			case tc.wantWaiting:
				require.Error(t, err)
				assert.ErrorIs(t, err, ErrWaitingForReadiness)
			case tc.wantErr != "":
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
			default:
				t.Fatal("test case has no expected outcome")
			}
		})
	}
}

func TestNode_Resolve(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		graph    *expv1alpha1.Graph
		populate func(rt *Runtime) // called before the assertion node is resolved
		assertID string            // node ID to Resolve and assert on
		// want is invoked once per resolved instance, in order.
		want    []func(t *testing.T, out map[string]any)
		wantErr string
	}{
		{
			name: "def with no variables passes through unchanged",
			graph: generator.NewGraph("g",
				generator.WithDef("naming", map[string]any{"prefix": "team-", "app": "billing"}),
			),
			assertID: "naming",
			want: []func(t *testing.T, out map[string]any){
				func(t *testing.T, out map[string]any) {
					assert.Equal(t, "team-", out["prefix"])
					assert.Equal(t, "billing", out["app"])
				},
			},
		},
		{
			name: "template substitution at metadata.name",
			graph: generator.NewGraph("g",
				generator.WithDef("naming", map[string]any{"prefix": "team-", "app": "billing"}),
				generator.WithTemplate("cm", map[string]any{
					"apiVersion": "v1", "kind": "ConfigMap",
					"metadata": map[string]any{"name": "${naming.prefix + naming.app}"},
					"data":     map[string]any{"k": "v"},
				}),
			),
			populate: func(rt *Runtime) { setFirst(rt, "naming") },
			assertID: "cm",
			want: []func(t *testing.T, out map[string]any){
				func(t *testing.T, out map[string]any) {
					md, _ := out["metadata"].(map[string]any)
					assert.Equal(t, "team-billing", md["name"])
				},
			},
		},
		{
			name: "template substitution inside an array element",
			graph: generator.NewGraph("g",
				generator.WithDef("v", map[string]any{"image": "nginx:1.27"}),
				generator.WithTemplate("p", map[string]any{
					"apiVersion": "v1", "kind": "Pod",
					"metadata": map[string]any{"name": "p"},
					"spec": map[string]any{"containers": []any{
						map[string]any{"name": "c", "image": "${v.image}"},
					}},
				}),
			),
			populate: func(rt *Runtime) { setFirst(rt, "v") },
			assertID: "p",
			want: []func(t *testing.T, out map[string]any){
				func(t *testing.T, out map[string]any) {
					spec, _ := out["spec"].(map[string]any)
					containers, _ := spec["containers"].([]any)
					require.Len(t, containers, 1)
					c, _ := containers[0].(map[string]any)
					assert.Equal(t, "nginx:1.27", c["image"])
				},
			},
		},
		{
			name: "eval failure surfaces a wrapped error with node id and path",
			graph: generator.NewGraph("g",
				generator.WithDef("seed", map[string]any{"k": "v"}),
				// The CEL fragment makes this def field dyn at compile
				// time, so downstream sub-field access compiles but errors
				// at runtime when the actual value doesn't have the field.
				generator.WithDef("base", map[string]any{"info": "${'literal'}"}),
				generator.WithTemplate("cm", map[string]any{
					"apiVersion": "v1", "kind": "ConfigMap",
					"metadata": map[string]any{"name": "${base.info.missing}"},
					"data":     map[string]any{"k": "v"},
				}),
			),
			populate: func(rt *Runtime) { setFirst(rt, "base") },
			assertID: "cm",
			wantErr:  "no such",
		},
		{
			name: "forEach over a typed list produces one rendered object per element",
			graph: generator.NewGraph("g",
				generator.WithDef("src", map[string]any{"names": []any{"alpha", "beta", "gamma"}}),
				generator.WithTemplate("p", map[string]any{
					"apiVersion": "v1", "kind": "Pod",
					"metadata": map[string]any{"name": "${'p-' + n}"},
					"spec":     map[string]any{"containers": []any{map[string]any{"name": "c", "image": "nginx"}}},
				}, generator.ForEachDim("n", "${src.names}")),
			),
			populate: func(rt *Runtime) { setFirst(rt, "src") },
			assertID: "p",
			want: []func(t *testing.T, out map[string]any){
				func(t *testing.T, out map[string]any) {
					md, _ := out["metadata"].(map[string]any)
					assert.Equal(t, "p-alpha", md["name"])
				},
				func(t *testing.T, out map[string]any) {
					md, _ := out["metadata"].(map[string]any)
					assert.Equal(t, "p-beta", md["name"])
				},
				func(t *testing.T, out map[string]any) {
					md, _ := out["metadata"].(map[string]any)
					assert.Equal(t, "p-gamma", md["name"])
				},
			},
		},
		{
			name: "two forEach axes produce the cartesian product",
			graph: generator.NewGraph("g",
				generator.WithDef("src", map[string]any{
					"regions": []any{"us", "eu"},
					"tiers":   []any{"hot", "cold"},
				}),
				generator.WithTemplate("p", map[string]any{
					"apiVersion": "v1", "kind": "Pod",
					"metadata": map[string]any{"name": "${r + '-' + t}"},
					"spec":     map[string]any{"containers": []any{map[string]any{"name": "c", "image": "nginx"}}},
				},
					generator.ForEachDim("r", "${src.regions}"),
					generator.ForEachDim("t", "${src.tiers}"),
				),
			),
			populate: func(rt *Runtime) { setFirst(rt, "src") },
			assertID: "p",
			// Cartesian product: us×hot, us×cold, eu×hot, eu×cold (4 rows).
			want: []func(t *testing.T, out map[string]any){
				func(t *testing.T, out map[string]any) {
					md, _ := out["metadata"].(map[string]any)
					assert.Equal(t, "us-hot", md["name"])
				},
				func(t *testing.T, out map[string]any) {
					md, _ := out["metadata"].(map[string]any)
					assert.Equal(t, "us-cold", md["name"])
				},
				func(t *testing.T, out map[string]any) {
					md, _ := out["metadata"].(map[string]any)
					assert.Equal(t, "eu-hot", md["name"])
				},
				func(t *testing.T, out map[string]any) {
					md, _ := out["metadata"].(map[string]any)
					assert.Equal(t, "eu-cold", md["name"])
				},
			},
		},
		{
			name: "forEach over an empty list produces zero rendered objects",
			graph: generator.NewGraph("g",
				generator.WithDef("src", map[string]any{"names": []any{}}),
				generator.WithTemplate("p", map[string]any{
					"apiVersion": "v1", "kind": "Pod",
					"metadata": map[string]any{"name": "${'p-' + n}"},
					"spec":     map[string]any{"containers": []any{map[string]any{"name": "c", "image": "nginx"}}},
				}, generator.ForEachDim("n", "${src.names}")),
			),
			populate: func(rt *Runtime) { setFirst(rt, "src") },
			assertID: "p",
			want:     []func(t *testing.T, out map[string]any){}, // expect 0 outputs
		},
		{
			name: "forEach over a non-list expression fails at runtime",
			graph: generator.NewGraph("g",
				// Literal seed satisfies the input-node rule.
				generator.WithDef("seed", map[string]any{"k": "v"}),
				// CEL fragment keeps the field dyn at compile time so
				// the static forEach-must-return-list check can't reject
				// upfront. The actual runtime value is a string, which
				// the runtime list assertion catches.
				generator.WithDef("src", map[string]any{"value": "${'not a list'}"}),
				generator.WithTemplate("p", map[string]any{
					"apiVersion": "v1", "kind": "Pod",
					"metadata": map[string]any{"name": "${'p-' + n}"},
					"spec":     map[string]any{"containers": []any{map[string]any{"name": "c", "image": "nginx"}}},
				}, generator.ForEachDim("n", "${src.value}")),
			),
			populate: func(rt *Runtime) { setFirst(rt, "src") },
			assertID: "p",
			wantErr:  "expected list",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			prog := compileGraph(t, tc.graph)
			rt := New(prog, tc.graph)
			if tc.populate != nil {
				tc.populate(rt)
			}
			out, err := rt.Node(tc.assertID).Resolve()
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.Len(t, out, len(tc.want))
			for i, assertFn := range tc.want {
				assertFn(t, out[i].Object)
			}
		})
	}
}
