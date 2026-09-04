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

package executor

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/kubernetes-sigs/kro/pkg/applyset"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/testutil/generator"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/watchrouter"
)

// withManagedFields sets the given manager names as Apply managedFields entries.
func withManagedFields(o *unstructured.Unstructured, managers ...string) *unstructured.Unstructured {
	mf := make([]metav1.ManagedFieldsEntry, 0, len(managers))
	for _, m := range managers {
		mf = append(mf, metav1.ManagedFieldsEntry{Manager: m, Operation: metav1.ManagedFieldsOperationApply, APIVersion: "v1", FieldsType: "FieldsV1", FieldsV1: &metav1.FieldsV1{Raw: []byte(`{}`)}})
	}
	o.SetManagedFields(mf)
	return o
}

// TestOwnedByGraphTemplate is the cross-engine ownership recognizer used by the
// RGD/instance path: any kro Graph template manager on the live object marks it
// as Graph-owned and therefore off-limits to a force-adopt.
func TestOwnedByGraphTemplate(t *testing.T) {
	t.Parallel()

	graphMgr := templateFieldManager(types.UID("some-graph"))

	newObj := func() *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "cm", "namespace": "default"},
		}}
	}

	assert.False(t, ownedByGraphTemplate(nil), "nil current is never Graph-owned")
	assert.False(t, ownedByGraphTemplate(withManagedFields(newObj())), "no managers is never Graph-owned")
	assert.False(t, ownedByGraphTemplate(withManagedFields(newObj(), FieldManager)),
		"the shared RGD field manager is not a Graph template writer")
	assert.False(t, ownedByGraphTemplate(withManagedFields(newObj(), "kubectl-client-side-apply")),
		"a plain external manager is not a Graph template writer")
	assert.True(t, ownedByGraphTemplate(withManagedFields(newObj(), graphMgr)),
		"a kro Graph template manager marks the object Graph-owned")
	assert.True(t, ownedByGraphTemplate(withManagedFields(newObj(), FieldManager, graphMgr)),
		"a Graph template manager among others still marks the object Graph-owned")
}

// TestSimple_RGD_RefusesToAdoptGraphOwnedObject is the core regression: the
// RGD/instance path (ConflictDetection off) must NOT silently adopt a live
// object already owned by a standalone Graph's template manager. Before the fix
// the RGD path force-applied under the shared field manager and stole the field
// (err=nil); after the fix it is refused as a soft not-ready conflict and the
// live field is left untouched. Requires envtest for real SSA managed-field
// tracking (the fake client strips managedFields on Get).
func TestSimple_RGD_RefusesToAdoptGraphOwnedObject(t *testing.T) {
	cl := patchEnvClient(t)
	ns := "default"
	ctx := context.Background()

	// A standalone Graph applies the object first (ConflictDetection on): it
	// lands a real kro-graphengine.tmpl.<...> managed-fields entry and, per the
	// design contract, NO applyset part-of label.
	graph := generator.NewGraph("graph",
		generator.WithNamespace(ns),
		generator.WithTemplate("cm", map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "graph-owned", "namespace": ns},
			"data":     map[string]any{"owner": "graph"},
		}),
	)
	graph.SetUID(types.UID("uid-graph-owned"))
	graphRT := compileAndBuildEnv(t, patchEnvCfg, graph)

	_, err := NewSimple(cl).WithConflictDetection(true).Apply(ctx, graphRT, watchrouter.NoopWatcher{})
	require.NoError(t, err)
	require.True(t, hasFieldManager(getConfigMap(t, cl, ns, "graph-owned"), templateFieldManager(graph.GetUID())),
		"the Graph must own the object under its template field manager")

	// An RGD instance now targets the same object (ConflictDetection off).
	rgd := generator.NewGraph("rgd",
		generator.WithNamespace(ns),
		generator.WithTemplate("cm", map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "graph-owned", "namespace": ns},
			"data":     map[string]any{"owner": "rgd"},
		}),
	)
	rgd.SetUID(types.UID("uid-rgd"))
	rgdRT := compileAndBuildEnv(t, patchEnvCfg, rgd)

	res, err := NewSimple(cl).Apply(ctx, rgdRT, watchrouter.NoopWatcher{})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrFieldManagerConflict),
		"RGD adopting a Graph-owned object is a field-manager conflict")
	assert.True(t, errors.Is(err, ErrNotReady),
		"the cross-engine refusal is soft not-ready so the reconcile backs off")
	assert.Empty(t, res.Applied, "the Graph-owned object must not be recorded as adopted")

	// The field must NOT have been overwritten.
	got := getConfigMap(t, cl, ns, "graph-owned")
	data, _, _ := unstructured.NestedStringMap(got.Object, "data")
	assert.Equal(t, "graph", data["owner"], "the Graph's field must be left untouched, not stolen")
}

