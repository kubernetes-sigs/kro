// Copyright 2025 The Kubernetes Authors.
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

package graphrevision

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/graph"
	"github.com/kubernetes-sigs/kro/pkg/graph/revisions"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
)

func TestGraphRevisionReconcilerCases(t *testing.T) {
	compiled := testCompiledGraph()

	tests := []struct {
		name             string
		mutateRevision   func(*v1alpha1.GraphRevision)
		buildClient      func(*testing.T, *runtime.Scheme, *v1alpha1.GraphRevision) client.Client
		seedRegistry     func(*revisions.Registry, *v1alpha1.GraphRevision)
		compile          compileGraphFunc
		wantErrContains  []string
		wantFinalizer    *bool
		wantVerified     *metav1.ConditionStatus
		wantReady        *metav1.ConditionStatus
		wantOrder        []string
		wantResourceIDs  []string
		wantRegistry     *revisions.Entry
		wantRegistryMiss bool
	}{
		{
			name: "compile success marks revision active",
			compile: func(rgd *v1alpha1.ResourceGraphDefinition, _ graph.RGDConfig) (*graph.Graph, error) {
				assert.Equal(t, "demo-rgd", rgd.Name)
				assert.Equal(t, revisionUID, rgd.UID)
				return compiled, nil
			},
			wantFinalizer:   boolPtr(true),
			wantVerified:    conditionStatusPtr(metav1.ConditionTrue),
			wantReady:       conditionStatusPtr(metav1.ConditionTrue),
			wantOrder:       []string{"config", "deploy"},
			wantResourceIDs: []string{"deploy"},
			wantRegistry: &revisions.Entry{
				RGDName:       "demo-rgd",
				Revision:      1,
				SpecHash:      "hash-1",
				State:         revisions.RevisionStateActive,
				CompiledGraph: compiled,
			},
		},
		{
			name: "compile failure marks revision failed",
			compile: func(*v1alpha1.ResourceGraphDefinition, graph.RGDConfig) (*graph.Graph, error) {
				return nil, errors.New("graph compile failed")
			},
			wantErrContains: []string{"graph compile failed"},
			wantFinalizer:   boolPtr(true),
			wantVerified:    conditionStatusPtr(metav1.ConditionFalse),
			wantReady:       conditionStatusPtr(metav1.ConditionFalse),
			wantRegistry: &revisions.Entry{
				RGDName:  "demo-rgd",
				Revision: 1,
				SpecHash: "hash-1",
				State:    revisions.RevisionStateFailed,
			},
		},
		{
			name: "deletion removes registry entry after finalizer removal",
			mutateRevision: func(revision *v1alpha1.GraphRevision) {
				metadata.SetGraphRevisionFinalizer(revision)
				ts := metav1.Now()
				revision.DeletionTimestamp = &ts
			},
			seedRegistry: func(registry *revisions.Registry, revision *v1alpha1.GraphRevision) {
				registry.Put(revisions.Entry{
					RGDName:       revision.Spec.ResourceGraphDefinitionName,
					Revision:      revision.Spec.Revision,
					State:         revisions.RevisionStateActive,
					CompiledGraph: &graph.Graph{},
				})
			},
			compile:          panicCompile,
			wantFinalizer:    boolPtr(false),
			wantRegistryMiss: true,
		},
		{
			name: "deletion keeps registry entry when finalizer patch fails",
			mutateRevision: func(revision *v1alpha1.GraphRevision) {
				metadata.SetGraphRevisionFinalizer(revision)
				ts := metav1.Now()
				revision.DeletionTimestamp = &ts
			},
			buildClient: func(t *testing.T, scheme *runtime.Scheme, revision *v1alpha1.GraphRevision) client.Client {
				base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(revision).Build()
				return &patchFailClient{Client: base, patchErr: errors.New("patch failed")}
			},
			seedRegistry: func(registry *revisions.Registry, revision *v1alpha1.GraphRevision) {
				registry.Put(revisions.Entry{
					RGDName:       revision.Spec.ResourceGraphDefinitionName,
					Revision:      revision.Spec.Revision,
					State:         revisions.RevisionStateActive,
					CompiledGraph: &graph.Graph{},
				})
			},
			compile:         panicCompile,
			wantErrContains: []string{"patch failed"},
			wantFinalizer:   boolPtr(true),
			wantRegistry: &revisions.Entry{
				RGDName:       "demo-rgd",
				Revision:      1,
				State:         revisions.RevisionStateActive,
				CompiledGraph: &graph.Graph{},
			},
		},
		{
			name: "reconcile returns setManaged patch error",
			buildClient: func(t *testing.T, scheme *runtime.Scheme, revision *v1alpha1.GraphRevision) client.Client {
				base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(revision).Build()
				return &patchFailClient{Client: base, patchErr: errors.New("patch failed")}
			},
			compile:          panicCompile,
			wantErrContains:  []string{"patch failed"},
			wantFinalizer:    boolPtr(false),
			wantRegistryMiss: true,
		},
		{
			name: "reconcile joins status patch failure with compile error",
			buildClient: func(t *testing.T, scheme *runtime.Scheme, revision *v1alpha1.GraphRevision) client.Client {
				base := fake.NewClientBuilder().
					WithScheme(scheme).
					WithStatusSubresource(&v1alpha1.GraphRevision{}).
					WithObjects(revision).
					Build()
				return &statusPatchFailClient{Client: base, patchErr: errors.New("status patch failed")}
			},
			compile: func(*v1alpha1.ResourceGraphDefinition, graph.RGDConfig) (*graph.Graph, error) {
				return nil, errors.New("graph compile failed")
			},
			wantErrContains: []string{"graph compile failed", "status patch failed"},
			wantFinalizer:   boolPtr(true),
			wantRegistry: &revisions.Entry{
				RGDName:  "demo-rgd",
				Revision: 1,
				SpecHash: "hash-1",
				State:    revisions.RevisionStateFailed,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			require.NoError(t, v1alpha1.AddToScheme(scheme))

			revision := newTestGraphRevision("demo-rgd-rev-1")
			if tt.mutateRevision != nil {
				tt.mutateRevision(revision)
			}

			buildClient := tt.buildClient
			if buildClient == nil {
				buildClient = func(t *testing.T, scheme *runtime.Scheme, revision *v1alpha1.GraphRevision) client.Client {
					return fake.NewClientBuilder().
						WithScheme(scheme).
						WithStatusSubresource(&v1alpha1.GraphRevision{}).
						WithObjects(revision).
						Build()
				}
			}
			cl := buildClient(t, scheme, revision)

			registry := revisions.NewRegistry()
			if tt.seedRegistry != nil {
				tt.seedRegistry(registry, revision)
			}

			reconciler := &GraphRevisionReconciler{
				Client:                  cl,
				compileGraph:            tt.compile,
				registry:                registry,
				rgdConfig:               graph.RGDConfig{},
				maxConcurrentReconciles: 1,
			}

			_, err := reconciler.Reconcile(context.Background(), revision)
			if len(tt.wantErrContains) == 0 {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				for _, want := range tt.wantErrContains {
					assert.Contains(t, err.Error(), want)
				}
			}

			assertStoredRevisionState(t, cl, revision, tt.wantFinalizer, tt.wantVerified, tt.wantReady, tt.wantOrder, tt.wantResourceIDs)
			assertRegistryState(t, registry, tt.wantRegistry, tt.wantRegistryMiss)
		})
	}
}

