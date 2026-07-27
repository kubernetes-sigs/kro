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

package executor

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	memory "k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/restmapper"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	expv1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/compiler"
	krotruntime "github.com/kubernetes-sigs/kro/pkg/graphengine/runtime"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/testutil/generator"
	testk8s "github.com/kubernetes-sigs/kro/pkg/graphengine/testutil/k8s"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/watchrouter"
)

var configMapGVK = schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, expv1alpha1.AddToScheme(s))
	return s
}

func newCompiler(t *testing.T) *compiler.Compiler {
	t.Helper()
	r, disco := testk8s.NewFakeResolver()
	rm := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(disco))
	return compiler.NewCompilerWithDependencies(r, rm)
}

func compileAndBuild(t *testing.T, g *expv1alpha1.Graph) *krotruntime.Runtime {
	t.Helper()
	p, err := newCompiler(t).Compile(g)
	require.NoError(t, err)
	return krotruntime.New(p, g)
}

func TestSimple_Apply(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		graph *expv1alpha1.Graph
		// program lets a row substitute a hand-built Program (escape hatch
		// for kinds the generator/compiler don't produce, like an unknown
		// NodeKind that exercises the dispatch default).
		program *compiler.Program
		wantErr string
		after   func(t *testing.T, c client.Client)
	}{
		{
			name: "template creates a ConfigMap with substituted name",
			graph: generator.NewGraph("g",
				generator.WithNamespace("default"),
				generator.WithDef("naming", map[string]any{"prefix": "team-", "app": "billing"}),
				generator.WithTemplate("cm", map[string]any{
					"apiVersion": "v1", "kind": "ConfigMap",
					"metadata": map[string]any{"name": "${naming.prefix + naming.app}"},
					"data":     map[string]any{"k": "v"},
				}),
			),
			after: func(t *testing.T, c client.Client) {
				cm := &unstructured.Unstructured{}
				cm.SetGroupVersionKind(configMapGVK)
				require.NoError(t, c.Get(context.Background(),
					types.NamespacedName{Namespace: "default", Name: "team-billing"}, cm))
			},
		},
		{
			name: "forEach over a typed list creates one resource per element",
			graph: generator.NewGraph("g",
				generator.WithNamespace("default"),
				generator.WithDef("src", map[string]any{"names": []any{"alpha", "beta"}}),
				generator.WithTemplate("cm", map[string]any{
					"apiVersion": "v1", "kind": "ConfigMap",
					"metadata": map[string]any{"name": "${'cm-' + n}"},
					"data":     map[string]any{"k": "v"},
				}, generator.ForEachDim("n", "${src.names}")),
			),
			after: func(t *testing.T, c client.Client) {
				for _, name := range []string{"cm-alpha", "cm-beta"} {
					cm := &unstructured.Unstructured{}
					cm.SetGroupVersionKind(configMapGVK)
					require.NoError(t, c.Get(context.Background(),
						types.NamespacedName{Namespace: "default", Name: name}, cm), "missing %q", name)
				}
			},
		},
		{
			name: "includeWhen false skips the node entirely (no resource created)",
			graph: generator.NewGraph("g",
				generator.WithNamespace("default"),
				generator.WithDef("flag", map[string]any{"enabled": false}),
				generator.WithTemplate("cm", map[string]any{
					"apiVersion": "v1", "kind": "ConfigMap",
					"metadata": map[string]any{"name": "guarded"},
					"data":     map[string]any{"k": "v"},
				}),
				generator.WithIncludeWhen("${flag.enabled}"),
			),
			after: func(t *testing.T, c client.Client) {
				cm := &unstructured.Unstructured{}
				cm.SetGroupVersionKind(configMapGVK)
				err := c.Get(context.Background(),
					types.NamespacedName{Namespace: "default", Name: "guarded"}, cm)
				require.Error(t, err)
				assert.True(t, apierrors.IsNotFound(err))
			},
		},
		{
			name: "includeWhen true applies normally",
			graph: generator.NewGraph("g",
				generator.WithNamespace("default"),
				generator.WithDef("flag", map[string]any{"enabled": true}),
				generator.WithTemplate("cm", map[string]any{
					"apiVersion": "v1", "kind": "ConfigMap",
					"metadata": map[string]any{"name": "guarded"},
					"data":     map[string]any{"k": "v"},
				}),
				generator.WithIncludeWhen("${flag.enabled}"),
			),
			after: func(t *testing.T, c client.Client) {
				cm := &unstructured.Unstructured{}
				cm.SetGroupVersionKind(configMapGVK)
				require.NoError(t, c.Get(context.Background(),
					types.NamespacedName{Namespace: "default", Name: "guarded"}, cm))
			},
		},
		{
			name: "readyWhen false surfaces as ErrNotReady",
			graph: generator.NewGraph("g",
				generator.WithNamespace("default"),
				generator.WithTemplate("cm", map[string]any{
					"apiVersion": "v1", "kind": "ConfigMap",
					"metadata": map[string]any{"name": "cm"},
					"data":     map[string]any{"k": "v"},
				}),
				// cm.data.k is "v" — assert it equals something else so the
				// readyWhen never converges. Real graphs would reference
				// status fields populated by some controller.
				generator.WithReadyWhen("${cm.data.k == 'something else'}"),
			),
			wantErr: ErrNotReady.Error(),
		},
		{
			name: "readyWhen true completes apply normally",
			graph: generator.NewGraph("g",
				generator.WithNamespace("default"),
				generator.WithTemplate("cm", map[string]any{
					"apiVersion": "v1", "kind": "ConfigMap",
					"metadata": map[string]any{"name": "cm"},
					"data":     map[string]any{"k": "v"},
				}),
				generator.WithReadyWhen("${cm.data.k == 'v'}"),
			),
			after: func(t *testing.T, c client.Client) {
				cm := &unstructured.Unstructured{}
				cm.SetGroupVersionKind(configMapGVK)
				require.NoError(t, c.Get(context.Background(),
					types.NamespacedName{Namespace: "default", Name: "cm"}, cm))
			},
		},
		{
			name: "namespaced template without namespace defaults to graph namespace",
			graph: generator.NewGraph("g",
				generator.WithNamespace("alpha"),
				generator.WithTemplate("cm", map[string]any{
					"apiVersion": "v1", "kind": "ConfigMap",
					"metadata": map[string]any{"name": "cm"},
					"data":     map[string]any{"k": "v"},
				}),
			),
			after: func(t *testing.T, c client.Client) {
				cm := &unstructured.Unstructured{}
				cm.SetGroupVersionKind(configMapGVK)
				require.NoError(t, c.Get(context.Background(),
					types.NamespacedName{Namespace: "alpha", Name: "cm"}, cm))
			},
		},
		{
			name: "ref node returns ErrUnsupported",
			graph: generator.NewGraph("g",
				generator.WithRef("existing", &expv1alpha1.ExternalRef{
					APIVersion: "v1", Kind: "Pod",
					Metadata: expv1alpha1.ExternalRefMetadata{Name: "p"},
				}),
			),
			wantErr: ErrUnsupported.Error(),
		},
		{
			name: "watch node returns ErrUnsupported",
			graph: generator.NewGraph("g",
				generator.WithDef("naming", map[string]any{"app": "x"}),
				generator.WithWatch("pods", &expv1alpha1.WatchSpec{
					APIVersion: "v1", Kind: "Pod",
					Selector: map[string]string{"app": "${naming.app}"},
				}),
			),
			wantErr: ErrUnsupported.Error(),
		},
		{
			name: "resolve failure during apply surfaces as a wrapped error",
			graph: generator.NewGraph("g",
				generator.WithNamespace("default"),
				generator.WithDef("seed", map[string]any{"k": "v"}),
				// Dyn def field so the sub-field access compiles; the
				// actual value is a string, so .bogus errors at runtime
				// during Resolve.
				generator.WithDef("base", map[string]any{"name": "${'ok'}"}),
				generator.WithTemplate("cm", map[string]any{
					"apiVersion": "v1", "kind": "ConfigMap",
					"metadata": map[string]any{"name": "${base.name.bogus}"},
					"data":     map[string]any{"k": "v"},
				}),
			),
			wantErr: "resolve",
		},
		{
			name: "unknown node kind hits the dispatch default",
			program: &compiler.Program{
				Nodes: map[string]*compiler.Node{"x": {
					ID:     "x",
					Kind:   compiler.NodeKind(99),
					Object: &unstructured.Unstructured{Object: map[string]any{}},
				}},
				TopologicalOrder: []string{"x"},
			},
			wantErr: "unknown kind",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cl := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
			ex := NewSimple(cl)
			var rt *krotruntime.Runtime
			if tc.program != nil {
				rt = krotruntime.New(tc.program, &expv1alpha1.Graph{})
			} else {
				rt = compileAndBuild(t, tc.graph)
			}
			_, err := ex.Apply(context.Background(), rt, watchrouter.NoopWatcher{})
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			if tc.after != nil {
				tc.after(t, cl)
			}
		})
	}
}

