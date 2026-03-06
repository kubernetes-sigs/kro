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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	apimachineryruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	krocel "github.com/kubernetes-sigs/kro/pkg/cel"
	"github.com/kubernetes-sigs/kro/pkg/controller/instance/applyset"
	"github.com/kubernetes-sigs/kro/pkg/graph"
	"github.com/kubernetes-sigs/kro/pkg/graph/variable"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
	"github.com/kubernetes-sigs/kro/pkg/requeue"
	krt "github.com/kubernetes-sigs/kro/pkg/runtime"
)

func TestProcessNodesReturnsDataPending(t *testing.T) {
	instance := newInstanceObject("demo", "default")
	pendingNode := &graph.Node{
		Meta: graph.NodeMeta{
			ID:         "deploy",
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

	controller, rcx, _ := newControllerAndContext(t, instance, newTestGraph(pendingNode))
	resources, err := controller.processNodes(rcx)
	require.Error(t, err)
	assert.True(t, krt.IsDataPending(err))
	assert.Empty(t, resources)
}

func TestReconcileNodesPaths(t *testing.T) {
	tests := []struct {
		name                  string
		node                  *graph.Node
		wantErr               string
		wantRequeue           bool
		wantState             InstanceState
		wantToolingAnnotation bool
	}{
		{
			name:                  "empty graph reconcile nodes returns cleanly",
			wantState:             InstanceStateActive,
			wantToolingAnnotation: true,
		},
		{
			name: "child apply mutation asks for requeue",
			node: &graph.Node{
				Meta: graph.NodeMeta{
					ID:         "deploy",
					Type:       graph.NodeTypeResource,
					GVR:        controllerTestDeployGVR,
					Namespaced: true,
				},
				Template: newDeploymentObject("demo", ""),
			},
			wantRequeue: true,
		},
		{
			name: "missing REST mapping requeues from project",
			node: &graph.Node{
				Meta: graph.NodeMeta{
					ID:         "mystery",
					Type:       graph.NodeTypeResource,
					GVR:        schema.GroupVersionResource{Group: "unknown.example", Version: "v1", Resource: "mysteries"},
					Namespaced: true,
				},
				Template: &unstructured.Unstructured{
					Object: map[string]interface{}{
						"apiVersion": "unknown.example/v1",
						"kind":       "Mystery",
						"metadata": map[string]interface{}{
							"name": "mystery",
						},
					},
				},
			},
			wantRequeue: true,
			wantErr:     "project failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			instance := newInstanceObject("demo", "default")
			graphToUse := newTestGraph()
			if tt.node != nil {
				graphToUse = newTestGraph(tt.node)
			}

			controller, rcx, raw := newControllerAndContext(t, instance, graphToUse)
			err := controller.reconcileNodes(rcx)

			if tt.wantRequeue {
				var retryAfter *requeue.RequeueNeededAfter
				require.ErrorAs(t, err, &retryAfter)
				if tt.wantErr != "" {
					assert.Contains(t, err.Error(), tt.wantErr)
				}
				assert.NotEqual(t, InstanceStateError, rcx.StateManager.State)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.wantState, rcx.StateManager.State)
			stored := getStoredParentObject(t, raw)
			_, ok := stored.GetAnnotations()[applyset.ApplySetToolingAnnotation]
			assert.Equal(t, tt.wantToolingAnnotation, ok)
		})
	}
}

func TestProcessNodePaths(t *testing.T) {
	disabled := false
	tests := []struct {
		name            string
		specEnabled     *bool
		node            *graph.Node
		currentObjs     []apimachineryruntime.Object
		reactorVerb     string
		reactorResource string
		reactorErr      string
		wantResources   int
		wantSkipApply   bool
		wantState       string
		wantErr         string
	}{
		{
			name:        "ignored node becomes skip apply",
			specEnabled: &disabled,
			node: &graph.Node{
				Meta: graph.NodeMeta{
					ID:         "deploy",
					Type:       graph.NodeTypeResource,
					GVR:        controllerTestDeployGVR,
					Namespaced: true,
				},
				Template: newDeploymentObject("demo", ""),
				IncludeWhen: []*krocel.Expression{
					mustCompileControllerExpr(t, "schema.spec.enabled", "schema"),
				},
			},
			wantResources: 1,
			wantSkipApply: true,
			wantState:     NodeStateSkipped,
		},
		{
			name: "resource get error marks node error",
			node: &graph.Node{
				Meta: graph.NodeMeta{
					ID:         "deploy",
					Type:       graph.NodeTypeResource,
					GVR:        controllerTestDeployGVR,
					Namespaced: true,
				},
				Template: newDeploymentObject("demo", ""),
			},
			reactorVerb:     "get",
			reactorResource: "deployments",
			reactorErr:      "get failed",
			wantState:       NodeStateError,
			wantErr:         "get failed",
		},
		{
			name: "external ref waits for missing object",
			node: &graph.Node{
				Meta: graph.NodeMeta{
					ID:         "external",
					Type:       graph.NodeTypeExternal,
					GVR:        controllerTestCMGVR,
					Namespaced: true,
				},
				Template: newConfigMapObject("missing", ""),
			},
			wantState: NodeStateWaitingForReadiness,
		},
		{
			name: "external ref becomes ready when object exists",
			node: &graph.Node{
				Meta: graph.NodeMeta{
					ID:         "external",
					Type:       graph.NodeTypeExternal,
					GVR:        controllerTestCMGVR,
					Namespaced: true,
				},
				Template: newConfigMapObject("present", ""),
			},
			currentObjs: []apimachineryruntime.Object{newConfigMapObject("present", "default")},
			wantState:   NodeStateSynced,
		},
		{
			name: "desired resolution errors mark node error",
			node: &graph.Node{
				Meta: graph.NodeMeta{
					ID:         "deploy",
					Type:       graph.NodeTypeResource,
					GVR:        controllerTestDeployGVR,
					Namespaced: true,
				},
				Template: &unstructured.Unstructured{
					Object: map[string]interface{}{
						"apiVersion": controllerTestDeployGVK.GroupVersion().String(),
						"kind":       controllerTestDeployGVK.Kind,
						"metadata": map[string]interface{}{
							"name": "${1 / 0}",
						},
					},
				},
				Variables: []*variable.ResourceField{
					standaloneField("metadata.name", mustCompileControllerExpr(t, "1 / 0"), variable.ResourceVariableKindStatic),
				},
			},
			wantState: NodeStateError,
			wantErr:   "division by zero",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			instance := newInstanceObject("demo", "default")
			if tt.specEnabled != nil {
				_ = unstructured.SetNestedField(instance.Object, *tt.specEnabled, "spec", "enabled")
			}

			controller, rcx, raw := newControllerAndContext(t, instance, newTestGraph(tt.node), tt.currentObjs...)
			if tt.reactorErr != "" {
				raw.PrependReactor(tt.reactorVerb, tt.reactorResource, func(action k8stesting.Action) (bool, apimachineryruntime.Object, error) {
					return true, nil, errors.New(tt.reactorErr)
				})
			}

			resources, err := controller.processNode(rcx, rcx.Runtime.Nodes()[0])
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
			} else {
				require.NoError(t, err)
			}

			assert.Len(t, resources, tt.wantResources)
			if tt.wantResources > 0 {
				assert.Equal(t, tt.wantSkipApply, resources[0].SkipApply)
			}

			assert.Equal(t, tt.wantState, rcx.StateManager.NodeStates[tt.node.Meta.ID].State)
		})
	}
}

