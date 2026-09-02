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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	krotruntime "github.com/kubernetes-sigs/kro/pkg/graphengine/runtime"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/testutil/generator"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/watchrouter"
)

// TestTemplateFieldManager_StableAndPerGraph verifies the per-Graph template
// manager is deterministic (stable across reconciles) and distinct across
// Graphs and across sibling subgraph nodes that reuse a local id.
func TestTemplateFieldManager_StableAndPerGraph(t *testing.T) {
	t.Parallel()

	a := templateFieldManager(types.UID("graph-a"))
	b := templateFieldManager(types.UID("graph-b"))

	assert.Equal(t, a, templateFieldManager(types.UID("graph-a")), "same Graph UID is stable")
	assert.NotEqual(t, a, b, "distinct Graph UIDs get distinct managers")
	// The manager is per-GRAPH, not per-node: every node of one Graph shares one
	// template manager so ownership is stable across a node rename (SSA narrows
	// the field set instead of orphaning the retired node's fields).
	assert.Equal(t,
		templateFieldManager(types.UID("g")),
		templateFieldManager(types.UID("g")),
		"all nodes of one Graph share a single per-Graph template manager")
	assert.Contains(t, a, templateFieldManagerPrefix, "carries the tmpl prefix")
}

// TestOwnedByForeignGraphTemplate classifies a conflicting owner as a peer
// Graph (reject) vs external drift (force-reclaim).
func TestOwnedByForeignGraphTemplate(t *testing.T) {
	t.Parallel()

	self := templateFieldManager(types.UID("me"))
	peer := templateFieldManager(types.UID("peer"))

	withManagers := func(names ...string) *unstructured.Unstructured {
		o := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "cm", "namespace": "default"},
		}}
		mf := make([]metav1.ManagedFieldsEntry, 0, len(names))
		for _, n := range names {
			mf = append(mf, metav1.ManagedFieldsEntry{Manager: n, Operation: metav1.ManagedFieldsOperationApply})
		}
		o.SetManagedFields(mf)
		return o
	}

	assert.False(t, ownedByForeignGraphTemplate(nil, self), "nil current is never foreign-owned")
	assert.False(t, ownedByForeignGraphTemplate(withManagers(), self), "no managers is never foreign-owned")
	assert.False(t, ownedByForeignGraphTemplate(withManagers(self), self), "self ownership is not foreign")
	assert.False(t, ownedByForeignGraphTemplate(withManagers("kubectl-client-side-apply"), self),
		"a foreign non-kro manager is external drift, not a peer Graph")
	assert.False(t, ownedByForeignGraphTemplate(withManagers(FieldManager), self),
		"the shared RGD field manager is not a Graph template writer")
	assert.True(t, ownedByForeignGraphTemplate(withManagers(peer), self),
		"another Graph's template manager is foreign-owned")
	assert.True(t, ownedByForeignGraphTemplate(withManagers("kubectl", peer), self),
		"a peer Graph among other managers is still foreign-owned")
}

