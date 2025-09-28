// Copyright 2025 The Kube Resource Orchestrator Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package applyset

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

type gvkrl struct {
	gvk      schema.GroupVersionKind
	resource string
	listKind string
}

var (
	testScheme = runtime.NewScheme()

	configMapGVK = schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}
	secretGVK    = schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Secret"}
	fooGVK       = schema.GroupVersionKind{Group: "foo.bar", Version: "v1", Kind: "Foo"}

	gvkrlList = []gvkrl{
		{gvk: configMapGVK, resource: "configmaps", listKind: "ConfigMapList"},
		{gvk: secretGVK, resource: "secrets", listKind: "SecretList"},
		{gvk: fooGVK, resource: "foos", listKind: "FooList"},
	}
)

func init() {
	testScheme.AddKnownTypes(configMapGVK.GroupVersion(), &unstructured.Unstructured{})
	testScheme.AddKnownTypes(secretGVK.GroupVersion(), &unstructured.Unstructured{})
}

func newTestApplySet(
	t *testing.T,
	parent *unstructured.Unstructured,
	objs ...runtime.Object,
) (Set, *fake.FakeDynamicClient) {
	allObjs := append([]runtime.Object{parent}, objs...)
	gvrToListKind := map[schema.GroupVersionResource]string{}
	defaultGroupVersions := []schema.GroupVersion{}
	for _, gvkrl := range gvkrlList {
		gvrToListKind[gvkrl.gvk.GroupVersion().WithResource(gvkrl.resource)] = gvkrl.listKind
		defaultGroupVersions = append(defaultGroupVersions, gvkrl.gvk.GroupVersion())
	}

	restMapper := meta.NewDefaultRESTMapper(defaultGroupVersions)
	for _, gvkrl := range gvkrlList {
		restMapper.Add(gvkrl.gvk, meta.RESTScopeNamespace)
	}
	dynamicClient := fake.NewSimpleDynamicClientWithCustomListKinds(testScheme, gvrToListKind, allObjs...)

	config := Config{
		ToolingID:    ToolingID{Name: "test", Version: "v1"},
		FieldManager: "test-manager",
		Log:          logr.Discard(),
	}

	aset, err := New(parent, restMapper, dynamicClient, config)
	assert.NoError(t, err)
	return aset, dynamicClient
}

func parentObj(gvk schema.GroupVersionKind, name string) *unstructured.Unstructured {
	apiVersion, kind := gvk.ToAPIVersionAndKind()
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": apiVersion,
			"kind":       kind,
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": "default",
				"uid":       "parent-uid",
			},
		},
	}
}

func configMap(name, ns string) ApplyableObject {
	return ApplyableObject{
		Unstructured: &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name":      name,
					"namespace": ns,
					"uid":       name + ns + "-uid",
				},
				"data": map[string]interface{}{
					"key": "value",
				},
			},
		},
		ID: name + ns,
	}
}

func foo(name, ns string) ApplyableObject {
	return ApplyableObject{
		Unstructured: &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "foo.bar/v1",
				"kind":       "Foo",
				"metadata": map[string]interface{}{
					"name":      name,
					"namespace": ns,
					"uid":       name + ns + "-uid",
				},
			},
		},
		ID: name + ns,
	}
}

func TestNew(t *testing.T) {
	parent := parentObj(configMapGVK, "parent-cm")

	t.Run("valid config", func(t *testing.T) {
		_, _ = newTestApplySet(t, parent)
	})

	t.Run("missing toolingID", func(t *testing.T) {
		_, err := New(parent, nil, nil, Config{
			FieldManager: "test-manager",
		})
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "toolingID is required")
	})

	t.Run("missing fieldManager", func(t *testing.T) {
		_, err := New(parent, nil, nil, Config{
			ToolingID: ToolingID{Name: "test", Version: "v1"},
		})
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "fieldManager is required")
	})
}

