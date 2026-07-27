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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
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

// emptyRuntime builds a Runtime over a trivial single-Def program so the
// scope-writing helpers have something to publish into.
func emptyRuntime() (*krotruntime.Runtime, *krotruntime.Node) {
	prog := &compiler.Program{
		Nodes:            map[string]*compiler.Node{"x": {ID: "x"}},
		TopologicalOrder: []string{"x"},
	}
	rt := krotruntime.New(prog, &expv1alpha1.Graph{})
	return rt, rt.Node("x")
}

// collectionNode builds a collection Node (non-empty ForEach) wired to a
// runtime so publishScope takes the list branch.
func collectionNode() (*krotruntime.Runtime, *krotruntime.Node) {
	prog := &compiler.Program{
		Nodes: map[string]*compiler.Node{"c": {
			ID:      "c",
			ForEach: []compiler.ForEachDimension{{Name: "n"}},
		}},
		TopologicalOrder: []string{"c"},
	}
	rt := krotruntime.New(prog, &expv1alpha1.Graph{})
	return rt, rt.Node("c")
}

func obj(name string) *unstructured.Unstructured {
	o := &unstructured.Unstructured{}
	o.SetAPIVersion("v1")
	o.SetKind("ConfigMap")
	o.SetName(name)
	return o
}

// TestPublishScope covers the three publishScope branches: collection
// (publishes a []any), singleton (publishes the lone object map), and the
// empty-singleton short-circuit (publishes nothing).
func TestPublishScope(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		mk   func() (*krotruntime.Runtime, *krotruntime.Node)
		objs []*unstructured.Unstructured
		id   string
		want func(t *testing.T, scope map[string]any, id string)
	}{
		{
			name: "collection publishes a list of object maps",
			mk:   collectionNode,
			id:   "c",
			objs: []*unstructured.Unstructured{obj("a"), obj("b")},
			want: func(t *testing.T, scope map[string]any, id string) {
				list, ok := scope[id].([]any)
				require.True(t, ok, "collection scope value must be []any")
				assert.Len(t, list, 2)
			},
		},
		{
			name: "singleton publishes the lone object map",
			mk:   emptyRuntime,
			id:   "x",
			objs: []*unstructured.Unstructured{obj("a")},
			want: func(t *testing.T, scope map[string]any, id string) {
				m, ok := scope[id].(map[string]any)
				require.True(t, ok)
				assert.Equal(t, "a", m["metadata"].(map[string]any)["name"])
			},
		},
		{
			name: "empty singleton publishes nothing",
			mk:   emptyRuntime,
			id:   "x",
			objs: nil,
			want: func(t *testing.T, scope map[string]any, id string) {
				_, present := scope[id]
				assert.False(t, present, "no value should be published for an empty singleton")
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rt, n := tc.mk()
			publishScope(rt, n, tc.objs)
			tc.want(t, rt.Scope(), tc.id)
		})
	}
}

// TestRefName covers both branches of refName: namespaced resources are
// rendered "ns/name"; cluster-scoped ones render just "name".
func TestRefName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		mr   expv1alpha1.ManagedResource
		want string
	}{
		{
			name: "cluster-scoped renders bare name",
			mr:   expv1alpha1.ManagedResource{Name: "global"},
			want: "global",
		},
		{
			name: "namespaced renders ns/name",
			mr:   expv1alpha1.ManagedResource{Namespace: "team", Name: "cm"},
			want: "team/cm",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, refName(tc.mr))
		})
	}
}

// TestDelete_UIDPrecondition exercises the UID-precondition branch of
// Delete: a resource carrying a UID adds a DeleteOptions precondition,
// which a recording client surfaces, while a resource without a UID does
// not. The fake client does not honor preconditions, so we assert on the
// captured options instead of behavior.
func TestDelete_UIDPrecondition(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		uid        string
		wantPrecon bool
	}{
		{name: "with UID adds a precondition", uid: "abc-123", wantPrecon: true},
		{name: "without UID adds no precondition", uid: "", wantPrecon: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := &recordingDeleteClient{}
			ex := NewSimple(rec)
			err := ex.Delete(context.Background(), []expv1alpha1.ManagedResource{{
				NodeID: "n", APIVersion: "v1", Kind: "ConfigMap",
				Namespace: "default", Name: "cm", UID: tc.uid,
			}})
			require.NoError(t, err)
			require.Len(t, rec.opts, 1)
			assert.Equal(t, tc.wantPrecon, rec.hasPrecondition(0))
		})
	}
}

