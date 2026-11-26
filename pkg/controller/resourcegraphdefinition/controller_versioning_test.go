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

package resourcegraphdefinition

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	metadatafake "k8s.io/client-go/metadata/fake"
	clienttesting "k8s.io/client-go/testing"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	featuregatetesting "k8s.io/component-base/featuregate/testing"

	internalv1alpha1 "github.com/kubernetes-sigs/kro/api/internal.kro.run/v1alpha1"
	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	kroclient "github.com/kubernetes-sigs/kro/pkg/client"
	krofake "github.com/kubernetes-sigs/kro/pkg/client/fake"
	"github.com/kubernetes-sigs/kro/pkg/dynamiccontroller"
	"github.com/kubernetes-sigs/kro/pkg/features"
	"github.com/kubernetes-sigs/kro/pkg/graph/revisions"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
)

func instanceGVRFor(apiVersion string) schema.GroupVersionResource {
	return metadata.GetResourceGraphDefinitionInstanceGVR(&v1alpha1.Schema{
		Group:      "example.io",
		APIVersion: apiVersion,
		Kind:       "Network",
	})
}

// newInstance builds a trimmed instance object of the test RGD's kind. The
// revision fields mirror what the instance controller writes to status.
func newInstance(name string, graphRevision int64, pin string) *unstructured.Unstructured {
	inst := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "example.io/v1alpha1",
		"kind":       "Network",
		"metadata": map[string]any{
			"name":      name,
			"namespace": "default",
		},
	}}
	if graphRevision > 0 {
		inst.Object["status"] = map[string]any{"graphRevision": graphRevision}
	}
	if pin != "" {
		inst.SetAnnotations(map[string]string{v1alpha1.InstanceGraphRevisionAnnotation: pin})
	}
	return inst
}

// newRunningDynamicControllerWithInstances is newRunningDynamicController with
// a dynamic client seeded with instances, so the parent informer's revision
// index has something to report.
func newRunningDynamicControllerWithInstances(
	t testing.TB,
	objects []runtime.Object,
	gvrs ...schema.GroupVersionResource,
) *dynamiccontroller.DynamicController {
	t.Helper()

	metaScheme := runtime.NewScheme()
	require.NoError(t, metav1.AddMetaToScheme(metaScheme))

	listKinds := make(map[schema.GroupVersionResource]string, len(gvrs))
	groupVersions := make([]schema.GroupVersion, 0, len(gvrs))
	for _, gvr := range gvrs {
		listKinds[gvr] = "NetworkList"
		groupVersions = append(groupVersions, gvr.GroupVersion())
	}

	dynamicClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		listKinds,
		objects...,
	)

	dc := dynamiccontroller.NewDynamicController(
		logr.Discard(),
		testDynamicControllerConfig(),
		metadatafake.NewSimpleMetadataClient(metaScheme),
		dynamicClient,
		apimeta.NewDefaultRESTMapper(groupVersions),
	)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = dc.Start(ctx)
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	t.Cleanup(func() {
		cancel()
		<-done
	})

	return dc
}

func TestRolloutStrategy(t *testing.T) {
	manual := &v1alpha1.Rollout{Strategy: v1alpha1.RolloutStrategyManual}
	latest := &v1alpha1.Rollout{Strategy: v1alpha1.RolloutStrategyLatest}

	tests := []struct {
		name    string
		gate    bool
		rollout *v1alpha1.Rollout
		want    v1alpha1.RolloutStrategy
	}{
		{name: "gate off ignores manual", gate: false, rollout: manual, want: v1alpha1.RolloutStrategyLatest},
		{name: "gate off without rollout", gate: false, want: v1alpha1.RolloutStrategyLatest},
		{name: "gate on without rollout", gate: true, want: v1alpha1.RolloutStrategyLatest},
		{name: "gate on with latest", gate: true, rollout: latest, want: v1alpha1.RolloutStrategyLatest},
		{name: "gate on with manual", gate: true, rollout: manual, want: v1alpha1.RolloutStrategyManual},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, features.FeatureGate, features.GraphRevisionPinning, tt.gate)

			rgd := newTestRGD("rgd-rollout")
			rgd.Spec.Rollout = tt.rollout

			assert.Equal(t, tt.want, rolloutStrategy(rgd))
		})
	}
}