func TestApplySet_Add(t *testing.T) {
	parent := parentObj(configMapGVK, "parent-cm")

	aset, _ := newTestApplySet(t, parent)

	_, err := aset.Add(context.Background(), configMap("test-cm", "default"))
	assert.NoError(t, err)

	as := aset.(*applySet)
	assert.Equal(t, 1, as.desired.Len())
	assert.Contains(t, as.desired.objects[0].GetLabels(), ApplysetPartOfLabel)

	assert.True(t, as.desiredNamespaces.Has("default"))
	_, exists := as.desiredRESTMappings[configMapGVK.GroupKind()]
	assert.True(t, exists)
}

func TestApplySet_ID(t *testing.T) {
	parent := parentObj(fooGVK, "test-foo")
	parent.SetNamespace("test-ns")

	aset, _ := newTestApplySet(t, parent)

	// from: base64(sha256(<name>.<namespace>.<kind>.<group>))
	// test-foo.test-ns.Foo.foo.bar
	// sha256: 2dca1c8242de82132464575841648a37c74545785854614378933d2d408ddd2b
	// base64: LcocgkLeghMkZFdYQWSKN8dFRXhYVGFDeJM9LUCd3Ss
	// format: applyset-%s-v1
	expectedID := "applyset-f9Rk5tKHoB72oV1tU2iFKxDwL8MBZTjMHFQ8V9WNNlA-v1"
	assert.Equal(t, expectedID, aset.(*applySet).ID())
}

func TestApplySet_Apply(t *testing.T) {
	parent := parentObj(secretGVK, "parent-secret")

	aset, dynamicClient := newTestApplySet(t, parent)
	_, err := aset.Add(context.Background(), configMap("test-cm", "default"))
	assert.NoError(t, err)

	dynamicClient.PrependReactor("patch", "configmaps", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
		var appliedCM unstructured.Unstructured
		patchAction := action.(k8stesting.PatchAction)
		assert.Equal(t, types.ApplyPatchType, patchAction.GetPatchType())
		err = json.Unmarshal(patchAction.GetPatch(), &appliedCM)
		assert.NoError(t, err)
		assert.NotNil(t, appliedCM)
		assert.Equal(t, "test-cm", appliedCM.GetName())
		assert.Contains(t, appliedCM.GetLabels(), ApplysetPartOfLabel)
		// The fake client needs to return the object that was applied.
		return true, &appliedCM, nil
	})

	// Reactor for parent update
	dynamicClient.PrependReactor("patch", "secrets", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
		patchAction := action.(k8stesting.PatchAction)
		assert.Equal(t, types.ApplyPatchType, patchAction.GetPatchType())

		var parentPatch unstructured.Unstructured
		err = json.Unmarshal(patchAction.GetPatch(), &parentPatch)
		assert.NoError(t, err)
		assert.Equal(t, "parent-secret", parentPatch.GetName())
		assert.Contains(t, parentPatch.GetLabels(), ApplySetParentIDLabel)
		assert.Contains(t, parentPatch.GetAnnotations(), ApplySetAdditionalNamespacesAnnotation)
		assert.Contains(t, parentPatch.GetAnnotations(), ApplySetGKsAnnotation)
		assert.Equal(t, parentPatch.GetAnnotations()[ApplySetAdditionalNamespacesAnnotation], "default")
		assert.Equal(t, parentPatch.GetAnnotations()[ApplySetGKsAnnotation], "ConfigMap")

		return true, &parentPatch, nil
	})

	result, err := aset.Apply(context.Background(), false)
	assert.NoError(t, err)
	assert.NoError(t, result.Errors())
	assert.Equal(t, 1, result.DesiredCount)
	assert.Len(t, result.AppliedObjects, 1)
}

