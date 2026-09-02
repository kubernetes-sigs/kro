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

package graph

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	expv1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/compiler"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/executor"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/registry"
	krotruntime "github.com/kubernetes-sigs/kro/pkg/graphengine/runtime"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/watchrouter"
)

// templateProgram builds a minimal compiled Program with a single static
// Template node whose rendered identity is (apiVersion, kind, namespace,
// name). No Variables/ForEach/IncludeWhen, so the node resolves in memory to
// exactly that object — enough for intendedManagedResources to project it.
func templateProgram(nodeID, apiVersion, kind, namespace, name string) *compiler.Program {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": apiVersion,
		"kind":       kind,
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
		},
	}}
	gv, _ := schema.ParseGroupVersion(apiVersion)
	node := &compiler.Node{
		ID:         nodeID,
		Kind:       compiler.NodeKindTemplate,
		GVR:        gv.WithKind(kind).GroupVersion().WithResource(kind),
		Namespaced: namespace != "",
		Object:     obj,
	}
	return &compiler.Program{
		Nodes:            map[string]*compiler.Node{nodeID: node},
		TopologicalOrder: []string{nodeID},
	}
}

// emptyNodeProgram builds a Program whose single node has no payload, so
// intendedManagedResources projects nothing and the pre-apply write-ahead is
// skipped. Used to isolate reconcile paths from the Finding A write-ahead.
func emptyNodeProgram(nodeID string) *compiler.Program {
	node := &compiler.Node{ID: nodeID, Kind: compiler.NodeKindTemplate}
	return &compiler.Program{
		Nodes:            map[string]*compiler.Node{nodeID: node},
		TopologicalOrder: []string{nodeID},
	}
}

// applyObservingExecutor records the ManagedResources PERSISTED on the API
// server at the moment Apply is entered, by reading them back through a
// captured client. This is what lets a test assert the pre-apply write-ahead
// (Finding A) landed on the server before any resource was applied.
type applyObservingExecutor struct {
	fakeExecutor
	cl  client.Client
	key types.NamespacedName
	// persistedAtApply is the server-side inventory observed when Apply ran.
	persistedAtApply []expv1alpha1.ManagedResource
	observed         bool
}

func (e *applyObservingExecutor) Apply(ctx context.Context, rt *krotruntime.Runtime, w watchrouter.Watcher) (executor.ApplyResult, error) {
	got := &expv1alpha1.Graph{}
	if err := e.cl.Get(ctx, e.key, got); err == nil {
		e.persistedAtApply = got.Status.ManagedResources
		e.observed = true
	}
	return e.fakeExecutor.Apply(ctx, rt, w)
}

// TestReconcile_WriteAheadIntentPersistedBeforeApply is the Finding A
// regression: the inventory teardown depends on must be durable on the API
// server BEFORE Apply creates any child. Before the fix the reconciler applied
// first and persisted the inventory only afterwards, so a lost status write
// after apply orphaned children (delete would see 0 entries). The fix
// write-aheads the union of previous + intended identities before Apply.
func TestReconcile_WriteAheadIntentPersistedBeforeApply(t *testing.T) {
	t.Parallel()
	key := types.NamespacedName{Namespace: "default", Name: "g"}

	g := graph("g", withFinalizer)
	cl := newClient(t, g)

	obs := &applyObservingExecutor{cl: cl, key: key}
	fc := &fakeCompiler{program: templateProgram("widget", "example.com/v1", "Widget", "default", "w")}
	r := &Reconciler{Client: cl, Compiler: fc, Registry: registry.New(), Executor: obs}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	require.NoError(t, err)

	require.True(t, obs.observed, "Apply must have been called")
	// The server-side inventory observed AT apply time must already contain
	// the intended Widget identity. Pre-fix this slice is empty.
	require.Len(t, obs.persistedAtApply, 1,
		"pre-apply intent must be persisted to the API server before Apply runs")
	mr := obs.persistedAtApply[0]
	assert.Equal(t, "example.com/v1", mr.APIVersion)
	assert.Equal(t, "Widget", mr.Kind)
	assert.Equal(t, "w", mr.Name)
	assert.Equal(t, "widget", mr.NodeID)
}

