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
	"sort"
	"strconv"
	"strings"
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
	"k8s.io/apimachinery/pkg/types"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/controller/instance/applyset"
	"github.com/kubernetes-sigs/kro/pkg/graph"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
	"github.com/kubernetes-sigs/kro/pkg/requeue"
)

func addDeletionScope(instance *unstructured.Unstructured, gvk schema.GroupVersionKind, namespace string) {
	annotations := instance.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	gks := map[string]struct{}{}
	for _, value := range strings.Split(annotations[applyset.ApplySetGKsAnnotation], ",") {
		if value != "" {
			gks[value] = struct{}{}
		}
	}
	gk := gvk.Kind
	if gvk.Group != "" {
		gk += "." + gvk.Group
	}
	gks[gk] = struct{}{}
	values := make([]string, 0, len(gks))
	for value := range gks {
		values = append(values, value)
	}
	sort.Strings(values)
	annotations[applyset.ApplySetGKsAnnotation] = strings.Join(values, ",")
	if namespace != "" && namespace != instance.GetNamespace() {
		annotations[applyset.ApplySetAdditionalNamespacesAnnotation] = namespace
	}
	instance.SetAnnotations(annotations)
	labels := instance.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels[applyset.ApplySetParentIDLabel] = applyset.ID(instance)
	instance.SetLabels(labels)
}

// newManagedObject creates a stable ApplySet member in the parent's persisted scope.
func newManagedObject(base, instance *unstructured.Unstructured, nodeID string, order int) *unstructured.Unstructured {
	obj := base.DeepCopy()
	addDeletionScope(instance, obj.GroupVersionKind(), obj.GetNamespace())
	labels := obj.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels[metadata.InstanceIDLabel] = string(instance.GetUID())
	labels[metadata.NodeIDLabel] = nodeID
	labels[metadata.ApplyOrderLabel] = strconv.Itoa(order)
	labels[applyset.ApplysetPartOfLabel] = applyset.ID(instance)
	obj.SetLabels(labels)
	obj.SetUID(types.UID(nodeID + "-uid"))
	return obj
}

func TestDiscoverLiveResources(t *testing.T) {
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

	managed := newManagedObject(newDeploymentObject("demo", "default"), instance, "deploy", 1)

	controller, rcx, _ := newControllerAndContext(t, instance, newTestGraph(deployNode), managed)

	live, _, err := controller.discoverDeletionInventory(rcx)
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
	managed := newManagedObject(newDeploymentObject("demo", "default"), instance, "deploy", 2)

	controller, rcx, _ := newControllerAndContext(t, instance, newTestGraph(externalNode, deployNode), sourceCM, managed)

	live, _, err := controller.discoverDeletionInventory(rcx)
	require.NoError(t, err)
	require.Len(t, live, 1)
	assert.Equal(t, "demo", live[0].Object.GetName())
}

func TestDiscoverLiveResourcesMultipleGVRs(t *testing.T) {
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
	cmNode := &graph.Node{
		Meta: graph.NodeMeta{
			ID:         "config",
			Type:       graph.NodeTypeResource,
			GVR:        controllerTestCMGVR,
			Namespaced: true,
		},
		Template: newConfigMapObject("config", ""),
	}

	managedDeploy := newManagedObject(newDeploymentObject("demo", "default"), instance, "deploy", 1)
	managedCM := newManagedObject(newConfigMapObject("config", "default"), instance, "config", 2)

	controller, rcx, _ := newControllerAndContext(t, instance, newTestGraph(deployNode, cmNode), managedDeploy, managedCM)

	live, _, err := controller.discoverDeletionInventory(rcx)
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

	addDeletionScope(instance, controllerTestDeployGVK, "default")
	controller, rcx, raw := newControllerAndContext(t, instance, newTestGraph(deployNode))
	raw.PrependReactor("list", "deployments", func(action k8stesting.Action) (bool, apimachineryruntime.Object, error) {
		return true, nil, errors.New("list failed")
	})

	_, _, err := controller.discoverDeletionInventory(rcx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "list failed")
}

