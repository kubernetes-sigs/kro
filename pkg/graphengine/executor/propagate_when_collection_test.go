// Copyright 2025 The Kubernetes Authors.
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
	"fmt"
	"github.com/kubernetes-sigs/kro/pkg/features"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	expv1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/testutil/generator"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/watchrouter"
)

// budgetGraph renders one ConfigMap per tenant at `version`. readyWhen models
// a controller acknowledging the version it saw (data.phase == data.version);
// the test plays the controller by writing data.phase. The gate admits an item
// only while fewer than `budget` items are updated and not yet ready.
func budgetGraph(tenants []any, version string, budget int, extraGate ...string) *expv1alpha1.Graph {
	gate := []string{fmt.Sprintf(`${web.filter(w, w.updated() && !w.ready()).size() < %d}`, budget)}
	gate = append(gate, extraGate...)
	return generator.NewGraph("g",
		generator.WithNamespace("default"),
		generator.WithDef("src", map[string]any{"tenants": tenants}),
		generator.WithTemplate("web", map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "${'web-' + t}"},
			"data":     map[string]any{"version": version},
		}, generator.ForEachDim("t", "${src.tenants}")),
		generator.WithReadyWhen(`${has(each.data.phase) && each.data.phase == each.data.version}`),
		generator.WithPropagateWhen(gate...),
	)
}

func tenantNames(n int) []any {
	out := make([]any, n)
	for i := range out {
		out[i] = fmt.Sprintf("t%02d", i)
	}
	return out
}

func getBudgetCM(t *testing.T, c client.Client, name string) *unstructured.Unstructured {
	t.Helper()
	cm := &unstructured.Unstructured{}
	cm.SetGroupVersionKind(configMapGVK)
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: name}, cm))
	return cm
}

func cmVersion(t *testing.T, c client.Client, name string) string {
	t.Helper()
	v, _, _ := unstructured.NestedString(getBudgetCM(t, c, name).Object, "data", "version")
	return v
}

// ack plays the item's controller: it marks the live object as having
// processed the version it currently carries.
func ack(t *testing.T, c client.Client, name string) {
	t.Helper()
	cm := getBudgetCM(t, c, name)
	v, _, _ := unstructured.NestedString(cm.Object, "data", "version")
	patch := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": name, "namespace": "default"},
		"data":     map[string]any{"phase": v},
	}}
	require.NoError(t, c.Patch(context.Background(), patch, client.Apply, client.FieldOwner("fake-controller"), client.ForceOwnership))
}

func countVersion(t *testing.T, c client.Client, n int, version string) int {
	t.Helper()
	k := 0
	for i := 0; i < n; i++ {
		if cmVersion(t, c, fmt.Sprintf("web-t%02d", i)) == version {
			k++
		}
	}
	return k
}

// pass runs one reconcile of the graph and returns the result.
func pass(t *testing.T, cl client.Client, g *expv1alpha1.Graph) (ApplyResult, error) {
	t.Helper()
	return NewSimple(cl).Apply(context.Background(), compileAndBuild(t, g), watchrouter.NoopWatcher{})
}

// converge creates all items at v1 and acks them, so the fleet starts healthy.
func converge(t *testing.T, cl client.Client, n int, version string) {
	t.Helper()
	_, err := pass(t, cl, budgetGraph(tenantNames(n), version, n+1))
	require.Error(t, err, "fresh items are not acknowledged yet")
	assert.True(t, errors.Is(err, ErrNotReady))
	for i := 0; i < n; i++ {
		ack(t, cl, fmt.Sprintf("web-t%02d", i))
	}
	_, err = pass(t, cl, budgetGraph(tenantNames(n), version, n+1))
	require.NoError(t, err, "a healthy fleet converges with no hold")
}