// TestIntendedManagedResources_ProjectsTemplateIdentities is a focused unit
// test for the projection helper Finding A relies on.
func TestIntendedManagedResources_ProjectsTemplateIdentities(t *testing.T) {
	t.Parallel()
	g := graph("g")
	prog := templateProgram("widget", "example.com/v1", "Widget", "default", "w")
	rt := krotruntime.New(prog, g)

	got := intendedManagedResources(rt)
	require.Len(t, got, 1)
	assert.Equal(t, "example.com/v1", got[0].APIVersion)
	assert.Equal(t, "Widget", got[0].Kind)
	assert.Equal(t, "w", got[0].Name)
	assert.Empty(t, got[0].UID, "pre-apply intent carries no UID")
}

// TestIntendedManagedResources_SkipsDynamicGVKWithoutNamespace pins the
// tracking.go:161 fix: a dynamic-GVK node has no compile-time REST scope
// (Namespaced()==false), so a rendered object with NO explicit namespace can't
// be namespace-defaulted in the projection the way the executor will at apply
// time. Emitting a ns="" intent entry would never dedup against the applied
// entry (ns=graph), churning status every cycle — so it must be skipped. A
// dynamic node that DOES set an explicit namespace keeps its intent entry.
func TestIntendedManagedResources_SkipsDynamicGVKWithoutNamespace(t *testing.T) {
	t.Parallel()
	g := graph("g") // namespace "default"

	dynNoNS := func(nodeID, name, namespace string) *compiler.Node {
		obj := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "example.com/v1",
			"kind":       "Widget",
			"metadata":   map[string]any{"name": name},
		}}
		if namespace != "" {
			_ = unstructured.SetNestedField(obj.Object, namespace, "metadata", "namespace")
		}
		return &compiler.Node{
			ID:         nodeID,
			Kind:       compiler.NodeKindTemplate,
			DynamicGVK: true,
			Namespaced: false, // dynamic: unknown at compile time
			Object:     obj,
		}
	}

	t.Run("dynamic node without explicit namespace is skipped", func(t *testing.T) {
		n := dynNoNS("dyn", "w", "")
		prog := &compiler.Program{
			Nodes:            map[string]*compiler.Node{"dyn": n},
			TopologicalOrder: []string{"dyn"},
		}
		got := intendedManagedResources(krotruntime.New(prog, g))
		assert.Empty(t, got, "a dynamic-GVK node with no explicit namespace must not emit a ns=\"\" intent entry")
	})

	t.Run("dynamic node with explicit namespace is kept", func(t *testing.T) {
		n := dynNoNS("dyn", "w", "other-ns")
		prog := &compiler.Program{
			Nodes:            map[string]*compiler.Node{"dyn": n},
			TopologicalOrder: []string{"dyn"},
		}
		got := intendedManagedResources(krotruntime.New(prog, g))
		require.Len(t, got, 1, "an explicit namespace is a stable identity and must be tracked")
		assert.Equal(t, "other-ns", got[0].Namespace)
		assert.Equal(t, "w", got[0].Name)
	})
}

// TestIntendedContributions_MatchesExecutorFieldManager pins the contribution
// write-ahead (graph/controller.go:314): the projected FieldManager MUST equal
// what the executor applies under, or the write-ahead ledger entry would never
// correlate with the contribution Release later looks for. Both derive it from
// the single shared executor.PatchFieldManager(graphUID, nodeID), so this
// asserts the projection reproduces that exact identity for a patch node.
func TestIntendedContributions_MatchesExecutorFieldManager(t *testing.T) {
	t.Parallel()
	g := graph("g") // namespace "default"
	g.SetUID(types.UID("graph-uid-123"))

	patchObj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]any{"name": "target", "namespace": "default"},
		"data":       map[string]any{"k": "v"},
	}}
	patchNode := &compiler.Node{
		ID:         "p",
		Kind:       compiler.NodeKindPatch,
		Namespaced: true,
		Object:     patchObj,
	}
	prog := &compiler.Program{
		Nodes:            map[string]*compiler.Node{"p": patchNode},
		TopologicalOrder: []string{"p"},
	}
	rt := krotruntime.New(prog, g)

	got := intendedContributions(rt)
	require.Len(t, got, 1, "the patch node's contribution must be projected")
	c := got[0]
	assert.Equal(t, "v1", c.APIVersion)
	assert.Equal(t, "ConfigMap", c.Kind)
	assert.Equal(t, "default", c.Namespace)
	assert.Equal(t, "target", c.Name)
	// The crux: the projected field manager is byte-identical to the executor's.
	assert.Equal(t, executor.PatchFieldManager("graph-uid-123", "p"), c.FieldManager,
		"write-ahead FieldManager must match the executor's, or Release cannot correlate the ledger entry")
}

