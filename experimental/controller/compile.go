// compile.go orchestrates compilation — turning a GraphRevision's spec into
// a compiled graph artifact, managing schema staleness and dynamic GVK
// resolution. Compilation itself lives in the compiler package; this file
// manages the controller-side instance state.
//
// Instance state is keyed by namespace/revision-name (per-Graph mutable state).
package graphcontroller

import (
	"context"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ellistarn/kro/experimental/controller/compiler"
	dagpkg "github.com/ellistarn/kro/experimental/controller/dag"
	graphpkg "github.com/ellistarn/kro/experimental/controller/graph"
)

// compileRevision parses a GraphRevision's spec, compiles it, and returns the
// compiled graph and per-instance state. Always recompiles; instance state is
// preserved across reconciles for correctness fields (previousAppliedKeys, etc.).
func (r *GraphReconciler) compileRevision(ctx context.Context, namespace string, revision *unstructured.Unstructured) (*graphpkg.GraphSpec, *instanceState, error) {
	instanceKey := revision.GetNamespace() + "/" + revision.GetName()

	// Retrieve existing instance state (may have resolvedDynamicGVKs from
	// a previous reconcile). These serve as hints for schema resolution of
	// dynamic GVK nodes on subsequent compilations.
	existing := r.Caches.get(instanceKey)
	var dynamicGVKHints map[string]schema.GroupVersionKind
	if existing != nil {
		dynamicGVKHints = existing.resolvedDynamicGVKs
	}

	// Parse spec, resolve types, compile, assemble DAG.
	spec, err := extractRevisionSpec(revision)
	if err != nil {
		return nil, nil, err
	}

	// Resolve types. Then pre-populate schemas for dynamic GVK nodes
	// whose GVK was resolved on a previous reconcile. The compiler
	// is unaware of dynamic GVKs — it just sees pre-populated types.
	typeInfo := compiler.ResolveNodeTypes(spec.Nodes, r.SchemaResolver)
	if r.SchemaResolver != nil && len(dynamicGVKHints) > 0 {
		for _, nodeID := range typeInfo.DynamicGVKNodes {
			if gvk, ok := dynamicGVKHints[nodeID]; ok {
				typeInfo.PrePopulateSchema(nodeID, gvk, r.SchemaResolver)
			}
		}
	}
	compiled, err := compiler.CompileGraphSpec(spec, typeInfo)
	if err != nil {
		return nil, nil, err
	}
	// Assemble a per-instance DAG from the shared topology and this
	// instance's node specs.
	dag := dagpkg.AssembleDAG(spec.Nodes, compiled.Topology)

	// If existing state exists, update it in-place (preserving correctness
	// fields like previousScope, previousKeys, etc.).
	if existing != nil {
		existing.compiled = compiled
		existing.dag = dag
		// Reset runtime caches that should not survive recompilation.
		existing.forEach = &forEachCarryForward{
			itemScope: map[string]map[string]any{},
			itemKeys:  map[string]map[string][]Applied{},
		}
		existing.deferredPruneKeys = nil
		r.Caches.set(instanceKey, existing)
		return spec, existing, nil
	}

	// New instance — create fresh mutable state.
	state := newInstanceState(compiled)
	state.dag = dag
	r.Caches.set(instanceKey, state)
	return spec, state, nil
}
