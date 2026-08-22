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

package instance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	apimachineryruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	memory "k8s.io/client-go/discovery/cached/memory"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/restmapper"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	clientfake "github.com/kubernetes-sigs/kro/pkg/client/fake"
	controllergraph "github.com/kubernetes-sigs/kro/pkg/controller/graph"
	"github.com/kubernetes-sigs/kro/pkg/controller/instance/applyset"
	"github.com/kubernetes-sigs/kro/pkg/dynamiccontroller"
	"github.com/kubernetes-sigs/kro/pkg/graph/revisions"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/compiler"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/executor"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/rgdadapter"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/watchrouter"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
	"github.com/kubernetes-sigs/kro/pkg/requeue"
	testk8s "github.com/kubernetes-sigs/kro/pkg/testutil/k8s"
)

// testStubCompiler is a test double for rgdadapter.Compiler.
type testStubCompiler struct {
	prog     *compiler.Program
	err      error
	gotGraph *v1alpha1.Graph
	gotOpts  []compiler.CompileOption
	calls    int
}

func (s *testStubCompiler) CompileWithOptions(g *v1alpha1.Graph, opts ...compiler.CompileOption) (*compiler.Program, error) {
	s.calls++
	s.gotGraph = g
	s.gotOpts = opts
	if s.err != nil {
		return nil, s.err
	}
	if s.prog != nil {
		return s.prog, nil
	}
	return &compiler.Program{
		Nodes:            map[string]*compiler.Node{},
		TopologicalOrder: []string{},
	}, nil
}

func newTestRealCompiler(t *testing.T) *compiler.Compiler {
	t.Helper()
	r, disco := testk8s.NewFakeResolver()
	rm := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(disco))
	return compiler.NewCompilerWithDependencies(r, rm)
}

func newFakeRuntimeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	s := apimachineryruntime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(s))
	require.NoError(t, v1alpha1.AddToScheme(s))
	require.NoError(t, appsv1.AddToScheme(s))
	return fakeclient.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
}

func testEmptyRGDSpec() *v1alpha1.ResourceGraphDefinitionSpec {
	return &v1alpha1.ResourceGraphDefinitionSpec{
		Schema: &v1alpha1.Schema{
			Kind:       "WebApp",
			Group:      "kro.run",
			APIVersion: "v1alpha1",
		},
	}
}

