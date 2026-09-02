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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	expv1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/testutil/generator"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
)

// getCM fetches an applied ConfigMap by name from the fake client.
func getCM(t *testing.T, cl client.Client, name string) *unstructured.Unstructured {
	t.Helper()
	cm := &unstructured.Unstructured{}
	cm.SetAPIVersion("v1")
	cm.SetKind("ConfigMap")
	require.NoError(t, cl.Get(context.Background(),
		types.NamespacedName{Namespace: "default", Name: name}, cm))
	return cm
}

// twoSubgraphCollisionGraph declares two subgraph nodes, subA and subB, each
// containing a node with the SAME local id "res". This is the exact collision
// the user hit: distinct managed resources (collide-a / collide-b) whose node
// paths are subA/res and subB/res. Each subgraph emits a single ConfigMap so
// applyScalarTemplate/stampKROMeta run per frame.
func twoSubgraphCollisionGraph() *expv1alpha1.Graph {
	sub := func(cmName string) *expv1alpha1.Graph {
		return generator.NewGraph("child",
			generator.WithNamespace("default"),
			generator.WithTemplate("res", map[string]any{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]any{"name": cmName},
				"data":     map[string]any{"k": "v"},
			}),
		)
	}
	return generator.NewGraph("g",
		generator.WithNamespace("default"),
		generator.WithSubgraph("subA", sub("collide-a")),
		generator.WithSubgraph("subB", sub("collide-b")),
	)
}

// TestSimple_SubgraphNodeID_DottedForm covers Option A's readable branch: when
// the qualified path fits inside the 63-char label limit, the kro.run/node-id
// label carries the '.'-joined path (subA.res) while the node-path annotation
// carries the '/'-form (subA/res). Two sibling subgraphs reusing the same local
// id "res" therefore get DISTINCT label tokens and no longer collide.
func TestSimple_SubgraphNodeID_DottedForm(t *testing.T) {
	t.Parallel()
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()

	_, err := NewSimple(cl).Apply(context.Background(),
		compileAndBuild(t, twoSubgraphCollisionGraph()), &recordingWatcher{})
	require.NoError(t, err)

	cases := []struct {
		cmName       string
		wantLabel    string
		wantNodePath string
	}{
		{"collide-a", "subA.res", "subA/res"},
		{"collide-b", "subB.res", "subB/res"},
	}
	for _, tc := range cases {
		cm := getCM(t, cl, tc.cmName)

		gotLabel := cm.GetLabels()[metadata.NodeIDLabel]
		assert.Equal(t, tc.wantLabel, gotLabel,
			"%s: node-id label must be the '.'-joined qualified path, not the bare local id", tc.cmName)
		assert.Empty(t, validation.IsValidLabelValue(gotLabel),
			"%s: node-id label %q must be a valid Kubernetes label value", tc.cmName, gotLabel)

		gotPath := cm.GetAnnotations()[metadata.NodePathAnnotation]
		assert.Equal(t, tc.wantNodePath, gotPath,
			"%s: node-path annotation must carry the full readable '/'-form", tc.cmName)
	}

	// The two sibling nodes must be distinguishable by the label alone — this
	// is what the collection watch selector matches on. If both stamped the
	// bare "res" (the pre-fix bug), a selector for one would match the other.
	a := getCM(t, cl, "collide-a").GetLabels()[metadata.NodeIDLabel]
	b := getCM(t, cl, "collide-b").GetLabels()[metadata.NodeIDLabel]
	assert.NotEqual(t, a, b,
		"sibling subgraph nodes that reuse a local id must get distinct node-id labels")
}

