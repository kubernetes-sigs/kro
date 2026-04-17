package graphcontroller

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	"github.com/kubernetes-sigs/kro/experimental/deploy"
	"github.com/kubernetes-sigs/kro/experimental/stdlib"
)

// ═══════════════════════════════════════════════════════════════════════════════
// Standard library tests
//
// The standard library is a set of reusable types built on Graph:
//
//   Decorator — watch instances of a kind, create a sub-Graph per item
//   Kind      — define a new Kubernetes Kind (CRD + Decorator)
//   Singleton — declare a resource that should exist exactly once
//
// Decorator is the first stdlib primitive — a raw Graph that bootstraps
// the Decorator CRD and controller. Kind is built on top of Decorator.
// These tests validate:
//   1. Implementation files compile through the real Graph compiler.
//   2. The CEL expressions produce correct results with mock data
//      (resolution logic, conflict detection, edge cases).
//   3. Embedded resources parse correctly.
// ═══════════════════════════════════════════════════════════════════════════════

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// stdlibExamplesDir returns the path to the interface examples.
func stdlibExamplesDir() string {
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(thisFile), "..", "examples")
}

// stdlibImplDir returns the path to the implementation controllers.
func stdlibImplDir() string {
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(thisFile), "..", "stdlib")
}

func splitYAMLDocs(data []byte) [][]byte {
	var docs [][]byte
	for _, part := range bytes.Split(data, []byte("\n---")) {
		docs = append(docs, part)
	}
	return docs
}

func parseGraphDocsFrom(t *testing.T, dir, filename string) []*GraphSpec {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, filename))
	require.NoError(t, err, "reading %s", filename)

	var specs []*GraphSpec
	for i, doc := range splitYAMLDocs(data) {
		doc = bytes.TrimSpace(doc)
		if len(doc) == 0 {
			continue
		}
		var obj map[string]any
		if err := yaml.Unmarshal(doc, &obj); err != nil {
			continue
		}
		if obj == nil {
			continue
		}
		kind, _ := obj["kind"].(string)
		if kind != "Graph" {
			continue
		}
		spec, err := extractGraphSpec(obj)
		if err != nil {
			continue
		}
		require.NotEmpty(t, spec.Nodes, "doc %d in %s has empty nodes", i, filename)
		specs = append(specs, spec)
	}
	return specs
}

func parseDocsByKindFrom(t *testing.T, dir, filename, targetKind string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, filename))
	require.NoError(t, err)

	var docs []map[string]any
	for _, doc := range splitYAMLDocs(data) {
		doc = bytes.TrimSpace(doc)
		if len(doc) == 0 {
			continue
		}
		var obj map[string]any
		if err := yaml.Unmarshal(doc, &obj); err != nil {
			continue
		}
		if obj == nil {
			continue
		}
		kind, _ := obj["kind"].(string)
		if kind == targetKind {
			docs = append(docs, obj)
		}
	}
	return docs
}

// Convenience wrappers for the examples directory.
func parseGraphDocs(t *testing.T, filename string) []*GraphSpec {
	return parseGraphDocsFrom(t, stdlibExamplesDir(), filename)
}

func parseDocsByKind(t *testing.T, filename, targetKind string) []map[string]any {
	return parseDocsByKindFrom(t, stdlibExamplesDir(), filename, targetKind)
}

// unescapeOneLevel strips one '$' from '$${}' patterns, simulating what
// evalString does when evaluating a template containing deferred expressions.
func unescapeOneLevel(s string) string {
	return strings.ReplaceAll(s, "$${", "${")
}

func unescapeNodeSlice(nodes []Node) []Node {
	out := make([]Node, len(nodes))
	for i, n := range nodes {
		out[i] = Node{
			ID:          n.ID,
			Template:    unescapeMap(n.Template),
			IncludeWhen: unescapeStrings(n.IncludeWhen),
			ReadyWhen:   unescapeStrings(n.ReadyWhen),
		}
		if n.ForEach != nil {
			out[i].ForEach = make(map[string]string, len(n.ForEach))
			for k, v := range n.ForEach {
				out[i].ForEach[k] = unescapeOneLevel(v)
			}
		}
	}
	return out
}