func TestApplySet_Prune(t *testing.T) {
	parent := parentObj(secretGVK, "parent-secret")

	// This CM will be pruned
	pruneCM := configMap("prune-cm", "default")
	pruneCM.Unstructured.SetLabels(map[string]string{
		ApplysetPartOfLabel: "applyset-wHf5Gity0G0nPN34KuNBIBBEOu2H9ED2KqsblMPFygM-v1",
	})
	aset, dynamicClient := newTestApplySet(t, parent, pruneCM)
	// Set the ApplysetPartOfLabel on the pruneCM to match the ID of the applyset
	// This is needed because the applyset ID is dynamically generated based on the parent object.
	// We need to ensure that the pruneCM has the correct label so it is discovered for pruning.
	as := aset.(*applySet)
	assert.Equal(t, "applyset-wHf5Gity0G0nPN34KuNBIBBEOu2H9ED2KqsblMPFygM-v1", as.ID())

	_, err := aset.Add(context.Background(), configMap("test-cm", "default"))
	assert.NoError(t, err)

	dynamicClient.PrependReactor("patch", "configmaps", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
		patchAction := action.(k8stesting.PatchAction)
		assert.Equal(t, types.ApplyPatchType, patchAction.GetPatchType())

		var appliedCM *unstructured.Unstructured
		err = json.Unmarshal(patchAction.GetPatch(), &appliedCM)
		assert.Equal(t, "test-cm", appliedCM.GetName())
		assert.Contains(t, appliedCM.GetLabels(), ApplysetPartOfLabel)
		assert.NoError(t, err)

		// The fake client needs to return the object that was applied.
		return true, appliedCM, nil
	})
	// Reactor for parent update
	dynamicClient.PrependReactor("patch", "secrets", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
		patchAction := action.(k8stesting.PatchAction)
		assert.Equal(t, types.ApplyPatchType, patchAction.GetPatchType())

		var parentPatch unstructured.Unstructured
		err = json.Unmarshal(patchAction.GetPatch(), &parentPatch)
		assert.NoError(t, err)
		assert.Equal(t, "parent-secret", parentPatch.GetName())
		assert.Contains(t, parentPatch.GetLabels(), ApplySetParentIDLabel)
		assert.Contains(t, parentPatch.GetAnnotations(), ApplySetAdditionalNamespacesAnnotation)
		assert.Contains(t, parentPatch.GetAnnotations(), ApplySetGKsAnnotation)
		assert.Equal(t, parentPatch.GetAnnotations()[ApplySetAdditionalNamespacesAnnotation], "default")
		assert.Equal(t, parentPatch.GetAnnotations()[ApplySetGKsAnnotation], "ConfigMap")

		return true, &parentPatch, nil
	})

	var pruned bool
	dynamicClient.PrependReactor("delete", "configmaps", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
		deleteAction := action.(k8stesting.DeleteAction)
		assert.Equal(t, "prune-cm", deleteAction.GetName())
		pruned = true
		return true, nil, nil
	})

	result, err := aset.Apply(context.Background(), true)
	assert.NoError(t, err)
	assert.NoError(t, result.Errors())
	assert.Equal(t, 1, result.DesiredCount)
	assert.Len(t, result.AppliedObjects, 1)
	assert.Len(t, result.PrunedObjects, 1)
	assert.True(t, pruned)
}