func testRGDSpecWithConfigMap(name string, readyWhenExpr string) *v1alpha1.ResourceGraphDefinitionSpec {
	res := &v1alpha1.Resource{
		ID: "cm",
		Template: apimachineryruntime.RawExtension{
			Raw: []byte(fmt.Sprintf(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":%q,"namespace":"default"},"data":{"key":"val"}}`, name)),
		},
	}
	if readyWhenExpr != "" {
		res.ReadyWhen = []string{readyWhenExpr}
	}
	return &v1alpha1.ResourceGraphDefinitionSpec{
		Schema: &v1alpha1.Schema{
			Kind:       "WebApp",
			Group:      "kro.run",
			APIVersion: "v1alpha1",
		},
		Resources: []*v1alpha1.Resource{res},
	}
}

func testRGDSpecWithAuthorConditions(condExpr string) *v1alpha1.ResourceGraphDefinitionSpec {
	statusBytes, _ := json.Marshal(map[string]any{
		"conditions": []any{condExpr},
	})
	return &v1alpha1.ResourceGraphDefinitionSpec{
		Schema: &v1alpha1.Schema{
			Kind:       "WebApp",
			Group:      "kro.run",
			APIVersion: "v1alpha1",
			Status:     apimachineryruntime.RawExtension{Raw: statusBytes},
		},
	}
}

func newGraphEngineControllerUnderTest(
	t *testing.T,
	raw *dynamicfake.FakeDynamicClient,
	rgdSpec *v1alpha1.ResourceGraphDefinitionSpec,
	revState revisions.RevisionState,
	comp rgdadapter.Compiler,
	geClient client.Client,
) (*Controller, *clientfake.FakeSet) {
	t.Helper()

	clientSet := clientfake.NewFakeSet(raw)
	clientSet.SetRESTMapper(buildControllerTestRESTMapper())
	registry := revisions.NewRegistry()
	if revState != "" {
		registry.Put(revisions.Entry{
			OwnerKey: controllerTestParentGVR.Resource,
			Revision: 1,
			State:    revState,
			RGDSpec:  rgdSpec,
		})
	}

	controller := NewController(
		zap.New(zap.UseDevMode(true)),
		ReconcileConfig{
			DefaultRequeueDuration: 2 * time.Second,
		},
		controllerTestParentGVR,
		registry.ResolverForRGD(controllerTestParentGVR.Resource),
		true,
		clientSet,
		metadata.NewKROMetaLabeler(),
		metadata.NewKROMetaLabeler(),
		newControllerTestCoordinator(t),
		record.NewFakeRecorder(100),
		geClient,
	)

	if comp != nil {
		controller.WithGraphEngineCompiler(comp)
	}

	return controller, clientSet
}

type fakeInstanceWatcher struct {
	watchedRequests []dynamiccontroller.WatchRequest
	watchErr        error
	doneCalls       []bool
}

func (f *fakeInstanceWatcher) Watch(req dynamiccontroller.WatchRequest) error {
	f.watchedRequests = append(f.watchedRequests, req)
	return f.watchErr
}

func (f *fakeInstanceWatcher) Done(commit bool) {
	f.doneCalls = append(f.doneCalls, commit)
}

type errorClient struct {
	client.Client
	patchErr error
	getErr   error
}

func (e *errorClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	if e.patchErr != nil {
		return e.patchErr
	}
	return e.Client.Patch(ctx, obj, patch, opts...)
}

func (e *errorClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if e.getErr != nil {
		return e.getErr
	}
	return e.Client.Get(ctx, key, obj, opts...)
}

// -----------------------------------------------------------------------------
// 1. orphanApplyOrder Tests
// -----------------------------------------------------------------------------

func TestOrphanApplyOrder(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	tests := []struct {
		name        string
		annotations map[string]string
		want        int
	}{
		{
			name:        "valid positive order",
			annotations: map[string]string{metadata.ApplyOrderAnnotation: "3"},
			want:        3,
		},
		{
			name:        "valid zero order",
			annotations: map[string]string{metadata.ApplyOrderAnnotation: "0"},
			want:        0,
		},
		{
			name:        "valid negative order",
			annotations: map[string]string{metadata.ApplyOrderAnnotation: "-2"},
			want:        -2,
		},
		{
			name:        "missing annotation",
			annotations: map[string]string{"other": "val"},
			want:        maxInt,
		},
		{
			name:        "nil annotations",
			annotations: nil,
			want:        maxInt,
		},
		{
			name:        "invalid non-integer string",
			annotations: map[string]string{metadata.ApplyOrderAnnotation: "abc"},
			want:        maxInt,
		},
		{
			name:        "empty string",
			annotations: map[string]string{metadata.ApplyOrderAnnotation: ""},
			want:        maxInt,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obj := &unstructured.Unstructured{Object: map[string]interface{}{}}
			if tt.annotations != nil {
				obj.SetAnnotations(tt.annotations)
			}
			candidate := applyset.OrphanCandidate{Object: obj}
			assert.Equal(t, tt.want, orphanApplyOrder(candidate))
		})
	}
}

// -----------------------------------------------------------------------------
// 2. delayedRequeue Tests
// -----------------------------------------------------------------------------

func TestDelayedRequeue(t *testing.T) {
	testErr := errors.New("transient error")

	t.Run("Controller with DefaultRequeueDuration == 0", func(t *testing.T) {
		c := &Controller{reconcileConfig: ReconcileConfig{DefaultRequeueDuration: 0}}
		got := c.delayedRequeue(testErr)
		require.Error(t, got)
		assert.False(t, requeue.IsRequeueError(got), "NoRequeue is not a requeue signal")
		var noReq *requeue.NoRequeue
		assert.True(t, errors.As(got, &noReq))
	})

	t.Run("Controller with DefaultRequeueDuration > 0", func(t *testing.T) {
		dur := 5 * time.Second
		c := &Controller{reconcileConfig: ReconcileConfig{DefaultRequeueDuration: dur}}
		got := c.delayedRequeue(testErr)
		require.Error(t, got)
		assert.True(t, requeue.IsRequeueError(got))
		var reqAfter *requeue.RequeueNeededAfter
		require.True(t, errors.As(got, &reqAfter))
		assert.Equal(t, dur, reqAfter.Duration())
	})

	t.Run("DeletionContext with DefaultRequeueDuration == 0", func(t *testing.T) {
		dcx := &DeletionContext{Config: ReconcileConfig{DefaultRequeueDuration: 0}}
		got := dcx.delayedRequeue(testErr)
		require.Error(t, got)
		var noReq *requeue.NoRequeue
		assert.True(t, errors.As(got, &noReq))
	})

	t.Run("DeletionContext with DefaultRequeueDuration > 0", func(t *testing.T) {
		dur := 3 * time.Second
		dcx := &DeletionContext{Config: ReconcileConfig{DefaultRequeueDuration: dur}}
		got := dcx.delayedRequeue(testErr)
		require.Error(t, got)
		var reqAfter *requeue.RequeueNeededAfter
		require.True(t, errors.As(got, &reqAfter))
		assert.Equal(t, dur, reqAfter.Duration())
	})
}

// -----------------------------------------------------------------------------
// 3. instanceWatcherBridge Tests
// -----------------------------------------------------------------------------

func TestInstanceWatcherBridge(t *testing.T) {
	t.Run("Watch forwards request faithfully", func(t *testing.T) {
		fw := &fakeInstanceWatcher{}
		bridge := &instanceWatcherBridge{w: fw}

		gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
		sel := labels.SelectorFromSet(labels.Set{"app": "web"})
		req := watchrouter.WatchRequest{
			NodeID:    "node-1",
			GVR:       gvr,
			Name:      "my-deploy",
			Namespace: "default",
			Selector:  sel,
		}

		err := bridge.Watch(req)
		require.NoError(t, err)
		require.Len(t, fw.watchedRequests, 1)
		assert.Equal(t, "node-1", fw.watchedRequests[0].NodeID)
		assert.Equal(t, gvr, fw.watchedRequests[0].GVR)
		assert.Equal(t, "my-deploy", fw.watchedRequests[0].Name)
		assert.Equal(t, "default", fw.watchedRequests[0].Namespace)
		assert.Equal(t, sel, fw.watchedRequests[0].Selector)
	})

	t.Run("Watch propagates error", func(t *testing.T) {
		expectedErr := errors.New("watch registry failed")
		fw := &fakeInstanceWatcher{watchErr: expectedErr}
		bridge := &instanceWatcherBridge{w: fw}

		err := bridge.Watch(watchrouter.WatchRequest{NodeID: "node-2"})
		assert.ErrorIs(t, err, expectedErr)
	})

	t.Run("Done forwards commit boolean", func(t *testing.T) {
		fw := &fakeInstanceWatcher{}
		bridge := &instanceWatcherBridge{w: fw}

		bridge.Done(true)
		bridge.Done(false)

		assert.Equal(t, []bool{true, false}, fw.doneCalls)
	})
}

// -----------------------------------------------------------------------------
// 4. isResourceDeleting Tests
// -----------------------------------------------------------------------------

func TestIsResourceDeleting(t *testing.T) {
	delErr := &executor.ResourceDeletingError{NodeID: "cm", Namespace: "default", Name: "my-cm"}
	assert.True(t, isResourceDeleting(delErr))
	assert.True(t, isResourceDeleting(fmt.Errorf("wrapped: %w", delErr)))
	assert.True(t, isResourceDeleting(executor.ErrResourceDeleting))
	assert.False(t, isResourceDeleting(errors.New("other error")))
	assert.False(t, isResourceDeleting(executor.ErrNotReady))
	assert.False(t, isResourceDeleting(nil))
}

// -----------------------------------------------------------------------------
// 5. persistGraphEngineStatus Tests
// -----------------------------------------------------------------------------

func TestPersistGraphEngineStatus(t *testing.T) {
	comp := newTestRealCompiler(t)

	t.Run("Built-in conditions with root ready -> state Active", func(t *testing.T) {
		inst := newInstanceObject("demo", "default")
		raw := newControllerTestDynamicClient(t, inst.DeepCopy())
		c, _ := newGraphEngineControllerUnderTest(t, raw, testEmptyRGDSpec(), revisions.RevisionStateActive, comp, nil)

		mark := NewConditionsMarkerFor(inst)
		mark.InstanceManaged()
		mark.GraphResolved()
		mark.ResourcesReady()

		rgd := &v1alpha1.ResourceGraphDefinition{
			ObjectMeta: metav1.ObjectMeta{Name: "webapps"},
			Spec:       *testEmptyRGDSpec(),
		}
		rt, _, err := rgdadapter.BuildRuntimeForInstance(rgd, inst, comp)
		require.NoError(t, err)

		err = c.persistGraphEngineStatus(context.Background(), inst, nil, rt, rgd, false)
		require.NoError(t, err)

		stored := getStoredParentObject(t, raw)
		status, _, _ := unstructured.NestedMap(stored.Object, "status")
		require.NotNil(t, status)
		assert.Equal(t, string(v1alpha1.InstanceStateActive), status["state"])
		conditions := conditionsFromInstance(stored)
		assert.Len(t, conditions, 4) // Ready, InstanceManaged, GraphResolved, ResourcesReady
	})

	t.Run("Built-in conditions with root not ready -> state InProgress", func(t *testing.T) {
		inst := newInstanceObject("demo", "default")
		raw := newControllerTestDynamicClient(t, inst.DeepCopy())
		c, _ := newGraphEngineControllerUnderTest(t, raw, testEmptyRGDSpec(), revisions.RevisionStateActive, comp, nil)

		mark := NewConditionsMarkerFor(inst)
		mark.InstanceManaged()
		mark.GraphResolved()
		mark.ResourcesNotReady("waiting")

		rgd := &v1alpha1.ResourceGraphDefinition{
			ObjectMeta: metav1.ObjectMeta{Name: "webapps"},
			Spec:       *testEmptyRGDSpec(),
		}
		rt, _, err := rgdadapter.BuildRuntimeForInstance(rgd, inst, comp)
		require.NoError(t, err)

		err = c.persistGraphEngineStatus(context.Background(), inst, nil, rt, rgd, false)
		require.NoError(t, err)

		stored := getStoredParentObject(t, raw)
		status, _, _ := unstructured.NestedMap(stored.Object, "status")
		require.NotNil(t, status)
		assert.Equal(t, string(v1alpha1.InstanceStateInProgress), status["state"])
	})

	t.Run("Built-in conditions with degraded true -> state Error", func(t *testing.T) {
		inst := newInstanceObject("demo", "default")
		raw := newControllerTestDynamicClient(t, inst.DeepCopy())
		c, _ := newGraphEngineControllerUnderTest(t, raw, testEmptyRGDSpec(), revisions.RevisionStateActive, comp, nil)

		mark := NewConditionsMarkerFor(inst)
		mark.InstanceManaged()
		mark.GraphResolved()
		mark.ResourcesReady() // root is ready, but degraded=true overrides it

		rgd := &v1alpha1.ResourceGraphDefinition{
			ObjectMeta: metav1.ObjectMeta{Name: "webapps"},
			Spec:       *testEmptyRGDSpec(),
		}
		rt, _, err := rgdadapter.BuildRuntimeForInstance(rgd, inst, comp)
		require.NoError(t, err)

		err = c.persistGraphEngineStatus(context.Background(), inst, nil, rt, rgd, true)
		require.NoError(t, err)

		stored := getStoredParentObject(t, raw)
		status, _, _ := unstructured.NestedMap(stored.Object, "status")
		require.NotNil(t, status)
		assert.Equal(t, string(v1alpha1.InstanceStateError), status["state"])
	})

	t.Run("Skip write when wire matches computed status", func(t *testing.T) {
		inst := newInstanceObject("demo", "default")
		mark := NewConditionsMarkerFor(inst)
		mark.InstanceManaged()
		mark.GraphResolved()
		mark.ResourcesReady()

		wireStatus := map[string]interface{}{
			"conditions": conditionsToInterfaceSlice(builtinConditions(inst)),
			"state":      string(v1alpha1.InstanceStateActive),
		}
		inst.Object["status"] = wireStatus

		raw := newControllerTestDynamicClient(t, inst.DeepCopy())
		c, _ := newGraphEngineControllerUnderTest(t, raw, testEmptyRGDSpec(), revisions.RevisionStateActive, comp, nil)

		rgd := &v1alpha1.ResourceGraphDefinition{
			ObjectMeta: metav1.ObjectMeta{Name: "webapps"},
			Spec:       *testEmptyRGDSpec(),
		}
		rt, _, err := rgdadapter.BuildRuntimeForInstance(rgd, inst, comp)
		require.NoError(t, err)

		raw.ClearActions()
		err = c.persistGraphEngineStatus(context.Background(), inst, wireStatus, rt, rgd, false)
		require.NoError(t, err)

		// 0 status patch actions because status matched
		assert.Equal(t, 0, countStatusUpdates(raw.Actions()))
	})

	t.Run("Author conditions projected and stamped", func(t *testing.T) {
		inst := newInstanceObject("demo", "default")
		raw := newControllerTestDynamicClient(t, inst.DeepCopy())
		spec := testRGDSpecWithAuthorConditions("${runtime.newCondition({type: 'CustomReady', status: 'True', reason: 'CustomOK', message: 'all good'})}")
		c, _ := newGraphEngineControllerUnderTest(t, raw, spec, revisions.RevisionStateActive, comp, nil)
		c.reconcileConfig.HasAuthorConditions = true

		mark := NewConditionsMarkerFor(inst)
		mark.InstanceManaged()
		mark.GraphResolved()
		mark.ResourcesReady()

		rgd := &v1alpha1.ResourceGraphDefinition{
			ObjectMeta: metav1.ObjectMeta{Name: "webapps"},
			Spec:       *spec,
		}
		rt, _, err := rgdadapter.BuildRuntimeForInstance(rgd, inst, comp)
		require.NoError(t, err)

		err = c.persistGraphEngineStatus(context.Background(), inst, nil, rt, rgd, false)
		require.NoError(t, err)

		stored := getStoredParentObject(t, raw)
		status, _, _ := unstructured.NestedMap(stored.Object, "status")
		require.NotNil(t, status)
		assert.Equal(t, string(v1alpha1.InstanceStateActive), status["state"])
		customCond := conditionByType(t, stored, "CustomReady")
		assert.Equal(t, metav1.ConditionTrue, customCond.Status)
		require.NotNil(t, customCond.Reason)
		assert.Equal(t, "CustomOK", *customCond.Reason)
	})

	t.Run("Author conditions incomplete merges with previous", func(t *testing.T) {
		inst := newInstanceObject("demo", "default")
		earlier := "2026-01-01T00:00:00Z"
		wireStatus := map[string]interface{}{
			"state": string(v1alpha1.InstanceStateInProgress),
			"conditions": []interface{}{
				map[string]interface{}{
					"type":               "PriorCond",
					"status":             "True",
					"reason":             "Old",
					"lastTransitionTime": earlier,
				},
			},
		}
		inst.Object["status"] = wireStatus

		raw := newControllerTestDynamicClient(t, inst.DeepCopy())
		// Expression referencing absent data (data-pending / incomplete)
		spec := testRGDSpecWithAuthorConditions("${runtime.newCondition({type: 'CustomReady', status: string(schema.status.absent)})}")
		c, _ := newGraphEngineControllerUnderTest(t, raw, spec, revisions.RevisionStateActive, comp, nil)
		c.reconcileConfig.HasAuthorConditions = true

		mark := NewConditionsMarkerFor(inst)
		mark.InstanceManaged()
		mark.GraphResolved()
		mark.ResourcesReady()

		rgd := &v1alpha1.ResourceGraphDefinition{
			ObjectMeta: metav1.ObjectMeta{Name: "webapps"},
			Spec:       *spec,
		}
		rt, _, err := rgdadapter.BuildRuntimeForInstance(rgd, inst, comp)
		require.NoError(t, err)

		err = c.persistGraphEngineStatus(context.Background(), inst, wireStatus, rt, rgd, false)
		require.NoError(t, err)

		stored := getStoredParentObject(t, raw)
		priorCond := conditionByType(t, stored, "PriorCond")
		assert.Equal(t, metav1.ConditionTrue, priorCond.Status)
	})

	t.Run("Author conditions projection error sets state Error", func(t *testing.T) {
		inst := newInstanceObject("demo", "default")
		raw := newControllerTestDynamicClient(t, inst.DeepCopy())
		// Specifically craft RGD with duplicate condition types
		statusBytes, _ := json.Marshal(map[string]any{
			"conditions": []any{
				"${runtime.newCondition({type: 'Dup', status: 'True'})}",
				"${runtime.newCondition({type: 'Dup', status: 'False'})}",
			},
		})
		spec := testEmptyRGDSpec()
		spec.Schema.Status = apimachineryruntime.RawExtension{Raw: statusBytes}

		c, _ := newGraphEngineControllerUnderTest(t, raw, spec, revisions.RevisionStateActive, comp, nil)
		c.reconcileConfig.HasAuthorConditions = true

		mark := NewConditionsMarkerFor(inst)
		mark.InstanceManaged()
		mark.GraphResolved()
		mark.ResourcesReady()

		rgd := &v1alpha1.ResourceGraphDefinition{
			ObjectMeta: metav1.ObjectMeta{Name: "webapps"},
			Spec:       *spec,
		}
		rt, _, err := rgdadapter.BuildRuntimeForInstance(rgd, inst, comp)
		require.NoError(t, err)

		err = c.persistGraphEngineStatus(context.Background(), inst, nil, rt, rgd, false)
		require.NoError(t, err)

		stored := getStoredParentObject(t, raw)
		status, _, _ := unstructured.NestedMap(stored.Object, "status")
		require.NotNil(t, status)
		assert.Equal(t, string(v1alpha1.InstanceStateError), status["state"])
	})

	t.Run("Status persist error propagated", func(t *testing.T) {
		inst := newInstanceObject("demo", "default")
		raw := newControllerTestDynamicClient(t, inst.DeepCopy())
		raw.PrependReactor("patch", "webapps", func(action k8stesting.Action) (bool, apimachineryruntime.Object, error) {
			return true, nil, errors.New("API server error during status patch")
		})

		c, _ := newGraphEngineControllerUnderTest(t, raw, testEmptyRGDSpec(), revisions.RevisionStateActive, comp, nil)
		rgd := &v1alpha1.ResourceGraphDefinition{
			ObjectMeta: metav1.ObjectMeta{Name: "webapps"},
			Spec:       *testEmptyRGDSpec(),
		}
		rt, _, err := rgdadapter.BuildRuntimeForInstance(rgd, inst, comp)
		require.NoError(t, err)

		err = c.persistGraphEngineStatus(context.Background(), inst, nil, rt, rgd, false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "API server error during status patch")
	})
}

// -----------------------------------------------------------------------------
// 6. reconcileViaGraphEngine: Revision Resolution & Early Exit Tests
// -----------------------------------------------------------------------------

func TestReconcileViaGraphEngine_RevisionHandling(t *testing.T) {
	comp := newTestRealCompiler(t)

	t.Run("Latest revision not found -> delayed requeue and condition set", func(t *testing.T) {
		inst := newInstanceObject("demo", "default")
		raw := newControllerTestDynamicClient(t, inst.DeepCopy())
		c, _ := newGraphEngineControllerUnderTest(t, raw, nil, "", comp, nil)

		watcher := &fakeInstanceWatcher{}
		err := c.reconcileViaGraphEngine(context.Background(), inst, watcher)
		require.Error(t, err)
		assert.True(t, requeue.IsRequeueError(err))
		assert.Contains(t, err.Error(), "latest issued revision not available")

		stored := getStoredParentObject(t, raw)
		cond := conditionByType(t, stored, GraphResolved)
		assert.Equal(t, metav1.ConditionFalse, cond.Status)
		require.NotNil(t, cond.Reason)
		assert.Equal(t, "ResolutionFailed", *cond.Reason)
		require.NotNil(t, cond.Message)
		assert.Contains(t, *cond.Message, "latest issued revision not found")
	})

	t.Run("Latest revision Failed -> non-delayed fatal requeue", func(t *testing.T) {
		inst := newInstanceObject("demo", "default")
		raw := newControllerTestDynamicClient(t, inst.DeepCopy())
		c, _ := newGraphEngineControllerUnderTest(t, raw, testEmptyRGDSpec(), revisions.RevisionStateFailed, comp, nil)

		watcher := &fakeInstanceWatcher{}
		err := c.reconcileViaGraphEngine(context.Background(), inst, watcher)
		require.Error(t, err)
		assert.False(t, requeue.IsRequeueError(err), "requeue.None is not a requeue error")
		var noReq *requeue.NoRequeue
		assert.True(t, errors.As(err, &noReq), "failed revision must return requeue.None")
		assert.Contains(t, err.Error(), "latest issued revision 1 failed")

		stored := getStoredParentObject(t, raw)
		cond := conditionByType(t, stored, GraphResolved)
		assert.Equal(t, metav1.ConditionFalse, cond.Status)
		require.NotNil(t, cond.Message)
		assert.Contains(t, *cond.Message, "latest issued revision 1 failed")
	})

	t.Run("Latest revision not Active (Pending) -> delayed requeue", func(t *testing.T) {
		inst := newInstanceObject("demo", "default")
		raw := newControllerTestDynamicClient(t, inst.DeepCopy())
		c, _ := newGraphEngineControllerUnderTest(t, raw, testEmptyRGDSpec(), revisions.RevisionStatePending, comp, nil)

		watcher := &fakeInstanceWatcher{}
		err := c.reconcileViaGraphEngine(context.Background(), inst, watcher)
		require.Error(t, err)
		assert.True(t, requeue.IsRequeueError(err))
		assert.Contains(t, err.Error(), "is not active (state=Pending)")

		stored := getStoredParentObject(t, raw)
		cond := conditionByType(t, stored, GraphResolved)
		assert.Equal(t, metav1.ConditionFalse, cond.Status)
		require.NotNil(t, cond.Message)
		assert.Contains(t, *cond.Message, "is not active (state=Pending)")
	})

	t.Run("Latest revision Active but RGDSpec is nil -> requeueUntilRGDSpecPopulated", func(t *testing.T) {
		inst := newInstanceObject("demo", "default")
		raw := newControllerTestDynamicClient(t, inst.DeepCopy())
		// Pass nil rgdSpec with Active state
		c, _ := newGraphEngineControllerUnderTest(t, raw, nil, revisions.RevisionStateActive, comp, nil)

		watcher := &fakeInstanceWatcher{}
		err := c.reconcileViaGraphEngine(context.Background(), inst, watcher)
		require.Error(t, err)
		assert.True(t, requeue.IsRequeueError(err))
		assert.Contains(t, err.Error(), "revision entry has no RGDSpec")

		stored := getStoredParentObject(t, raw)
		cond := conditionByType(t, stored, GraphResolved)
		assert.Equal(t, metav1.ConditionFalse, cond.Status)
		require.NotNil(t, cond.Message)
		assert.Contains(t, *cond.Message, "revision entry has no RGDSpec")
	})

	t.Run("updateConditionsStatus error on early exit is tolerated", func(t *testing.T) {
		inst := newInstanceObject("demo", "default")
		raw := newControllerTestDynamicClient(t, inst.DeepCopy())
		raw.PrependReactor("update", "webapps", func(action k8stesting.Action) (bool, apimachineryruntime.Object, error) {
			if action.GetSubresource() == "status" {
				return true, nil, errors.New("updateStatus conflict / error")
			}
			return false, nil, nil
		})

		c, _ := newGraphEngineControllerUnderTest(t, raw, nil, revisions.RevisionStatePending, comp, nil)
		watcher := &fakeInstanceWatcher{}
		err := c.reconcileViaGraphEngine(context.Background(), inst, watcher)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "is not active (state=Pending)")
	})
}