func TestGraphRevisionStatusCases(t *testing.T) {
	tests := []struct {
		name               string
		seed               func() *v1alpha1.GraphRevision
		action             func(context.Context, *GraphRevisionReconciler, *v1alpha1.GraphRevision) error
		wantErrContains    string
		wantPatchCalls     int
		wantStatusPatches  int
		assertStoredObject func(*testing.T, client.Client, *v1alpha1.GraphRevision)
	}{
		{
			name: "setManaged adds finalizer when absent",
			seed: func() *v1alpha1.GraphRevision { return newTestGraphRevision("managed-add") },
			action: func(ctx context.Context, reconciler *GraphRevisionReconciler, obj *v1alpha1.GraphRevision) error {
				return reconciler.setManaged(ctx, obj)
			},
			wantPatchCalls: 1,
			assertStoredObject: func(t *testing.T, cl client.Client, obj *v1alpha1.GraphRevision) {
				stored := &v1alpha1.GraphRevision{}
				require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(obj), stored))
				assert.True(t, metadata.HasGraphRevisionFinalizer(stored))
			},
		},
		{
			name: "setManaged is a no-op when finalizer already exists",
			seed: func() *v1alpha1.GraphRevision {
				obj := newTestGraphRevision("managed-noop")
				metadata.SetGraphRevisionFinalizer(obj)
				return obj
			},
			action: func(ctx context.Context, reconciler *GraphRevisionReconciler, obj *v1alpha1.GraphRevision) error {
				return reconciler.setManaged(ctx, obj)
			},
			wantPatchCalls: 0,
			assertStoredObject: func(t *testing.T, cl client.Client, obj *v1alpha1.GraphRevision) {
				stored := &v1alpha1.GraphRevision{}
				require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(obj), stored))
				assert.True(t, metadata.HasGraphRevisionFinalizer(stored))
			},
		},
		{
			name: "setUnmanaged removes finalizer when present",
			seed: func() *v1alpha1.GraphRevision {
				obj := newTestGraphRevision("unmanaged-remove")
				metadata.SetGraphRevisionFinalizer(obj)
				return obj
			},
			action: func(ctx context.Context, reconciler *GraphRevisionReconciler, obj *v1alpha1.GraphRevision) error {
				return reconciler.setUnmanaged(ctx, obj)
			},
			wantPatchCalls: 1,
			assertStoredObject: func(t *testing.T, cl client.Client, obj *v1alpha1.GraphRevision) {
				stored := &v1alpha1.GraphRevision{}
				require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(obj), stored))
				assert.False(t, metadata.HasGraphRevisionFinalizer(stored))
			},
		},
		{
			name: "setUnmanaged is a no-op when finalizer is already absent",
			seed: func() *v1alpha1.GraphRevision { return newTestGraphRevision("unmanaged-noop") },
			action: func(ctx context.Context, reconciler *GraphRevisionReconciler, obj *v1alpha1.GraphRevision) error {
				return reconciler.setUnmanaged(ctx, obj)
			},
			wantPatchCalls: 0,
			assertStoredObject: func(t *testing.T, cl client.Client, obj *v1alpha1.GraphRevision) {
				stored := &v1alpha1.GraphRevision{}
				require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(obj), stored))
				assert.False(t, metadata.HasGraphRevisionFinalizer(stored))
			},
		},
		{
			name: "updateStatus patches changed status",
			seed: func() *v1alpha1.GraphRevision { return newTestGraphRevision("status-update") },
			action: func(ctx context.Context, reconciler *GraphRevisionReconciler, obj *v1alpha1.GraphRevision) error {
				NewConditionsMarkerFor(obj).GraphVerified()
				return reconciler.updateStatus(ctx, obj, []string{"config", "deploy"}, []v1alpha1.ResourceInformation{buildResourceInfo("deploy", []string{"config"})})
			},
			wantStatusPatches: 1,
			assertStoredObject: func(t *testing.T, cl client.Client, obj *v1alpha1.GraphRevision) {
				stored := &v1alpha1.GraphRevision{}
				require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(obj), stored))
				assert.Equal(t, []string{"config", "deploy"}, stored.Status.TopologicalOrder)
				require.Len(t, stored.Status.Resources, 1)
				assert.Equal(t, "deploy", stored.Status.Resources[0].ID)
				verified := findCondition(stored.Status.Conditions, v1alpha1.GraphRevisionConditionTypeGraphVerified)
				require.NotNil(t, verified)
				assert.Equal(t, metav1.ConditionTrue, verified.Status)
			},
		},
		{
			name: "updateStatus is a no-op when status is already current",
			seed: func() *v1alpha1.GraphRevision {
				obj := newTestGraphRevision("status-noop")
				NewConditionsMarkerFor(obj).GraphVerified()
				obj.Status.TopologicalOrder = []string{"config", "deploy"}
				obj.Status.Resources = []v1alpha1.ResourceInformation{buildResourceInfo("deploy", []string{"config"})}
				return obj
			},
			action: func(ctx context.Context, reconciler *GraphRevisionReconciler, obj *v1alpha1.GraphRevision) error {
				return reconciler.updateStatus(ctx, obj, []string{"config", "deploy"}, []v1alpha1.ResourceInformation{buildResourceInfo("deploy", []string{"config"})})
			},
			wantStatusPatches: 0,
			assertStoredObject: func(t *testing.T, cl client.Client, obj *v1alpha1.GraphRevision) {
				stored := &v1alpha1.GraphRevision{}
				require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(obj), stored))
				assert.Equal(t, obj.Status, stored.Status)
			},
		},
		{
			name: "updateStatus returns an error when the object no longer exists",
			seed: func() *v1alpha1.GraphRevision { return nil },
			action: func(ctx context.Context, reconciler *GraphRevisionReconciler, obj *v1alpha1.GraphRevision) error {
				NewConditionsMarkerFor(obj).GraphVerified()
				return reconciler.updateStatus(ctx, obj, nil, nil)
			},
			wantErrContains:   "failed to get current graph revision",
			wantPatchCalls:    0,
			wantStatusPatches: 0,
			assertStoredObject: func(*testing.T, client.Client, *v1alpha1.GraphRevision) {
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			require.NoError(t, v1alpha1.AddToScheme(scheme))

			var stored *v1alpha1.GraphRevision
			if tt.seed != nil {
				stored = tt.seed()
			}
			obj := newTestGraphRevision("target")
			if stored != nil {
				obj = stored.DeepCopy()
			}

			builder := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.GraphRevision{})
			if stored != nil {
				builder = builder.WithObjects(stored)
			}
			counting := &countingClient{Client: builder.Build()}
			if stored != nil {
				fresh := &v1alpha1.GraphRevision{}
				require.NoError(t, counting.Get(context.Background(), client.ObjectKeyFromObject(stored), fresh))
				obj = fresh
			}
			reconciler := &GraphRevisionReconciler{Client: counting}

			err := tt.action(context.Background(), reconciler, obj)
			if tt.wantErrContains == "" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErrContains)
			}

			assert.Equal(t, tt.wantPatchCalls, counting.patchCalls)
			assert.Equal(t, tt.wantStatusPatches, counting.statusPatchCalls)
			tt.assertStoredObject(t, counting, obj)
		})
	}
}

