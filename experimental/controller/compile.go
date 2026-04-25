// compile.go orchestrates compilation caching — turning a GraphRevision's
// spec into a compiled graph artifact, managing cache keys, schema staleness,
// and dynamic GVK resolution. Compilation itself lives in the compiler
// package; this file manages the controller-side caching layer.
//
// Two cache levels:
//   - Instance state: keyed by namespace/revision-name (per-Graph mutable state)
//   - Compiled graph: keyed by compilation key (shared across structurally identical specs)
//
// For N identical child graphs (common in nested graph patterns with forEach),
// this means 1 compilation + N-1 key lookups instead of N compilations.
package graphcontroller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubernetes-sigs/kro/experimental/controller/compiler"
	dagpkg "github.com/kubernetes-sigs/kro/experimental/controller/dag"
	graphpkg "github.com/kubernetes-sigs/kro/experimental/controller/graph"
)

// childRefResolution holds the output of resolveChildRefHints — both the
// content hashes (for the compilation key) and the resolved objects (to
// pass to precompileExpressionChildGraphs, avoiding redundant API reads).
type childRefResolution struct {
	hashes  map[string]string         // nodeID → content hash
	objects map[string]map[string]any // nodeID → resolved object
}

// resolveChildRefHints resolves ref targets referenced by expression-valued
// child spec.nodes and returns content hashes and resolved objects. The hashes
// feed into the compilation key so that instances referencing different ref
// targets (e.g., different RGDs) get separate compiled artifacts. The objects
// are passed to precompileExpressionChildGraphs to avoid redundant API reads.
//
// Returns nil when no expression-valued child specs exist or when ref
// targets cannot be resolved (best effort — first compile with dyn types,
// narrowed when refs become resolvable).
func (r *GraphReconciler) resolveChildRefHints(ctx context.Context, namespace string, spec *graphpkg.GraphSpec) *childRefResolution {
	var res *childRefResolution
	for _, node := range spec.Nodes {
		if node.ForEach == nil {
			continue
		}
		body := node.Payload()
		if body == nil {
			continue
		}
		apiVersion, _ := body["apiVersion"].(string)
		kind, _ := body["kind"].(string)
		if !compiler.IsGraphCRLiteral(apiVersion, kind) {
			continue
		}
		specMap, ok := body["spec"].(map[string]any)
		if !ok {
			continue
		}
		nodesRaw, ok := specMap["nodes"]
		if !ok {
			continue
		}
		nodesExpr, ok := nodesRaw.(string)
		if !ok {
			continue
		}
		dollars, innerExpr, _, _ := graphpkg.FindExpr(nodesExpr, 0)
		if innerExpr == "" || len(dollars) != 1 {
			continue
		}
		for _, n := range spec.Nodes {
			if n.Ref == nil {
				continue
			}
			if !strings.Contains(innerExpr, n.ID+".") && !strings.Contains(innerExpr, n.ID+"[") {
				continue
			}
			obj, err := r.resolveRefForPrecompilation(ctx, namespace, n)
			if err != nil {
				// Can't resolve — omit from hints. Compilation proceeds
				// with the structural key only (permissive).
				continue
			}
			if res == nil {
				res = &childRefResolution{
					hashes:  make(map[string]string),
					objects: make(map[string]map[string]any),
				}
			}
			res.objects[n.ID] = obj
			// Use the ref target's UID + generation as the content
			// hash. Generation only increments on spec changes, which
			// is what determines the child graph structure. UID
			// distinguishes different objects with the same name.
			md, _ := obj["metadata"].(map[string]any)
			uid, _ := md["uid"].(string)
			gen, _ := md["generation"].(int64)
			res.hashes[n.ID] = fmt.Sprintf("%s@%d", uid, gen)
		}
	}
	return res
}