func TestProcessNodeCollectionTypes(t *testing.T) {
	instance := newInstanceObject("demo", "default")
	_ = unstructured.SetNestedSlice(instance.Object, []interface{}{"one"}, "spec", "items")

	collectionNode := newCollectionNodeForResources(t, "configs")
	externalCollection := newExternalCollectionNodeForResources(t, nil)

	currentCollection := newConfigMapObject("one", "default")
	currentCollection.SetLabels(map[string]string{
		metadata.InstanceIDLabel: string(instance.GetUID()),
		metadata.NodeIDLabel:     "configs",
	})
	currentExternal := newConfigMapObject("ext", "default")
	currentExternal.SetLabels(map[string]string{"app": "demo"})

	controller, rcx, _ := newControllerAndContext(t, instance, newTestGraph(collectionNode, externalCollection), currentCollection, currentExternal)
	resources, err := controller.processNode(rcx, rcx.Runtime.Nodes()[0])
	require.NoError(t, err)
	require.Len(t, resources, 1)
	assert.Equal(t, "configs-0", resources[0].ID)

	resources, err = controller.processNode(rcx, rcx.Runtime.Nodes()[1])
	require.NoError(t, err)
	assert.Nil(t, resources)
	assert.Equal(t, NodeStateSynced, rcx.StateManager.NodeStates["external-configs"].State)
}

