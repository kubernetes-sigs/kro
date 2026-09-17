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
	"maps"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextinstall "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/install"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	memory "k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/restmapper"

	expv1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/compiler"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/testutil/generator"
	testk8s "github.com/kubernetes-sigs/kro/pkg/testutil/k8s"
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
			name: "published computed def fields keep their dynamic values",
			graph: generator.NewGraph("g",
				generator.WithDef("input", map[string]any{"name": "alpha"}),
				generator.WithDef("values", map[string]any{
					"enabled": "${input.name == 'alpha'}",
					"mapping": "${{'name': input.name}}",
					"names":   "${[input.name, 'beta']}",
				}),
				generator.WithDef("consumer", map[string]any{
					"enabled": "${values.enabled}",
					"mapping": "${values.mapping}",
					"names":   "${values.names}",
				}),
			),
			populate: func(rt *Runtime) {
				setFirst(rt, "input")
				setFirst(rt, "values")
			},
			assertID: "consumer",
			want: []func(t *testing.T, out map[string]any){
				func(t *testing.T, out map[string]any) {
					assert.Equal(t, true, out["enabled"])
					assert.Equal(t, map[string]any{"name": "alpha"}, out["mapping"])
					assert.Equal(t, []any{"alpha", "beta"}, out["names"])
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

// mustCompilerWithRealCRDSchema is mustCompiler with the resolver serving the
// real apiextensions CustomResourceDefinition schema, so CRD templates are
// type-checked and rendered against the shape a live cluster reports.
func mustCompilerWithRealCRDSchema(t *testing.T) *compiler.Compiler {
	t.Helper()
	r, disco, err := testk8s.NewFakeResolverWithRealCRDSchema()
	require.NoError(t, err)
	rm := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(disco))
	return compiler.NewCompilerWithDependencies(r, rm)
}

// kindCRDTemplate returns a CustomResourceDefinition template whose names and
// version come from a def node "kindSpec" (group, kind, plural, version) and
// whose versions[0].schema.openAPIV3Schema is set to openAPIV3Schema — a
// static map, a CEL expression string, or a map containing expressions.
func kindCRDTemplate(openAPIV3Schema any) map[string]any {
	return map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition",
		"metadata": map[string]any{"name": "${kindSpec.plural + '.' + kindSpec.group}"},
		"spec": map[string]any{
			"group": "${kindSpec.group}",
			"names": map[string]any{"kind": "${kindSpec.kind}", "plural": "${kindSpec.plural}"},
			"scope": "Namespaced",
			"versions": []any{map[string]any{
				"name": "${kindSpec.version}", "served": true, "storage": true,
				"subresources": map[string]any{"status": map[string]any{}},
				"schema":       map[string]any{"openAPIV3Schema": openAPIV3Schema},
			}},
		},
	}
}

// renderCRD compiles g against the real CRD schema, publishes every def node,
// renders node "crd" and returns it decoded into a typed CRD. Along the way it
// checks what every apply path relies on: the rendered object is JSON-safe (it
// survives the unstructured deep copy) and its schema passes the apiextensions
// structural-schema validation the API server applies on admission.
func renderCRD(t *testing.T, g *expv1alpha1.Graph) (*extv1.CustomResourceDefinition, map[string]any) {
	t.Helper()
	prog, err := mustCompilerWithRealCRDSchema(t).Compile(g)
	require.NoError(t, err)
	rt := New(prog, g)
	for _, id := range prog.TopologicalOrder {
		if prog.Nodes[id].Kind == compiler.NodeKindDef {
			setFirst(rt, id)
		}
	}

	objs, err := rt.Node("crd").Resolve()
	require.NoError(t, err)
	require.Len(t, objs, 1)
	assert.Equal(t, objs[0].Object, objs[0].DeepCopy().Object, "rendered object must be JSON-safe")

	var crd extv1.CustomResourceDefinition
	require.NoError(t, k8sruntime.DefaultUnstructuredConverter.FromUnstructured(objs[0].Object, &crd))
	require.Len(t, crd.Spec.Versions, 1)
	require.NotNil(t, crd.Spec.Versions[0].Schema)
	require.NotNil(t, crd.Spec.Versions[0].Schema.OpenAPIV3Schema)

	// The API server converts to the internal type and validates the structural
	// schema on admission; v1 -> internal hoists a single version's schema to
	// spec.validation.
	convScheme := k8sruntime.NewScheme()
	apiextinstall.Install(convScheme)
	var internal apiextensions.CustomResourceDefinition
	require.NoError(t, convScheme.Convert(&crd, &internal, nil))
	require.NotNil(t, internal.Spec.Validation)
	structural, err := structuralschema.NewStructural(internal.Spec.Validation.OpenAPIV3Schema)
	require.NoError(t, err)
	assert.Empty(t, structuralschema.ValidateStructural(nil, structural))

	return &crd, objs[0].Object
}

