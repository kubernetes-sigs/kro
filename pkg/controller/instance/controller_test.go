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
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	apimachineryruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/cel/library"
	clientfake "github.com/kubernetes-sigs/kro/pkg/client/fake"
	"github.com/kubernetes-sigs/kro/pkg/graph"
	"github.com/kubernetes-sigs/kro/pkg/graph/revisions"
	"github.com/kubernetes-sigs/kro/pkg/graph/variable"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
	"github.com/kubernetes-sigs/kro/pkg/requeue"
)

func TestApplyManagedFinalizerAndLabels(t *testing.T) {
	tests := []struct {
		name            string
		presetFinalizer bool
		presetLabels    bool
		wantActions     int
		wantPatched     bool
	}{
		{
			name:            "no patch needed returns nil",
			presetFinalizer: true,
			presetLabels:    true,
		},
		{
			name:        "patches missing finalizer and labels",
			wantActions: 1,
			wantPatched: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			instance := newInstanceObject("demo", "default")
			if tt.presetFinalizer {
				metadata.SetInstanceFinalizer(instance)
			}
			if tt.presetLabels {
				metadata.NewKROMetaLabeler().ApplyLabels(instance)
			}

			controller, rcx, raw := newControllerAndContext(t, instance, newTestGraph())
			patched, err := controller.applyManagedFinalizerAndLabels(rcx)
			require.NoError(t, err)

			assert.Equal(t, tt.wantActions, len(raw.Actions()))
			if !tt.wantPatched {
				assert.Nil(t, patched, "no server write means nil, so callers don't rebind")
				patched = rcx.Instance
			} else {
				require.NotNil(t, patched)
			}
			assert.True(t, metadata.HasInstanceFinalizer(patched))
			for key, value := range metadata.NewKROMetaLabeler().Labels() {
				assert.Equal(t, value, patched.GetLabels()[key])
			}
		})
	}
}

