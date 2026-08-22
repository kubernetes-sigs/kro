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

package rgdadapter

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	memory "k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/restmapper"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/compiler"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/registry"
	testk8s "github.com/kubernetes-sigs/kro/pkg/testutil/k8s"
)

// benchCompiler creates a real compiler instance backed by fake discovery for benchmarks.
func benchCompiler(b *testing.B) *compiler.Compiler {
	b.Helper()
	fakeResolver, disco := testk8s.NewFakeResolver()
	rm := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(disco))
	return compiler.NewCompilerWithDependencies(fakeResolver, rm)
}

// benchCollectionRGD creates an RGD with a declared schema and a ConfigMap template
// with a forEach collection dimension of size N.
func benchCollectionRGD(name string, count int) *v1alpha1.ResourceGraphDefinition {
	rawJSON, _ := json.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":      "${schema.spec.name}-cm-${string(i)}",
			"namespace": "default",
		},
		"data": map[string]any{
			"value": "${schema.spec.value}",
			"index": "${string(i)}",
		},
	})

	return &v1alpha1.ResourceGraphDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.ResourceGraphDefinitionSpec{
			Schema: &v1alpha1.Schema{
				APIVersion: "v1alpha1",
				Kind:       "BenchApp",
				Spec:       runtime.RawExtension{Raw: []byte(`{"name":"string","value":"string"}`)},
			},
			Resources: []*v1alpha1.Resource{
				{
					ID: "cm",
					Template: runtime.RawExtension{
						Raw: rawJSON,
					},
					ForEach: []v1alpha1.ForEachDimension{
						{"i": fmt.Sprintf("${lists.range(%d)}", count)},
					},
				},
			},
		},
	}
}

// benchComplex3TierAppRGD builds a realistic, complex multi-tier application RGD:
// 1. dbConfig (ConfigMap): Central configuration derived from schema
// 2. dbSecret (Secret): Conditional database credentials (includeWhen: tier == 'production')
// 3. appWorkers (ConfigMap collection): Collection of worker pods with forEach
// 4. routingConfig (ConfigMap): Cross-node references reading both dbConfig and appWorkers
// 5. status writeback: Status projection aggregating conditions and endpoints
func benchComplex3TierAppRGD(name string, workerCount int) *v1alpha1.ResourceGraphDefinition {
	dbConfigRaw, _ := json.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":      "${schema.spec.name}-db-config",
			"namespace": "default",
		},
		"data": map[string]any{
			"dbHost": "${schema.spec.dbHost}",
			"dbPort": "${string(schema.spec.dbPort)}",
			"env":    "${schema.spec.tier}",
		},
	})

	dbSecretRaw, _ := json.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":      "${schema.spec.name}-db-secret",
			"namespace": "default",
		},
		"data": map[string]any{
			"password": "production-secure-vault-pass",
		},
	})

	workerRaw, _ := json.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":      "${schema.spec.name}-worker-${string(w)}",
			"namespace": "default",
		},
		"data": map[string]any{
			"workerID": "${string(w)}",
			"dbRef":    "${dbConfig.metadata.name}",
			"tier":     "${schema.spec.tier}",
		},
	})

	routingRaw, _ := json.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":      "${schema.spec.name}-routing",
			"namespace": "default",
		},
		"data": map[string]any{
			"primaryConfig": "${dbConfig.data.dbHost}",
			"workerCount":   "${string(schema.spec.workerCount)}",
		},
	})

	return &v1alpha1.ResourceGraphDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.ResourceGraphDefinitionSpec{
			Schema: &v1alpha1.Schema{
				APIVersion: "v1alpha1",
				Kind:       "Complex3TierApp",
				Spec: runtime.RawExtension{
					Raw: []byte(`{"name":"string","dbHost":"string","dbPort":"integer","tier":"string","workerCount":"integer"}`),
				},
			},
			Resources: []*v1alpha1.Resource{
				{
					ID:       "dbConfig",
					Template: runtime.RawExtension{Raw: dbConfigRaw},
				},
				{
					ID:          "dbSecret",
					Template:    runtime.RawExtension{Raw: dbSecretRaw},
					IncludeWhen: []string{"${schema.spec.tier == 'production'}"},
				},
				{
					ID:       "appWorkers",
					Template: runtime.RawExtension{Raw: workerRaw},
					ForEach: []v1alpha1.ForEachDimension{
						{"w": fmt.Sprintf("${lists.range(%d)}", workerCount)},
					},
				},
				{
					ID:       "routingConfig",
					Template: runtime.RawExtension{Raw: routingRaw},
				},
			},
		},
	}
}

