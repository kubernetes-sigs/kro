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

package executor

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	expv1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/compiler"
	krotruntime "github.com/kubernetes-sigs/kro/pkg/graphengine/runtime"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/testutil/generator"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/watchrouter"
)

// Patch field-manager semantics (contribute without ownership, release
// without delete, status-subresource routing) depend on the real API
// server's server-side-apply managed-field tracking, which the fake client
// does not model. These tests run against envtest and skip when
// KUBEBUILDER_ASSETS is unset.

var (
	patchEnvOnce sync.Once
	patchTestEnv *envtest.Environment
	patchEnvCfg  *rest.Config
	patchEnvErr  error
)

// TestMain owns the envtest lifecycle for the executor package: it boots
// lazily (only the patch tests need it) and is torn down once after the run.
func TestMain(m *testing.M) {
	code := m.Run()
	if patchTestEnv != nil {
		_ = patchTestEnv.Stop()
	}
	os.Exit(code)
}

// patchEnvClient boots (once) an envtest control plane and returns a typed
// client. Skips the calling test when KUBEBUILDER_ASSETS is not configured.
func patchEnvClient(t *testing.T) client.Client {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; skipping envtest patch tests")
	}
	patchEnvOnce.Do(func() {
		patchTestEnv = &envtest.Environment{}
		patchEnvCfg, patchEnvErr = patchTestEnv.Start()
	})
	if patchEnvErr != nil {
		t.Fatalf("start envtest: %v", patchEnvErr)
	}
	scheme := k8sruntime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, apiextensionsv1.AddToScheme(scheme))
	cl, err := client.New(patchEnvCfg, client.Options{Scheme: scheme})
	require.NoError(t, err)
	return cl
}

var podGVK = schema.GroupVersionKind{Version: "v1", Kind: "Pod"}

// mustCreateConfigMap seeds a ConfigMap that a patch node will contribute to.
func mustCreateConfigMap(t *testing.T, cl client.Client, ns, name string, data map[string]any) {
	t.Helper()
	cm := &unstructured.Unstructured{}
	cm.SetGroupVersionKind(configMapGVK)
	cm.SetNamespace(ns)
	cm.SetName(name)
	if data != nil {
		require.NoError(t, unstructured.SetNestedMap(cm.Object, data, "data"))
	}
	require.NoError(t, cl.Create(context.Background(), cm))
}

func getConfigMap(t *testing.T, cl client.Client, ns, name string) *unstructured.Unstructured {
	t.Helper()
	cm := &unstructured.Unstructured{}
	cm.SetGroupVersionKind(configMapGVK)
	require.NoError(t, cl.Get(context.Background(),
		types.NamespacedName{Namespace: ns, Name: name}, cm))
	return cm
}

func hasFieldManager(obj *unstructured.Unstructured, manager string) bool {
	for _, mf := range obj.GetManagedFields() {
		if mf.Manager == manager {
			return true
		}
	}
	return false
}

// TestPatch_ContributesFields verifies a patch node adds fields to a
// pre-existing object under its own per-node field manager, records a
// Contribution, and never records the target as an owned managed resource.
func TestPatch_ContributesFields(t *testing.T) {
	cl := patchEnvClient(t)
	ns := "default"
	mustCreateConfigMap(t, cl, ns, "existing", map[string]any{"orig": "kept"})

	g := generator.NewGraph("g",
		generator.WithNamespace(ns),
		generator.WithDef("cfg", map[string]any{"v": "contributed"}),
		generator.WithPatch("p", "v1", "ConfigMap", "existing", map[string]any{
			"data": map[string]any{"added": "${cfg.v}"},
		}),
	)
	g.SetUID("uid-contributes")

	rt := compileAndBuild(t, g)
	res, err := NewSimple(cl).Apply(context.Background(), rt, watchrouter.NoopWatcher{})
	require.NoError(t, err)

	// Patch nodes never enter the owned inventory; they are tracked as
	// contributions instead.
	assert.Empty(t, res.Applied, "patch must not be recorded as an owned resource")
	require.Len(t, res.Contributions, 1)
	c := res.Contributions[0]
	assert.Equal(t, "ConfigMap", c.Kind)
	assert.Equal(t, "existing", c.Name)
	assert.Equal(t, patchFieldManager(g.GetUID(), "p"), c.FieldManager)

	cm := getConfigMap(t, cl, ns, "existing")
	data, _, _ := unstructured.NestedStringMap(cm.Object, "data")
	assert.Equal(t, "contributed", data["added"], "contributed field is present")
	assert.Equal(t, "kept", data["orig"], "pre-existing field survives")
	assert.True(t, hasFieldManager(cm, c.FieldManager), "contribution is owned by the per-node field manager")
}