func TestSimple_Delete(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		seed    func(t *testing.T, c client.Client) []expv1alpha1.ManagedResource
		wantErr string
		after   func(t *testing.T, c client.Client)
	}{
		{
			name: "deletes a single tracked resource",
			seed: func(t *testing.T, c client.Client) []expv1alpha1.ManagedResource {
				return []expv1alpha1.ManagedResource{newSeededCM(t, c, "billing", "default")}
			},
			after: func(t *testing.T, c client.Client) {
				assertCMGone(t, c, "billing", "default")
			},
		},
		{
			name: "deletes multiple tracked resources in reverse-slice order",
			seed: func(t *testing.T, c client.Client) []expv1alpha1.ManagedResource {
				return []expv1alpha1.ManagedResource{
					newSeededCM(t, c, "cm-alpha", "default"),
					newSeededCM(t, c, "cm-beta", "default"),
				}
			},
			after: func(t *testing.T, c client.Client) {
				assertCMGone(t, c, "cm-alpha", "default")
				assertCMGone(t, c, "cm-beta", "default")
			},
		},
		{
			name: "untracked NotFound is tolerated",
			seed: func(t *testing.T, c client.Client) []expv1alpha1.ManagedResource {
				return []expv1alpha1.ManagedResource{{
					NodeID: "n", APIVersion: "v1", Kind: "ConfigMap",
					Namespace: "default", Name: "ghost",
				}}
			},
		},
		// UID-precondition behavior is tested at the integration
		// layer — controller-runtime's fake client does not honor
		// Preconditions on Delete, so a unit test here would only
		// exercise the fake. See test/integration/suites/core for
		// the real envtest-backed assertion.
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cl := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
			ex := NewSimple(cl)
			resources := tc.seed(t, cl)
			err := ex.Delete(context.Background(), resources)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			if tc.after != nil {
				tc.after(t, cl)
			}
		})
	}
}

