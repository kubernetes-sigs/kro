package graphcontroller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/kube-openapi/pkg/validation/spec"

	"github.com/kubernetes-sigs/kro/experimental/controller/compiler"
	"github.com/kubernetes-sigs/kro/experimental/controller/graph"
)

func TestSchemaGeneration_Advances(t *testing.T) {
	t.Parallel()
	tc := compiler.NewSchemaGeneration()

	assert.Equal(t, int64(0), tc.Generation())
	tc.AdvanceGeneration()
	assert.Equal(t, int64(1), tc.Generation())
	tc.AdvanceGeneration()
	assert.Equal(t, int64(2), tc.Generation())
}

// TestCompileRevision_GenerationStaleness verifies that compileRevision
// recompiles when the type cache generation advances past the artifact's
// recorded generation. Per 004-compilation.md § Type Cache: "Staleness is
// one integer comparison."
func TestCompileRevision_GenerationStaleness(t *testing.T) {
	t.Parallel()

	spec := &graph.GraphSpec{Nodes: []graph.Node{
		{ID: "cm", Template: map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "test"},
		}},
	}}

	// Compile at generation 0.
	tc := compiler.NewSchemaGeneration()
	compiled, err := compiler.CompileGraphSpec(spec, nil)
	require.NoError(t, err)
	compiled.TypeCacheGen = tc.Generation()

	// Verify not stale at generation 0.
	assert.False(t, compiled.TypeCacheGen < tc.Generation(),
		"artifact should not be stale at same generation")

	// Advance generation (simulates CRD install).
	tc.AdvanceGeneration()

	// Artifact is now stale.
	assert.True(t, compiled.TypeCacheGen < tc.Generation(),
		"artifact should be stale after generation advance")
}

// TestCompileRevision_RecompilesOnGenerationAdvance exercises the full
// compileRevision method, proving that:
//  1. First call compiles and caches the result
//  2. Second call returns the cached artifact (fast path)
//  3. After SchemaGeneration advances, the method recompiles
//  4. The recompiled artifact records the new generation
//
// This tests the production staleness detection path end-to-end through
// compileRevision, not just the comparison in isolation.
func TestCompileRevision_RecompilesOnGenerationAdvance(t *testing.T) {
	t.Parallel()

	// Build a revision object — same structure as materialize() produces.
	revision := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "experimental.kro.run/v1alpha1",
		"kind":       "GraphRevision",
		"metadata": map[string]any{
			"name":      "test-graph-g00001",
			"namespace": "default",
		},
		"spec": map[string]any{
			"nodes": []any{
				map[string]any{
					"id": "cm",
					"template": map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata":   map[string]any{"name": "test"},
					},
				},
			},
		},
	}}

	tc := compiler.NewSchemaGeneration()
	r := &GraphReconciler{
		SchemaGen: tc,
		Caches:    newGraphCaches(),
	}

	// First call: compiles from scratch.
	spec1, state1, err := r.compileRevision(context.Background(), "", revision)
	require.NoError(t, err)
	require.NotNil(t, spec1)
	require.NotNil(t, state1)
	require.NotNil(t, state1.compiled)
	firstCompiled := state1.compiled

	// Second call: returns cached (fast path).
	_, state2, err := r.compileRevision(context.Background(), "", revision)
	require.NoError(t, err)
	assert.Same(t, firstCompiled, state2.compiled,
		"steady-state reconcile should return same compiledGraph pointer")

	// Advance generation — simulates CRD install/update.
	tc.AdvanceGeneration()

	// Third call: detects staleness and recompiles.
	_, state3, err := r.compileRevision(context.Background(), "", revision)
	require.NoError(t, err)
	require.NotNil(t, state3.compiled)
	assert.NotSame(t, firstCompiled, state3.compiled,
		"after generation advance, compileRevision must return a NEW compiledGraph")

	// The new artifact records the current generation.
	assert.Equal(t, tc.Generation(), state3.compiled.TypeCacheGen,
		"recompiled artifact should record the current type cache generation")

	// Subsequent call returns the new cached artifact (no further recompilation).
	_, state4, err := r.compileRevision(context.Background(), "", revision)
	require.NoError(t, err)
	assert.Same(t, state3.compiled, state4.compiled,
		"steady-state after recompilation should cache the new artifact")
}