func TestApplySet_ApplyMultiNamespace(t *testing.T) {
	parent := parentObj(secretGVK, "parent-secret")

	aset, dynamicClient := newTestApplySet(t, parent)
	_, err := aset.Add(context.Background(), configMap("test-cm", "ns1"))
	assert.NoError(t, err)
	_, err = aset.Add(context.Background(), configMap("test-cm", "ns2"))
	assert.NoError(t, err)
	_, err = aset.Add(context.Background(), foo("test-foo", "ns3"))
	assert.NoError(t, err)

	dynamicClient.PrependReactor("patch", "configmaps", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
		var appliedCM unstructured.Unstructured
		patchAction := action.(k8stesting.PatchAction)
		assert.Equal(t, types.ApplyPatchType, patchAction.GetPatchType())
		err = json.Unmarshal(patchAction.GetPatch(), &appliedCM)
		assert.NoError(t, err)
		assert.NotNil(t, appliedCM)
		assert.Contains(t, "test-cm", appliedCM.GetName())
		assert.Contains(t, appliedCM.GetLabels(), ApplysetPartOfLabel)
		// The fake client needs to return the object that was applied.
		return true, &appliedCM, nil
	})

	dynamicClient.PrependReactor("patch", "foos", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
		var appliedFoo unstructured.Unstructured
		patchAction := action.(k8stesting.PatchAction)
		assert.Equal(t, types.ApplyPatchType, patchAction.GetPatchType())
		err = json.Unmarshal(patchAction.GetPatch(), &appliedFoo)
		assert.NoError(t, err)
		assert.NotNil(t, appliedFoo)
		assert.Contains(t, "test-foo", appliedFoo.GetName())
		assert.Contains(t, appliedFoo.GetLabels(), ApplysetPartOfLabel)
		// The fake client needs to return the object that was applied.
		return true, &appliedFoo, nil
	})
	// Reactor for parent update
	dynamicClient.PrependReactor("patch", "secrets", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
		patchAction := action.(k8stesting.PatchAction)
		assert.Equal(t, types.ApplyPatchType, patchAction.GetPatchType())

		var parentPatch unstructured.Unstructured
		err = json.Unmarshal(patchAction.GetPatch(), &parentPatch)
		assert.NoError(t, err)
		assert.Equal(t, "parent-secret", parentPatch.GetName())
		assert.Contains(t, parentPatch.GetLabels(), ApplySetParentIDLabel)
		assert.Contains(t, parentPatch.GetAnnotations(), ApplySetAdditionalNamespacesAnnotation)
		assert.Contains(t, parentPatch.GetAnnotations(), ApplySetGKsAnnotation)
		assert.Equal(t, parentPatch.GetAnnotations()[ApplySetAdditionalNamespacesAnnotation], "ns1,ns2,ns3")
		assert.Equal(t, parentPatch.GetAnnotations()[ApplySetGKsAnnotation], "ConfigMap,Foo.foo.bar")

		return true, &parentPatch, nil
	})

	result, err := aset.Apply(context.Background(), false)
	assert.NoError(t, err)
	assert.NoError(t, result.Errors())
	assert.Equal(t, 3, result.DesiredCount)
	assert.Len(t, result.AppliedObjects, 3)
}