// precompileExpressionChildGraphs resolves expression-valued spec.nodes in
// forEach Graph templates by fetching referenced ref node targets from the API
// and evaluating the expression. The resulting child node list is compiled with
// real type information to catch cycles, structural errors, and type errors
// (e.g., forEach over a non-list field) at the parent's compile time.
//
// This extends the pre-compilation mechanism in deferred.go which handles
// literal spec.nodes. When spec.nodes is an expression (e.g., referencing
// schema.spec.resources), deferred.go bails. This function fills that gap by
// resolving the ref targets that the expression depends on. Because it runs
// after the parent has created its CRDs, the schema resolver can look up the
// child's ref node types — enabling full type checking including forEach
// list-type validation (Phase 4b).
//
// resolvedRefs provides pre-resolved ref targets from resolveChildRefHints,
// avoiding redundant API reads. Refs not present in the map are fetched
// directly.
func (r *GraphReconciler) precompileExpressionChildGraphs(ctx context.Context, namespace string, spec *graphpkg.GraphSpec, compiled *compiler.CompiledGraph, resolvedRefs map[string]map[string]any) error {
	for _, node := range spec.Nodes {
		if node.ForEach == nil {
			continue
		}
		body := node.Payload()
		if body == nil {
			continue
		}

		// Check if the body is a Graph CR template.
		apiVersion, _ := body["apiVersion"].(string)
		kind, _ := body["kind"].(string)
		if !compiler.IsGraphCRLiteral(apiVersion, kind) {
			continue
		}

		// Check if spec.nodes is expression-valued.
		specMap, ok := body["spec"].(map[string]any)
		if !ok {
			continue
		}
		nodesRaw, ok := specMap["nodes"]
		if !ok {
			continue
		}
		nodesExpr, ok := nodesRaw.(string)
		if !ok {
			continue // literal []any — already handled by deferred.go
		}

		// Extract the inner CEL expression from the ${...} wrapper.
		dollars, innerExpr, _, _ := graphpkg.FindExpr(nodesExpr, 0)
		if innerExpr == "" || len(dollars) != 1 {
			continue // not a single-dollar expression, skip
		}

		// Build a scope with resolved ref nodes referenced by the expression.
		scope := map[string]any{
			compiler.ReservedNodeReadyVar: map[string]bool{},
		}
		for _, n := range spec.Nodes {
			if n.Ref == nil {
				continue
			}
			if !strings.Contains(innerExpr, n.ID+".") && !strings.Contains(innerExpr, n.ID+"[") {
				continue
			}
			// Use pre-resolved ref if available, otherwise fetch.
			if obj, ok := resolvedRefs[n.ID]; ok {
				scope[n.ID] = obj
			} else {
				obj, err := r.resolveRefForPrecompilation(ctx, namespace, n)
				if err != nil {
					// Best effort — if we can't resolve, skip pre-compilation.
					return nil
				}
				scope[n.ID] = obj
			}
		}
		if len(scope) == 1 {
			// No refs resolved (only __kroNodeReady in scope) — can't evaluate.
			continue
		}

		// Add a dummy forEach variable. Only metadata fields are needed;
		// these affect resource names but not dependency topology.
		scope[node.ForEach.VarName] = map[string]any{
			"metadata": map[string]any{
				"name":              "__precompile__",
				"namespace":         "default",
				"uid":               "00000000-0000-0000-0000-000000000000",
				"generation":        int64(0),
				"creationTimestamp": "2000-01-01T00:00:00Z",
			},
		}

		// Add remaining scope nodes as empty maps (best effort for expressions
		// that reference non-ref nodes like watchInstances).
		for _, n := range spec.Nodes {
			if _, exists := scope[n.ID]; !exists {
				scope[n.ID] = []any{}
			}
		}

		// Evaluate the spec.nodes expression.
		result, err := compiled.Eval(innerExpr, scope)
		if err != nil {
			// Expression evaluation failed — can't pre-compile. Skip gracefully.
			continue
		}
		nodeList, ok := result.([]any)
		if !ok {
			continue
		}

		// Parse and compile the child graph spec.
		childNodes, err := graphpkg.ParseNodeList(nodeList)
		if err != nil {
			// Parse errors (invalid IDs, duplicates, reserved keywords,
			// forEach conflicts, invalid apiVersion) surface directly.
			return fmt.Errorf("node %q: child graph: %w", node.ID, err)
		}
		childSpec := &graphpkg.GraphSpec{Nodes: childNodes}
		childTypeInfo := compiler.ResolveNodeTypes(childNodes, r.SchemaResolver)
		if _, err := compiler.CompileGraphSpec(childSpec, childTypeInfo); err != nil {
			// All compilation errors surface — cycles, expression
			// validation, type errors, etc. The compiler is the single
			// source of truth for structural correctness.
			return fmt.Errorf("node %q: child graph: %w", node.ID, err)
		}
	}
	return nil
}

// resolveRefForPrecompilation fetches a ref node's target object for use in
// pre-compilation scope. Similar to reconcileRef but without watcher setup
// or scope mutation.
func (r *GraphReconciler) resolveRefForPrecompilation(ctx context.Context, defaultNamespace string, node graphpkg.Node) (map[string]any, error) {
	ref := node.Ref
	gvk := graphpkg.GVKFromMap(ref)
	md, _ := ref["metadata"].(map[string]any)
	name, _ := md["name"].(string)
	namespace, _ := md["namespace"].(string)
	if namespace == "" {
		namespace = defaultNamespace
	}

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	if err := r.apiReader().Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, obj); err != nil {
		return nil, err
	}
	normalized, _ := graphpkg.NormalizeTypes(obj.Object).(map[string]any)
	return normalized, nil
}