// TestPatch_TargetAbsentSoftRequeue verifies that a patch whose target does
// not exist is a soft requeue: ErrNotReady, the node is Unresolved, and no
// contribution is recorded.
func TestPatch_TargetAbsentSoftRequeue(t *testing.T) {
	cl := patchEnvClient(t)

	g := generator.NewGraph("g",
		generator.WithNamespace("default"),
		generator.WithPatch("p", "v1", "ConfigMap", "missing", map[string]any{
			"data": map[string]any{"k": "v"},
		}),
	)
	g.SetUID("uid-absent")

	rt := compileAndBuild(t, g)
	res, err := NewSimple(cl).Apply(context.Background(), rt, watchrouter.NoopWatcher{})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotReady)
	assert.Contains(t, res.Unresolved, "p")
	assert.Empty(t, res.Contributions)
}

// TestPatch_DisjointContributionsCoexist verifies two patch nodes each
// contribute different fields to the same object and both survive, each under
// its own field manager.
func TestPatch_DisjointContributionsCoexist(t *testing.T) {
	cl := patchEnvClient(t)
	ns := "default"
	mustCreateConfigMap(t, cl, ns, "shared", nil)

	g := generator.NewGraph("g",
		generator.WithNamespace(ns),
		generator.WithPatch("p1", "v1", "ConfigMap", "shared", map[string]any{
			"data": map[string]any{"one": "from-p1"},
		}),
		generator.WithPatch("p2", "v1", "ConfigMap", "shared", map[string]any{
			"data": map[string]any{"two": "from-p2"},
		}),
	)
	g.SetUID("uid-disjoint")

	rt := compileAndBuild(t, g)
	res, err := NewSimple(cl).Apply(context.Background(), rt, watchrouter.NoopWatcher{})
	require.NoError(t, err)
	require.Len(t, res.Contributions, 2)

	cm := getConfigMap(t, cl, ns, "shared")
	data, _, _ := unstructured.NestedStringMap(cm.Object, "data")
	assert.Equal(t, "from-p1", data["one"])
	assert.Equal(t, "from-p2", data["two"])
	assert.True(t, hasFieldManager(cm, patchFieldManager(g.GetUID(), "p1")))
	assert.True(t, hasFieldManager(cm, patchFieldManager(g.GetUID(), "p2")))
}

// TestPatch_ReleaseDropsFieldsKeepsObject verifies release-on-prune: the
// contributed fields are dropped when the contribution is released, while the
// target object and other fields survive.
func TestPatch_ReleaseDropsFieldsKeepsObject(t *testing.T) {
	cl := patchEnvClient(t)
	ns := "default"
	mustCreateConfigMap(t, cl, ns, "releasable", map[string]any{"orig": "kept"})

	g := generator.NewGraph("g",
		generator.WithNamespace(ns),
		generator.WithPatch("p", "v1", "ConfigMap", "releasable", map[string]any{
			"data": map[string]any{"added": "gone-later"},
		}),
	)
	g.SetUID("uid-release")

	rt := compileAndBuild(t, g)
	ex := NewSimple(cl)
	res, err := ex.Apply(context.Background(), rt, watchrouter.NoopWatcher{})
	require.NoError(t, err)
	require.Len(t, res.Contributions, 1)

	cm := getConfigMap(t, cl, ns, "releasable")
	data, _, _ := unstructured.NestedStringMap(cm.Object, "data")
	require.Equal(t, "gone-later", data["added"])

	require.NoError(t, ex.Release(context.Background(), res.Contributions))

	cm = getConfigMap(t, cl, ns, "releasable")
	data, _, _ = unstructured.NestedStringMap(cm.Object, "data")
	_, stillThere := data["added"]
	assert.False(t, stillThere, "released field is dropped")
	assert.Equal(t, "kept", data["orig"], "unrelated field survives")
	assert.False(t, hasFieldManager(cm, res.Contributions[0].FieldManager), "field manager relinquished its fields")
}