// TestOwnedByForeignGraphTemplate_SameGraphNotForeign verifies a Graph never
// reports its OWN template ownership as a foreign peer. With the per-Graph
// template manager, self's prior apply matches directly; and a LEGACY per-node
// manager of the same Graph (shape "<prefix><graphSeg>.<nodeSeg>", as a
// pre-upgrade controller may have left on a live object) shares self's per-Graph
// segment and is likewise exempt. A genuine peer Graph (different UID) is still
// foreign.
func TestOwnedByForeignGraphTemplate_SameGraphNotForeign(t *testing.T) {
	t.Parallel()

	self := templateFieldManager(types.UID("same-graph"))
	// A legacy per-node manager of the SAME Graph: same graphSegment, plus a
	// ".<nodeSegment>" suffix. Build it from the shared fieldManager helper the
	// pre-upgrade controller used, so the graphSegment matches self exactly.
	legacySibling := fieldManager(templateFieldManagerPrefix, types.UID("same-graph"), "nodeB")
	require.NotEqual(t, self, legacySibling, "a legacy per-node manager differs from the per-Graph manager")

	withManagers := func(names ...string) *unstructured.Unstructured {
		o := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "cm", "namespace": "default"},
		}}
		mf := make([]metav1.ManagedFieldsEntry, 0, len(names))
		for _, n := range names {
			mf = append(mf, metav1.ManagedFieldsEntry{Manager: n, Operation: metav1.ManagedFieldsOperationApply})
		}
		o.SetManagedFields(mf)
		return o
	}

	assert.False(t, ownedByForeignGraphTemplate(withManagers(legacySibling), self),
		"a legacy per-node manager of the SAME Graph is not foreign")
	assert.False(t, ownedByForeignGraphTemplate(withManagers("kubectl", legacySibling), self),
		"a same-Graph legacy manager among other managers is still not foreign")

	// A genuine peer Graph (different UID) must still be foreign even when a
	// same-Graph legacy manager is also present.
	peer := templateFieldManager(types.UID("other-graph"))
	assert.True(t, ownedByForeignGraphTemplate(withManagers(legacySibling, peer), self),
		"a real peer Graph is still foreign even alongside a same-Graph legacy manager")
}

// TestAnyConflictOwnedByForeignGraphTemplate pins reviewer finding 3909839245:
// the peer-vs-drift decision on the template apply path must read the 409's OWN
// conflict causes, not a pre-apply snapshot of the live object, so a peer that
// took ownership in the read-then-write window (and thus caused the 409) is
// seen and never force-stolen. This classifier is the error-driven replacement
// for the stale-snapshot ownedByForeignGraphTemplate(current, ...) check.
func TestAnyConflictOwnedByForeignGraphTemplate(t *testing.T) {
	t.Parallel()

	self := templateFieldManager(types.UID("me"))
	peer := templateFieldManager(types.UID("peer"))
	// A legacy per-node manager of the SAME Graph as self shares self's per-Graph
	// segment (built via the shared fieldManager helper on the same UID).
	legacySibling := fieldManager(templateFieldManagerPrefix, types.UID("me"), "nodeB")

	// conflictErr builds a 409 Conflict whose Details.Causes name the given
	// managers as field-manager conflicts, mirroring the apiserver's SSA 409.
	conflictErr := func(managers ...string) error {
		causes := make([]metav1.StatusCause, 0, len(managers))
		for _, m := range managers {
			causes = append(causes, metav1.StatusCause{
				Type:    metav1.CauseTypeFieldManagerConflict,
				Message: `conflict with "` + m + `" using v1`,
				Field:   "data.owner",
			})
		}
		return &apierrors.StatusError{ErrStatus: metav1.Status{
			Status:  metav1.StatusFailure,
			Reason:  metav1.StatusReasonConflict,
			Details: &metav1.StatusDetails{Causes: causes},
		}}
	}

	assert.True(t, anyConflictOwnedByForeignGraphTemplate(conflictErr(peer), self),
		"a peer Graph's template manager in the causes is foreign-owned")
	assert.True(t, anyConflictOwnedByForeignGraphTemplate(conflictErr("kubectl", peer), self),
		"a peer Graph among other conflicting managers is still foreign-owned")
	assert.False(t, anyConflictOwnedByForeignGraphTemplate(conflictErr("kubectl-edit"), self),
		"external (non-tmpl) drift is not foreign-Graph-owned; it is force-reclaimed")
	assert.False(t, anyConflictOwnedByForeignGraphTemplate(conflictErr(self), self),
		"self ownership is never foreign")
	assert.False(t, anyConflictOwnedByForeignGraphTemplate(conflictErr(legacySibling), self),
		"a legacy per-node manager of the SAME Graph is not foreign")
	assert.True(t, anyConflictOwnedByForeignGraphTemplate(conflictErr(legacySibling, peer), self),
		"a real peer is foreign even alongside a same-Graph legacy manager")

	// Conservative fallbacks: an error with no parseable conflict causes, or a
	// conflict cause whose manager can't be read, must be treated as foreign so
	// an ambiguous conflict is never force-stolen.
	assert.True(t, anyConflictOwnedByForeignGraphTemplate(errors.New("opaque"), self),
		"an unparseable error is treated as foreign (never force-steal on ambiguity)")
	assert.True(t, anyConflictOwnedByForeignGraphTemplate(conflictErr(""), self),
		"a conflict cause with no readable manager is treated as foreign")
}