// -----------------------------------------------------------------------------
// 7. reconcileViaGraphEngine: Compiler Guards & Build Runtime Tests
// -----------------------------------------------------------------------------

func TestReconcileViaGraphEngine_CompilerGuards(t *testing.T) {
	t.Run("Compiler not wired -> programming error", func(t *testing.T) {
		inst := newInstanceObject("demo", "default")
		raw := newControllerTestDynamicClient(t, inst.DeepCopy())
		c, _ := newGraphEngineControllerUnderTest(t, raw, testEmptyRGDSpec(), revisions.RevisionStateActive, nil, nil)
		c.graphEngineCompiler = nil

		watcher := &fakeInstanceWatcher{}
		err := c.reconcileViaGraphEngine(context.Background(), inst, watcher)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "compiler not wired")
	})

	t.Run("BuildRuntimeForInstanceCached error -> condition marked and error returned", func(t *testing.T) {
		inst := newInstanceObject("demo", "default")
		raw := newControllerTestDynamicClient(t, inst.DeepCopy())
		stub := &testStubCompiler{err: errors.New("CEL compilation failed: invalid expression")}
		c, _ := newGraphEngineControllerUnderTest(t, raw, testEmptyRGDSpec(), revisions.RevisionStateActive, stub, nil)

		watcher := &fakeInstanceWatcher{}
		err := c.reconcileViaGraphEngine(context.Background(), inst, watcher)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "CEL compilation failed: invalid expression")

		stored := getStoredParentObject(t, raw)
		cond := conditionByType(t, stored, GraphResolved)
		assert.Equal(t, metav1.ConditionFalse, cond.Status)
		require.NotNil(t, cond.Message)
		assert.Contains(t, *cond.Message, "graph-engine build failed")
	})

	t.Run("BuildRuntimeForInstanceCached error with updateConditionsStatus error", func(t *testing.T) {
		inst := newInstanceObject("demo", "default")
		raw := newControllerTestDynamicClient(t, inst.DeepCopy())
		raw.PrependReactor("update", "webapps", func(action k8stesting.Action) (bool, apimachineryruntime.Object, error) {
			if action.GetSubresource() == "status" {
				return true, nil, errors.New("updateStatus error")
			}
			return false, nil, nil
		})

		stub := &testStubCompiler{err: errors.New("compile error")}
		c, _ := newGraphEngineControllerUnderTest(t, raw, testEmptyRGDSpec(), revisions.RevisionStateActive, stub, nil)

		watcher := &fakeInstanceWatcher{}
		err := c.reconcileViaGraphEngine(context.Background(), inst, watcher)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "compile error")
	})
}