func TestCollectionAndExternalCollectionProcessing(t *testing.T) {
	instance := newInstanceObject("demo", "default")
	_ = unstructured.SetNestedSlice(instance.Object, []interface{}{"one", "two"}, "spec", "items")

	collectionNode := &graph.Node{
		Meta: graph.NodeMeta{
			ID:         "configs",
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

	externalCollection := &graph.Node{
		Meta: graph.NodeMeta{
			ID:         "external-configs",
			Type:       graph.NodeTypeExternalCollection,
			GVR:        controllerTestCMGVR,
			Namespaced: true,
		},
		Template: &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": controllerTestCMGVK.GroupVersion().String(),
				"kind":       controllerTestCMGVK.Kind,
				"metadata": map[string]interface{}{
					"selector": map[string]interface{}{
						"matchLabels": map[string]interface{}{
							"app": "demo",
						},
					},
				},
			},
		},
	}

	current := newConfigMapObject("one", "default")
	current.SetLabels(map[string]string{
		metadata.InstanceIDLabel: string(instance.GetUID()),
		metadata.NodeIDLabel:     "configs",
		"app":                    "demo",
	})
	matchingExternal := newConfigMapObject("ext", "default")
	matchingExternal.SetLabels(map[string]string{"app": "demo"})

	controller, rcx, _ := newControllerAndContext(t, instance, newTestGraph(collectionNode, externalCollection), current, matchingExternal)

	collectionRuntimeNode := rcx.Runtime.Nodes()[0]
	desired, err := collectionRuntimeNode.GetDesired()
	require.NoError(t, err)
	resources, err := controller.processCollectionNode(rcx, collectionRuntimeNode, rcx.StateManager.NewNodeState("configs"), desired)
	require.NoError(t, err)
	require.Len(t, resources, 2)
	assert.Equal(t, "one", resources[0].Object.GetName())
	assert.NotNil(t, resources[0].Current)
	assert.Equal(t, "0", resources[0].Object.GetLabels()[metadata.CollectionIndexLabel])
	assert.Equal(t, "2", resources[0].Object.GetLabels()[metadata.CollectionSizeLabel])

	err = controller.processExternalCollectionNode(
		rcx,
		rcx.Runtime.Nodes()[1],
		rcx.StateManager.NewNodeState("external-configs"),
		[]*unstructured.Unstructured{externalCollection.Template.DeepCopy()},
	)
	require.NoError(t, err)
	assert.Equal(t, NodeStateSynced, rcx.StateManager.NodeStates["external-configs"].State)
}

func TestProcessExternalRefNodePaths(t *testing.T) {
	tests := []struct {
		name        string
		desired     []*unstructured.Unstructured
		currentObjs []apimachineryruntime.Object
		reactorErr  string
		wantState   string
		wantErr     string
	}{
		{
			name:      "skips when the desired list is empty",
			wantState: NodeStateSkipped,
		},
		{
			name:       "marks error when get fails",
			desired:    []*unstructured.Unstructured{newConfigMapObject("demo", "default")},
			reactorErr: "get failed",
			wantState:  NodeStateError,
			wantErr:    "get failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			instance := newInstanceObject("demo", "default")
			node := &graph.Node{
				Meta: graph.NodeMeta{
					ID:         "external",
					Type:       graph.NodeTypeExternal,
					GVR:        controllerTestCMGVR,
					Namespaced: true,
				},
				Template: newConfigMapObject("demo", ""),
			}

			controller, rcx, raw := newControllerAndContext(t, instance, newTestGraph(node), tt.currentObjs...)
			if tt.reactorErr != "" {
				raw.PrependReactor("get", "configmaps", func(action k8stesting.Action) (bool, apimachineryruntime.Object, error) {
					return true, nil, errors.New(tt.reactorErr)
				})
			}

			state := rcx.StateManager.NewNodeState(tt.name)
			err := controller.processExternalRefNode(rcx, rcx.Runtime.Nodes()[0], state, tt.desired)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tt.wantState, state.State)
		})
	}
}