// subgraphProgram wraps one or more child programs as inline subgraph
// (NodeKindGraph) nodes at the root, mirroring how the compiler emits a
// `graph:` node (Kind=NodeKindGraph, SubProgram=<child>). Each entry's key is
// the subgraph node ID; the value is the compiled child Program. Used to build
// realistic nested-frame runtimes for the write-ahead projection tests.
func subgraphProgram(children map[string]*compiler.Program) *compiler.Program {
	nodes := make(map[string]*compiler.Node, len(children))
	order := make([]string, 0, len(children))
	for id, child := range children {
		nodes[id] = &compiler.Node{ID: id, Kind: compiler.NodeKindGraph, SubProgram: child}
		order = append(order, id)
	}
	return &compiler.Program{Nodes: nodes, TopologicalOrder: order}
}

// patchProgram builds a minimal compiled Program with a single static Patch
// node whose rendered target identity is (apiVersion, kind, namespace, name).
func patchProgram(nodeID, apiVersion, kind, namespace, name string) *compiler.Program {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": apiVersion,
		"kind":       kind,
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
		},
		"data": map[string]any{"k": "v"},
	}}
	node := &compiler.Node{
		ID:         nodeID,
		Kind:       compiler.NodeKindPatch,
		Namespaced: namespace != "",
		Object:     obj,
	}
	return &compiler.Program{
		Nodes:            map[string]*compiler.Node{nodeID: node},
		TopologicalOrder: []string{nodeID},
	}
}

// TestIntendedManagedResources_RecursesSubgraphs pins the tracking.go:137 fix:
// a template node declared inside an inline subgraph is applied by the executor
// (applySubgraph) but, before the fix, had NO write-ahead inventory entry — a
// crash between Apply and the post-apply status persist would orphan it. The
// projection must recurse subgraph frames to arbitrary depth, qualifying each
// child NodeID with the subgraph prefix exactly as the executor records it.
func TestIntendedManagedResources_RecursesSubgraphs(t *testing.T) {
	t.Parallel()
	g := graph("g") // namespace "default"

	t.Run("one level deep qualifies sub/child", func(t *testing.T) {
		t.Parallel()
		child := templateProgram("child", "example.com/v1", "Widget", "default", "w")
		prog := subgraphProgram(map[string]*compiler.Program{"sub": child})
		rt := krotruntime.New(prog, g)

		got := intendedManagedResources(rt)
		require.Len(t, got, 1, "the subgraph's template node must be projected")
		assert.Equal(t, "sub/child", got[0].NodeID, "child NodeID is qualified with the subgraph prefix")
		assert.Equal(t, "example.com/v1", got[0].APIVersion)
		assert.Equal(t, "Widget", got[0].Kind)
		assert.Equal(t, "default", got[0].Namespace)
		assert.Equal(t, "w", got[0].Name)
		assert.Empty(t, got[0].UID, "pre-apply intent carries no UID")
	})

	t.Run("two levels deep qualifies subA/subB/leaf", func(t *testing.T) {
		t.Parallel()
		leaf := templateProgram("leaf", "example.com/v1", "Widget", "default", "w")
		inner := subgraphProgram(map[string]*compiler.Program{"subB": leaf})
		prog := subgraphProgram(map[string]*compiler.Program{"subA": inner})
		rt := krotruntime.New(prog, g)

		got := intendedManagedResources(rt)
		require.Len(t, got, 1, "a template nested two subgraphs deep must be projected")
		assert.Equal(t, "subA/subB/leaf", got[0].NodeID, "nested NodeIDs stack the subgraph prefix")
		assert.Equal(t, "Widget", got[0].Kind)
		assert.Equal(t, "w", got[0].Name)
	})

	t.Run("top-level and nested template both projected", func(t *testing.T) {
		t.Parallel()
		child := templateProgram("child", "example.com/v1", "Gadget", "default", "gg")
		prog := subgraphProgram(map[string]*compiler.Program{"sub": child})
		// Add a top-level template alongside the subgraph node.
		prog.Nodes["top"] = templateProgram("top", "example.com/v1", "Widget", "default", "w").Nodes["top"]
		prog.TopologicalOrder = []string{"top", "sub"}
		rt := krotruntime.New(prog, g)

		got := intendedManagedResources(rt)
		require.Len(t, got, 2)
		byNode := map[string]expv1alpha1.ManagedResource{}
		for _, mr := range got {
			byNode[mr.NodeID] = mr
		}
		require.Contains(t, byNode, "top")
		require.Contains(t, byNode, "sub/child")
		assert.Equal(t, "Widget", byNode["top"].Kind)
		assert.Equal(t, "Gadget", byNode["sub/child"].Kind)
	})

	t.Run("dynamic-no-namespace child is still skipped inside a subgraph", func(t *testing.T) {
		t.Parallel()
		dynObj := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "example.com/v1",
			"kind":       "Widget",
			"metadata":   map[string]any{"name": "w"},
		}}
		dynNode := &compiler.Node{
			ID:         "dyn",
			Kind:       compiler.NodeKindTemplate,
			DynamicGVK: true,
			Namespaced: false,
			Object:     dynObj,
		}
		child := &compiler.Program{
			Nodes:            map[string]*compiler.Node{"dyn": dynNode},
			TopologicalOrder: []string{"dyn"},
		}
		prog := subgraphProgram(map[string]*compiler.Program{"sub": child})
		rt := krotruntime.New(prog, g)

		got := intendedManagedResources(rt)
		assert.Empty(t, got, "the dynamic-no-namespace skip must hold inside a subgraph frame too")
	})
}

