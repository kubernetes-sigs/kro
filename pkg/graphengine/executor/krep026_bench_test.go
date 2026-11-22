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

// This file is the referenced benchmark for KREP-026 "Propagation Control",
// section "Collections: cost". Run it with:
//
//	go test ./pkg/graphengine/executor/ -run XXX -bench 'BenchmarkKREP026' -benchtime=5x
//
// It measures one reconcile of a 1000-item collection in the three states
// the KREP discusses, and TestKREP026GateEvaluationCounts pins the number of
// propagateWhen evaluations each state performs so the timings are known to
// measure what they claim.
//
// Measured 2026-11-11 on an Intel Xeon Platinum 8259CL, in-memory fake
// client (no apiserver round trips), N=1000:
//
//	ungated collection apply                         ~790 ms   (1000 no-op SSAs)
//	cap10, fleet converged                           ~1000 ms  (0 gate evals; pre-read + hash + no-op SSA)
//	cap10, stalled rollout (10 in flight)            ~115 ms   (1 gate eval, then first-false skip)
//	canaryEach, stalled (canary never converges)     ~750 ms   (999 gate evals; `each` disables the skip)

import (
	"context"
	"fmt"
	"testing"

	io_prometheus_client "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	expv1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/testutil/generator"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/watchrouter"
	"github.com/kubernetes-sigs/kro/pkg/metrics"
)

const krep026N = 1000

// The two gate forms the KREP contrasts.
const (
	// Cap: reads only the collection, so the first false ends the pass.
	krep026Cap10 = `${web.filter(w, w.updated() && !w.ready()).size() < 10}`
	// Canary barrier: reads `each`, so every pending item evaluates.
	krep026CanaryEach = `${each.data.tier == 'canary' || web.filter(w, w.data.tier == 'canary' && !(w.updated() && w.ready())).size() == 0}`
)

// krep026Graph renders one ConfigMap per tenant; t00 is the canary. readyWhen
// models a controller acknowledging the version it saw (data.phase == data.version).
func krep026Graph(n int, version, gate string) *expv1alpha1.Graph {
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

func gateEvals() float64 {
	m := &io_prometheus_client.Metric{}
	_ = metrics.NodeGateEvalTotal.Write(m)
	return m.Counter.GetValue()
}

// TestKREP026GateEvaluationCounts pins how many times propagateWhen runs in
// each state. These counts are what make the benchmark numbers meaningful.
func TestKREP026GateEvaluationCounts(t *testing.T) {
	const n = krep026N
	b := &testing.B{}

	t.Run("converged fleet evaluates the gate zero times", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
		seedConverged(b, cl, n, "v1")
		g := krep026Graph(n, "v1", krep026Cap10)
		before := gateEvals()
		_, err := pass(t, cl, g)
		require.NoError(t, err)
		assert.Equal(t, 0.0, gateEvals()-before, "every item is at its current render and bypasses the gate")
	})

	t.Run("stalled cap10 evaluates once then skips", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
		seedConverged(b, cl, n, "v1")
		g := krep026Graph(n, "v2", krep026Cap10)
		_, _ = pass(t, cl, g) // admits 10, never acked
		before := gateEvals()
		_, err := pass(t, cl, g)
		require.Error(t, err)
		assert.Contains(t, err.Error(), fmt.Sprintf("%d of %d items held", n-10, n))
		assert.Equal(t, 1.0, gateEvals()-before, "10 in flight bypass; first pending item is held; the rest are skipped")
	})

	t.Run("stalled canaryEach evaluates every pending item", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
		seedConverged(b, cl, n, "v1")
		g := krep026Graph(n, "v2", krep026CanaryEach)
		_, _ = pass(t, cl, g) // canary admitted, never acked
		before := gateEvals()
		_, err := pass(t, cl, g)
		require.Error(t, err)
		assert.Contains(t, err.Error(), fmt.Sprintf("%d of %d items held", n-1, n))
		assert.Equal(t, float64(n-1), gateEvals()-before, "canary bypasses (current); every standard item evaluates because the gate reads each")
	})
}

// BenchmarkKREP026 is the timing side of the table above.
func BenchmarkKREP026(b *testing.B) {
	const n = krep026N

	b.Run("ungated/apply", func(b *testing.B) {
		t := &testing.T{}
		cl := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
		g := generator.NewGraph("g", generator.WithNamespace("default"),
			generator.WithDef("src", map[string]any{"tenants": tenantNames(n)}),
			generator.WithTemplate("web", map[string]any{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]any{"name": "${'web-' + t}"},
				"data":     map[string]any{"version": "v1"},
			}, generator.ForEachDim("t", "${src.tenants}")))
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

	b.Run("cap10/converged", func(b *testing.B) {
		t := &testing.T{}
		cl := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
		seedConverged(b, cl, n, "v1")
		g := krep026Graph(n, "v1", krep026Cap10)
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

	b.Run("cap10/stalled", func(b *testing.B) {
		t := &testing.T{}
		cl := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
		seedConverged(b, cl, n, "v1")
		g := krep026Graph(n, "v2", krep026Cap10)
		_, _ = pass(t, cl, g)
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			_, _ = NewSimple(cl).Apply(context.Background(), compileAndBuild(t, g), watchrouter.NoopWatcher{})
		}
	})

	b.Run("canaryEach/stalled", func(b *testing.B) {
		t := &testing.T{}
		cl := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
		seedConverged(b, cl, n, "v1")
		g := krep026Graph(n, "v2", krep026CanaryEach)
		_, _ = pass(t, cl, g)
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			_, _ = NewSimple(cl).Apply(context.Background(), compileAndBuild(t, g), watchrouter.NoopWatcher{})
		}
	})
}
