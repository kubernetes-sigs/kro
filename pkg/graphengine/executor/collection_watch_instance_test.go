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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	expv1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/testutil/generator"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
)

// collectionGraphWithUID builds a Graph with a single collection node whose
// local id is always "res" (shared across Graphs on purpose) and a fixed UID,
// mirroring the standalone Graph path which has no LabelInjector.
func collectionGraphWithUID(name string, uid types.UID) *expv1alpha1.Graph {
	g := generator.NewGraph(name,
		generator.WithNamespace("default"),
		generator.WithDef("src", map[string]any{"names": []any{"x", "y"}}),
		generator.WithTemplate("res", map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "${'" + name + "-' + n}"},
			"data":     map[string]any{"k": "v"},
		}, generator.ForEachDim("n", "${src.names}")),
	)
	g.SetUID(uid)
	return g
}

// collectionWatchSelector applies the Graph on the standalone path (no
// LabelInjector) and returns the single collection selector watch it
// registered.
func collectionWatchSelector(t *testing.T, g *expv1alpha1.Graph) labels.Selector {
	t.Helper()
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	w := &recordingWatcher{}
	// No WithLabelInjector: this is the standalone Graph path, which the RGD
	// path's instance labeler does not touch.
	_, err := NewSimple(cl).Apply(context.Background(), compileAndBuild(t, g), w)
	require.NoError(t, err)
	for i := range w.reqs {
		if w.reqs[i].Selector != nil { // the collection selector watch
			return w.reqs[i].Selector
		}
	}
	t.Fatal("no collection selector watch was registered")
	return nil
}

// TestSimple_CollectionWatch_GraphPathStampsInstanceID is the regression test
// for the finding that the standalone Graph path stamped NO instance-id label
// on collection-node watches: two different Graphs whose collection nodes share
// a node id ("res") registered byte-identical selectors and woke each other on
// every event.
//
// Before the fix the selector was {node-id} only, so both Graphs' selectors
// were identical and cross-matched. After the fix each Graph's watch also
// carries an instance-id derived from the Graph's own UID (mirroring how the
// RGD path scopes watches by instance), so the two selectors are distinct and
// neither matches the other Graph's items.
func TestSimple_CollectionWatch_GraphPathStampsInstanceID(t *testing.T) {
	t.Parallel()

	graphA := collectionGraphWithUID("graph-a", types.UID("uid-a"))
	graphB := collectionGraphWithUID("graph-b", types.UID("uid-b"))

	selA := collectionWatchSelector(t, graphA)
	selB := collectionWatchSelector(t, graphB)

	// The Graph path must stamp an instance-id on the watch label set, so the
	// two selectors are NOT identical even though both collection nodes share
	// the local id "res".
	assert.NotEqual(t, selA.String(), selB.String(),
		"two Graphs sharing a collection node id must register DISTINCT watch selectors")

	// Concretely, each selector must carry its own Graph UID as the
	// instance-id, and must not match the peer Graph's items.
	itemA := labels.Set{metadata.NodeIDLabel: "res", metadata.InstanceIDLabel: "uid-a"}
	itemB := labels.Set{metadata.NodeIDLabel: "res", metadata.InstanceIDLabel: "uid-b"}

	assert.True(t, selA.Matches(itemA), "graph-a's watch must match graph-a's own items")
	assert.False(t, selA.Matches(itemB),
		"graph-a's watch must NOT match graph-b's items (self-wake regression)")
	assert.True(t, selB.Matches(itemB), "graph-b's watch must match graph-b's own items")
	assert.False(t, selB.Matches(itemA),
		"graph-b's watch must NOT match graph-a's items (self-wake regression)")

	// The instance-id label must actually be present in the selector (the
	// pre-fix bug left it absent on the Graph path).
	itemNoInstance := labels.Set{metadata.NodeIDLabel: "res"}
	assert.False(t, selA.Matches(itemNoInstance),
		"the Graph-path watch must require an instance-id, not match node-id alone")
}

// TestSimple_CollectionWatch_GraphPathStampsInstanceIDOnChildren is the other
// half of the finding: the standalone-Graph collection watch selects on
// {node-id, instance-id}, but the applied CHILDREN only carried node-id (the
// LabelInjector that stamps instance-id runs only on the RGD path). So the
// watch matched nothing and drift/deletion on those children went unobserved.
// After the fix stampKROMeta stamps the Graph UID as instance-id when no
// injector supplied one, so the applied objects match the selector.
func TestSimple_CollectionWatch_GraphPathStampsInstanceIDOnChildren(t *testing.T) {
	t.Parallel()

	g := collectionGraphWithUID("graph-a", types.UID("uid-a"))
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	w := &recordingWatcher{}
	_, err := NewSimple(cl).Apply(context.Background(), compileAndBuild(t, g), w)
	require.NoError(t, err)

	// Find the collection selector watch and each applied child.
	var sel labels.Selector
	for i := range w.reqs {
		if w.reqs[i].Selector != nil {
			sel = w.reqs[i].Selector
		}
	}
	require.NotNil(t, sel, "a collection selector watch must be registered")

	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMapList"})
	require.NoError(t, cl.List(context.Background(), list))
	require.Len(t, list.Items, 2, "both collection items must have been applied")
	for i := range list.Items {
		obj := &list.Items[i]
		assert.Equal(t, "uid-a", obj.GetLabels()[metadata.InstanceIDLabel],
			"applied child must carry the Graph UID as instance-id so the watch matches it")
		assert.True(t, sel.Matches(labels.Set(obj.GetLabels())),
			"the collection watch selector must match the applied child's labels")
	}
}

// TestDistinctWatchMappings covers the multi-GVR collection watch fix: a
// collection rendering items across several GVRs must yield one (gvr, sample)
// pair per DISTINCT GVR (first-seen order), so the caller registers a drift
// watch for every rendered type — not just mappings[0]. A static single-GVR
// collection collapses to one entry.
func TestDistinctWatchMappings(t *testing.T) {
	t.Parallel()
	gvrCM := schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
	gvrSecret := schema.GroupVersionResource{Version: "v1", Resource: "secrets"}

	mk := func(kind, name string) *unstructured.Unstructured {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: kind})
		u.SetName(name)
		return u
	}

	t.Run("single GVR collapses to one", func(t *testing.T) {
		desired := []*unstructured.Unstructured{mk("ConfigMap", "a"), mk("ConfigMap", "b")}
		mappings := []applyMapping{{gvr: gvrCM}, {gvr: gvrCM}}
		got := distinctWatchMappings(desired, mappings)
		require.Len(t, got, 1)
		assert.Equal(t, gvrCM, got[0].gvr)
		assert.Equal(t, "a", got[0].sample.GetName(), "first-seen sample is representative")
	})

	t.Run("multiple GVRs yield one entry each, first-seen order", func(t *testing.T) {
		desired := []*unstructured.Unstructured{mk("ConfigMap", "a"), mk("Secret", "s"), mk("ConfigMap", "b")}
		mappings := []applyMapping{{gvr: gvrCM}, {gvr: gvrSecret}, {gvr: gvrCM}}
		got := distinctWatchMappings(desired, mappings)
		require.Len(t, got, 2, "one watch mapping per distinct GVR")
		assert.Equal(t, gvrCM, got[0].gvr)
		assert.Equal(t, gvrSecret, got[1].gvr)
		assert.Equal(t, "s", got[1].sample.GetName(), "the Secret sample represents the Secret GVR")
	})
}
