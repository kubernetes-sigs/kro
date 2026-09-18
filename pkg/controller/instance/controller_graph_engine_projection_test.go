// Copyright 2025 The Kube Resource Orchestrator Authors
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
	"maps"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	apimachineryruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"k8s.io/kube-openapi/pkg/validation/spec"
	ctrl "sigs.k8s.io/controller-runtime"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/features"
	"github.com/kubernetes-sigs/kro/pkg/graph/revisions"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/compiler"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/registry"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/rgdadapter"
	geruntime "github.com/kubernetes-sigs/kro/pkg/graphengine/runtime"
	testk8s "github.com/kubernetes-sigs/kro/pkg/testutil/k8s"
)

func TestReconcile_IncludeWhenAfterInventoryProjection(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, features.FeatureGate, features.GraphKind, false)
	featuregatetesting.SetFeatureGateDuringTest(t, features.FeatureGate, features.CELOmitFunction, false)
	t.Logf("GraphKind=%t CELOmitFunction=%t", features.FeatureGate.Enabled(features.GraphKind), features.FeatureGate.Enabled(features.CELOmitFunction))

	// The synthesized author-status patch needs the parent kind's schema.
	resolver, _ := testk8s.NewFakeResolver()
	resolver.AddSchema(controllerTestParentGVK, &spec.Schema{SchemaProps: spec.SchemaProps{
		Type: []string{"object"},
		Properties: map[string]spec.Schema{
			"apiVersion": *spec.StringProperty(),
			"kind":       *spec.StringProperty(),
			"metadata": {SchemaProps: spec.SchemaProps{Type: []string{"object"}, Properties: map[string]spec.Schema{
				"name":      *spec.StringProperty(),
				"namespace": *spec.StringProperty(),
			}}},
			"status": {SchemaProps: spec.SchemaProps{Type: []string{"object"}, Properties: map[string]spec.Schema{
				"dbOwner": *spec.StringProperty(),
			}}},
		},
	}})
	comp := compiler.NewCompilerWithDependencies(resolver, buildControllerTestRESTMapper())

	for _, tt := range []struct {
		name         string
		includeWhen  string
		wantIncluded bool
	}{
		{"optional", `${db.?data.phase.orValue("missing") == "Running"}`, true},
		{"has", `${has(db.data)}`, true},
		{"negated_has", `${!has(db.data)}`, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rgdSpec := &v1alpha1.ResourceGraphDefinitionSpec{
				Schema: &v1alpha1.Schema{
					APIVersion: controllerTestParentGVK.Version,
					Kind:       controllerTestParentGVK.Kind,
					Group:      controllerTestParentGVK.Group,
					Spec:       apimachineryruntime.RawExtension{Raw: []byte(`{"name":"string"}`)},
					// This reference seeds db with {} before it has been applied.
					Status: apimachineryruntime.RawExtension{Raw: []byte(`{"dbOwner":"${db.data.owner}"}`)},
				},
				Resources: []*v1alpha1.Resource{
					{
						ID: "db",
						Template: apimachineryruntime.RawExtension{Raw: []byte(`{
							"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"${schema.spec.name}-db"},
							"data":{"owner":"${schema.spec.name}","phase":"Running"}}`)},
					},
					{
						ID:          "app",
						IncludeWhen: []string{tt.includeWhen},
						Template: apimachineryruntime.RawExtension{Raw: []byte(`{
							"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"${schema.spec.name}-app"},
							"data":{"value":"wanted"}}`)},
					},
					{
						// Also exercise contagious inclusion decisions made during projection.
						ID: "leaf",
						Template: apimachineryruntime.RawExtension{Raw: []byte(`{
							"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"${schema.spec.name}-leaf"},
							"data":{"value":"${app.data.value}"}}`)},
					},
				},
			}
			first, second := newInstanceObject("first", "default"), newInstanceObject("second", "default")
			for _, inst := range []*unstructured.Unstructured{first, second} {
				inst.Object["spec"] = map[string]any{"name": inst.GetName()}
			}
			raw := newControllerTestDynamicClient(t, first.DeepCopy(), second.DeepCopy())
			s := apimachineryruntime.NewScheme()
			require.NoError(t, clientgoscheme.AddToScheme(s))
			runtimeClient := fakeclient.NewClientBuilder().WithScheme(s).
				WithObjects(first.DeepCopy(), second.DeepCopy()).WithStatusSubresource(first).Build()
			c, _ := newGraphEngineControllerUnderTest(t, raw, rgdSpec, revisions.RevisionStateActive, comp, runtimeClient)

			for _, inst := range []*unstructured.Unstructured{first, second} {
				t.Run(inst.GetName(), func(t *testing.T) {
					ctx := context.Background()
					key := types.NamespacedName{Namespace: inst.GetNamespace(), Name: inst.GetName()}
					require.NoError(t, c.Reconcile(ctx, ctrl.Request{NamespacedName: key}))

					stored, err := raw.Resource(controllerTestParentGVR).Namespace(key.Namespace).Get(ctx, key.Name, metav1.GetOptions{})
					require.NoError(t, err)
					ready := conditionByType(t, stored, ResourcesReady).Status
					assert.Equal(t, metav1.ConditionTrue, ready)
					state, _, _ := unstructured.NestedString(stored.Object, "status", "state")
					assert.Equal(t, string(v1alpha1.InstanceStateActive), state)
					written := &unstructured.Unstructured{}
					written.SetGroupVersionKind(controllerTestParentGVK)
					require.NoError(t, runtimeClient.Get(ctx, key, written))
					owner, _, _ := unstructured.NestedString(written.Object, "status", "dbOwner")
					assert.Equal(t, key.Name, owner)
					t.Logf("state=%s ResourcesReady=%s dbOwner=%s", state, ready, owner)

					// Healthy status alone is insufficient: check the actual executor writes.
					for _, id := range []string{"db", "app", "leaf"} {
						obj := &unstructured.Unstructured{}
						obj.SetGroupVersionKind(controllerTestCMGVK)
						childKey := types.NamespacedName{Namespace: key.Namespace, Name: key.Name + "-" + id}
						err := runtimeClient.Get(ctx, childKey, obj)
						t.Logf("%s present=%t", childKey.Name, err == nil)
						if id != "db" && !tt.wantIncluded {
							assert.True(t, apierrors.IsNotFound(err), "%s must be excluded against the applied db; got %v", id, err)
							continue
						}
						if assert.NoError(t, err, "%s must be created using the applied db, not a cached placeholder verdict", id) {
							data, _, _ := unstructured.NestedStringMap(obj.Object, "data")
							if id == "db" {
								assert.Equal(t, map[string]string{"owner": key.Name, "phase": "Running"}, data)
							} else {
								assert.Equal(t, map[string]string{"value": "wanted"}, data)
							}
						}
					}
				})
			}
			assert.Equal(t, 1, c.programCache.Len(), "both instances share the revision's compiled program")
		})
	}
}

