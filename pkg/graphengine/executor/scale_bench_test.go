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
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	memory "k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/restmapper"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	apimachineryruntime "k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	expv1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/compiler"
	krotruntime "github.com/kubernetes-sigs/kro/pkg/graphengine/runtime"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/testutil/generator"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/watchrouter"
	testk8s "github.com/kubernetes-sigs/kro/pkg/testutil/k8s"
)

// benchLatencyClient simulates an API server with a controlled request latency (e.g. 1ms round-trip)
// to accurately measure how bounded parallelism scales with API round-trips.
type benchLatencyClient struct {
	client.Client
	latency    time.Duration
	patchCount atomic.Int64
	getCount   atomic.Int64
}

func (c *benchLatencyClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	c.patchCount.Add(1)
	if c.latency > 0 {
		time.Sleep(c.latency)
	}
	return c.Client.Patch(ctx, obj, patch, opts...)
}

func (c *benchLatencyClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	c.getCount.Add(1)
	if c.latency > 0 {
		time.Sleep(c.latency)
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

func benchScheme() *apimachineryruntime.Scheme {
	s := apimachineryruntime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = expv1alpha1.AddToScheme(s)
	return s
}

func benchCompileAndBuild(b *testing.B, g *expv1alpha1.Graph) *krotruntime.Runtime {
	fakeResolver, disco := testk8s.NewFakeResolver()
	rm := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(disco))
	p, err := compiler.NewCompilerWithDependencies(fakeResolver, rm).Compile(g)
	if err != nil && b != nil {
		b.Fatalf("compile failed: %v", err)
	}
	return krotruntime.New(p, g)
}

// largeBenchGraph constructs a collection Graph with N items.
func largeBenchGraph(count int) *expv1alpha1.Graph {
	items := make([]any, count)
	for i := 0; i < count; i++ {
		items[i] = fmt.Sprintf("item-%04d", i)
	}
	return generator.NewGraph("bench-graph",
		generator.WithNamespace("default"),
		generator.WithDef("src", map[string]any{"items": items}),
		generator.WithTemplate("cm", map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "${'cm-' + n}"},
			"data":     map[string]any{"value": "${n}"},
		}, generator.ForEachDim("n", "${src.items}")),
	)
}

// BenchmarkExecutor_ApplyConcurrencyTuning measures the collection apply throughput
// across different collection sizes (100, 500, 1000 items) and concurrency settings
// (1 = serial, 5, 10, 20 = default, 50).
func BenchmarkExecutor_ApplyConcurrencyTuning(b *testing.B) {
	sizes := []int{50, 200, 500}
	concurrencies := []int{1, 5, 10, 20, 50}

	for _, size := range sizes {
		g := largeBenchGraph(size)
		rt := benchCompileAndBuild(b, g)

		for _, concurrency := range concurrencies {
			b.Run(fmt.Sprintf("Items=%d/Concurrency=%d", size, concurrency), func(b *testing.B) {
				b.ReportAllocs()
				b.ResetTimer()

				for i := 0; i < b.N; i++ {
					baseClient := fake.NewClientBuilder().WithScheme(benchScheme()).Build()
					cl := &benchLatencyClient{
						Client:  baseClient,
						latency: 100 * time.Microsecond, // 100µs simulated API latency
					}

					ex := NewSimple(cl)
					ex.ApplyConcurrency = concurrency

					res, err := ex.Apply(context.Background(), rt, watchrouter.NoopWatcher{})
					if err != nil {
						b.Fatalf("apply failed: %v", err)
					}
					if len(res.Applied) != size {
						b.Fatalf("applied count=%d, want %d", len(res.Applied), size)
					}
				}
			})
		}
	}
}

// BenchmarkExecutor_ParallelReconcilesManyInstances measures overall executor throughput
// when multiple controller workers apply instances in parallel.
func BenchmarkExecutor_ParallelReconcilesManyInstances(b *testing.B) {
	g := largeBenchGraph(20)
	fakeResolver, disco := testk8s.NewFakeResolver()
	rm := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(disco))
	prog, err := compiler.NewCompilerWithDependencies(fakeResolver, rm).Compile(g)
	if err != nil {
		b.Fatalf("compile failed: %v", err)
	}

	b.ReportAllocs()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		baseClient := fake.NewClientBuilder().WithScheme(benchScheme()).Build()
		cl := &benchLatencyClient{
			Client:  baseClient,
			latency: 50 * time.Microsecond,
		}
		ex := NewSimple(cl)
		ex.ApplyConcurrency = 10

		for pb.Next() {
			// In production, each instance reconcile constructs its own single-use
			// Runtime from the shared compiled Program.
			rt := krotruntime.New(prog, g)
			_, err := ex.Apply(context.Background(), rt, watchrouter.NoopWatcher{})
			if err != nil {
				b.Fatalf("apply failed: %v", err)
			}
		}
	})
}
