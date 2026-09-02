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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubernetes-sigs/kro/pkg/graphengine/testutil/generator"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/watchrouter"
)

// TestPatch_ForceReclaimsOwnStaleIdentity is the executor-side half of the
// Finding C regression: a change of a patch node's own field-manager identity
// (e.g. commit 95a2027e re-keying a subgraph patch node's manager from its
// local id to its qualified path) must NOT deadlock the node in permanent soft
// not-ready. The stale manager still owns the field the new manager wants, so
// an unforced SSA apply 409s forever. The fix force-reclaims a conflict that is
// attributable to this Graph's OWN stale patch identity (a legacy pre-segment
// manager, or a same-Graph manager under a different node) while still leaving
// a foreign or peer-Graph conflict as soft not-ready.
func TestPatch_ForceReclaimsOwnStaleIdentity(t *testing.T) {
	const uid = "uid-patch-reclaim"

	// reclaimCase seeds contendedKey under a stale manager, then applies a
	// patch node 'p' (identity patchFieldManager(uid, "p")) that contends the
	// same field, and asserts the outcome.
	reclaimCase := func(t *testing.T, targetName, staleManager string, wantReclaimed bool) {
		t.Helper()
		cl := patchEnvClient(t)
		ns := "default"

		// Seed the ConfigMap field under the stale manager.
		mustCreateConfigMap(t, cl, ns, targetName, nil)
		cm := getConfigMap(t, cl, ns, targetName)
		cm.Object["data"] = map[string]any{"contendedKey": "stale-value"}
		require.NoError(t, cl.Patch(context.Background(), cm, client.Apply, client.FieldOwner(staleManager)))

		g := generator.NewGraph("g",
			generator.WithNamespace(ns),
			generator.WithPatch("p", "v1", "ConfigMap", targetName, map[string]any{
				"data": map[string]any{"contendedKey": "patch-value"},
			}),
		)
		g.SetUID(uid)
		rt := compileAndBuildEnv(t, patchEnvCfg, g)
		res, err := NewSimple(cl).Apply(context.Background(), rt, watchrouter.NoopWatcher{})

		got := getConfigMap(t, cl, ns, targetName)
		data, _, _ := unstructured.NestedStringMap(got.Object, "data")

		if wantReclaimed {
			require.NoError(t, err, "a conflict with this Graph's own stale patch identity must be force-reclaimed, not left soft")
			assert.NotContains(t, res.Unresolved, "p", "patch node must not be Unresolved after reclaim")
			assert.Equal(t, "patch-value", data["contendedKey"], "the patch node must own the field after force-reclaim")
			require.Len(t, res.Contributions, 1)
			assert.Equal(t, patchFieldManager(g.GetUID(), "p"), res.Contributions[0].FieldManager)
			return
		}
		require.Error(t, err, "a peer-Graph / foreign conflict must stay soft not-ready")
		assert.True(t, errors.Is(err, ErrNotReady), "peer/foreign patch conflict must be soft ErrNotReady, got %v", err)
		assert.Contains(t, res.Unresolved, "p", "peer/foreign conflicting patch node must be Unresolved")
		assert.Equal(t, "stale-value", data["contendedKey"], "kro must never steal a peer/foreign field")
	}

	t.Run("legacy pre-segment manager is reclaimed", func(t *testing.T) {
		// A legacy "<prefix><hash>" manager (no per-Graph segment) was only ever
		// produced by an earlier build of kro, so it is our own stale identity.
		reclaimCase(t, "reclaim-legacy", "kro-graphengine.patch.abc123def456", true)
	})

	t.Run("same-Graph different-node manager is reclaimed", func(t *testing.T) {
		// Same Graph UID, different node id: shares self's per-Graph segment, so
		// it is a re-keyed / sibling identity of ours — reclaimable.
		reclaimCase(t, "reclaim-samegraph", patchFieldManager(uid, "p-old-identity"), true)
	})

	t.Run("peer-Graph manager stays soft not-ready", func(t *testing.T) {
		// A different Graph UID yields a different per-Graph segment: a peer
		// Graph's field, which kro must never steal.
		reclaimCase(t, "reclaim-peergraph", patchFieldManager("uid-a-different-graph", "p"), false)
	})
}

// TestPatch_ConflictDecisionScopedToConflictingFields pins finding 3901227029:
// the force-reclaim decision must consider only the owners of the fields that
// ACTUALLY conflict, not unrelated fields on the target. Here this Graph's own
// stale patch identity owns data.a (an unrelated field), while a FOREIGN
// manager owns data.b; the patch node contends only data.b. kro must NOT
// force-steal data.b just because it happens to own the unrelated data.a — the
// foreign field stays put and the node is held soft not-ready.
func TestPatch_ConflictDecisionScopedToConflictingFields(t *testing.T) {
	const uid = "uid-patch-scoped-conflict"
	cl := patchEnvClient(t)
	ns := "default"
	name := "scoped-conflict"

	mustCreateConfigMap(t, cl, ns, name, nil)

	// This Graph's OWN stale patch identity owns the unrelated field data.a.
	ownStale := patchFieldManager(uid, "p-old-identity")
	cmA := &unstructured.Unstructured{}
	cmA.SetGroupVersionKind(configMapGVK)
	cmA.SetNamespace(ns)
	cmA.SetName(name)
	cmA.Object["data"] = map[string]any{"a": "ours-old"}
	require.NoError(t, cl.Patch(context.Background(), cmA, client.Apply, client.FieldOwner(ownStale)))

	// A FOREIGN manager owns the field data.b that the patch node will contend.
	cmB := &unstructured.Unstructured{}
	cmB.SetGroupVersionKind(configMapGVK)
	cmB.SetNamespace(ns)
	cmB.SetName(name)
	cmB.Object["data"] = map[string]any{"b": "theirs"}
	require.NoError(t, cl.Patch(context.Background(), cmB, client.Apply, client.FieldOwner("some-foreign-controller")))

	// The patch node contends ONLY data.b (owned by the foreign manager).
	g := generator.NewGraph("g",
		generator.WithNamespace(ns),
		generator.WithPatch("p", "v1", "ConfigMap", name, map[string]any{
			"data": map[string]any{"b": "patch-value"},
		}),
	)
	g.SetUID(uid)
	rt := compileAndBuildEnv(t, patchEnvCfg, g)
	res, err := NewSimple(cl).Apply(context.Background(), rt, watchrouter.NoopWatcher{})

	require.Error(t, err, "a conflict on a field owned by a foreign manager must stay soft not-ready")
	assert.True(t, errors.Is(err, ErrNotReady), "foreign field conflict must be soft ErrNotReady, got %v", err)
	assert.Contains(t, res.Unresolved, "p")

	got := getConfigMap(t, cl, ns, name)
	data, _, _ := unstructured.NestedStringMap(got.Object, "data")
	assert.Equal(t, "theirs", data["b"],
		"kro must not steal a foreign owner's conflicting field just because it owns an unrelated field on the same object")
}