func TestApplySet_PruneMultiNamespace(t *testing.T) {
	parent := parentObj(secretGVK, "parent-secret")

	// These CMs will be pruned
	pruneCM1 := configMap("prune-cm", "ns1")
	pruneCM1.Unstructured.SetLabels(map[string]string{
		ApplysetPartOfLabel: "applyset-wHf5Gity0G0nPN34KuNBIBBEOu2H9ED2KqsblMPFygM-v1",
	})
	pruneCM2 := configMap("prune-cm", "ns2")
	pruneCM2.Unstructured.SetLabels(map[string]string{
		ApplysetPartOfLabel: "applyset-wHf5Gity0G0nPN34KuNBIBBEOu2H9ED2KqsblMPFygM-v1",
	})
	aset, dynamicClient := newTestApplySet(t, parent, pruneCM1, pruneCM2)
	// Set the ApplysetPartOfLabel on the pruneCM to match the ID of the applyset
	// This is needed because the applyset ID is dynamically generated based on the parent object.
	// We need to ensure that the pruneCM has the correct label so it is discovered for pruning.
	as := aset.(*applySet)
	assert.Equal(t, "applyset-wHf5Gity0G0nPN34KuNBIBBEOu2H9ED2KqsblMPFygM-v1", as.ID())

	_, err := aset.Add(context.Background(), configMap("test-cm", "ns1"))
	assert.NoError(t, err)
	_, err = aset.Add(context.Background(), configMap("test-cm", "ns2"))
	assert.NoError(t, err)
	_, err = aset.Add(context.Background(), foo("test-foo", "ns3"))
	assert.NoError(t, err)

	dynamicClient.PrependReactor("patch", "configmaps", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
		var appliedCM unstructured.Unstructured
		patchAction := action.(k8stesting.PatchAction)
		assert.Equal(t, types.ApplyPatchType, patchAction.GetPatchType())
		err = json.Unmarshal(patchAction.GetPatch(), &appliedCM)
		assert.NoError(t, err)
		assert.NotNil(t, appliedCM)
		assert.Contains(t, "test-cm", appliedCM.GetName())
		assert.Contains(t, appliedCM.GetLabels(), ApplysetPartOfLabel)
		// The fake client needs to return the object that was applied.
		return true, &appliedCM, nil
	})

	dynamicClient.PrependReactor("patch", "foos", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
		var appliedFoo unstructured.Unstructured
		patchAction := action.(k8stesting.PatchAction)
		assert.Equal(t, types.ApplyPatchType, patchAction.GetPatchType())
		err = json.Unmarshal(patchAction.GetPatch(), &appliedFoo)
		assert.NoError(t, err)
		assert.NotNil(t, appliedFoo)
		assert.Contains(t, "test-foo", appliedFoo.GetName())
		assert.Contains(t, appliedFoo.GetLabels(), ApplysetPartOfLabel)
		// The fake client needs to return the object that was applied.
		return true, &appliedFoo, nil
	})
	// Reactor for parent update
	dynamicClient.PrependReactor("patch", "secrets", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
		patchAction := action.(k8stesting.PatchAction)
		assert.Equal(t, types.ApplyPatchType, patchAction.GetPatchType())

		var parentPatch unstructured.Unstructured
		err = json.Unmarshal(patchAction.GetPatch(), &parentPatch)
		assert.NoError(t, err)
		assert.Equal(t, "parent-secret", parentPatch.GetName())
		assert.Contains(t, parentPatch.GetLabels(), ApplySetParentIDLabel)
		assert.Contains(t, parentPatch.GetAnnotations(), ApplySetAdditionalNamespacesAnnotation)
		assert.Contains(t, parentPatch.GetAnnotations(), ApplySetGKsAnnotation)
		assert.Equal(t, parentPatch.GetAnnotations()[ApplySetAdditionalNamespacesAnnotation], "ns1,ns2,ns3")
		assert.Equal(t, parentPatch.GetAnnotations()[ApplySetGKsAnnotation], "ConfigMap,Foo.foo.bar")

		return true, &parentPatch, nil
	})

	var pruned int = 0
	dynamicClient.PrependReactor("delete", "configmaps", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
		deleteAction := action.(k8stesting.DeleteAction)
		assert.Equal(t, "prune-cm", deleteAction.GetName())
		pruned++
		return true, nil, nil
	})
	result, err := aset.Apply(context.Background(), true)
	assert.NoError(t, err)
	assert.NoError(t, result.Errors())
	assert.Equal(t, 3, result.DesiredCount)
	assert.Equal(t, pruned, 2)
	assert.Len(t, result.PrunedObjects, 2)
}

