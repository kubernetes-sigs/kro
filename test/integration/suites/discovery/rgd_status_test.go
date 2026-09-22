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

package discovery_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apiserver/pkg/cel/openapi/resolver"
	"k8s.io/client-go/discovery"
	"sigs.k8s.io/controller-runtime/pkg/client"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	ctrlinstance "github.com/kubernetes-sigs/kro/pkg/controller/instance"
	"github.com/kubernetes-sigs/kro/pkg/features"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
	"github.com/kubernetes-sigs/kro/test/integration/environment"
)

// This package has its own controller lifetime and default feature gates;
// the core suite enables GraphKind, which also starts the schema watcher.
func TestRGDStatusWithWarmDiscoveryAndDefaultGates(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping envtest-backed discovery test in short mode")
	}
	for gate, spec := range features.FeatureGate.GetAll() {
		require.Equal(t, spec.Default, features.FeatureGate.Enabled(gate), "feature gate %s", gate)
	}
	require.False(t, features.FeatureGate.Enabled(features.GraphKind))

	ctx := t.Context()
	env, err := environment.New(ctx, environment.ControllerConfig{
		ReconcileConfig: ctrlinstance.ReconcileConfig{DefaultRequeueDuration: time.Second},
	})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, env.Stop()) })
	require.Nil(t, env.SchemaWatcher)

	const namespace = "warm-discovery"
	require.NoError(t, env.Client.Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: namespace},
	}))
	liveDiscovery, err := discovery.NewDiscoveryClientForConfigAndClient(
		env.ClientSet.RESTConfig(), env.ClientSet.HTTPClient(),
	)
	require.NoError(t, err)
	liveSchemas := &resolver.ClientDiscoveryResolver{Discovery: liveDiscovery}

	createInstance := func(rgdName, kind, name, message string) *unstructured.Unstructured {
		t.Helper()
		rgd := generator.NewResourceGraphDefinition(rgdName,
			generator.WithSchema(kind, "v1alpha1",
				map[string]any{"message": "string"},
				map[string]any{"cmName": "${cm.metadata.name}"},
			),
			generator.WithResource("cm", map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]any{"name": "${schema.metadata.name}-cm"},
				"data":       map[string]any{"message": "${schema.spec.message}"},
			}, nil, nil),
		)
		require.NoError(t, env.Client.Create(ctx, rgd))
		require.EventuallyWithT(t, func(c *assert.CollectT) {
			require.NoError(c, env.Client.Get(ctx, client.ObjectKeyFromObject(rgd), rgd))
			assert.Equal(c, krov1alpha1.ResourceGraphDefinitionStateActive, rgd.Status.State, "%+v", rgd.Status)
		}, 30*time.Second, 250*time.Millisecond, "RGD %s should be active", rgdName)

		gvk := schema.GroupVersionKind{Group: "kro.run", Version: "v1alpha1", Kind: kind}
		// Check the live API independently, without touching the compiler's caches.
		require.EventuallyWithT(t, func(c *assert.CollectT) {
			resources, err := liveDiscovery.ServerResourcesForGroupVersion(gvk.GroupVersion().String())
			require.NoError(c, err)
			var resourceName string
			for _, resource := range resources.APIResources {
				if resource.Kind == kind && !strings.Contains(resource.Name, "/") {
					resourceName = resource.Name
				}
			}
			require.NotEmpty(c, resourceName, "live discovery should serve %s", kind)
			crd, err := env.ClientSet.APIExtensionsV1().CustomResourceDefinitions().Get(
				ctx, resourceName+"."+gvk.Group, metav1.GetOptions{},
			)
			require.NoError(c, err)
			var established apiextensionsv1.ConditionStatus
			for _, condition := range crd.Status.Conditions {
				if condition.Type == apiextensionsv1.Established {
					established = condition.Status
				}
			}
			assert.Equal(c, apiextensionsv1.ConditionTrue, established)
			_, err = liveSchemas.ResolveSchema(gvk)
			assert.NoError(c, err, "live OpenAPI should serve %s", kind)
		}, 30*time.Second, 250*time.Millisecond)
		t.Logf("%s: RGD active, CRD established, live discovery and OpenAPI ready", kind)

		instance := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": gvk.GroupVersion().String(),
			"kind":       kind,
			"metadata":   map[string]any{"name": name, "namespace": namespace},
			"spec":       map[string]any{"message": message},
		}}
		require.NoError(t, env.Client.Create(ctx, instance))
		return instance
	}

	waitForConvergence := func(instance *unstructured.Unstructured, message string) {
		t.Helper()
		require.EventuallyWithT(t, func(c *assert.CollectT) {
			got := instance.DeepCopy()
			require.NoError(c, env.Client.Get(ctx, client.ObjectKeyFromObject(instance), got))
			conditions, _, err := unstructured.NestedSlice(got.Object, "status", "conditions")
			require.NoError(c, err)
			var resolved map[string]any
			for _, raw := range conditions {
				condition := raw.(map[string]any)
				if condition["type"] == ctrlinstance.GraphResolved {
					resolved = condition
				}
			}
			assert.Equal(c, "True", resolved["status"], "GraphResolved condition: %+v", resolved)
			state, _, err := unstructured.NestedString(got.Object, "status", "state")
			require.NoError(c, err)
			assert.Equal(c, "ACTIVE", state)
			cmName, _, err := unstructured.NestedString(got.Object, "status", "cmName")
			require.NoError(c, err)
			assert.Equal(c, instance.GetName()+"-cm", cmName, "author status should be projected")

			cm := &corev1.ConfigMap{}
			require.NoError(c, env.Client.Get(ctx, types.NamespacedName{
				Namespace: namespace, Name: instance.GetName() + "-cm",
			}, cm))
			assert.Equal(c, message, cm.Data["message"])
		}, 30*time.Second, 250*time.Millisecond, "%s should converge", instance.GetKind())
		t.Logf("%s: child data=%q, GraphResolved=True, state=ACTIVE, status.cmName=%s-cm",
			instance.GetKind(), message, instance.GetName())
	}

	a := createInstance("warm-discovery-a", "WarmDiscoveryA", "a1", "from-a")
	waitForConvergence(a, "from-a")
	// A's child and author-status node have warmed the compiler's discovery.
	// Only now introduce B's Kind, in the same group/version and controller lifetime.
	b := createInstance("warm-discovery-b", "WarmDiscoveryB", "b1", "from-b")
	waitForConvergence(b, "from-b")
	waitForConvergence(a, "from-a")
}