func TestApplyManagedFinalizerAndLabelsError(t *testing.T) {
	instance := newInstanceObject("demo", "default")
	controller, rcx, raw := newControllerAndContext(t, instance, newTestGraph())
	raw.PrependReactor("patch", "webapps", func(action k8stesting.Action) (bool, apimachineryruntime.Object, error) {
		return true, nil, errors.New("patch failed")
	})

	_, err := controller.applyManagedFinalizerAndLabels(rcx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed applying managed finalizer/labels")
}

func TestEnsureManagedRefreshesInstanceState(t *testing.T) {
	instance := newInstanceObject("demo", "default")
	controller, rcx, _ := newControllerAndContext(t, instance, newTestGraph())

	require.NoError(t, controller.ensureManaged(rcx))
	assert.True(t, metadata.HasInstanceFinalizer(rcx.Instance))
	assert.Equal(t, metav1.ConditionTrue, conditionByType(t, rcx.Instance, InstanceManaged).Status)
}

func TestReconcileInstanceLoad(t *testing.T) {
	tests := []struct {
		name    string
		objects []apimachineryruntime.Object
		getErr  string
		request types.NamespacedName
		wantErr string
	}{
		{
			name:    "instance not found",
			request: types.NamespacedName{Name: "missing", Namespace: "default"},
		},
		{
			name:    "load errors are returned",
			objects: []apimachineryruntime.Object{newInstanceObject("demo", "default")},
			getErr:  "get failed",
			request: types.NamespacedName{Name: "demo", Namespace: "default"},
			wantErr: "get failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := newControllerTestDynamicClient(t, tt.objects...)
			if tt.getErr != "" {
				raw.PrependReactor("get", "webapps", func(action k8stesting.Action) (bool, apimachineryruntime.Object, error) {
					return true, nil, errors.New(tt.getErr)
				})
			}

			controller, _ := newControllerUnderTest(t, raw, newTestGraph())
			err := controller.Reconcile(context.Background(), ctrl.Request{NamespacedName: tt.request})

			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestReconcileStatusPaths(t *testing.T) {
	tests := []struct {
		name                string
		instanceAnnotations map[string]string
		wantState           string
		wantConditionType   string
		wantConditionStatus metav1.ConditionStatus
	}{
		{
			name:                "empty graph converges to active",
			wantState:           string(v1alpha1.InstanceStateActive),
			wantConditionType:   Ready,
			wantConditionStatus: metav1.ConditionTrue,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			instance := newInstanceObject("demo", "default")
			instance.SetAnnotations(tt.instanceAnnotations)

			raw := newControllerTestDynamicClient(t, instance.DeepCopy())
			controller, _ := newControllerUnderTest(t, raw, newTestGraph())
			err := controller.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{Name: instance.GetName(), Namespace: instance.GetNamespace()},
			})
			require.NoError(t, err)

			stored := getStoredParentObject(t, raw)
			state, found, err := unstructured.NestedString(stored.Object, "status", "state")
			require.NoError(t, err)
			require.True(t, found)
			assert.Equal(t, tt.wantState, state)
			assert.Equal(t, tt.wantConditionStatus, conditionByType(t, stored, tt.wantConditionType).Status)
		})
	}
}

func TestReconcileGraphResolutionFailureMarksCondition(t *testing.T) {
	tests := []struct {
		name        string
		entry       revisions.Entry
		hasLatest   bool
		wantReason  string
		wantMessage string
	}{
		{
			name:        "not available marks GraphResolved false",
			hasLatest:   false,
			wantReason:  "ResolutionFailed",
			wantMessage: "latest issued graph revision not available",
		},
		{
			name:      "pending revision marks GraphResolved false",
			hasLatest: true,
			entry: revisions.Entry{
				RGDName:  "webapps",
				Revision: 3,
				State:    revisions.RevisionStatePending,
			},
			wantReason:  "ResolutionFailed",
			wantMessage: "latest issued graph revision 3 is pending",
		},
		{
			name:      "failed revision marks GraphResolved false",
			hasLatest: true,
			entry: revisions.Entry{
				RGDName:  "webapps",
				Revision: 5,
				State:    revisions.RevisionStateFailed,
			},
			wantReason:  "ResolutionFailed",
			wantMessage: "latest issued graph revision 5 failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			instance := newInstanceObject("demo", "default")
			raw := newControllerTestDynamicClient(t, instance.DeepCopy())
			clientSet := clientfake.NewFakeSet(raw)
			clientSet.SetRESTMapper(buildControllerTestRESTMapper())

			controller := NewController(
				zap.New(zap.UseDevMode(true)),
				ReconcileConfig{DefaultRequeueDuration: 2 * time.Second},
				controllerTestParentGVR,
				testRevisionResolver{
					getLatestRevision: func() (revisions.Entry, bool) {
						return tt.entry, tt.hasLatest
					},
					getGraphRevision: func(int64) (revisions.Entry, bool) {
						return revisions.Entry{}, false
					},
				},
				true,
				clientSet,
				metadata.NewKROMetaLabeler(),
				metadata.NewKROMetaLabeler(),
				newControllerTestCoordinator(t),
				record.NewFakeRecorder(100),
			)

			err := controller.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{Name: "demo", Namespace: "default"},
			})
			require.Error(t, err)

			stored := getStoredParentObject(t, raw)
			cond := conditionByType(t, stored, GraphResolved)
			assert.Equal(t, metav1.ConditionFalse, cond.Status)
			require.NotNil(t, cond.Reason)
			assert.Equal(t, tt.wantReason, *cond.Reason)
			require.NotNil(t, cond.Message)
			assert.Contains(t, *cond.Message, tt.wantMessage)

			ready := conditionByType(t, stored, Ready)
			assert.Equal(t, metav1.ConditionFalse, ready.Status, "Ready should be false when GraphResolved is false")

			im := conditionByType(t, stored, InstanceManaged)
			assert.Equal(t, metav1.ConditionUnknown, im.Status, "InstanceManaged should be Unknown (never reached)")

			rr := conditionByType(t, stored, ResourcesReady)
			assert.Equal(t, metav1.ConditionUnknown, rr.Status, "ResourcesReady should be Unknown (never reached)")

			state, found, err := unstructured.NestedString(stored.Object, "status", "state")
			require.NoError(t, err)
			require.True(t, found)
			assert.Equal(t, string(v1alpha1.InstanceStateError), state)
		})
	}
}