// TestIntendedContributions_RecursesSubgraphs is the patch twin of
// TestIntendedManagedResources_RecursesSubgraphs: a patch node inside an inline
// subgraph must be projected, and its FieldManager MUST equal the executor's
// for the QUALIFIED node path (prefix+localID) — the executor derives it from
// patchFieldManager(uid, s.qualifiedPath(n.ID())) where qualifiedPath =
// nodePrefix+id, and applySubgraph extends nodePrefix by "<subID>/". If this
// drifts, the write-ahead ledger entry never correlates with the contribution
// Release later looks for.
func TestIntendedContributions_RecursesSubgraphs(t *testing.T) {
	t.Parallel()
	g := graph("g") // namespace "default"
	g.SetUID(types.UID("graph-uid-123"))

	t.Run("one level deep field manager matches executor for sub/patch", func(t *testing.T) {
		t.Parallel()
		child := patchProgram("patch", "v1", "ConfigMap", "default", "target")
		prog := subgraphProgram(map[string]*compiler.Program{"sub": child})
		rt := krotruntime.New(prog, g)

		got := intendedContributions(rt)
		require.Len(t, got, 1, "the subgraph's patch node contribution must be projected")
		c := got[0]
		assert.Equal(t, "v1", c.APIVersion)
		assert.Equal(t, "ConfigMap", c.Kind)
		assert.Equal(t, "default", c.Namespace)
		assert.Equal(t, "target", c.Name)
		// The crux: field manager is byte-identical to the executor's for the
		// QUALIFIED path "sub/patch", not the bare local id.
		assert.Equal(t, executor.PatchFieldManager("graph-uid-123", "sub/patch"), c.FieldManager,
			"write-ahead FieldManager must match the executor's qualified-path derivation")
		// And it must NOT be the (wrong) bare-id derivation.
		assert.NotEqual(t, executor.PatchFieldManager("graph-uid-123", "patch"), c.FieldManager,
			"a bare-id field manager would not correlate with the executor's qualified apply")
	})

	t.Run("two levels deep field manager matches executor for subA/subB/patch", func(t *testing.T) {
		t.Parallel()
		leaf := patchProgram("patch", "v1", "ConfigMap", "default", "target")
		inner := subgraphProgram(map[string]*compiler.Program{"subB": leaf})
		prog := subgraphProgram(map[string]*compiler.Program{"subA": inner})
		rt := krotruntime.New(prog, g)

		got := intendedContributions(rt)
		require.Len(t, got, 1)
		assert.Equal(t, executor.PatchFieldManager("graph-uid-123", "subA/subB/patch"), got[0].FieldManager,
			"nested patch field managers stack the subgraph prefix, matching the executor")
	})

	t.Run("dynamic-no-namespace patch child is still skipped inside a subgraph", func(t *testing.T) {
		t.Parallel()
		dynObj := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "example.com/v1",
			"kind":       "Widget",
			"metadata":   map[string]any{"name": "target"},
			"data":       map[string]any{"k": "v"},
		}}
		dynNode := &compiler.Node{
			ID:         "dynp",
			Kind:       compiler.NodeKindPatch,
			DynamicGVK: true,
			Namespaced: false,
			Object:     dynObj,
		}
		child := &compiler.Program{
			Nodes:            map[string]*compiler.Node{"dynp": dynNode},
			TopologicalOrder: []string{"dynp"},
		}
		prog := subgraphProgram(map[string]*compiler.Program{"sub": child})
		rt := krotruntime.New(prog, g)

		got := intendedContributions(rt)
		assert.Empty(t, got, "the dynamic-no-namespace skip must hold for patch nodes inside a subgraph too")
	})
}

