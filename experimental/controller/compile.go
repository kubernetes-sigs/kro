// compile.go orchestrates compilation caching — turning a GraphRevision's
// spec into a compiled graph artifact, managing schema staleness and dynamic
// GVK resolution. Compilation itself lives in the compiler package; this
// file manages the controller-side caching layer.
//
// Instance state is keyed by namespace/revision-name (per-Graph mutable state).
// Each reconcile checks whether the existing compilation is still fresh
// (generation-based staleness + dynamic GVK resolution).
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
// compiled graph and per-instance state. Uses generation-based staleness to
// avoid redundant recompilation.
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

	// Fast path: instance state already exists with a valid compilation
	// (steady-state reconcile). Check generation-based staleness and
	// whether dynamic GVK schemas can now be resolved.
	if existing != nil && existing.compiled != nil {
		genFresh := r.SchemaGen == nil || existing.compiled.TypeCacheGen >= r.SchemaGen.Generation()
		schemasFresh := !dynamicGVKSchemasStale(existing.compiled, dynamicGVKHints)
		if genFresh && schemasFresh {
			return existing.spec, existing, nil
		}
		// Stale — fall through to recompile.
	}

	// Slow path: parse spec, resolve types, compile, assemble DAG.
	spec, err := extractRevisionSpec(revision)
	if err != nil {
		return nil, nil, err
	}

	var cacheGen int64
	if r.SchemaGen != nil {
		cacheGen = r.SchemaGen.Generation()
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
	compiled.TypeCacheGen = cacheGen

	// Assemble a per-instance DAG from the shared topology and this
	// instance's node specs.
	dag := dagpkg.AssembleDAG(spec.Nodes, compiled.Topology)

	// If existing state exists, update it in-place (preserving correctness
	// fields like previousScope, previousKeys, etc.).
	if existing != nil {
		existing.compiled = compiled
		existing.spec = spec
		existing.dag = dag
		// Reset runtime caches that should not survive recompilation.
		existing.forEach = &forEachCarryForward{
			items:     map[string][]any{},
			itemScope: map[string]map[string]any{},
			itemKeys:  map[string]map[string][]string{},
		}
		existing.deferredPruneKeys = nil
		r.Caches.set(instanceKey, existing)
		return spec, existing, nil
	}

	// New instance — create fresh mutable state.
	state := newInstanceState(compiled)
	state.spec = spec
	state.dag = dag
	r.Caches.set(instanceKey, state)
	return spec, state, nil
}

// dynamicGVKSchemasStale reports whether the artifact has dynamic GVK nodes
// whose schemas can now be resolved (the instance has resolved GVKs from a
// previous reconcile, but the artifact was compiled without those schemas).
func dynamicGVKSchemasStale(compiled *compiler.CompiledGraph, resolvedGVKs map[string]schema.GroupVersionKind) bool {
	if len(compiled.DynamicGVKNodes) == 0 || len(resolvedGVKs) == 0 {
		return false
	}
	for _, nodeID := range compiled.DynamicGVKNodes {
		if _, resolved := resolvedGVKs[nodeID]; !resolved {
			continue
		}
		if _, hasSchema := compiled.ResourceSchemas[nodeID]; !hasSchema {
			return true // resolved GVK available but artifact compiled without schema
		}
	}
	return false
}
