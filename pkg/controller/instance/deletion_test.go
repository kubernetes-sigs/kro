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
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	apimachineryruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/graph"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
	"github.com/kubernetes-sigs/kro/pkg/requeue"
)

// newManagedObject creates a resource with the kro ownership labels that
// discoverLiveResources uses to find managed resources.
func newManagedObject(base *unstructured.Unstructured, instanceUID, nodeID string) *unstructured.Unstructured {
	obj := base.DeepCopy()
	labels := obj.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels[metadata.InstanceIDLabel] = instanceUID
	labels[metadata.NodeIDLabel] = nodeID
	obj.SetLabels(labels)
	return obj
}

func TestDiscoverLiveResources(t *testing.T) {
	instance := newInstanceObject("demo", "default")
	uid := string(instance.GetUID())

	deployNode := &graph.Node{
		Meta: graph.NodeMeta{
			ID:         "deploy",
			Type:       graph.NodeTypeResource,
			GVR:        controllerTestDeployGVR,
			Namespaced: true,
		},
		Template: newDeploymentObject("demo", ""),
	}

	managed := newManagedObject(newDeploymentObject("demo", "default"), uid, "deploy")

	controller, rcx, _ := newControllerAndContext(t, instance, newTestGraph(deployNode), managed)

	live, err := controller.discoverLiveResources(rcx)
	require.NoError(t, err)
	require.Len(t, live, 1)
	assert.Equal(t, "demo", live[0].Object.GetName())
	assert.Equal(t, controllerTestDeployGVR, live[0].GVR)
}

func TestDiscoverLiveResourcesSkipsExternalNodes(t *testing.T) {
	instance := newInstanceObject("demo", "default")
	uid := string(instance.GetUID())

	externalNode := &graph.Node{
		Meta: graph.NodeMeta{
			ID:         "source",
			Type:       graph.NodeTypeExternal,
			GVR:        controllerTestCMGVR,
			Namespaced: true,
		},
		Template: newConfigMapObject("source", ""),
	}
	deployNode := &graph.Node{
		Meta: graph.NodeMeta{
			ID:         "deploy",
			Type:       graph.NodeTypeResource,
			GVR:        controllerTestDeployGVR,
			Namespaced: true,
		},
		Template: newDeploymentObject("demo", ""),
	}

	// The external ConfigMap exists but should not be discovered.
	sourceCM := newConfigMapObject("source", "default")
	sourceCM.SetLabels(map[string]string{metadata.InstanceIDLabel: uid, metadata.NodeIDLabel: "source"})
	managed := newManagedObject(newDeploymentObject("demo", "default"), uid, "deploy")

	controller, rcx, _ := newControllerAndContext(t, instance, newTestGraph(externalNode, deployNode), sourceCM, managed)

	live, err := controller.discoverLiveResources(rcx)
	require.NoError(t, err)
	require.Len(t, live, 1)
	assert.Equal(t, "demo", live[0].Object.GetName())
}

func TestDiscoverLiveResourcesMultipleGVRs(t *testing.T) {
	instance := newInstanceObject("demo", "default")
	uid := string(instance.GetUID())

	deployNode := &graph.Node{
		Meta: graph.NodeMeta{
			ID:         "deploy",
			Type:       graph.NodeTypeResource,
			GVR:        controllerTestDeployGVR,
			Namespaced: true,
		},
		Template: newDeploymentObject("demo", ""),
	}
	cmNode := &graph.Node{
		Meta: graph.NodeMeta{
			ID:         "config",
			Type:       graph.NodeTypeResource,
			GVR:        controllerTestCMGVR,
			Namespaced: true,
		},
		Template: newConfigMapObject("config", ""),
	}

	managedDeploy := newManagedObject(newDeploymentObject("demo", "default"), uid, "deploy")
	managedCM := newManagedObject(newConfigMapObject("config", "default"), uid, "config")

	controller, rcx, _ := newControllerAndContext(t, instance, newTestGraph(deployNode, cmNode), managedDeploy, managedCM)

	live, err := controller.discoverLiveResources(rcx)
	require.NoError(t, err)
	assert.Len(t, live, 2)
}