// newSeededCM Creates a ConfigMap in the fake client and returns a
// ManagedResource pointing at it with the real UID populated.
func newSeededCM(t *testing.T, c client.Client, name, namespace string) expv1alpha1.ManagedResource {
	t.Helper()
	cm := &unstructured.Unstructured{}
	cm.SetGroupVersionKind(configMapGVK)
	cm.SetName(name)
	cm.SetNamespace(namespace)
	require.NoError(t, c.Create(context.Background(), cm))
	return expv1alpha1.ManagedResource{
		NodeID:     "n",
		APIVersion: "v1",
		Kind:       "ConfigMap",
		Namespace:  namespace,
		Name:       name,
		UID:        string(cm.GetUID()),
	}
}

func assertCMGone(t *testing.T, c client.Client, name, namespace string) {
	t.Helper()
	cm := &unstructured.Unstructured{}
	cm.SetGroupVersionKind(configMapGVK)
	err := c.Get(context.Background(),
		types.NamespacedName{Namespace: namespace, Name: name}, cm)
	assert.True(t, apierrors.IsNotFound(err), "%q/%q still present", namespace, name)
}

func TestSimple_PropagatesClientError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		op   func(ex *Simple, rt *krotruntime.Runtime) error
	}{
		{
			name: "apply propagates wrapped client error",
			op: func(ex *Simple, rt *krotruntime.Runtime) error {
				_, err := ex.Apply(context.Background(), rt, watchrouter.NoopWatcher{})
				return err
			},
		},
		{
			name: "delete propagates wrapped client error",
			op: func(ex *Simple, _ *krotruntime.Runtime) error {
				// Delete no longer needs the runtime; we hand it a
				// single tracked entry so the underlying Client.Delete
				// is actually called and surfaces the injected error.
				return ex.Delete(context.Background(), []expv1alpha1.ManagedResource{{
					NodeID: "n", APIVersion: "v1", Kind: "ConfigMap",
					Namespace: "default", Name: "x",
				}})
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := generator.NewGraph("g",
				generator.WithNamespace("default"),
				generator.WithTemplate("cm", map[string]any{
					"apiVersion": "v1", "kind": "ConfigMap",
					"metadata": map[string]any{"name": "x"},
					"data":     map[string]any{"k": "v"},
				}),
			)
			rt := compileAndBuild(t, g)
			ex := NewSimple(&errClient{err: errors.New("boom")})
			err := tc.op(ex, rt)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "boom")
		})
	}
}