// -----------------------------------------------------------------------------
// 8. reconcileViaGraphEngine: Stamp Metadata Errors
// -----------------------------------------------------------------------------

func TestReconcileViaGraphEngine_StampMetadata(t *testing.T) {
	comp := newTestRealCompiler(t)

	t.Run("Corrupted partial ApplySet metadata causes stampInstanceMetadata to fail", func(t *testing.T) {
		inst := newInstanceObject("demo", "default")
		// Set partial ApplySet inventory metadata without required hash
		inst.SetLabels(map[string]string{
			applyset.ApplySetParentIDLabel: applyset.ID(inst),
		})
		inst.SetAnnotations(map[string]string{
			applyset.ApplySetToolingAnnotation: applyset.ToolingID(),
			applyset.ApplySetGKsAnnotation:     "Deployment.apps",
			// Missing ApplySetInventoryHashAnnotation -> ValidateParentInventory fails
		})

		raw := newControllerTestDynamicClient(t, inst.DeepCopy())
		c, _ := newGraphEngineControllerUnderTest(t, raw, testEmptyRGDSpec(), revisions.RevisionStateActive, comp, nil)

		watcher := &fakeInstanceWatcher{}
		err := c.reconcileViaGraphEngine(context.Background(), inst, watcher)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cannot install finalizer with invalid ApplySet inventory")
	})

	t.Run("Dynamic client error during stampInstanceMetadata is returned", func(t *testing.T) {
		inst := newInstanceObject("demo", "default")
		raw := newControllerTestDynamicClient(t, inst.DeepCopy())
		raw.PrependReactor("patch", "webapps", func(action k8stesting.Action) (bool, apimachineryruntime.Object, error) {
			return true, nil, errors.New("patch metadata failed")
		})

		c, _ := newGraphEngineControllerUnderTest(t, raw, testEmptyRGDSpec(), revisions.RevisionStateActive, comp, nil)
		watcher := &fakeInstanceWatcher{}
		err := c.reconcileViaGraphEngine(context.Background(), inst, watcher)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed stamping instance metadata")
	})
}

// -----------------------------------------------------------------------------
// 9. reconcileViaGraphEngine: Successful Apply Path
// -----------------------------------------------------------------------------

func TestReconcileViaGraphEngine_Success(t *testing.T) {
	comp := newTestRealCompiler(t)

	t.Run("Successful apply with empty resources -> all conditions True and state Active", func(t *testing.T) {
		inst := newInstanceObject("demo", "default")
		raw := newControllerTestDynamicClient(t, inst.DeepCopy())
		fakeRuntimeCl := newFakeRuntimeClient(t)
		c, _ := newGraphEngineControllerUnderTest(t, raw, testEmptyRGDSpec(), revisions.RevisionStateActive, comp, fakeRuntimeCl)

		watcher := &fakeInstanceWatcher{}
		err := c.reconcileViaGraphEngine(context.Background(), inst, watcher)
		require.NoError(t, err)

		stored := getStoredParentObject(t, raw)
		assert.True(t, metadata.HasInstanceFinalizer(stored), "finalizer should be stamped")
		assert.Equal(t, metav1.ConditionTrue, conditionByType(t, stored, InstanceManaged).Status)
		assert.Equal(t, metav1.ConditionTrue, conditionByType(t, stored, GraphResolved).Status)
		assert.Equal(t, metav1.ConditionTrue, conditionByType(t, stored, ResourcesReady).Status)
		assert.Equal(t, metav1.ConditionTrue, conditionByType(t, stored, Ready).Status)

		status, _, _ := unstructured.NestedMap(stored.Object, "status")
		require.NotNil(t, status)
		assert.Equal(t, string(v1alpha1.InstanceStateActive), status["state"])

		// Verify ApplySet inventory metadata was stamped
		assert.NotEmpty(t, stored.GetLabels()[applyset.ApplySetParentIDLabel])
		assert.NotEmpty(t, stored.GetAnnotations()[applyset.ApplySetToolingAnnotation])
	})

	t.Run("Successful apply for cluster-scoped instance", func(t *testing.T) {
		inst := newInstanceObject("cluster-demo", "")
		raw := newControllerTestDynamicClient(t, inst.DeepCopy())
		fakeRuntimeCl := newFakeRuntimeClient(t)
		c, _ := newGraphEngineControllerUnderTest(t, raw, testEmptyRGDSpec(), revisions.RevisionStateActive, comp, fakeRuntimeCl)
		c.namespaced = false

		watcher := &fakeInstanceWatcher{}
		err := c.reconcileViaGraphEngine(context.Background(), inst, watcher)
		require.NoError(t, err)

		stored, err := raw.Resource(controllerTestParentGVR).Get(context.Background(), "cluster-demo", metav1.GetOptions{})
		require.NoError(t, err)
		assert.True(t, metadata.HasInstanceFinalizer(stored))
		assert.Equal(t, metav1.ConditionTrue, conditionByType(t, stored, ResourcesReady).Status)
	})

	t.Run("Successful apply with MaxCollectionSize option", func(t *testing.T) {
		inst := newInstanceObject("demo", "default")
		raw := newControllerTestDynamicClient(t, inst.DeepCopy())
		fakeRuntimeCl := newFakeRuntimeClient(t)
		c, _ := newGraphEngineControllerUnderTest(t, raw, testEmptyRGDSpec(), revisions.RevisionStateActive, comp, fakeRuntimeCl)
		c.reconcileConfig.MaxCollectionSize = 25

		watcher := &fakeInstanceWatcher{}
		err := c.reconcileViaGraphEngine(context.Background(), inst, watcher)
		require.NoError(t, err)

		stored := getStoredParentObject(t, raw)
		assert.Equal(t, metav1.ConditionTrue, conditionByType(t, stored, ResourcesReady).Status)
	})

	t.Run("NewController sets ApplyConcurrency on executor", func(t *testing.T) {
		ctrl := NewController(
			zap.New(zap.UseDevMode(true)),
			ReconcileConfig{
				ApplyConcurrency: 42,
			},
			controllerTestParentGVR,
			nil,
			true,
			nil,
			nil,
			nil,
			nil,
			nil,
			nil,
		)
		require.NotNil(t, ctrl)
		require.NotNil(t, ctrl.graphEngineExecutor)
		assert.Equal(t, 42, ctrl.graphEngineExecutor.ApplyConcurrency)
	})

	t.Run("Successful apply with real template resource creates object", func(t *testing.T) {
		inst := newInstanceObject("demo", "default")
		raw := newControllerTestDynamicClient(t, inst.DeepCopy())
		fakeRuntimeCl := newFakeRuntimeClient(t)
		spec := testRGDSpecWithConfigMap("app-config", "")
		c, _ := newGraphEngineControllerUnderTest(t, raw, spec, revisions.RevisionStateActive, comp, fakeRuntimeCl)

		watcher := &fakeInstanceWatcher{}
		err := c.reconcileViaGraphEngine(context.Background(), inst, watcher)
		require.NoError(t, err)

		stored := getStoredParentObject(t, raw)
		assert.Equal(t, metav1.ConditionTrue, conditionByType(t, stored, ResourcesReady).Status)
		status, _, _ := unstructured.NestedMap(stored.Object, "status")
		require.NotNil(t, status)
		assert.Equal(t, string(v1alpha1.InstanceStateActive), status["state"])
	})

	t.Run("Reconcile end-to-end via Controller.Reconcile", func(t *testing.T) {
		inst := newInstanceObject("demo", "default")
		raw := newControllerTestDynamicClient(t, inst.DeepCopy())
		fakeRuntimeCl := newFakeRuntimeClient(t)
		c, _ := newGraphEngineControllerUnderTest(t, raw, testEmptyRGDSpec(), revisions.RevisionStateActive, comp, fakeRuntimeCl)

		err := c.Reconcile(context.Background(), ctrl.Request{
			NamespacedName: types.NamespacedName{Name: "demo", Namespace: "default"},
		})
		require.NoError(t, err)

		stored := getStoredParentObject(t, raw)
		assert.Equal(t, metav1.ConditionTrue, conditionByType(t, stored, Ready).Status)
	})
}