func TestReconcileGraphResolveFailureKeepsOnlyAuthorConditionsOnWire(t *testing.T) {
	instance := newInstanceObject("demo", "default")

	authorCond := map[string]interface{}{
		"type":               "AuthorHealthy",
		"status":             "True",
		"reason":             "AuthorSaysHealthy",
		"message":            "author-written condition",
		"observedGeneration": int64(1),
		"lastTransitionTime": "2026-01-01T00:00:00Z",
	}
	require.NoError(t, unstructured.SetNestedField(instance.Object, "ACTIVE", "status", "state"))
	require.NoError(t, unstructured.SetNestedSlice(instance.Object, []interface{}{authorCond}, "status", "conditions"))

	raw := newControllerTestDynamicClient(t, instance.DeepCopy())
	clientSet := clientfake.NewFakeSet(raw)
	clientSet.SetRESTMapper(buildControllerTestRESTMapper())

	controller := NewController(
		zap.New(zap.UseDevMode(true)),
		ReconcileConfig{DefaultRequeueDuration: 2 * time.Second, HasAuthorConditions: true},
		controllerTestParentGVR,
		testRevisionResolver{
			getLatestRevision: func() (revisions.Entry, bool) {
				return revisions.Entry{}, false
			},
			getGraphRevision: func(int64) (revisions.Entry, bool) {
				return revisions.Entry{}, false
			},
		},
		true,
		clientSet,
		metadata.NewKROMetaLabeler(),
		metadata.NewKROMetaLabeler(),
		newControllerTestCoordinator(t),
		record.NewFakeRecorder(100),
	)

	err := controller.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "demo", Namespace: "default"},
	})
	require.Error(t, err)

	stored := getStoredParentObject(t, raw)
	conds, found, _ := unstructured.NestedSlice(stored.Object, "status", "conditions")
	require.True(t, found, "author's pre-existing condition must be preserved")

	types := map[string]bool{}
	for _, c := range conds {
		m := c.(map[string]interface{})
		types[m["type"].(string)] = true
	}

	assert.True(t, types["AuthorHealthy"], "author's AuthorHealthy must remain on the wire")

	assert.False(t, types["InstanceManaged"], "kro InstanceManaged must not appear on wire")
	assert.False(t, types["GraphResolved"], "kro GraphResolved must not appear on wire")
	assert.False(t, types["ResourcesReady"], "kro ResourcesReady must not appear on wire")
	assert.False(t, types["Ready"], "kro Ready must not appear on wire")

	assert.Equal(t, 1, len(conds), "wire holds only author conditions")

	state, _, _ := unstructured.NestedString(stored.Object, "status", "state")
	assert.Equal(t, "ERROR", state, "state flips to ERROR during graph-resolve failure")
}