// TestPatch_NestedSubgraphContributionsPropagatedAndReleased verifies that patch
// contributions from a nested subgraph are aggregated into the parent ApplyResult
// and can be released.
func TestPatch_NestedSubgraphContributionsPropagatedAndReleased(t *testing.T) {
	cl := patchEnvClient(t)
	ns := "default"
	mustCreateConfigMap(t, cl, ns, "nested-releasable", map[string]any{"orig": "kept"})

	child := generator.NewGraph("child",
		generator.WithPatch("p", "v1", "ConfigMap", "nested-releasable", map[string]any{
			"data": map[string]any{"added": "nested-val"},
		}),
	)
	g := generator.NewGraph("g",
		generator.WithNamespace(ns),
		generator.WithSubgraph("sub", child),
	)
	g.SetUID("uid-nested-release")

	rt := compileAndBuild(t, g)
	ex := NewSimple(cl)
	res, err := ex.Apply(context.Background(), rt, watchrouter.NoopWatcher{})
	require.NoError(t, err)
	require.Len(t, res.Contributions, 1, "nested patch contribution must be propagated to parent result")
	assert.Equal(t, "v1", res.Contributions[0].APIVersion)
	assert.Equal(t, "ConfigMap", res.Contributions[0].Kind)
	assert.Equal(t, "nested-releasable", res.Contributions[0].Name)

	cm := getConfigMap(t, cl, ns, "nested-releasable")
	data, _, _ := unstructured.NestedStringMap(cm.Object, "data")
	require.Equal(t, "nested-val", data["added"])

	require.NoError(t, ex.Release(context.Background(), res.Contributions))

	cm = getConfigMap(t, cl, ns, "nested-releasable")
	data, _, _ = unstructured.NestedStringMap(cm.Object, "data")
	_, stillThere := data["added"]
	assert.False(t, stillThere, "released field is dropped")
	assert.Equal(t, "kept", data["orig"], "unrelated field survives")
	assert.False(t, hasFieldManager(cm, res.Contributions[0].FieldManager), "field manager relinquished its fields")
}

// TestPatch_StatusSubresourceRouting verifies a patch with subresource=status
// contributes to the status subresource.
func TestPatch_StatusSubresourceRouting(t *testing.T) {
	cl := patchEnvClient(t)
	ns := "default"

	// A minimal Pod so the status subresource exists to contribute to.
	pod := &unstructured.Unstructured{}
	pod.SetGroupVersionKind(podGVK)
	pod.SetNamespace(ns)
	pod.SetName("statuspod")
	require.NoError(t, unstructured.SetNestedSlice(pod.Object, []any{
		map[string]any{"name": "c", "image": "nginx"},
	}, "spec", "containers"))
	require.NoError(t, cl.Create(context.Background(), pod))

	g := generator.NewGraph("g",
		generator.WithNamespace(ns),
		generator.WithPatchSpec("p", &expv1alpha1.PatchSpec{
			APIVersion:  "v1",
			Kind:        "Pod",
			Metadata:    expv1alpha1.PatchMetadata{Name: "statuspod"},
			Subresource: "status",
			Body: generator.RawExtFromMap(map[string]any{
				"status": map[string]any{"phase": "Running"},
			}),
		}),
	)
	g.SetUID("uid-status")

	rt := compileAndBuild(t, g)
	res, err := NewSimple(cl).Apply(context.Background(), rt, watchrouter.NoopWatcher{})
	require.NoError(t, err)
	require.Len(t, res.Contributions, 1)
	assert.Equal(t, "status", res.Contributions[0].Subresource)

	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(podGVK)
	require.NoError(t, cl.Get(context.Background(),
		types.NamespacedName{Namespace: ns, Name: "statuspod"}, got))
	phase, _, _ := unstructured.NestedString(got.Object, "status", "phase")
	assert.Equal(t, "Running", phase)
}

