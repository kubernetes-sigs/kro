package graphcontroller

import (
	"bytes"
	"fmt"
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

// makeSingleton creates a Singleton CR with single-template API.
// resource is "apiVersion/Kind/namespace/name".
func makeSingleton(name string, priority int64, resource string) map[string]any {
	return makeSingletonWithData(name, priority, resource, nil)
}

// makeSingletonWithData creates a Singleton CR with extra data in the template
// for distinguishing winners in resolution tests.
func makeSingletonWithData(name string, priority int64, resource string, data map[string]any) map[string]any {
	parts := strings.SplitN(resource, "/", 4)
	template := map[string]any{
		"apiVersion": parts[0],
		"kind":       parts[1],
		"metadata":   map[string]any{"namespace": parts[2], "name": parts[3]},
	}
	if data != nil {
		template["data"] = data
	}
	return map[string]any{
		"metadata": map[string]any{"name": name},
		"spec": map[string]any{
			"priority": priority,
			"template": template,
		},
	}
}

// makeSingletonClusterScoped creates a Singleton CR with a cluster-scoped resource
// (no namespace field in template metadata). resource is "apiVersion/Kind/name".
func makeSingletonClusterScoped(name string, priority int64, resource string) map[string]any {
	parts := strings.SplitN(resource, "/", 3)
	template := map[string]any{
		"apiVersion": parts[0],
		"kind":       parts[1],
		"metadata":   map[string]any{"name": parts[2]},
	}
	return map[string]any{
		"metadata": map[string]any{"name": name},
		"spec": map[string]any{
			"priority": priority,
			"template": template,
		},
	}
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
				Template: map[string]any{
					"apiVersion": "v1",
					"kind":       "Namespace",
					"metadata":   map[string]any{"name": "test-ns"},
				},
			},
			{
				ID: "policy",
				Template: map[string]any{
					"apiVersion": "networking.k8s.io/v1",
					"kind":       "NetworkPolicy",
					"metadata":   map[string]any{"name": "default-deny", "namespace": "${item.metadata.name}"},
					"spec":       map[string]any{"podSelector": map[string]any{}},
				},
			},
		}
		graph := &GraphSpec{Nodes: graphNodes}
		compiled, err := compileGraphSpec(graph, nil)
		require.NoError(t, err, "sub-Graph should compile")

		// item is a Watch node (has metadata.name, no selector).
		assert.Equal(t, ReferenceWatch, compiled.dag.References["item"])

		// policy depends on item.
		policyDeps := compiled.dag.Nodes[compiled.dag.Index["policy"]].Dependencies
		assert.True(t, policyDeps["item"], "policy should depend on item")
	})

	t.Run("sub-Graph with dependent nodes", func(t *testing.T) {
		graphNodes := []Node{
			{
				ID: "item",
				Template: map[string]any{
					"apiVersion": "v1",
					"kind":       "Namespace",
					"metadata":   map[string]any{"name": "test-ns"},
				},
			},
			{
				ID: "quota",
				Template: map[string]any{
					"apiVersion": "v1",
					"kind":       "ResourceQuota",
					"metadata":   map[string]any{"name": "default", "namespace": "${item.metadata.name}"},
					"spec":       map[string]any{"hard": map[string]any{"pods": "10"}},
				},
			},
			{
				ID: "policy",
				Template: map[string]any{
					"apiVersion": "networking.k8s.io/v1",
					"kind":       "NetworkPolicy",
					"metadata":   map[string]any{"name": "${quota.metadata.name}-deny", "namespace": "${item.metadata.name}"},
					"spec":       map[string]any{"podSelector": map[string]any{}},
				},
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

	// L0 should compile: watchKinds (WatchKind), k.metadata.* (forEach variable).
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
// The Singleton Kind creates a shared resolution Graph that watches all
// Singletons, groups them by target resource identity using .distinct(),
// and picks the winner per target.
//
// Tests evaluate the actual forEach and template CEL expressions from the
// resolution Graph with mock Singleton data.
// ═══════════════════════════════════════════════════════════════════════════════

func TestStdlibSingletonResolution(t *testing.T) {
	// Parse the Singleton Kind's nodes. The first node's template is
	// the resolution Graph. Extract and compile that Graph.
	//
	// The resolution Graph is embedded inside the Kind template, so its
	// expressions use $${} escaping to survive L1 compilation. We strip
	// one escape level before compiling, simulating what L1 evaluation does.
	docs := parseDocsByKindFrom(t, stdlibImplDir(), "singleton.yaml", "Kind")
	require.NotEmpty(t, docs)
	spec, _ := docs[0]["spec"].(map[string]any)
	rawNodes, _ := spec["nodes"]
	nodes, err := parseNodeList(rawNodes)
	require.NoError(t, err)
	require.NotEmpty(t, nodes)

	// The first node's template is the resolution Graph.
	resolutionGraph := nodes[0].Template
	resolutionSpec, _ := resolutionGraph["spec"].(map[string]any)
	resolutionRawNodes, _ := resolutionSpec["nodes"]
	resolutionNodes, err := parseNodeList(resolutionRawNodes)
	require.NoError(t, err)

	// Strip one escape level ($${} → ${}) before compiling standalone.
	resolutionNodes = unescapeNodeSlice(resolutionNodes)

	graph := &GraphSpec{Nodes: resolutionNodes}
	compiled, err := compileGraphSpec(graph, nil)
	require.NoError(t, err, "resolution Graph should compile")

	// The "targets" node has forEach over distinct keys and creates
	// sub-Graphs. Verify structure.
	targetsNode := compiled.dag.Nodes[compiled.dag.Index["targets"]]
	assert.NotNil(t, targetsNode.ForEach, "targets node should have forEach")
	assert.Empty(t, targetsNode.IncludeWhen, "targets node should not have includeWhen")

	// Helper: evaluate forEach target keys.
	evalTargets := func(singletons []any) []string {
		t.Helper()
		scope := map[string]any{"singletons": singletons}
		raw := evalForEachExpr(t, compiled, "targets", scope)
		targets := make([]string, len(raw))
		for i, v := range raw {
			targets[i] = v.(string)
		}
		return targets
	}

	// Helper: evaluate the sub-Graph's spec.nodes CEL expression to extract
	// the winning template. The targets node's template is a Graph with
	// spec.nodes as a CEL expression. We evaluate that expression to get
	// the node list, then extract the "resource" node's template.
	evalWinner := func(singletons []any, key string) map[string]any {
		t.Helper()
		scope := map[string]any{"singletons": singletons, "key": key}
		// Evaluate the targets node's template to get the sub-Graph.
		result := evalTemplateExpr(t, compiled, "targets", scope)
		subGraph, ok := result.(map[string]any)
		require.True(t, ok, "sub-Graph template should produce a map, got %T", result)

		// Extract spec.nodes from the evaluated sub-Graph template.
		subSpec, _ := subGraph["spec"].(map[string]any)
		require.NotNil(t, subSpec, "sub-Graph should have spec")
		subNodes, ok := subSpec["nodes"].([]any)
		require.True(t, ok, "spec.nodes should be a list, got %T", subSpec["nodes"])
		require.NotEmpty(t, subNodes, "spec.nodes should not be empty")

		// The first (and only) node is the "resource" node with the winning template.
		resourceNode, ok := subNodes[0].(map[string]any)
		require.True(t, ok, "resource node should be a map")
		template, ok := resourceNode["template"].(map[string]any)
		require.True(t, ok, "resource template should be a map, got %T", resourceNode["template"])
		return template
	}

	t.Run("single Singleton always wins", func(t *testing.T) {
		s := makeSingleton("only-one", 100, "v1/ConfigMap/default/foo")
		targets := evalTargets([]any{s})
		assert.Len(t, targets, 1, "one singleton → one target")

		winner := evalWinner([]any{s}, targets[0])
		assert.Equal(t, "ConfigMap", winner["kind"])
		meta, _ := winner["metadata"].(map[string]any)
		assert.Equal(t, "foo", meta["name"])
	})

	t.Run("higher priority wins", func(t *testing.T) {
		a := makeSingletonWithData("team-a", 100, "v1/ConfigMap/monitoring/dashboard",
			map[string]any{"owner": "team-a"})
		b := makeSingletonWithData("team-b", 200, "v1/ConfigMap/monitoring/dashboard",
			map[string]any{"owner": "team-b"})
		items := []any{a, b}

		targets := evalTargets(items)
		assert.Len(t, targets, 1, "same resource → one target")

		winner := evalWinner(items, targets[0])
		data, _ := winner["data"].(map[string]any)
		assert.Equal(t, "team-b", data["owner"],
			"team-b (priority 200) should win over team-a (priority 100)")
	})

	t.Run("lower priority loses", func(t *testing.T) {
		a := makeSingletonWithData("alpha", 500, "v1/ConfigMap/default/shared",
			map[string]any{"owner": "alpha"})
		b := makeSingletonWithData("beta", 50, "v1/ConfigMap/default/shared",
			map[string]any{"owner": "beta"})
		items := []any{a, b}

		targets := evalTargets(items)
		assert.Len(t, targets, 1)
		winner := evalWinner(items, targets[0])
		data, _ := winner["data"].(map[string]any)
		assert.Equal(t, "alpha", data["owner"],
			"alpha (priority 500) should win over beta (priority 50)")
	})

	t.Run("same priority tie broken by name", func(t *testing.T) {
		a := makeSingletonWithData("alpha", 100, "v1/ConfigMap/default/shared",
			map[string]any{"owner": "alpha"})
		b := makeSingletonWithData("beta", 100, "v1/ConfigMap/default/shared",
			map[string]any{"owner": "beta"})
		items := []any{a, b}

		targets := evalTargets(items)
		assert.Len(t, targets, 1)
		winner := evalWinner(items, targets[0])
		data, _ := winner["data"].(map[string]any)
		assert.Equal(t, "alpha", data["owner"],
			"alpha wins tie (lexicographically lower name)")
	})

	t.Run("three-way conflict highest wins", func(t *testing.T) {
		a := makeSingletonWithData("team-a", 100, "v1/ConfigMap/default/config",
			map[string]any{"owner": "team-a"})
		b := makeSingletonWithData("team-b", 200, "v1/ConfigMap/default/config",
			map[string]any{"owner": "team-b"})
		c := makeSingletonWithData("team-c", 300, "v1/ConfigMap/default/config",
			map[string]any{"owner": "team-c"})
		items := []any{a, b, c}

		targets := evalTargets(items)
		assert.Len(t, targets, 1, "same resource → one target")
		winner := evalWinner(items, targets[0])
		data, _ := winner["data"].(map[string]any)
		assert.Equal(t, "team-c", data["owner"],
			"team-c (priority 300) should win three-way conflict")
	})

	t.Run("different GVK same name not conflicting", func(t *testing.T) {
		a := makeSingleton("sa", 100, "v1/ServiceAccount/default/monitor")
		b := makeSingleton("cm", 100, "v1/ConfigMap/default/monitor")
		items := []any{a, b}

		targets := evalTargets(items)
		assert.Len(t, targets, 2, "different GVKs → two targets")
	})

	t.Run("different namespace not conflicting", func(t *testing.T) {
		a := makeSingleton("prod", 100, "v1/ConfigMap/production/config")
		b := makeSingleton("staging", 100, "v1/ConfigMap/staging/config")
		items := []any{a, b}

		targets := evalTargets(items)
		assert.Len(t, targets, 2, "same name in different namespaces → two targets")
	})

	t.Run("many Singletons one resource", func(t *testing.T) {
		items := make([]any, 10)
		for i := range items {
			items[i] = makeSingleton(
				fmt.Sprintf("s%02d", i),
				int64(i*10),
				"v1/ConfigMap/default/contested",
			)
		}

		targets := evalTargets(items)
		assert.Len(t, targets, 1, "all target the same resource → one target")

		winner := evalWinner(items, targets[0])
		assert.Equal(t, "ConfigMap", winner["kind"])
	})

	t.Run("negative priority", func(t *testing.T) {
		a := makeSingleton("fallback", -100, "v1/ConfigMap/default/config")
		b := makeSingleton("override", -1, "v1/ConfigMap/default/config")
		items := []any{a, b}

		targets := evalTargets(items)
		assert.Len(t, targets, 1)
		_ = evalWinner(items, targets[0])
	})

	t.Run("zero priority", func(t *testing.T) {
		a := makeSingleton("alpha", 0, "v1/ConfigMap/default/config")
		b := makeSingleton("beta", 0, "v1/ConfigMap/default/config")
		items := []any{a, b}

		targets := evalTargets(items)
		assert.Len(t, targets, 1)
		_ = evalWinner(items, targets[0])
	})

	t.Run("cluster-scoped resource no namespace", func(t *testing.T) {
		a := makeSingletonClusterScoped("role-a", 100, "rbac.authorization.k8s.io/v1/ClusterRole/admin")
		b := makeSingletonClusterScoped("role-b", 200, "rbac.authorization.k8s.io/v1/ClusterRole/admin")
		items := []any{a, b}

		targets := evalTargets(items)
		assert.Len(t, targets, 1, "same cluster-scoped resource → one target")
		_ = evalWinner(items, targets[0])
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
				Template: map[string]any{
					"apiVersion": "experimental.kro.run/v1alpha1",
					"kind":       "WebApp",
					"selector":   map[string]any{},
				},
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
				Template: map[string]any{
					"apiVersion": "v1",
					"kind":       "Pod",
					"selector":   map[string]any{"app": "monitoring"},
				},
			},
			{
				ID:      "sa",
				ForEach: map[string]string{"ns": "${items.map(i, i.metadata.namespace).distinct()}"},
				Template: map[string]any{
					"apiVersion": "v1",
					"kind":       "ServiceAccount",
					"metadata":   map[string]any{"name": "monitoring", "namespace": "${ns}"},
				},
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

			var foundKind string
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
				foundKind = kind
				meta, _ := obj["metadata"].(map[string]any)
				name, _ := meta["name"].(string)
				t.Logf("  %s (kind: %s)", name, kind)
			}
			if expected, ok := expectedKinds[entry.Name()]; ok {
				assert.Equal(t, expected, foundKind, "%s should be kind %s", entry.Name(), expected)
			}
		})
	}
}
