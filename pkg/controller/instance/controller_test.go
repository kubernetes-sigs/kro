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
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	apimachineryruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/controller/instance/applyset"
	"github.com/kubernetes-sigs/kro/pkg/graph/revisions"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/compiler"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/rgdadapter"
	geruntime "github.com/kubernetes-sigs/kro/pkg/graphengine/runtime"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
	"github.com/kubernetes-sigs/kro/pkg/requeue"
)

// TestReconcileInstanceLoad exercises the engine-agnostic instance-load path
// that runs before the deletion/graph-engine branches.
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

func TestReconcileDeletionRemovesFinalizer(t *testing.T) {
	instance := newInstanceObject("demo", "default")
	addEmptyDeletionScope(instance)
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

func TestReconcileDeletionPreservesAuthorStatusWithoutRuntime(t *testing.T) {
	instance := newInstanceObject("demo", "default")
	addEmptyDeletionScope(instance)
	metadata.SetInstanceFinalizer(instance)
	instance.SetDeletionTimestamp(new(metav1.NewTime(time.Now())))
	require.NoError(t, unstructured.SetNestedMap(instance.Object, map[string]any{
		"state":    string(v1alpha1.InstanceStateActive),
		"endpoint": "https://example.test",
		"conditions": []any{map[string]any{
			"type":               "AuthorHealthy",
			"status":             "True",
			"reason":             "Healthy",
			"lastTransitionTime": "2026-01-01T00:00:00Z",
		}},
	}, "status"))

	raw := newControllerTestDynamicClient(t, instance.DeepCopy())
	controller, _ := newControllerUnderTest(t, raw, newTestGraph())
	controller.reconcileConfig.HasAuthorConditions = true

	require.NoError(t, controller.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: instance.GetName(), Namespace: instance.GetNamespace()},
	}))

	stored := getStoredParentObject(t, raw)
	assert.False(t, metadata.HasInstanceFinalizer(stored))
	assert.Equal(t, "https://example.test", stored.Object["status"].(map[string]any)["endpoint"])
	conditions := conditionsFromInstance(stored)
	require.Len(t, conditions, 2)
	authorHealthy := conditionByType(t, stored, "AuthorHealthy")
	require.NotNil(t, authorHealthy.LastTransitionTime)
	assert.Equal(t, "2026-01-01T00:00:00Z", authorHealthy.LastTransitionTime.UTC().Format(time.RFC3339))
	resourcesReady := conditionByType(t, stored, ResourcesReady)
	assert.Equal(t, metav1.ConditionUnknown, resourcesReady.Status)
	assert.Equal(t, new("UnderDeletion"), resourcesReady.Reason)
}

func TestReconcileDeletionSurfacesErrorsWithAuthorConditions(t *testing.T) {
	instance := newInstanceObject("demo", "default")
	metadata.SetInstanceFinalizer(instance)
	instance.SetDeletionTimestamp(new(metav1.NewTime(time.Now())))
	require.NoError(t, unstructured.SetNestedMap(instance.Object, map[string]any{
		"state": string(v1alpha1.InstanceStateActive),
		"conditions": []any{map[string]any{
			"type":               "AuthorHealthy",
			"status":             "True",
			"reason":             "Healthy",
			"lastTransitionTime": "2026-01-01T00:00:00Z",
		}},
	}, "status"))

	raw := newControllerTestDynamicClient(t, instance.DeepCopy())
	controller, _ := newControllerUnderTest(t, raw, newTestGraph())
	controller.reconcileConfig.HasAuthorConditions = true

	err := controller.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: instance.GetName(), Namespace: instance.GetNamespace()},
	})
	require.Error(t, err)

	stored := getStoredParentObject(t, raw)
	assert.True(t, metadata.HasInstanceFinalizer(stored))
	assert.Equal(t, metav1.ConditionTrue, conditionByType(t, stored, "AuthorHealthy").Status)
	resourcesReady := conditionByType(t, stored, ResourcesReady)
	assert.Equal(t, metav1.ConditionUnknown, resourcesReady.Status)
	require.NotNil(t, resourcesReady.Message)
	assert.Contains(t, *resourcesReady.Message, "deletion blocked")
	assert.Contains(t, *resourcesReady.Message, applyset.ApplySetParentIDLabel)
}

