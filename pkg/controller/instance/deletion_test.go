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
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	apimachineryruntime "k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	krocel "github.com/kubernetes-sigs/kro/pkg/cel"
	"github.com/kubernetes-sigs/kro/pkg/graph"
	"github.com/kubernetes-sigs/kro/pkg/graph/variable"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
	"github.com/kubernetes-sigs/kro/pkg/requeue"
)

func TestPlanNodesForDeletionSkipsUnresolvedIdentityAndPicksLastExistingNode(t *testing.T) {
	instance := newInstanceObject("demo", "default")

	pendingNode := &graph.Node{
		Meta: graph.NodeMeta{
			ID:         "pending",
			Type:       graph.NodeTypeResource,
			GVR:        controllerTestDeployGVR,
			Namespaced: true,
		},
		Template: &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": controllerTestDeployGVK.GroupVersion().String(),
				"kind":       controllerTestDeployGVK.Kind,
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}",
				},
			},
		},
		Variables: []*variable.ResourceField{
			standaloneField("metadata.name", mustCompileControllerExpr(t, "schema.spec.name", "schema"), variable.ResourceVariableKindStatic),
		},
	}
	existingNode := &graph.Node{
		Meta: graph.NodeMeta{
			ID:         "deploy",
			Type:       graph.NodeTypeResource,
			GVR:        controllerTestDeployGVR,
			Namespaced: true,
		},
		Template: newDeploymentObject("demo", ""),
	}

	controller, rcx, _ := newControllerAndContext(t, instance, newTestGraph(pendingNode, existingNode), newDeploymentObject("demo", "default"))
	node, err := controller.planNodesForDeletion(rcx)
	require.NoError(t, err)
	require.NotNil(t, node)
	assert.Equal(t, "deploy", node.Spec.Meta.ID)
	assert.Equal(t, NodeStateDeleted, rcx.StateManager.NodeStates["pending"].State)
	assert.Equal(t, NodeStateInProgress, rcx.StateManager.NodeStates["deploy"].State)
}

func TestPlanNodesForDeletionSkipsIgnoredExternalAndMissingNodes(t *testing.T) {
	instance := newInstanceObject("demo", "default")
	_ = unstructured.SetNestedSlice(instance.Object, []interface{}{"one"}, "spec", "items")
	_ = unstructured.SetNestedField(instance.Object, false, "spec", "enabled")

	ignoredNode := &graph.Node{
		Meta: graph.NodeMeta{
			ID:         "ignored",
			Type:       graph.NodeTypeResource,
			GVR:        controllerTestDeployGVR,
			Namespaced: true,
		},
		Template: newDeploymentObject("ignored", ""),
		IncludeWhen: []*krocel.Expression{
			mustCompileControllerExpr(t, "schema.spec.enabled", "schema"),
		},
	}
	externalNode := &graph.Node{
		Meta: graph.NodeMeta{
			ID:         "external",
			Type:       graph.NodeTypeExternal,
			GVR:        controllerTestCMGVR,
			Namespaced: true,
		},
		Template: newConfigMapObject("external", ""),
	}
	collectionNode := newDeletionCollectionNode(t, "configs")
	missingNode := &graph.Node{
		Meta: graph.NodeMeta{
			ID:         "missing",
			Type:       graph.NodeTypeResource,
			GVR:        controllerTestDeployGVR,
			Namespaced: true,
		},
		Template: newDeploymentObject("missing", ""),
	}

	currentCollection := newConfigMapObject("one", "default")
	currentCollection.SetLabels(map[string]string{
		metadata.InstanceIDLabel: string(instance.GetUID()),
		metadata.NodeIDLabel:     metadata.SafeNodeID("configs"),
	})

	controller, rcx, _ := newControllerAndContext(t, instance, newTestGraph(ignoredNode, externalNode, collectionNode, missingNode), currentCollection)
	node, err := controller.planNodesForDeletion(rcx)
	require.NoError(t, err)
	require.NotNil(t, node)
	assert.Equal(t, "configs", node.Spec.Meta.ID)
	assert.Equal(t, NodeStateSkipped, rcx.StateManager.NodeStates["ignored"].State)
	assert.Equal(t, NodeStateSkipped, rcx.StateManager.NodeStates["external"].State)
	assert.Equal(t, NodeStateDeleted, rcx.StateManager.NodeStates["missing"].State)
}

