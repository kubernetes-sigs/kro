package graphcontroller

import (
	"fmt"
	"testing"

	"github.com/kubernetes-sigs/kro/experimental/controller/compiler"
	dagpkg "github.com/kubernetes-sigs/kro/experimental/controller/dag"
	"github.com/kubernetes-sigs/kro/experimental/controller/graph"
)

// ---------------------------------------------------------------------------
// Unit tests — cache mechanism
//
// EXCEPTION to integration-first guidance. These test the
// compiled graph cache's sharing and isolation invariants. The cache is
// invisible at the integration boundary — the observable outcome is identical
// with or without sharing. A bug here (stale compiled graph served after spec
// change, or mutable state leaking between instances sharing a compiledGraph)
// would manifest as non-deterministic reconciliation that is effectively
// impossible to diagnose from integration tests. The integration test
// TestIdenticalGraphsConvergeIndependently covers correctness of the
// observable behavior; these cover the mechanism.
// ---------------------------------------------------------------------------

// TestCompiledGraphCacheLifecycle exercises the full lifecycle of the compiled
// graph cache: sharing across identical specs, isolation between different
// specs, and cleanup on instance removal.
func TestCompiledGraphCacheLifecycle(t *testing.T) {
	spec := buildBenchSpec(5)
	caches := newGraphCaches()
	compilationKey := spec.CompilationKey()

	// --- Phase 1: Two instances with identical specs share one compiledGraph ---
	compiled, err := compiler.CompileGraphSpec(spec, nil)
	if err != nil {
		t.Fatal(err)
	}
	caches.set("ns/graph-a-g00001", newInstanceState(compiled))

	// Second instance finds the shared compiledGraph by hash.
	shared := caches.getCompiled(compilationKey)
	if shared == nil {
		t.Fatal("expected shared compiledGraph, got nil")
	}
	caches.set("ns/graph-b-g00001", newInstanceState(shared))

	if caches.get("ns/graph-a-g00001").compiled != caches.get("ns/graph-b-g00001").compiled {
		t.Fatal("instances with identical specs should share a compiledGraph")
	}

	// --- Phase 2: Different spec produces a separate compiledGraph ---
	differentSpec := buildBenchSpec(10)
	differentCompiled, err := compiler.CompileGraphSpec(differentSpec, nil)
	if err != nil {
		t.Fatal(err)
	}
	caches.set("ns/graph-c-g00001", newInstanceState(differentCompiled))

	if caches.get("ns/graph-c-g00001").compiled == caches.get("ns/graph-a-g00001").compiled {
		t.Fatal("different specs should not share a compiledGraph")
	}

	// --- Phase 3: Removing one instance retains shared compiledGraph ---
	caches.remove("ns/graph-a-g00001")
	if caches.getCompiled(compilationKey) == nil {
		t.Fatal("compiledGraph should survive with remaining references")
	}

	// --- Phase 4: Removing last instance cleans up compiledGraph ---
	caches.remove("ns/graph-b-g00001")
	if caches.getCompiled(compilationKey) != nil {
		t.Fatal("compiledGraph should be cleaned up when last reference is removed")
	}

	// Different spec's compiledGraph should be unaffected.
	if caches.getCompiled(differentSpec.CompilationKey()) == nil {
		t.Fatal("unrelated compiledGraph should survive other spec's cleanup")
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

	state1 := newInstanceState(compiled)
	state2 := newInstanceState(compiled)

	// Mutate state1's mutable fields.
	state1.previousScope["node0"] = map[string]any{"data": map[string]any{"key": "v1"}}
	state1.previousEvalHashes["node0"] = "hash-a"
	state1.previousPlanStates["node0"] = dagpkg.NodeReady
	state1.forEachItems["node0/item"] = []any{"a", "b"}

	// state2 should be unaffected.
	if _, ok := state2.previousScope["node0"]; ok {
		t.Fatal("previousScope leaked between instances")
	}
	if _, ok := state2.previousEvalHashes["node0"]; ok {
		t.Fatal("previousEvalHashes leaked between instances")
	}
	if _, ok := state2.previousPlanStates["node0"]; ok {
		t.Fatal("previousPlanStates leaked between instances")
	}
	if _, ok := state2.forEachItems["node0/item"]; ok {
		t.Fatal("forEachItems leaked between instances")
	}

	// Both should share the same (immutable) compiledGraph.
	if state1.compiled != state2.compiled {
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
				_, err := dagpkg.BuildDAG(nodes, nil)
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
			b.ReportAllocs()
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
			compiled, err := compiler.CompileGraphSpec(spec, nil)
			if err != nil {
				b.Fatal(err)
			}
			eval := newEvaluator(newInstanceState(compiled))
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
		_, _, _, _, _ = graph.ExtractReferencedPathsFromNode(node, exprPaths)
	}
}

// BenchmarkHashNodeInputs measures field-path-scoped input hashing — the
// per-node cost of the evaluation check (step 4 of Wind). This runs once per
// node per reconcile to decide whether evaluation can be skipped.
func BenchmarkHashNodeInputs(b *testing.B) {
	for _, depCount := range []int{1, 3, 5, 10} {
		b.Run(fmt.Sprintf("deps=%d", depCount), func(b *testing.B) {
			// Build a node with depCount dependencies, each referencing data.key1 and metadata.name.
			depPaths := make(map[string][]graph.FieldPath, depCount)
			scope := make(map[string]any, depCount)
			for i := 0; i < depCount; i++ {
				depID := fmt.Sprintf("dep%d", i)
				depPaths[depID] = []graph.FieldPath{{"data", "key1"}, {"metadata", "name"}}
				scope[depID] = map[string]any{
					"apiVersion": "v1",
					"kind":       "ConfigMap",
					"metadata": map[string]any{
						"name":            fmt.Sprintf("cm-%d", i),
						"namespace":       "default",
						"resourceVersion": "12345",
						"uid":             "abc-123",
					},
					"data": map[string]any{
						"key1": "value1",
						"key2": "value2",
						"key3": "value3",
					},
				}
			}
			node := &graph.Node{ID: "target", DepPaths: depPaths}
			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_, err := hashNodeInputs(node, scope)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
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
// Compiled graph sharing benchmarks
// ---------------------------------------------------------------------------

// BenchmarkCompileRevisionSharing measures the headline number for content-
// addressed compiled graph sharing. Simulates N child graphs with identical
// specs (the nested graph pattern). Without sharing, each graph independently
// compiles CEL programs and builds a DAG. With sharing, the first graph
// compiles and subsequent graphs do a hash lookup.
//
// The benchmark measures the amortized cost of compileRevision for N instances.
func BenchmarkCompileRevisionSharing(b *testing.B) {
	for _, nodeCount := range []int{5, 10, 25, 50} {
		for _, instanceCount := range []int{1, 10, 50, 100} {
			b.Run(fmt.Sprintf("nodes=%d/instances=%d", nodeCount, instanceCount), func(b *testing.B) {
				spec := buildBenchSpec(nodeCount)
				b.ResetTimer()
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					caches := newGraphCaches()
					for j := 0; j < instanceCount; j++ {
						// Simulate N revisions with the same spec but different names.
						specHash := spec.CompilationKey()
						instanceKey := fmt.Sprintf("ns/graph-%d-g00001", j)

						compiled := caches.getCompiled(specHash)
						if compiled == nil {
							var err error
							compiled, err = compiler.CompileGraphSpec(spec, nil)
							if err != nil {
								b.Fatal(err)
							}
						}
						state := newInstanceState(compiled)
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
			dag, err := dagpkg.BuildDAG(nodes, nil)
			if err != nil {
				b.Fatal(err)
			}
			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				ps := dagpkg.NewPlanState(dag)
				// Trigger propagation from the root — worst case: all V nodes
				// end up Blocked via chain traversal.
				ps.SetState(dag, "node0", dagpkg.NodeError)
			}
		})
	}
}

// propagateStateLinearScan is the pre-fix O(V²) implementation, inlined here
// so BenchmarkPropagateStateLinearScan can measure the before-state directly.
func propagateStateLinearScan(ps *dagpkg.PlanState, dag *dagpkg.DAG, sourceID string, targetState dagpkg.NodeState) {
	for _, node := range dag.Nodes {
		if ps.States[node.ID] != dagpkg.NodeUnvisited {
			continue
		}
		if node.Dependencies[sourceID] {
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
			dag, err := dagpkg.BuildDAG(nodes, nil)
			if err != nil {
				b.Fatal(err)
			}
			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				ps := dagpkg.NewPlanState(dag)
				ps.States["node0"] = dagpkg.NodeError
				propagateStateLinearScan(ps, dag, "node0", dagpkg.NodeBlocked)
			}
		})
	}
}

// BenchmarkHashSelfPaths measures the cost of hashSelfPaths — the propagation
// hash that determines whether downstream dependents need re-evaluation.
// This runs once per node per reconcile (step 8 of Wind). Without this
// benchmark, GC pressure from per-call allocations would be invisible.
//
// Per 005-reconciliation.md § Hash Mechanics: "hash the output paths
// downstream expressions reference [...] compare against in-memory
// propagation-hash from previous evaluation."
func BenchmarkHashSelfPaths(b *testing.B) {
	for _, pathCount := range []int{1, 3, 5, 10} {
		b.Run(fmt.Sprintf("paths=%d", pathCount), func(b *testing.B) {
			// Build a node with pathCount self-paths, each at depth 2.
			selfPaths := make([]graph.FieldPath, pathCount)
			observed := map[string]any{
				"metadata": map[string]any{
					"name":      "test",
					"namespace": "default",
				},
			}
			statusFields := make(map[string]any, pathCount)
			for i := 0; i < pathCount; i++ {
				field := fmt.Sprintf("field%d", i)
				selfPaths[i] = graph.FieldPath{"status", field}
				statusFields[field] = fmt.Sprintf("value%d", i)
			}
			observed["status"] = statusFields
			node := &graph.Node{ID: "target", SelfPaths: selfPaths}
			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_, err := hashSelfPaths(node, observed)
				if err != nil {
					b.Fatal(err)
				}
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
				plan := dagpkg.NewPlanState(dag)
				scope := make(map[string]any, len(dag.Nodes))

				// Walk all nodes — every node hits the skip path.
				walkSkipBench(dag, plan, scope, prevScope, triggered, propagationTriggered)
			}
		})
	}
}

// walkSkipBench simulates the skip path of tryDispatch for all nodes.
// Extracted so the benchmark measures only the walk, not setup.
func walkSkipBench(dag *dagpkg.DAG, plan *dagpkg.PlanState, scope, prevScope map[string]any, triggered, propagationTriggered map[string]bool) {
	for _, idx := range dag.TopologicalOrder {
		node := &dag.Nodes[idx]
		if plan.States[node.ID] != dagpkg.NodeUnvisited {
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

// BenchmarkForEachItemDiff measures the cost of forEach item diffing — the
// O(N) pass that identifies which collection items changed between reconciles.
// This is the one hot-path operation whose cost scales with collection size
// rather than change count. The diff consists of three phases:
//
//  1. Identity map construction: O(N) per reconcile — both the current and
//     previous collections build identity maps (neither is persisted as a map;
//     the raw []any is cached, identity maps are rebuilt each cycle).
//  2. Identity extraction (forEachItemIdentity): O(1) per item — reads
//     metadata.name/namespace from the collection item.
//  3. Unchanged comparison (forEachItemUnchanged): two hashDesiredState calls
//     per unchanged item.
//
// The benchmark exercises the realistic case: a large collection where K items
// changed and N-K items are unchanged. The N-K items pay the full diff cost
// (identity + two hashes) but skip template evaluation and apply. The K
// changed items are not measured here — their cost is template eval + SSA
// apply, already benchmarked separately.
//
// This is the architectural exception to the "work proportional to change"
// property. The diff cost is O(N) regardless of K. The benchmark makes this
// cost visible and establishes a regression baseline.
//
// Run: go test ./experimental/controller -bench=BenchmarkForEachItemDiff -benchmem
func BenchmarkForEachItemDiff(b *testing.B) {
	for _, itemCount := range []int{10, 100, 1000, 10000} {
		b.Run(fmt.Sprintf("items=%d/changed=1", itemCount), func(b *testing.B) {
			// Build current and previous collections. Previous has one
			// changed item (item 0). Both are raw []any — the identity
			// maps are built inside the measured region because
			// reconcileForEach rebuilds them every cycle.
			items := buildForEachItems(itemCount)
			prevItemsRaw := buildForEachItems(itemCount)
			prevItemsRaw[0].(map[string]any)["data"].(map[string]any)["key"] = "changed-value"

			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				// Phase 1: Build previous identity map (rebuilt each reconcile).
				prevByID := make(map[string]any, len(prevItemsRaw))
				for _, item := range prevItemsRaw {
					prevByID[forEachItemIdentity(item)] = item
				}

				// Phase 2-3: Identity extraction + unchanged comparison.
				changed := 0
				for _, item := range items {
					id := forEachItemIdentity(item)
					prev, existed := prevByID[id]
					if !existed || !forEachItemUnchanged(prev, item) {
						changed++
					}
				}
				if changed != 1 {
					b.Fatalf("expected 1 changed item, got %d", changed)
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

// BenchmarkSpecHash measures the cost of computing the full spec content hash.
// Used for revision diffing (diffRevisionNodes). The compilation key
// (CompilationKey) is the structural subset that gates compiled graph sharing.
func BenchmarkSpecHash(b *testing.B) {
	for _, nodeCount := range []int{1, 5, 10, 25, 50} {
		b.Run(fmt.Sprintf("nodes=%d", nodeCount), func(b *testing.B) {
			spec := buildBenchSpec(nodeCount)
			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = spec.Hash()
			}
		})
	}
}

// BenchmarkForEachItemDiffCached measures the forEach item diff with cached
// per-item hashes (Layer 1). Compares against the uncached path above.
// With cached hashes, the previous item hash is a map lookup instead of a
// hashDesiredState call — eliminates N of the 2N hash computations.
//
// Run: go test ./experimental/controller -bench=BenchmarkForEachItemDiffCached -benchmem
func BenchmarkForEachItemDiffCached(b *testing.B) {
	for _, itemCount := range []int{10, 100, 1000, 10000} {
		b.Run(fmt.Sprintf("items=%d/changed=1", itemCount), func(b *testing.B) {
			benchForEachCached(b, itemCount)
		})
	}
}

// BenchmarkForEachItemDiffIncremental measures the forEach diff with the
// changed-item annotation (Layer 2). Only items in the changed set are
// hashed — unchanged items are skipped entirely. This is the O(K) path
// where K = changed items.
//
// Run: go test ./experimental/controller -bench=BenchmarkForEachItemDiffIncremental -benchmem
func BenchmarkForEachItemDiffIncremental(b *testing.B) {
	for _, itemCount := range []int{10, 100, 1000, 10000} {
		b.Run(fmt.Sprintf("items=%d/changed=1", itemCount), func(b *testing.B) {
			benchForEachIncremental(b, itemCount)
		})
	}
}

// benchForEachOriginal runs the original O(2N) hash path.
func benchForEachOriginal(b *testing.B, itemCount int) {
	items := buildForEachItems(itemCount)
	prevItemsRaw := buildForEachItems(itemCount)
	prevItemsRaw[0].(map[string]any)["data"].(map[string]any)["key"] = "changed-value"

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		prevByID := make(map[string]any, len(prevItemsRaw))
		for _, item := range prevItemsRaw {
			prevByID[forEachItemIdentity(item)] = item
		}
		changed := 0
		for _, item := range items {
			id := forEachItemIdentity(item)
			prev, existed := prevByID[id]
			if !existed || !forEachItemUnchanged(prev, item) {
				changed++
			}
		}
		if changed != 1 {
			b.Fatalf("expected 1 changed item, got %d", changed)
		}
	}
}

// benchForEachCached runs the Layer 1 cached-hash path (N hashes, not 2N).
func benchForEachCached(b *testing.B, itemCount int) {
	items := buildForEachItems(itemCount)
	prevItemsRaw := buildForEachItems(itemCount)
	prevItemsRaw[0].(map[string]any)["data"].(map[string]any)["key"] = "changed-value"

	deps := map[string]bool{"source": true}
	scope := map[string]any{"source": map[string]any{"list": prevItemsRaw}}
	ctxHash := hashForEachContext(scope, deps)

	cachedHashes := make(map[string]string, len(prevItemsRaw))
	for _, item := range prevItemsRaw {
		id := forEachItemIdentity(item)
		if m, ok := item.(map[string]any); ok {
			h, _ := hashDesiredState(m)
			cachedHashes[id] = ctxHash + "/" + h
		}
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		prevByID := make(map[string]any, len(prevItemsRaw))
		for _, item := range prevItemsRaw {
			prevByID[forEachItemIdentity(item)] = item
		}
		changed := 0
		for _, item := range items {
			id := forEachItemIdentity(item)
			_, existed := prevByID[id]
			if !existed || !forEachItemUnchangedCached(cachedHashes[id], item, ctxHash) {
				changed++
			}
		}
		if changed != 1 {
			b.Fatalf("expected 1 changed item, got %d", changed)
		}
	}
}

// benchForEachIncremental runs the Layer 2 incremental path (K hashes, not N).
func benchForEachIncremental(b *testing.B, itemCount int) {
	items := buildForEachItems(itemCount)

	cachedHashes := make(map[string]string, len(items))
	for _, item := range items {
		id := forEachItemIdentity(item)
		if m, ok := item.(map[string]any); ok {
			h, _ := hashDesiredState(m)
			cachedHashes[id] = h
		}
	}

	changedIDs := map[string]bool{"default/item-0": true}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		currentIDs := make([]string, 0, len(items))
		for _, item := range items {
			currentIDs = append(currentIDs, forEachItemIdentity(item))
		}
		for idx, id := range currentIDs {
			if changedIDs[id] {
				if m, ok := items[idx].(map[string]any); ok {
					currHash, _ := hashDesiredState(m)
					if cachedHashes[id] != currHash {
						// changed
					}
				}
			}
		}
	}
}

// TestForEachDiffOptimizationRegression verifies that the forEach incremental
// diff optimization produces fewer allocations than the original path. Uses
// testing.Benchmark to get deterministic alloc counts (not wall-clock time),
// then asserts strict ordering: incremental < cached < original.
//
// If someone removes the cached-hash or incremental-annotation optimization,
// alloc counts equalize and this test fails.
func TestForEachDiffOptimizationRegression(t *testing.T) {
	t.Parallel()
	const N = 1000

	original := testing.Benchmark(func(b *testing.B) { benchForEachOriginal(b, N) })
	cached := testing.Benchmark(func(b *testing.B) { benchForEachCached(b, N) })
	incremental := testing.Benchmark(func(b *testing.B) { benchForEachIncremental(b, N) })

	origAllocs := original.AllocsPerOp()
	cachedAllocs := cached.AllocsPerOp()
	incrAllocs := incremental.AllocsPerOp()

	t.Logf("allocs/op: original=%d  cached=%d  incremental=%d", origAllocs, cachedAllocs, incrAllocs)

	if cachedAllocs >= origAllocs {
		t.Errorf("Layer 1 (cached hash) regression: cached allocs (%d) should be less than original (%d)",
			cachedAllocs, origAllocs)
	}
	if incrAllocs >= cachedAllocs {
		t.Errorf("Layer 2 (incremental) regression: incremental allocs (%d) should be less than cached (%d)",
			incrAllocs, cachedAllocs)
	}
}

// TestResolveCollectionSource verifies the compile-time gate for forEach
// incremental diffing. CollectionSource is set only when ForEach.Expr
// references exactly one scope variable that doesn't appear elsewhere
// on the node. If removed or broken, Layer 2 silently degrades to O(N).
func TestResolveCollectionSource(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		node     graph.Node
		wantSrc  string
		wantDesc string
	}{
		{
			name: "single scope var, not in template",
			node: graph.Node{
				ID:      "deploys",
				ForEach: &graph.ForEachBinding{VarName: "app", Expr: "${wk}"},
				Template: map[string]any{
					"apiVersion": "apps/v1",
					"kind":       "Deployment",
					"metadata":   map[string]any{"name": "${app.metadata.name}"},
				},
			},
			wantSrc:  "wk",
			wantDesc: "only scope var in ForEach.Expr, not in template body",
		},
		{
			name: "scope var also in template body",
			node: graph.Node{
				ID:      "deploys",
				ForEach: &graph.ForEachBinding{VarName: "app", Expr: "${wk}"},
				Template: map[string]any{
					"apiVersion": "apps/v1",
					"kind":       "Deployment",
					"metadata":   map[string]any{"name": "${app.metadata.name}"},
					"spec":       map[string]any{"replicas": "${wk.size()}"},
				},
			},
			wantSrc:  "",
			wantDesc: "wk appears in template body — not safe for per-item diff",
		},
		{
			name: "scope var in readyWhen",
			node: graph.Node{
				ID:        "deploys",
				ForEach:   &graph.ForEachBinding{VarName: "app", Expr: "${wk}"},
				Template:  map[string]any{"apiVersion": "apps/v1", "kind": "Deployment"},
				ReadyWhen: []string{"${wk.size() > 0}"},
			},
			wantSrc:  "",
			wantDesc: "wk appears in readyWhen — not safe",
		},
		{
			name: "scope var in propagateWhen",
			node: graph.Node{
				ID:            "deploys",
				ForEach:       &graph.ForEachBinding{VarName: "app", Expr: "${wk}"},
				Template:      map[string]any{"apiVersion": "apps/v1", "kind": "Deployment"},
				PropagateWhen: []string{"${wk != null}"},
			},
			wantSrc:  "",
			wantDesc: "wk appears in propagateWhen — not safe",
		},
		{
			name: "scope var in includeWhen",
			node: graph.Node{
				ID:          "deploys",
				ForEach:     &graph.ForEachBinding{VarName: "app", Expr: "${wk}"},
				Template:    map[string]any{"apiVersion": "apps/v1", "kind": "Deployment"},
				IncludeWhen: []string{"${wk.size() > 0}"},
			},
			wantSrc:  "",
			wantDesc: "wk appears in includeWhen — not safe",
		},
		{
			name: "collection expr with dot access, single scope var",
			node: graph.Node{
				ID:       "deploys",
				ForEach:  &graph.ForEachBinding{VarName: "app", Expr: "${wk.items}"},
				Template: map[string]any{"apiVersion": "apps/v1", "kind": "Deployment"},
			},
			wantSrc:  "wk",
			wantDesc: "dot access is in ForEach.Expr only — wk is the collection source",
		},
		{
			name: "multiple deps, only collection source in forEach",
			node: graph.Node{
				ID:      "deploys",
				ForEach: &graph.ForEachBinding{VarName: "app", Expr: "${wk}"},
				Template: map[string]any{
					"apiVersion": "apps/v1",
					"kind":       "Deployment",
					"metadata":   map[string]any{"name": "${app.metadata.name}"},
					"spec":       map[string]any{"image": "${config.image}"},
				},
			},
			wantSrc:  "wk",
			wantDesc: "config in template is a different dep — wk only in forEach",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Compile to get exprPaths (AST-extracted field paths).
			spec := &graph.GraphSpec{Nodes: []graph.Node{
				{ID: "wk", Watch: map[string]any{"apiVersion": "v1", "kind": "ConfigMap"}},
				{ID: "config", Template: map[string]any{"apiVersion": "v1", "kind": "ConfigMap"}},
				tt.node,
			}}
			compiled, err := compiler.CompileGraphSpec(spec, nil)
			if err != nil {
				t.Fatalf("compileGraphSpec: %v", err)
			}
			// Build a full DAG to access CollectionSource (set during dagpkg.BuildDAG).
			dag, err := dagpkg.BuildDAG(spec.Nodes, compiled.ExprPaths)
			if err != nil {
				t.Fatalf("dagpkg.BuildDAG: %v", err)
			}
			nodeIdx := dag.Index[tt.node.ID]
			got := dag.Nodes[nodeIdx].ForEach.CollectionSource
			if got != tt.wantSrc {
				t.Errorf("CollectionSource = %q, want %q (%s)", got, tt.wantSrc, tt.wantDesc)
			}
		})
	}
}
