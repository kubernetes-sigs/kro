package graphcontroller

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// ---------------------------------------------------------------------------
// Unit tests — definition node reference detection and DAG integration
// ---------------------------------------------------------------------------

func TestDefinesReference(t *testing.T) {
	t.Run("no apiVersion no kind returns ReferenceDefinition", func(t *testing.T) {
		n := Node{
			ID:       "naming",
			Template: map[string]any{"prefix": "${spec.name}"},
		}
		assert.Equal(t, ReferenceDefinition, n.Reference())
	})

	t.Run("empty template returns ReferenceUnresolved", func(t *testing.T) {
		n := Node{ID: "empty", Template: map[string]any{}}
		assert.Equal(t, ReferenceUnresolved, n.Reference())
	})

	t.Run("nil template returns ReferenceUnresolved", func(t *testing.T) {
		n := Node{ID: "nil"}
		assert.Equal(t, ReferenceUnresolved, n.Reference())
	})

	t.Run("with apiVersion and kind returns non-Definition", func(t *testing.T) {
		n := Node{
			ID: "cfg",
			Template: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]any{"name": "cfg"},
				"data":       map[string]any{"k": "v"},
			},
		}
		assert.NotEqual(t, ReferenceDefinition, n.Reference())
	})
}

func TestDefinesReferenceString(t *testing.T) {
	assert.Equal(t, "definition", ReferenceDefinition.String())
}

func TestDefinesDAGDependencies(t *testing.T) {
	nodes := []Node{
		{
			ID: "deploy",
			Template: map[string]any{
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
				"metadata":   map[string]any{"name": "my-app"},
				"spec":       map[string]any{"replicas": 1},
			},
		},
		{
			ID: "naming",
			Template: map[string]any{
				"fullName": "${deploy.metadata.name + '-' + spec.env}",
			},
		},
		{
			ID: "svc",
			Template: map[string]any{
				"apiVersion": "v1",
				"kind":       "Service",
				"metadata":   map[string]any{"name": "${naming.fullName + '-svc'}"},
			},
		},
	}
	dag, err := BuildDAG(nodes, nil)
	require.NoError(t, err)

	namingNode := dag.Nodes[dag.Index["naming"]]
	assert.True(t, namingNode.Dependencies["deploy"], "naming should depend on deploy")

	svcNode := dag.Nodes[dag.Index["svc"]]
	assert.True(t, svcNode.Dependencies["naming"], "svc should depend on naming")

	assert.Equal(t, 3, len(dag.Levels))
	assert.Contains(t, dag.Levels[0], dag.Index["deploy"])
	assert.Contains(t, dag.Levels[1], dag.Index["naming"])
	assert.Contains(t, dag.Levels[2], dag.Index["svc"])
}

func TestDefinesCycleDetection(t *testing.T) {
	nodes := []Node{
		{
			ID:       "naming",
			Template: map[string]any{"prefix": "${svc.metadata.name}"},
		},
		{
			ID: "svc",
			Template: map[string]any{
				"apiVersion": "v1",
				"kind":       "Service",
				"metadata":   map[string]any{"name": "${naming.prefix + '-svc'}"},
			},
		},
	}
	_, err := BuildDAG(nodes, nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrDependencyError)
}

func TestDefinesChain(t *testing.T) {
	nodes := []Node{
		{
			ID:       "a",
			Template: map[string]any{"prefix": "app"},
		},
		{
			ID:       "b",
			Template: map[string]any{"fullName": "${a.prefix + '-service'}"},
		},
		{
			ID: "c",
			Template: map[string]any{
				"apiVersion": "v1",
				"kind":       "Service",
				"metadata":   map[string]any{"name": "${b.fullName}"},
			},
		},
	}
	dag, err := BuildDAG(nodes, nil)
	require.NoError(t, err)

	assert.Equal(t, 3, len(dag.Levels))
	assert.Contains(t, dag.Levels[0], dag.Index["a"])
	assert.Contains(t, dag.Levels[1], dag.Index["b"])
	assert.Contains(t, dag.Levels[2], dag.Index["c"])
	assert.True(t, dag.Nodes[dag.Index["b"]].Dependencies["a"])
	assert.True(t, dag.Nodes[dag.Index["c"]].Dependencies["b"])
}

// ---------------------------------------------------------------------------
// Unit tests — reconcileDefinition and forEach defines code paths
// ---------------------------------------------------------------------------

// compileDefinesSpec is a test helper that compiles a GraphSpec and returns an
// evaluator ready for reconcileDefinition calls. Fails the test on compile error.
func compileDefinesSpec(t *testing.T, spec *GraphSpec) *evaluator {
	t.Helper()
	compiled, err := compileGraphSpec(spec)
	require.NoError(t, err)
	return &evaluator{
		compiled: compiled,
		scope:    map[string]any{},
	}
}

