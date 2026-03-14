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
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/graph"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
	"github.com/kubernetes-sigs/kro/pkg/requeue"
)

func TestApplyManagedFinalizerAndLabels(t *testing.T) {
	tests := []struct {
		name            string
		presetFinalizer bool
		presetLabels    bool
		wantActions     int
		wantSameObject  bool
	}{
		{
			name:            "no patch needed",
			presetFinalizer: true,
			presetLabels:    true,
			wantSameObject:  true,
		},
		{
			name:        "patches missing finalizer and labels",
			wantActions: 1,
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
			assert.Equal(t, tt.wantSameObject, patched == rcx.Instance)
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
		instanceLabels      map[string]string
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
		{
			name:                "suspended reconciliation sets condition",
			instanceLabels:      map[string]string{metadata.InstanceReconcileLabel: "disabled"},
			wantState:           string(v1alpha1.InstanceStateActive),
			wantConditionType:   ReconciliationSuspended,
			wantConditionStatus: metav1.ConditionTrue,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			instance := newInstanceObject("demo", "default")
			instance.SetLabels(tt.instanceLabels)

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

func TestReconcileDeletionRemovesFinalizer(t *testing.T) {
	instance := newInstanceObject("demo", "default")
	metadata.SetInstanceFinalizer(instance)
	now := metav1.NewTime(time.Now())
	instance.SetDeletionTimestamp(&now)

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
				now := metav1.Now()
				current.SetDeletionTimestamp(&now)
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
				now := metav1.Now()
				current.SetDeletionTimestamp(&now)
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