func TestApplySet_PruneOldNamespace(t *testing.T) {
	parent := parentObj(secretGVK, "parent-secret")
	// Add annotation for old namespace
	parent.SetAnnotations(map[string]string{
		"applyset.kubernetes.io/additional-namespaces": "oldns1",
	})

	// CM in old namespace should be pruned
	pruneCM1 := configMap("prune-cm", "oldns1")
	pruneCM1.Unstructured.SetLabels(map[string]string{
		ApplysetPartOfLabel: "applyset-wHf5Gity0G0nPN34KuNBIBBEOu2H9ED2KqsblMPFygM-v1",
	})
	pruneCM2 := configMap("prune-cm", "ns2")
	pruneCM2.Unstructured.SetLabels(map[string]string{
		ApplysetPartOfLabel: "applyset-wHf5Gity0G0nPN34KuNBIBBEOu2H9ED2KqsblMPFygM-v1",
	})
	aset, dynamicClient := newTestApplySet(t, parent, pruneCM1, pruneCM2)
	// Set the ApplysetPartOfLabel on the pruneCM to match the ID of the applyset
	// This is needed because the applyset ID is dynamically generated based on the parent object.
	// We need to ensure that the pruneCM has the correct label so it is discovered for pruning.
	as := aset.(*applySet)
	assert.Equal(t, "applyset-wHf5Gity0G0nPN34KuNBIBBEOu2H9ED2KqsblMPFygM-v1", as.ID())

	_, err := aset.Add(context.Background(), configMap("test-cm", "ns1"))
	assert.NoError(t, err)
	_, err = aset.Add(context.Background(), configMap("test-cm", "ns2"))
	assert.NoError(t, err)
	_, err = aset.Add(context.Background(), foo("test-foo", "ns3"))
	assert.NoError(t, err)

	dynamicClient.PrependReactor("patch", "configmaps", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
		var appliedCM unstructured.Unstructured
		patchAction := action.(k8stesting.PatchAction)
		assert.Equal(t, types.ApplyPatchType, patchAction.GetPatchType())
		err = json.Unmarshal(patchAction.GetPatch(), &appliedCM)
		assert.NoError(t, err)
		assert.NotNil(t, appliedCM)
		assert.Contains(t, "test-cm", appliedCM.GetName())
		assert.Contains(t, appliedCM.GetLabels(), ApplysetPartOfLabel)
		// The fake client needs to return the object that was applied.
		return true, &appliedCM, nil
	})

	dynamicClient.PrependReactor("patch", "foos", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
		var appliedFoo unstructured.Unstructured
		patchAction := action.(k8stesting.PatchAction)
		assert.Equal(t, types.ApplyPatchType, patchAction.GetPatchType())
		err = json.Unmarshal(patchAction.GetPatch(), &appliedFoo)
		assert.NoError(t, err)
		assert.NotNil(t, appliedFoo)
		assert.Contains(t, "test-foo", appliedFoo.GetName())
		assert.Contains(t, appliedFoo.GetLabels(), ApplysetPartOfLabel)
		// The fake client needs to return the object that was applied.
		return true, &appliedFoo, nil
	})
	// Reactor for parent update
	dynamicClient.PrependReactor("patch", "secrets", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
		patchAction := action.(k8stesting.PatchAction)
		assert.Equal(t, types.ApplyPatchType, patchAction.GetPatchType())

		var parentPatch unstructured.Unstructured
		err = json.Unmarshal(patchAction.GetPatch(), &parentPatch)
		assert.NoError(t, err)
		assert.Equal(t, "parent-secret", parentPatch.GetName())
		assert.Contains(t, parentPatch.GetLabels(), ApplySetParentIDLabel)
		assert.Contains(t, parentPatch.GetAnnotations(), ApplySetAdditionalNamespacesAnnotation)
		assert.Contains(t, parentPatch.GetAnnotations(), ApplySetGKsAnnotation)
		//assert.Equal(t, parentPatch.GetAnnotations()[ApplySetAdditionalNamespacesAnnotation], "ns1,ns2,ns3,oldns1")
		assert.Equal(t, parentPatch.GetAnnotations()[ApplySetGKsAnnotation], "ConfigMap,Foo.foo.bar")

		return true, &parentPatch, nil
	})

	var pruned int = 0
	dynamicClient.PrependReactor("delete", "configmaps", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
		deleteAction := action.(k8stesting.DeleteAction)
		assert.Equal(t, "prune-cm", deleteAction.GetName())
		pruned++
		return true, nil, nil
	})
	result, err := aset.Apply(context.Background(), true)
	assert.NoError(t, err)
	assert.NoError(t, result.Errors())
	assert.Equal(t, 3, result.DesiredCount)
	assert.Equal(t, 2, pruned)
	assert.Len(t, result.PrunedObjects, 2)
}

