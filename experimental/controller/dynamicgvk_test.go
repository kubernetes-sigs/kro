package graphcontroller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	openapi "k8s.io/kube-openapi/pkg/validation/spec"
)

// TestMergeDynamicGVK_FirstResolution verifies that first resolution reports stale.
func TestMergeDynamicGVK_FirstResolution(t *testing.T) {
	t.Parallel()
	state := &instanceState{}
	widgetGVK := schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "Widget"}

	stale := state.mergeDynamicGVK("dynamic-node", widgetGVK)
	assert.True(t, stale, "first resolution should report stale (key will change)")
	assert.Equal(t, widgetGVK, state.resolvedDynamicGVKs["dynamic-node"])
}

// TestMergeDynamicGVK_SameGVK verifies steady-state does not report stale.
func TestMergeDynamicGVK_SameGVK(t *testing.T) {
	t.Parallel()
	widgetGVK := schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "Widget"}
	state := &instanceState{
		resolvedDynamicGVKs: map[string]schema.GroupVersionKind{"dynamic-node": widgetGVK},
	}

	stale := state.mergeDynamicGVK("dynamic-node", widgetGVK)
	assert.False(t, stale, "same GVK should not report stale")
}

// TestMergeDynamicGVK_GVKChange verifies that GVK change reports stale.
func TestMergeDynamicGVK_GVKChange(t *testing.T) {
	t.Parallel()
	widgetGVK := schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "Widget"}
	gadgetGVK := schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "Gadget"}
	state := &instanceState{
		resolvedDynamicGVKs: map[string]schema.GroupVersionKind{"dynamic-node": widgetGVK},
	}

	stale := state.mergeDynamicGVK("dynamic-node", gadgetGVK)
	assert.True(t, stale, "different GVK should report stale")
	assert.Equal(t, gadgetGVK, state.resolvedDynamicGVKs["dynamic-node"])
}

// TestMergeDynamicGVK_PerNode verifies per-node isolation.
func TestMergeDynamicGVK_PerNode(t *testing.T) {
	t.Parallel()
	state := &instanceState{}
	widgetGVK := schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "Widget"}
	gadgetGVK := schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "Gadget"}

	assert.True(t, state.mergeDynamicGVK("node-a", widgetGVK))
	assert.True(t, state.mergeDynamicGVK("node-b", gadgetGVK))
	assert.False(t, state.mergeDynamicGVK("node-a", widgetGVK))
	assert.False(t, state.mergeDynamicGVK("node-b", gadgetGVK))
}

// TestCompilationKeyWithHints verifies that hints change the key.
func TestCompilationKeyWithHints(t *testing.T) {
	t.Parallel()
	structKey := "abc123"

	// No hints → same key.
	assert.Equal(t, structKey, compilationKeyWithHints(structKey, nil))
	assert.Equal(t, structKey, compilationKeyWithHints(structKey, map[string]schema.GroupVersionKind{}))

	// With hints → different key.
	widgetGVK := schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "Widget"}
	gadgetGVK := schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "Gadget"}

	key1 := compilationKeyWithHints(structKey, map[string]schema.GroupVersionKind{"node": widgetGVK})
	key2 := compilationKeyWithHints(structKey, map[string]schema.GroupVersionKind{"node": gadgetGVK})

	assert.NotEqual(t, structKey, key1, "hints should change the key")
	assert.NotEqual(t, key1, key2, "different GVKs should produce different keys")

	// Same hints → same key (deterministic).
	key1b := compilationKeyWithHints(structKey, map[string]schema.GroupVersionKind{"node": widgetGVK})
	assert.Equal(t, key1, key1b, "same hints should produce same key")
}