func TestDiscoverLiveResourcesListError(t *testing.T) {
	instance := newInstanceObject("demo", "default")

	deployNode := &graph.Node{
		Meta: graph.NodeMeta{
			ID:         "deploy",
			Type:       graph.NodeTypeResource,
			GVR:        controllerTestDeployGVR,
			Namespaced: true,
		},
		Template: newDeploymentObject("demo", ""),
	}

	controller, rcx, raw := newControllerAndContext(t, instance, newTestGraph(deployNode))
	raw.PrependReactor("list", "deployments", func(action k8stesting.Action) (bool, apimachineryruntime.Object, error) {
		return true, nil, errors.New("list failed")
	})

	_, err := controller.discoverLiveResources(rcx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "list failed")
}

func TestReconcileDeletionDeletesAllAndRequeues(t *testing.T) {
	instance := newInstanceObject("demo", "default")
	uid := string(instance.GetUID())

	deployNode := &graph.Node{
		Meta: graph.NodeMeta{
			ID:         "deploy",
			Type:       graph.NodeTypeResource,
			GVR:        controllerTestDeployGVR,
			Namespaced: true,
		},
		Template: newDeploymentObject("demo", ""),
	}

	managed := newManagedObject(newDeploymentObject("demo", "default"), uid, "deploy")

	controller, rcx, _ := newControllerAndContext(t, instance, newTestGraph(deployNode), managed)

	err := controller.reconcileDeletion(rcx)
	var retryAfter *requeue.RequeueNeededAfter
	require.ErrorAs(t, err, &retryAfter)
	assert.Equal(t, v1alpha1.InstanceStateDeleting, rcx.StateManager.State)
	assert.Equal(t, v1alpha1.NodeStateDeleting, rcx.StateManager.NodeStates["deploy"].State)
}

func TestReconcileDeletionRemovesFinalizerWhenNoLiveResources(t *testing.T) {
	instance := newInstanceObject("demo", "default")
	metadata.SetInstanceFinalizer(instance)

	deployNode := &graph.Node{
		Meta: graph.NodeMeta{
			ID:         "deploy",
			Type:       graph.NodeTypeResource,
			GVR:        controllerTestDeployGVR,
			Namespaced: true,
		},
		Template: newDeploymentObject("demo", ""),
	}

	// No managed objects in the cluster.
	controller, rcx, _ := newControllerAndContext(t, instance, newTestGraph(deployNode))

	err := controller.reconcileDeletion(rcx)
	require.NoError(t, err)
	assert.False(t, metadata.HasInstanceFinalizer(rcx.Instance))
}

func TestReconcileDeletionSkipsAlreadyTerminatingResources(t *testing.T) {
	instance := newInstanceObject("demo", "default")
	uid := string(instance.GetUID())

	deployNode := &graph.Node{
		Meta: graph.NodeMeta{
			ID:         "deploy",
			Type:       graph.NodeTypeResource,
			GVR:        controllerTestDeployGVR,
			Namespaced: true,
		},
		Template: newDeploymentObject("demo", ""),
	}

	managed := newManagedObject(newDeploymentObject("demo", "default"), uid, "deploy")
	now := metav1.NewTime(time.Now())
	managed.SetDeletionTimestamp(&now)

	controller, rcx, raw := newControllerAndContext(t, instance, newTestGraph(deployNode), managed)

	// Track whether DELETE is called — it should NOT be.
	deletesCalled := 0
	raw.PrependReactor("delete", "deployments", func(action k8stesting.Action) (bool, apimachineryruntime.Object, error) {
		deletesCalled++
		return false, nil, nil
	})

	err := controller.reconcileDeletion(rcx)
	var retryAfter *requeue.RequeueNeededAfter
	require.ErrorAs(t, err, &retryAfter)
	assert.Equal(t, 0, deletesCalled, "DELETE should not be called for already-terminating resources")
	assert.Equal(t, v1alpha1.NodeStateDeleting, rcx.StateManager.NodeStates["deploy"].State)
}