// TestReconcileManagedFastPathDoesNotLeakBuiltins is a regression test for
// the already-managed reconcile path: with no finalizer/label patch needed,
// the wire snapshot must not be re-captured from the marker-mutated
// in-memory object, or an incomplete evaluation merges the built-ins onto
// the wire alongside the author's conditions.
func TestReconcileManagedFastPathDoesNotLeakBuiltins(t *testing.T) {
	instance := newInstanceObject("demo", "default")
	metadata.SetInstanceFinalizer(instance)
	metadata.NewKROMetaLabeler().ApplyLabels(instance)
	require.NoError(t, unstructured.SetNestedField(instance.Object, int64(1), "metadata", "generation"))
	require.NoError(t, unstructured.SetNestedSlice(instance.Object, []interface{}{
		map[string]interface{}{
			"type":               "Flaky",
			"status":             "True",
			"reason":             "SeenBefore",
			"lastTransitionTime": "2026-01-01T00:00:00Z",
		},
	}, "status", "conditions"))

	instanceNode := authorConditionsInstanceNode(t,
		mustCompileControllerExpr(t,
			`runtime.newCondition({type: 'Steady', status: 'True', reason: '', message: ''})`,
			library.RuntimeVarName,
		),
		mustCompileControllerExpr(t,
			`runtime.newCondition({type: 'Flaky', status: schema.spec.missing.value, reason: '', message: ''})`,
			library.RuntimeVarName, "schema",
		),
	)

	raw := newControllerTestDynamicClient(t, instance.DeepCopy())
	controller, _ := newControllerUnderTest(t, raw, newTestGraphWithInstance(instanceNode))

	require.NoError(t, controller.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "demo", Namespace: "default"},
	}))

	stored := getStoredParentObject(t, raw)
	byType := map[string]v1alpha1.Condition{}
	for _, c := range conditionsFromInstance(stored) {
		byType[string(c.Type)] = c
	}

	assert.Contains(t, byType, "Steady")
	require.Contains(t, byType, "Flaky", "the pending condition must be carried forward from the wire")
	require.NotNil(t, byType["Flaky"].LastTransitionTime)
	assert.Equal(t, "2026-01-01T00:00:00Z", byType["Flaky"].LastTransitionTime.UTC().Format(time.RFC3339))
	for _, builtin := range []string{InstanceManaged, GraphResolved, ResourcesReady, Ready} {
		assert.NotContains(t, byType, builtin,
			"marker-written built-ins must not leak onto the wire through the fast-path rebind")
	}
}

// TestReconcileManagedFastPathSkipsNoopStatusWrite verifies the skip-write
// guard fires through the full reconcile of an already-managed instance
// whose persisted status matches the computed one.
func TestReconcileManagedFastPathSkipsNoopStatusWrite(t *testing.T) {
	instance := newInstanceObject("demo", "default")
	metadata.SetInstanceFinalizer(instance)
	metadata.NewKROMetaLabeler().ApplyLabels(instance)
	require.NoError(t, unstructured.SetNestedField(instance.Object, int64(1), "metadata", "generation"))
	require.NoError(t, unstructured.SetNestedField(instance.Object,
		string(v1alpha1.InstanceStateActive), "status", "state"))
	require.NoError(t, unstructured.SetNestedSlice(instance.Object, []interface{}{
		map[string]interface{}{
			"type":               "AppReady",
			"status":             "True",
			"reason":             "Healthy",
			"lastTransitionTime": "2026-01-01T00:00:00Z",
			"observedGeneration": int64(1),
		},
	}, "status", "conditions"))

	instanceNode := authorConditionsInstanceNode(t, mustCompileControllerExpr(t,
		`runtime.newCondition({type: 'AppReady', status: 'True', reason: 'Healthy', message: ''})`,
		library.RuntimeVarName,
	))

	raw := newControllerTestDynamicClient(t, instance.DeepCopy())
	controller, _ := newControllerUnderTest(t, raw, newTestGraphWithInstance(instanceNode))

	require.NoError(t, controller.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "demo", Namespace: "default"},
	}))

	for _, action := range raw.Actions() {
		assert.NotEqual(t, "update", action.GetVerb(),
			"steady state must not issue a status write")
	}
}