// Scenario: 30 healthy tenants, bump v1→v2 with a budget of 10. Exactly 10
// move per pass as long as they are acknowledged; the node is held while any
// item is held; nothing is pruned.
func TestSimple_CollectionBudget_TenAtATime(t *testing.T) {
	t.Parallel()
	const n, budget = 30, 10
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	converge(t, cl, n, "v1")

	g := budgetGraph(tenantNames(n), "v2", budget)

	// Pass 1: first 10 admitted, 20 held, node soft not-ready.
	res, err := pass(t, cl, g)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNotReady), "held items must surface as ErrNotReady, got %v", err)
	assert.Contains(t, err.Error(), "20 of 30 items held")
	assert.True(t, errors.Is(err, ErrHeld), "held items must be distinguishable as ErrHeld")
	assert.Equal(t, budget, countVersion(t, cl, n, "v2"))
	assert.Len(t, res.Applied, n, "held items keep their identity so nothing is pruned")
	for i := 0; i < budget; i++ {
		assert.Equal(t, "v2", cmVersion(t, cl, fmt.Sprintf("web-t%02d", i)), "items are admitted in input order")
	}

	// Pass 2 without any ack: nothing more moves.
	_, err = pass(t, cl, g)
	require.Error(t, err)
	assert.Equal(t, budget, countVersion(t, cl, n, "v2"), "no ack, no new admissions")

	// Controller acks 4 → 4 slots free → 4 more admitted.
	for i := 0; i < 4; i++ {
		ack(t, cl, fmt.Sprintf("web-t%02d", i))
	}
	_, err = pass(t, cl, g)
	require.Error(t, err)
	assert.Equal(t, budget+4, countVersion(t, cl, n, "v2"))

	// Ack everything, run to completion.
	for p := 0; p < 5; p++ {
		for i := 0; i < n; i++ {
			ack(t, cl, fmt.Sprintf("web-t%02d", i))
		}
		_, err = pass(t, cl, g)
		if err == nil {
			break
		}
	}
	require.NoError(t, err, "fleet converges once every item is acknowledged")
	assert.Equal(t, n, countVersion(t, cl, n, "v2"))
}

// Scenario: bad release. The first 10 never acknowledge. The remaining 20 stay
// on v1 forever; reverting to v1 re-applies only the 10 and converges.
func TestSimple_CollectionBudget_BadReleaseStopsAtBudget(t *testing.T) {
	t.Parallel()
	const n, budget = 30, 10
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	converge(t, cl, n, "v1")

	bad := budgetGraph(tenantNames(n), "v2-bad", budget)
	for p := 0; p < 5; p++ {
		_, err := pass(t, cl, bad)
		require.Error(t, err)
	}
	assert.Equal(t, budget, countVersion(t, cl, n, "v2-bad"), "blast radius is capped at the budget")
	assert.Equal(t, n-budget, countVersion(t, cl, n, "v1"))

	// Revert. The 10 on v2-bad are the only items whose stamp differs; they
	// are not updated() so they never count against the budget and are all
	// re-applied to v1 in one pass. The controller never acknowledged v2-bad,
	// so their last ack (v1) matches again and they read ready at once. The
	// 20 untouched items are updated()&&ready() and pass through.
	good := budgetGraph(tenantNames(n), "v1", budget)
	_, err := pass(t, cl, good)
	require.NoError(t, err, "revert converges in one pass")
	assert.Equal(t, n, countVersion(t, cl, n, "v1"), "revert lands on every item in one pass")
}

// Scenario: 15 tenants are already broken (never acknowledged v1) before the
// rollout. They are not updated(), so they do not count against the budget
// and the rollout still starts.
func TestSimple_CollectionBudget_PreBrokenTenantsDoNotBlock(t *testing.T) {
	t.Parallel()
	const n, budget, broken = 30, 10, 15
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	_, err := pass(t, cl, budgetGraph(tenantNames(n), "v1", n+1))
	require.Error(t, err)
	for i := broken; i < n; i++ {
		ack(t, cl, fmt.Sprintf("web-t%02d", i)) // only the tail is healthy
	}

	g := budgetGraph(tenantNames(n), "v2", budget)
	_, err = pass(t, cl, g)
	require.Error(t, err)
	assert.Equal(t, budget, countVersion(t, cl, n, "v2"),
		"15 broken-on-old-version tenants must not consume the budget")
}

// Scenario: a fully converged fleet re-reconciles with no change. Every item
// is updated()&&ready(); nothing is held and the node is ready.
func TestSimple_CollectionBudget_ConvergedPassThrough(t *testing.T) {
	t.Parallel()
	const n = 12
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	converge(t, cl, n, "v1")

	g := budgetGraph(tenantNames(n), "v1", 3)
	res, err := pass(t, cl, g)
	require.NoError(t, err)
	assert.Len(t, res.Applied, n)
	assert.Empty(t, res.Unresolved)
}

