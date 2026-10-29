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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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
	"github.com/kubernetes-sigs/kro/pkg/graphengine/watchrouter"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
	testk8s "github.com/kubernetes-sigs/kro/pkg/testutil/k8s"
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

// TestDelete_UIDPrecondition checks identity and snapshot preconditions,
// ownership checks, and retryable cleanup errors.
func TestDelete_UIDPrecondition(t *testing.T) {
	t.Parallel()
	const ownerUID types.UID = "graph-uid"
	self := templateFieldManager(ownerUID)
	peer := templateFieldManager("peer-uid")
	forbidden := apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, "cm", errors.New("denied"))
	missing := apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, "cm")
	noMatch := &meta.NoKindMatchError{GroupKind: configMapGVK.GroupKind(), SearchedVersions: []string{"v1"}}
	unavailable := apierrors.NewServiceUnavailable("try again")
	conflict := apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, "cm", errors.New("precondition mismatch"))
	cases := []struct {
		name         string
		uid          string
		managers     []string
		labelsOnly   bool
		emptyOwner   bool
		emptyLiveUID bool
		getErr       error
		deleteErr    error
		wantUID      types.UID
		wantErr      error
	}{
		{name: "recorded UID is used without a GET", uid: "recorded", getErr: forbidden, wantUID: "recorded"},
		{name: "recorded UID needs no owner argument", uid: "recorded", emptyOwner: true, wantUID: "recorded"},
		{name: "current self marker recovers live UID", managers: []string{self}, wantUID: "live"},
		{name: "template manager suffix does not establish ownership", managers: []string{self + ".extra"}},
		{name: "self plus non-template manager recovers", managers: []string{self, "kubectl"}, wantUID: "live"},
		{name: "unmarked object survives"},
		{name: "peer object survives", managers: []string{peer}},
		{name: "peer vetoes self marker", managers: []string{self, peer}},
		{name: "patch manager does not establish ownership", managers: []string{patchFieldManager(ownerUID, "n")}},
		{name: "shared RGD manager does not establish ownership", managers: []string{FieldManager}},
		{name: "unrelated manager does not establish ownership", managers: []string{"kro-other"}},
		{name: "collection labels do not establish ownership", labelsOnly: true},
		{name: "empty owner cannot recover", emptyOwner: true, managers: []string{templateFieldManager("")}},
		{name: "empty live UID cannot be deleted", managers: []string{self}, emptyLiveUID: true},
		{name: "missing object skips", getErr: missing},
		{name: "UID-free NoMatch GET skips", getErr: noMatch},
		{name: "UID-free Forbidden GET skips", getErr: forbidden},
		{name: "transient GET failure propagates", getErr: unavailable, wantErr: unavailable},
		{name: "verified DELETE Forbidden propagates", managers: []string{self}, deleteErr: forbidden, wantUID: "live", wantErr: forbidden},
		{name: "recorded DELETE Forbidden propagates", uid: "recorded", deleteErr: forbidden, wantUID: "recorded", wantErr: forbidden},
		{name: "known UID NoMatch stays an error", uid: "recorded", deleteErr: noMatch, wantUID: "recorded", wantErr: noMatch},
		{name: "verified DELETE NoMatch propagates", managers: []string{self}, deleteErr: noMatch, wantUID: "live", wantErr: noMatch},
		{name: "verified DELETE NotFound is tolerated", managers: []string{self}, deleteErr: missing, wantUID: "live"},
		{name: "verified DELETE Conflict is retryable", managers: []string{self}, deleteErr: conflict, wantUID: "live", wantErr: conflict},
		{name: "recorded DELETE Conflict is tolerated", uid: "recorded", deleteErr: conflict, wantUID: "recorded"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			live := obj("cm")
			live.SetNamespace("default")
			live.SetResourceVersion("observed")
			if !tc.emptyLiveUID {
				live.SetUID("live")
			}
			for _, manager := range tc.managers {
				live.SetManagedFields(append(live.GetManagedFields(), metav1.ManagedFieldsEntry{Manager: manager}))
			}
			if tc.labelsOnly {
				live.SetLabels(map[string]string{metadata.InstanceIDLabel: string(ownerUID), metadata.NodeIDLabel: "n"})
			}
			owner := ownerUID
			if tc.emptyOwner {
				owner = ""
			}
			rec := &recordingDeleteClient{live: live, getErr: tc.getErr, deleteErr: tc.deleteErr}
			ex := NewSimple(rec)
			err := ex.Delete(context.Background(), owner, []expv1alpha1.ManagedResource{{
				NodeID: "n", APIVersion: "v1", Kind: "ConfigMap",
				Namespace: "default", Name: "cm", UID: tc.uid,
			}})
			if tc.wantErr != nil {
				assert.ErrorIs(t, err, tc.wantErr)
			} else {
				require.NoError(t, err)
			}
			if tc.uid != "" || tc.emptyOwner {
				assert.Empty(t, rec.gets, "recorded UID and empty-owner paths must not look up a live UID")
			}
			if tc.wantUID == "" {
				assert.Empty(t, rec.opts, "unverifiable entries must not issue DELETE")
			} else {
				require.Len(t, rec.opts, 1)
				preconditions := (&client.DeleteOptions{}).ApplyOptions(rec.opts[0]).Preconditions
				require.NotNil(t, preconditions)
				require.NotNil(t, preconditions.UID)
				assert.Equal(t, tc.wantUID, *preconditions.UID)
				if tc.uid == "" {
					require.NotNil(t, preconditions.ResourceVersion)
					assert.Equal(t, live.GetResourceVersion(), *preconditions.ResourceVersion)
				} else {
					assert.Nil(t, preconditions.ResourceVersion, "recorded UIDs use identity-only preconditions")
				}
			}
		})
	}
}

