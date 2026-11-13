// Copyright 2025 The Kubernetes Authors.
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

package dynamiccontroller

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/metadata/fake"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	controllerruntime "sigs.k8s.io/controller-runtime"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/features"
	kwatch "github.com/kubernetes-sigs/kro/pkg/watch"
)

var testInstanceGVR = schema.GroupVersionResource{Group: "test", Version: "v1", Resource: "tests"}

func newTestInstance(ns, name string, annotations map[string]string, status map[string]any) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "test/v1",
		"kind":       "Test",
		"metadata": map[string]any{
			"name":      name,
			"namespace": ns,
		},
		"spec": map[string]any{"replicas": int64(3)},
	}}
	if annotations != nil {
		obj.SetAnnotations(annotations)
	}
	if status != nil {
		obj.Object["status"] = status
	}
	return obj
}

func TestTrimInstance(t *testing.T) {
	t.Run("keeps identity, metadata and revision status fields", func(t *testing.T) {
		obj := newTestInstance("default", "a", map[string]string{"x": "y"}, map[string]any{
			"graphRevision":       int64(2),
			"targetGraphRevision": int64(3),
			"state":               "ACTIVE",
			"conditions":          []any{map[string]any{"type": "Ready"}},
			"authorField":         "value",
		})

		out, err := trimInstance(obj)
		require.NoError(t, err)
		trimmed, ok := out.(*unstructured.Unstructured)
		require.True(t, ok)

		assert.Equal(t, map[string]any{
			"apiVersion": "test/v1",
			"kind":       "Test",
			"metadata": map[string]any{
				"name":        "a",
				"namespace":   "default",
				"annotations": map[string]any{"x": "y"},
			},
			"status": map[string]any{
				"graphRevision":       int64(2),
				"targetGraphRevision": int64(3),
			},
		}, trimmed.Object)
	})

	t.Run("drops status entirely when no revision fields are present", func(t *testing.T) {
		obj := newTestInstance("default", "a", nil, map[string]any{"state": "ACTIVE"})
		out, err := trimInstance(obj)
		require.NoError(t, err)
		trimmed := out.(*unstructured.Unstructured)
		_, has := trimmed.Object["status"]
		assert.False(t, has)
		_, hasSpec := trimmed.Object["spec"]
		assert.False(t, hasSpec)
	})

	t.Run("is idempotent", func(t *testing.T) {
		obj := newTestInstance("default", "a", nil, map[string]any{
			"graphRevision": int64(1),
			"state":         "ACTIVE",
		})
		once, err := trimInstance(obj)
		require.NoError(t, err)
		first := once.(*unstructured.Unstructured).DeepCopy()

		twice, err := trimInstance(once)
		require.NoError(t, err)
		assert.Equal(t, first.Object, twice.(*unstructured.Unstructured).Object)
	})

	t.Run("passes other types through unchanged", func(t *testing.T) {
		pom := &v1.PartialObjectMetadata{}
		pom.SetName("a")
		out, err := trimInstance(pom)
		require.NoError(t, err)
		assert.Same(t, pom, out)

		tombstone := cacheDeletedFinalStateUnknown()
		out, err = trimInstance(tombstone)
		require.NoError(t, err)
		assert.Equal(t, tombstone, out)
	})
}

func cacheDeletedFinalStateUnknown() any {
	return struct{ Key string }{Key: "default/a"}
}

func TestIndexGraphRevisions(t *testing.T) {
	tests := []struct {
		name     string
		obj      any
		expected []string
	}{
		{
			name:     "int64 status revisions",
			obj:      newTestInstance("default", "a", nil, map[string]any{"graphRevision": int64(2), "targetGraphRevision": int64(3)}),
			expected: []string{"2", "3"},
		},
		{
			name:     "float64 status revisions",
			obj:      newTestInstance("default", "a", nil, map[string]any{"graphRevision": float64(4)}),
			expected: []string{"4"},
		},
		{
			name: "annotation pin is indexed and deduplicated",
			obj: newTestInstance("default", "a",
				map[string]string{v1alpha1.InstanceGraphRevisionAnnotation: "2"},
				map[string]any{"graphRevision": int64(2), "targetGraphRevision": int64(5)}),
			expected: []string{"2", "5"},
		},
		{
			name: "latest annotation is ignored",
			obj: newTestInstance("default", "a",
				map[string]string{v1alpha1.InstanceGraphRevisionAnnotation: v1alpha1.GraphRevisionLatest},
				map[string]any{"graphRevision": int64(1)}),
			expected: []string{"1"},
		},
		{
			name: "non-numeric annotation is ignored",
			obj: newTestInstance("default", "a",
				map[string]string{v1alpha1.InstanceGraphRevisionAnnotation: "abc"}, nil),
			expected: []string{},
		},
		{
			name: "values below one are ignored",
			obj: newTestInstance("default", "a",
				map[string]string{v1alpha1.InstanceGraphRevisionAnnotation: "0"},
				map[string]any{"graphRevision": int64(0), "targetGraphRevision": int64(-1)}),
			expected: []string{},
		},
		{
			name:     "non-unstructured object yields nothing",
			obj:      &v1.PartialObjectMetadata{},
			expected: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			values, err := indexGraphRevisions(tt.obj)
			require.NoError(t, err)
			assert.ElementsMatch(t, tt.expected, values)
		})
	}
}