func TestReconcileApplySetInventory_PruneGate(t *testing.T) {
	cases := []struct {
		name       string
		hardErr    bool
		unresolved []string
		observed   bool
		wantPruned bool
		wantShrink bool
	}{
		{name: "full resolution prunes even unobserved owners", wantPruned: true, wantShrink: true},
		{name: "hard error vetoes prune", hardErr: true, observed: true},
		{name: "completed empty template prunes during partial cycle", unresolved: []string{"other"}, observed: true, wantPruned: true},
		{name: "hard error vetoes partial prune", hardErr: true, unresolved: []string{"other"}, observed: true},
		{name: "unobserved template retains members", unresolved: []string{"other"}},
		{name: "unresolved template retains even observed members", unresolved: []string{"cm"}, observed: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inst := newInstanceObject("demo", "default")
			orphan := newManagedObject(newConfigMapObject("retired", "default"), inst, "cm", 1)
			raw := newControllerTestDynamicClient(t, inst.DeepCopy(), orphan)
			c, _ := newControllerUnderTest(t, raw, newTestGraph())
			rt := geruntime.New(&compiler.Program{
				Nodes:            map[string]*compiler.Node{"cm": {ID: "cm", Kind: compiler.NodeKindTemplate}},
				TopologicalOrder: []string{"cm"},
			}, nil)
			if tc.observed {
				rt.Node("cm").SetObserved([]*unstructured.Unstructured{}, nil)
			}
			fullyResolved, completed := pruneGate(rt, tc.hardErr, tc.unresolved)
			require.NoError(t, c.reconcileApplySetInventory(t.Context(), c.log, inst, nil, nil, applyset.Metadata{}, fullyResolved, completed))

			stored, err := raw.Resource(controllerTestCMGVR).Namespace("default").Get(t.Context(), "retired", metav1.GetOptions{})
			if tc.wantPruned {
				require.True(t, apierrors.IsNotFound(err), "retired member must be deleted: %v", err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, orphan.GetUID(), stored.GetUID())
				for _, action := range raw.Actions() {
					assert.NotEqual(t, "delete", action.GetVerb(), "retention must not attempt deletion")
				}
			}
			parent := getStoredParentObject(t, raw)
			require.NoError(t, applyset.ValidateParentInventory(parent))
			if tc.wantShrink {
				assert.Empty(t, parent.GetAnnotations()[applyset.ApplySetGKsAnnotation])
			} else {
				assert.Equal(t, "ConfigMap", parent.GetAnnotations()[applyset.ApplySetGKsAnnotation])
			}
		})
	}
}