func unescapeStrings(ss []string) []string {
	if ss == nil {
		return nil
	}
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = unescapeOneLevel(s)
	}
	return out
}

func unescapeMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = unescapeValue(v)
	}
	return out
}

func unescapeValue(v any) any {
	switch val := v.(type) {
	case string:
		return unescapeOneLevel(val)
	case map[string]any:
		return unescapeMap(val)
	case []any:
		out := make([]any, len(val))
		for i, item := range val {
			out[i] = unescapeValue(item)
		}
		return out
	default:
		return v
	}
}

// evalForEachExpr evaluates the forEach collection expression for a node
// and returns the resulting list. Scope must contain the upstream
// dependencies (e.g., "singletons" for the resolution Graph).
func evalForEachExpr(t *testing.T, compiled *compiledGraph, nodeID string, scope map[string]any) []any {
	t.Helper()
	node := compiled.dag.Nodes[compiled.dag.Index[nodeID]]
	eval := &evaluator{compiled: compiled, scope: scope}
	for _, expr := range node.ForEach {
		result, err := eval.evalString(expr)
		require.NoError(t, err)
		items, ok := result.([]any)
		if !ok {
			items = []any{result}
		}
		return items
	}
	t.Fatal("node has no forEach")
	return nil
}

// evalTemplateExpr evaluates a node's template with the given scope.
// For string templates (single CEL expression), returns the raw result.
// For map templates, returns the evaluated map.
func evalTemplateExpr(t *testing.T, compiled *compiledGraph, nodeID string, scope map[string]any) any {
	t.Helper()
	node := compiled.dag.Nodes[compiled.dag.Index[nodeID]]
	eval := &evaluator{compiled: compiled, scope: scope}
	result, err := eval.template(node.Template)
	require.NoError(t, err)
	return result
}

// ═══════════════════════════════════════════════════════════════════════════════
// Decorator (stdlib/decorator.yaml)
//
// The Decorator is a Kind that creates a controller Graph per instance.
// Each controller watches the specified kind and creates a sub-Graph
// per item with an auto-prepended "item" Watch node plus user nodes.
// ═══════════════════════════════════════════════════════════════════════════════

func TestStdlibDecoratorIsKind(t *testing.T) {
	docs := parseDocsByKindFrom(t, stdlibImplDir(), "decorator.yaml", "Kind")
	require.Len(t, docs, 1, "decorator.yaml should contain exactly one Kind")

	spec, _ := docs[0]["spec"].(map[string]any)
	assert.Equal(t, "Decorator", spec["kind"])
	assert.Equal(t, "experimental.kro.run", spec["group"])

	// The Kind should have nodes that create a controllerGraph.
	rawNodes, _ := spec["nodes"]
	nodes, err := parseNodeList(rawNodes)
	require.NoError(t, err)
	require.NotEmpty(t, nodes)
	assert.Equal(t, "controllerGraph", nodes[0].ID)
}