func TestReconcileRejectsRolloutWithGateDisabled(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, features.FeatureGate, features.GraphRevisionPinning, false)

	rgd := newTestRGD("rgd-rollout-gate-off")
	rgd.Spec.Rollout = &v1alpha1.Rollout{Strategy: v1alpha1.RolloutStrategyManual}

	cl := newTestClient(t, interceptor.Funcs{})
	reconciler := &ResourceGraphDefinitionReconciler{
		Client:            cl,
		apiReader:         cl,
		metadataLabeler:   metadata.NewKROMetaLabeler(),
		rgBuilder:         newTestBuilder(),
		crdManager:        &stubCRDManager{},
		revisionsRegistry: revisions.NewRegistry(),
		cfg:               Config{MaxGraphRevisions: 20, ProgressRequeueDelay: 3 * time.Second},
	}

	result, topologicalOrder, resourcesInfo, err := reconciler.reconcileResourceGraphDefinition(context.Background(), rgd)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "GraphRevisionPinning")
	assert.Equal(t, ctrl.Result{}, result)
	assert.Nil(t, topologicalOrder)
	assert.Nil(t, resourcesInfo)
	assertConditionExact(t, rgd, GraphAccepted, metav1.ConditionFalse, "InvalidResourceGraph",
		"spec.rollout requires the GraphRevisionPinning feature gate to be enabled")
}

func newServingReconciler(t testing.TB, manager *stubCRDManager, dc *dynamiccontroller.DynamicController) *ResourceGraphDefinitionReconciler {
	t.Helper()

	cl := newTestClient(t, interceptor.Funcs{})
	return &ResourceGraphDefinitionReconciler{
		Client:            cl,
		apiReader:         cl,
		metadataLabeler:   metadata.NewKROMetaLabeler(),
		rgBuilder:         newTestBuilder(),
		dynamicController: dc,
		crdManager:        manager,
		clientSet:         newKROFakeSet(),
		instanceLogger:    logr.Discard(),
		newEventRecorder:  newFakeEventRecorderFactory(),
		revisionsRegistry: revisions.NewRegistry(),
		cfg:               Config{MaxGraphRevisions: 20, ProgressRequeueDelay: 3 * time.Second},
	}
}

func TestEnsureServingStateRejectDefaultChanges(t *testing.T) {
	tests := []struct {
		name    string
		gate    bool
		rollout *v1alpha1.Rollout
		want    bool
	}{
		{name: "manual strategy rejects default changes", gate: true, rollout: &v1alpha1.Rollout{Strategy: v1alpha1.RolloutStrategyManual}, want: true},
		{name: "latest strategy allows default changes", gate: true, rollout: &v1alpha1.Rollout{Strategy: v1alpha1.RolloutStrategyLatest}, want: false},
		{name: "gate off allows default changes", gate: false, rollout: &v1alpha1.Rollout{Strategy: v1alpha1.RolloutStrategyManual}, want: false},
		{name: "no rollout allows default changes", gate: true, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, features.FeatureGate, features.GraphRevisionPinning, tt.gate)

			rgd := newTestRGD("rgd-serving-defaults")
			rgd.Spec.Rollout = tt.rollout

			manager := &stubCRDManager{}
			dc := newRunningDynamicControllerWithInstances(t, nil, instanceGVRFor("v1alpha1"))
			reconciler := newServingReconciler(t, manager, dc)
			mark := NewConditionsMarkerFor(rgd)

			_, _, err := reconciler.ensureServingState(context.Background(), rgd, newProcessedGraph(), mark)
			require.NoError(t, err)
			assert.Equal(t, tt.want, manager.lastEnsureOptions.RejectDefaultChanges)
		})
	}
}