// the reconciler believes they succeeded) then delegates. It simulates a lost
// status write — the exact crash window Finding A guards.
//
// (Retained as documentation of the crash model; Finding B's regression uses
// patchErrClient to fail the terminal status write directly.)

// TestReconcile_StatusWriteErrorNotDiscardedOnNotReady is the Finding B
// regression: when Apply returns a soft ErrNotReady AND updateStatus fails,
// the joined error still matched errors.Is(ErrNotReady) and the reconcile
// returned nil, silently discarding the status-write failure. The fix keeps
// the status-write error separate and surfaces it regardless of the not-ready
// branch.
func TestReconcile_StatusWriteErrorNotDiscardedOnNotReady(t *testing.T) {
	t.Parallel()
	key := types.NamespacedName{Namespace: "default", Name: "g"}

	g := graph("g", withFinalizer)
	cl := newClient(t, g)
	// updateStatus is the reconcile's terminal status Patch. Fail it.
	wrapped := &patchErrClient{Client: cl, statusErr: errors.New("status boom")}

	exec := &fakeExecutor{applyErr: fmt.Errorf("apply %q: %w", "n", executor.ErrNotReady)}
	// Use an empty (payload-less) node so the pre-apply write-ahead projects
	// nothing and does NOT fire — this isolates the failing write to the
	// TERMINAL updateStatus, which is exactly the path Finding B guards.
	r := &Reconciler{Client: wrapped, Compiler: &fakeCompiler{program: emptyNodeProgram("n")}, Registry: registry.New(), Executor: exec}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	// Pre-fix: err == nil (status failure swallowed by the ErrNotReady branch).
	require.Error(t, err, "a failed status write must never be discarded, even on soft not-ready")
	assert.Contains(t, err.Error(), "status boom")
}

// TestReconcile_ReleaseReachableOnSoftNotReady is the reachability half of
// Finding C: when Apply returns a soft ErrNotReady, the early apply-error
// TestReconcile_NoReleaseOnSoftNotReady pins the corrected Finding C contract:
// release of patch contributions runs on the CLEAN-apply path ONLY. On a soft
// ErrNotReady, a patch node's contribution is absent from result.Contributions
// whether it was genuinely removed OR is merely data-pending this cycle, and
// executor.Contribution carries no NodeID to tell those apart — so releasing
// here would drop fields a still-wanted patch node set (a transient flap). The
// field-manager-identity-change deadlock this path once tried to break is now
// fixed at the source in the executor (contributeApply force-reclaims a
// same-Graph stale patch identity), so no controller-side release on soft
// errors is needed.
func TestReconcile_NoReleaseOnSoftNotReady(t *testing.T) {
	t.Parallel()
	key := types.NamespacedName{Namespace: "default", Name: "g"}

	// Prior contribution recorded on the Graph; this cycle Apply reports NO
	// contributions and a soft ErrNotReady with the patch node Unresolved
	// (data-pending, still wanted).
	prior := []executor.Contribution{{
		APIVersion:   "v1",
		Kind:         "ConfigMap",
		Namespace:    "default",
		Name:         "target",
		FieldManager: "kro-graphengine.patch.oldidentity",
	}}

	g := graph("g", withFinalizer, func(g *expv1alpha1.Graph) {
		g.Status.Contributions = toAPIContributions(prior)
	})
	cl := newClient(t, g)

	exec := &fakeExecutor{
		applyErr:    fmt.Errorf("apply %q (patch): %w", "p", executor.ErrNotReady),
		applyResult: executor.ApplyResult{Unresolved: []string{"p"}},
	}
	r := &Reconciler{Client: cl, Compiler: &fakeCompiler{program: emptyNodeProgram("n")}, Registry: registry.New(), Executor: exec}

	_, _ = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})

	// The prior contribution must NOT be released on a soft not-ready cycle:
	// a data-pending patch node is still wanted, and releasing its fields would
	// flap them until the node resolves next cycle.
	assert.Empty(t, exec.releaseCalls, "release must not fire on soft not-ready (would flap a data-pending patch's fields)")
}