// TestSimple_DefaultNamespace covers the small branching matrix of
// defaultNamespace: cluster-scoped, already-set namespace, and the
// namespaced-with-empty-graph-namespace short-circuit.
func TestSimple_DefaultNamespace(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		namespaced bool
		objNS      string
		graphNS    string
		wantObjNS  string
	}{
		{name: "cluster-scoped is left untouched", namespaced: false, wantObjNS: ""},
		{name: "namespace already set is preserved", namespaced: true, objNS: "explicit", graphNS: "fallback", wantObjNS: "explicit"},
		{name: "empty graph namespace yields empty object namespace", namespaced: true, wantObjNS: ""},
		{name: "namespaced fills from graph namespace", namespaced: true, graphNS: "alpha", wantObjNS: "alpha"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := &expv1alpha1.Graph{}
			g.Namespace = tc.graphNS
			rt := krotruntime.New(&compiler.Program{
				Nodes:            map[string]*compiler.Node{"x": {ID: "x"}},
				TopologicalOrder: []string{"x"},
			}, g)
			obj := &unstructured.Unstructured{}
			obj.SetNamespace(tc.objNS)
			(&Simple{}).defaultNamespace(rt, tc.namespaced, obj)
			assert.Equal(t, tc.wantObjNS, obj.GetNamespace())
		})
	}
}

// errClient fails every write with err. Read paths aren't needed.
type errClient struct {
	client.Client
	err error
}

func (e *errClient) Patch(context.Context, client.Object, client.Patch, ...client.PatchOption) error {
	return e.err
}
func (e *errClient) Delete(context.Context, client.Object, ...client.DeleteOption) error {
	return e.err
}

// TestSimple_Apply_DynamicGVK exercises the specialize-at-resolution path:
// a template whose apiVersion is a CEL expression has its GVK resolved from
// scope at apply time, REST-mapped through the live mapper, applied, and
// tracked with the concrete GVK. A GVK the cluster can't map yet (CRD not
// installed) is a soft requeue, not a hard failure.
func TestSimple_Apply_DynamicGVK(t *testing.T) {
	t.Parallel()

	_, disco := testk8s.NewFakeResolver()
	rm := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(disco))

	t.Run("dynamic apiVersion resolves, applies, and tracks the concrete GVK", func(t *testing.T) {
		t.Parallel()
		g := generator.NewGraph("g",
			generator.WithNamespace("default"),
			generator.WithDef("cfg", map[string]any{"group": "v1"}),
			generator.WithTemplate("cm", map[string]any{
				"apiVersion": "${cfg.group}",
				"kind":       "ConfigMap",
				"metadata":   map[string]any{"name": "dyn-cm"},
				"data":       map[string]any{"k": "v"},
			}),
		)
		rt := compileAndBuild(t, g)
		cl := fake.NewClientBuilder().WithScheme(newScheme(t)).WithRESTMapper(rm).Build()

		res, err := NewSimple(cl).Apply(context.Background(), rt, watchrouter.NoopWatcher{})
		require.NoError(t, err)

		require.Len(t, res.Applied, 1)
		assert.Equal(t, "v1", res.Applied[0].APIVersion)
		assert.Equal(t, "ConfigMap", res.Applied[0].Kind)
		assert.Equal(t, "dyn-cm", res.Applied[0].Name)
		// Namespaced scope was resolved at apply time and defaulted.
		assert.Equal(t, "default", res.Applied[0].Namespace)

		cm := &unstructured.Unstructured{}
		cm.SetGroupVersionKind(configMapGVK)
		require.NoError(t, cl.Get(context.Background(),
			types.NamespacedName{Namespace: "default", Name: "dyn-cm"}, cm))
	})

	t.Run("unmappable GVK is a soft requeue, nothing applied", func(t *testing.T) {
		t.Parallel()
		g := generator.NewGraph("g",
			generator.WithNamespace("default"),
			generator.WithDef("cfg", map[string]any{"group": "example.com/v1"}),
			generator.WithTemplate("widget", map[string]any{
				"apiVersion": "${cfg.group}",
				"kind":       "Widget",
				"metadata":   map[string]any{"name": "w"},
			}),
		)
		rt := compileAndBuild(t, g)
		cl := fake.NewClientBuilder().WithScheme(newScheme(t)).WithRESTMapper(rm).Build()

		res, err := NewSimple(cl).Apply(context.Background(), rt, watchrouter.NoopWatcher{})
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrNotReady)
		assert.Contains(t, res.Unresolved, "widget")
		assert.Empty(t, res.Applied)
	})
}