// TestMappingFor covers mappingFor's dynamic-vs-static split and the
// incomplete-GVK guard.
func TestMappingFor(t *testing.T) {
	t.Parallel()
	_, disco := testk8s.NewFakeResolver()
	rm := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(disco))
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).WithRESTMapper(rm).Build()
	ex := NewSimple(cl)

	cases := []struct {
		name    string
		node    *compiler.Node
		obj     *unstructured.Unstructured
		wantGVR schema.GroupVersionResource
		wantNSd bool
		wantErr string
	}{
		{
			name: "static node returns the compiled GVR and scope",
			node: func() *compiler.Node {
				n := &compiler.Node{ID: "static", Namespaced: true}
				n.GVR = schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
				return n
			}(),
			obj:     obj("cm"),
			wantGVR: schema.GroupVersionResource{Version: "v1", Resource: "configmaps"},
			wantNSd: true,
		},
		{
			name: "dynamic node with incomplete GVK errors before REST mapping",
			node: &compiler.Node{ID: "dyn-bad", DynamicGVK: true},
			obj: func() *unstructured.Unstructured {
				o := &unstructured.Unstructured{}
				o.SetKind("") // no kind/version → incomplete
				return o
			}(),
			wantErr: "incomplete GVK",
		},
		{
			name:    "dynamic node resolves a known GVK through the REST mapper",
			node:    &compiler.Node{ID: "dyn-ok", DynamicGVK: true},
			obj:     obj("cm"),
			wantGVR: schema.GroupVersionResource{Version: "v1", Resource: "configmaps"},
			wantNSd: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			n := nodeFromSpec(tc.node.ID, tc.node)
			gvr, nsd, err := ex.mappingFor(n, tc.obj)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantGVR, gvr)
			assert.Equal(t, tc.wantNSd, nsd)
		})
	}
}

// TestWatchObject covers the nil-watcher short-circuit and the real
// registration path through a recording watcher.
func TestWatchObject(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		watcher   watchrouter.Watcher
		wantCalls int
	}{
		{name: "nil watcher is a no-op", watcher: nil, wantCalls: 0},
		{name: "real watcher receives one registration", watcher: nil /* set below */, wantCalls: 1},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var rec *recordingWatcher
			w := tc.watcher
			if i == 1 {
				rec = &recordingWatcher{}
				w = rec
			}
			err := (&Simple{}).watchObject(w, "n",
				schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}, obj("cm"))
			require.NoError(t, err)
			if rec != nil {
				assert.Len(t, rec.reqs, tc.wantCalls)
			}
		})
	}
}