func TestApplySet_PruneOldGVKs(t *testing.T) {
	parent := parentObj(secretGVK, "parent-secret")
	// Add annotation for old GK
	parent.SetAnnotations(map[string]string{
		"applyset.kubernetes.io/contains-group-kinds": "Foo.foo.bar",
	})

	// Foo type should be pruned
	pruneFoo := foo("prune-foo", "ns1")
	pruneFoo.Unstructured.SetLabels(map[string]string{
		ApplysetPartOfLabel: "applyset-wHf5Gity0G0nPN34KuNBIBBEOu2H9ED2KqsblMPFygM-v1",
	})
	aset, dynamicClient := newTestApplySet(t, parent, pruneFoo)
	// Set the ApplysetPartOfLabel on the pruneCM to match the ID of the applyset
	// This is needed because the applyset ID is dynamically generated based on the parent object.
	// We need to ensure that the pruneCM has the correct label so it is discovered for pruning.
	as := aset.(*applySet)
	assert.Equal(t, "applyset-wHf5Gity0G0nPN34KuNBIBBEOu2H9ED2KqsblMPFygM-v1", as.ID())

	_, err := aset.Add(context.Background(), configMap("test-cm", "ns1"))
	assert.NoError(t, err)
	_, err = aset.Add(context.Background(), configMap("test-cm", "ns2"))
	assert.NoError(t, err)

	dynamicClient.PrependReactor("patch", "configmaps", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
		var appliedCM unstructured.Unstructured
		patchAction := action.(k8stesting.PatchAction)
		assert.Equal(t, types.ApplyPatchType, patchAction.GetPatchType())
		err = json.Unmarshal(patchAction.GetPatch(), &appliedCM)
		assert.NoError(t, err)
		assert.NotNil(t, appliedCM)
		assert.Contains(t, "test-cm", appliedCM.GetName())
		assert.Contains(t, appliedCM.GetLabels(), ApplysetPartOfLabel)
		// The fake client needs to return the object that was applied.
		return true, &appliedCM, nil
	})

	// Reactor for parent update
	dynamicClient.PrependReactor("patch", "secrets", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
		patchAction := action.(k8stesting.PatchAction)
		assert.Equal(t, types.ApplyPatchType, patchAction.GetPatchType())

		var parentPatch unstructured.Unstructured
		err = json.Unmarshal(patchAction.GetPatch(), &parentPatch)
		assert.NoError(t, err)
		assert.Equal(t, "parent-secret", parentPatch.GetName())
		assert.Contains(t, parentPatch.GetLabels(), ApplySetParentIDLabel)
		assert.Contains(t, parentPatch.GetAnnotations(), ApplySetAdditionalNamespacesAnnotation)
		assert.Contains(t, parentPatch.GetAnnotations(), ApplySetGKsAnnotation)
		assert.Equal(t, parentPatch.GetAnnotations()[ApplySetAdditionalNamespacesAnnotation], "ns1,ns2")
		//assert.Equal(t, parentPatch.GetAnnotations()[ApplySetGKsAnnotation], "ConfigMap,Foo.foo.bar")

		return true, &parentPatch, nil
	})

	var pruned int = 0
	dynamicClient.PrependReactor("delete", "foos", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
		deleteAction := action.(k8stesting.DeleteAction)
		assert.Equal(t, "prune-foo", deleteAction.GetName())
		pruned++
		return true, nil, nil
	})
	result, err := aset.Apply(context.Background(), true)
	assert.NoError(t, err)
	assert.NoError(t, result.Errors())
	assert.Equal(t, 2, result.DesiredCount)
	assert.Equal(t, 1, pruned)
	assert.Len(t, result.PrunedObjects, 1)
}