func TestGraphRevisionConstructorCase(t *testing.T) {
	registry := revisions.NewRegistry()
	cfg := graph.RGDConfig{MaxCollectionSize: 10, MaxCollectionDimensionSize: 3}

	reconciler := NewGraphRevisionReconciler(nil, registry, 7, cfg)

	require.NotNil(t, reconciler)
	assert.Equal(t, registry, reconciler.registry)
	assert.Equal(t, cfg, reconciler.rgdConfig)
	assert.Equal(t, 7, reconciler.maxConcurrentReconciles)
	require.NotNil(t, reconciler.compileGraph)
}

func assertStoredRevisionState(
	t *testing.T,
	cl client.Client,
	revision *v1alpha1.GraphRevision,
	wantFinalizer *bool,
	wantVerified *metav1.ConditionStatus,
	wantReady *metav1.ConditionStatus,
	wantOrder []string,
	wantResourceIDs []string,
) {
	t.Helper()

	stored := &v1alpha1.GraphRevision{}
	err := cl.Get(context.Background(), client.ObjectKeyFromObject(revision), stored)
	if wantFinalizer != nil && !*wantFinalizer && apierrors.IsNotFound(err) {
		return
	}
	require.NoError(t, err)

	if wantFinalizer != nil {
		assert.Equal(t, *wantFinalizer, metadata.HasGraphRevisionFinalizer(stored))
	}
	if wantVerified != nil {
		verified := findCondition(stored.Status.Conditions, v1alpha1.GraphRevisionConditionTypeGraphVerified)
		require.NotNil(t, verified)
		assert.Equal(t, *wantVerified, verified.Status)
	}
	if wantReady != nil {
		ready := findCondition(stored.Status.Conditions, v1alpha1.GraphRevisionConditionTypeReady)
		require.NotNil(t, ready)
		assert.Equal(t, *wantReady, ready.Status)
	}
	if wantOrder != nil {
		assert.Equal(t, wantOrder, stored.Status.TopologicalOrder)
	}
	if wantResourceIDs != nil {
		assert.Equal(t, wantResourceIDs, resourceIDs(stored.Status.Resources))
	}
}