// TestApplySubgraph_NilProgram pins the guard that rejects a subgraph node
// whose compiled child program is missing.
func TestApplySubgraph_NilProgram(t *testing.T) {
	t.Parallel()
	prog := &compiler.Program{
		Nodes: map[string]*compiler.Node{"sub": {
			ID:   "sub",
			Kind: compiler.NodeKindGraph,
			// SubProgram intentionally nil.
		}},
		TopologicalOrder: []string{"sub"},
	}
	rt := krotruntime.New(prog, generator.NewGraph("g", generator.WithNamespace("default")))
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()

	_, err := NewSimple(cl).Apply(context.Background(), rt, watchrouter.NoopWatcher{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no compiled program")
}

// TestApply_SoftAndWatchErrors covers the soft-error continuation paths in
// Apply that the happy-path table doesn't reach: an includeWhen that
// references not-yet-available data marks the node Unresolved and requeues,
// and a watch-registration failure aborts the apply with a wrapped error.
func TestApply_SoftAndWatchErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		graph       *expv1alpha1.Graph
		watcher     watchrouter.Watcher
		wantErr     string
		wantNotRdy  bool
		unresolved  []string
		wantApplied int
	}{
		{
			name: "includeWhen on pending data marks the node Unresolved and requeues",
			graph: generator.NewGraph("g",
				generator.WithNamespace("default"),
				generator.WithDef("seed", map[string]any{"k": "v"}),
				// dyn field resolves to a string; the sub-field access is a
				// data-pending CEL error at runtime, so includeWhen is soft.
				generator.WithDef("cfg", map[string]any{"flag": "${'literal'}"}),
				generator.WithTemplate("cm", map[string]any{
					"apiVersion": "v1", "kind": "ConfigMap",
					"metadata": map[string]any{"name": "cm"},
					"data":     map[string]any{"k": "v"},
				}),
				generator.WithIncludeWhen("${cfg.flag.bogus != ''}"),
			),
			watcher:    watchrouter.NoopWatcher{},
			wantNotRdy: true,
			unresolved: []string{"cm"},
		},
		{
			name: "watch registration failure aborts apply with a wrapped error",
			graph: generator.NewGraph("g",
				generator.WithNamespace("default"),
				generator.WithTemplate("cm", map[string]any{
					"apiVersion": "v1", "kind": "ConfigMap",
					"metadata": map[string]any{"name": "cm"},
					"data":     map[string]any{"k": "v"},
				}),
			),
			watcher: errWatcher{},
			wantErr: "register watch",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rt := compileAndBuild(t, tc.graph)
			cl := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
			res, err := NewSimple(cl).Apply(context.Background(), rt, tc.watcher)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.Error(t, err)
			if tc.wantNotRdy {
				assert.ErrorIs(t, err, ErrNotReady)
			}
			for _, u := range tc.unresolved {
				assert.Contains(t, res.Unresolved, u)
			}
		})
	}
}

// errWatcher fails every Watch call so the registration error path is hit.
type errWatcher struct{}

func (errWatcher) Watch(watchrouter.WatchRequest) error { return assert.AnError }
func (errWatcher) Done(bool)                            {}

// nodeFromSpec materializes a runtime.Node for the given compiled spec by
// constructing a one-node Runtime and fetching the wrapper back, since
// runtime.Node has no exported constructor.
func nodeFromSpec(id string, spec *compiler.Node) *krotruntime.Node {
	prog := &compiler.Program{
		Nodes:            map[string]*compiler.Node{id: spec},
		TopologicalOrder: []string{id},
	}
	rt := krotruntime.New(prog, &expv1alpha1.Graph{})
	return rt.Node(id)
}

// recordingDeleteClient captures the DeleteOptions passed to each Delete so
// the UID-precondition branch can be asserted without an envtest server.
type recordingDeleteClient struct {
	clientStub
	opts [][]client.DeleteOption
}

func (r *recordingDeleteClient) Delete(_ context.Context, _ client.Object, opts ...client.DeleteOption) error {
	r.opts = append(r.opts, opts)
	return nil
}

// hasPrecondition reports whether the i-th captured Delete carried a UID
// precondition.
func (r *recordingDeleteClient) hasPrecondition(i int) bool {
	for _, o := range r.opts[i] {
		do, ok := o.(*client.DeleteOptions)
		if ok && do.Preconditions != nil && do.Preconditions.UID != nil {
			return true
		}
	}
	return false
}

// clientStub satisfies client.Client; only Delete is overridden by the
// embedding type. Methods panic if unexpectedly called.
type clientStub struct{ client.Client }

// recordingWatcher records every WatchRequest it receives.
type recordingWatcher struct {
	reqs []watchrouter.WatchRequest
}

func (w *recordingWatcher) Watch(req watchrouter.WatchRequest) error {
	w.reqs = append(w.reqs, req)
	return nil
}

func (w *recordingWatcher) Done(_ bool) {}
