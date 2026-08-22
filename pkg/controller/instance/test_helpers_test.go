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
	"fmt"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	apimachineryruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	metadatafake "k8s.io/client-go/metadata/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"
	"k8s.io/kube-openapi/pkg/validation/spec"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	clientfake "github.com/kubernetes-sigs/kro/pkg/client/fake"
	"github.com/kubernetes-sigs/kro/pkg/dynamiccontroller"
	"github.com/kubernetes-sigs/kro/pkg/graph"
	"github.com/kubernetes-sigs/kro/pkg/graph/dag"
	"github.com/kubernetes-sigs/kro/pkg/graph/revisions"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
	watch "github.com/kubernetes-sigs/kro/pkg/watch"
)

var (
	controllerTestParentGVR = schema.GroupVersionResource{Group: "kro.run", Version: "v1alpha1", Resource: "webapps"}
	controllerTestParentGVK = schema.GroupVersionKind{Group: "kro.run", Version: "v1alpha1", Kind: "WebApp"}
	controllerTestDeployGVR = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	controllerTestDeployGVK = schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}
	controllerTestCMGVR     = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}
	controllerTestCMGVK     = schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}
)

func buildControllerTestRESTMapper() meta.RESTMapper {
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{
		{Group: "", Version: "v1"},
		{Group: "apps", Version: "v1"},
		{Group: "kro.run", Version: "v1alpha1"},
	})
	mapper.Add(controllerTestCMGVK, meta.RESTScopeNamespace)
	mapper.Add(controllerTestDeployGVK, meta.RESTScopeNamespace)
	mapper.Add(controllerTestParentGVK, meta.RESTScopeNamespace)
	return mapper
}

func newControllerTestDynamicClient(t *testing.T, objs ...apimachineryruntime.Object) *dynamicfake.FakeDynamicClient {
	t.Helper()

	scheme := apimachineryruntime.NewScheme()

	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		scheme,
		map[schema.GroupVersionResource]string{
			controllerTestParentGVR: "WebAppList",
			controllerTestDeployGVR: "DeploymentList",
			controllerTestCMGVR:     "ConfigMapList",
		},
		objs...,
	)
	addApplyReactor(client)
	return client
}

func addApplyReactor(client *dynamicfake.FakeDynamicClient) {
	var rvCounter int64

	client.PrependReactor("patch", "*", func(action k8stesting.Action) (bool, apimachineryruntime.Object, error) {
		patchAction, ok := action.(k8stesting.PatchAction)
		if !ok || patchAction.GetPatchType() != types.ApplyPatchType {
			return false, nil, nil
		}

		obj := &unstructured.Unstructured{}
		if err := obj.UnmarshalJSON(patchAction.GetPatch()); err != nil {
			return true, nil, err
		}

		gvr := action.GetResource()
		namespace := action.GetNamespace()
		var (
			current *unstructured.Unstructured
			create  bool
		)
		stored, err := client.Tracker().Get(gvr, namespace, patchAction.GetName())
		if err != nil {
			if !apierrors.IsNotFound(err) {
				return true, nil, err
			}
			current = &unstructured.Unstructured{Object: map[string]interface{}{}}
			create = true
		} else {
			existing, ok := stored.(*unstructured.Unstructured)
			if !ok {
				return true, nil, fmt.Errorf("unexpected object type %T", stored)
			}
			current = existing.DeepCopy()
		}

		mergeMaps(current.Object, obj.Object)
		current.SetGroupVersionKind(obj.GroupVersionKind())
		if current.GetName() == "" {
			current.SetName(patchAction.GetName())
		}
		if current.GetNamespace() == "" {
			current.SetNamespace(namespace)
		}
		if current.GetUID() == "" {
			current.SetUID(types.UID("uid-" + current.GetName()))
		}
		current.SetResourceVersion(strconv.FormatInt(atomic.AddInt64(&rvCounter, 1), 10))

		if create {
			if err := client.Tracker().Create(gvr, current, namespace); err != nil {
				return true, nil, err
			}
		} else if err := client.Tracker().Update(gvr, current, namespace); err != nil {
			return true, nil, err
		}

		return true, current.DeepCopy(), nil
	})
}

func mergeMaps(dst, src map[string]interface{}) {
	for key, value := range src {
		srcMap, srcIsMap := value.(map[string]interface{})
		dstMap, dstIsMap := dst[key].(map[string]interface{})
		if srcIsMap && dstIsMap {
			mergeMaps(dstMap, srcMap)
			continue
		}
		dst[key] = value
	}
}

func newControllerTestCoordinator(t *testing.T) *dynamiccontroller.WatchCoordinator {
	t.Helper()

	scheme := apimachineryruntime.NewScheme()
	require.NoError(t, metav1.AddMetaToScheme(scheme))

	log := zap.New(zap.UseDevMode(true))
	metadataClient := metadatafake.NewSimpleMetadataClient(scheme)

	var coord *dynamiccontroller.WatchCoordinator
	watches := watch.NewManager(metadataClient, time.Hour, func(event watch.Event) {
		if coord != nil {
			coord.RouteEvent(event)
		}
	}, log)

	coord = dynamiccontroller.NewWatchCoordinator(watches, func(schema.GroupVersionResource, types.NamespacedName) {}, log)
	return coord
}

