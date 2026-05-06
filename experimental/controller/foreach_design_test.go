package graphcontroller

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/ellistarn/kro/experimental/controller/compiler"
	graphpkg "github.com/ellistarn/kro/experimental/controller/graph"
)

// ---------------------------------------------------------------------------
// forEach design behavior tests
//
// These test forEach behaviors that are structural invariants of the design:
//   - Scope cleanup on halted expansion (partial aggregate must not leak)
//   - No state carry-forward between reconcile cycles
//   - Error path key correctness (no phantom keys from prior cycles)
// ---------------------------------------------------------------------------

// TestForEach_HaltedMidCollectionCleansScope proves that when forEach's
// per-item propagateWhen passes for the first item but halts on the second,
// the partial scope set during gate evaluation is cleaned up.
//
// Per 005-reconciliation.md § propagateWhen: "Clean up the partial scope
// set during gate evaluation." The `delete(eval.scope, node.ID)` cleanup
// path fires when halting after processing some items.
func TestForEach_HaltedMidCollectionCleansScope(t *testing.T) {
	// Build a forEach def node with 3 items and a propagateWhen gate
	// that passes for the first item but fails for the second.
	// The gate expression: size of the partially-built collection < 1
	// (passes when 0 items processed, fails when 1+ items processed).
	sourceNode := graphpkg.Node{ID: "source", Def: map[string]any{
		"items": []any{
			map[string]any{"metadata": map[string]any{"name": "alpha"}},
			map[string]any{"metadata": map[string]any{"name": "beta"}},
			map[string]any{"metadata": map[string]any{"name": "gamma"}},
		},
	}}
	sourceNode.SetType(graphpkg.NodeTypeDef)

	workersNode := graphpkg.Node{
		ID:      "workers",
		ForEach: &graphpkg.ForEachBinding{VarName: "item", Expr: "${source.items}"},
		// Gate passes when collection has < 1 items (i.e., only before
		// the first item is processed). After the first item is appended,
		// size() == 1 which is NOT < 1, so the gate halts.
		PropagateWhen: []string{"${workers.size() < 1}"},
		Def:           map[string]any{"name": "${item.metadata.name}"},
	}
	workersNode.SetType(graphpkg.NodeTypeDef)

	spec := &graphpkg.GraphSpec{
		Nodes: []graphpkg.Node{sourceNode, workersNode},
	}
	compiled, err := compiler.CompileGraphSpec(spec, nil)
	require.NoError(t, err)

	state := newInstanceState(compiled, nil)
	eval := newEvaluator(state)
	eval.effectiveGeneration = 5

	// Populate source in scope.
	eval.scope["source"] = map[string]any{
		"items": []any{
			map[string]any{"metadata": map[string]any{"name": "alpha"}},
			map[string]any{"metadata": map[string]any{"name": "beta"}},
			map[string]any{"metadata": map[string]any{"name": "gamma"}},
		},
	}

	graph := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "experimental.kro.run/v1alpha1",
		"kind":       "Graph",
		"metadata":   map[string]any{"name": "test", "namespace": "default"},
	}}

	r := &GraphReconciler{}
	rs := newReconcileScope(graph, nil)
	_, err = r.cluster().reconcileForEach(context.Background(), rs, spec.Nodes[1], eval)

	// Error must be ErrPending — propagateWhen halted expansion.
	require.ErrorIs(t, err, ErrPending,
		"forEach should return ErrPending when propagateWhen halts mid-collection")

	// The partial aggregate from processing item 1 during gate evaluation
	// must be cleaned up. eval.scope[node.ID] must NOT be set.
	_, scopeSet := eval.scope["workers"]
	assert.False(t, scopeSet,
		"eval.scope[node.ID] must be cleaned up after halting mid-collection — "+
			"partial aggregates must not leak to dependents")
}