func TestPlanNodesForDeletionErrors(t *testing.T) {
	tests := []struct {
		name      string
		node      *graph.Node
		configure func(*unstructured.Unstructured)
		verb      string
		resource  string
		wantErr   string
	}{
		{
			name: "list errors bubble up for collection nodes",
			node: newDeletionCollectionNode(t, "configs"),
			configure: func(instance *unstructured.Unstructured) {
				_ = unstructured.SetNestedSlice(instance.Object, []interface{}{"one"}, "spec", "items")
			},
			verb:     "list",
			resource: "configmaps",
			wantErr:  "list failed",
		},
		{
			name: "get errors bubble up for resource nodes",
			node: &graph.Node{
				Meta: graph.NodeMeta{
					ID:         "deploy",
					Type:       graph.NodeTypeResource,
					GVR:        controllerTestDeployGVR,
					Namespaced: true,
				},
				Template: newDeploymentObject("demo", ""),
			},
			verb:     "get",
			resource: "deployments",
			wantErr:  "get failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			instance := newInstanceObject("demo", "default")
			if tt.configure != nil {
				tt.configure(instance)
			}

			controller, rcx, raw := newControllerAndContext(t, instance, newTestGraph(tt.node))
			raw.PrependReactor(tt.verb, tt.resource, func(action k8stesting.Action) (bool, apimachineryruntime.Object, error) {
				return true, nil, errors.New(tt.wantErr)
			})

			_, err := controller.planNodesForDeletion(rcx)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestDeleteTarget(t *testing.T) {
	tests := []struct {
		name        string
		observed    []*unstructured.Unstructured
		deleteErr   string
		wantState   string
		wantErrText string
	}{
		{
			name:      "marks deleted when there are no targets",
			wantState: NodeStateDeleted,
		},
		{
			name:      "marks deleted when the target no longer exists",
			observed:  []*unstructured.Unstructured{newDeploymentObject("gone", "default")},
			wantState: NodeStateDeleted,
		},
		{
			name:      "marks deleting when the API accepted deletion",
			observed:  []*unstructured.Unstructured{newDeploymentObject("demo", "default")},
			wantState: NodeStateDeleting,
		},
		{
			name:        "marks error when deletion fails",
			observed:    []*unstructured.Unstructured{newDeploymentObject("demo", "default")},
			deleteErr:   "delete failed",
			wantState:   NodeStateError,
			wantErrText: "delete failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
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

			controller, rcx, raw := newControllerAndContext(t, instance, newTestGraph(resourceNode), newDeploymentObject("demo", "default"))
			node := rcx.Runtime.Nodes()[0]
			node.SetObserved(tt.observed)

			if tt.deleteErr != "" {
				raw.PrependReactor("delete", "deployments", func(action k8stesting.Action) (bool, apimachineryruntime.Object, error) {
					return true, nil, errors.New(tt.deleteErr)
				})
			}

			state := rcx.StateManager.NewNodeState(tt.name)
			err := controller.deleteTarget(rcx, node, state)
			if tt.wantErrText != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErrText)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tt.wantState, state.State)
		})
	}
}

func TestReconcileDeletionRequeuesWhileChildDeletionInFlight(t *testing.T) {
	instance := newInstanceObject("demo", "default")
	node := &graph.Node{
		Meta: graph.NodeMeta{
			ID:         "deploy",
			Type:       graph.NodeTypeResource,
			GVR:        controllerTestDeployGVR,
			Namespaced: true,
		},
		Template: newDeploymentObject("demo", ""),
	}

	controller, rcx, _ := newControllerAndContext(t, instance, newTestGraph(node), newDeploymentObject("demo", "default"))
	err := controller.reconcileDeletion(rcx)
	var retryAfter *requeue.RequeueNeededAfter
	require.ErrorAs(t, err, &retryAfter)
	assert.Equal(t, InstanceStateDeleting, rcx.StateManager.State)
}

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

func newDeletionCollectionNode(t *testing.T, id string) *graph.Node {
	t.Helper()

	return &graph.Node{
		Meta: graph.NodeMeta{
			ID:         id,
			Type:       graph.NodeTypeCollection,
			GVR:        controllerTestCMGVR,
			Namespaced: true,
		},
		Template: &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": controllerTestCMGVK.GroupVersion().String(),
				"kind":       controllerTestCMGVK.Kind,
				"metadata": map[string]interface{}{
					"name": "${item}",
				},
			},
		},
		Variables: []*variable.ResourceField{
			standaloneField("metadata.name", mustCompileControllerExpr(t, "item", "item"), variable.ResourceVariableKindIteration),
		},
		ForEach: []graph.ForEachDimension{{
			Name:       "item",
			Expression: mustCompileControllerExpr(t, "schema.spec.items", "schema"),
		}},
	}
}