// TestSimple_SubgraphCollectionWatch_NoCrossMatch is the behavioral guard: a
// collection node inside subA declares a selector watch whose node-id token is
// subA.res. That selector must match subA's own items (labelled subA.res) and
// must NOT match subB's items (labelled subB.res), even though both share the
// same instance-id. Before the fix both were labelled "res" and the selectors
// cross-matched.
func TestSimple_SubgraphCollectionWatch_NoCrossMatch(t *testing.T) {
	t.Parallel()

	collectionSub := func(prefix string) *expv1alpha1.Graph {
		return generator.NewGraph("child",
			generator.WithNamespace("default"),
			generator.WithDef("src", map[string]any{"names": []any{"x", "y"}}),
			generator.WithTemplate("res", map[string]any{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]any{"name": "${'" + prefix + "-' + n}"},
				"data":     map[string]any{"k": "v"},
			}, generator.ForEachDim("n", "${src.names}")),
		)
	}
	g := generator.NewGraph("g",
		generator.WithNamespace("default"),
		generator.WithSubgraph("subA", collectionSub("a")),
		generator.WithSubgraph("subB", collectionSub("b")),
	)

	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	ex := NewSimple(cl).WithLabelInjector(func(obj *unstructured.Unstructured) {
		l := obj.GetLabels()
		if l == nil {
			l = map[string]string{}
		}
		// Same instance-id on every item across BOTH subgraphs — mirrors the
		// real instance labeler. The node-id token is what must disambiguate.
		l[metadata.InstanceIDLabel] = "uid-1"
		obj.SetLabels(l)
	})
	w := &recordingWatcher{}

	_, err := ex.Apply(context.Background(), compileAndBuild(t, g), w)
	require.NoError(t, err)

	// Two collection nodes → two selector watches, keyed by the qualified path.
	byKey := map[string]*recordedReq{}
	for i := range w.reqs {
		r := w.reqs[i]
		byKey[r.NodeID] = &recordedReq{selector: r.Selector, namespace: r.Namespace}
	}
	subA := byKey["subA/res"]
	subB := byKey["subB/res"]
	require.NotNil(t, subA, "subA collection watch must be keyed by the qualified path subA/res")
	require.NotNil(t, subB, "subB collection watch must be keyed by the qualified path subB/res")
	require.NotNil(t, subA.selector)
	require.NotNil(t, subB.selector)

	subAItem := labels.Set{metadata.NodeIDLabel: "subA.res", metadata.InstanceIDLabel: "uid-1"}
	subBItem := labels.Set{metadata.NodeIDLabel: "subB.res", metadata.InstanceIDLabel: "uid-1"}

	assert.True(t, subA.selector.Matches(subAItem), "subA's watch must match subA's own items")
	assert.False(t, subA.selector.Matches(subBItem),
		"subA's watch must NOT match subB's items despite the shared instance-id and local id")
	assert.True(t, subB.selector.Matches(subBItem), "subB's watch must match subB's own items")
	assert.False(t, subB.selector.Matches(subAItem),
		"subB's watch must NOT match subA's items")

	// Both subgraph collections template every item into the graph namespace,
	// so the watch is scoped to that single namespace (the optimization). A
	// collection that spanned namespaces would fall back to "" — see
	// TestSimple_CollectionWatchNamespace.
	assert.Equal(t, "default", subA.namespace,
		"a single-namespace collection watch should be scoped to that namespace")
}

type recordedReq struct {
	selector  labels.Selector
	namespace string
}

// TestPatchFieldManager_QualifiedPathIsUnique guards that a patch node's field
// manager is keyed on the fully-qualified node path. Two sibling subgraphs that
// reuse the same local id ("res") must get DISTINCT managers — otherwise their
// patch contributions share one SSA manager and release-on-prune could drop the
// other subgraph's fields. Root-level ids keep their bare form so existing
// (non-nested) patch managers are unchanged.
func TestPatchFieldManager_QualifiedPathIsUnique(t *testing.T) {
	t.Parallel()
	const uid = "parent-uid"

	subA := patchFieldManager(uid, "subA/res")
	subB := patchFieldManager(uid, "subB/res")
	assert.NotEqual(t, subA, subB,
		"sibling subgraph patch nodes reusing a local id must get distinct field managers")

	// Stable for a given path.
	assert.Equal(t, subA, patchFieldManager(uid, "subA/res"),
		"the field manager must be deterministic for a given qualified path")

	// Distinct parents never collide even at the same path.
	assert.NotEqual(t, subA, patchFieldManager("other-uid", "subA/res"))

	// Within the SSA 128-char limit and kro-prefixed.
	assert.LessOrEqual(t, len(subA), 128)
	assert.True(t, strings.HasPrefix(subA, "kro-graphengine.patch."))
}