func TestReconcileDeletionDeletesAllAndRequeues(t *testing.T) {
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

	managed := newManagedObject(newDeploymentObject("demo", "default"), instance, "deploy", 1)

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

	deployNode := &graph.Node{
		Meta: graph.NodeMeta{
			ID:         "deploy",
			Type:       graph.NodeTypeResource,
			GVR:        controllerTestDeployGVR,
			Namespaced: true,
		},
		Template: newDeploymentObject("demo", ""),
	}

	managed := newManagedObject(newDeploymentObject("demo", "default"), instance, "deploy", 1)
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

	deployNode := &graph.Node{
		Meta: graph.NodeMeta{
			ID:         "deploy",
			Type:       graph.NodeTypeResource,
			GVR:        controllerTestDeployGVR,
			Namespaced: true,
		},
		Template: newDeploymentObject("demo", ""),
	}

	managed := newManagedObject(newDeploymentObject("demo", "default"), instance, "deploy", 1)

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
	managed := newManagedObject(newDeploymentObject("mirror", "default"), instance, "mirror", 2)

	controller, rcx, _ := newControllerAndContext(t, instance, newTestGraph(externalNode, deployNode), managed)

	err := controller.reconcileDeletion(rcx)
	var retryAfter *requeue.RequeueNeededAfter
	require.ErrorAs(t, err, &retryAfter)
	assert.Equal(t, v1alpha1.NodeStateDeleting, rcx.StateManager.NodeStates["mirror"].State)
	assert.Equal(t, v1alpha1.NodeStateSkipped, rcx.StateManager.NodeStates["source"].State)
}

func TestReconcileDeletionMarksExternalAndMissingNodesCorrectly(t *testing.T) {
	instance := newInstanceObject("demo", "default")

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
	managed := newManagedObject(newDeploymentObject("deploy", "default"), instance, "deploy", 2)

	controller, rcx, _ := newControllerAndContext(t, instance, newTestGraph(externalNode, managedNode), managed)

	err := controller.reconcileDeletion(rcx)
	var retryAfter *requeue.RequeueNeededAfter
	require.ErrorAs(t, err, &retryAfter)
	assert.Equal(t, v1alpha1.NodeStateSkipped, rcx.StateManager.NodeStates["ext"].State)
	assert.Equal(t, v1alpha1.NodeStateDeleting, rcx.StateManager.NodeStates["deploy"].State)
}

func TestReconcileDeletionDeletesOnlyHighestOrderWave(t *testing.T) {
	instance := newInstanceObject("demo", "default")
	nodeA := &graph.Node{Meta: graph.NodeMeta{ID: "a", Type: graph.NodeTypeResource, GVR: controllerTestDeployGVR, Namespaced: true}}
	nodeB := &graph.Node{Meta: graph.NodeMeta{ID: "b", Type: graph.NodeTypeResource, GVR: controllerTestDeployGVR, Namespaced: true}}
	a := newManagedObject(newDeploymentObject("a", "default"), instance, "a", 1)
	b := newManagedObject(newDeploymentObject("b", "default"), instance, "b", 2)

	controller, rcx, raw := newControllerAndContext(t, instance, newTestGraph(nodeA, nodeB), a, b)
	var deleted []string
	raw.PrependReactor("delete", "deployments", func(action k8stesting.Action) (bool, apimachineryruntime.Object, error) {
		deleted = append(deleted, action.(k8stesting.DeleteAction).GetName())
		return true, nil, nil
	})

	err := controller.reconcileDeletion(rcx)
	var retryAfter *requeue.RequeueNeededAfter
	require.ErrorAs(t, err, &retryAfter)
	assert.Equal(t, []string{"b"}, deleted)
	assert.Equal(t, v1alpha1.NodeStateDeleting, rcx.StateManager.NodeStates["a"].State)
	assert.Equal(t, v1alpha1.NodeStateDeleting, rcx.StateManager.NodeStates["b"].State)
}