// TestDynamicGVK_ProgressionFromDynToTyped is the end-to-end test proving
// that a dynamic GVK node progresses from dyn (first reconcile) to typed
// (second reconcile with hints). This is the load-bearing invariant for
// RGD compat: without this progression, forEach type validation cannot work
// for dynamic GVK nodes.
//
// Simulates the Kind controller lifecycle:
//  1. First compileRevision: no hints → bootstrap artifact (dyn)
//  2. Reconciler evaluates expression → resolves GVK → records in state
//  3. Second compileRevision: hints available → typed artifact with schema
func TestDynamicGVK_ProgressionFromDynToTyped(t *testing.T) {
	t.Parallel()

	widgetGVK := schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "Widget"}
	widgetSchema := &openapi.Schema{
		SchemaProps: openapi.SchemaProps{
			Type: []string{"object"},
			Properties: map[string]openapi.Schema{
				"apiVersion": {SchemaProps: openapi.SchemaProps{Type: []string{"string"}}},
				"kind":       {SchemaProps: openapi.SchemaProps{Type: []string{"string"}}},
				"metadata":   {SchemaProps: openapi.SchemaProps{Type: []string{"object"}}},
				"spec": {SchemaProps: openapi.SchemaProps{
					Type: []string{"object"},
					Properties: map[string]openapi.Schema{
						"name": {SchemaProps: openapi.SchemaProps{Type: []string{"string"}}},
					},
				}},
			},
		},
	}
	resolver := &stubSchemaResolver{schemas: map[schema.GroupVersionKind]*openapi.Schema{
		widgetGVK: widgetSchema,
	}}

	revision := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "experimental.kro.run/v1alpha1",
		"kind":       "GraphRevision",
		"metadata": map[string]any{
			"name":      "kind-graph-g00001",
			"namespace": "default",
		},
		"spec": map[string]any{
			"nodes": []any{
				map[string]any{
					"id": "parent",
					"template": map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata":   map[string]any{"name": "config"},
						"data":       map[string]any{"targetKind": "Widget"},
					},
				},
				map[string]any{
					"id": "schema",
					"template": map[string]any{
						"apiVersion": "example.com/v1",
						"kind":       "${parent.data.targetKind}",
						"metadata":   map[string]any{"name": "instance"},
					},
				},
			},
		},
	}}

	r := &GraphReconciler{
		SchemaResolver: resolver,
		SchemaGen:      NewSchemaGeneration(),
		Caches:         newGraphCaches(),
	}

	// Phase 1: first compileRevision — no hints, bootstrap artifact (dyn).
	_, state1, err := r.compileRevision(context.Background(), "", revision)
	require.NoError(t, err)
	require.NotNil(t, state1.compiled)
	bootstrapArtifact := state1.compiled
	assert.Contains(t, bootstrapArtifact.dynamicGVKNodes, "schema",
		"first compilation should detect dynamic GVK node")
	assert.NotContains(t, bootstrapArtifact.resourceSchemas, "schema",
		"first compilation should NOT have schema for dynamic node (no hints)")

	// Phase 2: simulate reconciler resolving the GVK (during DAG walk).
	stale := state1.mergeDynamicGVK("schema", widgetGVK)
	assert.True(t, stale, "first resolution should signal requeue")

	// Phase 3: second compileRevision — hints available, typed artifact.
	_, state2, err := r.compileRevision(context.Background(), "", revision)
	require.NoError(t, err)
	require.NotNil(t, state2.compiled)
	assert.NotSame(t, bootstrapArtifact, state2.compiled,
		"second compilation should produce a NEW artifact (different key)")
	assert.Contains(t, state2.compiled.resourceSchemas, "schema",
		"second compilation should have schema for dynamic node (hint used)")

	// Verify the schema is Widget's schema.
	schemaResolved := state2.compiled.resourceSchemas["schema"]
	require.NotNil(t, schemaResolved)
	assert.Contains(t, schemaResolved.Properties["spec"].Properties, "name",
		"resolved schema should be Widget's with spec.name field")

	// Phase 4: steady-state — same hints, cache hit.
	_, state3, err := r.compileRevision(context.Background(), "", revision)
	require.NoError(t, err)
	assert.Same(t, state2.compiled, state3.compiled,
		"steady-state should return same typed artifact (cache hit)")
}