// kindSpecNames is the def node every CRD render test reads its names from.
var kindSpecNames = map[string]any{
	"group": "example.com", "kind": "App", "plural": "apps", "version": "v1alpha1",
}

// assertKindCRD checks the CRD fields that come from the kindSpecNames def.
func assertKindCRD(t *testing.T, crd *extv1.CustomResourceDefinition) {
	t.Helper()
	assert.Equal(t, "apps.example.com", crd.Name)
	assert.Equal(t, "example.com", crd.Spec.Group)
	assert.Equal(t, "App", crd.Spec.Names.Kind)
	assert.Equal(t, "apps", crd.Spec.Names.Plural)
	assert.Equal(t, "v1alpha1", crd.Spec.Versions[0].Name)
	root := crd.Spec.Versions[0].Schema.OpenAPIV3Schema
	assert.Equal(t, "object", root.Type)
	assert.ElementsMatch(t, []string{"apiVersion", "kind", "metadata", "spec", "status"}, slices.Collect(maps.Keys(root.Properties)))
}

// TestNode_Resolve_CRDTemplateSchemaFromCEL renders a CustomResourceDefinition
// whose openAPIV3Schema is produced by CEL, for both authoring styles: the
// whole openAPIV3Schema as one expression, and a static root with the spec and
// status sub-schemas as expressions. Both must render the same CRD, decode into
// a typed CRD and pass structural validation.
func TestNode_Resolve_CRDTemplateSchemaFromCEL(t *testing.T) {
	t.Parallel()

	const specSchema = `{"type": "object", "required": ["name"], "properties": {"name": {"type": "string"}, "replicas": {"type": "integer", "default": 1, "minimum": 0}}}`
	const statusSchema = `{"type": "object", "properties": {"readyReplicas": {"type": "integer"}}}`
	wantSpecSchema := extv1.JSONSchemaProps{
		Type:     "object",
		Required: []string{"name"},
		Properties: map[string]extv1.JSONSchemaProps{
			"name":     {Type: "string"},
			"replicas": {Type: "integer", Default: &extv1.JSON{Raw: []byte("1")}, Minimum: new(float64(0))},
		},
	}
	wantStatusSchema := extv1.JSONSchemaProps{
		Type:       "object",
		Properties: map[string]extv1.JSONSchemaProps{"readyReplicas": {Type: "integer"}},
	}

	templates := map[string]map[string]any{
		"whole openAPIV3Schema is one expression": kindCRDTemplate(
			`${{"type": "object", "properties": {"apiVersion": {"type": "string"}, "kind": {"type": "string"}, "metadata": {"type": "object"}, "spec": ` + specSchema + `, "status": ` + statusSchema + `}}}`,
		),
		"sub-schemas nested inside a static root": kindCRDTemplate(map[string]any{
			"type": "object",
			"properties": map[string]any{
				"apiVersion": map[string]any{"type": "string"},
				"kind":       map[string]any{"type": "string"},
				"metadata":   map[string]any{"type": "object"},
				"spec":       "${" + specSchema + "}",
				"status":     "${" + statusSchema + "}",
			},
		}),
	}

	rendered := map[string]map[string]any{}
	for name, tmpl := range templates {
		t.Run(name, func(t *testing.T) {
			g := generator.NewGraph("g",
				generator.WithDef("kindSpec", kindSpecNames),
				generator.WithTemplate("crd", tmpl),
			)
			crd, obj := renderCRD(t, g)
			rendered[name] = obj
			assertKindCRD(t, crd)
			root := crd.Spec.Versions[0].Schema.OpenAPIV3Schema
			assert.Equal(t, wantSpecSchema, root.Properties["spec"])
			assert.Equal(t, wantStatusSchema, root.Properties["status"])
		})
	}

	require.Len(t, rendered, 2)
	assert.Equal(t,
		rendered["whole openAPIV3Schema is one expression"],
		rendered["sub-schemas nested inside a static root"],
		"both authoring styles must render the same CRD")
}