// contestedGraph builds and compiles a Graph that templates a single ConfigMap
// named "contested" in ns with data.owner=owner — Skarlso's repro shape.
func contestedGraph(t *testing.T, name, owner, ns string) *krotruntime.Runtime {
	t.Helper()
	g := generator.NewGraph(name,
		generator.WithNamespace(ns),
		generator.WithTemplate("cm", map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "contested", "namespace": ns},
			"data":     map[string]any{"owner": owner},
		}),
	)
	g.SetUID(types.UID("uid-" + name))
	return compileAndBuildEnv(t, patchEnvCfg, g)
}

// TestTemplate_PeerGraphConflict_NoFlipFlop is the regression test for
// https://github.com/kubernetes-sigs/kro/pull/1355#issuecomment-5412875343:
// two Graphs that template the same ConfigMap with different data must NOT
// flip-flop it. With ConflictDetection on, the first Graph to apply owns the
// object; the second is REFUSED at apply because the live object is already
// owned by a peer Graph's template manager. Per cheeseandcereal's approved
// decision (reject-to-co-manage, uniform-SOFT policy) this refusal is SOFT
// not-ready (ErrFieldManagerConflict wrapping ErrNotReady), matching both the
// same-field peer conflict and the RGD path's ownedByGraphTemplate guard: the
// reconcile backs off and self-heals if the peer releases, rather than going
// permanently degraded. The second Graph records NOTHING in Applied (so Delete
// never sees the object) and never overwrites the value. Requires envtest for
// real SSA managed-field tracking.
func TestTemplate_PeerGraphConflict_NoFlipFlop(t *testing.T) {
	cl := patchEnvClient(t)
	ns := "default"
	ctx := context.Background()

	ownerA := contestedGraph(t, "owner-a", "a", ns)
	ownerB := contestedGraph(t, "owner-b", "b", ns)

	exec := NewSimple(cl).WithConflictDetection(true)

	// owner-a applies first and takes ownership of data.owner.
	resA, err := exec.Apply(ctx, ownerA, watchrouter.NoopWatcher{})
	require.NoError(t, err)
	require.Len(t, resA.Applied, 1)

	cm := getConfigMap(t, cl, ns, "contested")
	data, _, _ := unstructured.NestedStringMap(cm.Object, "data")
	require.Equal(t, "a", data["owner"], "owner-a owns the field after first apply")

	// owner-b now tries to co-manage the same object: it must be SOFT-refused as
	// a peer-owned object, record nothing in Applied, and the value must NOT
	// change across repeated tries.
	for range 3 {
		resB, err := exec.Apply(ctx, ownerB, watchrouter.NoopWatcher{})
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrFieldManagerConflict), "peer-owned object is refused for co-management")
		assert.True(t, errors.Is(err, ErrNotReady), "peer-adoption refusal is SOFT not-ready (self-heals if the peer releases)")
		assert.Empty(t, resB.Applied, "a refused peer-owned object is never recorded in inventory")

		cm := getConfigMap(t, cl, ns, "contested")
		data, _, _ := unstructured.NestedStringMap(cm.Object, "data")
		assert.Equal(t, "a", data["owner"], "owner-a's value is stable — no flip-flop")
	}

	// owner-a re-applies idempotently: still owns, no conflict, no change.
	_, err = exec.Apply(ctx, ownerA, watchrouter.NoopWatcher{})
	require.NoError(t, err)
	cm = getConfigMap(t, cl, ns, "contested")
	data, _, _ = unstructured.NestedStringMap(cm.Object, "data")
	assert.Equal(t, "a", data["owner"])
}