func TestCRDFailureReason(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "version removal blocked", err: fmt.Errorf("wrapped: %w", kroclient.ErrVersionRemovalBlocked), want: "VersionRemovalBlocked"},
		{name: "spoke schema mismatch", err: fmt.Errorf("wrapped: %w", kroclient.ErrSpokeSchemaMismatch), want: "SpokeSchemaMismatch"},
		{name: "unattributed failure", err: errors.New("boom"), want: "Failed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, crdFailureReason(tt.err))
		})
	}
}

func TestEnsureServingStateSurfacesCRDFailureReason(t *testing.T) {
	tests := []struct {
		name       string
		ensureErr  error
		wantReason string
	}{
		{
			name:       "version removal blocked",
			ensureErr:  fmt.Errorf("%w: version v1alpha1 of CRD networks.example.io is still listed in status.storedVersions", kroclient.ErrVersionRemovalBlocked),
			wantReason: "VersionRemovalBlocked",
		},
		{
			name:       "spoke schema mismatch",
			ensureErr:  fmt.Errorf("%w: cannot update CRD networks.example.io: version v1alpha1 cannot be converted to v1beta1", kroclient.ErrSpokeSchemaMismatch),
			wantReason: "SpokeSchemaMismatch",
		},
		{
			name:       "generic failure",
			ensureErr:  errors.New("api server unavailable"),
			wantReason: "Failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rgd := newTestRGD("rgd-serving-reason")
			manager := &stubCRDManager{ensureErr: tt.ensureErr}
			reconciler := newServingReconciler(t, manager, newRunningDynamicController(t))
			mark := NewConditionsMarkerFor(rgd)

			_, _, err := reconciler.ensureServingState(context.Background(), rgd, newProcessedGraph(), mark)
			require.Error(t, err)
			assertConditionExact(t, rgd, KindReady, metav1.ConditionFalse, tt.wantReason, tt.ensureErr.Error())
		})
	}
}

// After a kro restart no instance controller is registered. A rejected CRD
// update must not keep it that way while the live CRD is still this RGD's own.
func TestEnsureServingStateKeepsServingTheLiveCRDWhenTheUpdateIsRejected(t *testing.T) {
	ensureErr := fmt.Errorf("%w: cannot update CRD networks.example.io: version v1alpha1 cannot be converted to v1beta1", kroclient.ErrSpokeSchemaMismatch)
	gvr := instanceGVRFor("v1alpha1")

	tests := []struct {
		name           string
		mutate         func(live *extv1.CustomResourceDefinition)
		getErr         error
		wantRegistered bool
	}{
		{
			name:           "the live CRD is this RGD's own and serves the hub version",
			wantRegistered: true,
		},
		{
			name:   "the CRD does not exist",
			getErr: apierrors.NewNotFound(schema.GroupResource{Resource: "customresourcedefinitions"}, "networks.example.io"),
		},
		{
			name:   "the CRD is not owned by kro",
			mutate: func(live *extv1.CustomResourceDefinition) { live.Labels = nil },
		},
		{
			name: "the CRD belongs to another RGD",
			mutate: func(live *extv1.CustomResourceDefinition) {
				live.Labels[metadata.ResourceGraphDefinitionNameLabel] = "other-graph"
			},
		},
		{
			name: "the CRD was applied by an earlier RGD of the same name",
			mutate: func(live *extv1.CustomResourceDefinition) {
				live.Labels[metadata.ResourceGraphDefinitionIDLabel] = "earlier-uid"
			},
		},
		{
			name: "the live CRD does not serve the hub version",
			mutate: func(live *extv1.CustomResourceDefinition) {
				live.Spec.Versions[0].Served = false
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rgd := newTestRGD("rgd-keep-serving")
			rgd.UID = "rgd-uid"

			live := newProcessedGraph().CRD
			live.Labels = map[string]string{
				metadata.OwnedLabel:                       "true",
				metadata.ResourceGraphDefinitionNameLabel: rgd.Name,
				metadata.ResourceGraphDefinitionIDLabel:   string(rgd.UID),
			}
			live.Spec.Versions = []extv1.CustomResourceDefinitionVersion{{Name: "v1alpha1", Served: true, Storage: true}}
			if tt.mutate != nil {
				tt.mutate(live)
			}

			manager := &stubCRDManager{ensureErr: ensureErr, getReturn: live, getErr: tt.getErr}
			dc := newRunningDynamicController(t)
			reconciler := newServingReconciler(t, manager, dc)
			mark := NewConditionsMarkerFor(rgd)

			_, _, err := reconciler.ensureServingState(context.Background(), rgd, newProcessedGraph(), mark)
			require.ErrorIs(t, err, kroclient.ErrSpokeSchemaMismatch, "the rejection is still reported")
			assertConditionExact(t, rgd, KindReady, metav1.ConditionFalse, "SpokeSchemaMismatch", ensureErr.Error())

			_, registered := reconciler.registeredGVR(rgd.UID)
			assert.Equal(t, tt.wantRegistered, registered)
			_, watched := dc.InstanceKeys(gvr)
			assert.Equal(t, tt.wantRegistered, watched)
			if tt.wantRegistered {
				assert.True(t, conditionFor(t, rgd, ControllerReady).IsTrue())
			}
		})
	}
}

