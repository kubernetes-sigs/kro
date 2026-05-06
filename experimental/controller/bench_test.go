package graphcontroller

import (
	"fmt"
	"testing"

	"github.com/ellistarn/kro/experimental/controller/compiler"
	dagpkg "github.com/ellistarn/kro/experimental/controller/dag"
	"github.com/ellistarn/kro/experimental/controller/graph"
)

// ---------------------------------------------------------------------------
// Unit tests — cache mechanism
// ---------------------------------------------------------------------------

// TestInstanceCacheLifecycle exercises the per-instance cache lifecycle:
// set, get, remove, and size tracking.
func TestInstanceCacheLifecycle(t *testing.T) {
	spec := buildBenchSpec(5)
	caches := newInstanceMap()

	compiled, err := compiler.CompileGraphSpec(spec, nil)
	if err != nil {
		t.Fatal(err)
	}
	caches.set("ns/graph-a-g00001", newInstanceState(compiled, nil))
	caches.set("ns/graph-b-g00001", newInstanceState(compiled, nil))

	if caches.CacheSizes() != 2 {
		t.Fatalf("expected 2 instances, got %d", caches.CacheSizes())
	}

	caches.remove("ns/graph-a-g00001")
	if caches.CacheSizes() != 1 {
		t.Fatalf("expected 1 instance after remove, got %d", caches.CacheSizes())
	}
	if caches.get("ns/graph-a-g00001") != nil {
		t.Fatal("removed instance should be nil")
	}
	if caches.get("ns/graph-b-g00001") == nil {
		t.Fatal("remaining instance should still exist")
	}

	caches.remove("ns/graph-b-g00001")
	if caches.CacheSizes() != 0 {
		t.Fatalf("expected 0 instances after all removed, got %d", caches.CacheSizes())
	}
}

