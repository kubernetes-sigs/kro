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
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	metadatafake "k8s.io/client-go/metadata/fake"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	featuregatetesting "k8s.io/component-base/featuregate/testing"

	internalv1alpha1 "github.com/kubernetes-sigs/kro/api/internal.kro.run/v1alpha1"
	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	kroclient "github.com/kubernetes-sigs/kro/pkg/client"
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
			ensureErr:  fmt.Errorf("%w: version v1alpha1 of CRD networks.example.io differs incompatibly", kroclient.ErrSpokeSchemaMismatch),
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