// TestSimple_Apply_Nesting exercises nested-graph execution end to end: a
// subgraph runs its child Program in a scope seeded from the parent (capture
// + shadowing), publishes child outputs under the subgraph node ID, and the
// child's managed resources are tracked with frame-qualified NodeIDs.
func TestSimple_Apply_Nesting(t *testing.T) {
	t.Parallel()

	getCM := func(t *testing.T, c client.Client, ns, name string) {
		t.Helper()
		cm := &unstructured.Unstructured{}
		cm.SetGroupVersionKind(configMapGVK)
		require.NoError(t, c.Get(context.Background(),
			types.NamespacedName{Namespace: ns, Name: name}, cm), "missing %q", name)
	}

	t.Run("subgraph applies a child resource named from a captured parent def", func(t *testing.T) {
		t.Parallel()
		child := generator.NewGraph("child",
			generator.WithTemplate("cm", map[string]any{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]any{"name": "${cfg.name}"}, // captures parent cfg
				"data":     map[string]any{"k": "v"},
			}),
		)
		g := generator.NewGraph("g",
			generator.WithNamespace("default"),
			generator.WithDef("cfg", map[string]any{"name": "nested-cm"}),
			generator.WithSubgraph("sub", child),
		)
		rt := compileAndBuild(t, g)
		cl := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()

		res, err := NewSimple(cl).Apply(context.Background(), rt, watchrouter.NoopWatcher{})
		require.NoError(t, err)
		getCM(t, cl, "default", "nested-cm")

		require.Len(t, res.Applied, 1)
		assert.Equal(t, "sub/cm", res.Applied[0].NodeID, "child identities are frame-qualified")
		assert.Equal(t, "nested-cm", res.Applied[0].Name)
	})

	t.Run("parent reads a child output through the subgraph node ID", func(t *testing.T) {
		t.Parallel()
		child := generator.NewGraph("child",
			generator.WithDef("out", map[string]any{"name": "from-child"}),
		)
		g := generator.NewGraph("g",
			generator.WithNamespace("default"),
			generator.WithSubgraph("sub", child),
			generator.WithTemplate("cm", map[string]any{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]any{"name": "${sub.out.name}"}, // reads child output
				"data":     map[string]any{"k": "v"},
			}),
		)
		rt := compileAndBuild(t, g)
		cl := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()

		_, err := NewSimple(cl).Apply(context.Background(), rt, watchrouter.NoopWatcher{})
		require.NoError(t, err)
		getCM(t, cl, "default", "from-child")
	})

	t.Run("a child def shadows the parent def of the same name", func(t *testing.T) {
		t.Parallel()
		child := generator.NewGraph("child",
			generator.WithDef("cfg", map[string]any{"name": "inner-name"}),
			generator.WithTemplate("cm", map[string]any{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]any{"name": "${cfg.name}"},
				"data":     map[string]any{"k": "v"},
			}),
		)
		g := generator.NewGraph("g",
			generator.WithNamespace("default"),
			generator.WithDef("cfg", map[string]any{"name": "outer-name"}),
			generator.WithSubgraph("sub", child),
		)
		rt := compileAndBuild(t, g)
		cl := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()

		_, err := NewSimple(cl).Apply(context.Background(), rt, watchrouter.NoopWatcher{})
		require.NoError(t, err)
		getCM(t, cl, "default", "inner-name") // child shadows parent
	})

	t.Run("deeply nested subgraph applies and qualifies identities by path", func(t *testing.T) {
		t.Parallel()
		grandchild := generator.NewGraph("gc",
			generator.WithTemplate("cm", map[string]any{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]any{"name": "${root.name}"}, // captures grandparent
				"data":     map[string]any{"k": "v"},
			}),
		)
		child := generator.NewGraph("child", generator.WithSubgraph("mid", grandchild))
		g := generator.NewGraph("g",
			generator.WithNamespace("default"),
			generator.WithDef("root", map[string]any{"name": "deep-cm"}),
			generator.WithSubgraph("outer", child),
		)
		rt := compileAndBuild(t, g)
		cl := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()

		res, err := NewSimple(cl).Apply(context.Background(), rt, watchrouter.NoopWatcher{})
		require.NoError(t, err)
		getCM(t, cl, "default", "deep-cm")
		require.Len(t, res.Applied, 1)
		assert.Equal(t, "outer/mid/cm", res.Applied[0].NodeID, "identity qualified by the full frame path")
	})
}