func newControllerUnderTest(t *testing.T, raw *dynamicfake.FakeDynamicClient, g *graph.Graph) (*Controller, *clientfake.FakeSet) {
	t.Helper()

	clientSet := clientfake.NewFakeSet(raw)
	clientSet.SetRESTMapper(buildControllerTestRESTMapper())
	registry := revisions.NewRegistry()
	registry.Put(revisions.Entry{
		OwnerKey:      controllerTestParentGVR.Resource,
		Revision:      1,
		State:         revisions.RevisionStateActive,
		CompiledGraph: g,
	})

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
		nil, // graphEngineClient: nil in tests (flag off)
	)

	return controller, clientSet
}

func newControllerAndDeletionContext(
	t *testing.T,
	instance *unstructured.Unstructured,
	g *graph.Graph,
	extraObjs ...apimachineryruntime.Object,
) (*Controller, *DeletionContext, *dynamicfake.FakeDynamicClient) {
	t.Helper()

	objs := append([]apimachineryruntime.Object{instance.DeepCopy()}, extraObjs...)
	raw := newControllerTestDynamicClient(t, objs...)
	controller, clientSet := newControllerUnderTest(t, raw, g)

	dcx := NewDeletionContext(
		context.Background(),
		controller.log,
		controllerTestParentGVR,
		instance.GetNamespace() != "",
		clientSet.Dynamic(),
		clientSet.RESTMapper(),
		controller.reconcileConfig,
		instance.DeepCopy(),
	)

	return controller, dcx, raw
}

func newInstanceObject(name, namespace string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": controllerTestParentGVK.GroupVersion().String(),
			"kind":       controllerTestParentGVK.Kind,
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": namespace,
			},
			"spec": map[string]interface{}{},
		},
	}
	obj.SetGroupVersionKind(controllerTestParentGVK)
	obj.SetUID(types.UID(name + "-uid"))
	obj.SetResourceVersion("1")
	return obj
}

func newDeploymentObject(name, namespace string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": controllerTestDeployGVK.GroupVersion().String(),
			"kind":       controllerTestDeployGVK.Kind,
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": namespace,
			},
		},
	}
	obj.SetGroupVersionKind(controllerTestDeployGVK)
	return obj
}

func newConfigMapObject(name, namespace string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": controllerTestCMGVK.GroupVersion().String(),
			"kind":       controllerTestCMGVK.Kind,
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": namespace,
			},
			"data": map[string]interface{}{
				"value": "x",
			},
		},
	}
	obj.SetGroupVersionKind(controllerTestCMGVK)
	return obj
}

func newTestGraph(nodes ...*graph.Node) *graph.Graph {
	instanceNode := &graph.Node{
		Meta: graph.NodeMeta{
			ID:         graph.InstanceNodeID,
			Type:       graph.NodeTypeInstance,
			GVR:        controllerTestParentGVR,
			Namespaced: true,
		},
		Template: &unstructured.Unstructured{
			Object: map[string]interface{}{
				"status": map[string]interface{}{},
			},
		},
	}
	return newTestGraphWithInstance(instanceNode, nodes...)
}

func newTestGraphWithInstance(instanceNode *graph.Node, nodes ...*graph.Node) *graph.Graph {
	nodeMap := make(map[string]*graph.Node, len(nodes))
	dependencyGraph := dag.NewDirectedAcyclicGraph[string]()
	resourceSchemas := map[string]*spec.Schema{
		graph.InstanceNodeID: nil,
	}
	order := make([]string, 0, len(nodes))
	for _, node := range nodes {
		nodeMap[node.Meta.ID] = node
		resourceSchemas[node.Meta.ID] = nil
		order = append(order, node.Meta.ID)
		if err := dependencyGraph.AddVertex(node.Meta.ID, node.Meta.Index); err != nil {
			panic(fmt.Sprintf("adding test graph vertex %q: %v", node.Meta.ID, err))
		}
	}
	for _, node := range nodes {
		if err := dependencyGraph.AddDependencies(node.Meta.ID, node.Meta.Dependencies); err != nil {
			panic(fmt.Sprintf("adding test graph dependencies for %q: %v", node.Meta.ID, err))
		}
	}
	layers, err := dependencyGraph.ReverseTopologicalLayers()
	if err != nil {
		panic(fmt.Sprintf("calculating test graph apply orders: %v", err))
	}
	applyOrders := make(map[string]int, len(nodes))
	for i, layer := range layers {
		for _, nodeID := range layer {
			applyOrders[nodeID] = len(layers) - i
		}
	}

	return &graph.Graph{
		DAG:              dependencyGraph,
		Instance:         instanceNode,
		Nodes:            nodeMap,
		Resources:        nodeMap,
		TopologicalOrder: order,
		ApplyOrders:      applyOrders,
		ResourceSchemas:  resourceSchemas,
	}
}

func conditionByType(t *testing.T, obj *unstructured.Unstructured, condType string) v1alpha1.Condition {
	t.Helper()

	conditions := (&unstructuredWrapper{obj}).GetConditions()
	for _, condition := range conditions {
		if condition.Type == v1alpha1.ConditionType(condType) {
			return condition
		}
	}
	t.Fatalf("condition %q not found", condType)
	return v1alpha1.Condition{}
}

func getStoredParentObject(t *testing.T, client *dynamicfake.FakeDynamicClient) *unstructured.Unstructured {
	t.Helper()

	resource := client.Resource(controllerTestParentGVR)
	obj, err := resource.Namespace("default").Get(context.Background(), "demo", metav1.GetOptions{})
	require.NoError(t, err)
	return obj
}