func TestStdlibDecoratorEvalStringEscape(t *testing.T) {
	// The Decorator's controllerGraph constructs sub-Graph nodes via a
	// CEL expression: $${[...item Watch...] + decorator.spec.nodes}.
	// User nodes in decorator.spec.nodes contain ${item.*} expressions
	// that MUST survive L1 evalString without being evaluated — they're
	// data that passes through to L2 (the sub-Graph).
	//
	// This test builds a synthetic graph mirroring the Decorator's L1
	// structure, evaluates the spec.nodes CEL expression with mock data
	// containing ${item.*} strings, and asserts they arrive unchanged.
	graph := &GraphSpec{
		Nodes: []Node{
			{
				ID: "decorator",
				Template: map[string]any{
					"apiVersion": "experimental.kro.run/v1alpha1",
					"kind":       "Decorator",
					"metadata":   map[string]any{"name": "test", "namespace": "default"},
				},
			},
			{
				ID: "items",
				Template: map[string]any{
					"apiVersion": "v1",
					"kind":       "Namespace",
					"selector":   map[string]any{},
				},
			},
			{
				ID:      "instances",
				ForEach: map[string]string{"item": "${items}"},
				Template: map[string]any{
					"apiVersion": "experimental.kro.run/v1alpha1",
					"kind":       "Graph",
					"metadata":   map[string]any{"name": "${decorator.metadata.name}-${item.metadata.name}"},
					"spec": map[string]any{
						// This is the critical expression. At L1, evalString
						// evaluates it and returns a list. The list contains
						// maps with ${item.*} strings from decorator.spec.nodes.
						// Those strings must NOT be evaluated at L1.
						"nodes": `${[{"id": "item", "template": {"apiVersion": decorator.spec.watch.apiVersion, "kind": decorator.spec.watch.kind, "metadata": {"name": item.metadata.name}}}] + decorator.spec.nodes}`,
					},
				},
			},
		},
	}

	compiled, err := compileGraphSpec(graph, nil)
	require.NoError(t, err)

	// Mock data: decorator.spec.nodes contains a user node with ${item.*}.
	scope := map[string]any{
		"decorator": map[string]any{
			"metadata": map[string]any{"name": "test-dec", "namespace": "default"},
			"spec": map[string]any{
				"watch": map[string]any{"apiVersion": "v1", "kind": "Namespace"},
				"nodes": []any{
					map[string]any{
						"id": "policy",
						"template": map[string]any{
							"apiVersion": "networking.k8s.io/v1",
							"kind":       "NetworkPolicy",
							"metadata":   map[string]any{"name": "deny", "namespace": "${item.metadata.name}"},
						},
					},
				},
			},
		},
		"item": map[string]any{
			"metadata": map[string]any{"name": "prod-ns", "namespace": "default"},
		},
	}

	// Evaluate the instances template.
	result := evalTemplateExpr(t, compiled, "instances", scope)
	resultMap, ok := result.(map[string]any)
	require.True(t, ok)
	resultSpec, _ := resultMap["spec"].(map[string]any)
	resultNodes, ok := resultSpec["nodes"].([]any)
	require.True(t, ok, "spec.nodes should be a list after evalString")
	require.GreaterOrEqual(t, len(resultNodes), 2)

	// The user's node template should contain the UNEVALUATED string
	// "${item.metadata.name}" — NOT the concrete value "prod-ns".
	userNode, _ := resultNodes[1].(map[string]any)
	userTmpl, _ := userNode["template"].(map[string]any)
	userMeta, _ := userTmpl["metadata"].(map[string]any)
	assert.Equal(t, "${item.metadata.name}", userMeta["namespace"],
		"${item.*} expressions must survive evalString — they are evaluated at L2, not L1")
}

func TestStdlibDecoratorSubGraph(t *testing.T) {
	// Simulate what the Decorator controller produces: a sub-Graph
	// with an "item" Watch node plus user nodes.
	t.Run("sub-Graph with item Watch compiles", func(t *testing.T) {
		// This is what the Decorator controller creates per item.
		graphNodes := []Node{
			{
				ID: "item",
				Ref: map[string]any{
					"apiVersion": "v1",
					"kind":       "Namespace",
					"metadata":   map[string]any{"name": "test-ns"},
				},
				ref: NodeTypeRef,
			},
			{
				ID: "policy",
				Template: map[string]any{
					"apiVersion": "networking.k8s.io/v1",
					"kind":       "NetworkPolicy",
					"metadata":   map[string]any{"name": "default-deny", "namespace": "${item.metadata.name}"},
					"spec":       map[string]any{"podSelector": map[string]any{}},
				},
				ref: NodeTypeTemplate,
			},
		}
		graph := &GraphSpec{Nodes: graphNodes}
		compiled, err := compileGraphSpec(graph, nil)
		require.NoError(t, err, "sub-Graph should compile")

		// item is a Watch node (has metadata.name, no selector).
		assert.Equal(t, NodeTypeRef, compiled.dag.References["item"])

		// policy depends on item.
		policyDeps := compiled.dag.Nodes[compiled.dag.Index["policy"]].Dependencies
		assert.True(t, policyDeps["item"], "policy should depend on item")
	})

	t.Run("sub-Graph with dependent nodes", func(t *testing.T) {
		graphNodes := []Node{
			{
				ID: "item",
				Ref: map[string]any{
					"apiVersion": "v1",
					"kind":       "Namespace",
					"metadata":   map[string]any{"name": "test-ns"},
				},
				ref: NodeTypeRef,
			},
			{
				ID: "quota",
				Template: map[string]any{
					"apiVersion": "v1",
					"kind":       "ResourceQuota",
					"metadata":   map[string]any{"name": "default", "namespace": "${item.metadata.name}"},
					"spec":       map[string]any{"hard": map[string]any{"pods": "10"}},
				},
				ref: NodeTypeTemplate,
			},
			{
				ID: "policy",
				Template: map[string]any{
					"apiVersion": "networking.k8s.io/v1",
					"kind":       "NetworkPolicy",
					"metadata":   map[string]any{"name": "${quota.metadata.name}-deny", "namespace": "${item.metadata.name}"},
					"spec":       map[string]any{"podSelector": map[string]any{}},
				},
				ref: NodeTypeTemplate,
			},
		}
		graph := &GraphSpec{Nodes: graphNodes}
		compiled, err := compileGraphSpec(graph, nil)
		require.NoError(t, err)

		policyDeps := compiled.dag.Nodes[compiled.dag.Index["policy"]].Dependencies
		assert.True(t, policyDeps["quota"], "policy should depend on quota")
		assert.True(t, policyDeps["item"], "policy should depend on item")
	})
}