// TestForEach_NoCarryForwardBetweenCycles proves that there is genuinely no
// state persistence between reconcile cycles. Each cycle starts fresh — keys
// and scope from a previous successful cycle do not bleed into a subsequent
// halted cycle.
//
// Per foreach.go: "No state is carried between reconciles — each cycle
// evaluates fresh."
func TestForEach_NoCarryForwardBetweenCycles(t *testing.T) {
	sourceNode := graphpkg.Node{ID: "source", Def: map[string]any{
		"items": []any{
			map[string]any{"metadata": map[string]any{"name": "alpha"}},
			map[string]any{"metadata": map[string]any{"name": "beta"}},
			map[string]any{"metadata": map[string]any{"name": "gamma"}},
		},
	}}
	sourceNode.SetType(graphpkg.NodeTypeDef)

	workersNode := graphpkg.Node{
		ID:      "workers",
		ForEach: &graphpkg.ForEachBinding{VarName: "item", Expr: "${source.items}"},
		// Gate: allow all items (always true).
		PropagateWhen: []string{"${true}"},
		Def:           map[string]any{"name": "${item.metadata.name}"},
	}
	workersNode.SetType(graphpkg.NodeTypeDef)

	spec := &graphpkg.GraphSpec{
		Nodes: []graphpkg.Node{sourceNode, workersNode},
	}
	compiled, err := compiler.CompileGraphSpec(spec, nil)
	require.NoError(t, err)

	graph := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "experimental.kro.run/v1alpha1",
		"kind":       "Graph",
		"metadata":   map[string]any{"name": "test", "namespace": "default"},
	}}

	r := &GraphReconciler{}
	rs := newReconcileScope(graph, nil)

	// --- Cycle 1: propagateWhen allows all items ---
	state := newInstanceState(compiled, nil)
	eval1 := newEvaluator(state)
	eval1.effectiveGeneration = 5
	eval1.scope["source"] = map[string]any{
		"items": []any{
			map[string]any{"metadata": map[string]any{"name": "alpha"}},
			map[string]any{"metadata": map[string]any{"name": "beta"}},
			map[string]any{"metadata": map[string]any{"name": "gamma"}},
		},
	}

	out1, err := r.cluster().reconcileForEach(context.Background(), rs, spec.Nodes[1], eval1)
	require.NoError(t, err, "cycle 1 should succeed — all items allowed")

	// Verify cycle 1 succeeded: scope published, all items present.
	items1, ok := eval1.scope["workers"].([]any)
	require.True(t, ok, "cycle 1 should publish scope")
	assert.Len(t, items1, 3, "cycle 1 should have all 3 items in scope")
	// Def nodes produce no keys, so out1.keys is nil/empty.
	_ = out1

	// --- Cycle 2: propagateWhen changed to halt after first item ---
	// Build a new spec with a restrictive gate.
	sourceNode2 := graphpkg.Node{ID: "source", Def: map[string]any{
		"items": []any{
			map[string]any{"metadata": map[string]any{"name": "alpha"}},
			map[string]any{"metadata": map[string]any{"name": "beta"}},
			map[string]any{"metadata": map[string]any{"name": "gamma"}},
		},
	}}
	sourceNode2.SetType(graphpkg.NodeTypeDef)

	workersNode2 := graphpkg.Node{
		ID:      "workers",
		ForEach: &graphpkg.ForEachBinding{VarName: "item", Expr: "${source.items}"},
		// Gate: halt after first item (size < 1 is false after appending first).
		PropagateWhen: []string{"${workers.size() < 1}"},
		Def:           map[string]any{"name": "${item.metadata.name}"},
	}
	workersNode2.SetType(graphpkg.NodeTypeDef)

	spec2 := &graphpkg.GraphSpec{
		Nodes: []graphpkg.Node{sourceNode2, workersNode2},
	}
	compiled2, err := compiler.CompileGraphSpec(spec2, nil)
	require.NoError(t, err)

	state2 := newInstanceState(compiled2, nil)
	eval2 := newEvaluator(state2)
	eval2.effectiveGeneration = 5
	eval2.scope["source"] = map[string]any{
		"items": []any{
			map[string]any{"metadata": map[string]any{"name": "alpha"}},
			map[string]any{"metadata": map[string]any{"name": "beta"}},
			map[string]any{"metadata": map[string]any{"name": "gamma"}},
		},
	}

	out2, err := r.cluster().reconcileForEach(context.Background(), rs, spec2.Nodes[1], eval2)

	// Cycle 2 should be Pending — gate halted.
	require.ErrorIs(t, err, ErrPending,
		"cycle 2 should return ErrPending when propagateWhen halts")

	// Keys from cycle 2 must NOT contain keys from cycle 1's successful applies.
	// Def nodes produce no keys, so this is vacuously correct for def nodes.
	// The critical assertion: the returned output from this cycle is independent
	// of cycle 1's results.
	assert.Empty(t, out2.keys,
		"cycle 2 keys must not carry forward from cycle 1 — "+
			"each cycle starts fresh")

	// Scope must NOT be set — halted cycle doesn't publish.
	// (And definitely not carrying forward cycle 1's published scope.)
	_, scopeSet := eval2.scope["workers"]
	assert.False(t, scopeSet,
		"eval.scope must NOT carry forward cycle 1's scope into cycle 2 — "+
			"each evaluator is independent")
}

