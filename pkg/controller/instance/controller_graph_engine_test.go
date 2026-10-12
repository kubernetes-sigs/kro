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
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
		registry.ResolverFor(controllerTestParentGVR.Resource),
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

// managedFieldsInjectingClient stamps a managedFields entry under `manager`
// onto every object it GETs (the fake client strips managedFields).
type managedFieldsInjectingClient struct {
	client.Client
	manager string
}

func (m *managedFieldsInjectingClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if err := m.Client.Get(ctx, key, obj, opts...); err != nil {
		return err
	}
	obj.SetManagedFields([]metav1.ManagedFieldsEntry{{Manager: m.manager, Operation: metav1.ManagedFieldsOperationApply}})
	return nil
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
			obj := &unstructured.Unstructured{Object: map[string]any{}}
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

		wireStatus := map[string]any{
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
		wireStatus := map[string]any{
			"state": string(v1alpha1.InstanceStateInProgress),
			"conditions": []any{
				map[string]any{
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
		assert.Contains(t, err.Error(), "cannot install finalizer with invalid applyset inventory")
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

	// A field-manager conflict (here the cross-engine guard refusing a template
	// object owned by a standalone Graph) stays soft, but the message must say so
	// instead of the generic "waiting for unresolved resource".
	t.Run("Executor returns field-manager conflict -> ResourcesReady False (NotReady) with a field-manager-conflict message, state InProgress", func(t *testing.T) {
		inst := newInstanceObject("demo", "default")
		raw := newControllerTestDynamicClient(t, inst.DeepCopy())
		existing := newConfigMapObject("app-config", "default")
		fakeRuntimeCl := &managedFieldsInjectingClient{
			Client: newFakeRuntimeClient(t, existing),
			// A standalone Graph's template manager: "kro-graphengine.tmpl.<graphSegment>".
			manager: "kro-graphengine.tmpl.d2ba416cfd76",
		}
		spec := testRGDSpecWithConfigMap("app-config", "")
		c, _ := newGraphEngineControllerUnderTest(t, raw, spec, revisions.RevisionStateActive, comp, fakeRuntimeCl)

		watcher := &fakeInstanceWatcher{}
		err := c.reconcileViaGraphEngine(context.Background(), inst, watcher)
		require.Error(t, err)
		assert.True(t, requeue.IsRequeueError(err), "a field-manager conflict stays a soft requeue")
		assert.True(t, errors.Is(err, executor.ErrNotReady))
		assert.True(t, errors.Is(err, executor.ErrFieldManagerConflict))

		stored := getStoredParentObject(t, raw)
		cond := conditionByType(t, stored, ResourcesReady)
		assert.Equal(t, metav1.ConditionFalse, cond.Status)
		require.NotNil(t, cond.Reason)
		assert.Equal(t, "NotReady", *cond.Reason)
		require.NotNil(t, cond.Message)
		assert.Contains(t, *cond.Message, "field manager conflict:",
			"the message must identify the contention instead of the generic readiness wait")
		assert.NotContains(t, *cond.Message, "waiting for unresolved resource")
		assert.Contains(t, *cond.Message, "owned by a foreign kro Graph", "the executor's detail is preserved")

		status, _, _ := unstructured.NestedMap(stored.Object, "status")
		require.NotNil(t, status)
		assert.Equal(t, string(v1alpha1.InstanceStateInProgress), status["state"])
	})

	t.Run("Executor returns typed ResourceDeletingError -> ResourcesReady False (ResourceDeleting), state InProgress", func(t *testing.T) {
		inst := newInstanceObject("demo", "default")
		raw := newControllerTestDynamicClient(t, inst.DeepCopy())

		// Seed fakeRuntimeClient with a ConfigMap that has DeletionTimestamp set
		deletingCM := &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name":              "app-config",
					"namespace":         "default",
					"deletionTimestamp": time.Now().Format(time.RFC3339),
					"finalizers":        []any{"kro.run/test"},
				},
				"data": map[string]any{"key": "val"},
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

	// FINDING 2 regression (end-to-end): an UNRESOLVED node that owns no managed
	// resource must not veto pruning of resources owned by OTHER nodes. Here an
	// externalRef (a read-only ref node) points at an absent object, so it stays
	// unresolved every cycle. A Deployment orphan — owned by a template node no
	// longer in the graph — must still be pruned. Before the fix (pruneGate fed
	// the raw Unresolved set) the ownerless ref vetoes every prune and the orphan
	// survives forever; after the fix ownedUnresolved drops it and the orphan is
	// deleted.
	t.Run("unresolved ownerless ref node does not veto pruning of another node's orphan", func(t *testing.T) {
		inst := newInstanceObject("demo", "default")
		addDeletionScope(inst, controllerTestDeployGVK, "default")

		orphanDeploy := newManagedObject(newDeploymentObject("orphan-deploy", "default"), inst, "deploy", 1)

		raw := newControllerTestDynamicClient(t, inst.DeepCopy(), orphanDeploy)
		fakeRuntimeCl := newFakeRuntimeClient(t)

		// Graph: one owning ConfigMap template + one externalRef to an ABSENT
		// ConfigMap. The ref node owns nothing and stays Unresolved (target not
		// found), while the Deployment orphan is no longer produced by any node.
		spec := &v1alpha1.ResourceGraphDefinitionSpec{
			Schema: &v1alpha1.Schema{
				APIVersion: "v1alpha1",
				Kind:       "WebApp",
				Group:      "kro.run",
				Spec:       apimachineryruntime.RawExtension{Raw: []byte(`{}`)},
			},
			Resources: []*v1alpha1.Resource{
				{
					ID: "cm",
					Template: apimachineryruntime.RawExtension{
						Raw: []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"cm-1","namespace":"default"}}`),
					},
				},
				{
					ID: "existing",
					ExternalRef: &v1alpha1.ExternalRef{
						APIVersion: "v1",
						Kind:       "ConfigMap",
						Metadata:   v1alpha1.ExternalRefMetadata{Name: "absent-cm", Namespace: "default"},
					},
				},
			},
		}
		c, _ := newGraphEngineControllerUnderTest(t, raw, spec, revisions.RevisionStateActive, comp, fakeRuntimeCl)

		watcher := &fakeInstanceWatcher{}
		// The absent externalRef holds the instance not-ready (soft requeue), but
		// pruning of the unrelated orphan must proceed on this same cycle.
		err := c.reconcileViaGraphEngine(context.Background(), inst, watcher)
		if err != nil {
			assert.True(t, requeue.IsRequeueError(err), "absent externalRef should only soft-requeue, got %v", err)
		}

		_, getErr := raw.Tracker().Get(controllerTestDeployGVR, "default", "orphan-deploy")
		assert.Error(t, getErr, "orphan-deploy owned by a removed template node must be pruned despite the unresolved ownerless ref node")
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
		// FINDING 663: a prune UID conflict leaves the orphan in place and the
		// inventory unshrunk, so the reconcile must soft-requeue to retry rather
		// than return clean success (which would strand the conflict until an
		// unrelated event). It is a requeue error, not a hard error.
		require.Error(t, err)
		assert.True(t, requeue.IsRequeueError(err), "a prune UID conflict must soft-requeue, got %v", err)

		stored := getStoredParentObject(t, raw)
		// Superset inventory preserved (the orphan's GroupKind is retained so a
		// later cycle can retry the prune).
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

func TestReconcileViaGraphEngine_PartialPruningBoundaries(t *testing.T) {
	for _, priorID := range []string{"entries", "renamedAway"} {
		t.Run(priorID, func(t *testing.T) {
			inst := newInstanceObject("demo", "default")
			inst.Object["spec"] = map[string]any{"ready": "false", "values": []any{"a1"}, "retiredValues": []any{}}
			gate := newManagedObject(newConfigMapObject("gate-new", "default"), inst, "gate", 1)
			oldGate := newManagedObject(newConfigMapObject("gate-old", "default"), inst, "gate", 1)
			retired := newManagedObject(newConfigMapObject("retired", "default"), inst, "cms", 1)
			entry1 := newManagedObject(newConfigMapObject("entry-a1", "default"), inst, priorID, 2)
			entry2 := newManagedObject(newConfigMapObject("entry-a2", "default"), inst, priorID, 2)
			skipped := newManagedObject(newConfigMapObject("skipped", "retained-ns"), inst, "skipped", 1)
			defMember := newManagedObject(newConfigMapObject("def-member", "default"), inst, "schema", 1)
			refMember := newManagedObject(newConfigMapObject("ref-member", "default"), inst, "existing", 1)
			addDeletionScope(inst, controllerTestDeployGVK, "default")
			for _, obj := range []*unstructured.Unstructured{gate, oldGate, retired, entry1, entry2, skipped, defMember, refMember} {
				obj.SetUID(types.UID(obj.GetName() + "-uid"))
			}
			raw := newControllerTestDynamicClient(t, inst.DeepCopy(), gate, oldGate, retired, entry1, entry2, skipped, defMember, refMember)
			runtimeClient := newFakeRuntimeClient(t, gate, entry1, entry2)
			spec := testEmptyRGDSpec()
			spec.Schema.Spec.Raw = []byte(`{"ready":"string","values":"[]string","retiredValues":"[]string"}`)
			spec.Resources = []*v1alpha1.Resource{
				{
					ID: "gate", ReadyWhen: []string{"${gate.data.ready == 'true'}"},
					Template: apimachineryruntime.RawExtension{Raw: []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"gate-new","namespace":"default"},"data":{"ready":"${schema.spec.ready}"}}`)},
				},
				{
					ID: "entries", ForEach: []v1alpha1.ForEachDimension{{"v": "${schema.spec.values}"}},
					Template: apimachineryruntime.RawExtension{Raw: []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"entry-${v}","namespace":"default"},"data":{"ready":"${gate.data.ready}"}}`)},
				},
				{
					ID: "cms", ForEach: []v1alpha1.ForEachDimension{{"v": "${schema.spec.retiredValues}"}},
					Template: apimachineryruntime.RawExtension{Raw: []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"retired-${v}","namespace":"default"}}`)},
				},
				{
					ID: "skipped", IncludeWhen: []string{"${false}"},
					Template: apimachineryruntime.RawExtension{Raw: []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"skipped","namespace":"retained-ns"}}`)},
				},
				{
					ID:          "existing",
					ExternalRef: &v1alpha1.ExternalRef{APIVersion: "v1", Kind: "ConfigMap", Metadata: v1alpha1.ExternalRefMetadata{Name: "gate-new", Namespace: "default"}},
				},
			}
			c, _ := newGraphEngineControllerUnderTest(t, raw, spec, revisions.RevisionStateActive, newTestRealCompiler(t), runtimeClient)

			// Repeat the partial cycle: the second pass has only retained candidates.
			for range 2 {
				err := c.reconcileViaGraphEngine(t.Context(), inst, &fakeInstanceWatcher{})
				require.ErrorIs(t, err, executor.ErrNotReady)
				for _, name := range []string{"retired", "gate-old"} {
					_, err := raw.Tracker().Get(controllerTestCMGVR, "default", name)
					assert.True(t, apierrors.IsNotFound(err), "completed empty and nonempty templates may prune: %s: %v", name, err)
				}
				for _, obj := range []*unstructured.Unstructured{gate, entry1, entry2, skipped, defMember, refMember} {
					stored, err := raw.Tracker().Get(controllerTestCMGVR, obj.GetNamespace(), obj.GetName())
					require.NoError(t, err)
					assert.Equal(t, obj.GetUID(), stored.(*unstructured.Unstructured).GetUID())
				}
				inst = getStoredParentObject(t, raw)
				require.NoError(t, applyset.ValidateParentInventory(inst))
				assert.Equal(t, "ConfigMap,Deployment.apps", inst.GetAnnotations()[applyset.ApplySetGKsAnnotation])
				assert.Equal(t, "retained-ns", inst.GetAnnotations()[applyset.ApplySetAdditionalNamespacesAnnotation])
			}

			// Once the dependency is ready, the requested reduction and skipped-node
			// retirement take effect. A renamed node reuses the surviving member's UID.
			require.NoError(t, unstructured.SetNestedField(inst.Object, "true", "spec", "ready"))
			require.NoError(t, raw.Tracker().Update(controllerTestParentGVR, inst.DeepCopy(), "default"))
			require.NoError(t, c.reconcileViaGraphEngine(t.Context(), inst, &fakeInstanceWatcher{}))
			kept, err := raw.Tracker().Get(controllerTestCMGVR, "default", "entry-a1")
			require.NoError(t, err)
			assert.Equal(t, entry1.GetUID(), kept.(*unstructured.Unstructured).GetUID())
			_, err = raw.Tracker().Get(controllerTestCMGVR, "default", "entry-a2")
			assert.True(t, apierrors.IsNotFound(err))
			_, err = raw.Tracker().Get(controllerTestCMGVR, "retained-ns", "skipped")
			assert.True(t, apierrors.IsNotFound(err))
			stored := getStoredParentObject(t, raw)
			assert.Equal(t, "ConfigMap", stored.GetAnnotations()[applyset.ApplySetGKsAnnotation])
			assert.Empty(t, stored.GetAnnotations()[applyset.ApplySetAdditionalNamespacesAnnotation])
			require.NoError(t, applyset.ValidateParentInventory(stored))
		})
	}
}

func TestReconcileViaGraphEngine_PartialPruneErrors(t *testing.T) {
	spec := testEmptyRGDSpec()
	spec.Schema.Spec.Raw = []byte(`{"values":"[]string"}`)
	spec.Resources = []*v1alpha1.Resource{
		{
			ID: "cms", ForEach: []v1alpha1.ForEachDimension{{"v": "${schema.spec.values}"}},
			Template: apimachineryruntime.RawExtension{Raw: []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"cp-${v}","namespace":"default"}}`)},
		},
		{
			ID:       "summary",
			Template: apimachineryruntime.RawExtension{Raw: []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"summary","namespace":"default"},"data":{"first":"${cms[0].metadata.name}"}}`)},
		},
	}
	for _, tc := range []struct {
		name, verb, wantMessage string
		pruneErr                error
		soft                    bool
	}{
		{name: "hard delete failure overrides soft apply", verb: "delete", wantMessage: "delete denied by policy", pruneErr: apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, "retired", errors.New("delete denied by policy"))},
		{name: "hard list failure overrides soft apply", verb: "list", wantMessage: "list denied by policy", pruneErr: errors.New("list denied by policy")},
		{name: "UID conflict preserves soft apply message", verb: "delete", wantMessage: "summary", pruneErr: apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, "retired", errors.New("UID mismatch")), soft: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inst := newInstanceObject("demo", "default")
			inst.SetGeneration(7)
			inst.Object["spec"] = map[string]any{"values": []any{}}
			retired := newManagedObject(newConfigMapObject("retired", "default"), inst, "cms", 1)
			summary := newManagedObject(newConfigMapObject("summary", "default"), inst, "summary", 2)
			addDeletionScope(inst, controllerTestDeployGVK, "retained-ns")
			raw := newControllerTestDynamicClient(t, inst.DeepCopy(), retired, summary)
			raw.PrependReactor(tc.verb, "configmaps", func(action k8stesting.Action) (bool, apimachineryruntime.Object, error) {
				if action.GetVerb() == "delete" {
					deletion := action.(k8stesting.DeleteAction)
					assert.Equal(t, "retired", deletion.GetName(), "unresolved summary must never be targeted")
					require.NotNil(t, deletion.GetDeleteOptions().Preconditions)
					assert.Equal(t, new(retired.GetUID()), deletion.GetDeleteOptions().Preconditions.UID)
				}
				return true, nil, tc.pruneErr
			})
			c, _ := newGraphEngineControllerUnderTest(t, raw, spec, revisions.RevisionStateActive, newTestRealCompiler(t), newFakeRuntimeClient(t))
			err := c.reconcileViaGraphEngine(t.Context(), inst, &fakeInstanceWatcher{})
			require.Error(t, err)
			assert.True(t, requeue.IsRequeueError(err))
			assert.Equal(t, tc.soft, errors.Is(err, executor.ErrNotReady))
			assert.Contains(t, err.Error(), tc.wantMessage)
			attempted := false
			for _, action := range raw.Actions() {
				attempted = attempted || action.Matches(tc.verb, "configmaps")
			}
			assert.True(t, attempted, "prune failure must be exercised")
			stored := getStoredParentObject(t, raw)
			condition := conditionByType(t, stored, ResourcesReady)
			assert.Equal(t, metav1.ConditionFalse, condition.Status)
			assert.Equal(t, inst.GetGeneration(), condition.ObservedGeneration)
			require.NotNil(t, condition.Message)
			assert.Contains(t, *condition.Message, tc.wantMessage)
			if tc.soft {
				assert.NotContains(t, *condition.Message, "prune of retired resources failed")
			} else {
				assert.Contains(t, *condition.Message, "prune of retired resources failed")
			}
			assert.Equal(t, string(v1alpha1.InstanceStateInProgress), stored.Object["status"].(map[string]any)["state"])
			assert.Equal(t, "ConfigMap,Deployment.apps", stored.GetAnnotations()[applyset.ApplySetGKsAnnotation])
			assert.Equal(t, "retained-ns", stored.GetAnnotations()[applyset.ApplySetAdditionalNamespacesAnnotation])
			require.NoError(t, applyset.ValidateParentInventory(stored))
			for _, obj := range []*unstructured.Unstructured{retired, summary} {
				live, err := raw.Tracker().Get(controllerTestCMGVR, "default", obj.GetName())
				require.NoError(t, err)
				assert.Equal(t, obj.GetUID(), live.(*unstructured.Unstructured).GetUID())
			}
		})
	}
}

func TestReconcileViaGraphEngine_InventoryErrorPrecedence(t *testing.T) {
	for _, hardApply := range []bool{false, true} {
		t.Run(fmt.Sprintf("hardApply=%v", hardApply), func(t *testing.T) {
			inst := newInstanceObject("demo", "default")
			retired := newManagedObject(newConfigMapObject("retired", "default"), inst, "late", 1)
			input := newConfigMapObject("input", "default")
			input.Object["data"] = map[string]any{"namespace": "late-ns"}
			spec := testEmptyRGDSpec()
			spec.Resources = []*v1alpha1.Resource{
				{
					ID:          "input",
					ExternalRef: &v1alpha1.ExternalRef{APIVersion: "v1", Kind: "ConfigMap", Metadata: v1alpha1.ExternalRefMetadata{Name: "input", Namespace: "default"}},
				},
				{
					ID:       "late",
					Template: apimachineryruntime.RawExtension{Raw: []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"late","namespace":"${input.data.namespace}"}}`)},
				},
				{
					ID:       "blocked",
					Template: apimachineryruntime.RawExtension{Raw: []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"blocked","namespace":"default"},"data":{"value":"${input.data.absent}"}}`)},
				},
			}
			wantMessage := "inventory growth denied"
			wantState := v1alpha1.InstanceStateInProgress
			if hardApply {
				duplicate := spec.Resources[1].DeepCopy()
				duplicate.ID = "duplicate"
				spec.Resources = append(spec.Resources, duplicate)
				wantMessage = "duplicate resource identity across nodes"
				wantState = v1alpha1.InstanceStateError
			}
			raw := newControllerTestDynamicClient(t, inst.DeepCopy(), retired)
			growAttempted := false
			raw.PrependReactor("patch", "webapps", func(action k8stesting.Action) (bool, apimachineryruntime.Object, error) {
				patch := &unstructured.Unstructured{}
				require.NoError(t, patch.UnmarshalJSON(action.(k8stesting.PatchAction).GetPatch()))
				if strings.Contains(patch.GetAnnotations()[applyset.ApplySetAdditionalNamespacesAnnotation], "late-ns") {
					growAttempted = true
					return true, nil, errors.New("inventory growth denied")
				}
				return false, nil, nil
			})
			c, _ := newGraphEngineControllerUnderTest(t, raw, spec, revisions.RevisionStateActive, newTestRealCompiler(t), newFakeRuntimeClient(t, input))
			err := c.reconcileViaGraphEngine(t.Context(), inst, &fakeInstanceWatcher{})
			require.ErrorContains(t, err, wantMessage)
			assert.NotErrorIs(t, err, executor.ErrNotReady)
			assert.True(t, growAttempted, "ref-derived namespace must reach post-apply inventory growth")
			for _, action := range raw.Actions() {
				assert.NotEqual(t, "delete", action.GetVerb(), "inventory failure must veto deletion")
			}
			stored := getStoredParentObject(t, raw)
			condition := conditionByType(t, stored, ResourcesReady)
			require.NotNil(t, condition.Message)
			assert.Contains(t, *condition.Message, wantMessage)
			assert.Equal(t, string(wantState), stored.Object["status"].(map[string]any)["state"])
			assert.Equal(t, "ConfigMap", stored.GetAnnotations()[applyset.ApplySetGKsAnnotation])
			live, err := raw.Tracker().Get(controllerTestCMGVR, "default", "retired")
			require.NoError(t, err)
			assert.Equal(t, retired.GetUID(), live.(*unstructured.Unstructured).GetUID())
		})
	}
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

	meta, _ := c.candidateMetadata(rt, inst)
	assert.True(t, meta.GroupKinds.Has(schema.GroupKind{Group: "", Kind: "ConfigMap"}))
	assert.True(t, meta.AdditionalNamespaces.Has("custom-ns"))
}

// TestCandidateMetadata_SkipsIgnoredNodes is the FINDING 1 regression: a
// template node skipped this reconcile (includeWhen evaluates to false) must
// NOT contribute its GroupKind to the pre-apply inventory superset. If it did,
// reconcileApplySetInventory would align the inventory DOWN to the exact
// applied batch (which excludes the skipped node), removing the GroupKind on
// every cycle — a watch-event write that re-enqueues the instance and fights
// the pre-apply writer in a permanent loop (~20 writes/sec observed).
//
// Before the fix (candidateMetadata not checking IsIgnored) the skipped node's
// GroupKind is present and this test fails; after the fix it is absent.
func TestCandidateMetadata_SkipsIgnoredNodes(t *testing.T) {
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
					ID: "kept",
					Template: apimachineryruntime.RawExtension{
						Raw: []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"kept-cm"}}`),
					},
				},
				{
					ID: "skipped",
					// includeWhen is a constant false, so this node is ignored
					// this reconcile and owns no cluster resource.
					IncludeWhen: []string{`${1 == 2}`},
					Template: apimachineryruntime.RawExtension{
						Raw: []byte(`{"apiVersion":"v1","kind":"ResourceQuota","metadata":{"name":"skipped-rq"}}`),
					},
				},
			},
		},
	}
	rt, _, err := rgdadapter.BuildRuntimeForInstance(rgd, inst, comp)
	require.NoError(t, err)

	raw := newControllerTestDynamicClient(t, inst.DeepCopy())
	c, _ := newGraphEngineControllerUnderTest(t, raw, testEmptyRGDSpec(), revisions.RevisionStateActive, comp, nil)

	meta, _ := c.candidateMetadata(rt, inst)
	assert.True(t, meta.GroupKinds.Has(schema.GroupKind{Group: "", Kind: "ConfigMap"}),
		"included node's GroupKind must be in the candidate inventory")
	assert.False(t, meta.GroupKinds.Has(schema.GroupKind{Group: "", Kind: "ResourceQuota"}),
		"skipped (includeWhen:false) node must NOT contribute its GroupKind to the candidate inventory")
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

	t.Run("partial cycles grow before pruning and never shrink", func(t *testing.T) {
		for _, tc := range []struct {
			name       string
			candidates bool
			completed  sets.Set[string]
			failGrow   bool
			wantPruned bool
		}{
			{name: "prune after growth", candidates: true, completed: sets.New("cms"), wantPruned: true},
			{name: "no candidates", completed: sets.New("cms")},
			{name: "all candidates retained", candidates: true, completed: sets.New("other")},
			{name: "no eligible templates", candidates: true},
			{name: "growth failure", candidates: true, completed: sets.New("cms"), failGrow: true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				inst := newInstanceObject("demo", "default")
				retired := newManagedObject(newDeploymentObject("retired", "default"), inst, "cms", 1)
				retained := newManagedObject(newDeploymentObject("retained", "retained-ns"), inst, "summary", 2)
				objects := []apimachineryruntime.Object{inst.DeepCopy()}
				if tc.candidates {
					objects = append(objects, retired, retained)
				}
				raw := newControllerTestDynamicClient(t, objects...)
				c, clientSet := newGraphEngineControllerUnderTest(t, raw, testEmptyRGDSpec(), revisions.RevisionStateActive, comp, nil)
				applier := applyset.New(applyset.Config{Client: raw, RESTMapper: clientSet.RESTMapper(), ParentNamespace: "default", Log: c.log}, inst)
				superset, err := applier.Union(applyset.Metadata{})
				require.NoError(t, err)
				applied := []v1alpha1.ManagedResource{{NodeID: "late", APIVersion: "v1", Kind: "ConfigMap", Namespace: "late-ns", Name: "late", UID: "late-uid"}}
				deletes := 0
				raw.PrependReactor("delete", "deployments", func(action k8stesting.Action) (bool, apimachineryruntime.Object, error) {
					deletes++
					parent, err := raw.Tracker().Get(controllerTestParentGVR, "default", "demo")
					require.NoError(t, err)
					annotations := parent.(*unstructured.Unstructured).GetAnnotations()
					assert.Equal(t, "ConfigMap,Deployment.apps", annotations[applyset.ApplySetGKsAnnotation], "growth must be durable before DELETE")
					assert.Equal(t, "late-ns,retained-ns", annotations[applyset.ApplySetAdditionalNamespacesAnnotation])
					return false, nil, nil
				})
				if tc.failGrow {
					raw.PrependReactor("patch", "webapps", func(k8stesting.Action) (bool, apimachineryruntime.Object, error) {
						return true, nil, errors.New("inventory growth denied")
					})
				}
				err = c.reconcileApplySetInventory(t.Context(), c.log, inst, applier, applied, superset, false, tc.completed)
				if tc.failGrow {
					require.ErrorContains(t, err, "inventory growth denied")
				} else {
					require.NoError(t, err)
					stored := getStoredParentObject(t, raw)
					require.NoError(t, applyset.ValidateParentInventory(stored))
					assert.Equal(t, "ConfigMap,Deployment.apps", stored.GetAnnotations()[applyset.ApplySetGKsAnnotation])
					assert.Equal(t, "late-ns,retained-ns", stored.GetAnnotations()[applyset.ApplySetAdditionalNamespacesAnnotation])
				}
				if tc.wantPruned {
					assert.Equal(t, 1, deletes)
					_, err := raw.Tracker().Get(controllerTestDeployGVR, "default", "retired")
					assert.True(t, apierrors.IsNotFound(err))
				} else {
					assert.Zero(t, deletes)
				}
				if tc.candidates {
					live, err := raw.Tracker().Get(controllerTestDeployGVR, "retained-ns", "retained")
					require.NoError(t, err)
					assert.Equal(t, retained.GetUID(), live.(*unstructured.Unstructured).GetUID())
				}
			})
		}
	})

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

		err := c.reconcileApplySetInventory(context.Background(), c.log, inst, nil, applied, applyset.Metadata{}, true, nil)
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
		err := c.reconcileApplySetInventory(context.Background(), c.log, inst, nil, nil, applyset.Metadata{}, true, nil)
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

		err := c.reconcileApplySetInventory(context.Background(), c.log, inst, nil, applied, applyset.Metadata{}, true, nil)
		require.Error(t, err)
		assert.True(t, errors.Is(err, applyset.ErrDuplicateResource))
	})
}