func TestReconcileDeletionDeleteErrorBubblesUp(t *testing.T) {
	instance := newInstanceObject("demo", "default")
	uid := string(instance.GetUID())

	deployNode := &graph.Node{
		Meta: graph.NodeMeta{
			ID:         "deploy",
			Type:       graph.NodeTypeResource,
			GVR:        controllerTestDeployGVR,
			Namespaced: true,
		},
		Template: newDeploymentObject("demo", ""),
	}

	managed := newManagedObject(newDeploymentObject("demo", "default"), uid, "deploy")

	controller, rcx, raw := newControllerAndContext(t, instance, newTestGraph(deployNode), managed)
	raw.PrependReactor("delete", "deployments", func(action k8stesting.Action) (bool, apimachineryruntime.Object, error) {
		return true, nil, errors.New("delete failed")
	})

	err := controller.reconcileDeletion(rcx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "delete failed")
	assert.Equal(t, v1alpha1.NodeStateError, rcx.StateManager.NodeStates["deploy"].State)
}

func TestReconcileDeletionExternalRefGoneDoesNotBlock(t *testing.T) {
	instance := newInstanceObject("demo", "default")
	uid := string(instance.GetUID())

	externalNode := &graph.Node{
		Meta: graph.NodeMeta{
			ID:         "source",
			Type:       graph.NodeTypeExternal,
			GVR:        controllerTestCMGVR,
			Namespaced: true,
		},
		Template: newConfigMapObject("source", ""),
	}
	deployNode := &graph.Node{
		Meta: graph.NodeMeta{
			ID:           "mirror",
			Type:         graph.NodeTypeResource,
			GVR:          controllerTestDeployGVR,
			Namespaced:   true,
			Dependencies: []string{"source"},
		},
		Template: newDeploymentObject("mirror", ""),
	}

	// External ref is gone — not in the fake client.
	// Managed resource still exists (has a controller finalizer keeping it alive).
	managed := newManagedObject(newDeploymentObject("mirror", "default"), uid, "mirror")

	controller, rcx, _ := newControllerAndContext(t, instance, newTestGraph(externalNode, deployNode), managed)

	err := controller.reconcileDeletion(rcx)
	var retryAfter *requeue.RequeueNeededAfter
	require.ErrorAs(t, err, &retryAfter)
	assert.Equal(t, v1alpha1.NodeStateDeleting, rcx.StateManager.NodeStates["mirror"].State)
	assert.Equal(t, v1alpha1.NodeStateSkipped, rcx.StateManager.NodeStates["source"].State)
}

func TestReconcileDeletionMarksExternalAndMissingNodesCorrectly(t *testing.T) {
	instance := newInstanceObject("demo", "default")
	uid := string(instance.GetUID())

	externalNode := &graph.Node{
		Meta: graph.NodeMeta{
			ID:         "ext",
			Type:       graph.NodeTypeExternal,
			GVR:        controllerTestCMGVR,
			Namespaced: true,
		},
		Template: newConfigMapObject("ext", ""),
	}
	managedNode := &graph.Node{
		Meta: graph.NodeMeta{
			ID:         "deploy",
			Type:       graph.NodeTypeResource,
			GVR:        controllerTestDeployGVR,
			Namespaced: true,
		},
		Template: newDeploymentObject("deploy", ""),
	}

	// One managed resource still alive — the state loop will run.
	managed := newManagedObject(newDeploymentObject("deploy", "default"), uid, "deploy")

	controller, rcx, _ := newControllerAndContext(t, instance, newTestGraph(externalNode, managedNode), managed)

	err := controller.reconcileDeletion(rcx)
	var retryAfter *requeue.RequeueNeededAfter
	require.ErrorAs(t, err, &retryAfter)
	assert.Equal(t, v1alpha1.NodeStateSkipped, rcx.StateManager.NodeStates["ext"].State)
	assert.Equal(t, v1alpha1.NodeStateDeleting, rcx.StateManager.NodeStates["deploy"].State)
}

// --- Tests for setUnmanaged and removeFinalizer (unchanged) ---