// TestReconcile_SoftNotReadyStillPrunesRetiredNode pins finding 357: a node
// that is soft not-ready this cycle must NOT veto pruning of an UNRELATED
// resource whose owning node was removed from the spec. Previously all pruning
// was gated on a fully clean apply, so one never-ready node leaked every
// retired resource until it resolved. diffManagedResources keeps unresolved
// nodes' entries, so a prune candidate on a soft cycle is genuinely retired and
// safe to delete.
func TestReconcile_SoftNotReadyStillPrunesRetiredNode(t *testing.T) {
	t.Parallel()
	key := types.NamespacedName{Namespace: "default", Name: "g"}

	// Previously-tracked resource owned by node "gone", which is no longer in
	// the graph. A separate node "widget" is not-ready this cycle.
	g := graph("g", withFinalizer, func(g *expv1alpha1.Graph) {
		g.Status.ManagedResources = []expv1alpha1.ManagedResource{{
			NodeID:     "gone",
			APIVersion: "v1",
			Kind:       "ConfigMap",
			Namespace:  "default",
			Name:       "retired-cm",
			UID:        "uid-retired",
		}}
	})
	cl := newClient(t, g)

	// Apply: soft not-ready, node "widget" Unresolved, nothing applied. The
	// "gone" resource is neither Applied nor Unresolved -> a prune candidate.
	exec := &fakeExecutor{
		applyErr:    fmt.Errorf("apply %q: %w", "widget", executor.ErrNotReady),
		applyResult: executor.ApplyResult{Unresolved: []string{"widget"}},
	}
	r := &Reconciler{Client: cl, Compiler: &fakeCompiler{program: emptyNodeProgram("widget")}, Registry: registry.New(), Executor: exec}

	_, _ = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})

	// The retired resource must have been pruned despite the soft not-ready.
	require.Len(t, exec.deleteCalls, 1, "prune must run on a soft not-ready cycle for a retired node")
	require.Len(t, exec.deleteCalls[0], 1)
	assert.Equal(t, "retired-cm", exec.deleteCalls[0][0].Name,
		"the retired node's resource is the prune candidate")

	// Persisted status must no longer track the pruned resource.
	got := &expv1alpha1.Graph{}
	require.NoError(t, cl.Get(context.Background(), key, got))
	for _, mr := range got.Status.ManagedResources {
		assert.NotEqual(t, "retired-cm", mr.Name, "a successfully pruned resource must drop from status")
	}
}

// TestReconcile_ErrorPathKeepsIntentSuperset guards the Finding A hardening:
// on a soft apply error the in-memory status (which the terminal updateStatus
// overwrites onto the server) must not shrink below the written-ahead intent,
// so a partially-applied resource still has a durable inventory entry.
func TestReconcile_ErrorPathKeepsIntentSuperset(t *testing.T) {
	t.Parallel()
	key := types.NamespacedName{Namespace: "default", Name: "g"}

	g := graph("g", withFinalizer)
	cl := newClient(t, g)

	// Apply reports a soft not-ready and an EMPTY Applied set (nothing observed
	// this cycle), simulating a crash/partial apply. The intent projected from
	// the template must still land in persisted status.
	exec := &fakeExecutor{
		applyErr:    fmt.Errorf("apply %q: %w", "widget", executor.ErrNotReady),
		applyResult: executor.ApplyResult{Unresolved: []string{"widget"}},
	}
	fc := &fakeCompiler{program: templateProgram("widget", "example.com/v1", "Widget", "default", "w")}
	r := &Reconciler{Client: cl, Compiler: fc, Registry: registry.New(), Executor: exec}

	_, _ = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})

	got := &expv1alpha1.Graph{}
	require.NoError(t, cl.Get(context.Background(), key, got))
	require.Len(t, got.Status.ManagedResources, 1,
		"intent superset must survive a soft-error cycle in persisted status")
	assert.Equal(t, "Widget", got.Status.ManagedResources[0].Kind)
	assert.Equal(t, "w", got.Status.ManagedResources[0].Name)
}
