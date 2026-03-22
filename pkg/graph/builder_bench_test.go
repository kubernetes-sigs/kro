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

package graph

import (
	"fmt"
	"testing"

	memory2 "k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/restmapper"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
	"github.com/kubernetes-sigs/kro/pkg/testutil/k8s"
)

// newBenchBuilder creates a Builder with fake resolvers for benchmarking.
func newBenchBuilder(b *testing.B) *Builder {
	b.Helper()
	fakeResolver, fakeDiscovery := k8s.NewFakeResolver()
	restMapper := restmapper.NewDeferredDiscoveryRESTMapper(memory2.NewMemCacheClient(fakeDiscovery))
	return &Builder{
		schemaResolver: fakeResolver,
		restMapper:     restMapper,
	}
}

// BenchmarkNewRGD_SimplePodAndConfig benchmarks a minimal RGD: one Pod + one ConfigMap.
func BenchmarkNewRGD_SimplePodAndConfig(b *testing.B) {
	builder := newBenchBuilder(b)
	rgd := generator.NewResourceGraphDefinition("bench-simple",
		generator.WithSchema(
			"SimpleApp", "v1alpha1",
			map[string]interface{}{
				"name": "string",
			},
			map[string]interface{}{
				"configName": "${config.metadata.name}",
			},
		),
		generator.WithResource("config", map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]interface{}{
				"name":      "${schema.spec.name}-config",
				"namespace": "default",
			},
			"data": map[string]interface{}{
				"app": "${schema.spec.name}",
			},
		}, nil, nil),
		generator.WithResource("pod", map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Pod",
			"metadata": map[string]interface{}{
				"name":      "${schema.spec.name}",
				"namespace": "default",
			},
			"spec": map[string]interface{}{
				"containers": []interface{}{
					map[string]interface{}{
						"name":  "app",
						"image": "nginx",
					},
				},
			},
		}, nil, nil),
	)

	b.ResetTimer()
	for b.Loop() {
		_, err := builder.NewResourceGraphDefinition(rgd, defaultRGDConfig)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkNewRGD_ManyPods benchmarks an RGD with many similar resources to
// stress the type building path with repeated schema conversions.
func BenchmarkNewRGD_ManyPods(b *testing.B) {
	builder := newBenchBuilder(b)

	opts := []generator.ResourceGraphDefinitionOption{
		generator.WithSchema(
			"PodSet", "v1alpha1",
			map[string]interface{}{
				"name": "string",
			},
			map[string]interface{}{
				"pod00Phase": "${pod00.status.phase}",
			},
		),
	}

	// 20 pods, each referencing schema.spec.name
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("pod%02d", i)
		opts = append(opts, generator.WithResource(id, map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Pod",
			"metadata": map[string]interface{}{
				"name":      "${schema.spec.name}-" + id,
				"namespace": "default",
			},
			"spec": map[string]interface{}{
				"containers": []interface{}{
					map[string]interface{}{
						"name":  id,
						"image": "nginx",
					},
				},
			},
		}, nil, nil))
	}

	rgd := generator.NewResourceGraphDefinition("bench-many-pods", opts...)

	b.ResetTimer()
	for b.Loop() {
		_, err := builder.NewResourceGraphDefinition(rgd, defaultRGDConfig)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkNewRGD_WithCollections benchmarks a collection-based RGD
// to measure forEach + type inference overhead.
func BenchmarkNewRGD_WithCollections(b *testing.B) {
	builder := newBenchBuilder(b)
	rgd := generator.NewResourceGraphDefinition("bench-collections",
		generator.WithSchema(
			"PodPerRegion", "v1alpha1",
			map[string]interface{}{
				"name":    "string",
				"regions": "[]string",
			},
			map[string]interface{}{
				"podCount": "${string(pods.size())}",
			},
		),
		generator.WithResourceCollection("pods", map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Pod",
			"metadata": map[string]interface{}{
				"name":      "${schema.spec.name + '-' + region}",
				"namespace": "default",
			},
			"spec": map[string]interface{}{
				"containers": []interface{}{
					map[string]interface{}{
						"name":  "app",
						"image": "nginx",
					},
				},
			},
		},
			[]krov1alpha1.ForEachDimension{
				{"region": "${schema.spec.regions}"},
			},
			nil, nil),
	)

	b.ResetTimer()
	for b.Loop() {
		_, err := builder.NewResourceGraphDefinition(rgd, defaultRGDConfig)
		if err != nil {
			b.Fatal(err)
		}
	}
}