// -----------------------------------------------------------------------------
// 10. reconcileViaGraphEngine: Soft Errors (ErrNotReady & ResourceDeleting)
// -----------------------------------------------------------------------------

func TestReconcileViaGraphEngine_SoftErrors(t *testing.T) {
	comp := newTestRealCompiler(t)

	t.Run("Executor returns ErrNotReady -> ResourcesReady False (NotReady), state InProgress", func(t *testing.T) {
		inst := newInstanceObject("demo", "default")
		raw := newControllerTestDynamicClient(t, inst.DeepCopy())
		fakeRuntimeCl := newFakeRuntimeClient(t)
		// readyWhen expression that evaluates to false: ${cm.data.key == 'other'}
		spec := testRGDSpecWithConfigMap("app-config", "${cm.data.key == 'other'}")
		c, _ := newGraphEngineControllerUnderTest(t, raw, spec, revisions.RevisionStateActive, comp, fakeRuntimeCl)

		watcher := &fakeInstanceWatcher{}
		err := c.reconcileViaGraphEngine(context.Background(), inst, watcher)
		require.Error(t, err)
		assert.True(t, requeue.IsRequeueError(err))
		assert.True(t, errors.Is(err, executor.ErrNotReady))

		stored := getStoredParentObject(t, raw)
		cond := conditionByType(t, stored, ResourcesReady)
		assert.Equal(t, metav1.ConditionFalse, cond.Status)
		require.NotNil(t, cond.Reason)
		assert.Equal(t, "NotReady", *cond.Reason)
		require.NotNil(t, cond.Message)
		assert.Contains(t, *cond.Message, "waiting for unresolved resource")

		status, _, _ := unstructured.NestedMap(stored.Object, "status")
		require.NotNil(t, status)
		assert.Equal(t, string(v1alpha1.InstanceStateInProgress), status["state"])
	})

	t.Run("Executor returns typed ResourceDeletingError -> ResourcesReady False (ResourceDeleting), state InProgress", func(t *testing.T) {
		inst := newInstanceObject("demo", "default")
		raw := newControllerTestDynamicClient(t, inst.DeepCopy())

		// Seed fakeRuntimeClient with a ConfigMap that has DeletionTimestamp set
		deletingCM := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name":              "app-config",
					"namespace":         "default",
					"deletionTimestamp": time.Now().Format(time.RFC3339),
					"finalizers":        []interface{}{"kro.run/test"},
				},
				"data": map[string]interface{}{"key": "val"},
			},
		}
		fakeRuntimeCl := newFakeRuntimeClient(t, deletingCM)
		spec := testRGDSpecWithConfigMap("app-config", "")
		c, _ := newGraphEngineControllerUnderTest(t, raw, spec, revisions.RevisionStateActive, comp, fakeRuntimeCl)

		watcher := &fakeInstanceWatcher{}
		err := c.reconcileViaGraphEngine(context.Background(), inst, watcher)
		require.Error(t, err)
		assert.True(t, requeue.IsRequeueError(err))
		assert.True(t, isResourceDeleting(err))

		stored := getStoredParentObject(t, raw)
		cond := conditionByType(t, stored, ResourcesReady)
		assert.Equal(t, metav1.ConditionFalse, cond.Status)
		require.NotNil(t, cond.Reason)
		assert.Equal(t, "ResourceDeleting", *cond.Reason)

		status, _, _ := unstructured.NestedMap(stored.Object, "status")
		require.NotNil(t, status)
		assert.Equal(t, string(v1alpha1.InstanceStateInProgress), status["state"])
	})
}

// -----------------------------------------------------------------------------
// 11. reconcileViaGraphEngine: Hard Apply Errors & Inventory Errors
// -----------------------------------------------------------------------------

func TestReconcileViaGraphEngine_HardErrorsAndInventory(t *testing.T) {
	comp := newTestRealCompiler(t)

	t.Run("Executor returns hard error -> ResourcesReady False, degraded state Error", func(t *testing.T) {
		inst := newInstanceObject("demo", "default")
		raw := newControllerTestDynamicClient(t, inst.DeepCopy())
		spec := testRGDSpecWithConfigMap("app-config", "")
		errCl := &errorClient{
			Client:   newFakeRuntimeClient(t),
			patchErr: errors.New("SSA apply failed: connection refused"),
		}
		c, _ := newGraphEngineControllerUnderTest(t, raw, spec, revisions.RevisionStateActive, comp, errCl)

		watcher := &fakeInstanceWatcher{}
		err := c.reconcileViaGraphEngine(context.Background(), inst, watcher)
		require.Error(t, err)
		assert.True(t, requeue.IsRequeueError(err))

		stored := getStoredParentObject(t, raw)
		cond := conditionByType(t, stored, ResourcesReady)
		assert.Equal(t, metav1.ConditionFalse, cond.Status)
		require.NotNil(t, cond.Reason)
		assert.Equal(t, "NotReady", *cond.Reason)
		require.NotNil(t, cond.Message)
		assert.Contains(t, *cond.Message, "resource reconciliation failed")

		status, _, _ := unstructured.NestedMap(stored.Object, "status")
		require.NotNil(t, status)
		assert.Equal(t, string(v1alpha1.InstanceStateError), status["state"])
	})

	t.Run("ApplySet orphan pruning succeeds in reverse apply order", func(t *testing.T) {
		inst := newInstanceObject("demo", "default")
		addDeletionScope(inst, controllerTestDeployGVK, "default")
		addDeletionScope(inst, controllerTestCMGVK, "default")

		orphanDeploy := newManagedObject(newDeploymentObject("orphan-deploy", "default"), inst, "deploy", 2)
		orphanCM := newManagedObject(newConfigMapObject("orphan-cm", "default"), inst, "cm", 1)

		raw := newControllerTestDynamicClient(t, inst.DeepCopy(), orphanDeploy, orphanCM)
		fakeRuntimeCl := newFakeRuntimeClient(t)
		c, _ := newGraphEngineControllerUnderTest(t, raw, testEmptyRGDSpec(), revisions.RevisionStateActive, comp, fakeRuntimeCl)

		watcher := &fakeInstanceWatcher{}
		err := c.reconcileViaGraphEngine(context.Background(), inst, watcher)
		require.NoError(t, err)

		// Both orphans must have been pruned from dynamic client
		_, err = raw.Tracker().Get(controllerTestDeployGVR, "default", "orphan-deploy")
		assert.Error(t, err, "orphan-deploy should be deleted")
		_, err = raw.Tracker().Get(controllerTestCMGVR, "default", "orphan-cm")
		assert.Error(t, err, "orphan-cm should be deleted")
	})

	t.Run("ApplySet orphan pruning with UID conflict preserves inventory", func(t *testing.T) {
		inst := newInstanceObject("demo", "default")
		addDeletionScope(inst, controllerTestDeployGVK, "default")

		orphanDeploy := newManagedObject(newDeploymentObject("orphan-deploy", "default"), inst, "deploy", 2)

		raw := newControllerTestDynamicClient(t, inst.DeepCopy(), orphanDeploy)
		raw.PrependReactor("delete", "deployments", func(action k8stesting.Action) (bool, apimachineryruntime.Object, error) {
			return true, nil, apierrors.NewConflict(schema.GroupResource{Group: "apps", Resource: "deployments"}, "orphan-deploy", errors.New("UID mismatch"))
		})

		fakeRuntimeCl := newFakeRuntimeClient(t)
		c, _ := newGraphEngineControllerUnderTest(t, raw, testEmptyRGDSpec(), revisions.RevisionStateActive, comp, fakeRuntimeCl)

		watcher := &fakeInstanceWatcher{}
		err := c.reconcileViaGraphEngine(context.Background(), inst, watcher)
		require.NoError(t, err)

		stored := getStoredParentObject(t, raw)
		// Superset inventory preserved
		assert.Contains(t, stored.GetAnnotations()[applyset.ApplySetGKsAnnotation], "Deployment.apps")
	})

	t.Run("ApplySet inventory patch failure returns error and gates prune", func(t *testing.T) {
		inst := newInstanceObject("demo", "default")
		addDeletionScope(inst, controllerTestDeployGVK, "default")
		orphanDeploy := newManagedObject(newDeploymentObject("orphan-deploy", "default"), inst, "deploy", 2)

		raw := newControllerTestDynamicClient(t, inst.DeepCopy(), orphanDeploy)
		// Fail the superset inventory patch
		raw.PrependReactor("patch", "webapps", func(action k8stesting.Action) (bool, apimachineryruntime.Object, error) {
			patchAction := action.(k8stesting.PatchAction)
			if string(patchAction.GetPatch()) != "" && action.GetSubresource() != "status" {
				return true, nil, errors.New("patch superset inventory failed")
			}
			return false, nil, nil
		})

		fakeRuntimeCl := newFakeRuntimeClient(t)
		c, _ := newGraphEngineControllerUnderTest(t, raw, testEmptyRGDSpec(), revisions.RevisionStateActive, comp, fakeRuntimeCl)

		watcher := &fakeInstanceWatcher{}
		err := c.reconcileViaGraphEngine(context.Background(), inst, watcher)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "patch superset inventory failed")

		// Prune must have been withheld
		storedDeploy, err := raw.Tracker().Get(controllerTestDeployGVR, "default", "orphan-deploy")
		require.NoError(t, err)
		assert.NotNil(t, storedDeploy)
	})

	t.Run("ApplySet orphan list failure returns error", func(t *testing.T) {
		inst := newInstanceObject("demo", "default")
		addDeletionScope(inst, controllerTestDeployGVK, "default")

		raw := newControllerTestDynamicClient(t, inst.DeepCopy())
		raw.PrependReactor("list", "deployments", func(action k8stesting.Action) (bool, apimachineryruntime.Object, error) {
			return true, nil, errors.New("list deployments error")
		})

		fakeRuntimeCl := newFakeRuntimeClient(t)
		c, _ := newGraphEngineControllerUnderTest(t, raw, testEmptyRGDSpec(), revisions.RevisionStateActive, comp, fakeRuntimeCl)

		watcher := &fakeInstanceWatcher{}
		err := c.reconcileViaGraphEngine(context.Background(), inst, watcher)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "list orphans")
	})

	t.Run("persistGraphEngineStatus failure propagates immediately", func(t *testing.T) {
		inst := newInstanceObject("demo", "default")
		raw := newControllerTestDynamicClient(t, inst.DeepCopy())
		raw.PrependReactor("patch", "webapps", func(action k8stesting.Action) (bool, apimachineryruntime.Object, error) {
			if action.GetSubresource() == "status" {
				return true, nil, errors.New("status SSA patch fatal error")
			}
			return false, nil, nil
		})

		fakeRuntimeCl := newFakeRuntimeClient(t)
		c, _ := newGraphEngineControllerUnderTest(t, raw, testEmptyRGDSpec(), revisions.RevisionStateActive, comp, fakeRuntimeCl)

		watcher := &fakeInstanceWatcher{}
		err := c.reconcileViaGraphEngine(context.Background(), inst, watcher)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "status SSA patch fatal error")
	})
}