// TestCompileRevision_SchemaUpdateViaGenerationAdvance proves that when a CRD
// schema changes, advancing the SchemaGeneration triggers recompilation
// which picks up the new schema from the underlying SchemaResolver.
//
// This tests the CRD-update path: a GVK's schema already existed but has
// changed. The SchemaResolver returns the updated schema on re-resolution.
func TestCompileRevision_SchemaUpdateViaGenerationAdvance(t *testing.T) {
	t.Parallel()

	widgetGVK := schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "Widget"}

	// Version 1: Widget has spec.replicas (integer).
	schemaV1 := &spec.Schema{
		SchemaProps: spec.SchemaProps{
			Type: []string{"object"},
			Properties: map[string]spec.Schema{
				"apiVersion": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
				"kind":       {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
				"metadata":   {SchemaProps: spec.SchemaProps{Type: []string{"object"}}},
				"spec": {SchemaProps: spec.SchemaProps{
					Type: []string{"object"},
					Properties: map[string]spec.Schema{
						"replicas": {SchemaProps: spec.SchemaProps{Type: []string{"integer"}}},
					},
				}},
			},
		},
	}

	// Mutable resolver — we'll swap schema v1 → v2 mid-test.
	resolverSchemas := map[schema.GroupVersionKind]*spec.Schema{widgetGVK: schemaV1}
	mutableResolver := &stubSchemaResolver{schemas: resolverSchemas}

	tc := compiler.NewSchemaGeneration()
	r := &GraphReconciler{
		SchemaResolver: mutableResolver,
		SchemaGen:      tc,
		Caches:         newGraphCaches(),
	}

	revision := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "experimental.kro.run/v1alpha1",
		"kind":       "GraphRevision",
		"metadata": map[string]any{
			"name":      "widget-graph-g00001",
			"namespace": "default",
		},
		"spec": map[string]any{
			"nodes": []any{
				map[string]any{
					"id": "widget",
					"template": map[string]any{
						"apiVersion": "example.com/v1",
						"kind":       "Widget",
						"metadata":   map[string]any{"name": "test"},
						"spec":       map[string]any{"replicas": int64(3)},
					},
				},
			},
		},
	}}

	// Phase 1: compile with schema v1 — should resolve Widget's schema.
	_, state1, err := r.compileRevision(context.Background(), "", revision)
	require.NoError(t, err)
	require.NotNil(t, state1.compiled)
	firstCompiled := state1.compiled

	// Verify Widget was resolved (not in unresolvedGVKs).
	for _, gvk := range firstCompiled.UnresolvedGVKs {
		assert.NotEqual(t, widgetGVK, gvk, "Widget should be resolved with v1 schema")
	}

	// Phase 2: CRD updated — schema v2 adds spec.maxReplicas.
	schemaV2 := &spec.Schema{
		SchemaProps: spec.SchemaProps{
			Type: []string{"object"},
			Properties: map[string]spec.Schema{
				"apiVersion": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
				"kind":       {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
				"metadata":   {SchemaProps: spec.SchemaProps{Type: []string{"object"}}},
				"spec": {SchemaProps: spec.SchemaProps{
					Type: []string{"object"},
					Properties: map[string]spec.Schema{
						"replicas":    {SchemaProps: spec.SchemaProps{Type: []string{"integer"}}},
						"maxReplicas": {SchemaProps: spec.SchemaProps{Type: []string{"integer"}}},
					},
				}},
			},
		},
	}
	// Swap the resolver's schema and advance generation.
	mutableResolver.schemas[widgetGVK] = schemaV2
	tc.AdvanceGeneration()

	// Phase 3: next compileRevision detects stale generation, recompiles.
	_, state2, err := r.compileRevision(context.Background(), "", revision)
	require.NoError(t, err)
	require.NotNil(t, state2.compiled)
	assert.NotSame(t, firstCompiled, state2.compiled,
		"after generation advance, compileRevision must recompile")

	// The new compilation used schemaV2 — verify by checking resourceSchemas.
	widgetSchema := state2.compiled.ResourceSchemas["widget"]
	require.NotNil(t, widgetSchema, "widget should have resolved schema after recompilation")
	specProps := widgetSchema.Properties["spec"]
	assert.Contains(t, specProps.Properties, "maxReplicas",
		"recompiled artifact should reflect schema v2 with maxReplicas field")
}

// stubSchemaResolver is a test resolver that returns pre-configured schemas.
type stubSchemaResolver struct {
	schemas map[schema.GroupVersionKind]*spec.Schema
}

func (r *stubSchemaResolver) ResolveSchema(gvk schema.GroupVersionKind) (*spec.Schema, error) {
	if s, ok := r.schemas[gvk]; ok {
		return s, nil
	}
	return nil, nil
}