// ═══════════════════════════════════════════════════════════════════════════════
// Kind Controller (stdlib/kind.yaml)
//
// The Kind controller is the bootstrap root — a raw Graph that watches
// all Kinds, creates CRDs per Kind, watches instances, and creates
// per-instance Graphs. It hand-rolls the forEach pattern that Decorator
// codifies.
// ═══════════════════════════════════════════════════════════════════════════════

func TestStdlibKindController(t *testing.T) {
	specs := parseGraphDocsFrom(t, stdlibImplDir(), "kind.yaml")
	require.Len(t, specs, 1)

	compiled, err := compileGraphSpec(specs[0], nil)
	require.NoError(t, err)
	dag := compiled.dag

	// watchKinds should depend on kindCrd (includeWhen references it).
	assert.Contains(t, dag.Dependents["kindCrd"], dag.Index["watchKinds"])

	// controllers depends on watchKinds (forEach over watchKinds).
	assert.Contains(t, dag.Dependents["watchKinds"], dag.Index["controllers"])

	// controllers should have forEach.
	controllerNode := dag.Nodes[dag.Index["controllers"]]
	assert.NotNil(t, controllerNode.ForEach, "controllers should have forEach")

	t.Logf("Kind controller: %d nodes, %d levels", len(dag.Nodes), len(dag.Levels))
	for _, node := range dag.Nodes {
		t.Logf("  %s: %s", node.ID, dag.References[node.ID])
	}
}

func TestStdlibKindEscapeLevels(t *testing.T) {
	specs := parseGraphDocsFrom(t, stdlibImplDir(), "kind.yaml")
	require.Len(t, specs, 1)

	compiled, err := compileGraphSpec(specs[0], nil)
	require.NoError(t, err)

	// Escaped expressions ($${...}) should not be compiled at L0.
	for expr := range compiled.programs {
		assert.NotContains(t, expr, "plural(k.spec.kind)", "escaped expression leaked into L0")
		assert.NotContains(t, expr, "k.spec.group", "escaped expression leaked into L0")
	}

	// L0 should compile: watchKinds (Watch), k.metadata.* (forEach variable).
	foundWatchKinds, foundKMeta := false, false
	for expr := range compiled.programs {
		if expr == "watchKinds" {
			foundWatchKinds = true
		}
		if expr == "k.metadata.name" || expr == "k.metadata.namespace" {
			foundKMeta = true
		}
	}
	assert.True(t, foundWatchKinds, "'watchKinds' should be compiled at L0")
	assert.True(t, foundKMeta, "'k.metadata.*' should be compiled at L0")
}

