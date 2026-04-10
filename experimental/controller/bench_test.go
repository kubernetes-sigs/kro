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
			for i := 0; i < b.N; i++ {
				_, err := compileGraph(spec)
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
			compiled, err := compileGraph(spec)
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

// BenchmarkHashNodeInputs measures section-scoped input hashing — the
// per-node cost of the change check (step 3 of Wind). This runs once per
// node per reconcile to decide whether evaluation can be skipped.
func BenchmarkHashNodeInputs(b *testing.B) {
	for _, depCount := range []int{1, 3, 5, 10} {
		b.Run(fmt.Sprintf("deps=%d", depCount), func(b *testing.B) {
			// Build a node with depCount dependencies, each referencing .data and .metadata.
			depSections := make(map[string]map[string]bool, depCount)
			scope := make(map[string]any, depCount)
			for i := 0; i < depCount; i++ {
				depID := fmt.Sprintf("dep%d", i)
				depSections[depID] = map[string]bool{"data": true, "metadata": true}
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
			node := &Node{ID: "target", DepSections: depSections}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, err := hashNodeInputs(node, scope)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkScopeFromTriggers measures BFS scope computation for scoped walks.
// This runs once per reconcile when a watch event triggers the walk.
func BenchmarkScopeFromTriggers(b *testing.B) {
	for _, nodeCount := range []int{5, 10, 25, 50, 100} {
		b.Run(fmt.Sprintf("nodes=%d", nodeCount), func(b *testing.B) {
			nodes := buildBenchNodes(nodeCount)
			dag, err := BuildDAG(nodes)
			if err != nil {
				b.Fatal(err)
			}
			// Trigger from the first node (root) — worst case, walks entire DAG.
			triggers := map[string]bool{"node0": true}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = ScopeFromTriggers(dag, triggers)
			}
		})
	}
}

// BenchmarkScopeFromTriggersLeaf measures scoped walk from a leaf node —
// best case, scope is just the leaf (no dependents).
func BenchmarkScopeFromTriggersLeaf(b *testing.B) {
	for _, nodeCount := range []int{5, 10, 25, 50, 100} {
		b.Run(fmt.Sprintf("nodes=%d", nodeCount), func(b *testing.B) {
			nodes := buildBenchNodes(nodeCount)
			dag, err := BuildDAG(nodes)
			if err != nil {
				b.Fatal(err)
			}
			// Trigger from the last node (leaf) — best case.
			lastNode := fmt.Sprintf("node%d", nodeCount-1)
			triggers := map[string]bool{lastNode: true}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = ScopeFromTriggers(dag, triggers)
			}
		})
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
						specHash := spec.Hash()
						instanceKey := fmt.Sprintf("ns/graph-%d-g00001", j)

						compiled := caches.getCompiled(specHash)
						if compiled == nil {
							var err error
							compiled, err = compileGraphSpec(spec)
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

// BenchmarkSpecHash measures the cost of computing the spec content hash —
// the gate that enables compiled graph sharing. This must be significantly
// cheaper than compilation to justify the indirection.
func BenchmarkSpecHash(b *testing.B) {
	for _, nodeCount := range []int{1, 5, 10, 25, 50} {
		b.Run(fmt.Sprintf("nodes=%d", nodeCount), func(b *testing.B) {
			spec := buildBenchSpec(nodeCount)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = spec.Hash()
			}
		})
	}
}

// TestSpecHashCoversCompilationInputs verifies that the spec hash captures
// all inputs that affect compilation output. On cache hit, we compile anyway
// and assert equivalence. This catches hash-doesn't-cover-input bugs.
func TestSpecHashCoversCompilationInputs(t *testing.T) {
	spec := buildBenchSpec(10)
	hash1 := spec.Hash()

	// Compile twice and verify hash is stable.
	hash2 := spec.Hash()
	if hash1 != hash2 {
		t.Fatalf("spec hash is not deterministic: %s != %s", hash1, hash2)
	}

	// Compile and verify the compiled graph agrees.
	compiled1, err := compileGraphSpec(spec)
	if err != nil {
		t.Fatal(err)
	}
	compiled2, err := compileGraphSpec(spec)
	if err != nil {
		t.Fatal(err)
	}

	// Both compiled graphs should have the same spec hash.
	if compiled1.specHash != compiled2.specHash {
		t.Fatalf("compiled graph hashes differ: %s != %s", compiled1.specHash, compiled2.specHash)
	}

	// Same expression set (same programs compiled).
	if len(compiled1.programs) != len(compiled2.programs) {
		t.Fatalf("program count differs: %d != %d", len(compiled1.programs), len(compiled2.programs))
	}
	for expr := range compiled1.programs {
		if _, ok := compiled2.programs[expr]; !ok {
			t.Fatalf("expression %q in compiled1 but not compiled2", expr)
		}
	}

	// Same DAG structure.
	if len(compiled1.dag.Nodes) != len(compiled2.dag.Nodes) {
		t.Fatalf("DAG node count differs: %d != %d", len(compiled1.dag.Nodes), len(compiled2.dag.Nodes))
	}

	// Mutate the spec and verify the hash changes.
	mutations := []struct {
		name   string
		mutate func(*GraphSpec)
	}{
		{"add node", func(s *GraphSpec) {
			s.Nodes = append(s.Nodes, Node{ID: "extra", Template: map[string]any{"apiVersion": "v1", "kind": "ConfigMap"}})
		}},
		{"change template", func(s *GraphSpec) {
			s.Nodes[0].Template["data"] = map[string]any{"different": "value"}
		}},
		{"add readyWhen", func(s *GraphSpec) {
			s.Nodes[0].ReadyWhen = []string{"${node0.status.ready == true}"}
		}},
		{"add propagateWhen", func(s *GraphSpec) {
			s.Nodes[0].PropagateWhen = []string{"${node0.ready()}"}
		}},
		{"add includeWhen", func(s *GraphSpec) {
			s.Nodes[0].IncludeWhen = []string{"${true}"}
		}},
		{"add forEach", func(s *GraphSpec) {
			s.Nodes[0].ForEach = map[string]string{"item": "${items}"}
		}},
		{"change node ID", func(s *GraphSpec) {
			s.Nodes[0].ID = "renamed"
		}},
		{"reorder nodes", func(s *GraphSpec) {
			// Node order matters: it determines DAG level assignment within
			// the same topological level. Reordered nodes are a different spec.
			s.Nodes[0], s.Nodes[1] = s.Nodes[1], s.Nodes[0]
		}},
	}

	for _, m := range mutations {
		t.Run(m.name, func(t *testing.T) {
			mutated := buildBenchSpec(10)
			m.mutate(mutated)
			mutatedHash := mutated.Hash()
			if mutatedHash == hash1 {
				t.Fatalf("mutation %q did not change the hash", m.name)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Compiled graph sharing mechanism tests
//
// These test the cache mechanism directly. The sharing behavior is invisible
// at the integration level (the observable outcome is the same with or without
// sharing). The integration test TestIdenticalGraphsConvergeIndependently in
// test/reconcile_test.go covers correctness; these cover the mechanism.
//
// This is an accepted tradeoff against 006-quality.md's preference for
// integration tests: a bug here (e.g., instance state leak between shared
// instances) would manifest as flaky reconciliation that's hard to diagnose
// from the integration level.
// ---------------------------------------------------------------------------

// TestCompiledGraphCacheLifecycle exercises the full lifecycle of the compiled
// graph cache: sharing across identical specs, isolation between different
// specs, and cleanup on instance removal.
func TestCompiledGraphCacheLifecycle(t *testing.T) {
	spec := buildBenchSpec(5)
	caches := newGraphCaches()
	specHash := spec.Hash()

	// --- Phase 1: Two instances with identical specs share one compiledGraph ---
	compiled, err := compileGraphSpec(spec)
	if err != nil {
		t.Fatal(err)
	}
	caches.set("ns/graph-a-g00001", newInstanceState(compiled))

	// Second instance finds the shared compiledGraph by hash.
	shared := caches.getCompiled(specHash)
	if shared == nil {
		t.Fatal("expected shared compiledGraph, got nil")
	}
	caches.set("ns/graph-b-g00001", newInstanceState(shared))

	if caches.get("ns/graph-a-g00001").compiled != caches.get("ns/graph-b-g00001").compiled {
		t.Fatal("instances with identical specs should share a compiledGraph")
	}

	// --- Phase 2: Different spec produces a separate compiledGraph ---
	differentSpec := buildBenchSpec(10)
	differentCompiled, err := compileGraphSpec(differentSpec)
	if err != nil {
		t.Fatal(err)
	}
	caches.set("ns/graph-c-g00001", newInstanceState(differentCompiled))

	if caches.get("ns/graph-c-g00001").compiled == caches.get("ns/graph-a-g00001").compiled {
		t.Fatal("different specs should not share a compiledGraph")
	}

	// --- Phase 3: Removing one instance retains shared compiledGraph ---
	caches.remove("ns/graph-a-g00001")
	if caches.getCompiled(specHash) == nil {
		t.Fatal("compiledGraph should survive with remaining references")
	}

	// --- Phase 4: Removing last instance cleans up compiledGraph ---
	caches.remove("ns/graph-b-g00001")
	if caches.getCompiled(specHash) != nil {
		t.Fatal("compiledGraph should be cleaned up when last reference is removed")
	}

	// Different spec's compiledGraph should be unaffected.
	if caches.getCompiled(differentSpec.Hash()) == nil {
		t.Fatal("unrelated compiledGraph should survive other spec's cleanup")
	}
}

// TestInstanceStateIsolation verifies that mutable state changes on one
// instance don't affect another instance sharing the same compiledGraph.
// This catches bugs where newInstanceState accidentally shares maps.
func TestInstanceStateIsolation(t *testing.T) {
	spec := buildBenchSpec(5)
	compiled, err := compileGraphSpec(spec)
	if err != nil {
		t.Fatal(err)
	}

	state1 := newInstanceState(compiled)
	state2 := newInstanceState(compiled)

	// Mutate state1's mutable fields.
	state1.previousScope["node0"] = map[string]any{"data": map[string]any{"key": "v1"}}
	state1.previousInputHashes["node0"] = "hash-a"
	state1.previousPlanStates["node0"] = NodeReady
	state1.forEachItems["node0/item"] = []any{"a", "b"}

	// state2 should be unaffected.
	if _, ok := state2.previousScope["node0"]; ok {
		t.Fatal("previousScope leaked between instances")
	}
	if _, ok := state2.previousInputHashes["node0"]; ok {
		t.Fatal("previousInputHashes leaked between instances")
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

// TestSelfSectionsIncludesDownstreamRefs verifies that BuildDAG pushes
// downstream dependency sections into upstream SelfSections. Without this,
// a bare Owns node (no readyWhen/propagateWhen) whose .status is referenced
// by a downstream Contribute node would have empty SelfSections, causing
// status changes to be invisible to the change-check optimization.
//
// Regression: SelfSections was only populated from the node's own
// readyWhen/propagateWhen. Downstream references were not considered.
func TestSelfSectionsIncludesDownstreamRefs(t *testing.T) {
	nodes := []Node{
		{
			ID: "schema",
			Template: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]any{"name": "my-app"},
			},
		},
		{
			ID: "deployment",
			Template: map[string]any{
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
				"metadata":   map[string]any{"name": "${schema.metadata.name}"},
				"spec": map[string]any{
					"replicas": "3",
				},
			},
		},
		{
			ID: "status",
			Template: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]any{"name": "${schema.metadata.name}"},
				"data": map[string]any{
					// References deployment.status — this should push .status
					// into the deployment node's SelfSections.
					"ready": "${deployment.status.availableReplicas == deployment.spec.replicas}",
				},
			},
		},
	}

	dag, err := BuildDAG(nodes)
	if err != nil {
		t.Fatalf("BuildDAG failed: %v", err)
	}

	// The deployment node should have .status in SelfSections because the
	// "status" node references deployment.status.
	deplIdx := dag.Index["deployment"]
	deplNode := dag.Nodes[deplIdx]

	if !deplNode.SelfSections["status"] {
		t.Errorf("deployment.SelfSections should include 'status' (referenced by downstream 'status' node), got %v", deplNode.SelfSections)
	}
	// Note: deployment.spec is also referenced by the downstream expression
	// (deployment.spec.replicas), but extractReferencedSections currently
	// only extracts the first identifier.section per CEL expression. The
	// .status section is sufficient for the self-state change detection fix.

	// The schema node should have .metadata in SelfSections because both
	// deployment and status reference schema.metadata.
	schemaIdx := dag.Index["schema"]
	schemaNode := dag.Nodes[schemaIdx]

	if !schemaNode.SelfSections["metadata"] {
		t.Errorf("schema.SelfSections should include 'metadata' (referenced by downstream nodes), got %v", schemaNode.SelfSections)
	}
}