// TestRGDInPlaceStatusFieldAdd is the s7reg regression: adding a status field to
// a live RGD must keep its instances ACTIVE with the new field, not stuck ERROR.
func TestRGDInPlaceStatusFieldAdd(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping envtest-backed discovery test in short mode")
	}
	ctx := t.Context()
	env, err := environment.New(ctx, environment.ControllerConfig{
		ReconcileConfig: ctrlinstance.ReconcileConfig{DefaultRequeueDuration: time.Second},
	})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, env.Stop()) })

	const namespace = "inplace-status"
	require.NoError(t, env.Client.Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: namespace},
	}))

	const kind = "InPlaceStatus"
	gvk := schema.GroupVersionKind{Group: "kro.run", Version: "v1alpha1", Kind: kind}

	// v1: one status field, out = cm name.
	rgd := generator.NewResourceGraphDefinition("inplace-status",
		generator.WithSchema(kind, "v1alpha1",
			map[string]any{"message": "string"},
			map[string]any{"out": "${cm.metadata.name}"},
		),
		generator.WithResource("cm", map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata":   map[string]any{"name": "${schema.metadata.name}-cm"},
			"data":       map[string]any{"message": "${schema.spec.message}"},
		}, nil, nil),
	)
	require.NoError(t, env.Client.Create(ctx, rgd))
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		require.NoError(c, env.Client.Get(ctx, client.ObjectKeyFromObject(rgd), rgd))
		assert.Equal(c, krov1alpha1.ResourceGraphDefinitionStateActive, rgd.Status.State, "%+v", rgd.Status)
	}, 30*time.Second, 250*time.Millisecond)

	newInstance := func(name string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": gvk.GroupVersion().String(),
			"kind":       kind,
			"metadata":   map[string]any{"name": name, "namespace": namespace},
			"spec":       map[string]any{"message": "hi-" + name},
		}}
	}
	assertActiveWithFields := func(name string, wantOut2 bool) {
		t.Helper()
		require.EventuallyWithT(t, func(c *assert.CollectT) {
			got := &unstructured.Unstructured{}
			got.SetGroupVersionKind(gvk)
			require.NoError(c, env.Client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, got))
			state, _, _ := unstructured.NestedString(got.Object, "status", "state")
			assert.Equal(c, "ACTIVE", state, "instance %s state: %+v", name, got.Object["status"])
			out, _, _ := unstructured.NestedString(got.Object, "status", "out")
			assert.Equal(c, name+"-cm", out, "status.out on %s", name)
			if wantOut2 {
				out2, found, _ := unstructured.NestedString(got.Object, "status", "out2")
				assert.True(c, found, "status.out2 should be present on %s", name)
				assert.Equal(c, "hi-"+name, out2, "status.out2 on %s", name)
			}
		}, 40*time.Second, 250*time.Millisecond, "%s should be ACTIVE with expected status", name)
	}

	// Existing instance converges under v1.
	a := newInstance("a1")
	require.NoError(t, env.Client.Create(ctx, a))
	assertActiveWithFields("a1", false)
	t.Log("a1 ACTIVE under v1 (status.out only)")

	// In-place edit: add a second status field out2 = spec.message.
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		require.NoError(c, env.Client.Get(ctx, client.ObjectKeyFromObject(rgd), rgd))
		updated := generator.NewResourceGraphDefinition("inplace-status",
			generator.WithSchema(kind, "v1alpha1",
				map[string]any{"message": "string"},
				map[string]any{
					"out":  "${cm.metadata.name}",
					"out2": "${schema.spec.message}",
				},
			),
			generator.WithResource("cm", map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]any{"name": "${schema.metadata.name}-cm"},
				"data":       map[string]any{"message": "${schema.spec.message}"},
			}, nil, nil),
		)
		rgd.Spec = updated.Spec
		require.NoError(c, env.Client.Update(ctx, rgd))
	}, 10*time.Second, 250*time.Millisecond)
	t.Log("RGD updated in place: added status.out2")

	// Pre-existing instance must pick up out2 and stay ACTIVE (not ERROR).
	assertActiveWithFields("a1", true)
	t.Log("a1 ACTIVE with status.out2 after in-place add (no stuck ERROR)")

	// A brand-new instance created after the edit must also converge with out2.
	b := newInstance("b1")
	require.NoError(t, env.Client.Create(ctx, b))
	assertActiveWithFields("b1", true)
	t.Log("b1 ACTIVE with status.out2")
}