func TestProcessExternalRefNodeWaitsForReadiness(t *testing.T) {
	instance := newInstanceObject("demo", "default")
	node := &graph.Node{
		Meta: graph.NodeMeta{
			ID:         "external",
			Type:       graph.NodeTypeExternal,
			GVR:        controllerTestCMGVR,
			Namespaced: true,
		},
		Template: newConfigMapObject("demo", ""),
		ReadyWhen: []*krocel.Expression{
			mustCompileControllerExpr(t, "external.metadata.labels.ready == 'true'", "external"),
		},
	}

	controller, rcx, _ := newControllerAndContext(t, instance, newTestGraph(node), newConfigMapObject("demo", "default"))
	state := rcx.StateManager.NewNodeState("external")
	err := controller.processExternalRefNode(rcx, rcx.Runtime.Nodes()[0], state, []*unstructured.Unstructured{newConfigMapObject("demo", "default")})
	require.NoError(t, err)
	assert.Equal(t, NodeStateWaitingForReadiness, state.State)
}

func TestProcessExternalCollectionNodePaths(t *testing.T) {
	tests := []struct {
		name        string
		node        *graph.Node
		desired     []*unstructured.Unstructured
		currentObjs []apimachineryruntime.Object
		reactorErr  string
		wantState   string
		wantErr     string
	}{
		{
			name:      "skips when the desired list is empty",
			node:      newExternalCollectionNodeForResources(t, nil),
			wantState: NodeStateSkipped,
		},
		{
			name:       "marks error when list fails",
			node:       newExternalCollectionNodeForResources(t, nil),
			desired:    []*unstructured.Unstructured{newConfigMapObject("demo", "default")},
			reactorErr: "list failed",
			wantState:  NodeStateError,
			wantErr:    "list failed",
		},
		{
			name: "marks error for invalid selectors",
			node: newExternalCollectionNodeForResources(t, nil),
			desired: []*unstructured.Unstructured{{
				Object: map[string]interface{}{
					"apiVersion": controllerTestCMGVK.GroupVersion().String(),
					"kind":       controllerTestCMGVK.Kind,
					"metadata": map[string]interface{}{
						"selector": map[string]interface{}{
							"matchExpressions": []interface{}{
								map[string]interface{}{
									"key":      "app",
									"operator": "Bogus",
								},
							},
						},
					},
				},
			}},
			wantState: NodeStateError,
			wantErr:   "invalid label selector",
		},
		{
			name: "waits for collection readiness",
			node: newExternalCollectionNodeForResources(t, []*krocel.Expression{mustCompileControllerExpr(t, "each.metadata.labels.ready == 'true'", "each")}),
			currentObjs: []apimachineryruntime.Object{func() *unstructured.Unstructured {
				obj := newConfigMapObject("match", "default")
				obj.SetLabels(map[string]string{"app": "demo"})
				return obj
			}()},
			desired: []*unstructured.Unstructured{func() *unstructured.Unstructured {
				obj := newConfigMapObject("demo", "default")
				obj.Object["metadata"].(map[string]interface{})["selector"] = map[string]interface{}{
					"matchLabels": map[string]interface{}{"app": "demo"},
				}
				return obj
			}()},
			wantState: NodeStateWaitingForReadiness,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			instance := newInstanceObject("demo", "default")
			controller, rcx, raw := newControllerAndContext(t, instance, newTestGraph(tt.node), tt.currentObjs...)
			if tt.reactorErr != "" {
				raw.PrependReactor("list", "configmaps", func(action k8stesting.Action) (bool, apimachineryruntime.Object, error) {
					return true, nil, errors.New(tt.reactorErr)
				})
			}

			state := rcx.StateManager.NewNodeState(tt.name)
			err := controller.processExternalCollectionNode(rcx, rcx.Runtime.Nodes()[0], state, tt.desired)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tt.wantState, state.State)
		})
	}
}