func TestEnsureServingStateLeavesARegisteredControllerAloneWhenTheUpdateIsRejected(t *testing.T) {
	rgd := newTestRGD("rgd-keep-registered")
	rgd.UID = "rgd-uid"
	manager := &stubCRDManager{}
	reconciler := newServingReconciler(t, manager, newRunningDynamicController(t))

	_, _, err := reconciler.ensureServingState(context.Background(), rgd, newProcessedGraph(), NewConditionsMarkerFor(rgd))
	require.NoError(t, err)
	requested := len(manager.requestedCRDNames)

	manager.ensureErr = errors.New("crd boom")
	_, _, err = reconciler.ensureServingState(context.Background(), rgd, newProcessedGraph(), NewConditionsMarkerFor(rgd))
	require.Error(t, err)
	assert.Len(t, manager.requestedCRDNames, requested, "an RGD that is already served needs no look at the live CRD")
}

func TestGarbageCollectGraphRevisionsRetainsReferenced(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, features.FeatureGate, features.GraphRevisionPinning, true)

	rgd := newTestRGD("rgd-gc-referenced")
	gvr := instanceGVRFor("v1alpha1")

	dc := newRunningDynamicControllerWithInstances(t, []runtime.Object{
		newInstance("from-status", 1, ""),
		newInstance("from-annotation", 0, "1"),
	}, gvr)
	require.NoError(t, dc.Register(context.Background(), gvr, func(context.Context, ctrl.Request) error { return nil }))

	refs, ok := dc.ReferencedGraphRevisions(gvr)
	require.True(t, ok)
	require.Equal(t, []int64{1}, refs.UnsortedList())

	revisionObjects := make([]client.Object, 0, 8)
	for rev := int64(1); rev <= 8; rev++ {
		revisionObjects = append(revisionObjects, newListedGraphRevision(rgd, rev, fmt.Sprintf("hash-%d", rev)))
	}
	cl := newTestClient(t, interceptor.Funcs{}, revisionObjects...)

	reconciler := &ResourceGraphDefinitionReconciler{
		Client:            cl,
		apiReader:         cl,
		dynamicController: dc,
		revisionsRegistry: revisions.NewRegistry(),
		cfg:               Config{MaxGraphRevisions: 5, ProgressRequeueDelay: 3 * time.Second},
	}
	reconciler.registeredControllers.Store(rgd.UID, gvr)

	require.NoError(t, reconciler.garbageCollectGraphRevisions(context.Background(), rgd))

	list := &internalv1alpha1.GraphRevisionList{}
	require.NoError(t, cl.List(context.Background(), list))
	remaining := make([]int64, 0, len(list.Items))
	for i := range list.Items {
		remaining = append(remaining, list.Items[i].Spec.Revision)
	}
	assert.ElementsMatch(t, []int64{1, 4, 5, 6, 7, 8}, remaining,
		"only unreferenced revisions below the retention floor may be deleted")
}