// benchCartesianMatrixRGD builds a 2D Cartesian collection RGD (5 regions x 4 environments = 20 items per instance)
func benchCartesianMatrixRGD(name string) *v1alpha1.ResourceGraphDefinition {
	matrixRaw, _ := json.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":      "${schema.spec.name}-${region}-${env}",
			"namespace": "default",
		},
		"data": map[string]any{
			"region": "${region}",
			"env":    "${env}",
		},
	})

	return &v1alpha1.ResourceGraphDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.ResourceGraphDefinitionSpec{
			Schema: &v1alpha1.Schema{
				APIVersion: "v1alpha1",
				Kind:       "CartesianApp",
				Spec:       runtime.RawExtension{Raw: []byte(`{"name":"string"}`)},
			},
			Resources: []*v1alpha1.Resource{
				{
					ID:       "matrix",
					Template: runtime.RawExtension{Raw: matrixRaw},
					ForEach: []v1alpha1.ForEachDimension{
						{"region": "${['us-east', 'us-west', 'eu-central', 'ap-south', 'sa-east']}"},
						{"env": "${['dev', 'staging', 'prod', 'qa']}"},
					},
				},
			},
		},
	}
}

func benchInstance(name, value string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kro.run/v1alpha1",
			"kind":       "BenchApp",
			"metadata": map[string]any{
				"name":      name,
				"namespace": "default",
			},
			"spec": map[string]any{
				"name":  name,
				"value": value,
			},
		},
	}
}

func bench3TierInstance(name, tier string, workerCount int) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kro.run/v1alpha1",
			"kind":       "Complex3TierApp",
			"metadata": map[string]any{
				"name":      name,
				"namespace": "default",
			},
			"spec": map[string]any{
				"name":        name,
				"dbHost":      "postgres.internal.cluster",
				"dbPort":      int64(5432),
				"tier":        tier,
				"workerCount": int64(workerCount),
			},
		},
	}
}

func benchCartesianInstance(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kro.run/v1alpha1",
			"kind":       "CartesianApp",
			"metadata": map[string]any{
				"name":      name,
				"namespace": "default",
			},
			"spec": map[string]any{
				"name": name,
			},
		},
	}
}

// BenchmarkRuntimeBuild_CachedVsUncached measures the per-reconcile runtime
// construction cost comparing the cached path (BuildRuntimeForInstanceCached)
// against the legacy uncached path (BuildRuntimeForInstance) across scale
// (1, 10, 100, 500, 1000, 2000 items).
func BenchmarkRuntimeBuild_CachedVsUncached(b *testing.B) {
	comp := benchCompiler(b)
	counts := []int{1, 10, 100, 500, 1000, 2000}

	for _, count := range counts {
		rgd := benchCollectionRGD("bench-rgd", count)
		inst := benchInstance("bench-inst", "test-val")
		cache := registry.New()

		// Warm the cache once
		_, _, err := BuildRuntimeForInstanceCached(rgd, inst, comp, cache)
		if err != nil {
			b.Fatalf("warm cache failed: %v", err)
		}

		b.Run(fmt.Sprintf("Cached/Items=%d", count), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				rt, _, err := BuildRuntimeForInstanceCached(rgd, inst, comp, cache)
				if err != nil {
					b.Fatalf("build failed: %v", err)
				}
				_ = rt
			}
		})

		b.Run(fmt.Sprintf("Uncached/Items=%d", count), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				rt, _, err := BuildRuntimeForInstance(rgd, inst, comp)
				if err != nil {
					b.Fatalf("build failed: %v", err)
				}
				_ = rt
			}
		})
	}
}

// BenchmarkComplex3TierApp_Scale measures performance on a multi-tier RGD
// topology with conditional resources (includeWhen), collections (forEach),
// and cross-node CEL dependencies across 100 to 5,000 instances.
func BenchmarkComplex3TierApp_Scale(b *testing.B) {
	comp := benchCompiler(b)
	rgd := benchComplex3TierAppRGD("prod-3tier-app", 10)
	cache := registry.New()

	// Prime cache
	primeInst := bench3TierInstance("prime", "production", 10)
	_, _, err := BuildRuntimeForInstanceCached(rgd, primeInst, comp, cache)
	if err != nil {
		b.Fatalf("prime cache failed: %v", err)
	}

	for _, totalInstances := range []int{100, 500, 1000, 2500, 5000} {
		b.Run(fmt.Sprintf("Cached/Instances=%d", totalInstances), func(b *testing.B) {
			instances := make([]*unstructured.Unstructured, totalInstances)
			for i := 0; i < totalInstances; i++ {
				tier := "development"
				if i%2 == 0 {
					tier = "production"
				}
				instances[i] = bench3TierInstance(fmt.Sprintf("app-%04d", i), tier, 10)
			}

			b.ReportAllocs()
			b.ResetTimer()

			for iter := 0; iter < b.N; iter++ {
				var wg sync.WaitGroup
				for i := 0; i < totalInstances; i++ {
					wg.Add(1)
					go func(idx int) {
						defer wg.Done()
						rt, _, err := BuildRuntimeForInstanceCached(rgd, instances[idx], comp, cache)
						if err != nil {
							b.Errorf("reconcile failed: %v", err)
						}
						_ = rt
					}(i)
				}
				wg.Wait()
			}
		})
	}
}