// TestSimple_SubgraphNodeID_HashFallback covers Option A's fallback branch:
// when the '.'-joined qualified path would exceed the 63-char label limit
// (deep nesting or long node names), the label falls back to a bounded hash
// while the node-path annotation still carries the full readable path.
// TestSimple_CollectionWatchNamespace verifies the collection-watch namespace
// optimization: the single selector watch is scoped to one namespace when the
// whole collection lands there, and left all-namespaces ("") only when it
// genuinely spans namespaces.
func TestSimple_CollectionWatchNamespace(t *testing.T) {
	t.Parallel()

	collectionWatchNS := func(t *testing.T, g *expv1alpha1.Graph) string {
		t.Helper()
		cl := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
		w := &recordingWatcher{}
		_, err := NewSimple(cl).Apply(context.Background(), compileAndBuild(t, g), w)
		require.NoError(t, err)
		for i := range w.reqs {
			if w.reqs[i].Selector != nil { // the collection selector watch
				return w.reqs[i].Namespace
			}
		}
		t.Fatal("no collection selector watch was registered")
		return ""
	}

	t.Run("single namespace is scoped", func(t *testing.T) {
		t.Parallel()
		g := generator.NewGraph("g",
			generator.WithNamespace("team-a"),
			generator.WithDef("src", map[string]any{"names": []any{"alpha", "beta"}}),
			generator.WithTemplate("cm", map[string]any{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]any{"name": "${'cm-' + n}"}, // no namespace → defaults to graph ns
				"data":     map[string]any{"k": "v"},
			}, generator.ForEachDim("n", "${src.names}")),
		)
		assert.Equal(t, "team-a", collectionWatchNS(t, g),
			"every item defaults to the graph namespace, so the watch should be scoped to it")
	})

	t.Run("explicit single namespace is scoped", func(t *testing.T) {
		t.Parallel()
		g := generator.NewGraph("g",
			generator.WithNamespace("team-a"),
			generator.WithDef("src", map[string]any{"names": []any{"alpha", "beta"}}),
			generator.WithTemplate("cm", map[string]any{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]any{"name": "${'cm-' + n}", "namespace": "other"},
				"data":     map[string]any{"k": "v"},
			}, generator.ForEachDim("n", "${src.names}")),
		)
		assert.Equal(t, "other", collectionWatchNS(t, g),
			"all items pin the same explicit namespace, so the watch should be scoped to it")
	})

	t.Run("multiple namespaces fall back to all-namespaces", func(t *testing.T) {
		t.Parallel()
		g := generator.NewGraph("g",
			generator.WithNamespace("team-a"),
			generator.WithDef("src", map[string]any{"names": []any{"alpha", "beta"}}),
			generator.WithTemplate("cm", map[string]any{
				"apiVersion": "v1", "kind": "ConfigMap",
				// alpha → ns-alpha, beta → ns-beta: the collection spans namespaces.
				"metadata": map[string]any{"name": "${'cm-' + n}", "namespace": "${'ns-' + n}"},
				"data":     map[string]any{"k": "v"},
			}, generator.ForEachDim("n", "${src.names}")),
		)
		assert.Equal(t, "", collectionWatchNS(t, g),
			"a collection spanning namespaces must keep an all-namespaces watch")
	})
}

func TestSimple_SubgraphNodeID_HashFallback(t *testing.T) {
	t.Parallel()

	// A node name long enough that even a single frame of qualification blows
	// the 63-char budget once joined. Node ids are alphanumeric only.
	longSeg := strings.Repeat("Xy", 34) // 68 chars > 63
	inner := generator.NewGraph("child",
		generator.WithNamespace("default"),
		generator.WithTemplate(longSeg, map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "deep-cm"},
			"data":     map[string]any{"k": "v"},
		}),
	)
	g := generator.NewGraph("g",
		generator.WithNamespace("default"),
		generator.WithSubgraph("outer", inner),
	)

	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	_, err := NewSimple(cl).Apply(context.Background(),
		compileAndBuild(t, g), &recordingWatcher{})
	require.NoError(t, err)

	cm := getCM(t, cl, "deep-cm")

	gotLabel := cm.GetLabels()[metadata.NodeIDLabel]
	assert.Empty(t, validation.IsValidLabelValue(gotLabel),
		"hashed node-id label %q must still be a valid label value", gotLabel)
	assert.LessOrEqual(t, len(gotLabel), validation.LabelValueMaxLength,
		"hashed node-id label must be within the 63-char limit")
	assert.True(t, strings.HasPrefix(gotLabel, "h-"),
		"an over-long path must fall back to the hashed 'h-' form, got %q", gotLabel)

	// Readability is preserved in the annotation regardless of the label hash.
	wantPath := "outer/" + longSeg
	assert.Equal(t, wantPath, cm.GetAnnotations()[metadata.NodePathAnnotation],
		"node-path annotation must carry the full readable path even when the label is hashed")

	// The hash must be deterministic for the same path.
	cm2 := getCM(t, cl, "deep-cm")
	assert.Equal(t, gotLabel, cm2.GetLabels()[metadata.NodeIDLabel],
		"the node-id hash must be stable for a given path")
}