func TestGarbageCollectGraphRevisionsSkippedWhenReferencesUnavailable(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, features.FeatureGate, features.GraphRevisionPinning, true)

	rgd := newTestRGD("rgd-gc-no-refs")
	cl := newTestClient(t, interceptor.Funcs{},
		newListedGraphRevision(rgd, 1, "hash-1"),
		newListedGraphRevision(rgd, 2, "hash-2"),
		newListedGraphRevision(rgd, 3, "hash-3"),
	)

	reconciler := &ResourceGraphDefinitionReconciler{
		Client:            cl,
		apiReader:         cl,
		dynamicController: newRunningDynamicController(t),
		revisionsRegistry: revisions.NewRegistry(),
		cfg:               Config{MaxGraphRevisions: 1, ProgressRequeueDelay: 3 * time.Second},
	}
	// No controller registered for this RGD: the referenced set is unknown.

	require.NoError(t, reconciler.garbageCollectGraphRevisions(context.Background(), rgd))

	list := &internalv1alpha1.GraphRevisionList{}
	require.NoError(t, cl.List(context.Background(), list))
	assert.Len(t, list.Items, 3, "GC must not delete anything while references are unknown")
}

func TestEnsureControllerDeregistersPreviousGVR(t *testing.T) {
	rgd := newTestRGD("rgd-hub-move")
	oldGVR := instanceGVRFor("v1alpha1")
	newGVR := instanceGVRFor("v1beta1")

	dc := newRunningDynamicController(t)
	reconciler := newServingReconciler(t, &stubCRDManager{}, dc)
	reconciler.metadataLabeler = metadata.NewKROMetaLabeler()

	require.NoError(t, reconciler.ensureResourceGraphDefinitionController(
		context.Background(), rgd, newProcessedGraph(), metadata.NewKROMetaLabeler()))

	registered, ok := reconciler.registeredGVR(rgd.UID)
	require.True(t, ok)
	assert.Equal(t, oldGVR, registered)
	_, watching := dc.InstanceKeys(oldGVR)
	assert.True(t, watching, "old GVR must be watched after the first registration")

	// Hub moves to v1beta1: the graph now reports the new instance GVR.
	movedGraph := newProcessedGraph()
	movedGraph.Instance.Meta.GVR = newGVR

	require.NoError(t, reconciler.ensureResourceGraphDefinitionController(
		context.Background(), rgd, movedGraph, metadata.NewKROMetaLabeler()))

	registered, ok = reconciler.registeredGVR(rgd.UID)
	require.True(t, ok)
	assert.Equal(t, newGVR, registered)

	_, watching = dc.InstanceKeys(oldGVR)
	assert.False(t, watching, "previous GVR must stop being watched after a hub move")
	_, watching = dc.InstanceKeys(newGVR)
	assert.True(t, watching, "new GVR must be watched after a hub move")
}

func TestCleanupDeregistersRegisteredGVRNotSpecGVR(t *testing.T) {
	rgd := newTestRGD("rgd-cleanup-registered")
	registeredGVR := instanceGVRFor("v1beta1")
	// The spec has already moved on to a different hub version.
	specGVR := metadata.GetResourceGraphDefinitionInstanceGVR(rgd.Spec.Schema)
	require.NotEqual(t, specGVR, registeredGVR)

	dc := newRunningDynamicController(t)
	require.NoError(t, dc.Register(context.Background(), registeredGVR, func(context.Context, ctrl.Request) error { return nil }))
	require.NoError(t, dc.Register(context.Background(), specGVR, func(context.Context, ctrl.Request) error { return nil }))

	reconciler := &ResourceGraphDefinitionReconciler{
		dynamicController: dc,
		crdManager:        &stubCRDManager{},
		revisionsRegistry: revisions.NewRegistry(),
	}
	reconciler.registeredControllers.Store(rgd.UID, registeredGVR)

	require.NoError(t, reconciler.cleanupResourceGraphDefinition(context.Background(), rgd))

	_, watching := dc.InstanceKeys(registeredGVR)
	assert.False(t, watching, "the GVR that was registered must be deregistered")
	_, watching = dc.InstanceKeys(specGVR)
	assert.True(t, watching, "the spec-derived GVR was never registered for this RGD and must stay untouched")
}