func TestDefinesReconcile(t *testing.T) {
	r := &GraphReconciler{}
	ctx := context.Background()

	t.Run("literals enter scope", func(t *testing.T) {
		spec := &GraphSpec{Nodes: []Node{
			{ID: "cfg", Template: map[string]any{"region": "us-west-2", "env": "prod"}},
		}}
		eval := compileDefinesSpec(t, spec)

		err := r.reconcileDefinition(ctx, spec.Nodes[0], eval)
		require.NoError(t, err)

		result, ok := eval.scope["cfg"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "us-west-2", result["region"])
		assert.Equal(t, "prod", result["env"])
	})

	t.Run("CEL expressions evaluate against scope", func(t *testing.T) {
		spec := &GraphSpec{Nodes: []Node{
			{ID: "upstream", Template: map[string]any{"name": "myapp"}},
			{ID: "derived", Template: map[string]any{"full": "${upstream.name + '-svc'}"}},
		}}
		eval := compileDefinesSpec(t, spec)

		// Populate upstream in scope first (simulates DAG walk order).
		eval.scope["upstream"] = map[string]any{"name": "myapp"}

		err := r.reconcileDefinition(ctx, spec.Nodes[1], eval)
		require.NoError(t, err)

		result, ok := eval.scope["derived"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "myapp-svc", result["full"])
	})

	t.Run("readyWhen satisfied marks ready", func(t *testing.T) {
		spec := &GraphSpec{Nodes: []Node{
			{
				ID:        "cfg",
				Template:  map[string]any{"count": "3"},
				ReadyWhen: []string{"${cfg.count == '3'}"},
			},
		}}
		eval := compileDefinesSpec(t, spec)

		_, err := r.reconcileNode(ctx, nil, spec.Nodes[0], ReferenceDefinition, eval, nil, false)
		require.NoError(t, err)

		result, ok := eval.scope["cfg"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, true, result["__ready"])
	})

	t.Run("readyWhen unsatisfied returns error and marks not ready", func(t *testing.T) {
		spec := &GraphSpec{Nodes: []Node{
			{
				ID:        "cfg",
				Template:  map[string]any{"count": "0"},
				ReadyWhen: []string{"${cfg.count == '3'}"},
			},
		}}
		eval := compileDefinesSpec(t, spec)

		_, err := r.reconcileNode(ctx, nil, spec.Nodes[0], ReferenceDefinition, eval, nil, false)
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrWaitingForReadiness))

		result, ok := eval.scope["cfg"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, false, result["__ready"])
	})

	t.Run("CEL eval failure returns error", func(t *testing.T) {
		// Reference a declared node that isn't in scope — triggers runtime
		// eval failure (not compile failure).
		spec := &GraphSpec{Nodes: []Node{
			{ID: "upstream", Template: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]any{"name": "cfg"},
			}},
			{ID: "bad", Template: map[string]any{"val": "${upstream.data.key}"}},
		}}
		eval := compileDefinesSpec(t, spec)
		// upstream is declared but NOT populated in scope — eval fails at runtime.

		err := r.reconcileDefinition(ctx, spec.Nodes[1], eval)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "definition bad")
	})
}

func TestDefinesForEachReconcile(t *testing.T) {
	r := &GraphReconciler{}
	ctx := context.Background()

	t.Run("per-item CEL error is collected", func(t *testing.T) {
		// forEach over a list where the template references a declared but
		// absent node. The forEach branch should collect the error per-item.
		spec := &GraphSpec{Nodes: []Node{
			{ID: "upstream", Template: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]any{"name": "cfg"},
			}},
			{
				ID:       "items",
				ForEach:  map[string]string{"w": "${['a', 'b']}"},
				Template: map[string]any{"val": "${upstream.data.key}"},
			},
		}}
		compiled, err := compileGraphSpec(spec)
		require.NoError(t, err)

		eval := &evaluator{
			compiled:         compiled,
			scope:            map[string]any{},
			forEachNewScope:  map[string]map[string]any{},
			forEachNewKeys:   map[string]map[string][]string{},
			forEachNewItems:  map[string][]any{},
			forEachPrevItems: map[string][]any{},
			forEachPrevScope: map[string]map[string]any{},
			forEachPrevKeys:  map[string]map[string][]string{},
		}

		graph := &unstructured.Unstructured{Object: map[string]any{
			"metadata": map[string]any{"name": "test", "namespace": "default"},
		}}

		_, err = r.reconcileForEach(ctx, graph, spec.Nodes[1], eval, nil, false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "forEach defines items")
	})
}