func TestApplyDecoratorLabelsAndPatchMetadata(t *testing.T) {
	instance := newInstanceObject("demo", "default")
	controller, rcx, raw := newControllerAndContext(t, instance, newTestGraph())

	conflictingLabeler := metadata.GenericLabeler{
		metadata.InstanceIDLabel: "conflict",
	}
	rcx.Labeler = conflictingLabeler

	obj := newConfigMapObject("demo", "default")
	obj.SetLabels(map[string]string{"keep": "yes"})
	controller.applyDecoratorLabels(rcx, obj, "configs", &CollectionInfo{Index: 1, Size: 3})

	assert.Equal(t, "yes", obj.GetLabels()["keep"])
	assert.Equal(t, "configs", obj.GetLabels()[metadata.NodeIDLabel])
	assert.Equal(t, "1", obj.GetLabels()[metadata.CollectionIndexLabel])
	assert.Equal(t, string(instance.GetUID()), obj.GetLabels()[metadata.InstanceIDLabel])
	assert.Empty(t, obj.GetLabels()[metadata.ManagedByLabelKey])

	require.NoError(t, controller.patchInstanceWithApplySetMetadata(rcx, applyset.Metadata{
		ID:                   "demo",
		Tooling:              "tests",
		GroupKinds:           sets.New[schema.GroupKind](controllerTestDeployGVK.GroupKind()),
		AdditionalNamespaces: sets.New[string]("other"),
	}))
	stored := getStoredParentObject(t, raw)
	assert.Equal(t, "demo", stored.GetLabels()[applyset.ApplySetParentIDLabel])
}

func TestPruneOrphansPaths(t *testing.T) {
	tests := []struct {
		name            string
		reactorVerb     string
		reactorResource string
		reactorErr      error
		batchMeta       applyset.Metadata
		wantPruned      bool
		wantRetry       bool
		wantErr         string
	}{
		{
			name:            "list errors are requeued",
			reactorVerb:     "list",
			reactorResource: "configmaps",
			reactorErr:      errors.New("list failed"),
			wantErr:         "prune failed",
		},
		{
			name:            "uid conflicts request retry without error",
			reactorVerb:     "delete",
			reactorResource: "configmaps",
			reactorErr:      apierrors.NewConflict(controllerTestCMGVR.GroupResource(), "orphan", errors.New("uid mismatch")),
			wantPruned:      false,
			wantRetry:       true,
		},
		{
			name:            "shrink metadata patch failures are tolerated",
			reactorVerb:     "patch",
			reactorResource: "webapps",
			reactorErr:      errors.New("patch failed"),
			batchMeta: applyset.Metadata{
				ID:                   "demo",
				GroupKinds:           sets.New[schema.GroupKind](controllerTestCMGVK.GroupKind()),
				AdditionalNamespaces: sets.New[string]("default"),
			},
			wantPruned: true,
			wantRetry:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			instance := newInstanceObject("demo", "default")
			orphan := newApplysetManagedConfigMap(instance, "orphan", "default")
			controller, rcx, raw := newControllerAndContext(t, instance, newTestGraph(), orphan)
			raw.PrependReactor(tt.reactorVerb, tt.reactorResource, func(action k8stesting.Action) (bool, apimachineryruntime.Object, error) {
				return true, nil, tt.reactorErr
			})

			applier := controller.createApplySet(rcx)
			pruned, retry, err := controller.pruneOrphans(rcx, applier, &applyset.ApplyResult{}, applyset.Metadata{
				GroupKinds: sets.New[schema.GroupKind](controllerTestCMGVK.GroupKind()),
			}, tt.batchMeta)

			if tt.wantErr != "" {
				var retryAfter *requeue.RequeueNeededAfter
				require.ErrorAs(t, err, &retryAfter)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.wantPruned, pruned)
			assert.Equal(t, tt.wantRetry, retry)
		})
	}
}

