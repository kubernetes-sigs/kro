package graphcontroller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/kubernetes-sigs/kro/experimental/controller/compiler"
	dagpkg "github.com/kubernetes-sigs/kro/experimental/controller/dag"
	graphpkg "github.com/kubernetes-sigs/kro/experimental/controller/graph"
)

// ---------------------------------------------------------------------------
// Unit tests — retained after audit
//
// These test def-node DAG construction and forEach-def error handling.
// The DAG construction for def nodes (cycle detection, multi-hop chains)
// uses a different code path than template↔template dependencies.
// The forEach-def error path (scope-not-published-on-error) has no e2e
// surface — a regression would silently expose partial arrays to dependents.
// ---------------------------------------------------------------------------

func TestDefinesCycleDetection(t *testing.T) {
	nodes := []graphpkg.Node{
		node(graphpkg.Node{
			ID:  "naming",
			Def: map[string]any{"prefix": "${svc.metadata.name}"},
		}, graphpkg.NodeTypeDef),
		node(graphpkg.Node{
			ID: "svc",
			Template: map[string]any{
				"apiVersion": "v1",
				"kind":       "Service",
				"metadata":   map[string]any{"name": "${naming.prefix + '-svc'}"},
			},
		}, graphpkg.NodeTypeTemplate),
	}
	_, err := dagpkg.BuildDAG(nodes, nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, compiler.ErrDependencyError)
}

func TestDefinesChain(t *testing.T) {
	nodes := []graphpkg.Node{
		node(graphpkg.Node{
			ID:  "a",
			Def: map[string]any{"prefix": "app"},
		}, graphpkg.NodeTypeDef),
		node(graphpkg.Node{
			ID:  "b",
			Def: map[string]any{"fullName": "${a.prefix + '-service'}"},
		}, graphpkg.NodeTypeDef),
		node(graphpkg.Node{
			ID: "c",
			Template: map[string]any{
				"apiVersion": "v1",
				"kind":       "Service",
				"metadata":   map[string]any{"name": "${b.fullName}"},
			},
		}, graphpkg.NodeTypeTemplate),
	}
	dag, err := dagpkg.BuildDAG(nodes, nil)
	require.NoError(t, err)

	assert.Equal(t, 3, len(dag.Levels))
	assert.Contains(t, dag.Levels[0], dag.Index["a"])
	assert.Contains(t, dag.Levels[1], dag.Index["b"])
	assert.Contains(t, dag.Levels[2], dag.Index["c"])
	assert.True(t, dag.Nodes[dag.Index["b"]].Dependencies["a"])
	assert.True(t, dag.Nodes[dag.Index["c"]].Dependencies["b"])
}

// ---------------------------------------------------------------------------
// forEach defines — error handling and scope isolation
//
// compileDefinesSpec is a test helper that compiles a GraphSpec and returns
// an evaluator ready for reconcileDefinition calls.
// ---------------------------------------------------------------------------

func compileDefinesSpec(t *testing.T, spec *graphpkg.GraphSpec) *evaluator {
	t.Helper()
	compiled, err := compiler.CompileGraphSpec(spec, nil)
	require.NoError(t, err)
	return &evaluator{
		compiled: compiled,
		scope:    map[string]any{},
	}
}

func TestDefinesForEachReconcile(t *testing.T) {
	r := &GraphReconciler{}
	ctx := context.Background()

	t.Run("per-item CEL error is collected", func(t *testing.T) {
		spec := &graphpkg.GraphSpec{Nodes: []graphpkg.Node{
			node(graphpkg.Node{ID: "upstream", Template: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]any{"name": "cfg"},
			}}, graphpkg.NodeTypeTemplate),
			node(graphpkg.Node{
				ID:      "items",
				ForEach: &graphpkg.ForEachBinding{VarName: "w", Expr: "${['a', 'b']}"},
				Def:     map[string]any{"val": "${upstream.data.key}"},
			}, graphpkg.NodeTypeDef),
		}}
		compiled, err := compiler.CompileGraphSpec(spec, nil)
		require.NoError(t, err)

		eval := &evaluator{
			compiled:          compiled,
			scope:             map[string]any{},
			forEachNewScope:   map[string]map[string]any{},
			forEachNewKeys:    map[string]map[string][]string{},
			forEachNewHashes:  map[string]map[string]string{},
			forEachNewItems:   map[string][]any{},
			forEachPrevItems:  map[string][]any{},
			forEachPrevScope:  map[string]map[string]any{},
			forEachPrevKeys:   map[string]map[string][]string{},
			forEachPrevHashes: map[string]map[string]string{},
		}

		graph := &unstructured.Unstructured{Object: map[string]any{
			"metadata": map[string]any{"name": "test", "namespace": "default"},
		}}

		_, err = r.reconcileForEach(ctx, graph, spec.Nodes[1], eval, nil, false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "forEach defines items")
	})

	// Regression: forEach parent scope must NOT be published when any child
	// errored. Before the fix, eval.scope[parent] = partial allApplied was
	// set BEFORE the error check — dependents could observe a partial array
	// if the coordinator's Block path was ever weakened.
	t.Run("ForEach_RegressionScopeNotPublishedOnChildError", func(t *testing.T) {
		spec := &graphpkg.GraphSpec{Nodes: []graphpkg.Node{
			node(graphpkg.Node{ID: "upstream", Template: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]any{"name": "cfg"},
			}}, graphpkg.NodeTypeTemplate),
			node(graphpkg.Node{
				ID:      "items",
				ForEach: &graphpkg.ForEachBinding{VarName: "w", Expr: "${['a', 'b']}"},
				Def:     map[string]any{"val": "${upstream.data.key}"},
			}, graphpkg.NodeTypeDef),
		}}
		compiled, err := compiler.CompileGraphSpec(spec, nil)
		require.NoError(t, err)

		eval := &evaluator{
			compiled:          compiled,
			scope:             map[string]any{},
			forEachNewScope:   map[string]map[string]any{},
			forEachNewKeys:    map[string]map[string][]string{},
			forEachNewHashes:  map[string]map[string]string{},
			forEachNewItems:   map[string][]any{},
			forEachPrevItems:  map[string][]any{},
			forEachPrevScope:  map[string]map[string]any{},
			forEachPrevKeys:   map[string]map[string][]string{},
			forEachPrevHashes: map[string]map[string]string{},
		}

		graph := &unstructured.Unstructured{Object: map[string]any{
			"metadata": map[string]any{"name": "test", "namespace": "default"},
		}}

		_, err = r.reconcileForEach(ctx, graph, spec.Nodes[1], eval, nil, false)
		require.Error(t, err, "forEach with failing child templates must return an error")

		_, published := eval.scope["items"]
		assert.False(t, published,
			"forEach parent scope must not be published when any child errored")
	})
}