// Scenario: canary first. A tier label on the item (via each) must go first;
// the rest wait until every canary is updated and acknowledged.
func TestSimple_CollectionBudget_CanaryFirstViaEach(t *testing.T) {
	t.Parallel()
	const n = 6
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	converge(t, cl, n, "v1")

	// t04 is the canary (chosen by name here; the gate reads it off each).
	canary := `${each.metadata.name == 'web-t04'
	   || web.exists(w, w.metadata.name == 'web-t04' && w.updated() && w.ready())}`
	g := budgetGraph(tenantNames(n), "v2", 100, canary)

	_, err := pass(t, cl, g)
	require.Error(t, err)
	assert.Equal(t, 1, countVersion(t, cl, n, "v2"))
	assert.Equal(t, "v2", cmVersion(t, cl, "web-t04"), "only the canary moved, regardless of its position")

	ack(t, cl, "web-t04")
	_, err = pass(t, cl, g)
	require.Error(t, err, "rest are in flight until acked")
	assert.Equal(t, n, countVersion(t, cl, n, "v2"), "once the canary is healthy everyone else is admitted")
}

// Scenario: a new tenant appears mid-rollout. It has no live object, so it is
// neither updated nor ready; it is created if budget allows and counts as in
// flight afterwards.
func TestSimple_CollectionBudget_NewTenantMidRollout(t *testing.T) {
	t.Parallel()
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	converge(t, cl, 5, "v1")

	// Budget 2, 6 tenants (one brand-new, sorted first alphabetically: "aaa").
	tenants := append([]any{"aaa"}, tenantNames(5)...)
	g := budgetGraph(tenants, "v2", 2)
	_, err := pass(t, cl, g)
	require.Error(t, err)
	assert.True(t, cmExists(t, cl, "web-aaa"), "new tenant is created when budget allows")
	assert.Equal(t, "v2", cmVersion(t, cl, "web-t00"))
	assert.Equal(t, "v1", cmVersion(t, cl, "web-t01"), "third item is held: aaa + t00 fill the budget")
}

// The single-node form (no self-reference) still gates the collection as one
// unit: every item or none.
func TestSimple_CollectionBudget_WholeNodeGateUnchanged(t *testing.T) {
	t.Parallel()
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	g := generator.NewGraph("g",
		generator.WithNamespace("default"),
		generator.WithDef("src", map[string]any{"tenants": tenantNames(3), "go": false}),
		generator.WithTemplate("web", map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "${'web-' + t}"},
			"data":     map[string]any{"version": "v1"},
		}, generator.ForEachDim("t", "${src.tenants}")),
		generator.WithPropagateWhen(`${src.go}`),
	)
	res, err := pass(t, cl, g)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNotReady))
	assert.Contains(t, res.Unresolved, "web")
	assert.False(t, cmExists(t, cl, "web-t00"))
}

// Self-reference is only meaningful on a collection. A singleton gating on
// itself is still rejected at compile time.
func TestCompile_PropagateWhenSelfRefRejectedOnSingleton(t *testing.T) {
	t.Parallel()
	g := generator.NewGraph("g",
		generator.WithNamespace("default"),
		generator.WithTemplate("a", map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "a"},
			"data":     map[string]any{"k": "v"},
		}),
		generator.WithPropagateWhen(`${a.data.k == "v"}`),
	)
	_, err := newCompiler(t).CompileWithOptions(g)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "references its own id")
}

// `each` in propagateWhen requires a forEach.
func TestCompile_PropagateWhenEachRejectedOnSingleton(t *testing.T) {
	t.Parallel()
	g := generator.NewGraph("g",
		generator.WithNamespace("default"),
		generator.WithTemplate("a", map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "a"},
			"data":     map[string]any{"k": "v"},
		}),
		generator.WithPropagateWhen(`${each.data.k == "v"}`),
	)
	_, err := newCompiler(t).CompileWithOptions(g)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "has no forEach")
}

// Inserting or removing a sibling shifts every item's collection-index and
// collection-size label. Those are kro bookkeeping, not desired state: the
// render hash must not change, so no item becomes !updated() and the budget
// is not consumed by a no-op rollout.
func TestSimple_CollectionBudget_SiblingInsertDoesNotInvalidateUpdated(t *testing.T) {
	t.Parallel()
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	converge(t, cl, 5, "v1")

	// Insert "aaa" (sorts first → every existing item's index shifts by one)
	// with a budget of 1. Only the new item should need admission.
	tenants := append([]any{"aaa"}, tenantNames(5)...)
	g := budgetGraph(tenants, "v1", 1)
	res, err := pass(t, cl, g)
	require.Error(t, err, "the new item is in flight until acked")
	assert.True(t, cmExists(t, cl, "web-aaa"))
	assert.Len(t, res.Applied, 6)
	assert.NotContains(t, err.Error(), "items held", "no existing item may be held: their renders did not change")

	// Remove t04 (collection-size changes for everyone) with budget 1.
	g = budgetGraph(tenantNames(4), "v1", 1)
	ack(t, cl, "web-aaa")
	_, err = pass(t, cl, budgetGraph(append([]any{"aaa"}, tenantNames(4)...), "v1", 1))
	require.NoError(t, err, "size change alone must not hold anyone")
	_ = g
}