// recreatedRGDReconciler wires a reconciler for an RGD that has no revisions
// and no instance controller yet, while its CRD and instances are still there.
func recreatedRGDReconciler(
	t testing.TB,
	rgd *v1alpha1.ResourceGraphDefinition,
	manager *stubCRDManager,
	instances ...runtime.Object,
) (*ResourceGraphDefinitionReconciler, *dynamicfake.FakeDynamicClient) {
	t.Helper()

	dynamicClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{instanceGVRFor("v1alpha1"): "NetworkList"},
		instances...,
	)
	cl := newTestClient(t, interceptor.Funcs{})
	return &ResourceGraphDefinitionReconciler{
		Client:            cl,
		apiReader:         cl,
		metadataLabeler:   metadata.NewKROMetaLabeler(),
		rgBuilder:         newTestBuilder(),
		crdManager:        manager,
		clientSet:         krofake.NewFakeSet(dynamicClient),
		revisionsRegistry: revisions.NewRegistry(),
		cfg:               Config{MaxGraphRevisions: 20, ProgressRequeueDelay: 3 * time.Second},
	}, dynamicClient
}

// survivingCRD is the instance CRD a deleted RGD of the given name left behind.
func survivingCRD(rgdName string) *extv1.CustomResourceDefinition {
	crd := migrationTestCRD()
	crd.Labels[metadata.ResourceGraphDefinitionNameLabel] = rgdName
	crd.Spec.Versions = []extv1.CustomResourceDefinitionVersion{{Name: "v1alpha1", Served: true, Storage: true}}
	return crd
}

// A recreated RGD starts without revisions and without status. Its instances
// still hold revision numbers, and none of those may name a new graph.
func TestReconcileIssuesAboveTheRevisionsSurvivingInstancesHold(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, features.FeatureGate, features.GraphRevisionPinning, true)

	rgd := newTestRGD("rgd-recreated")
	reconciler, _ := recreatedRGDReconciler(t, rgd, &stubCRDManager{getReturn: survivingCRD(rgd.Name)},
		newInstance("one", 1, ""),
		newInstance("four", 4, ""),
	)

	_, _, _, err := reconciler.reconcileResourceGraphDefinition(context.Background(), rgd)
	require.NoError(t, err)

	revisionList := &internalv1alpha1.GraphRevisionList{}
	require.NoError(t, reconciler.Client.List(context.Background(), revisionList))
	require.Len(t, revisionList.Items, 1)
	assert.Equal(t, int64(5), revisionList.Items[0].Spec.Revision)
	assert.Equal(t, int64(5), rgd.Status.LastIssuedRevision)
}