func assertRegistryState(t *testing.T, registry *revisions.Registry, want *revisions.Entry, wantMissing bool) {
	t.Helper()

	entry, ok := registry.Get("demo-rgd", 1)
	if wantMissing {
		assert.False(t, ok)
		return
	}
	require.NotNil(t, want)
	require.True(t, ok)
	assert.Equal(t, want.RGDName, entry.RGDName)
	assert.Equal(t, want.Revision, entry.Revision)
	assert.Equal(t, want.SpecHash, entry.SpecHash)
	assert.Equal(t, want.State, entry.State)
	if want.CompiledGraph == nil {
		assert.Nil(t, entry.CompiledGraph)
	} else {
		require.NotNil(t, entry.CompiledGraph)
		assert.Equal(t, want.CompiledGraph.TopologicalOrder, entry.CompiledGraph.TopologicalOrder)
	}
}

func resourceIDs(resources []v1alpha1.ResourceInformation) []string {
	ids := make([]string, 0, len(resources))
	for _, resource := range resources {
		ids = append(ids, resource.ID)
	}
	return ids
}

func testCompiledGraph() *graph.Graph {
	return &graph.Graph{
		TopologicalOrder: []string{"config", "deploy"},
		Nodes: map[string]*graph.Node{
			"config": {Meta: graph.NodeMeta{Dependencies: nil}},
			"deploy": {Meta: graph.NodeMeta{Dependencies: []string{"config"}}},
		},
	}
}