// propagateWhen on a ref: a ref has no desired state to hold.
func TestCompile_PropagateWhenRejectedOnRef(t *testing.T) {
	t.Parallel()
	g := generator.NewGraph("g",
		generator.WithNamespace("default"),
		generator.WithDef("src", map[string]any{"ok": true}),
		generator.WithRef("live", &expv1alpha1.ExternalRef{
			APIVersion: "v1", Kind: "ConfigMap",
			Metadata: expv1alpha1.ExternalRefMetadata{Name: "live", Namespace: "default"},
		}),
		generator.WithPropagateWhen(`${src.ok}`),
	)
	_, err := newCompiler(t).CompileWithOptions(g)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not supported on ref nodes")
}

// updated()/ready() are facts about the items of a gated collection. Every
// other receiver is rejected: a single resource is always at its current
// render when read; a node's readiness is <id>.ready(), a node reference, not an item fact.
func TestCompile_ItemFactsRejectedOffCollectionItems(t *testing.T) {
	t.Parallel()
	singleton := func(id string) generator.GraphOption {
		return generator.WithTemplate(id, map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": id},
			"data":     map[string]any{"k": "v"},
		})
	}
	coll := func(id string) generator.GraphOption {
		return generator.WithTemplate(id, map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "${'" + id + "-' + t}"},
			"data":     map[string]any{"k": "v"},
		}, generator.ForEachDim("t", "${src.tenants}"))
	}
	src := generator.WithDef("src", map[string]any{"tenants": []any{"a", "b"}})

	cases := []struct {
		name string
		opts []generator.GraphOption
		want string // "" means compile must succeed
	}{
		{"singleton updated() in gate", []generator.GraphOption{src, singleton("a"), singleton("b"), generator.WithPropagateWhen(`${a.updated()}`)}, "updated() is only defined"},
		{"singleton ready() in gate is a node reference", []generator.GraphOption{src, singleton("a"), singleton("b"), generator.WithPropagateWhen(`${a.ready()}`)}, ""},
		{"own ready() in gate rejected", []generator.GraphOption{src, singleton("a"), generator.WithPropagateWhen(`${a.ready()}`)}, "can never be satisfied"},
		{"collection node ready() from a dependent", []generator.GraphOption{src, coll("web"), singleton("after"), generator.WithPropagateWhen(`${web.ready()}`)}, ""},
		{"internal map name rejected", []generator.GraphOption{src, singleton("a"), singleton("b"), generator.WithPropagateWhen(`${__kro_ready.a}`)}, "is internal"},
		{"foreign collection items in gate", []generator.GraphOption{src, coll("other"), coll("web"), generator.WithPropagateWhen(`${other.filter(o, o.ready()).size() == 0}`)}, "ready() is only defined"},
		{"foreign collection items in gate via all()", []generator.GraphOption{src, coll("other"), coll("web"), generator.WithPropagateWhen(`${other.all(o, o.updated())}`)}, "updated() is only defined"},
		{"each.updated() in readyWhen", []generator.GraphOption{src, coll("web"), generator.WithReadyWhen(`${each.updated()}`)}, "updated() is only defined"},
		{"own items in gate", []generator.GraphOption{src, coll("web"), generator.WithPropagateWhen(`${web.filter(w, w.updated() && !w.ready()).size() < 10}`)}, ""},
		{"each in gate", []generator.GraphOption{src, coll("web"), generator.WithPropagateWhen(`${each.updated() || web.exists(w, w.ready())}`)}, ""},
		{"nested comprehension over own items", []generator.GraphOption{src, coll("web"), generator.WithPropagateWhen(`${web.filter(w, w.ready()).all(x, x.updated())}`)}, ""},
		{"cel.bind over own items", []generator.GraphOption{src, coll("web"), generator.WithPropagateWhen(`${cel.bind(done, web.filter(w, w.updated() && w.ready()).size(), web.filter(w, w.updated() && !w.ready()).size() < (done == 0 ? 1 : done))}`)}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := generator.NewGraph("g", append([]generator.GraphOption{generator.WithNamespace("default")}, tc.opts...)...)
			_, err := newCompiler(t).CompileWithOptions(g)
			if tc.want == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

// With the alpha gate off the field is rejected at compile time.
func TestCompile_PropagateWhenRequiresFeatureGate(t *testing.T) {
	// Not parallel: flips the global gate.
	require.NoError(t, features.FeatureGate.Set("PropagateWhen=false"))
	t.Cleanup(func() { _ = features.FeatureGate.Set("PropagateWhen=true") })
	g := generator.NewGraph("g",
		generator.WithNamespace("default"),
		generator.WithDef("src", map[string]any{"ok": true}),
		generator.WithTemplate("a", map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "a"},
		}),
		generator.WithPropagateWhen(`${src.ok}`),
	)
	_, err := newCompiler(t).CompileWithOptions(g)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "PropagateWhen feature gate")
}