// Only unresolved owning nodes restrict pruning and inventory shrink.
func TestOwnedUnresolved(t *testing.T) {
	comp := newTestRealCompiler(t)
	inst := newInstanceObject("demo", "default")

	// RGD with a template resource (owns a ConfigMap) AND a read-only
	// externalRef resource (a ref node, which owns nothing — kro never applies
	// or prunes it). Both target the ConfigMap kind, which the fake resolver
	// knows, so the runtime compiles without a synthesized instance schema.
	rgd := &v1alpha1.ResourceGraphDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "webapp"},
		Spec: v1alpha1.ResourceGraphDefinitionSpec{
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
						Raw: []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"cm-1"}}`),
					},
				},
				{
					ID: "existing",
					ExternalRef: &v1alpha1.ExternalRef{
						APIVersion: "v1",
						Kind:       "ConfigMap",
						Metadata:   v1alpha1.ExternalRefMetadata{Name: "imported", Namespace: "default"},
					},
				},
			},
		},
	}
	rt, _, err := rgdadapter.BuildRuntimeForInstance(rgd, inst, comp)
	require.NoError(t, err)

	// Sanity: the runtime carries both an owning template node ("cm") and the
	// ownerless read-only ref node ("existing").
	cmNode := rt.Node("cm")
	require.NotNil(t, cmNode, "template node must exist")
	require.Equal(t, compiler.NodeKindTemplate, cmNode.Kind(), "cm is an owning template node")
	refNode := rt.Node("existing")
	require.NotNil(t, refNode, "externalRef node must exist")
	require.Equal(t, compiler.NodeKindRef, refNode.Kind(), "externalRef compiles to an ownerless ref node")

	t.Run("ownerless nodes do not restrict pruning or shrink", func(t *testing.T) {
		owning := ownedUnresolved(rt, []string{"existing", "schema"})
		assert.Empty(t, owning, "ownerless ref node must not veto pruning")
		fullyResolved, _ := pruneGate(rt, false, owning)
		assert.True(t, fullyResolved)
	})

	t.Run("owning template node prevents full resolution", func(t *testing.T) {
		owning := ownedUnresolved(rt, []string{"cm"})
		assert.Equal(t, []string{"cm"}, owning)
		fullyResolved, _ := pruneGate(rt, false, owning)
		assert.False(t, fullyResolved)
	})

	t.Run("mixed set keeps only owning nodes", func(t *testing.T) {
		owning := ownedUnresolved(rt, []string{"existing", "cm"})
		assert.Equal(t, []string{"cm"}, owning, "only the owning node survives the filter")
		fullyResolved, _ := pruneGate(rt, false, owning)
		assert.False(t, fullyResolved)
	})

	t.Run("unknown node id is conservatively kept", func(t *testing.T) {
		owning := ownedUnresolved(rt, []string{"sub/child"})
		assert.Equal(t, []string{"sub/child"}, owning)
		fullyResolved, _ := pruneGate(rt, false, owning)
		assert.False(t, fullyResolved, "unknown owners must keep the inventory broad")
	})
}

// TestReconcile_EmitsInitialConditionEventsOnFirstReconcile asserts that on the
// very first reconcile (where stampInstanceMetadata patches metadata and
// updates the in-memory instance), condition-transition events are emitted for
// the full initial condition set (InstanceManaged, GraphResolved, ResourcesReady, Ready).
func TestReconcile_EmitsInitialConditionEventsOnFirstReconcile(t *testing.T) {
	comp := newTestRealCompiler(t)
	inst := newInstanceObject("demo", "default")
	raw := newControllerTestDynamicClient(t, inst.DeepCopy())
	controller, _ := newGraphEngineControllerUnderTest(t, raw, testEmptyRGDSpec(), revisions.RevisionStateActive, comp, nil)

	fakeRecorder := record.NewFakeRecorder(100)
	controller.eventRecorder = fakeRecorder
	controller.eventsEnabled = true

	err := controller.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "demo", Namespace: "default"},
	})
	require.NoError(t, err)

	events := drainEvents(fakeRecorder)
	require.NotEmpty(t, events, "initial condition set must emit transition events on first reconcile")

	// Verify events for all four built-in conditions were emitted
	var hasManaged, hasResolved, hasResourcesReady, hasReady bool
	for _, e := range events {
		if strings.Contains(e, InstanceManaged) {
			hasManaged = true
		}
		if strings.Contains(e, GraphResolved) {
			hasResolved = true
		}
		if strings.Contains(e, ResourcesReady) {
			hasResourcesReady = true
		}
		if strings.Contains(e, Ready) {
			hasReady = true
		}
	}
	assert.True(t, hasManaged, "must emit event for InstanceManaged")
	assert.True(t, hasResolved, "must emit event for GraphResolved")
	assert.True(t, hasResourcesReady, "must emit event for ResourcesReady")
	assert.True(t, hasReady, "must emit event for Ready")

	// On second reconcile without changes, no new events should be emitted
	err = controller.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "demo", Namespace: "default"},
	})
	require.NoError(t, err)
	events2 := drainEvents(fakeRecorder)
	assert.Empty(t, events2, "no events should be emitted when conditions have not changed")
}

// TestReconcile_FailedRevisionDoesNotDelayedRequeue asserts that when the latest
// revision is in a failed state, Reconcile returns requeue.None (not a delayed requeue)
// and marks the instance condition explaining the failed revision.
func TestReconcile_FailedRevisionDoesNotDelayedRequeue(t *testing.T) {
	comp := newTestRealCompiler(t)
	inst := newInstanceObject("demo", "default")
	raw := newControllerTestDynamicClient(t, inst.DeepCopy())
	controller, _ := newGraphEngineControllerUnderTest(t, raw, testEmptyRGDSpec(), revisions.RevisionStateFailed, comp, nil)

	err := controller.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "demo", Namespace: "default"},
	})
	require.Error(t, err)
	assert.False(t, requeue.IsRequeueError(err), "failed revision must return requeue.None, not a requeue error")
	var noReq *requeue.NoRequeue
	assert.True(t, errors.As(err, &noReq), "error must be *requeue.NoRequeue")
	assert.Contains(t, err.Error(), "latest issued revision 1 failed")

	stored := getStoredParentObject(t, raw)
	cond := conditionByType(t, stored, GraphResolved)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	require.NotNil(t, cond.Message)
	assert.Contains(t, *cond.Message, "latest issued revision 1 failed")
}