// TestInstanceStateIsolation verifies that mutable state changes on one
// instance don't affect another instance sharing the same compiledGraph.
// This catches bugs where newInstanceState accidentally shares maps.
func TestInstanceStateIsolation(t *testing.T) {
	spec := buildBenchSpec(5)
	compiled, err := compiler.CompileGraphSpec(spec, nil)
	if err != nil {
		t.Fatal(err)
	}

	state1 := newInstanceState(compiled, nil)
	state2 := newInstanceState(compiled, nil)

	// Both should share the same (immutable) compiledGraph.
	if state1.compilation.compiled != state2.compilation.compiled {
		t.Fatal("instances should share the same compiledGraph pointer")
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
// Run: go test ./experimental/controller -bench=. -benchmem
// ---------------------------------------------------------------------------

// BenchmarkCompileGraph measures CEL environment creation + expression
// compilation for graphs of increasing size. This runs once per spec change
// and is cached thereafter.
func BenchmarkCompileGraph(b *testing.B) {
	for _, nodeCount := range []int{1, 5, 10, 25, 50} {
		b.Run(fmt.Sprintf("nodes=%d", nodeCount), func(b *testing.B) {
			spec := buildBenchSpec(nodeCount)
			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_, err := compiler.CompileGraphSpec(spec, nil)
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
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_, err := dagpkg.BuildDAG(nodes, nil, nil)
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
			compiled, err := compiler.CompileGraphSpec(spec, nil)
			if err != nil {
				b.Fatal(err)
			}
			eval := newEvaluator(newInstanceState(compiled, nil))
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
			b.ReportAllocs()
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
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = graph.NormalizeTypes(obj)
	}
}

// BenchmarkExtractReferencedPaths measures field-path extraction from
// pre-extracted CEL AST paths. This runs once per node during DAG construction.
func BenchmarkExtractReferencedPaths(b *testing.B) {
	node := graph.Node{
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

	// Simulate pre-extracted paths (as would come from compileGraphSpec).
	exprPaths := map[string]map[string][]graph.FieldPath{
		"deploy.metadata.name":             {"deploy": {{"metadata", "name"}}},
		"deploy.spec.selector.matchLabels": {"deploy": {{"spec", "selector", "matchLabels"}}},
		"config.data.port":                 {"config": {{"data", "port"}}},
		"deploy.spec.template.spec.containers[0].ports[0].containerPort": {"deploy": {{"spec", "template"}}},
		"service.spec.clusterIP != ''":                                   {"service": {{"spec", "clusterIP"}}},
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _, _, _ = graph.ExtractReferencedPathsFromNode(node, exprPaths, nil)
	}
}

// ---------------------------------------------------------------------------
// Benchmark data builders
// ---------------------------------------------------------------------------

// buildBenchSpec creates a graph.GraphSpec with N nodes in a linear chain.
// graph.Node 0 is a standalone ConfigMap; nodes 1..N-1 each reference the previous.
func buildBenchSpec(n int) *graph.GraphSpec {
	nodes := buildBenchNodes(n)
	return &graph.GraphSpec{Nodes: nodes}
}

// buildBenchNodes creates N nodes forming a linear dependency chain.
func buildBenchNodes(n int) []graph.Node {
	nodes := make([]graph.Node, n)
	nodes[0] = graph.Node{
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
		nodes[i] = graph.Node{
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

// buildBenchSpecWithExprs creates a 2-node spec where node 1 has N
// CEL expressions referencing node 0.
func buildBenchSpecWithExprs(exprCount int) *graph.GraphSpec {
	// Source data must have at least exprCount keys.
	sourceData := make(map[string]any, exprCount)
	for i := 0; i < exprCount; i++ {
		sourceData[fmt.Sprintf("key%d", i)] = fmt.Sprintf("v%d", i)
	}

	targetData := make(map[string]any, exprCount)
	for i := 0; i < exprCount; i++ {
		targetData[fmt.Sprintf("field%d", i)] = fmt.Sprintf("${source.data.key%d}", i)
	}

	return &graph.GraphSpec{
		Nodes: []graph.Node{
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

// ---------------------------------------------------------------------------
// Instance cache benchmarks
// ---------------------------------------------------------------------------

// BenchmarkCompileRevisionPerInstance measures per-instance compilation cost.
// Each instance compiles independently (no sharing layer).
func BenchmarkCompileRevisionPerInstance(b *testing.B) {
	for _, nodeCount := range []int{5, 10, 25, 50} {
		for _, instanceCount := range []int{1, 10, 50, 100} {
			b.Run(fmt.Sprintf("nodes=%d/instances=%d", nodeCount, instanceCount), func(b *testing.B) {
				spec := buildBenchSpec(nodeCount)
				b.ResetTimer()
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					caches := newInstanceMap()
					for j := 0; j < instanceCount; j++ {
						instanceKey := fmt.Sprintf("ns/graph-%d-g00001", j)
						compiled, err := compiler.CompileGraphSpec(spec, nil)
						if err != nil {
							b.Fatal(err)
						}
						state := newInstanceState(compiled, nil)
						caches.set(instanceKey, state)
					}
				}
			})
		}
	}
}

// BenchmarkPropagateState measures the cost of propagateState — the function
// that marks all downstream dependents of an erroring node with dagpkg.NodeBlocked.
//
// This is the fix benchmark. The old implementation was O(V²): a linear scan
// over all nodes at every recursive call level, producing O(V) work per level
// and O(V) levels for a chain graph, so O(V²) total. The fix uses the
// Dependents reverse adjacency list for O(V+E) traversal.
//
// The chain topology (A→B→C→…) is the worst case for the old implementation
// because every recursive level scans all V nodes to find the single next
// dependent. With the fix, each call walks only the single outgoing edge.
//
// Run: go test ./experimental/controller -bench=BenchmarkPropagateState -benchmem
func BenchmarkPropagateState(b *testing.B) {
	for _, nodeCount := range []int{10, 100, 1000} {
		b.Run(fmt.Sprintf("nodes=%d", nodeCount), func(b *testing.B) {
			nodes := buildBenchNodes(nodeCount)
			dag, err := dagpkg.BuildDAG(nodes, nil, nil)
			if err != nil {
				b.Fatal(err)
			}
			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				ps := NewPlanState(dag)
				// Trigger propagation from the root — worst case: all V nodes
				// end up Blocked via chain traversal.
				ps.SetState("node0", NodeError)
			}
		})
	}
}

// propagateStateLinearScan is the pre-fix O(V²) implementation, inlined here
// so BenchmarkPropagateStateLinearScan can measure the before-state directly.
func propagateStateLinearScan(ps *PlanState, dag *dagpkg.DAG, sourceID string, targetState NodeState) {
	for _, node := range dag.Nodes {
		if ps.States[node.ID] != NodeUnvisited {
			continue
		}
		if _, ok := node.Dependencies[sourceID]; ok {
			ps.States[node.ID] = targetState
			propagateStateLinearScan(ps, dag, node.ID, targetState)
		}
	}
}

// BenchmarkPropagateStateLinearScan is the reference baseline — the O(V²)
// linear-scan algorithm that BenchmarkPropagateState replaces. Compare the
// two to see the improvement: at 1000 nodes the linear scan is ~V/2 times
// slower because each of the V recursive calls scans all V nodes, while the
// fix walks only the single outgoing edge per call.
func BenchmarkPropagateStateLinearScan(b *testing.B) {
	for _, nodeCount := range []int{10, 100, 1000} {
		b.Run(fmt.Sprintf("nodes=%d", nodeCount), func(b *testing.B) {
			nodes := buildBenchNodes(nodeCount)
			dag, err := dagpkg.BuildDAG(nodes, nil, nil)
			if err != nil {
				b.Fatal(err)
			}
			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				ps := NewPlanState(dag)
				ps.States["node0"] = NodeError
				propagateStateLinearScan(ps, dag, "node0", NodeBlocked)
			}
		})
	}
}

// BenchmarkWalkSkip measures the cost of the O(1) skip check per untriggered
// node during the DAG walk. The design commits: "Otherwise — skip. O(1) per
// skipped node." (005-reconciliation.md § Reconcile). This benchmark compiles
// a large DAG, builds the walk infrastructure, and measures the amortized
// per-node cost of restoring previous scope + dispatching dependents.
//
// The benchmark exercises the skip path only (no trigger, no propagation) —
// the hot path during steady-state reconciliation where no events have fired.
//
// The 9 constant allocs per iteration are map creation for dagpkg.NewPlanState (States)
// and the scope map — pure setup, not walk-path overhead.
func BenchmarkWalkSkip(b *testing.B) {
	for _, nodeCount := range []int{10, 50, 100, 500} {
		b.Run(fmt.Sprintf("nodes=%d", nodeCount), func(b *testing.B) {
			spec := buildBenchSpec(nodeCount)
			compiled, err := compiler.CompileGraphSpec(spec, nil)
			if err != nil {
				b.Fatal(err)
			}
			dag := dagpkg.AssembleDAG(spec.Nodes, compiled.Topology)

			// Pre-build steady-state data once — not part of the measured path.
			prevScope := make(map[string]any, len(dag.Nodes))
			for _, node := range dag.Nodes {
				prevScope[node.ID] = map[string]any{
					"metadata": map[string]any{"name": node.ID},
					"data":     map[string]any{"key": "value"},
				}
			}
			triggered := map[string]bool{}            // nothing triggered
			propagationTriggered := map[string]bool{} // no propagation

			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				// Allocate only the per-walk state (plan + scope map).
				plan := NewPlanState(dag)
				scope := make(map[string]any, len(dag.Nodes))

				// Walk all nodes — every node hits the skip path.
				walkSkipBench(dag, plan, scope, prevScope, triggered, propagationTriggered)
			}
		})
	}
}

// walkSkipBench simulates the skip path of tryDispatch for all nodes.
// Extracted so the benchmark measures only the walk, not setup.
func walkSkipBench(dag *dagpkg.DAG, plan *PlanState, scope, prevScope map[string]any, triggered, propagationTriggered map[string]bool) {
	for _, idx := range dag.TopologicalOrder {
		node := &dag.Nodes[idx]
		if plan.States[node.ID] != NodeUnvisited {
			continue
		}
		// This is the skip check from tryDispatch step 1:
		// no trigger + no propagation + has previous scope → skip.
		if !triggered[node.ID] && !propagationTriggered[node.ID] {
			if prev, ok := prevScope[node.ID]; ok {
				scope[node.ID] = prev
			}
			continue
		}
	}
}

// ---------------------------------------------------------------------------
// forEach item diffing benchmarks
// ---------------------------------------------------------------------------

// BenchmarkForEachItemDiff measures the cost of forEach item identity
// extraction — the remaining per-item cost now that hash-based skip
// detection has been removed. Every item is re-evaluated every cycle.
//
// Run: go test ./experimental/controller -bench=BenchmarkForEachItemDiff -benchmem
func BenchmarkForEachItemDiff(b *testing.B) {
	for _, itemCount := range []int{10, 100, 1000, 10000} {
		b.Run(fmt.Sprintf("items=%d", itemCount), func(b *testing.B) {
			items := buildForEachItems(itemCount)

			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				for _, item := range items {
					_ = forEachItemIdentity(item)
				}
			}
		})
	}
}

// buildForEachItems creates N ConfigMap-like items for forEach benchmarks.
// Each item has metadata (name, namespace, uid, resourceVersion) and a data
// section — representative of a typical Watch collection driving forEach.
func buildForEachItems(n int) []any {
	items := make([]any, n)
	for i := 0; i < n; i++ {
		items[i] = map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":            fmt.Sprintf("item-%d", i),
				"namespace":       "default",
				"uid":             fmt.Sprintf("uid-%d", i),
				"resourceVersion": "12345",
			},
			"data": map[string]any{
				"key": fmt.Sprintf("value-%d", i),
			},
		}
	}
	return items
}