func newTestGraphRevision(name string) *v1alpha1.GraphRevision {
	return &v1alpha1.GraphRevision{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: v1alpha1.GraphRevisionSpec{
			ResourceGraphDefinitionName: "demo-rgd",
			ResourceGraphDefinitionUID:  revisionUID,
			Revision:                    1,
			SpecHash:                    "hash-1",
			DefinitionSpec: v1alpha1.ResourceGraphDefinitionSpec{
				Schema: &v1alpha1.Schema{
					Kind:       "Demo",
					APIVersion: "v1alpha1",
					Group:      "kro.run",
				},
			},
		},
	}
}

func findCondition(conditions []v1alpha1.Condition, t v1alpha1.ConditionType) *v1alpha1.Condition {
	for i := range conditions {
		if conditions[i].Type == t {
			return &conditions[i]
		}
	}
	return nil
}

var revisionUID = types.UID("demo-rgd-uid")

func boolPtr(v bool) *bool { return &v }

func conditionStatusPtr(v metav1.ConditionStatus) *metav1.ConditionStatus { return &v }

func panicCompile(*v1alpha1.ResourceGraphDefinition, graph.RGDConfig) (*graph.Graph, error) {
	return nil, errors.New("compile should not be called")
}

type patchFailClient struct {
	client.Client
	patchErr error
}

func (c *patchFailClient) Patch(_ context.Context, _ client.Object, _ client.Patch, _ ...client.PatchOption) error {
	return c.patchErr
}

type statusPatchFailClient struct {
	client.Client
	patchErr error
}

func (c *statusPatchFailClient) Status() client.StatusWriter {
	return failingStatusWriter{SubResourceWriter: c.Client.Status(), patchErr: c.patchErr}
}

type failingStatusWriter struct {
	client.SubResourceWriter
	patchErr error
}

func (w failingStatusWriter) Patch(_ context.Context, _ client.Object, _ client.Patch, _ ...client.SubResourcePatchOption) error {
	return w.patchErr
}

type countingClient struct {
	client.Client
	patchCalls       int
	statusPatchCalls int
}

func (c *countingClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	c.patchCalls++
	return c.Client.Patch(ctx, obj, patch, opts...)
}

func (c *countingClient) Status() client.StatusWriter {
	return countingStatusWriter{SubResourceWriter: c.Client.Status(), parent: c}
}

type countingStatusWriter struct {
	client.SubResourceWriter
	parent *countingClient
}

func (w countingStatusWriter) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
	w.parent.statusPatchCalls++
	return w.SubResourceWriter.Patch(ctx, obj, patch, opts...)
}