func TestProcessApplyResultsAndReadiness(t *testing.T) {
	instance := newInstanceObject("demo", "default")
	_ = unstructured.SetNestedSlice(instance.Object, []interface{}{"one"}, "spec", "items")

	resourceNode := &graph.Node{
		Meta: graph.NodeMeta{
			ID:         "deploy",
			Type:       graph.NodeTypeResource,
			GVR:        controllerTestDeployGVR,
			Namespaced: true,
		},
		Template: newDeploymentObject("demo", ""),
		ReadyWhen: []*krocel.Expression{
			mustCompileControllerExpr(t, "deploy.metadata.labels.ready == 'true'", "deploy"),
		},
	}
	collectionNode := &graph.Node{
		Meta: graph.NodeMeta{
			ID:         "configs",
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

	controller, rcx, _ := newControllerAndContext(t, instance, newTestGraph(resourceNode, collectionNode))
	_, err := rcx.Runtime.Nodes()[1].GetDesired()
	require.NoError(t, err)

	rcx.StateManager.NewNodeState("deploy")
	rcx.StateManager.NewNodeState("configs")

	err = controller.processApplyResults(rcx, &applyset.ApplyResult{
		Applied: []applyset.ApplyResultItem{
			{
				ID:       "deploy",
				Observed: newDeploymentObject("demo", "default"),
			},
			{
				ID:    "configs-0",
				Error: errors.New("collection failed"),
			},
		},
	})
	require.Error(t, err)
	assert.Equal(t, NodeStateWaitingForReadiness, rcx.StateManager.NodeStates["deploy"].State)
	assert.Equal(t, NodeStateError, rcx.StateManager.NodeStates["configs"].State)

	waiting := &NodeState{}
	setStateFromReadiness(rcx.Runtime.Nodes()[0], waiting)
	assert.Equal(t, NodeStateWaitingForReadiness, waiting.State)

	errorNode := &graph.Node{
		Meta: graph.NodeMeta{
			ID:         "bad",
			Type:       graph.NodeTypeResource,
			GVR:        controllerTestDeployGVR,
			Namespaced: true,
		},
		Template: newDeploymentObject("bad", ""),
		ReadyWhen: []*krocel.Expression{
			mustCompileControllerExpr(t, "1"),
		},
	}
	errState := &NodeState{}
	controller, rcx, _ = newControllerAndContext(t, instance, newTestGraph(errorNode))
	rcx.Runtime.Nodes()[0].SetObserved([]*unstructured.Unstructured{newDeploymentObject("bad", "default")})
	setStateFromReadiness(rcx.Runtime.Nodes()[0], errState)
	assert.Equal(t, NodeStateError, errState.State)

	controller, rcx, _ = newControllerAndContext(t, instance, newTestGraph(resourceNode))
	rcx.StateManager.NewNodeState("deploy")
	err = controller.processApplyResults(rcx, &applyset.ApplyResult{
		Applied: []applyset.ApplyResultItem{{
			ID:    "deploy",
			Error: errors.New("apply failed"),
		}},
	})
	require.Error(t, err)
	assert.Equal(t, NodeStateError, rcx.StateManager.NodeStates["deploy"].State)
}

func TestUpdateCollectionFromApplyResultsPaths(t *testing.T) {
	tests := []struct {
		name      string
		items     []interface{}
		results   map[string]applyset.ApplyResultItem
		wantState string
	}{
		{
			name:      "handles empty collections as ready",
			items:     []interface{}{},
			results:   map[string]applyset.ApplyResultItem{},
			wantState: NodeStateSynced,
		},
		{
			name:  "sets ready when observed items are present",
			items: []interface{}{"one"},
			results: map[string]applyset.ApplyResultItem{
				"configs-0": {Observed: newConfigMapObject("one", "default")},
			},
			wantState: NodeStateSynced,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			instance := newInstanceObject("demo", "default")
			_ = unstructured.SetNestedSlice(instance.Object, tt.items, "spec", "items")

			collection := newCollectionNodeForResources(t, "configs")
			controller, rcx, _ := newControllerAndContext(t, instance, newTestGraph(collection))
			node := rcx.Runtime.Nodes()[0]
			_, err := node.GetDesired()
			require.NoError(t, err)

			state := rcx.StateManager.NewNodeState("configs")
			require.NoError(t, controller.updateCollectionFromApplyResults(rcx, node, state, tt.results))
			assert.Equal(t, tt.wantState, state.State)
		})
	}
}

func TestUpdateCollectionFromApplyResultsErrorAndPendingPaths(t *testing.T) {
	tests := []struct {
		name      string
		node      *graph.Node
		items     []interface{}
		wantState string
		wantErr   string
	}{
		{
			name:      "returns nil while collection desired data is still pending",
			node:      newCollectionNodeForResources(t, "configs"),
			wantState: NodeStateInProgress,
		},
		{
			name: "marks error when collection desired resolution fails",
			node: &graph.Node{
				Meta: graph.NodeMeta{
					ID:         "configs",
					Type:       graph.NodeTypeCollection,
					GVR:        controllerTestCMGVR,
					Namespaced: true,
				},
				Template: &unstructured.Unstructured{
					Object: map[string]interface{}{
						"apiVersion": controllerTestCMGVK.GroupVersion().String(),
						"kind":       controllerTestCMGVK.Kind,
						"metadata": map[string]interface{}{
							"name": "${1 / 0}",
						},
					},
				},
				Variables: []*variable.ResourceField{
					standaloneField("metadata.name", mustCompileControllerExpr(t, "1 / 0"), variable.ResourceVariableKindIteration),
				},
				ForEach: []graph.ForEachDimension{{
					Name:       "item",
					Expression: mustCompileControllerExpr(t, "schema.spec.items", "schema"),
				}},
			},
			items:     []interface{}{"one"},
			wantState: NodeStateError,
			wantErr:   "division by zero",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			instance := newInstanceObject("demo", "default")
			if tt.items != nil {
				_ = unstructured.SetNestedSlice(instance.Object, tt.items, "spec", "items")
			}

			controller, rcx, _ := newControllerAndContext(t, instance, newTestGraph(tt.node))
			state := rcx.StateManager.NewNodeState("configs")
			err := controller.updateCollectionFromApplyResults(rcx, rcx.Runtime.Nodes()[0], state, map[string]applyset.ApplyResultItem{})

			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tt.wantState, state.State)
		})
	}
}