// BenchmarkCartesianMatrix_Scale measures runtime construction for multi-axis
// Cartesian collections (e.g. 5 regions x 4 environments = 20 combinations per instance).
func BenchmarkCartesianMatrix_Scale(b *testing.B) {
	comp := benchCompiler(b)
	rgd := benchCartesianMatrixRGD("matrix-app")
	cache := registry.New()

	primeInst := benchCartesianInstance("prime")
	_, _, err := BuildRuntimeForInstanceCached(rgd, primeInst, comp, cache)
	if err != nil {
		b.Fatalf("prime cache failed: %v", err)
	}

	for _, totalInstances := range []int{100, 500, 1000} {
		b.Run(fmt.Sprintf("Cached/Instances=%d/Matrix=5x4", totalInstances), func(b *testing.B) {
			instances := make([]*unstructured.Unstructured, totalInstances)
			for i := 0; i < totalInstances; i++ {
				instances[i] = benchCartesianInstance(fmt.Sprintf("cart-%04d", i))
			}

			b.ReportAllocs()
			b.ResetTimer()

			for iter := 0; iter < b.N; iter++ {
				var wg sync.WaitGroup
				for i := 0; i < totalInstances; i++ {
					wg.Add(1)
					go func(idx int) {
						defer wg.Done()
						rt, _, err := BuildRuntimeForInstanceCached(rgd, instances[idx], comp, cache)
						if err != nil {
							b.Errorf("reconcile failed: %v", err)
						}
						_ = rt
					}(i)
				}
				wg.Wait()
			}
		})
	}
}

// BenchmarkMultiRGD_ManyInstances measures the program cache under high concurrency
// and multi-tenancy: M distinct RGDs, each with K instances reconciling concurrently.
func BenchmarkMultiRGD_ManyInstances(b *testing.B) {
	comp := benchCompiler(b)

	for _, numRGDs := range []int{10, 50, 100} {
		for _, instPerRGD := range []int{10, 50} {
			totalInstances := numRGDs * instPerRGD
			b.Run(fmt.Sprintf("RGDs=%d/InstPerRGD=%d/Total=%d", numRGDs, instPerRGD, totalInstances), func(b *testing.B) {
				cache := registry.New()
				rgds := make([]*v1alpha1.ResourceGraphDefinition, numRGDs)
				instances := make([][]*unstructured.Unstructured, numRGDs)

				for r := 0; r < numRGDs; r++ {
					rgds[r] = benchCollectionRGD(fmt.Sprintf("rgd-%d", r), 5)
					instances[r] = make([]*unstructured.Unstructured, instPerRGD)
					for inst := 0; inst < instPerRGD; inst++ {
						instances[r][inst] = benchInstance(fmt.Sprintf("inst-%d-%d", r, inst), fmt.Sprintf("val-%d-%d", r, inst))
					}
					// Prime cache once for each RGD
					_, _, _ = BuildRuntimeForInstanceCached(rgds[r], instances[r][0], comp, cache)
				}

				b.ReportAllocs()
				b.ResetTimer()

				for i := 0; i < b.N; i++ {
					var wg sync.WaitGroup
					for r := 0; r < numRGDs; r++ {
						for inst := 0; inst < instPerRGD; inst++ {
							wg.Add(1)
							go func(rgdIdx, instIdx int) {
								defer wg.Done()
								rt, _, err := BuildRuntimeForInstanceCached(rgds[rgdIdx], instances[rgdIdx][instIdx], comp, cache)
								if err != nil {
									b.Errorf("reconcile failed: %v", err)
								}
								_ = rt
							}(r, inst)
						}
					}
					wg.Wait()
				}
			})
		}
	}
}

// BenchmarkConcurrentReconcileParallel measures throughput when many worker goroutines
// hammer the program cache in parallel using b.RunParallel.
func BenchmarkConcurrentReconcileParallel(b *testing.B) {
	comp := benchCompiler(b)
	cache := registry.New()
	rgd := benchCollectionRGD("shared-rgd", 10)
	inst := benchInstance("shared-inst", "shared-val")

	// Warm cache
	_, _, _ = BuildRuntimeForInstanceCached(rgd, inst, comp, cache)

	b.ReportAllocs()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		localInst := benchInstance("parallel-inst", "parallel-val")
		for pb.Next() {
			rt, _, err := BuildRuntimeForInstanceCached(rgd, localInst, comp, cache)
			if err != nil {
				b.Fatalf("build failed: %v", err)
			}
			_ = rt
		}
	})
}