func TestRevisionFloor(t *testing.T) {
	const rgdName = "rgd-recreated"
	foreign := func(mutate func(*extv1.CustomResourceDefinition)) *extv1.CustomResourceDefinition {
		crd := survivingCRD(rgdName)
		mutate(crd)
		return crd
	}
	withTarget := newInstance("pinned", 2, "9")
	withTarget.Object["status"].(map[string]any)["targetGraphRevision"] = int64(9)

	tests := []struct {
		name       string
		gateOff    bool
		registered bool
		manager    *stubCRDManager
		instances  []runtime.Object
		listErr    error
		want       int64
		wantErr    string
		wantNoGet  bool
	}{
		{
			name:      "highest revision an instance ran",
			manager:   &stubCRDManager{getReturn: survivingCRD(rgdName)},
			instances: []runtime.Object{newInstance("one", 1, ""), newInstance("four", 4, ""), newInstance("fresh", 0, "")},
			want:      4,
		},
		{
			name:      "a pin and its target are not a revision the instance ran",
			manager:   &stubCRDManager{getReturn: survivingCRD(rgdName)},
			instances: []runtime.Object{withTarget},
			want:      2,
		},
		{
			name:    "no instances",
			manager: &stubCRDManager{getReturn: survivingCRD(rgdName)},
		},
		{
			name:    "no CRD yet",
			manager: &stubCRDManager{getErr: apierrors.NewNotFound(schema.GroupResource{Resource: "customresourcedefinitions"}, "networks.kro.run")},
		},
		{
			name: "CRD of another RGD",
			manager: &stubCRDManager{getReturn: foreign(func(crd *extv1.CustomResourceDefinition) {
				crd.Labels[metadata.ResourceGraphDefinitionNameLabel] = "someone-else"
			})},
			instances: []runtime.Object{newInstance("four", 4, "")},
		},
		{
			name: "CRD kro does not own",
			manager: &stubCRDManager{getReturn: foreign(func(crd *extv1.CustomResourceDefinition) {
				delete(crd.Labels, metadata.OwnedLabel)
			})},
			instances: []runtime.Object{newInstance("four", 4, "")},
		},
		{
			name:       "an RGD with a registered controller issued its instances' numbers itself",
			registered: true,
			manager:    &stubCRDManager{getReturn: survivingCRD(rgdName)},
			instances:  []runtime.Object{newInstance("four", 4, "")},
			wantNoGet:  true,
		},
		{
			name:      "gate off",
			gateOff:   true,
			manager:   &stubCRDManager{getReturn: survivingCRD(rgdName)},
			instances: []runtime.Object{newInstance("four", 4, "")},
			wantNoGet: true,
		},
		{
			name:    "a failed CRD read is not guessed around",
			manager: &stubCRDManager{getErr: errors.New("api server unavailable")},
			wantErr: "api server unavailable",
		},
		{
			name:    "a failed instance list is not guessed around",
			manager: &stubCRDManager{getReturn: survivingCRD(rgdName)},
			listErr: errors.New("list refused"),
			wantErr: "list refused",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, features.FeatureGate, features.GraphRevisionPinning, !tt.gateOff)

			rgd := newTestRGD(rgdName)
			rgd.UID = "new-uid"
			reconciler, dynamicClient := recreatedRGDReconciler(t, rgd, tt.manager, tt.instances...)
			if tt.listErr != nil {
				dynamicClient.PrependReactor("list", "networks", func(clienttesting.Action) (bool, runtime.Object, error) {
					return true, nil, tt.listErr
				})
			}
			if tt.registered {
				reconciler.registeredControllers.Store(rgd.UID, instanceGVRFor("v1alpha1"))
			}

			got, err := reconciler.revisionFloor(context.Background(), rgd)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
			if tt.wantNoGet {
				assert.Empty(t, tt.manager.requestedCRDNames)
			}
		})
	}
}

func TestReconcileDoesNotIssueWhenTheHeldRevisionsAreUnknown(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, features.FeatureGate, features.GraphRevisionPinning, true)

	rgd := newTestRGD("rgd-recreated")
	reconciler, _ := recreatedRGDReconciler(t, rgd, &stubCRDManager{getErr: errors.New("api server unavailable")})

	_, _, _, err := reconciler.reconcileResourceGraphDefinition(context.Background(), rgd)
	require.ErrorContains(t, err, "api server unavailable")

	revisionList := &internalv1alpha1.GraphRevisionList{}
	require.NoError(t, reconciler.Client.List(context.Background(), revisionList))
	assert.Empty(t, revisionList.Items)
}