// TestPatch_StatusSubresourceLegacyUpdateConflictResolution verifies that when a status field
// was previously owned by a legacy manager via an Update operation (e.g. v0.9.3 UpdateStatus),
// a subsequent status-subresource SSA patch successfully reclaims ownership with ForceOwnership
// without returning a 409 Conflict.
func TestPatch_StatusSubresourceLegacyUpdateConflictResolution(t *testing.T) {
	cl := patchEnvClient(t)
	ns := "default"

	// 1. Create a custom CRD with a status subresource so SSA and Update conflict tracking
	// matches real-world custom resources and kro instance CRDs.
	crd := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: "widgets.test.kro.run",
		},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "test.kro.run",
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Kind:     "Widget",
				ListKind: "WidgetList",
				Plural:   "widgets",
				Singular: "widget",
			},
			Scope: apiextensionsv1.NamespaceScoped,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name:    "v1",
				Served:  true,
				Storage: true,
				Subresources: &apiextensionsv1.CustomResourceSubresources{
					Status: &apiextensionsv1.CustomResourceSubresourceStatus{},
				},
				Schema: &apiextensionsv1.CustomResourceValidation{
					OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
						Type: "object",
						Properties: map[string]apiextensionsv1.JSONSchemaProps{
							"spec": {
								Type:                   "object",
								XPreserveUnknownFields: ptr(true),
							},
							"status": {
								Type:                   "object",
								XPreserveUnknownFields: ptr(true),
							},
						},
					},
				},
			}},
		},
	}
	_ = cl.Create(context.Background(), crd)

	// Wait for CRD to be established
	require.NoError(t, wait.PollUntilContextTimeout(context.Background(), 100*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		gotCRD := &apiextensionsv1.CustomResourceDefinition{}
		if err := cl.Get(ctx, types.NamespacedName{Name: "widgets.test.kro.run"}, gotCRD); err != nil {
			return false, nil
		}
		for _, c := range gotCRD.Status.Conditions {
			if c.Type == apiextensionsv1.Established && c.Status == apiextensionsv1.ConditionTrue {
				return true, nil
			}
		}
		return false, nil
	}))

	widgetGVK := schema.GroupVersionKind{Group: "test.kro.run", Version: "v1", Kind: "Widget"}

	// Create the target Widget instance
	widget := &unstructured.Unstructured{}
	widget.SetGroupVersionKind(widgetGVK)
	widget.SetNamespace(ns)
	widget.SetName("target-widget")
	widget.Object["spec"] = map[string]any{"field": "val"}
	require.NoError(t, cl.Create(context.Background(), widget))

	// 2. Write status under a legacy field manager via client-go Update (operation=Update, simulating v0.9.3).
	widget.Object["status"] = map[string]any{"phase": "Pending", "message": "v0.9.3-state"}
	require.NoError(t, cl.Status().Update(context.Background(), widget, client.FieldOwner("kro-v0.9.3-legacy-manager")))

	// 3. Apply a status change through a patch node (contributeApply path) under kro's SSA manager.
	g := generator.NewGraph("g",
		generator.WithNamespace(ns),
		generator.WithPatchSpec("p", &expv1alpha1.PatchSpec{
			APIVersion:  "test.kro.run/v1",
			Kind:        "Widget",
			Metadata:    expv1alpha1.PatchMetadata{Name: "target-widget"},
			Subresource: "status",
			Body: generator.RawExtFromMap(map[string]any{
				"status": map[string]any{"phase": "Running"},
			}),
		}),
	)
	g.SetUID("uid-status-upgrade")

	rt := compileAndBuildEnv(t, patchEnvCfg, g)
	res, err := NewSimple(cl).Apply(context.Background(), rt, watchrouter.NoopWatcher{})

	// 4. Assert it SUCCEEDS (no conflict error) and value is updated to Running.
	require.NoError(t, err)
	require.Len(t, res.Contributions, 1)

	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(widgetGVK)
	require.NoError(t, cl.Get(context.Background(),
		types.NamespacedName{Namespace: ns, Name: "target-widget"}, got))
	phase, _, _ := unstructured.NestedString(got.Object, "status", "phase")
	assert.Equal(t, "Running", phase)
}