func TestReconcileDeletionAdvancesAfterHigherWaveDisappears(t *testing.T) {
	instance := newInstanceObject("demo", "default")
	nodeA := &graph.Node{Meta: graph.NodeMeta{ID: "a", Type: graph.NodeTypeResource, GVR: controllerTestDeployGVR, Namespaced: true}}
	nodeB := &graph.Node{Meta: graph.NodeMeta{ID: "b", Type: graph.NodeTypeResource, GVR: controllerTestDeployGVR, Namespaced: true}}
	a := newManagedObject(newDeploymentObject("a", "default"), instance, "a", 1)
	b := newManagedObject(newDeploymentObject("b", "default"), instance, "b", 2)

	controller, rcx, raw := newControllerAndContext(t, instance, newTestGraph(nodeA, nodeB), a, b)
	require.Error(t, controller.reconcileDeletion(rcx))

	var deleted []string
	raw.PrependReactor("delete", "deployments", func(action k8stesting.Action) (bool, apimachineryruntime.Object, error) {
		deleted = append(deleted, action.(k8stesting.DeleteAction).GetName())
		return true, nil, nil
	})
	require.Error(t, controller.reconcileDeletion(rcx))
	assert.Equal(t, []string{"a"}, deleted)
}

func TestReconcileDeletionTerminatingHighestOrderBlocksLowerOrders(t *testing.T) {
	instance := newInstanceObject("demo", "default")
	nodeA := &graph.Node{Meta: graph.NodeMeta{ID: "a", Type: graph.NodeTypeResource, GVR: controllerTestDeployGVR, Namespaced: true}}
	nodeB := &graph.Node{Meta: graph.NodeMeta{ID: "b", Type: graph.NodeTypeResource, GVR: controllerTestDeployGVR, Namespaced: true}}
	a := newManagedObject(newDeploymentObject("a", "default"), instance, "a", 1)
	b := newManagedObject(newDeploymentObject("b", "default"), instance, "b", 2)
	now := metav1.Now()
	b.SetDeletionTimestamp(&now)

	controller, rcx, raw := newControllerAndContext(t, instance, newTestGraph(nodeA, nodeB), a, b)
	deletes := 0
	raw.PrependReactor("delete", "deployments", func(action k8stesting.Action) (bool, apimachineryruntime.Object, error) {
		deletes++
		return true, nil, nil
	})

	require.Error(t, controller.reconcileDeletion(rcx))
	assert.Zero(t, deletes)
}

func TestReconcileDeletionDeletesAllResourcesInHighestWave(t *testing.T) {
	instance := newInstanceObject("demo", "default")
	node := &graph.Node{Meta: graph.NodeMeta{ID: "workers", Type: graph.NodeTypeCollection, GVR: controllerTestDeployGVR, Namespaced: true}}
	one := newManagedObject(newDeploymentObject("one", "default"), instance, "workers", 3)
	two := newManagedObject(newDeploymentObject("two", "default"), instance, "workers", 3)

	controller, rcx, raw := newControllerAndContext(t, instance, newTestGraph(node), one, two)
	var deleted []string
	raw.PrependReactor("delete", "deployments", func(action k8stesting.Action) (bool, apimachineryruntime.Object, error) {
		deleted = append(deleted, action.(k8stesting.DeleteAction).GetName())
		return true, nil, nil
	})

	require.Error(t, controller.reconcileDeletion(rcx))
	sort.Strings(deleted)
	assert.Equal(t, []string{"one", "two"}, deleted)
}