// TestReconcileSkipsNoopStatusWriteForWholeNumbers verifies the skip-write
// guard is not defeated by Go number types alone: apimachinery decodes a
// persisted whole number as int64 while CEL evaluation yields float64, and
// both serialize to the same JSON. The int64 is seeded explicitly because
// the fake dynamic client stores objects as-is and never round-trips them
// through JSON the way a real API server response would.
func TestReconcileSkipsNoopStatusWriteForWholeNumbers(t *testing.T) {
	instance := newInstanceObject("demo", "default")
	metadata.SetInstanceFinalizer(instance)
	metadata.NewKROMetaLabeler().ApplyLabels(instance)
	require.NoError(t, unstructured.SetNestedField(instance.Object, int64(1), "metadata", "generation"))
	require.NoError(t, unstructured.SetNestedField(instance.Object,
		string(v1alpha1.InstanceStateActive), "status", "state"))
	// The wire value as a real API server would return it: int64.
	require.NoError(t, unstructured.SetNestedField(instance.Object, int64(3), "status", "replicas"))
	require.NoError(t, unstructured.SetNestedSlice(instance.Object, []interface{}{
		map[string]interface{}{
			"type":               "AppReady",
			"status":             "True",
			"reason":             "Healthy",
			"lastTransitionTime": "2026-01-01T00:00:00Z",
			"observedGeneration": int64(1),
		},
	}, "status", "conditions"))

	instanceNode := authorConditionsInstanceNode(t, mustCompileControllerExpr(t,
		`runtime.newCondition({type: 'AppReady', status: 'True', reason: 'Healthy', message: ''})`,
		library.RuntimeVarName,
	))
	// A CEL double that is a whole number lands in the computed status as
	// float64(3), semantically equal to the persisted int64(3).
	instanceNode.Template = &unstructured.Unstructured{
		Object: map[string]interface{}{
			"status": map[string]interface{}{"replicas": "${1.5 * 2.0}"},
		},
	}
	instanceNode.Variables = []*variable.ResourceField{
		standaloneField("status.replicas", mustCompileControllerExpr(t, "1.5 * 2.0"), variable.ResourceVariableKindStatic),
	}

	raw := newControllerTestDynamicClient(t, instance.DeepCopy())
	controller, _ := newControllerUnderTest(t, raw, newTestGraphWithInstance(instanceNode))

	require.NoError(t, controller.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "demo", Namespace: "default"},
	}))

	for _, action := range raw.Actions() {
		assert.NotEqual(t, "update", action.GetVerb(),
			"a float64 whole number must compare equal to its persisted int64 form")
	}
}

func TestReconcileDeletionRemovesFinalizer(t *testing.T) {
	instance := newInstanceObject("demo", "default")
	metadata.SetInstanceFinalizer(instance)
	instance.SetDeletionTimestamp(new(metav1.NewTime(time.Now())))

	raw := newControllerTestDynamicClient(t, instance.DeepCopy())
	controller, _ := newControllerUnderTest(t, raw, newTestGraph())

	err := controller.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: instance.GetName(), Namespace: instance.GetNamespace()},
	})
	require.NoError(t, err)

	stored := getStoredParentObject(t, raw)
	assert.False(t, metadata.HasInstanceFinalizer(stored))
	assert.Equal(t, metav1.ConditionUnknown, conditionByType(t, stored, ResourcesReady).Status)
}

func TestReconcileResourceMutationRequestsRequeue(t *testing.T) {
	instance := newInstanceObject("demo", "default")
	resourceNode := &graph.Node{
		Meta: graph.NodeMeta{
			ID:         "deploy",
			Type:       graph.NodeTypeResource,
			GVR:        controllerTestDeployGVR,
			Namespaced: true,
		},
		Template: newDeploymentObject("demo", ""),
	}

	raw := newControllerTestDynamicClient(t, instance.DeepCopy())
	controller, _ := newControllerUnderTest(t, raw, newTestGraph(resourceNode))

	err := controller.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: instance.GetName(), Namespace: instance.GetNamespace()},
	})
	var retryAfter *requeue.RequeueNeededAfter
	require.ErrorAs(t, err, &retryAfter)

	stored := getStoredParentObject(t, raw)
	assert.Equal(t, metav1.ConditionFalse, conditionByType(t, stored, ResourcesReady).Status)
}