func TestWatchAndResourceHelpers(t *testing.T) {
	rcx := &ReconcileContext{
		Log:     zap.New(zap.UseDevMode(true)),
		Watcher: erroringWatcher{},
	}

	requestWatch(rcx, "deploy", controllerTestDeployGVR, "demo", "default")
	requestCollectionWatch(rcx, "configs", controllerTestCMGVR, "default", labels.Everything())

	instance := newInstanceObject("demo", "default")
	_, helperRCX, _ := newControllerAndContext(t, instance, newTestGraph())
	client := resourceClientFor(helperRCX, graph.NodeMeta{
		GVR:        controllerTestCMGVR,
		Namespaced: false,
	}, "ignored")
	assert.NotNil(t, client)
}

func TestReconcileNodesRetryBranches(t *testing.T) {
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

	controller, rcx, raw := newControllerAndContext(t, instance, newTestGraph(resourceNode))
	raw.PrependReactor("patch", "deployments", func(action k8stesting.Action) (bool, apimachineryruntime.Object, error) {
		return true, nil, errors.New("apply failed")
	})
	err := controller.reconcileNodes(rcx)
	var retryAfter *requeue.RequeueNeededAfter
	require.ErrorAs(t, err, &retryAfter)
	assert.Contains(t, err.Error(), "apply failed")

	instance = newInstanceObject("demo", "default")
	instance.SetAnnotations(map[string]string{
		applyset.ApplySetGKsAnnotation: "ConfigMap",
	})
	orphan := newApplysetManagedConfigMap(instance, "orphan", "default")
	controller, rcx, raw = newControllerAndContext(t, instance, newTestGraph(), orphan)
	raw.PrependReactor("delete", "configmaps", func(action k8stesting.Action) (bool, apimachineryruntime.Object, error) {
		return true, nil, apierrors.NewConflict(controllerTestCMGVR.GroupResource(), "orphan", errors.New("uid mismatch"))
	})
	err = controller.reconcileNodes(rcx)
	require.ErrorAs(t, err, &retryAfter)
	assert.Contains(t, err.Error(), "UID conflicts")
}

func newCollectionNodeForResources(t *testing.T, id string) *graph.Node {
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

func newExternalCollectionNodeForResources(t *testing.T, readyWhen []*krocel.Expression) *graph.Node {
	t.Helper()

	return &graph.Node{
		Meta: graph.NodeMeta{
			ID:         "external-configs",
			Type:       graph.NodeTypeExternalCollection,
			GVR:        controllerTestCMGVR,
			Namespaced: true,
		},
		Template: &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": controllerTestCMGVK.GroupVersion().String(),
				"kind":       controllerTestCMGVK.Kind,
				"metadata": map[string]interface{}{
					"selector": map[string]interface{}{
						"matchLabels": map[string]interface{}{
							"app": "demo",
						},
					},
				},
			},
		},
		ReadyWhen: readyWhen,
	}
}