// ═══════════════════════════════════════════════════════════════════════════════
// Singleton resolution (stdlib/singleton.yaml)
//
// The Singleton is implemented as Kind + Decorator. The Decorator watches
// all Singletons and creates a sub-Graph per item. Each sub-Graph has a
// peers Watch and an includeWhen gate that self-determines the claim holder.
//
// The includeWhen expression is tested directly by compiling it from the
// YAML and evaluating against mock peer data.
// ═══════════════════════════════════════════════════════════════════════════════

func TestStdlibSingletonResolution(t *testing.T) {
	// Parse the Kind from singleton.yaml and extract spec.nodes.
	docs := parseDocsByKindFrom(t, stdlibImplDir(), "singleton.yaml", "Kind")
	require.NotEmpty(t, docs)
	spec, _ := docs[0]["spec"].(map[string]any)
	rawNodes, _ := spec["nodes"]
	nodes, err := parseNodeList(rawNodes)
	require.NoError(t, err)

	// Build a mini graph: schema Watch + all parsed Kind nodes.
	miniNodes := []Node{
		{ID: "schema", Template: map[string]any{"apiVersion": "experimental.kro.run/v1alpha1", "kind": "Singleton", "metadata": map[string]any{"name": "test"}}},
	}
	miniNodes = append(miniNodes, nodes...)
	miniSpec := &GraphSpec{Nodes: miniNodes}
	compiled, err := compileGraphSpec(miniSpec, nil)
	require.NoError(t, err, "singleton per-instance graph should compile")

	// Collect CEL expressions from definition nodes for manual evaluation.
	nodesByID := make(map[string]*Node)
	for i := range nodes {
		nodesByID[nodes[i].ID] = &nodes[i]
	}

	// Helper: extract CEL expression from a ${...} string.
	expr := func(s string) string {
		t.Helper()
		_, e, _, _ := findExpr(s, 0)
		require.NotEmpty(t, e, "expected ${...} expression in %q", s)
		return e
	}

	// Helper: build a mock Singleton.
	singleton := func(name string, priority int64, apiVersion, kind, ns, resName string) map[string]any {
		tmpl := map[string]any{
			"apiVersion": apiVersion,
			"kind":       kind,
			"metadata":   map[string]any{"name": resName},
		}
		if ns != "" {
			tmpl["metadata"].(map[string]any)["namespace"] = ns
		}
		return map[string]any{
			"metadata": map[string]any{"name": name},
			"spec": map[string]any{
				"priority": priority,
				"template": tmpl,
			},
		}
	}

	// Helper: compute identity string for a Singleton (matches the CEL formula).
	resourceIdentity := func(s map[string]any) string {
		t.Helper()
		tmpl := s["spec"].(map[string]any)["template"].(map[string]any)
		meta := tmpl["metadata"].(map[string]any)
		ns, _ := meta["namespace"].(string)
		return tmpl["apiVersion"].(string) + "/" + tmpl["kind"].(string) + "/" + ns + "/" + meta["name"].(string)
	}

	// Helper: build the full definition chain and evaluate whether
	// a given Singleton is the claim holder among all singletons.
	isHolder := func(schema map[string]any, singletons []any) bool {
		t.Helper()

		// Build identities list (simulates the forEach definition).
		identities := make([]any, len(singletons))
		for i, p := range singletons {
			pm := p.(map[string]any)
			identities[i] = map[string]any{
				"name":     pm["metadata"].(map[string]any)["name"],
				"priority": pm["spec"].(map[string]any)["priority"],
				"identity": resourceIdentity(pm),
			}
		}

		identity := resourceIdentity(schema)

		// Build peers list (simulates the forEach definition).
		var peers []any
		for _, id := range identities {
			if id.(map[string]any)["identity"] == identity {
				peers = append(peers, map[string]any{
					"name":     id.(map[string]any)["name"],
					"priority": id.(map[string]any)["priority"],
				})
			}
		}

		// Evaluate holder.name.
		scope := map[string]any{"peers": peers}
		holderName, err := compiled.eval(expr(nodesByID["holder"].Def["name"].(string)), scope)
		require.NoError(t, err)

		// Evaluate includeWhen.
		scope["holder"] = map[string]any{"name": holderName}
		scope["schema"] = schema
		result, err := compiled.eval(expr(nodesByID["target"].IncludeWhen[0]), scope)
		require.NoError(t, err)
		return result.(bool)
	}

	teamA := singleton("team-a", 10, "v1", "ConfigMap", "default", "contested")
	teamB := singleton("team-b", 100, "v1", "ConfigMap", "default", "contested")
	teamC := singleton("team-c", 100, "v1", "ConfigMap", "default", "contested")
	different := singleton("other", 50, "v1", "Secret", "default", "other-res")

	t.Run("single Singleton always wins", func(t *testing.T) {
		assert.True(t, isHolder(teamA, []any{teamA}))
	})

	t.Run("higher priority wins", func(t *testing.T) {
		peers := []any{teamA, teamB}
		assert.False(t, isHolder(teamA, peers), "team-a (10) should lose to team-b (100)")
		assert.True(t, isHolder(teamB, peers), "team-b (100) should win")
	})

	t.Run("same priority tie broken by name", func(t *testing.T) {
		peers := []any{teamB, teamC}
		assert.True(t, isHolder(teamB, peers), "team-b should win (lexicographically lower)")
		assert.False(t, isHolder(teamC, peers), "team-c should lose")
	})

	t.Run("different target not conflicting", func(t *testing.T) {
		peers := []any{teamA, different}
		assert.True(t, isHolder(teamA, peers), "team-a should win for its target")
		assert.True(t, isHolder(different, peers), "different target should also win")
	})

	t.Run("three-way conflict highest wins", func(t *testing.T) {
		peers := []any{teamA, teamB, teamC}
		assert.False(t, isHolder(teamA, peers))
		assert.True(t, isHolder(teamB, peers), "team-b (100, lowest name) should win")
		assert.False(t, isHolder(teamC, peers))
	})
}