// TestPatch_FieldManagerConflictSoftRequeue verifies that when a patch node encounters
// an SSA field-manager conflict with a pre-existing manager, it is treated as a soft error
// (ErrNotReady): the patch node is marked Unresolved, the error message mentions the
// contending manager, and the topological walk continues to apply downstream nodes.
func TestPatch_FieldManagerConflictSoftRequeue(t *testing.T) {
	cl := patchEnvClient(t)
	ns := "default"
	targetName := "conflict-target"

	// 1. Seed ConfigMap with a field owned by a foreign manager via SSA
	mustCreateConfigMap(t, cl, ns, targetName, nil)
	cm := getConfigMap(t, cl, ns, targetName)
	cm.Object["data"] = map[string]any{"contendedKey": "initial-value"}
	require.NoError(t, cl.Patch(context.Background(), cm, client.Apply, client.FieldOwner("foreign-manager")))

	// 2. Build graph with:
	// - patch node 'p' patching contendedKey without ForceOwnership (which will conflict with foreign-manager)
	// - downstream template node 'downstream' creating 'downstream-cm'
	g := generator.NewGraph("g",
		generator.WithNamespace(ns),
		generator.WithPatch("p", "v1", "ConfigMap", targetName, map[string]any{
			"data": map[string]any{"contendedKey": "patch-value"},
		}),
		generator.WithTemplate("downstream", map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "downstream-cm"},
			"data":     map[string]any{"k": "v"},
		}),
	)
	g.SetUID("uid-patch-conflict")

	rt := compileAndBuildEnv(t, patchEnvCfg, g)
	res, err := NewSimple(cl).Apply(context.Background(), rt, watchrouter.NoopWatcher{})

	// 3. Error must be soft (ErrNotReady), node 'p' is Unresolved, and contending manager is in message.
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNotReady), "field-manager conflict on patch must be soft ErrNotReady, got %v", err)
	assert.Contains(t, res.Unresolved, "p", "conflicting patch node must be recorded as Unresolved")
	assert.Contains(t, err.Error(), "foreign-manager", "error message must include the contending field manager name")

	// 4. Topological walk must NOT have aborted: downstream node was applied!
	require.Len(t, res.Applied, 1, "downstream node must be applied despite patch conflict")
	assert.Equal(t, "downstream-cm", res.Applied[0].Name)
	downstreamCM := getConfigMap(t, cl, ns, "downstream-cm")
	data, _, _ := unstructured.NestedStringMap(downstreamCM.Object, "data")
	assert.Equal(t, "v", data["k"])
}

// TestPatch_NonConflictErrorIsHardAbort verifies that non-conflict errors on patch nodes
// (e.g. schema/validation errors from the API server) abort the topological walk immediately.
func TestPatch_NonConflictErrorIsHardAbort(t *testing.T) {
	cl := patchEnvClient(t)
	ns := "default"
	targetName := "pod-target"

	// Create a minimal Pod
	pod := &unstructured.Unstructured{}
	pod.SetGroupVersionKind(podGVK)
	pod.SetNamespace(ns)
	pod.SetName(targetName)
	require.NoError(t, unstructured.SetNestedSlice(pod.Object, []any{
		map[string]any{"name": "c", "image": "nginx"},
	}, "spec", "containers"))
	require.NoError(t, cl.Create(context.Background(), pod))

	// An invalid patch with an invalid activeDeadlineSeconds value (fails validation with 422 Invalid without field conflict)
	g := generator.NewGraph("g",
		generator.WithNamespace(ns),
		generator.WithPatch("p", "v1", "Pod", targetName, map[string]any{
			"spec": map[string]any{"activeDeadlineSeconds": int64(-1)},
		}),
		generator.WithTemplate("downstream", map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "downstream-cm-aborted"},
			"data":     map[string]any{"k": "v"},
		}),
	)
	g.SetUID("uid-patch-nonconflict")

	rt := compileAndBuildEnv(t, patchEnvCfg, g)
	res, err := NewSimple(cl).Apply(context.Background(), rt, watchrouter.NoopWatcher{})

	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrNotReady), "non-conflict patch error must NOT be soft ErrNotReady, got: %v", err)
	assert.True(t, apierrors.IsInvalid(err), "expected 422 Invalid error from API server, got: %v", err)
	assert.Empty(t, res.Applied, "walk must abort immediately without applying downstream nodes")

	downstreamCM := &unstructured.Unstructured{}
	downstreamCM.SetGroupVersionKind(configMapGVK)
	err = cl.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "downstream-cm-aborted"}, downstreamCM)
	assert.True(t, apierrors.IsNotFound(err), "downstream resource must not have been created")
}

func compileAndBuildEnv(t *testing.T, cfg *rest.Config, g *expv1alpha1.Graph) *krotruntime.Runtime {
	t.Helper()
	httpClient, err := rest.HTTPClientFor(cfg)
	require.NoError(t, err)
	cmp, err := compiler.NewCompiler(cfg, httpClient)
	require.NoError(t, err)
	p, err := cmp.Compile(g)
	require.NoError(t, err)
	return krotruntime.New(p, g)
}

func ptr[T any](v T) *T {
	return &v
}
