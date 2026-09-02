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
	"github.com/kubernetes-sigs/kro/pkg/features"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/compiler"
	krotruntime "github.com/kubernetes-sigs/kro/pkg/graphengine/runtime"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/testutil/generator"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/watchrouter"
	testk8s "github.com/kubernetes-sigs/kro/pkg/testutil/k8s"
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

func compileAndBuild(t *testing.T, g *expv1alpha1.Graph, opts ...compiler.CompileOption) *krotruntime.Runtime {
	t.Helper()
	p, err := newCompiler(t).CompileWithOptions(g, opts...)
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
			name: "ref to a missing external object is soft ErrNotReady",
			graph: generator.NewGraph("g",
				generator.WithNamespace("default"),
				generator.WithRef("existing", &expv1alpha1.ExternalRef{
					APIVersion: "v1", Kind: "ConfigMap",
					Metadata: expv1alpha1.ExternalRefMetadata{Name: "not-there"},
				}),
			),
			wantErr: ErrNotReady.Error(),
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

// TestSimple_ApplyRef verifies the read-only ref path: kro GETs the live
// external object, publishes its fields into scope for dependents, and never
// records it as a managed resource (so it is never pruned or deleted).
func TestSimple_ApplyRef(t *testing.T) {
	t.Parallel()
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()

	// An object that exists in the cluster but is NOT managed by this Graph.
	external := &unstructured.Unstructured{}
	external.SetGroupVersionKind(configMapGVK)
	external.SetName("external-web-config")
	external.SetNamespace("default")
	require.NoError(t, unstructured.SetNestedStringMap(external.Object,
		map[string]string{"MESSAGE": "hi from the cluster"}, "data"))
	require.NoError(t, cl.Create(context.Background(), external))

	g := generator.NewGraph("g",
		generator.WithNamespace("default"),
		generator.WithRef("extcfg", &expv1alpha1.ExternalRef{
			APIVersion: "v1", Kind: "ConfigMap",
			Metadata: expv1alpha1.ExternalRefMetadata{Name: "external-web-config"},
		}),
		generator.WithTemplate("copy", map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "copied"},
			"data":     map[string]any{"MESSAGE": "${extcfg.data.MESSAGE}"},
		}),
	)

	ex := NewSimple(cl)
	rt := compileAndBuild(t, g)
	res, err := ex.Apply(context.Background(), rt, watchrouter.NoopWatcher{})
	require.NoError(t, err)

	// The dependent template consumed the external value.
	copied := &unstructured.Unstructured{}
	copied.SetGroupVersionKind(configMapGVK)
	require.NoError(t, cl.Get(context.Background(),
		types.NamespacedName{Namespace: "default", Name: "copied"}, copied))
	msg, _, _ := unstructured.NestedString(copied.Object, "data", "MESSAGE")
	assert.Equal(t, "hi from the cluster", msg)

	// The ref object must never be tracked as managed — it is read-only.
	for _, mr := range res.Applied {
		assert.NotEqual(t, "extcfg", mr.NodeID, "ref node must not be recorded as a managed resource")
		assert.NotEqual(t, "external-web-config", mr.Name, "external ref object must never be managed")
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
					Namespace: "default", Name: "ghost", UID: "ghost-uid",
				}}
			},
		},
		{
			name: "empty UID is skipped and does not delete pre-existing object",
			seed: func(t *testing.T, c client.Client) []expv1alpha1.ManagedResource {
				cm := &unstructured.Unstructured{}
				cm.SetGroupVersionKind(configMapGVK)
				cm.SetName("victim")
				cm.SetNamespace("default")
				require.NoError(t, c.Create(context.Background(), cm))
				return []expv1alpha1.ManagedResource{{
					NodeID: "forged", APIVersion: "v1", Kind: "ConfigMap",
					Namespace: "default", Name: "victim", UID: "",
				}}
			},
			after: func(t *testing.T, c client.Client) {
				cm := &unstructured.Unstructured{}
				cm.SetGroupVersionKind(configMapGVK)
				err := c.Get(context.Background(),
					types.NamespacedName{Namespace: "default", Name: "victim"}, cm)
				require.NoError(t, err, "victim must not be deleted when UID is empty")
			},
		},
		{
			name: "matching UID deletes pre-existing object",
			seed: func(t *testing.T, c client.Client) []expv1alpha1.ManagedResource {
				return []expv1alpha1.ManagedResource{newSeededCM(t, c, "legit", "default")}
			},
			after: func(t *testing.T, c client.Client) {
				assertCMGone(t, c, "legit", "default")
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
	cm.SetUID(types.UID("uid-" + name))
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
					Namespace: "default", Name: "x", UID: "uid-x",
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
func (e *errClient) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	// applyTemplate GETs the live object before SSA to check for a
	// terminating resource; surface the injected error here so the wrapped
	// client error still propagates out of Apply.
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

	t.Run("nested subgraph propagates patch contributions to parent ApplyResult", func(t *testing.T) {
		t.Parallel()
		child := generator.NewGraph("child",
			generator.WithPatch("p", "v1", "ConfigMap", "target-cm", map[string]any{
				"data": map[string]any{"nested": "val"},
			}),
		)
		g := generator.NewGraph("g",
			generator.WithNamespace("default"),
			generator.WithSubgraph("sub", child),
		)
		rt := compileAndBuild(t, g)
		targetCM := &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name":      "target-cm",
					"namespace": "default",
				},
				"data": map[string]any{"orig": "val"},
			},
		}
		cl := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(targetCM).Build()

		res, err := NewSimple(cl).Apply(context.Background(), rt, watchrouter.NoopWatcher{})
		require.NoError(t, err)
		require.Len(t, res.Contributions, 1)
		assert.Equal(t, "ConfigMap", res.Contributions[0].Kind)
		assert.Equal(t, "target-cm", res.Contributions[0].Name)
	})
}