// ═══════════════════════════════════════════════════════════════════════════════
// Interface file structural validation
// ═══════════════════════════════════════════════════════════════════════════════

func TestStdlibInterfaceFiles(t *testing.T) {
	cases := []struct {
		file    string
		kind    string
		minDocs int
	}{
		{"6-kind.yaml", "Kind", 1},
		{"7-decorator.yaml", "Decorator", 1},
		{"8-singleton.yaml", "Singleton", 2},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			docs := parseDocsByKind(t, tc.file, tc.kind)
			assert.GreaterOrEqual(t, len(docs), tc.minDocs,
				"expected at least %d %s documents", tc.minDocs, tc.kind)

			for i, doc := range docs {
				meta, _ := doc["metadata"].(map[string]any)
				name, _ := meta["name"].(string)
				assert.NotEmpty(t, name, "doc %d should have metadata.name", i)

				_, ok := doc["spec"].(map[string]any)
				assert.True(t, ok, "doc %d should have spec", i)

				t.Logf("  %s/%s", tc.kind, name)
			}
		})
	}
}

// ═══════════════════════════════════════════════════════════════════════════════
// CEL patterns used by the standard library
// ═══════════════════════════════════════════════════════════════════════════════

func TestStdlibSizeGuard(t *testing.T) {
	spec := &GraphSpec{
		Nodes: []Node{
			{
				ID: "webapps",
				Watch: map[string]any{
					"apiVersion": "experimental.kro.run/v1alpha1",
					"kind":       "WebApp",
					"selector":   map[string]any{},
				},
				ref: NodeTypeWatch,
			},
			{
				ID:          "dashboard",
				IncludeWhen: []string{"${webapps.size() > 0}"},
				Template: map[string]any{
					"apiVersion": "v1",
					"kind":       "ConfigMap",
					"metadata":   map[string]any{"name": "webapp-dashboard", "namespace": "monitoring"},
					"data":       map[string]any{},
				},
				ref: NodeTypeTemplate,
			},
		},
	}
	compiled, err := compileGraphSpec(spec, nil)
	require.NoError(t, err)
	found := false
	for expr := range compiled.programs {
		if expr == "webapps.size() > 0" {
			found = true
		}
	}
	assert.True(t, found, "webapps.size() > 0 should be compiled")
}