func TestReconcileDeletionRejectsInvalidOrderBeforeDeleting(t *testing.T) {
	tests := []struct {
		name string
		raw  *string
	}{
		{name: "missing"},
		{name: "malformed", raw: ptr.To("later")},
		{name: "zero", raw: ptr.To("0")},
		{name: "negative", raw: ptr.To("-1")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			instance := newInstanceObject("demo", "default")
			nodeA := &graph.Node{Meta: graph.NodeMeta{ID: "a", Type: graph.NodeTypeResource, GVR: controllerTestDeployGVR, Namespaced: true}}
			nodeB := &graph.Node{Meta: graph.NodeMeta{ID: "b", Type: graph.NodeTypeResource, GVR: controllerTestDeployGVR, Namespaced: true}}
			a := newManagedObject(newDeploymentObject("a", "default"), instance, "a", 1)
			b := newManagedObject(newDeploymentObject("b", "default"), instance, "b", 2)
			labels := b.GetLabels()
			delete(labels, metadata.ApplyOrderLabel)
			if tt.raw != nil {
				labels[metadata.ApplyOrderLabel] = *tt.raw
			}
			b.SetLabels(labels)

			controller, rcx, raw := newControllerAndContext(t, instance, newTestGraph(nodeA, nodeB), a, b)
			deletes := 0
			raw.PrependReactor("delete", "deployments", func(action k8stesting.Action) (bool, apimachineryruntime.Object, error) {
				deletes++
				return true, nil, nil
			})

			err := controller.reconcileDeletion(rcx)
			require.Error(t, err)
			assert.Contains(t, err.Error(), metadata.ApplyOrderLabel)
			assert.Contains(t, err.Error(), "default/b")
			assert.Zero(t, deletes)
			assert.Equal(t, v1alpha1.NodeStateError, rcx.StateManager.NodeStates["b"].State)
		})
	}
}

func TestReconcileDeletionUIDConflictDoesNotAdvance(t *testing.T) {
	instance := newInstanceObject("demo", "default")
	nodeA := &graph.Node{Meta: graph.NodeMeta{ID: "a", Type: graph.NodeTypeResource, GVR: controllerTestDeployGVR, Namespaced: true}}
	nodeB := &graph.Node{Meta: graph.NodeMeta{ID: "b", Type: graph.NodeTypeResource, GVR: controllerTestDeployGVR, Namespaced: true}}
	a := newManagedObject(newDeploymentObject("a", "default"), instance, "a", 1)
	b := newManagedObject(newDeploymentObject("b", "default"), instance, "b", 2)

	controller, rcx, raw := newControllerAndContext(t, instance, newTestGraph(nodeA, nodeB), a, b)
	var deleted []string
	raw.PrependReactor("delete", "deployments", func(action k8stesting.Action) (bool, apimachineryruntime.Object, error) {
		name := action.(k8stesting.DeleteAction).GetName()
		deleted = append(deleted, name)
		return true, nil, apierrors.NewConflict(schema.GroupResource{Group: "apps", Resource: "deployments"}, name, errors.New("UID mismatch"))
	})

	err := controller.reconcileDeletion(rcx)
	var retryAfter *requeue.RequeueNeededAfter
	require.ErrorAs(t, err, &retryAfter)
	assert.Equal(t, []string{"b"}, deleted)
}

func TestDeletionInventoryIncludesRemovedGraphNodes(t *testing.T) {
	instance := newInstanceObject("demo", "default")
	current := &graph.Node{Meta: graph.NodeMeta{ID: "current", Type: graph.NodeTypeResource, GVR: controllerTestDeployGVR, Namespaced: true}}
	removed := newManagedObject(newDeploymentObject("removed", "default"), instance, "old-node", 4)
	controller, rcx, _ := newControllerAndContext(t, instance, newTestGraph(current), removed)

	candidates, _, err := controller.discoverDeletionInventory(rcx)
	require.NoError(t, err)
	require.Len(t, candidates, 1)
	assert.Equal(t, "removed", candidates[0].Object.GetName())
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