// -----------------------------------------------------------------------------
// 12. Helper Functions Unit Tests
// -----------------------------------------------------------------------------

func TestApplySetMetadataFromApplied(t *testing.T) {
	parent := newInstanceObject("parent-inst", "test-ns")

	t.Run("empty applied", func(t *testing.T) {
		meta := applySetMetadataFromApplied(parent, nil)
		assert.Equal(t, applyset.ID(parent), meta.ID)
		assert.Equal(t, applyset.ToolingID(), meta.Tooling)
		assert.Equal(t, 0, meta.GroupKinds.Len())
		assert.Equal(t, 0, meta.AdditionalNamespaces.Len())
	})

	t.Run("applied with same and different namespaces", func(t *testing.T) {
		applied := []v1alpha1.ManagedResource{
			{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Namespace:  "test-ns", // same as parent -> excluded from AdditionalNamespaces
				Name:       "dep-1",
			},
			{
				APIVersion: "v1",
				Kind:       "ConfigMap",
				Namespace:  "other-ns", // different -> included in AdditionalNamespaces
				Name:       "cm-1",
			},
			{
				APIVersion: "invalid/version/extra", // invalid GV -> skipped
				Kind:       "Broken",
			},
		}

		meta := applySetMetadataFromApplied(parent, applied)
		assert.True(t, meta.GroupKinds.Has(schema.GroupKind{Group: "apps", Kind: "Deployment"}))
		assert.True(t, meta.GroupKinds.Has(schema.GroupKind{Group: "", Kind: "ConfigMap"}))
		assert.False(t, meta.AdditionalNamespaces.Has("test-ns"), "parent namespace must be excluded per KEP-3659")
		assert.True(t, meta.AdditionalNamespaces.Has("other-ns"))
	})
}

func TestCandidateMetadata(t *testing.T) {
	comp := newTestRealCompiler(t)
	inst := newInstanceObject("demo", "default")

	rgd := &v1alpha1.ResourceGraphDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "webapp"},
		Spec: v1alpha1.ResourceGraphDefinitionSpec{
			Schema: &v1alpha1.Schema{
				APIVersion: "v1alpha1",
				Kind:       "WebApp",
				Spec:       apimachineryruntime.RawExtension{Raw: []byte(`{}`)},
			},
			Resources: []*v1alpha1.Resource{
				{
					ID: "cm1",
					Template: apimachineryruntime.RawExtension{
						Raw: []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"cm-1"}}`),
					},
				},
				{
					ID: "cm2",
					Template: apimachineryruntime.RawExtension{
						Raw: []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"cm-2","namespace":"custom-ns"}}`),
					},
				},
			},
		},
	}
	rt, _, err := rgdadapter.BuildRuntimeForInstance(rgd, inst, comp)
	require.NoError(t, err)

	raw := newControllerTestDynamicClient(t, inst.DeepCopy())
	c, _ := newGraphEngineControllerUnderTest(t, raw, testEmptyRGDSpec(), revisions.RevisionStateActive, comp, nil)

	meta := c.candidateMetadata(rt, inst)
	assert.True(t, meta.GroupKinds.Has(schema.GroupKind{Group: "", Kind: "ConfigMap"}))
	assert.True(t, meta.AdditionalNamespaces.Has("custom-ns"))
}

func TestInventoryUpToDate(t *testing.T) {
	inst := newInstanceObject("demo", "default")
	inst.SetLabels(map[string]string{"l1": "v1", "l2": "v2"})
	inst.SetAnnotations(map[string]string{"a1": "w1", "a2": "w2"})

	assert.True(t, inventoryUpToDate(inst,
		map[string]string{"l1": "v1"},
		map[string]string{"a1": "w1"},
	))

	assert.False(t, inventoryUpToDate(inst,
		map[string]string{"l1": "different"},
		map[string]string{"a1": "w1"},
	))

	assert.False(t, inventoryUpToDate(inst,
		map[string]string{"missing": "v"},
		map[string]string{"a1": "w1"},
	))

	assert.False(t, inventoryUpToDate(inst,
		map[string]string{"l1": "v1"},
		map[string]string{"a1": "different"},
	))

	assert.False(t, inventoryUpToDate(inst,
		map[string]string{"l1": "v1"},
		map[string]string{"missing": "w"},
	))
}

func TestPatchInstanceApplySetMetadata(t *testing.T) {
	inst := newInstanceObject("demo", "default")
	meta := applyset.Metadata{
		ID:                   applyset.ID(inst),
		Tooling:              applyset.ToolingID(),
		GroupKinds:           sets.New(schema.GroupKind{Group: "apps", Kind: "Deployment"}),
		AdditionalNamespaces: sets.New("other-ns"),
	}

	t.Run("fast path when already up to date", func(t *testing.T) {
		instCopy := inst.DeepCopy()
		instCopy.SetLabels(meta.Labels())
		instCopy.SetAnnotations(meta.Annotations())

		raw := newControllerTestDynamicClient(t, instCopy)
		c, _ := newGraphEngineControllerUnderTest(t, raw, testEmptyRGDSpec(), revisions.RevisionStateActive, nil, nil)
		raw.ClearActions()

		err := c.patchInstanceApplySetMetadata(context.Background(), instCopy, meta)
		require.NoError(t, err)
		assert.Equal(t, 0, len(raw.Actions()), "no API call when already up to date")
	})

	t.Run("patches metadata on namespaced instance", func(t *testing.T) {
		instCopy := inst.DeepCopy()
		raw := newControllerTestDynamicClient(t, instCopy)
		c, _ := newGraphEngineControllerUnderTest(t, raw, testEmptyRGDSpec(), revisions.RevisionStateActive, nil, nil)

		err := c.patchInstanceApplySetMetadata(context.Background(), instCopy, meta)
		require.NoError(t, err)

		stored := getStoredParentObject(t, raw)
		assert.Equal(t, meta.Labels()[applyset.ApplySetParentIDLabel], stored.GetLabels()[applyset.ApplySetParentIDLabel])
		assert.Equal(t, meta.Annotations()[applyset.ApplySetToolingAnnotation], stored.GetAnnotations()[applyset.ApplySetToolingAnnotation])
	})

	t.Run("patches metadata on cluster-scoped instance", func(t *testing.T) {
		clusterInst := newInstanceObject("cluster-demo", "")
		raw := newControllerTestDynamicClient(t, clusterInst)
		c, _ := newGraphEngineControllerUnderTest(t, raw, testEmptyRGDSpec(), revisions.RevisionStateActive, nil, nil)
		c.namespaced = false

		err := c.patchInstanceApplySetMetadata(context.Background(), clusterInst, meta)
		require.NoError(t, err)
	})
}

// -----------------------------------------------------------------------------
// 13. Additional Edge Cases & Direct Coverage Tests
// -----------------------------------------------------------------------------