func TestStdlibDistinctCEL(t *testing.T) {
	spec := &GraphSpec{
		Nodes: []Node{
			{
				ID: "items",
				Watch: map[string]any{
					"apiVersion": "v1",
					"kind":       "Pod",
					"selector":   map[string]any{"app": "monitoring"},
				},
				ref: NodeTypeWatch,
			},
			{
				ID:      "sa",
				ForEach: map[string]string{"ns": "${items.map(i, i.metadata.namespace).distinct()}"},
				Template: map[string]any{
					"apiVersion": "v1",
					"kind":       "ServiceAccount",
					"metadata":   map[string]any{"name": "monitoring", "namespace": "${ns}"},
				},
				ref: NodeTypeTemplate,
			},
		},
	}
	compiled, err := compileGraphSpec(spec, nil)
	require.NoError(t, err)
	found := false
	for expr := range compiled.programs {
		if expr == "items.map(i, i.metadata.namespace).distinct()" {
			found = true
		}
	}
	assert.True(t, found, "distinct() expression should be compiled")
}

// ═══════════════════════════════════════════════════════════════════════════════
// Graph compilation
// ═══════════════════════════════════════════════════════════════════════════════

func TestStdlibKindGraphCompiles(t *testing.T) {
	specs := parseGraphDocsFrom(t, stdlibImplDir(), "kind.yaml")
	require.Len(t, specs, 1, "kind.yaml should contain exactly one Graph")

	compiled, err := compileGraphSpec(specs[0], nil)
	require.NoError(t, err, "kind graph should compile")
	dag := compiled.dag
	assert.NotEmpty(t, dag.TopologicalOrder, "kind graph should have nodes")
	t.Logf("kind graph: %d nodes, %d levels", len(dag.Nodes), len(dag.Levels))
	for _, node := range dag.Nodes {
		t.Logf("  %s: %s", node.ID, dag.References[node.ID])
	}
}

// ═══════════════════════════════════════════════════════════════════════════════
// Embedded resources parse correctly
// ═══════════════════════════════════════════════════════════════════════════════

func TestStdlibEmbeddedCRDsParse(t *testing.T) {
	entries, err := fs.ReadDir(deploy.CRDs, ".")
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(entries), 2, "expected at least 2 CRD files (Graph, GraphRevision)")

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		t.Run(entry.Name(), func(t *testing.T) {
			data, err := fs.ReadFile(deploy.CRDs, entry.Name())
			require.NoError(t, err)

			var obj map[string]any
			require.NoError(t, yaml.Unmarshal(data, &obj))
			assert.Equal(t, "CustomResourceDefinition", obj["kind"])

			meta, _ := obj["metadata"].(map[string]any)
			name, _ := meta["name"].(string)
			assert.NotEmpty(t, name)
			t.Logf("  CRD: %s", name)
		})
	}
}

func TestStdlibEmbeddedResourcesParse(t *testing.T) {
	entries, err := fs.ReadDir(stdlib.Resources, ".")
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(entries), 4, "expected at least 4 stdlib files")

	expectedKinds := map[string]string{
		"kind.yaml":      "Graph",
		"decorator.yaml": "Kind",
		"singleton.yaml": "Kind",
		"rgd.yaml":       "Kind",
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}
		t.Run(entry.Name(), func(t *testing.T) {
			data, err := fs.ReadFile(stdlib.Resources, entry.Name())
			require.NoError(t, err)

			var kinds []string
			for _, doc := range splitYAMLDocs(data) {
				doc = bytes.TrimSpace(doc)
				if len(doc) == 0 {
					continue
				}
				var obj map[string]any
				if err := yaml.Unmarshal(doc, &obj); err != nil || obj == nil {
					continue
				}
				kind, _ := obj["kind"].(string)
				kinds = append(kinds, kind)
				meta, _ := obj["metadata"].(map[string]any)
				name, _ := meta["name"].(string)
				t.Logf("  %s (kind: %s)", name, kind)
			}
			if expected, ok := expectedKinds[entry.Name()]; ok {
				assert.Contains(t, kinds, expected, "%s should contain kind %s", entry.Name(), expected)
			}
		})
	}
}