func TestSimple_Delete_ContinuesAfterErrors(t *testing.T) {
	getErr := apierrors.NewServiceUnavailable("read unavailable")
	deleteErr := apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, "cm", errors.New("delete denied"))
	rec := &recordingDeleteClient{getErr: getErr, deleteErr: deleteErr}
	err := NewSimple(rec).Delete(context.Background(), "graph-uid", []expv1alpha1.ManagedResource{
		{APIVersion: "v1", Kind: "ConfigMap", Name: "dependency", UID: "uid-dependency"},
		{APIVersion: "v1", Kind: "ConfigMap", Name: "writeahead"},
		{APIVersion: "v1", Kind: "ConfigMap", Name: "dependent", UID: "uid-dependent"},
	})
	assert.ErrorIs(t, err, getErr)
	assert.ErrorIs(t, err, deleteErr)
	assert.Equal(t, []string{"delete dependent", "get writeahead", "delete dependency"}, rec.calls)
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
			err := (&Simple{}).watchObject(context.Background(), w, "n",
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
// and a watch-registration failure is soft (issue #17): the object is still
// applied and the apply does not abort.
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
				generator.WithTemplate("src", map[string]any{
					"apiVersion": "v1", "kind": "ConfigMap",
					"metadata": map[string]any{"name": "src"},
					"data":     map[string]any{"k": "v"},
				}),
				generator.WithTemplate("cm", map[string]any{
					"apiVersion": "v1", "kind": "ConfigMap",
					"metadata": map[string]any{"name": "cm"},
					"data":     map[string]any{"k": "v"},
				}),
				generator.WithIncludeWhen("${src.data.bogus != ''}"),
			),
			watcher:    watchrouter.NoopWatcher{},
			wantNotRdy: true,
			unresolved: []string{"cm"},
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
	live      *unstructured.Unstructured
	getErr    error
	deleteErr error
	gets      []client.ObjectKey
	calls     []string
	opts      [][]client.DeleteOption
}

func (r *recordingDeleteClient) Get(_ context.Context, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	r.gets = append(r.gets, key)
	r.calls = append(r.calls, "get "+key.Name)
	if r.getErr != nil {
		return r.getErr
	}
	obj.(*unstructured.Unstructured).Object = r.live.DeepCopy().Object
	return nil
}

func (r *recordingDeleteClient) Delete(_ context.Context, obj client.Object, opts ...client.DeleteOption) error {
	r.calls = append(r.calls, "delete "+obj.GetName())
	r.opts = append(r.opts, opts)
	return r.deleteErr
}

// clientStub satisfies client.Client; Get and Delete are overridden by the
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