func TestSetUnmanaged(t *testing.T) {
	tests := []struct {
		name          string
		withFinalizer bool
		patchErr      string
		wantErrText   string
		wantSame      bool
		wantManaged   bool
	}{
		{
			name:        "returns the original object when finalizer is absent",
			wantSame:    true,
			wantManaged: false,
		},
		{
			name:          "removes the managed finalizer when present",
			withFinalizer: true,
			wantManaged:   false,
		},
		{
			name:          "returns patch errors",
			withFinalizer: true,
			patchErr:      "patch failed",
			wantErrText:   "failed to update unmanaged state",
			wantManaged:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			instance := newInstanceObject("demo", "default")
			if tt.withFinalizer {
				metadata.SetInstanceFinalizer(instance)
			}

			controller, rcx, raw := newControllerAndContext(t, instance, newTestGraph())
			if tt.patchErr != "" {
				raw.PrependReactor("patch", "webapps", func(action k8stesting.Action) (bool, apimachineryruntime.Object, error) {
					return true, nil, errors.New(tt.patchErr)
				})
			}

			patched, err := controller.setUnmanaged(rcx, rcx.Instance)
			if tt.wantErrText != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErrText)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.wantSame, patched == rcx.Instance)
			assert.Equal(t, tt.wantManaged, metadata.HasInstanceFinalizer(patched))
		})
	}
}

func TestSetUnmanagedRetriesOnConflict(t *testing.T) {
	instance := newInstanceObject("demo", "default")
	instance.SetResourceVersion("1")
	metadata.SetInstanceFinalizer(instance)
	instance.SetFinalizers(append(instance.GetFinalizers(), "other.io/finalizer"))

	controller, rcx, raw := newControllerAndContext(t, instance, newTestGraph())

	var attempts atomic.Int32
	raw.PrependReactor("get", "webapps", func(action k8stesting.Action) (bool, apimachineryruntime.Object, error) {
		attempt := attempts.Add(1)
		obj := instance.DeepCopy()
		obj.SetResourceVersion(string('0' + attempt))
		return true, obj, nil
	})

	raw.PrependReactor("patch", "webapps", func(action k8stesting.Action) (bool, apimachineryruntime.Object, error) {
		patchAction := action.(k8stesting.PatchAction)
		var patchPayload map[string]interface{}
		err := json.Unmarshal(patchAction.GetPatch(), &patchPayload)
		require.NoError(t, err)

		patchMeta, ok := patchPayload["metadata"].(map[string]interface{})
		require.True(t, ok, "patch must include metadata")
		_, hasResourceVersion := patchMeta["resourceVersion"]
		require.True(t, hasResourceVersion, "patch must include resourceVersion")

		finalizers, ok := patchMeta["finalizers"].([]interface{})
		require.True(t, ok, "patch must include finalizers array")
		require.Equal(t, []interface{}{"other.io/finalizer"}, finalizers, "only kro finalizer should be removed")

		attempt := attempts.Load()
		if attempt == 1 {
			return true, nil, apierrors.NewConflict(
				schema.GroupResource{Group: "kro.run", Resource: "webapps"},
				"demo",
				errors.New("conflict"),
			)
		}
		result := instance.DeepCopy()
		result.SetFinalizers([]string{"other.io/finalizer"})
		result.SetResourceVersion("2")
		return true, result, nil
	})

	patched, err := controller.setUnmanaged(rcx, rcx.Instance)
	require.NoError(t, err)
	assert.False(t, metadata.HasInstanceFinalizer(patched))
	assert.Equal(t, []string{"other.io/finalizer"}, patched.GetFinalizers())
	assert.GreaterOrEqual(t, attempts.Load(), int32(2))
}

func TestRemoveFinalizerMarksInstanceNotManagedOnError(t *testing.T) {
	instance := newInstanceObject("demo", "default")
	metadata.SetInstanceFinalizer(instance)

	controller, rcx, raw := newControllerAndContext(t, instance, newTestGraph())
	raw.PrependReactor("patch", "webapps", func(action k8stesting.Action) (bool, apimachineryruntime.Object, error) {
		return true, nil, errors.New("patch failed")
	})

	err := controller.removeFinalizer(rcx)
	require.Error(t, err)
	assert.Equal(t, metav1.ConditionFalse, conditionByType(t, rcx.Instance, InstanceManaged).Status)
}
