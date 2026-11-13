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
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	expv1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/cel/library"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/compiler"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/testutil/generator"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/watchrouter"
)

// The three KREP-026 collection gate forms. Each runs once per item against
// the whole collection, so a pass is O(N^2) CEL work.
var gateForms = map[string]string{
	"cap10":       `${web.filter(w, w.updated() && !w.ready()).size() < 10}`,
	"noPreBroken": `${web.filter(w, !w.updated() && !w.ready()).size() == 0}`,
	"exponential": `${cel.bind(done, web.filter(w, w.updated() && w.ready()).size(), web.filter(w, w.updated() && !w.ready()).size() < (done == 0 ? 1 : done))}`,
	// Reads each: no early exit. Worst case is every item pending and the
	// barrier false for all of them (no canary has converged), so all N
	// evaluate and all N are held, every pass.
	"canaryEach": `${each.data.tier == 'canary' || web.filter(w, w.data.tier == 'canary' && !(w.updated() && w.ready())).size() == 0}`,
}

var benchSizes = []int{10, 100, 500, 1000}

// ackExisting plays the controller for every item that exists.
func ackExisting(b *testing.B, cl client.Client) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(configMapGVK.GroupVersion().WithKind("ConfigMapList"))
	require.NoError(b, cl.List(context.Background(), list, client.InNamespace("default")))
	for i := range list.Items {
		cm := &list.Items[i]
		v, _, _ := unstructured.NestedString(cm.Object, "data", "version")
		patch := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": cm.GetName(), "namespace": "default"},
			"data":     map[string]any{"phase": v},
		}}
		require.NoError(b, cl.Patch(context.Background(), patch, client.Apply, client.FieldOwner("fake-controller"), client.ForceOwnership))
	}
}

// seedConverged brings an N-item fleet to converged state at version through
// the per-item path (so every item carries a spec-hash stamp), using an
// unbounded cap so creation takes one wave.
func seedConverged(b *testing.B, cl client.Client, n int, version string) {
	t := &testing.T{}
	wide := gateGraph(n, version, fmt.Sprintf(`${web.filter(w, w.updated() && !w.ready()).size() < %d}`, n+1))
	_, _ = pass(t, cl, wide)
	ackExisting(b, cl)
	_, err := pass(t, cl, wide)
	require.NoError(b, err, "seed fleet must converge")
}

func gateGraph(n int, version, gate string) *expv1alpha1.Graph {
	return generator.NewGraph("g",
		generator.WithNamespace("default"),
		generator.WithDef("src", map[string]any{"tenants": tenantNames(n)}),
		generator.WithTemplate("web", map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "${'web-' + t}"},
			"data":     map[string]any{"version": version, "tier": "${t == 't00' ? 'canary' : 'standard'}"},
		}, generator.ForEachDim("t", "${src.tenants}")),
		generator.WithReadyWhen(`${has(each.data.phase) && each.data.phase == each.data.version}`),
		generator.WithPropagateWhen(gate),
	)
}

// BenchmarkGateCEL isolates the CEL cost: N gate evaluations over an N-item
// collection value, exactly the work applyCollectionPerItem does per pass,
// with no cluster I/O. Items are a steady-state mix (10 in flight, rest
// converged) so filters do real work.
func BenchmarkGateCEL(b *testing.B) {
	for name, gate := range gateForms {
		for _, n := range benchSizes {
			b.Run(fmt.Sprintf("%s/n=%d", name, n), func(b *testing.B) {
				rt := compileAndBuild(&testing.T{}, gateGraph(n, "v1", gate))
				node := rt.Nodes()[1]
				items := make([]any, n)
				for i := range items {
					obj := map[string]any{
						"apiVersion": "v1", "kind": "ConfigMap",
						"metadata": map[string]any{"name": fmt.Sprintf("web-t%02d", i), "namespace": "default"},
						"data":     map[string]any{"version": "v1", "phase": "v1"},
					}
					items[i] = itemFacts(obj, true, i >= 10) // first 10 in flight
				}
				scope := map[string]any{}
				for k, v := range node.Scope() {
					scope[k] = v
				}
				scope["web"] = items
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					for i := 0; i < n; i++ {
						scope[compiler.EachVarName] = items[i]
						if _, _, err := node.EvalHeld(scope); err != nil {
							b.Fatal(err)
						}
					}
				}
				b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(n)/1e3, "µs/item")
			})
		}
	}
}

// BenchmarkGatePass measures a full executor pass (pre-read N live objects,
// N gate evaluations, SSA for admitted items, hashing) against the in-memory
// fake client, for a converged fleet re-reconciling with no change. That is
// the steady-state cost a Graph pays on every requeue once rolled out.
func BenchmarkGatePass(b *testing.B) {
	for name, gate := range gateForms {
		for _, n := range benchSizes {
			b.Run(fmt.Sprintf("%s/n=%d", name, n), func(b *testing.B) {
				t := &testing.T{}
				cl := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
				seedConverged(b, cl, n, "v1")
				g := gateGraph(n, "v1", gate)
				_, err := pass(t, cl, g)
				require.NoError(b, err, "fleet must be converged before measuring")
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					if _, err := NewSimple(cl).Apply(context.Background(), compileAndBuild(t, g), watchrouter.NoopWatcher{}); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

// BenchmarkGatePassRollout is the worst case: the fleet is converged on v1,
// the Graph says v2, and the controller never acknowledges. After the first
// pass 10 items are in flight and every remaining item evaluates the gate
// (and is held) on every pass. N gate evaluations x N-item filters, plus N
// pre-read GETs. This is the per-requeue cost during a stalled rollout.
func BenchmarkGatePassRollout(b *testing.B) {
	for name, gate := range gateForms {
		for _, n := range benchSizes {
			b.Run(fmt.Sprintf("%s/n=%d", name, n), func(b *testing.B) {
				t := &testing.T{}
				cl := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
				seedConverged(b, cl, n, "v1")
				v2 := gateGraph(n, "v2", gate)
				_, _ = pass(t, cl, v2) // admits the first wave; never acked
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					_, err := NewSimple(cl).Apply(context.Background(), compileAndBuild(t, v2), watchrouter.NoopWatcher{})
					_ = err // cap/exponential hold most of the fleet; noPreBroken admits everything (no items are broken)
				}
			})
		}
	}
}

// BenchmarkPlainCollectionPass is the baseline: the same collection with no
// propagateWhen, applied through the existing 20-wide errgroup path.
func BenchmarkPlainCollectionPass(b *testing.B) {
	for _, n := range benchSizes {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			t := &testing.T{}
			cl := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
			g := generator.NewGraph("g",
				generator.WithNamespace("default"),
				generator.WithDef("src", map[string]any{"tenants": tenantNames(n)}),
				generator.WithTemplate("web", map[string]any{
					"apiVersion": "v1", "kind": "ConfigMap",
					"metadata": map[string]any{"name": "${'web-' + t}"},
					"data":     map[string]any{"version": "v1"},
				}, generator.ForEachDim("t", "${src.tenants}")),
			)
			_, err := pass(t, cl, g)
			require.NoError(b, err)
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if _, err := NewSimple(cl).Apply(context.Background(), compileAndBuild(t, g), watchrouter.NoopWatcher{}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

var _ = library.ItemReadyKey