// TestSimple_RGD_RefusesToAdoptGraphOwnedObject_Collection is the collection
// analogue: a Graph-owned member is held soft not-ready, not adopted.
func TestSimple_RGD_RefusesToAdoptGraphOwnedObject_Collection(t *testing.T) {
	cl := patchEnvClient(t)
	ns := "default"
	ctx := context.Background()

	// A Graph owns cm-alpha under a template field manager.
	graph := generator.NewGraph("graph",
		generator.WithNamespace(ns),
		generator.WithTemplate("cm", map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "cm-alpha", "namespace": ns},
			"data":     map[string]any{"owner": "graph"},
		}),
	)
	graph.SetUID(types.UID("uid-graph-coll"))
	_, err := NewSimple(cl).WithConflictDetection(true).Apply(ctx, compileAndBuildEnv(t, patchEnvCfg, graph), watchrouter.NoopWatcher{})
	require.NoError(t, err)

	// An RGD collection expands to cm-alpha (contested) and cm-beta (free).
	res, err := NewSimple(cl).Apply(ctx, compileAndBuild(t, collectionCMGraph()), watchrouter.NoopWatcher{})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNotReady), "a Graph-owned collection member is held soft not-ready")

	appliedNames := make([]string, 0, len(res.Applied))
	for _, a := range res.Applied {
		appliedNames = append(appliedNames, a.Name)
	}
	assert.NotContains(t, appliedNames, "cm-alpha", "the Graph-owned member must not be recorded as adopted")

	got := getConfigMap(t, cl, ns, "cm-alpha")
	data, _, _ := unstructured.NestedStringMap(got.Object, "data")
	assert.Equal(t, "graph", data["owner"], "the Graph's field must be left untouched, not stolen")
}

// TestSimple_RGD_AppliesOwnAndUnownedObject verifies the guard does not regress
// the ordinary RGD path: an object with no foreign markers still applies, and
// re-applying an object the RGD path itself already owns still applies.
func TestSimple_RGD_AppliesOwnAndUnownedObject(t *testing.T) {
	cl := patchEnvClient(t)
	ns := "default"
	ctx := context.Background()

	rgd := generator.NewGraph("rgd",
		generator.WithNamespace(ns),
		generator.WithTemplate("cm", map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "rgd-self", "namespace": ns},
			"data":     map[string]any{"owner": "rgd"},
		}),
	)
	rgd.SetUID(types.UID("uid-rgd-self"))
	rt := compileAndBuildEnv(t, patchEnvCfg, rgd)

	exec := NewSimple(cl) // ConflictDetection off (RGD/instance path)

	// Fresh (unowned) object: applies.
	res, err := exec.Apply(ctx, rt, watchrouter.NoopWatcher{})
	require.NoError(t, err)
	require.Len(t, res.Applied, 1)
	require.True(t, hasFieldManager(getConfigMap(t, cl, ns, "rgd-self"), FieldManager),
		"the RGD path owns the object under the shared field manager")

	// Re-apply the object it now owns under the shared field manager: still applies.
	res, err = exec.Apply(ctx, rt, watchrouter.NoopWatcher{})
	require.NoError(t, err)
	require.Len(t, res.Applied, 1)
}

// TestSimple_Graph_RefusesRGDOwnedObject is the symmetry sanity check: the
// Graph path (ConflictDetection on) still refuses to adopt an object carrying
// the RGD applyset part-of label, via the existing applySetConflict guard which
// runs on both paths. This must still hold after the RGD-side change.
func TestSimple_Graph_RefusesRGDOwnedObject(t *testing.T) {
	t.Parallel()

	live := liveCM("rgd-owned")
	live.SetLabels(map[string]string{applyset.ApplysetPartOfLabel: "applyset-rgd-v1"})
	live.Object["data"] = map[string]any{"owner": "rgd"}

	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(live).Build()

	g := generator.NewGraph("g",
		generator.WithNamespace("default"),
		generator.WithTemplate("cm", map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "rgd-owned"},
			"data":     map[string]any{"owner": "graph"},
		}),
	)

	res, err := NewSimple(cl).WithConflictDetection(true).Apply(context.Background(),
		compileAndBuild(t, g), watchrouter.NoopWatcher{})

	require.Error(t, err)
	var conflictErr *applyset.ApplySetConflictError
	require.True(t, errors.As(err, &conflictErr), "Graph must refuse an RGD-owned object via applySetConflict")
	assert.Equal(t, "applyset-rgd-v1", conflictErr.CurrentApplySetID)
	assert.Empty(t, res.Applied)

	got := getFakeCM(t, cl, "rgd-owned")
	data, _, _ := unstructured.NestedStringMap(got.Object, "data")
	assert.Equal(t, "rgd", data["owner"], "the RGD's field must be left untouched, not stolen")
}

// getFakeCM reads a ConfigMap from a fake client by name in the default ns.
func getFakeCM(t *testing.T, cl client.Client, name string) *unstructured.Unstructured {
	t.Helper()
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(configMapGVK)
	require.NoError(t, cl.Get(context.Background(),
		types.NamespacedName{Namespace: "default", Name: name}, got))
	return got
}