// TestSimple_SoftDependency exercises explicit soft-dependency configuration
// (via WithSoftDependencies), where a node gets no DAG edge to its targets and
// is seeded with an empty scope so it can apply without waiting for the target.
func TestSimple_SoftDependency(t *testing.T) {
	require.NoError(t, features.FeatureGate.Set("CELOmitFunction=true"))
	t.Cleanup(func() {
		_ = features.FeatureGate.Set("CELOmitFunction=false")
	})

	getCM := func(t *testing.T, c client.Client, name string) *unstructured.Unstructured {
		t.Helper()
		cm := &unstructured.Unstructured{}
		cm.SetGroupVersionKind(configMapGVK)
		require.NoError(t, c.Get(context.Background(),
			types.NamespacedName{Namespace: "default", Name: name}, cm))
		return cm
	}

	t.Run("explicit soft ref applies without gating and omits the field when target unpublished", func(t *testing.T) {
		t.Parallel()
		// "cm" is configured with WithSoftDependencies; the soft ref
		// adds no DAG edge, so "cm" resolves while "later" is unpublished.
		g := generator.NewGraph("g",
			generator.WithNamespace("default"),
			generator.WithTemplate("cm", map[string]any{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]any{"name": "cm"},
				"data": map[string]any{
					"always":   "present",
					"optional": "${later.?field.orValue(omit())}",
				},
			}),
			generator.WithDef("later", map[string]any{"field": "hello"}),
		)
		rt := compileAndBuild(t, g, compiler.WithSoftDependencies("cm"))
		assert.Empty(t, rt.Program().Nodes["cm"].HardDepIDs(), "explicit soft ref must not have hard deps")
		assert.Equal(t, []string{"later"}, rt.Program().Nodes["cm"].SoftDepIDs())

		cl := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
		_, err := NewSimple(cl).Apply(context.Background(), rt, watchrouter.NoopWatcher{})
		require.NoError(t, err, "explicit soft ref must not gate or data-pend")

		cm := getCM(t, cl, "cm")
		data, _, _ := unstructured.NestedMap(cm.Object, "data")
		assert.Equal(t, "present", data["always"])
		_, hasOptional := data["optional"]
		assert.False(t, hasOptional, "optional field must be omitted while target is unpublished")
	})

	t.Run("explicit soft ref resolves the value once the target has published", func(t *testing.T) {
		t.Parallel()
		// "later" is declared first, so it publishes before "cm" resolves and
		// the optional expression sees the real value.
		g := generator.NewGraph("g",
			generator.WithNamespace("default"),
			generator.WithDef("later", map[string]any{"field": "hello"}),
			generator.WithTemplate("cm", map[string]any{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]any{"name": "cm"},
				"data": map[string]any{
					"optional": "${later.?field.orValue(omit())}",
				},
			}),
		)
		rt := compileAndBuild(t, g, compiler.WithSoftDependencies("cm"))
		assert.Empty(t, rt.Program().Nodes["cm"].HardDepIDs(), "still a soft ref, no hard edge")

		cl := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
		_, err := NewSimple(cl).Apply(context.Background(), rt, watchrouter.NoopWatcher{})
		require.NoError(t, err)

		cm := getCM(t, cl, "cm")
		data, _, _ := unstructured.NestedMap(cm.Object, "data")
		assert.Equal(t, "hello", data["optional"], "value present once target published")
	})
}