// TestNode_Resolve_SimpleSchemaToOpenAPI renders a CustomResourceDefinition
// whose openAPIV3Schema comes from simpleschema.toOpenAPI() applied to a
// SimpleSchema block (spec, types, status) held in a def node — the graph-native
// way to define a Kind. The CEL result must land in the rendered object as
// JSON-safe values, decode into a typed CRD and pass structural validation.
func TestNode_Resolve_SimpleSchemaToOpenAPI(t *testing.T) {
	t.Parallel()

	kindSpec := map[string]any{
		"types": map[string]any{
			"Owner": map[string]any{"team": "string | required=true"},
		},
		"spec": map[string]any{
			"name":     "string | required=true",
			"replicas": "integer | default=1 minimum=0",
			"owner":    "Owner",
		},
		"status": map[string]any{
			"readyReplicas": "integer",
			// Deferred: the def evaluates to the literal string
			// "${service.spec.clusterIP}", which the function then sees as an
			// expression-valued status field.
			"url": "${'${service.spec.clusterIP}'}",
		},
	}
	for k, v := range kindSpecNames {
		kindSpec[k] = v
	}

	g := generator.NewGraph("g",
		generator.WithDef("kindSpec", kindSpec),
		generator.WithTemplate("crd", kindCRDTemplate("${simpleschema.toOpenAPI(kindSpec)}")),
	)
	crd, _ := renderCRD(t, g)
	assertKindCRD(t, crd)

	root := crd.Spec.Versions[0].Schema.OpenAPIV3Schema
	assert.Equal(t, extv1.JSONSchemaProps{
		Type:     "object",
		Required: []string{"name"},
		Properties: map[string]extv1.JSONSchemaProps{
			"name":     {Type: "string"},
			"replicas": {Type: "integer", Default: &extv1.JSON{Raw: []byte("1")}, Minimum: new(float64(0))},
			"owner": {
				Type:       "object",
				Required:   []string{"team"},
				Properties: map[string]extv1.JSONSchemaProps{"team": {Type: "string"}},
			},
		},
	}, root.Properties["spec"])
	assert.Equal(t, extv1.JSONSchemaProps{
		Type: "object",
		Properties: map[string]extv1.JSONSchemaProps{
			"readyReplicas": {Type: "integer"},
			// An expression-valued status field cannot be typed by the function.
			"url": {XPreserveUnknownFields: new(true)},
		},
	}, root.Properties["status"])
}

