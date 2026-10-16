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

package graph

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	metadatafake "k8s.io/client-go/metadata/fake"
	clienttesting "k8s.io/client-go/testing"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	expv1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/compiler"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/executor"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/registry"
	krotruntime "github.com/kubernetes-sigs/kro/pkg/graphengine/runtime"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/watchrouter"
)

// declaringExecutor declares one watch per Apply and succeeds.
type declaringExecutor struct {
	fakeExecutor
	req watchrouter.WatchRequest
}

func (e *declaringExecutor) Apply(_ context.Context, _ *krotruntime.Runtime, w watchrouter.Watcher) (executor.ApplyResult, error) {
	if err := w.Watch(e.req); err != nil {
		return executor.ApplyResult{}, err
	}
	return e.applyResult, e.applyErr
}

func findCond(g *expv1alpha1.Graph, t string) *expv1alpha1.Condition {
	for i := range g.Status.Conditions {
		if string(g.Status.Conditions[i].Type) == t {
			return &g.Status.Conditions[i]
		}
	}
	return nil
}

// TestWatchesHealthyCondition_ForbiddenThenGranted drives a Graph whose identity cannot list ConfigMaps.
func TestWatchesHealthyCondition_ForbiddenThenGranted(t *testing.T) {
	var allow atomic.Bool
	scheme := metadatafake.NewTestScheme()
	metaClient := metadatafake.NewSimpleMetadataClient(scheme)
	metaClient.PrependReactor("list", "configmaps", func(_ clienttesting.Action) (bool, runtime.Object, error) {
		if allow.Load() {
			return false, nil, nil
		}
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, "", fmt.Errorf("denied"))
	})
	router := watchrouter.NewRouter(logr.Discard(), watchrouter.Config{}, metaClient)
	t.Cleanup(router.Manager().Shutdown)

	g := graph("g", withFinalizer)
	cl := newClient(t, g)
	cmGVR := schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
	r := &Reconciler{
		Client:   cl,
		Compiler: &fakeCompiler{program: &compiler.Program{Nodes: map[string]*compiler.Node{"cm": {}}}},
		Registry: registry.New(),
		Executor: &declaringExecutor{req: watchrouter.WatchRequest{NodeID: "cm", GVR: cmGVR, Name: "c", Namespace: "default"}},
		Router:   router,
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "g"}}

	res, err := r.Reconcile(context.Background(), req)
	require.NoError(t, err)
	assert.Zero(t, res.RequeueAfter)

	require.Eventually(t, func() bool {
		return router.Manager().WatchState(cmGVR).BlockedErr != nil
	}, 2*time.Second, 10*time.Millisecond)
	_, err = r.Reconcile(context.Background(), req)
	require.NoError(t, err)

	got := &expv1alpha1.Graph{}
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(g), got))
	wh := findCond(got, WatchesHealthy)
	require.NotNil(t, wh, "WatchesHealthy must be written")
	assert.Equal(t, metav1.ConditionFalse, wh.Status)
	require.NotNil(t, wh.Reason)
	assert.Equal(t, "WatchBlocked", *wh.Reason)
	require.NotNil(t, wh.Message)
	assert.Contains(t, *wh.Message, "configmaps")
	assert.Contains(t, *wh.Message, "Forbidden")
	ready := findCond(got, Ready)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionTrue, ready.Status, "a degraded watch must not fail readiness")

	allow.Store(true)
	require.Eventually(t, func() bool {
		st := router.Manager().WatchState(cmGVR)
		return st.Synced && st.BlockedErr == nil
	}, 20*time.Second, 50*time.Millisecond)
	_, err = r.Reconcile(context.Background(), req)
	require.NoError(t, err)
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(g), got))
	wh = findCond(got, WatchesHealthy)
	require.NotNil(t, wh)
	assert.Equal(t, metav1.ConditionTrue, wh.Status, "condition clears once the watch syncs")
}

// TestWatchesHealthyCondition_HealthyIsTrueAndStable pins that an unchanged healthy condition is not rewritten.
func TestWatchesHealthyCondition_HealthyIsTrueAndStable(t *testing.T) {
	router := watchrouter.NewRouter(logr.Discard(), watchrouter.Config{}, metadatafake.NewSimpleMetadataClient(metadatafake.NewTestScheme()))
	t.Cleanup(router.Manager().Shutdown)
	g := graph("g", withFinalizer)
	cl := newClient(t, g)
	cmGVR := schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
	r := &Reconciler{
		Client:   cl,
		Compiler: &fakeCompiler{program: &compiler.Program{Nodes: map[string]*compiler.Node{"cm": {}}}},
		Registry: registry.New(),
		Executor: &declaringExecutor{req: watchrouter.WatchRequest{NodeID: "cm", GVR: cmGVR, Name: "c", Namespace: "default"}},
		Router:   router,
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "g"}}

	_, err := r.Reconcile(context.Background(), req)
	require.NoError(t, err)
	require.NoError(t, router.Manager().WaitForSync(context.Background(), cmGVR))
	_, err = r.Reconcile(context.Background(), req)
	require.NoError(t, err)

	got := &expv1alpha1.Graph{}
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(g), got))
	wh := findCond(got, WatchesHealthy)
	require.NotNil(t, wh)
	assert.Equal(t, metav1.ConditionTrue, wh.Status)
	first := wh.LastTransitionTime

	_, err = r.Reconcile(context.Background(), req)
	require.NoError(t, err)
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(g), got))
	wh = findCond(got, WatchesHealthy)
	require.NotNil(t, wh)
	assert.Equal(t, first, wh.LastTransitionTime, "identical state must not rewrite the condition")
}