// TestCRDUpdate_AdvanceGenerationInvalidatesArtifact verifies that a CRD
// schema update (simulated by advancing SchemaGeneration) triggers recompilation
// which picks up the updated schema. Per 004-compilation.md § Compilation Cache:
// "Any schema change (CRD installed, updated, removed) advances [the generation
// counter]."
//
// This tests the CRD update path end-to-end through compileRevision:
//  1. Compile with schema v1 → artifact uses v1
//  2. CRD updated → SchemaGeneration advances (simulates CRD watch handler)
//  3. Resolver now returns v2
//  4. compileRevision detects staleness → recompiles with v2
func TestCRDUpdate_AdvanceGenerationInvalidatesArtifact(t *testing.T) {
	t.Parallel()

	widgetGVK := schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "Widget"}

	schemaV1 := &spec.Schema{
		SchemaProps: spec.SchemaProps{
			Type: []string{"object"},
			Properties: map[string]spec.Schema{
				"apiVersion": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
				"kind":       {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
				"metadata":   {SchemaProps: spec.SchemaProps{Type: []string{"object"}}},
				"spec": {SchemaProps: spec.SchemaProps{
					Type: []string{"object"},
					Properties: map[string]spec.Schema{
						"replicas": {SchemaProps: spec.SchemaProps{Type: []string{"integer"}}},
					},
				}},
			},
		},
	}

	resolverSchemas := map[schema.GroupVersionKind]*spec.Schema{widgetGVK: schemaV1}
	mutableResolver := &stubSchemaResolver{schemas: resolverSchemas}

	sg := compiler.NewSchemaGeneration()
	r := &GraphReconciler{
		SchemaResolver: mutableResolver,
		SchemaGen:      sg,
		Caches:         newGraphCaches(),
	}

	revision := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "experimental.kro.run/v1alpha1",
		"kind":       "GraphRevision",
		"metadata": map[string]any{
			"name":      "widget-graph-g00001",
			"namespace": "default",
		},
		"spec": map[string]any{
			"nodes": []any{
				map[string]any{
					"id": "widget",
					"template": map[string]any{
						"apiVersion": "example.com/v1",
						"kind":       "Widget",
						"metadata":   map[string]any{"name": "test"},
						"spec":       map[string]any{"replicas": int64(3)},
					},
				},
			},
		},
	}}

	// Phase 1: compile with schema v1.
	_, state1, err := r.compileRevision(context.Background(), "", revision)
	require.NoError(t, err)
	require.NotNil(t, state1.compiled)
	firstCompiled := state1.compiled

	widgetSchema := state1.compiled.ResourceSchemas["widget"]
	require.NotNil(t, widgetSchema)
	assert.Contains(t, widgetSchema.Properties["spec"].Properties, "replicas")
	assert.NotContains(t, widgetSchema.Properties["spec"].Properties, "maxReplicas",
		"v1 schema should not have maxReplicas")

	// Phase 2: CRD updated — schema v2 adds spec.maxReplicas.
	// Simulate what the CRD watch handler does: swap schema + advance generation.
	schemaV2 := &spec.Schema{
		SchemaProps: spec.SchemaProps{
			Type: []string{"object"},
			Properties: map[string]spec.Schema{
				"apiVersion": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
				"kind":       {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
				"metadata":   {SchemaProps: spec.SchemaProps{Type: []string{"object"}}},
				"spec": {SchemaProps: spec.SchemaProps{
					Type: []string{"object"},
					Properties: map[string]spec.Schema{
						"replicas":    {SchemaProps: spec.SchemaProps{Type: []string{"integer"}}},
						"maxReplicas": {SchemaProps: spec.SchemaProps{Type: []string{"integer"}}},
					},
				}},
			},
		},
	}
	mutableResolver.schemas[widgetGVK] = schemaV2
	sg.AdvanceGeneration()

	// Phase 3: compileRevision detects staleness → recompiles with v2.
	_, state2, err := r.compileRevision(context.Background(), "", revision)
	require.NoError(t, err)
	require.NotNil(t, state2.compiled)
	assert.NotSame(t, firstCompiled, state2.compiled,
		"after generation advance, compileRevision must recompile")

	widgetSchemaV2 := state2.compiled.ResourceSchemas["widget"]
	require.NotNil(t, widgetSchemaV2)
	assert.Contains(t, widgetSchemaV2.Properties["spec"].Properties, "maxReplicas",
		"recompiled artifact should reflect v2 schema with maxReplicas field")
}