func TestReconcileTerminatingManagedResourcesSetDeletingStatus(t *testing.T) {
	tests := []struct {
		name              string
		configureInstance func(*unstructured.Unstructured)
		graph             *graph.Graph
		buildCurrent      func(*unstructured.Unstructured) []apimachineryruntime.Object
		wantMessage       string
	}{
		{
			name: "single resource",
			graph: newTestGraph(&graph.Node{
				Meta: graph.NodeMeta{
					ID:         "deploy",
					Type:       graph.NodeTypeResource,
					GVR:        controllerTestDeployGVR,
					Namespaced: true,
				},
				Template: newDeploymentObject("demo", ""),
			}),
			buildCurrent: func(_ *unstructured.Unstructured) []apimachineryruntime.Object {
				current := newDeploymentObject("demo", "default")
				current.SetDeletionTimestamp(new(metav1.Now()))
				return []apimachineryruntime.Object{current}
			},
			wantMessage: `resource "default/demo" for node "deploy" is currently being deleted`,
		},
		{
			name: "collection resource",
			configureInstance: func(instance *unstructured.Unstructured) {
				_ = unstructured.SetNestedSlice(instance.Object, []interface{}{"one"}, "spec", "items")
			},
			graph: newTestGraph(newCollectionNodeForResources(t)),
			buildCurrent: func(instance *unstructured.Unstructured) []apimachineryruntime.Object {
				current := newConfigMapObject("one", "default")
				current.SetLabels(map[string]string{
					metadata.InstanceIDLabel: string(instance.GetUID()),
					metadata.NodeIDLabel:     "configs",
				})
				current.SetDeletionTimestamp(new(metav1.Now()))
				return []apimachineryruntime.Object{current}
			},
			wantMessage: `resource "default/one" for node "configs" is currently being deleted`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			instance := newInstanceObject("demo", "default")
			if tt.configureInstance != nil {
				tt.configureInstance(instance)
			}

			objs := tt.buildCurrent(instance)
			args := append([]apimachineryruntime.Object{instance.DeepCopy()}, objs...)
			raw := newControllerTestDynamicClient(t, args...)
			controller, _ := newControllerUnderTest(t, raw, tt.graph)

			err := controller.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{Name: instance.GetName(), Namespace: instance.GetNamespace()},
			})
			var retryAfter *requeue.RequeueNeededAfter
			require.ErrorAs(t, err, &retryAfter)

			stored := getStoredParentObject(t, raw)
			state, found, err := unstructured.NestedString(stored.Object, "status", "state")
			require.NoError(t, err)
			require.True(t, found)
			assert.Equal(t, string(v1alpha1.InstanceStateInProgress), state)

			cond := conditionByType(t, stored, ResourcesReady)
			assert.Equal(t, metav1.ConditionFalse, cond.Status)
			require.NotNil(t, cond.Reason)
			assert.Equal(t, "ResourceDeleting", *cond.Reason)
			require.NotNil(t, cond.Message)
			assert.Contains(t, *cond.Message, tt.wantMessage)
		})
	}
}

func TestReconcileManagedStateFailureMarksStatus(t *testing.T) {
	instance := newInstanceObject("demo", "default")
	raw := newControllerTestDynamicClient(t, instance.DeepCopy())
	raw.PrependReactor("patch", "webapps", func(action k8stesting.Action) (bool, apimachineryruntime.Object, error) {
		return true, nil, errors.New("patch failed")
	})

	controller, _ := newControllerUnderTest(t, raw, newTestGraph())
	err := controller.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: instance.GetName(), Namespace: instance.GetNamespace()},
	})
	require.Error(t, err)

	stored := getStoredParentObject(t, raw)
	assert.Equal(t, metav1.ConditionFalse, conditionByType(t, stored, InstanceManaged).Status)
}