// newPinningController builds a started dynamic controller whose parent
// informers are revision-aware and backed by the given seeded instances.
func newPinningController(t *testing.T, objects ...runtime.Object) *DynamicController {
	t.Helper()

	metaScheme := runtime.NewScheme()
	require.NoError(t, v1.AddMetaToScheme(metaScheme))
	metaClient := fake.NewSimpleMetadataClient(metaScheme)
	mapper := meta.NewDefaultRESTMapper(metaScheme.PreferredVersionAllGroups())

	dynamicClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{testInstanceGVR: "TestList"},
		objects...,
	)

	cfg := testConfig()
	cfg.WatchSyncTimeout = 10 * time.Second
	dc := NewDynamicController(noopLogger(), cfg, metaClient, dynamicClient, mapper)

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	dc.ctx.Store(&ctx)
	return dc
}

func registerNoopHandler(t *testing.T, dc *DynamicController) {
	t.Helper()
	require.NoError(t, dc.Register(t.Context(), testInstanceGVR, Handler(func(context.Context, controllerruntime.Request) error {
		return nil
	})))
}

func TestReferencedGraphRevisions(t *testing.T) {
	t.Run("gate off reports unavailable", func(t *testing.T) {
		dc := newPinningController(t)
		refs, ok := dc.ReferencedGraphRevisions(testInstanceGVR)
		assert.False(t, ok)
		assert.Nil(t, refs)
	})

	t.Run("gate on returns the union of referenced revisions", func(t *testing.T) {
		featuregatetesting.SetFeatureGateDuringTest(t, features.FeatureGate, features.GraphRevisionPinning, true)

		dc := newPinningController(t,
			newTestInstance("default", "a", nil, map[string]any{"graphRevision": int64(1), "targetGraphRevision": int64(2)}),
			newTestInstance("default", "b", map[string]string{v1alpha1.InstanceGraphRevisionAnnotation: "7"}, nil),
			newTestInstance("other", "c", map[string]string{v1alpha1.InstanceGraphRevisionAnnotation: v1alpha1.GraphRevisionLatest},
				map[string]any{"graphRevision": int64(2)}),
		)
		registerNoopHandler(t, dc)

		refs, ok := dc.ReferencedGraphRevisions(testInstanceGVR)
		require.True(t, ok)
		assert.ElementsMatch(t, []int64{1, 2, 7}, refs.UnsortedList())
	})

	t.Run("gate on but no informer reports unavailable", func(t *testing.T) {
		featuregatetesting.SetFeatureGateDuringTest(t, features.FeatureGate, features.GraphRevisionPinning, true)

		dc := newPinningController(t)
		refs, ok := dc.ReferencedGraphRevisions(testInstanceGVR)
		assert.False(t, ok)
		assert.Nil(t, refs)
	})
}

func TestInstanceKeys(t *testing.T) {
	t.Run("returns cached instance keys", func(t *testing.T) {
		featuregatetesting.SetFeatureGateDuringTest(t, features.FeatureGate, features.GraphRevisionPinning, true)

		dc := newPinningController(t,
			newTestInstance("default", "a", nil, nil),
			newTestInstance("other", "b", nil, nil),
		)
		registerNoopHandler(t, dc)

		keys, ok := dc.InstanceKeys(testInstanceGVR)
		require.True(t, ok)
		assert.ElementsMatch(t, []types.NamespacedName{
			{Namespace: "default", Name: "a"},
			{Namespace: "other", Name: "b"},
		}, keys)
	})

	t.Run("no informer reports unavailable", func(t *testing.T) {
		featuregatetesting.SetFeatureGateDuringTest(t, features.FeatureGate, features.GraphRevisionPinning, true)

		dc := newPinningController(t)
		keys, ok := dc.InstanceKeys(testInstanceGVR)
		assert.False(t, ok)
		assert.Nil(t, keys)
	})
}

func TestEnqueueFromInformer_GraphRevisionPinChange(t *testing.T) {
	makeObj := func(pin string) *v1.PartialObjectMetadata {
		obj := &v1.PartialObjectMetadata{}
		obj.SetName("a")
		obj.SetNamespace("default")
		obj.SetGeneration(5)
		if pin != "" {
			obj.SetAnnotations(map[string]string{v1alpha1.InstanceGraphRevisionAnnotation: pin})
		}
		return obj
	}

	tests := []struct {
		name        string
		gateOn      bool
		oldPin      string
		newPin      string
		expectQueue int
	}{
		{name: "pin added, gate on — enqueues", gateOn: true, newPin: "2", expectQueue: 1},
		{name: "pin retargeted, gate on — enqueues", gateOn: true, oldPin: "1", newPin: "2", expectQueue: 1},
		{name: "pin removed, gate on — enqueues", gateOn: true, oldPin: "1", expectQueue: 1},
		{name: "pin unchanged, gate on — skips", gateOn: true, oldPin: "1", newPin: "1", expectQueue: 0},
		{name: "pin retargeted, gate off — skips", gateOn: false, oldPin: "1", newPin: "2", expectQueue: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.gateOn {
				featuregatetesting.SetFeatureGateDuringTest(t, features.FeatureGate, features.GraphRevisionPinning, true)
			}

			scheme := runtime.NewScheme()
			require.NoError(t, v1.AddMetaToScheme(scheme))
			client := fake.NewSimpleMetadataClient(scheme)
			mapper := meta.NewDefaultRESTMapper(scheme.PreferredVersionAllGroups())

			dc := NewDynamicController(noopLogger(), testConfig(), client, dynamicfake.NewSimpleDynamicClient(runtime.NewScheme()), mapper)
			dc.enqueueFromInformer(testInstanceGVR, makeObj(tt.oldPin), makeObj(tt.newPin), kwatch.EventUpdate)
			assert.Equal(t, tt.expectQueue, dc.queue.Len())
		})
	}
}