func TestReconcileApplySetInventory_Direct(t *testing.T) {
	comp := newTestRealCompiler(t)

	t.Run("Union error propagates as error", func(t *testing.T) {
		inst := newInstanceObject("demo", "default")
		inst.SetAnnotations(map[string]string{
			applyset.ApplySetGKsAnnotation: "Invalid.Format.With.Too.Many.Dots",
		})
		raw := newControllerTestDynamicClient(t, inst.DeepCopy())
		c, _ := newGraphEngineControllerUnderTest(t, raw, testEmptyRGDSpec(), revisions.RevisionStateActive, comp, nil)

		applied := []v1alpha1.ManagedResource{
			{
				NodeID:     "cm",
				APIVersion: "v1",
				Kind:       "ConfigMap",
				Name:       "cm-1",
			},
		}

		err := c.reconcileApplySetInventory(context.Background(), c.log, inst, nil, applied, applyset.Metadata{}, true)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "applyset union:")
	})

	t.Run("Superset inventory patch failure returns error", func(t *testing.T) {
		inst := newInstanceObject("demo", "default")
		raw := newControllerTestDynamicClient(t, inst.DeepCopy())
		raw.PrependReactor("patch", "webapps", func(action k8stesting.Action) (bool, apimachineryruntime.Object, error) {
			if action.GetSubresource() != "status" {
				return true, nil, errors.New("superset patch failed")
			}
			return false, nil, nil
		})
		c, _ := newGraphEngineControllerUnderTest(t, raw, testEmptyRGDSpec(), revisions.RevisionStateActive, comp, nil)

		rgd := &v1alpha1.ResourceGraphDefinition{
			ObjectMeta: metav1.ObjectMeta{Name: "webapp"},
			Spec: v1alpha1.ResourceGraphDefinitionSpec{
				Schema: &v1alpha1.Schema{
					APIVersion: "v1alpha1",
					Kind:       "WebApp",
					Spec:       apimachineryruntime.RawExtension{Raw: []byte(`{}`)},
				},
				Resources: []*v1alpha1.Resource{
					{
						ID: "cm",
						Template: apimachineryruntime.RawExtension{
							Raw: []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"cm-1"}}`),
						},
					},
				},
			},
		}
		rt, _, rterr := rgdadapter.BuildRuntimeForInstance(rgd, inst, comp)
		require.NoError(t, rterr)

		_, _, err := c.preApplyApplySetInventory(context.Background(), c.log, inst, rt)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "patch pre-apply superset inventory: superset patch failed")
	})

	t.Run("Shrink inventory failure after conflict-free prune returns error", func(t *testing.T) {
		inst := newInstanceObject("demo", "default")
		addDeletionScope(inst, controllerTestDeployGVK, "default")
		orphanDeploy := newManagedObject(newDeploymentObject("orphan-deploy", "default"), inst, "deploy", 1)

		raw := newControllerTestDynamicClient(t, inst.DeepCopy(), orphanDeploy)
		// Fail the shrink patch
		raw.PrependReactor("patch", "webapps", func(action k8stesting.Action) (bool, apimachineryruntime.Object, error) {
			if action.GetSubresource() == "status" {
				return false, nil, nil
			}
			return true, nil, errors.New("shrink patch failed")
		})

		c, _ := newGraphEngineControllerUnderTest(t, raw, testEmptyRGDSpec(), revisions.RevisionStateActive, comp, nil)
		err := c.reconcileApplySetInventory(context.Background(), c.log, inst, nil, nil, applyset.Metadata{}, true)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "align inventory after apply/prune: shrink patch failed")
	})

	t.Run("Duplicate resources in applied set returns ErrDuplicateResource", func(t *testing.T) {
		inst := newInstanceObject("demo", "default")
		raw := newControllerTestDynamicClient(t, inst.DeepCopy())
		c, _ := newGraphEngineControllerUnderTest(t, raw, testEmptyRGDSpec(), revisions.RevisionStateActive, comp, nil)

		// Two distinct nodeIDs targeting the exact same GVK, namespace, and name
		applied := []v1alpha1.ManagedResource{
			{
				NodeID:     "cm1",
				APIVersion: "v1",
				Kind:       "ConfigMap",
				Namespace:  "default",
				Name:       "duplicate-cm",
			},
			{
				NodeID:     "cm2",
				APIVersion: "v1",
				Kind:       "ConfigMap",
				Namespace:  "default",
				Name:       "duplicate-cm",
			},
		}

		err := c.reconcileApplySetInventory(context.Background(), c.log, inst, nil, applied, applyset.Metadata{}, true)
		require.Error(t, err)
		assert.True(t, errors.Is(err, applyset.ErrDuplicateResource))
	})
}

func TestPruneGraphEngineOrphans_Direct(t *testing.T) {
	comp := newTestRealCompiler(t)

	t.Run("KeepUIDs populated from applied resources", func(t *testing.T) {
		inst := newInstanceObject("demo", "default")
		addDeletionScope(inst, controllerTestDeployGVK, "default")

		// Create two managed deployments; one has its UID in keepUIDs
		deploy1 := newManagedObject(newDeploymentObject("dep-1", "default"), inst, "deploy", 1)
		deploy1.SetUID(types.UID("keep-uid-1"))

		deploy2 := newManagedObject(newDeploymentObject("dep-2", "default"), inst, "deploy", 1)
		deploy2.SetUID(types.UID("orphan-uid-2"))

		raw := newControllerTestDynamicClient(t, inst.DeepCopy(), deploy1, deploy2)
		c, clientSet := newGraphEngineControllerUnderTest(t, raw, testEmptyRGDSpec(), revisions.RevisionStateActive, comp, nil)

		applier := applyset.New(applyset.Config{
			Client:          clientSet.Dynamic(),
			RESTMapper:      clientSet.RESTMapper(),
			Log:             c.log,
			ParentNamespace: inst.GetNamespace(),
		}, inst)

		applied := []v1alpha1.ManagedResource{
			{
				NodeID:     "deploy",
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Namespace:  "default",
				Name:       "dep-1",
				UID:        "keep-uid-1",
			},
		}

		meta := applySetMetadataFromApplied(inst, applied)
		supersetMeta, _ := applier.Union(meta)

		pruned, conflictFree, err := c.pruneGraphEngineOrphans(context.Background(), c.log, applier, applied, supersetMeta)
		require.NoError(t, err)
		assert.True(t, pruned)
		assert.True(t, conflictFree)

		// dep-1 was kept, dep-2 was pruned
		stored1, err := raw.Tracker().Get(controllerTestDeployGVR, "default", "dep-1")
		require.NoError(t, err)
		assert.NotNil(t, stored1)

		_, err = raw.Tracker().Get(controllerTestDeployGVR, "default", "dep-2")
		assert.Error(t, err)
		_ = meta
	})

	t.Run("DeleteOrphan error is returned", func(t *testing.T) {
		inst := newInstanceObject("demo", "default")
		addDeletionScope(inst, controllerTestDeployGVK, "default")
		orphanDeploy := newManagedObject(newDeploymentObject("orphan-deploy", "default"), inst, "deploy", 1)

		raw := newControllerTestDynamicClient(t, inst.DeepCopy(), orphanDeploy)
		raw.PrependReactor("delete", "deployments", func(action k8stesting.Action) (bool, apimachineryruntime.Object, error) {
			return true, nil, errors.New("delete failed: internal server error")
		})

		c, clientSet := newGraphEngineControllerUnderTest(t, raw, testEmptyRGDSpec(), revisions.RevisionStateActive, comp, nil)
		applier := applyset.New(applyset.Config{
			Client:          clientSet.Dynamic(),
			RESTMapper:      clientSet.RESTMapper(),
			Log:             c.log,
			ParentNamespace: inst.GetNamespace(),
		}, inst)

		supersetMeta, _ := applier.Project(nil)
		_, _, err := c.pruneGraphEngineOrphans(context.Background(), c.log, applier, nil, supersetMeta)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "delete failed: internal server error")
	})
}

func TestRequeueUntilRGDSpecPopulated_Direct(t *testing.T) {
	comp := newTestRealCompiler(t)

	t.Run("updateConditionsStatus succeeds", func(t *testing.T) {
		inst := newInstanceObject("demo", "default")
		raw := newControllerTestDynamicClient(t, inst.DeepCopy())
		c, _ := newGraphEngineControllerUnderTest(t, raw, nil, revisions.RevisionStateActive, comp, nil)

		err := c.requeueUntilRGDSpecPopulated(context.Background(), inst)
		require.Error(t, err)
		assert.True(t, requeue.IsRequeueError(err))
		assert.Contains(t, err.Error(), "revision entry has no RGDSpec")
	})

	t.Run("updateConditionsStatus fails", func(t *testing.T) {
		inst := newInstanceObject("demo", "default")
		raw := newControllerTestDynamicClient(t, inst.DeepCopy())
		raw.PrependReactor("update", "webapps", func(action k8stesting.Action) (bool, apimachineryruntime.Object, error) {
			if action.GetSubresource() == "status" {
				return true, nil, errors.New("update status error")
			}
			return false, nil, nil
		})
		c, _ := newGraphEngineControllerUnderTest(t, raw, nil, revisions.RevisionStateActive, comp, nil)

		err := c.requeueUntilRGDSpecPopulated(context.Background(), inst)
		require.Error(t, err)
		assert.True(t, requeue.IsRequeueError(err))
		assert.Contains(t, err.Error(), "revision entry has no RGDSpec")
	})
}

func TestReconcileViaGraphEngine_ExtraBranches(t *testing.T) {
	comp := newTestRealCompiler(t)

	t.Run("GetLatestRevision false with updateConditionsStatus error", func(t *testing.T) {
		inst := newInstanceObject("demo", "default")
		raw := newControllerTestDynamicClient(t, inst.DeepCopy())
		raw.PrependReactor("update", "webapps", func(action k8stesting.Action) (bool, apimachineryruntime.Object, error) {
			if action.GetSubresource() == "status" {
				return true, nil, errors.New("update status failure")
			}
			return false, nil, nil
		})
		c, _ := newGraphEngineControllerUnderTest(t, raw, nil, "", comp, nil)

		watcher := &fakeInstanceWatcher{}
		err := c.reconcileViaGraphEngine(context.Background(), inst, watcher)
		require.Error(t, err)
		assert.True(t, requeue.IsRequeueError(err))
		assert.Contains(t, err.Error(), "latest issued revision not available")
	})

	t.Run("RevisionStateFailed with updateConditionsStatus error", func(t *testing.T) {
		inst := newInstanceObject("demo", "default")
		raw := newControllerTestDynamicClient(t, inst.DeepCopy())
		raw.PrependReactor("update", "webapps", func(action k8stesting.Action) (bool, apimachineryruntime.Object, error) {
			if action.GetSubresource() == "status" {
				return true, nil, errors.New("update status failure")
			}
			return false, nil, nil
		})
		c, _ := newGraphEngineControllerUnderTest(t, raw, testEmptyRGDSpec(), revisions.RevisionStateFailed, comp, nil)

		watcher := &fakeInstanceWatcher{}
		err := c.reconcileViaGraphEngine(context.Background(), inst, watcher)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "latest issued revision 1 failed")
	})

	t.Run("Non-typed ErrResourceDeleting", func(t *testing.T) {
		inst := newInstanceObject("demo", "default")
		raw := newControllerTestDynamicClient(t, inst.DeepCopy())
		spec := testRGDSpecWithConfigMap("app-config", "")
		errCl := &errorClient{
			Client:   newFakeRuntimeClient(t),
			patchErr: fmt.Errorf("wrapped deleting sentinel: %w", executor.ErrResourceDeleting),
		}
		c, _ := newGraphEngineControllerUnderTest(t, raw, spec, revisions.RevisionStateActive, comp, errCl)

		watcher := &fakeInstanceWatcher{}
		err := c.reconcileViaGraphEngine(context.Background(), inst, watcher)
		require.Error(t, err)
		assert.True(t, isResourceDeleting(err))

		stored := getStoredParentObject(t, raw)
		cond := conditionByType(t, stored, ResourcesReady)
		assert.Equal(t, metav1.ConditionFalse, cond.Status)
		require.NotNil(t, cond.Reason)
		assert.Equal(t, "ResourceDeleting", *cond.Reason)
	})
}

func TestReconcileViaGraphEngine_PatchContributions(t *testing.T) {
	comp := newTestRealCompiler(t)

	t.Run("Malformed patch contributions annotation returns error and sets ResourcesNotReady", func(t *testing.T) {
		inst := newInstanceObject("demo", "default")
		anns := inst.GetAnnotations()
		if anns == nil {
			anns = map[string]string{}
		}
		anns[metadata.PatchContributionsAnnotation] = "not-json"
		inst.SetAnnotations(anns)

		raw := newControllerTestDynamicClient(t, inst.DeepCopy())
		fakeRuntimeCl := newFakeRuntimeClient(t)
		c, _ := newGraphEngineControllerUnderTest(t, raw, testEmptyRGDSpec(), revisions.RevisionStateActive, comp, fakeRuntimeCl)

		watcher := &fakeInstanceWatcher{}
		err := c.reconcileViaGraphEngine(context.Background(), inst, watcher)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "read patch contributions")

		stored := getStoredParentObject(t, raw)
		cond := conditionByType(t, stored, ResourcesReady)
		assert.Equal(t, metav1.ConditionFalse, cond.Status)
		require.NotNil(t, cond.Message)
		assert.Contains(t, *cond.Message, "malformed patch-contribution inventory")
	})

	t.Run("Patch contribution removed between reconciles is released and ledger cleared", func(t *testing.T) {
		inst := newInstanceObject("demo", "default")
		contribs := []executor.Contribution{
			{
				APIVersion:   "v1",
				Kind:         "ConfigMap",
				Namespace:    "default",
				Name:         "target-cm",
				FieldManager: "fm-old-patch",
			},
		}
		rawJSON, err := controllergraph.MarshalContributions(contribs)
		require.NoError(t, err)
		anns := inst.GetAnnotations()
		if anns == nil {
			anns = map[string]string{}
		}
		anns[metadata.PatchContributionsAnnotation] = rawJSON
		inst.SetAnnotations(anns)

		targetCM := newConfigMapObject("target-cm", "default")
		fakeRuntimeCl := newFakeRuntimeClient(t, targetCM)
		raw := newControllerTestDynamicClient(t, inst.DeepCopy())

		c, _ := newGraphEngineControllerUnderTest(t, raw, testEmptyRGDSpec(), revisions.RevisionStateActive, comp, fakeRuntimeCl)

		watcher := &fakeInstanceWatcher{}
		err = c.reconcileViaGraphEngine(context.Background(), inst, watcher)
		require.NoError(t, err)

		stored := getStoredParentObject(t, raw)
		storedContribs, err := controllergraph.ReadContributions(stored)
		require.NoError(t, err)
		assert.Empty(t, storedContribs, "pruned patch contribution should be removed from ledger")
	})

	t.Run("Patch release failure keeps union in ledger and returns error", func(t *testing.T) {
		inst := newInstanceObject("demo", "default")
		contribs := []executor.Contribution{
			{
				APIVersion:   "v1",
				Kind:         "ConfigMap",
				Namespace:    "default",
				Name:         "target-cm",
				FieldManager: "fm-old-patch",
			},
		}
		rawJSON, err := controllergraph.MarshalContributions(contribs)
		require.NoError(t, err)
		anns := inst.GetAnnotations()
		if anns == nil {
			anns = map[string]string{}
		}
		anns[metadata.PatchContributionsAnnotation] = rawJSON
		inst.SetAnnotations(anns)

		fakeRuntimeCl := &errorClient{
			Client:   newFakeRuntimeClient(t),
			patchErr: errors.New("release error"),
		}
		raw := newControllerTestDynamicClient(t, inst.DeepCopy())

		c, _ := newGraphEngineControllerUnderTest(t, raw, testEmptyRGDSpec(), revisions.RevisionStateActive, comp, fakeRuntimeCl)

		watcher := &fakeInstanceWatcher{}
		err = c.reconcileViaGraphEngine(context.Background(), inst, watcher)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "release error")

		stored := getStoredParentObject(t, raw)
		storedContribs, err := controllergraph.ReadContributions(stored)
		require.NoError(t, err)
		assert.Len(t, storedContribs, 1, "unreleased patch contribution must be retained in ledger")
	})

	t.Run("Duplicate applied identities yield hardErr, ResourcesNotReady, and degraded Error state", func(t *testing.T) {
		inst := newInstanceObject("demo", "default")
		raw := newControllerTestDynamicClient(t, inst.DeepCopy())

		// Construct an RGD spec with two distinct nodes that render the same object (same GVK, ns, name)
		spec := &v1alpha1.ResourceGraphDefinitionSpec{
			Schema: &v1alpha1.Schema{
				APIVersion: "v1alpha1",
				Kind:       "DuplicateApp",
				Group:      "kro.run",
			},
			Resources: []*v1alpha1.Resource{
				{
					ID: "cm1",
					Template: apimachineryruntime.RawExtension{
						Raw: []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"shared-cm"}}`),
					},
				},
				{
					ID: "cm2",
					Template: apimachineryruntime.RawExtension{
						Raw: []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"shared-cm"}}`),
					},
				},
			},
		}

		fakeRuntimeCl := newFakeRuntimeClient(t)
		c, _ := newGraphEngineControllerUnderTest(t, raw, spec, revisions.RevisionStateActive, comp, fakeRuntimeCl)

		watcher := &fakeInstanceWatcher{}
		err := c.reconcileViaGraphEngine(context.Background(), inst, watcher)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "found resources with conflicts")

		stored := getStoredParentObject(t, raw)
		cond := conditionByType(t, stored, ResourcesReady)
		assert.Equal(t, metav1.ConditionFalse, cond.Status)
		require.NotNil(t, cond.Message)
		assert.Contains(t, *cond.Message, "duplicate resource in graph")

		status, _, _ := unstructured.NestedMap(stored.Object, "status")
		require.NotNil(t, status)
		assert.Equal(t, string(v1alpha1.InstanceStateError), status["state"])
	})

	t.Run("Pre-apply applyset union failure causes delayed requeue", func(t *testing.T) {
		inst := newInstanceObject("demo", "default")
		metadata.SetInstanceFinalizer(inst)
		inst.SetLabels(metadata.NewInstanceLabeler(inst, true).Labels())
		// Set malformed inventory annotation to cause applier.Union to fail
		anns := map[string]string{
			applyset.ApplySetGKsAnnotation: "invalid.group.with.bad.chars!/Kind",
		}
		inst.SetAnnotations(anns)
		raw := newControllerTestDynamicClient(t, inst.DeepCopy())
		spec := testRGDSpecWithConfigMap("app-config", "")
		fakeRuntimeCl := newFakeRuntimeClient(t)
		c, _ := newGraphEngineControllerUnderTest(t, raw, spec, revisions.RevisionStateActive, comp, fakeRuntimeCl)

		watcher := &fakeInstanceWatcher{}
		err := c.reconcileViaGraphEngine(context.Background(), inst, watcher)
		require.Error(t, err)
		assert.True(t, requeue.IsRequeueError(err))
		assert.Contains(t, err.Error(), "pre-apply applyset union failed")
	})

	t.Run("candidateMetadata includes conditional nodes without poisoning IsIgnored", func(t *testing.T) {
		inst := newInstanceObject("demo", "default")
		raw := newControllerTestDynamicClient(t, inst.DeepCopy())

		// Construct an RGD spec with a conditional node whose includeWhen depends on upstream node (unresolved at candidateMetadata time)
		spec := &v1alpha1.ResourceGraphDefinitionSpec{
			Schema: &v1alpha1.Schema{
				APIVersion: "v1alpha1",
				Kind:       "CondApp",
				Group:      "kro.run",
			},
			Resources: []*v1alpha1.Resource{
				{
					ID: "cm1",
					Template: apimachineryruntime.RawExtension{
						Raw: []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"cm1"}}`),
					},
				},
				{
					ID: "cm2",
					IncludeWhen: []string{
						`${cm1.metadata.name == "cm1"}`,
					},
					Template: apimachineryruntime.RawExtension{
						Raw: []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"cm2"}}`),
					},
				},
			},
		}

		fakeRuntimeCl := newFakeRuntimeClient(t)
		c, _ := newGraphEngineControllerUnderTest(t, raw, spec, revisions.RevisionStateActive, comp, fakeRuntimeCl)

		watcher := &fakeInstanceWatcher{}
		err := c.reconcileViaGraphEngine(context.Background(), inst, watcher)
		require.NoError(t, err)

		stored := getStoredParentObject(t, raw)
		cond := conditionByType(t, stored, ResourcesReady)
		assert.Equal(t, metav1.ConditionTrue, cond.Status)
	})
}
