// Copyright 2026 The Kube Resource Orchestrator Authors
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

package instance

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apimachineryruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	apiwatch "k8s.io/apimachinery/pkg/watch"
	metadatafake "k8s.io/client-go/metadata/fake"
	clienttesting "k8s.io/client-go/testing"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/dynamiccontroller"
	"github.com/kubernetes-sigs/kro/pkg/graph/revisions"
	"github.com/kubernetes-sigs/kro/pkg/watch"
)

// forbiddenCoordinator returns a coordinator whose metadata client denies LIST on configmaps until allow is set.
func forbiddenCoordinator(t *testing.T, allow *atomic.Bool) (*dynamiccontroller.WatchCoordinator, *watch.Manager) {
	t.Helper()
	scheme := apimachineryruntime.NewScheme()
	require.NoError(t, metav1.AddMetaToScheme(scheme))
	mc := metadatafake.NewSimpleMetadataClient(scheme)
	mc.PrependReactor("list", "configmaps", func(_ clienttesting.Action) (bool, apimachineryruntime.Object, error) {
		if allow.Load() {
			return false, nil, nil
		}
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, "", fmt.Errorf("denied"))
	})
	log := zap.New(zap.UseDevMode(true))
	var coord *dynamiccontroller.WatchCoordinator
	wm := watch.NewManager(mc, time.Hour, func(e watch.Event) {
		if coord != nil {
			coord.RouteEvent(e)
		}
	}, log)
	coord = dynamiccontroller.NewWatchCoordinator(wm, func(schema.GroupVersionResource, types.NamespacedName) {}, log)
	t.Cleanup(wm.Shutdown)
	return coord, wm
}

// TestReconcile_WatchesHealthyCondition_ForbiddenThenGranted covers Forbidden -> WatchesHealthy=False with Ready=True -> True after access is granted.
func TestReconcile_WatchesHealthyCondition_ForbiddenThenGranted(t *testing.T) {
	var allow atomic.Bool
	coord, wm := forbiddenCoordinator(t, &allow)
	comp := newTestRealCompiler(t)
	inst := newInstanceObject("demo", "default")
	raw := newControllerTestDynamicClient(t, inst.DeepCopy())
	spec := testRGDSpecWithConfigMap("app-config", "")
	c, _ := newGraphEngineControllerUnderTest(t, raw, spec, revisions.RevisionStateActive, comp, newFakeRuntimeClient(t))
	c.coordinator = coord
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "demo", Namespace: "default"}}
	cmGVR := schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}

	require.NoError(t, c.Reconcile(context.Background(), req))
	require.Eventually(t, func() bool { return wm.WatchState(cmGVR).BlockedErr != nil }, 2*time.Second, 10*time.Millisecond)
	require.NoError(t, c.Reconcile(context.Background(), req))

	stored := getStoredParentObject(t, raw)
	wh := conditionByType(t, stored, WatchesHealthy)
	assert.Equal(t, metav1.ConditionFalse, wh.Status)
	require.NotNil(t, wh.Reason)
	assert.Equal(t, "WatchBlocked", *wh.Reason)
	require.NotNil(t, wh.Message)
	assert.Contains(t, *wh.Message, "configmaps")
	assert.Contains(t, *wh.Message, "Forbidden")
	assert.Equal(t, metav1.ConditionTrue, conditionByType(t, stored, Ready).Status,
		"a degraded watch must not fail readiness")
	assert.Equal(t, 1, wm.ActiveWatchCount(), "informer retained despite blocking error")

	allow.Store(true)
	require.Eventually(t, func() bool {
		st := wm.WatchState(cmGVR)
		return st.Synced && st.BlockedErr == nil
	}, 20*time.Second, 50*time.Millisecond)
	require.NoError(t, c.Reconcile(context.Background(), req))
	stored = getStoredParentObject(t, raw)
	assert.Equal(t, metav1.ConditionTrue, conditionByType(t, stored, WatchesHealthy).Status)
}