func TestNode_TolerateDataPending(t *testing.T) {
	t.Parallel()

	t.Run("omits data-pending map field when TolerateDataPending is true", func(t *testing.T) {
		t.Parallel()
		g := generator.NewGraph("g",
			generator.WithDef("seed", map[string]any{"k": "v"}),
			generator.WithDef("upstream", map[string]any{"data": "${'val'}"}),
			generator.WithTemplate("cm", map[string]any{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]any{"name": "cm-test"},
				"data": map[string]any{
					"k1": "${upstream.data}",
					"k2": "${upstream.data.missing}",
				},
			}),
		)
		p, err := mustCompiler(t).CompileWithOptions(g, compiler.WithDataPendingTolerant("cm"))
		require.NoError(t, err)

		rt := New(p, g)
		setFirst(rt, "seed")
		setFirst(rt, "upstream")

		out, err := rt.Node("cm").Resolve()
		require.NoError(t, err)
		require.Len(t, out, 1)

		data, ok := out[0].Object["data"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "val", data["k1"])
		_, hasK2 := data["k2"]
		assert.False(t, hasK2, "data-pending map field should be omitted")
	})

	// Option A: a data-pending array element under tolerance omits the WHOLE
	// enclosing array field (index corruption prevents omitting a single
	// element), while every sibling field still renders and the node does NOT
	// data-pend. Before the fix this returned ErrDataPending and dropped the
	// whole node.
	t.Run("omits the whole enclosing array field when one element is data-pending", func(t *testing.T) {
		t.Parallel()
		g := generator.NewGraph("g",
			generator.WithDef("seed", map[string]any{"k": "v"}),
			generator.WithDef("upstream", map[string]any{"data": "${'val'}", "enabled": "${true}"}),
			generator.WithTemplate("vpc", map[string]any{
				"apiVersion": "ec2.services.k8s.aws/v1alpha1",
				"kind":       "VPC",
				"metadata":   map[string]any{"name": "vpc-test"},
				"spec": map[string]any{
					// A sibling scalar that resolves fine must survive.
					"enableDNSSupport": "${upstream.enabled}",
					"cidrBlocks": []any{
						// Element [0] resolves; element [1] is data-pending.
						"${upstream.data}",
						"${upstream.data.missing}",
					},
				},
			}),
		)
		p, err := mustCompiler(t).CompileWithOptions(g, compiler.WithDataPendingTolerant("vpc"))
		require.NoError(t, err)

		rt := New(p, g)
		setFirst(rt, "seed")
		setFirst(rt, "upstream")

		out, err := rt.Node("vpc").Resolve()
		require.NoError(t, err, "a data-pending array element must not data-pend the whole node")
		require.Len(t, out, 1)

		spec, ok := out[0].Object["spec"].(map[string]any)
		require.True(t, ok)
		// Sibling scalar renders.
		assert.Equal(t, true, spec["enableDNSSupport"], "sibling scalar field must still render")
		// The whole array field is absent (not present-but-empty, not partial).
		_, hasCidr := spec["cidrBlocks"]
		assert.False(t, hasCidr, "enclosing array field must be absent, not empty or partial")
	})

	// Option A applies to a nested array field too: status.conditions[1] omits
	// status.conditions, leaving the rest of status intact.
	t.Run("omits the nested enclosing array field", func(t *testing.T) {
		t.Parallel()
		g := generator.NewGraph("g",
			generator.WithDef("seed", map[string]any{"k": "v"}),
			generator.WithDef("upstream", map[string]any{"data": "${'val'}"}),
			generator.WithTemplate("vpc", map[string]any{
				"apiVersion": "ec2.services.k8s.aws/v1alpha1",
				"kind":       "VPC",
				"metadata":   map[string]any{"name": "vpc-test"},
				"status": map[string]any{
					// Sibling scalar under the same parent survives.
					"vpcID": "${upstream.data}",
					"conditions": []any{
						map[string]any{"type": "${upstream.data}"},
						map[string]any{"type": "${upstream.data.missing}"},
					},
				},
			}),
		)
		p, err := mustCompiler(t).CompileWithOptions(g, compiler.WithDataPendingTolerant("vpc"))
		require.NoError(t, err)

		rt := New(p, g)
		setFirst(rt, "seed")
		setFirst(rt, "upstream")

		out, err := rt.Node("vpc").Resolve()
		require.NoError(t, err)
		require.Len(t, out, 1)

		status, ok := out[0].Object["status"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "val", status["vpcID"], "sibling field under the same parent must survive")
		_, hasConditions := status["conditions"]
		assert.False(t, hasConditions, "nested enclosing array field status.conditions must be absent")
	})

	// A non-tolerant node still data-pends the whole node on any pending field,
	// array element included.
	t.Run("non-tolerant node data-pends the whole node on a pending array element", func(t *testing.T) {
		t.Parallel()
		g := generator.NewGraph("g",
			generator.WithDef("seed", map[string]any{"k": "v"}),
			generator.WithDef("upstream", map[string]any{"data": "${'val'}"}),
			generator.WithTemplate("vpc", map[string]any{
				"apiVersion": "ec2.services.k8s.aws/v1alpha1",
				"kind":       "VPC",
				"metadata":   map[string]any{"name": "vpc-test"},
				"spec": map[string]any{
					"cidrBlocks": []any{
						"${upstream.data.missing}",
						"${upstream.data}",
					},
				},
			}),
		)
		// No WithDataPendingTolerant: tolerance is off.
		p, err := mustCompiler(t).Compile(g)
		require.NoError(t, err)

		rt := New(p, g)
		setFirst(rt, "seed")
		setFirst(rt, "upstream")

		_, err = rt.Node("vpc").Resolve()
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrDataPending), "a non-tolerant node must data-pend the whole node")
	})
}