// Admission is sequential; apply is not. With a 20 ms apply latency, 40 items
// and a budget that admits everything, a serial loop takes >= 800 ms and a
// batched one at concurrency 20 takes about 40 ms. Bypassed (already-current)
// items are applied in parallel too.
func TestCollectionGatedApplyIsParallel(t *testing.T) {
	t.Parallel()
	const n = 40
	const latency = 20 * time.Millisecond
	base := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	var inflight, peak atomic.Int32
	cl := interceptor.NewClient(base, interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			cur := inflight.Add(1)
			for {
				p := peak.Load()
				if cur <= p || peak.CompareAndSwap(p, cur) {
					break
				}
			}
			time.Sleep(latency)
			inflight.Add(-1)
			return c.Patch(ctx, obj, patch, opts...)
		},
	})
	g := budgetGraph(tenantNames(n), "v1", n+1) // budget never binds

	t.Run("pending items", func(t *testing.T) {
		peak.Store(0)
		start := time.Now()
		_, err := pass(t, cl, g)
		require.Error(t, err) // not ready yet, fine
		elapsed := time.Since(start)
		assert.Less(t, elapsed, time.Duration(n)*latency/4, "serial would be >= %v", time.Duration(n)*latency)
		assert.GreaterOrEqual(t, int(peak.Load()), 10, "expected parallel applies, peak in-flight %d", peak.Load())
	})

	t.Run("already-current items", func(t *testing.T) {
		peak.Store(0)
		start := time.Now()
		_, _ = pass(t, cl, g)
		elapsed := time.Since(start)
		assert.Less(t, elapsed, time.Duration(n)*latency/4)
		assert.GreaterOrEqual(t, int(peak.Load()), 10, "bypassed items must also apply in parallel, peak %d", peak.Load())
	})
}

// An item that does not exist yet is pending (!updated() && ready()), not
// stuck: there is no object to be unready about and nothing has been
// disrupted. A "no broken items" guard therefore creates a fresh collection;
// reading absent items as !ready() held it forever.
func TestCollectionAbsentItemsArePending(t *testing.T) {
	t.Parallel()
	noPreBroken := generator.NewGraph("g",
		generator.WithNamespace("default"),
		generator.WithDef("src", map[string]any{"tenants": tenantNames(5)}),
		generator.WithTemplate("web", map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "${'web-' + t}"},
			"data":     map[string]any{"version": "v1"},
		}, generator.ForEachDim("t", "${src.tenants}")),
		generator.WithReadyWhen(`${has(each.data.phase) && each.data.phase == each.data.version}`),
		generator.WithPropagateWhen(`${web.filter(w, !w.updated() && !w.ready()).size() == 0}`),
	)
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	_, err := pass(t, cl, noPreBroken)
	require.Error(t, err) // created, not yet acked: not ready, not held
	assert.NotContains(t, err.Error(), "items held")
	cms := &unstructured.UnstructuredList{}
	cms.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMapList"})
	require.NoError(t, cl.List(context.Background(), cms))
	assert.Len(t, cms.Items, 5, "all 5 created on the first pass")

	// Once one item exists and is broken (applied, never acked), the guard
	// holds the rest of a v2 rollout: that is a real existing issue.
	v2 := generator.NewGraph("g",
		generator.WithNamespace("default"),
		generator.WithDef("src", map[string]any{"tenants": tenantNames(5)}),
		generator.WithTemplate("web", map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "${'web-' + t}"},
			"data":     map[string]any{"version": "v2"},
		}, generator.ForEachDim("t", "${src.tenants}")),
		generator.WithReadyWhen(`${has(each.data.phase) && each.data.phase == each.data.version}`),
		generator.WithPropagateWhen(`${web.filter(w, !w.updated() && !w.ready()).size() == 0}`),
	)
	_, err = pass(t, cl, v2)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "5 of 5 items held", "existing unacked v1 items are broken: guard holds")
}