func TestPruneGraphEngineOrphans_Direct(t *testing.T) {
	comp := newTestRealCompiler(t)

	t.Run("partial attribution uses canonical root paths with legacy fallback", func(t *testing.T) {
		longID := strings.Repeat("long", 17)
		inst := newInstanceObject("demo", "default")
		raw := newControllerTestDynamicClient(t, inst.DeepCopy())
		runtimeClient := newFakeRuntimeClient(t)
		longSpec := testRGDSpecWithConfigMap("annotated-long", "")
		longSpec.Resources[0].ID = longID
		c, _ := newGraphEngineControllerUnderTest(t, raw, longSpec, revisions.RevisionStateActive, comp, runtimeClient)
		require.NoError(t, c.reconcileViaGraphEngine(t.Context(), inst, &fakeInstanceWatcher{}))
		stampedLong := newConfigMapObject("annotated-long", "default")
		require.NoError(t, runtimeClient.Get(t.Context(), client.ObjectKeyFromObject(stampedLong), stampedLong))
		hashedLabel := stampedLong.GetLabels()[metadata.NodeIDLabel]
		require.Equal(t, metadata.NodeIDToken(longID), hashedLabel)
		require.Equal(t, longID, stampedLong.GetAnnotations()[metadata.NodePathAnnotation])
		cases := []struct {
			name, path, label string
			pruned            bool
		}{
			{name: "annotated", path: "cms", label: "cms", pruned: true},
			{name: "legacy", label: "cms", pruned: true},
			{name: "path-without-label", path: "cms", pruned: true},
			{name: "path-overrides-label", path: "cms", label: "summary", pruned: true},
			{name: "annotated-long", path: longID, label: hashedLabel, pruned: true},
			{name: "hash-only", label: hashedLabel},
			{name: "unattributed"},
			{name: "unknown-path", path: "removed", label: "cms"},
			{name: "renamed-away", label: "oldcms"},
			{name: "unresolved-path", path: "summary", label: "cms"},
			{name: "legacy-unresolved", label: "summary"},
			{name: "distinct-root", path: "summaryx", label: "summaryx", pruned: true},
			{name: "qualified-path", path: "cms/child", label: "cms"},
			{name: "qualified-label", label: "cms.child"},
		}
		for _, tc := range cases {
			base := newConfigMapObject(tc.name, "default")
			if tc.name == "annotated-long" {
				base = stampedLong
			}
			obj := newManagedObject(base, inst, tc.label, 1)
			obj.SetUID(types.UID(tc.name + "-uid"))
			if tc.name != "annotated-long" {
				annotations := obj.GetAnnotations()
				annotations[metadata.NodePathAnnotation] = tc.path
				obj.SetAnnotations(annotations)
			}
			require.NoError(t, raw.Tracker().Create(controllerTestCMGVR, obj, "default"))
		}
		require.NoError(t, c.reconcileApplySetInventory(t.Context(), c.log, inst, nil, nil, applyset.Metadata{}, false, sets.New("cms", "summaryx", longID)))
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				obj, err := raw.Resource(controllerTestCMGVR).Namespace("default").Get(t.Context(), tc.name, metav1.GetOptions{})
				if tc.pruned {
					require.True(t, apierrors.IsNotFound(err), "attributable retired member must be pruned: %v", err)
				} else {
					require.NoError(t, err)
					assert.Equal(t, types.UID(tc.name+"-uid"), obj.GetUID())
				}
			})
		}
		// Full resolution removes the remaining unknown and legacy members too.
		require.NoError(t, c.reconcileApplySetInventory(t.Context(), c.log, inst, nil, nil, applyset.Metadata{}, true, nil))
		remaining, err := raw.Resource(controllerTestCMGVR).Namespace("default").List(t.Context(), metav1.ListOptions{})
		require.NoError(t, err)
		assert.Empty(t, remaining.Items)
	})

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

		pruned, conflictFree, err := c.pruneGraphEngineOrphans(context.Background(), c.log, applier, applied, supersetMeta, sets.New("deploy"))
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
		_, _, err := c.pruneGraphEngineOrphans(context.Background(), c.log, applier, nil, supersetMeta, nil)
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
			Client:   newFakeRuntimeClient(t, newConfigMapObject("target-cm", "default")),
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

	t.Run("Duplicate rendered identities prevent child writes and persist failure status", func(t *testing.T) {
		inst := newInstanceObject("demo", "default")
		retired := newManagedObject(newConfigMapObject("retired", "default"), inst, "cm1", 1)
		raw := newControllerTestDynamicClient(t, inst.DeepCopy(), retired)

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
		assert.Contains(t, err.Error(), "cm1")
		assert.Contains(t, err.Error(), "cm2")
		assert.True(t, requeue.IsRequeueError(err))

		cm := newConfigMapObject("shared-cm", "default")
		assert.True(t, apierrors.IsNotFound(fakeRuntimeCl.Get(context.Background(), client.ObjectKeyFromObject(cm), cm)),
			"the duplicate must be rejected before either child is created")
		for _, action := range raw.Actions() {
			assert.NotEqual(t, "delete", action.GetVerb(), "hard duplicate-identity failure must veto pruning")
		}
		_, getErr := raw.Tracker().Get(controllerTestCMGVR, "default", "retired")
		require.NoError(t, getErr)

		stored := getStoredParentObject(t, raw)
		cond := conditionByType(t, stored, ResourcesReady)
		assert.Equal(t, metav1.ConditionFalse, cond.Status)
		require.NotNil(t, cond.Message)
		assert.Contains(t, *cond.Message, "cm1")
		assert.Contains(t, *cond.Message, "cm2")

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

func TestReconcileViaGraphEngine_DuplicateIdentityCollections(t *testing.T) {
	for _, overlap := range []bool{false, true} {
		t.Run(fmt.Sprintf("scalarOverlap=%t", overlap), func(t *testing.T) {
			inst := newInstanceObject("demo", "default")
			spec := testRGDSpecWithConfigMap("shared-cm", "")
			spec.Resources[0].ForEach = []v1alpha1.ForEachDimension{{"ns": `${['', schema.metadata.namespace]}`}}
			spec.Resources[0].Template.Raw = []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"shared-cm","namespace":"${ns}"}}`)
			if overlap {
				// A resolved collection row must also be checked against scalar nodes.
				spec.Resources[0].ForEach = []v1alpha1.ForEachDimension{{"ns": `${[schema.metadata.namespace]}`}}
				spec.Resources = append(spec.Resources, &v1alpha1.Resource{
					ID:       "scalar",
					Template: apimachineryruntime.RawExtension{Raw: []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"shared-cm"}}`)},
				})
			}
			raw := newControllerTestDynamicClient(t, inst.DeepCopy())
			cl := newFakeRuntimeClient(t)
			c, _ := newGraphEngineControllerUnderTest(t, raw, spec, revisions.RevisionStateActive, newTestRealCompiler(t), cl)
			err := c.reconcileViaGraphEngine(t.Context(), inst, &fakeInstanceWatcher{})
			require.Error(t, err)
			cm := newConfigMapObject("shared-cm", "default")
			assert.True(t, apierrors.IsNotFound(cl.Get(t.Context(), client.ObjectKeyFromObject(cm), cm)),
				"no row may write before namespace-defaulted duplicates are rejected")
			assert.Equal(t, metav1.ConditionFalse, conditionByType(t, getStoredParentObject(t, raw), ResourcesReady).Status)
		})
	}
}