func TestCandidateMetadata_PreservesRuntimeInputs(t *testing.T) {
	rgd := &v1alpha1.ResourceGraphDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "projection-inputs"},
		Spec: v1alpha1.ResourceGraphDefinitionSpec{
			Schema: &v1alpha1.Schema{
				APIVersion: "v1alpha1",
				Kind:       "WebApp",
				Spec:       apimachineryruntime.RawExtension{Raw: []byte(`{"names":"[]string","targetNamespace":"string"}`)},
			},
			Resources: []*v1alpha1.Resource{{
				ID:      "members",
				ForEach: []v1alpha1.ForEachDimension{{"name": "${schema.spec.names}"}},
				Template: apimachineryruntime.RawExtension{Raw: []byte(`{
					"apiVersion":"v1","kind":"ConfigMap",
					"metadata":{"name":"${schema.metadata.name}-${name}","namespace":"${schema.spec.targetNamespace}"}}`)},
			}},
		},
	}
	comp := newTestRealCompiler(t)
	sharedCache := registry.New()
	var program *compiler.Program
	c := &Controller{}
	for _, tt := range []struct {
		name   string
		limit  int
		size   int
		inline bool
	}{
		{"bounded", 1, 2, false},
		{"within-limit", 2, 2, false},
		{"disabled", 0, geruntime.DefaultMaxCollectionSize + 1, false},
		{"inline", 2, 2, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			names := make([]any, tt.size)
			for i := range names {
				names[i] = fmt.Sprintf("member-%d", i)
			}
			inst := newInstanceObject(tt.name, "default")
			targetNS := tt.name + "-target"
			inst.Object["spec"] = map[string]any{"names": names, "targetNamespace": targetNS}
			var cache rgdadapter.ProgramCache = sharedCache
			if tt.inline {
				cache = nil
			}
			rt, _, err := rgdadapter.BuildRuntimeForInstanceCached(rgd, inst, comp, cache, geruntime.WithMaxCollectionSize(tt.limit))
			require.NoError(t, err)
			if !tt.inline {
				if program != nil {
					require.Same(t, program, rt.Program())
				}
				program = rt.Program()
			}
			before := maps.Clone(rt.Scope())
			// Projection must use the runtime's effective schema snapshot.
			inst.Object["spec"] = map[string]any{"names": []any{}, "targetNamespace": "later"}

			meta, _ := c.candidateMetadata(rt, inst)
			assert.Equal(t, sets.New(schema.GroupKind{Kind: "ConfigMap"}), meta.GroupKinds)
			if tt.limit > 0 && tt.size > tt.limit {
				assert.Empty(t, meta.AdditionalNamespaces, "an over-limit collection must use the static GroupKind fallback")
			} else {
				assert.Equal(t, sets.New(targetNS), meta.AdditionalNamespaces, "projection must preserve the cached schema override and collection limit")
			}
			assert.True(t, maps.Equal(before, rt.Scope()), "projection must not publish into the execution scope")
			assert.Nil(t, rt.Node(rgdadapter.SchemaNodeID).Observed(), "projection must not record speculative observations on execution nodes")
		})
	}
}
