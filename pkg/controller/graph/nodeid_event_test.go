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

package graph

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/kubernetes-sigs/kro/pkg/graphengine/compiler"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/registry"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
)

func progOf(nodes ...*compiler.Node) *compiler.Program {
	p := &compiler.Program{Nodes: make(map[string]*compiler.Node, len(nodes))}
	for _, n := range nodes {
		p.Nodes[n.ID] = n
		p.TopologicalOrder = append(p.TopologicalOrder, n.ID)
	}
	return p
}

func tmplNode(id string) *compiler.Node {
	return &compiler.Node{ID: id, Kind: compiler.NodeKindTemplate}
}

func subNode(id string, child *compiler.Program) *compiler.Node {
	return &compiler.Node{ID: id, Kind: compiler.NodeKindGraph, SubProgram: child}
}

func TestEncodedNodePaths_LeavesFittingPathsAlone(t *testing.T) {
	t.Parallel()

	p := progOf(tmplNode("frontend"), subNode("subA", progOf(tmplNode("res"))))
	assert.Empty(t, encodedNodePaths(p, ""))
}

func TestEncodedNodePaths_ReportsOverLongRootID(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("a", 64)
	p := progOf(tmplNode("frontend"), tmplNode(long))
	assert.Equal(t, []string{long}, encodedNodePaths(p, ""))
}

func TestEncodedNodePaths_ReportsPathsOverflowedByQualificationAlone(t *testing.T) {
	t.Parallel()

	outer, middle, leaf := strings.Repeat("a", 21), strings.Repeat("b", 21), strings.Repeat("c", 20)
	require.False(t, metadata.NodeIDTokenIsHashed(leaf), "the leaf id alone must fit")

	p := progOf(subNode(outer, progOf(subNode(middle, progOf(tmplNode(leaf))))))

	got := encodedNodePaths(p, "")
	require.Len(t, got, 1)
	assert.Equal(t, outer+"/"+middle+"/"+leaf, got[0], "the event must name the whole path, not the leaf")
	assert.True(t, metadata.NodeIDTokenIsHashed(got[0]))
}

func TestEncodedNodePaths_IgnoresNonTemplateKinds(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("a", 64)
	for _, kind := range []compiler.NodeKind{compiler.NodeKindRef, compiler.NodeKindDef, compiler.NodeKindPatch} {
		p := progOf(&compiler.Node{ID: long, Kind: kind})
		assert.Empty(t, encodedNodePaths(p, ""), "kind %s stamps no node-id label", kind)
	}
}

func TestEncodedNodePaths_IsStablyOrdered(t *testing.T) {
	t.Parallel()

	a, b, c := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)
	p := progOf(tmplNode(a), tmplNode(b), tmplNode(c))

	want := []string{a, b, c}
	for range 20 {
		assert.Equal(t, want, encodedNodePaths(p, ""), "order must follow TopologicalOrder, not map iteration")
	}
}

func TestEncodedNodePaths_ToleratesMissingAndNilPrograms(t *testing.T) {
	t.Parallel()

	assert.Nil(t, encodedNodePaths(nil, ""))
	assert.Empty(t, encodedNodePaths(&compiler.Program{
		Nodes:            map[string]*compiler.Node{},
		TopologicalOrder: []string{"ghost"},
	}, ""))
	assert.Empty(t, encodedNodePaths(progOf(subNode("subA", nil)), ""))
}

func TestWarnOnEncodedNodeIDs(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("a", 64)
	recorder := record.NewFakeRecorder(10)
	r := &Reconciler{Recorder: recorder}
	r.warnOnEncodedNodeIDs(graph("g"), progOf(tmplNode("frontend"), tmplNode(long)))

	var got []string
	for len(recorder.Events) > 0 {
		got = append(got, <-recorder.Events)
	}

	require.Len(t, got, 1, "only the over-long node should warn")
	assert.Contains(t, got[0], "Warning NodeIDEncoded")
	assert.Contains(t, got[0], long)
	assert.Contains(t, got[0], metadata.NodeIDToken(long))
	assert.Contains(t, got[0], metadata.NodePathAnnotation)
}

func TestWarnOnEncodedNodeIDs_NoRecorder(t *testing.T) {
	t.Parallel()

	r := &Reconciler{}
	assert.NotPanics(t, func() {
		r.warnOnEncodedNodeIDs(graph("g"), progOf(tmplNode(strings.Repeat("a", 64))))
	})
}

func TestReconcile_WarnsOnEveryReconcile(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("a", 64)
	g := graph("g", withFinalizer)
	recorder := record.NewFakeRecorder(10)
	r := &Reconciler{
		Client:   newClient(t, g),
		Compiler: &fakeCompiler{program: progOf(tmplNode(long))},
		Registry: registry.New(),
		Executor: &fakeExecutor{},
		Recorder: recorder,
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "g"}}

	for range 3 {
		_, err := r.Reconcile(context.Background(), req)
		require.NoError(t, err)
	}

	assert.Len(t, recorder.Events, 3,
		"a cache hit must still warn: the label stays hashed, so the event must not age out")
	assert.Contains(t, <-recorder.Events, "NodeIDEncoded")
}