func TestReconcileViaGraphEngine_DuplicateIdentityStatusAndInventory(t *testing.T) {
	for _, authorConditions := range []bool{false, true} {
		t.Run(fmt.Sprintf("authorConditions=%t", authorConditions), func(t *testing.T) {
			inst := newInstanceObject("demo", "default")
			inst.SetGeneration(1)
			mark := NewConditionsMarkerFor(inst)
			mark.InstanceManaged()
			mark.GraphResolved()
			mark.ResourcesReady()
			inst.Object["status"].(map[string]any)["state"] = string(v1alpha1.InstanceStateActive)
			inst.Object["status"].(map[string]any)["endpoint"] = "previous"
			inst.SetGeneration(2)
			metadata.SetInstanceFinalizer(inst)

			orphan := newManagedObject(newDeploymentObject("retired", "other-ns"), inst, "retired", 1)
			contribs := []executor.Contribution{{APIVersion: "v1", Kind: "ConfigMap", Namespace: "default", Name: "prior-target", FieldManager: "prior-manager"}}
			ledger, err := controllergraph.MarshalContributions(contribs)
			require.NoError(t, err)
			priorAnnotations := inst.GetAnnotations()
			priorAnnotations[metadata.PatchContributionsAnnotation] = ledger
			inst.SetAnnotations(priorAnnotations)

			spec := testRGDSpecWithConfigMap("shared-cm", "")
			other := spec.Resources[0].DeepCopy()
			other.ID = "other"
			spec.Resources = append(spec.Resources, other)
			condType := ResourcesReady
			if authorConditions {
				condType = "AuthorReady"
				spec.Schema.Status.Raw = []byte(`{"conditions":["${runtime.newCondition({type: 'AuthorReady', status: runtime.condition(schema, 'ResourcesReady').status, reason: 'Projected'})}"]}`)
				inst.Object["status"].(map[string]any)["conditions"] = []any{map[string]any{
					"type": condType, "status": "True", "reason": "Previous", "observedGeneration": int64(1),
					"lastTransitionTime": "2026-01-01T00:00:00Z",
				}}
			}
			raw := newControllerTestDynamicClient(t, inst.DeepCopy(), orphan.DeepCopy())
			cl := newFakeRuntimeClient(t)
			c, _ := newGraphEngineControllerUnderTest(t, raw, spec, revisions.RevisionStateActive, newTestRealCompiler(t), cl)
			c.reconcileConfig.HasAuthorConditions = authorConditions

			require.Error(t, c.reconcileViaGraphEngine(t.Context(), inst, &fakeInstanceWatcher{}))
			stored := getStoredParentObject(t, raw)
			status := stored.Object["status"].(map[string]any)
			assert.Equal(t, string(v1alpha1.InstanceStateError), status["state"])
			assert.Equal(t, "previous", status["endpoint"])
			cond := conditionByType(t, stored, condType)
			assert.Equal(t, metav1.ConditionFalse, cond.Status)
			for _, entry := range status["conditions"].([]any) {
				condition := entry.(map[string]any)
				assert.Equal(t, int64(2), condition["observedGeneration"])
			}
			for key, value := range priorAnnotations {
				assert.Equal(t, value, stored.GetAnnotations()[key], "prior inventory %s", key)
			}
			live, err := raw.Tracker().Get(controllerTestDeployGVR, "other-ns", "retired")
			require.NoError(t, err)
			assert.Equal(t, orphan, live, "rejection must not prune prior managed resources")
		})
	}
}