func TestSimple_OptionalRef_GatingRegression(t *testing.T) {
	t.Parallel()

	// A template referencing an externalRef via optional chaining
	// (${input.?data.ECHO_VALUE.orValue("not found")}) must be a HARD dependency:
	// if input is absent / not ready, the dependent template must NOT be created
	// prematurely.
	g := generator.NewGraph("g",
		generator.WithNamespace("default"),
		generator.WithRef("input", &expv1alpha1.ExternalRef{
			APIVersion: "v1", Kind: "ConfigMap",
			Metadata: expv1alpha1.ExternalRefMetadata{
				Name:      "input-cm",
				Namespace: "default",
			},
		}),
		generator.WithTemplate("owned", map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "owned-cm"},
			"data": map[string]any{
				"ECHO_VALUE": "${input.?data.ECHO_VALUE.orValue(\"not found\")}",
			},
		}),
	)

	rt := compileAndBuild(t, g)
	assert.Equal(t, []string{"input"}, rt.Program().Nodes["owned"].HardDepIDs(),
		"expression optional reference must be a hard dependency")
	assert.Empty(t, rt.Program().Nodes["owned"].SoftDepIDs())
	assert.Equal(t, []string{"input", "owned"}, rt.Program().TopologicalOrder)

	// Step 1: Cluster has NO input ConfigMap.
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	simple := NewSimple(cl)

	_, err := simple.Apply(context.Background(), rt, watchrouter.NoopWatcher{})
	require.Error(t, err, "apply must return error when external ref is missing")

	// Owned resource must NOT exist in the cluster.
	ownedCM := &unstructured.Unstructured{}
	ownedCM.SetGroupVersionKind(configMapGVK)
	err = cl.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "owned-cm"}, ownedCM)
	require.True(t, apierrors.IsNotFound(err), "owned resource must NOT be created while dependency is unready; got err: %v", err)

	// Step 2: Now supply the external ConfigMap and re-apply.
	inputCM := &unstructured.Unstructured{}
	inputCM.SetGroupVersionKind(configMapGVK)
	inputCM.SetName("input-cm")
	inputCM.SetNamespace("default")
	inputCM.Object["data"] = map[string]any{"ECHO_VALUE": "Hello, World!"}
	require.NoError(t, cl.Create(context.Background(), inputCM))

	// Rebuild runtime so state is clean for next reconcile cycle
	rt = compileAndBuild(t, g)
	_, err = simple.Apply(context.Background(), rt, watchrouter.NoopWatcher{})
	require.NoError(t, err, "apply must succeed once external ref is present")

	// Owned resource must now exist with the referenced value.
	err = cl.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "owned-cm"}, ownedCM)
	require.NoError(t, err)
	data, _, _ := unstructured.NestedMap(ownedCM.Object, "data")
	assert.Equal(t, "Hello, World!", data["ECHO_VALUE"])
}

func TestSimple_BareOptional_OmitsField(t *testing.T) {
	require.NoError(t, features.FeatureGate.Set("CELOmitFunction=true"))
	t.Cleanup(func() {
		_ = features.FeatureGate.Set("CELOmitFunction=false")
	})

	// A bare ${id.data.?missing} against a resolved object must omit the field
	// from the unstructured object when CELOmitFunction is enabled.
	g := generator.NewGraph("g",
		generator.WithNamespace("default"),
		generator.WithTemplate("src", map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "src-cm"},
			"data": map[string]any{
				"present": "value",
			},
		}),
		generator.WithTemplate("cm", map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "bare-opt-cm"},
			"data": map[string]any{
				"present": "${src.data.present}",
				"missing": "${src.data.?missing}",
			},
		}),
	)

	rt := compileAndBuild(t, g)
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	_, err := NewSimple(cl).Apply(context.Background(), rt, watchrouter.NoopWatcher{})
	require.NoError(t, err)

	cm := &unstructured.Unstructured{}
	cm.SetGroupVersionKind(configMapGVK)
	require.NoError(t, cl.Get(context.Background(),
		types.NamespacedName{Namespace: "default", Name: "bare-opt-cm"}, cm))

	data, _, _ := unstructured.NestedMap(cm.Object, "data")
	assert.Equal(t, "value", data["present"])
	_, hasMissing := data["missing"]
	assert.False(t, hasMissing, "bare optional missing field must be omitted, not null or present")
}