// TestTemplate_PeerGraphDisjointFields_RefusedNotAdopted is the regression test
// for cheeseandcereal's review on the Delete path
// (pkg/graphengine/executor/simple.go): because multiple Graphs could manage
// the same resource without conflict detection catching DISJOINT field sets, a
// second Graph could adopt a peer's object, record it in inventory, and later
// delete it. The approved fix rejects at apply: a standalone Graph refuses to
// ADOPT an object already owned by a peer Graph's template manager BEFORE apply,
// so it never enters inventory. Here Graph A owns data.a; Graph B (different
// UID) templates the SAME ConfigMap setting only data.b — a DISJOINT field, so
// there is no SSA field-level 409. Before the fix Graph B silently adopted the
// object and wrote data.b; after the fix Graph B is SOFT-refused
// (ErrFieldManagerConflict wrapping ErrNotReady), records nothing in Applied,
// and data.b is never written. Requires envtest for real SSA managed-field tracking.
func TestTemplate_PeerGraphDisjointFields_RefusedNotAdopted(t *testing.T) {
	cl := patchEnvClient(t)
	ns := "default"
	ctx := context.Background()

	// Graph A owns data.a on the shared object.
	graphA := generator.NewGraph("disjoint-a",
		generator.WithNamespace(ns),
		generator.WithTemplate("cm", map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "disjoint", "namespace": ns},
			"data":     map[string]any{"a": "from-a"},
		}),
	)
	graphA.SetUID(types.UID("uid-disjoint-a"))
	rtA := compileAndBuildEnv(t, patchEnvCfg, graphA)

	// Graph B (DIFFERENT UID) templates the SAME object but only sets data.b,
	// a DISJOINT field — no field-level SSA conflict with Graph A's data.a.
	graphB := generator.NewGraph("disjoint-b",
		generator.WithNamespace(ns),
		generator.WithTemplate("cm", map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "disjoint", "namespace": ns},
			"data":     map[string]any{"b": "from-b"},
		}),
	)
	graphB.SetUID(types.UID("uid-disjoint-b"))
	rtB := compileAndBuildEnv(t, patchEnvCfg, graphB)

	exec := NewSimple(cl).WithConflictDetection(true)

	// Graph A applies first and takes ownership of data.a.
	resA, err := exec.Apply(ctx, rtA, watchrouter.NoopWatcher{})
	require.NoError(t, err)
	require.Len(t, resA.Applied, 1, "owner-a records its object in inventory")

	cm := getConfigMap(t, cl, ns, "disjoint")
	data, _, _ := unstructured.NestedStringMap(cm.Object, "data")
	require.Equal(t, "from-a", data["a"], "graph A owns data.a")

	// Graph B tries to manage the same object with a DISJOINT field. It must be
	// SOFT-refused (peer-owned, wraps ErrNotReady), record NOTHING in Applied, and
	// data.b must never be written — repeated tries must stay stable (no adoption).
	for range 3 {
		resB, err := exec.Apply(ctx, rtB, watchrouter.NoopWatcher{})
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrFieldManagerConflict),
			"disjoint-field adoption of a peer's object is refused with ErrFieldManagerConflict")
		assert.True(t, errors.Is(err, ErrNotReady),
			"peer-adoption refusal is SOFT not-ready (self-heals if the peer releases)")
		assert.Empty(t, resB.Applied,
			"the refused object is never recorded in inventory, so Delete never sees it")

		cm := getConfigMap(t, cl, ns, "disjoint")
		data, _, _ := unstructured.NestedStringMap(cm.Object, "data")
		assert.Equal(t, "from-a", data["a"], "graph A's field is untouched")
		_, hasB := data["b"]
		assert.False(t, hasB, "graph B's disjoint field is NOT written — no silent double-management")
	}
}