// compileRevision parses a GraphRevision's spec, compiles it, and returns the
// compiled graph and per-instance state. Uses two cache levels: instance state
// keyed by namespace/revision-name, and compiled artifacts keyed by structural
// compilation key.
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
		existing.compiled = nil
	}

	// Parse the spec.
	spec, err := extractRevisionSpec(revision)
	if err != nil {
		return nil, nil, err
	}

	// Resolve ref targets referenced by expression-valued child specs.
	// These hashes feed into the compilation key so that instances
	// referencing different ref targets (e.g., different RGDs) produce
	// different keys and compile separately. This follows the same
	// pattern as dynamic GVK hints: runtime state that affects
	// compilation output feeds into the key.
	childRefs := r.resolveChildRefHints(ctx, namespace, spec)
	var childRefHashes map[string]string
	if childRefs != nil {
		childRefHashes = childRefs.hashes
	}

	// Compute the compilation key. The structural key is combined with
	// resolved dynamic GVK hints and child ref content hashes to produce
	// the full cache key. Instances with the same structure, same resolved
	// GVKs, and same ref targets share a compiled artifact.
	// On first reconcile (no hints), all instances share the bootstrap artifact.
	compilationKey := graphpkg.CompilationKeyWithHints(spec.CompilationKey(), dynamicGVKHints, childRefHashes)
	compiled := r.Caches.getCompiled(compilationKey)
	// Validate the cached artifact is not stale (generation check).
	if compiled != nil && r.SchemaGen != nil && compiled.TypeCacheGen < r.SchemaGen.Generation() {
		compiled = nil // stale — recompile
	}
	if compiled == nil {
		// No shared compiled graph or stale — compile from scratch.
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
		compiled, err = compiler.CompileGraphSpec(spec, typeInfo)
		if err != nil {
			return nil, nil, err
		}
		// Override the artifact's key to include hints so it's stored
		// under the hinted key by set(). CompileGraphSpec sets the
		// structural key; we extend it with the same hints used for
		// the cache lookup above.
		compiled.CompilationKey = compilationKey
		// Pre-compile child graphs with expression-valued spec.nodes.
		// On staleness recompilation (CRD installed since last compile),
		// the schema resolver can now resolve child ref node types,
		// enabling forEach list-type validation in the child.
		var resolvedRefs map[string]map[string]any
		if childRefs != nil {
			resolvedRefs = childRefs.objects
		}
		if err := r.precompileExpressionChildGraphs(ctx, namespace, spec, compiled, resolvedRefs); err != nil {
			return nil, nil, err
		}
		compiled.TypeCacheGen = cacheGen
	}

	// Assemble a per-instance DAG from the shared topology and this
	// instance's node specs. The topology (sort order, edges, levels)
	// is a compilation artifact shared across instances; the nodes contain
	// per-instance concrete values.
	dag := dagpkg.AssembleDAG(spec.Nodes, compiled.Topology)

	// Check if this is an evicted instance (compiled was nil'd by
	// evictUnresolved). The per-node mutable state is valid across
	// type-resolution recompilation — only the compiled pointer needs
	// to be replaced and runtime caches reset.
	//
	// Safety invariant: schema resolution does not affect template evaluation.
	// CEL expressions produce identical output for identical inputs regardless
	// of whether the target GVK schema is resolved. If recompilation ever
	// changes template output semantics (not just validation), this in-place
	// update must be gated on a compilation fingerprint that detects the change.
	if existing := r.Caches.get(instanceKey); existing != nil {
		// existing.compiled is nil (evicted). Recompile in-place.
		existing.compiled = compiled
		existing.spec = spec
		existing.dag = dag
		// Reset runtime caches that should not survive recompilation.
		// Per-node state (hashes, scopes, references, resync timers,
		// applied keys) is preserved — node structure is unchanged.
		existing.forEachItems = map[string][]any{}
		existing.forEachItemScope = map[string]map[string]any{}
		existing.forEachItemKeys = map[string]map[string][]string{}
		existing.collectionCache = make(map[string][]any)
		existing.collectionDirty = make(map[string]bool)
		existing.nodeReady = make(map[string]bool)
		existing.systemErrorBackoff = make(map[string]time.Duration)
		existing.deferredPruneKeys = nil
		// Ensure the compiled graph is tracked in the content-addressed cache.
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