func TestReconcileViaGraphEngine_DuplicateIdentityExcludesIgnored(t *testing.T) {
	for _, includeWhen := range []string{`${false}`, `${schema.spec.enabled}`} {
		t.Run(includeWhen, func(t *testing.T) {
			inst := newInstanceObject("demo", "default")
			inst.Object["spec"] = map[string]any{"enabled": false}
			spec := testRGDSpecWithConfigMap("shared-cm", "")
			spec.Schema.Spec.Raw = []byte(`{"enabled":"boolean"}`)
			skipped := spec.Resources[0].DeepCopy()
			skipped.ID = "skipped"
			skipped.IncludeWhen = []string{includeWhen}
			spec.Resources = append(spec.Resources, skipped)
			raw := newControllerTestDynamicClient(t, inst.DeepCopy())
			cl := newFakeRuntimeClient(t)
			c, _ := newGraphEngineControllerUnderTest(t, raw, spec, revisions.RevisionStateActive, newTestRealCompiler(t), cl)
			require.NoError(t, c.reconcileViaGraphEngine(t.Context(), inst, &fakeInstanceWatcher{}))
			cm := newConfigMapObject("shared-cm", "default")
			require.NoError(t, cl.Get(t.Context(), client.ObjectKeyFromObject(cm), cm))
			assert.Equal(t, "cm", cm.GetLabels()[metadata.NodeIDLabel])
		})
	}
}