// TestBuiltinConditions_IncludesWatchesHealthy pins that the status allowlist carries the new type to the wire (and therefore to author CEL).
func TestBuiltinConditions_IncludesWatchesHealthy(t *testing.T) {
	inst := newInstanceObject("demo", "default")
	mark := NewConditionsMarkerFor(inst)
	mark.InstanceManaged()
	mark.GraphResolved()
	mark.ResourcesReady()
	mark.WatchesBlocked("x")
	types := map[string]metav1.ConditionStatus{}
	for _, c := range builtinConditions(inst) {
		types[string(c.Type)] = c.Status
	}
	assert.Equal(t, metav1.ConditionFalse, types[WatchesHealthy])
	assert.Equal(t, metav1.ConditionTrue, types[Ready], "WatchesHealthy is not a Ready dependent")
	assert.Contains(t, v1alpha1.KROBuiltinConditionTypes, WatchesHealthy)
}

// TestReconcile_WatchesHealthyCondition_PendingThenSynced: while the ConfigMap
// informer's initial list is still in flight the condition is Unknown/WatchesPending
// and Ready is unaffected; once the list completes it flips to True.
func TestReconcile_WatchesHealthyCondition_PendingThenSynced(t *testing.T) {
	release := make(chan struct{})
	scheme := apimachineryruntime.NewScheme()
	require.NoError(t, metav1.AddMetaToScheme(scheme))
	mc := metadatafake.NewSimpleMetadataClient(scheme)
	mc.PrependReactor("list", "configmaps", func(_ clienttesting.Action) (bool, apimachineryruntime.Object, error) {
		<-release
		return false, nil, nil
	})
	mc.PrependWatchReactor("configmaps", func(_ clienttesting.Action) (bool, apiwatch.Interface, error) {
		select {
		case <-release:
			return false, nil, nil
		default:
			return true, nil, fmt.Errorf("not yet") // transient: keeps the watch-list path from syncing early
		}
	})
	log := zap.New(zap.UseDevMode(true))
	var coord *dynamiccontroller.WatchCoordinator
	wm := watch.NewManager(mc, time.Hour, func(e watch.Event) {
		if coord != nil {
			coord.RouteEvent(e)
		}
	}, log)
	coord = dynamiccontroller.NewWatchCoordinator(wm, func(schema.GroupVersionResource, types.NamespacedName) {}, log)
	t.Cleanup(wm.Shutdown)

	comp := newTestRealCompiler(t)
	inst := newInstanceObject("demo", "default")
	raw := newControllerTestDynamicClient(t, inst.DeepCopy())
	c, _ := newGraphEngineControllerUnderTest(t, raw, testRGDSpecWithConfigMap("app-config", ""), revisions.RevisionStateActive, comp, newFakeRuntimeClient(t))
	c.coordinator = coord
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "demo", Namespace: "default"}}
	cmGVR := schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}

	require.NoError(t, c.Reconcile(context.Background(), req))
	stored := getStoredParentObject(t, raw)
	wh := conditionByType(t, stored, WatchesHealthy)
	assert.Equal(t, metav1.ConditionUnknown, wh.Status)
	require.NotNil(t, wh.Reason)
	assert.Equal(t, "WatchesPending", *wh.Reason)
	require.NotNil(t, wh.Message)
	assert.Contains(t, *wh.Message, "configmaps")
	assert.Equal(t, metav1.ConditionTrue, conditionByType(t, stored, Ready).Status, "pending watch must not hold Ready")

	close(release)
	require.Eventually(t, func() bool { return wm.WatchState(cmGVR).Synced }, 10*time.Second, 20*time.Millisecond)
	require.NoError(t, c.Reconcile(context.Background(), req))
	stored = getStoredParentObject(t, raw)
	assert.Equal(t, metav1.ConditionTrue, conditionByType(t, stored, WatchesHealthy).Status)
}
