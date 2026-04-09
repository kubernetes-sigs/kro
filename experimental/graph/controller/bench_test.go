package graphcontroller

import (
	"fmt"
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// ---------------------------------------------------------------------------
// Unit tests
// ---------------------------------------------------------------------------

// TestGVKToGVR documents the pluralization contract: these are the GVK→GVR
// mappings the controller depends on being correct. If the pluralization
// library changes or is swapped, this test catches regressions.
func TestGVKToGVR(t *testing.T) {
	tests := []struct {
		kind       string
		wantPlural string
	}{
		// Regular +s
		{"Deployment", "deployments"},
		{"Service", "services"},
		{"ConfigMap", "configmaps"},
		{"Secret", "secrets"},
		{"Namespace", "namespaces"},
		{"Node", "nodes"},
		{"EndpointSlice", "endpointslices"},

		// -y → -ies
		{"NetworkPolicy", "networkpolicies"},

		// -ss → -sses (double-s)
		{"IngressClass", "ingressclasses"},
		{"StorageClass", "storageclasses"},

		// -s → -ses / -ss → -sses
		{"Ingress", "ingresses"},

		// Already plural-looking
		{"Endpoints", "endpoints"},
	}

	for _, tt := range tests {
		t.Run(tt.kind, func(t *testing.T) {
			gvk := schema.GroupVersionKind{Group: "test", Version: "v1", Kind: tt.kind}
			gvr := gvkToGVR(gvk)
			if gvr.Resource != tt.wantPlural {
				t.Errorf("gvkToGVR(%s): got %q, want %q", tt.kind, gvr.Resource, tt.wantPlural)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Benchmarks — baseline measurements for the hot-path operations.
//
// These measure the pure-compute cost of the reconcile loop's critical
// sections. API server round-trips are excluded — those are dominated by
// network latency and are measured in the integration tests. The benchmarks
// here capture the CPU-bound work that scales with DAG size and expression
// count.
//
// Run: go test ./experimental/graph/controller -bench=. -benchmem
// ---------------------------------------------------------------------------

// BenchmarkCompileGraph measures CEL environment creation + expression
// compilation for graphs of increasing size. This runs once per spec change
// and is cached thereafter.
func BenchmarkCompileGraph(b *testing.B) {
	for _, nodeCount := range []int{1, 5, 10, 25, 50} {
		b.Run(fmt.Sprintf("nodes=%d", nodeCount), func(b *testing.B) {
			spec := buildBenchSpec(nodeCount)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, err := compileGraph(spec, 1)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkBuildDAG measures DAG construction (Kahn's algorithm + level
// computation) for linear dependency chains.
func BenchmarkBuildDAG(b *testing.B) {
	for _, nodeCount := range []int{1, 5, 10, 25, 50, 100} {
		b.Run(fmt.Sprintf("nodes=%d", nodeCount), func(b *testing.B) {
			nodes := buildBenchNodes(nodeCount)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, err := BuildDAG(nodes)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkHashDesiredState measures the FNV-64a hashing used for
// content-addressed apply. This runs once per node per reconcile.
func BenchmarkHashDesiredState(b *testing.B) {
	for _, fieldCount := range []int{5, 20, 50, 100} {
		b.Run(fmt.Sprintf("fields=%d", fieldCount), func(b *testing.B) {
			obj := buildBenchObject(fieldCount)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, err := hashDesiredState(obj)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkTemplateEvaluation measures CEL expression evaluation through
// the evaluator. This is the per-node cost during the DAG walk.
func BenchmarkTemplateEvaluation(b *testing.B) {
	for _, exprCount := range []int{1, 5, 10, 20} {
		b.Run(fmt.Sprintf("exprs=%d", exprCount), func(b *testing.B) {
			spec := buildBenchSpecWithExprs(exprCount)
			cache, err := compileGraph(spec, 1)
			if err != nil {
				b.Fatal(err)
			}
			eval := newEvaluator(cache)
			// Populate scope with a source node matching the spec's source data.
			sourceData := make(map[string]any, exprCount)
			for i := 0; i < exprCount; i++ {
				sourceData[fmt.Sprintf("key%d", i)] = fmt.Sprintf("value%d", i)
			}
			eval.scope["source"] = map[string]any{
				"metadata": map[string]any{
					"name":      "test",
					"namespace": "default",
					"uid":       "abc-123",
				},
				"data": sourceData,
			}
			tmpl := spec.Nodes[1].Template
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, err := eval.toMap(tmpl)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkNormalizeTypes measures the type normalization pass that converts
// JSON float64 to int64 for CEL compatibility.
func BenchmarkNormalizeTypes(b *testing.B) {
	obj := map[string]any{
		"metadata": map[string]any{
			"name":              "test",
			"namespace":         "default",
			"resourceVersion":   "12345",
			"generation":        float64(3),
			"creationTimestamp": "2024-01-01T00:00:00Z",
		},
		"spec": map[string]any{
			"replicas": float64(3),
			"selector": map[string]any{
				"matchLabels": map[string]any{
					"app": "test",
				},
			},
			"template": map[string]any{
				"spec": map[string]any{
					"containers": []any{
						map[string]any{
							"name":  "app",
							"image": "nginx:1.25",
							"ports": []any{
								map[string]any{
									"containerPort": float64(80),
								},
							},
						},
					},
				},
			},
		},
		"status": map[string]any{
			"replicas":          float64(3),
			"availableReplicas": float64(3),
			"updatedReplicas":   float64(3),
		},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = normalizeTypes(obj)
	}
}

// BenchmarkExtractReferencedIDs measures dependency extraction from CEL
// expressions. This runs once per node during DAG construction.
func BenchmarkExtractReferencedIDs(b *testing.B) {
	node := Node{
		ID: "service",
		Template: map[string]any{
			"apiVersion": "v1",
			"kind":       "Service",
			"metadata": map[string]any{
				"name": "${deploy.metadata.name}-svc",
			},
			"spec": map[string]any{
				"selector": "${deploy.spec.selector.matchLabels}",
				"ports": []any{
					map[string]any{
						"port":       "${config.data.port}",
						"targetPort": "${deploy.spec.template.spec.containers[0].ports[0].containerPort}",
					},
				},
			},
		},
		ReadyWhen: []string{
			"${service.spec.clusterIP != ''}",
		},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = extractReferencedIDs(node)
	}
}

// ---------------------------------------------------------------------------
// Benchmark data builders
// ---------------------------------------------------------------------------

// buildBenchSpec creates a GraphSpec with N nodes in a linear chain.
// Node 0 is a standalone ConfigMap; nodes 1..N-1 each reference the previous.
func buildBenchSpec(n int) *GraphSpec {
	nodes := buildBenchNodes(n)
	return &GraphSpec{Nodes: nodes}
}

// buildBenchNodes creates N nodes forming a linear dependency chain.
func buildBenchNodes(n int) []Node {
	nodes := make([]Node, n)
	nodes[0] = Node{
		ID: "node0",
		Template: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name": "bench-0",
			},
			"data": map[string]any{
				"key": "value",
			},
		},
	}
	for i := 1; i < n; i++ {
		prev := fmt.Sprintf("node%d", i-1)
		nodes[i] = Node{
			ID: fmt.Sprintf("node%d", i),
			Template: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name": fmt.Sprintf("bench-%d", i),
				},
				"data": map[string]any{
					"upstream": fmt.Sprintf("${%s.data.key}", prev),
				},
			},
		}
	}
	return nodes
}

// buildBenchObject creates a map with the given number of data fields.
func buildBenchObject(fieldCount int) map[string]any {
	data := make(map[string]any, fieldCount)
	for i := 0; i < fieldCount; i++ {
		data[fmt.Sprintf("key%d", i)] = fmt.Sprintf("value%d", i)
	}
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":      "bench-hash",
			"namespace": "default",
		},
		"data": data,
	}
}

// buildBenchSpecWithExprs creates a 2-node spec where node 1 has N
// CEL expressions referencing node 0.
func buildBenchSpecWithExprs(exprCount int) *GraphSpec {
	// Source data must have at least exprCount keys.
	sourceData := make(map[string]any, exprCount)
	for i := 0; i < exprCount; i++ {
		sourceData[fmt.Sprintf("key%d", i)] = fmt.Sprintf("v%d", i)
	}

	targetData := make(map[string]any, exprCount)
	for i := 0; i < exprCount; i++ {
		targetData[fmt.Sprintf("field%d", i)] = fmt.Sprintf("${source.data.key%d}", i)
	}

	return &GraphSpec{
		Nodes: []Node{
			{
				ID: "source",
				Template: map[string]any{
					"apiVersion": "v1",
					"kind":       "ConfigMap",
					"metadata": map[string]any{
						"name": "bench-source",
					},
					"data": sourceData,
				},
			},
			{
				ID:       "target",
				Template: targetData,
			},
		},
	}
}