// TestTemplate_ExternalDrift_ForceReclaimed verifies conflict detection does
// not break drift correction: a foreign (non-kro) manager editing a
// template-owned field is reclaimed by a forced re-apply, so a hand-edit still
// converges back — only a PEER GRAPH is refused.
func TestTemplate_ExternalDrift_ForceReclaimed(t *testing.T) {
	cl := patchEnvClient(t)
	ns := "default"
	ctx := context.Background()

	g := generator.NewGraph("drift",
		generator.WithNamespace(ns),
		generator.WithTemplate("cm", map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "drifted", "namespace": ns},
			"data":     map[string]any{"owner": "kro"},
		}),
	)
	g.SetUID(types.UID("uid-drift"))
	rt := compileAndBuildEnv(t, patchEnvCfg, g)

	exec := NewSimple(cl).WithConflictDetection(true)
	_, err := exec.Apply(ctx, rt, watchrouter.NoopWatcher{})
	require.NoError(t, err)

	// A foreign actor (kubectl-style) force-applies a competing value under its
	// own field manager, taking ownership of data.owner away from kro.
	drift := &unstructured.Unstructured{}
	drift.SetGroupVersionKind(configMapGVK)
	drift.SetNamespace(ns)
	drift.SetName("drifted")
	require.NoError(t, unstructured.SetNestedStringMap(drift.Object, map[string]string{"owner": "human"}, "data"))
	require.NoError(t, cl.Patch(ctx, drift, client.Apply,
		client.FieldOwner("kubectl-edit"), client.ForceOwnership))

	// kro re-applies: the foreign manager is external drift, not a peer Graph,
	// so it is reclaimed by force and converges back to the desired value.
	_, err = exec.Apply(ctx, rt, watchrouter.NoopWatcher{})
	require.NoError(t, err)
	cm := getConfigMap(t, cl, ns, "drifted")
	data, _, _ := unstructured.NestedStringMap(cm.Object, "data")
	assert.Equal(t, "kro", data["owner"], "external drift is reclaimed, not refused")
}

// TestTemplate_NodeRenameReleasesRetiredFields pins finding 3901230191: renaming
// a template node must not leave behind fields the new template no longer sets.
// With the per-Graph template manager, node "old" (applying data.a AND data.b)
// and its rename to "new" (applying only data.a) write under the SAME manager,
// so SSA narrows the manager's field set on the second apply and drops data.b
// from the live object — instead of orphaning it under a retired per-node
// manager that nothing applies under anymore. Requires envtest for real SSA
// managed-field tracking.
func TestTemplate_NodeRenameReleasesRetiredFields(t *testing.T) {
	cl := patchEnvClient(t)
	ns := "default"
	ctx := context.Background()
	const uid = "uid-rename-release"

	// First revision: node "old" applies data.a and data.b.
	gOld := generator.NewGraph("rename",
		generator.WithNamespace(ns),
		generator.WithTemplate("old", map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "renamed", "namespace": ns},
			"data":     map[string]any{"a": "from-a", "b": "from-b"},
		}),
	)
	gOld.SetUID(types.UID(uid))
	rtOld := compileAndBuildEnv(t, patchEnvCfg, gOld)

	exec := NewSimple(cl).WithConflictDetection(true)
	_, err := exec.Apply(ctx, rtOld, watchrouter.NoopWatcher{})
	require.NoError(t, err)

	cm := getConfigMap(t, cl, ns, "renamed")
	data, _, _ := unstructured.NestedStringMap(cm.Object, "data")
	require.Equal(t, "from-a", data["a"])
	require.Equal(t, "from-b", data["b"], "both fields present after the first revision")

	// Second revision: the node is RENAMED to "new" and the template now sets
	// ONLY data.a. Same Graph UID, so the same per-Graph template manager.
	gNew := generator.NewGraph("rename",
		generator.WithNamespace(ns),
		generator.WithTemplate("new", map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "renamed", "namespace": ns},
			"data":     map[string]any{"a": "from-a"},
		}),
	)
	gNew.SetUID(types.UID(uid))
	rtNew := compileAndBuildEnv(t, patchEnvCfg, gNew)

	_, err = exec.Apply(ctx, rtNew, watchrouter.NoopWatcher{})
	require.NoError(t, err)

	cm = getConfigMap(t, cl, ns, "renamed")
	data, _, _ = unstructured.NestedStringMap(cm.Object, "data")
	assert.Equal(t, "from-a", data["a"], "the retained field survives the rename")
	_, hasB := data["b"]
	assert.False(t, hasB, "the retired node's field data.b is released, not orphaned, after the rename")
}