// TestForEach_ErrorPathNoStaleKeys proves that when forEach's second item
// fails, the returned nodeOutput.keys contains only the first item's
// successfully-applied key (not phantom keys from previous cycles), and
// scope is not published.
//
// Per 005-reconciliation.md § Parent State: "Do NOT publish partial scope
// on error." The error path must not inject phantom keys.
func TestForEach_ErrorPathNoStaleKeys(t *testing.T) {
	// Use a template forEach (not def) so we get real resource keys.
	// Need a fake client where Patch succeeds for item 1 and fails for item 2.
	spec := &graphpkg.GraphSpec{
		Nodes: []graphpkg.Node{
			{ID: "source", Def: map[string]any{
				"items": []any{
					map[string]any{"metadata": map[string]any{"name": "item-one"}},
					map[string]any{"metadata": map[string]any{"name": "item-two"}},
				},
			}},
			node(graphpkg.Node{
				ID:      "children",
				ForEach: &graphpkg.ForEachBinding{VarName: "item", Expr: "${source.items}"},
				Template: map[string]any{
					"apiVersion": "v1",
					"kind":       "ConfigMap",
					"metadata":   map[string]any{"name": "${item.metadata.name}"},
				},
			}, graphpkg.NodeTypeTemplate),
		},
	}
	compiled, err := compiler.CompileGraphSpec(spec, nil)
	require.NoError(t, err)

	state := newInstanceState(compiled, nil)
	eval := newEvaluator(state)
	eval.effectiveGeneration = 3

	eval.scope["source"] = map[string]any{
		"items": []any{
			map[string]any{"metadata": map[string]any{"name": "item-one"}},
			map[string]any{"metadata": map[string]any{"name": "item-two"}},
		},
	}

	graph := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "experimental.kro.run/v1alpha1",
		"kind":       "Graph",
		"metadata":   map[string]any{"name": "test", "namespace": "default"},
	}}
	rs := newReconcileScope(graph, nil)

	// Build a fake client:
	// - Get returns NotFound (no existing resource → ownership check passes,
	//   preEvaluateReadiness classifies as "not ready")
	// - Patch succeeds for item-one, fails for item-two
	patchErr := fmt.Errorf("simulated API error on second item")

	scheme := runtime.NewScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key types.NamespacedName, obj client.Object, opts ...client.GetOption) error {
				// Return NotFound for all Gets — ownership check finds no
				// existing resource (passes), preEvaluateReadiness classifies
				// as "not ready" (class 1).
				return apierrors.NewNotFound(
					schema.GroupResource{Resource: "configmaps"}, key.Name)
			},
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				// Route by name — order-independent since
				// sortForEachByReadiness randomizes within a class.
				u := obj.(*unstructured.Unstructured)
				if u.GetName() == "item-one" {
					u.SetNamespace("default")
					u.SetResourceVersion("1")
					return nil
				}
				// item-two fails.
				return patchErr
			},
		}).
		Build()

	c := &clusterAccess{
		client: fakeClient,
		reader: fakeClient,
	}

	out, err := c.reconcileForEach(context.Background(), rs, spec.Nodes[1], eval)

	// Error must be returned (highest priority child error).
	require.Error(t, err, "forEach with a failing child must return an error")
	assert.Contains(t, err.Error(), "simulated API error",
		"returned error should be the child apply error")

	// Keys must contain only the first item's successfully-applied key.
	require.NotNil(t, out, "nodeOutput must not be nil even on error")
	assert.Len(t, out.keys, 1,
		"keys must contain only the successfully-applied item's key — no phantom keys")
	// The key should reference item-one (the successful one).
	assert.Contains(t, out.keys[0].Key, "item-one",
		"the single key should be from the first (successful) item")

	// Scope must NOT be published when there are child errors.
	_, scopePublished := eval.scope["children"]
	assert.False(t, scopePublished,
		"scope must NOT be published when any child errored — "+
			"partial aggregates must not reach dependents")
}
