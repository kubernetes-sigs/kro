package graphcontroller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	openapi "k8s.io/kube-openapi/pkg/validation/spec"

	"github.com/ellistarn/kro/experimental/controller/compiler"
)

// TestMergeDynamicGVK_FirstResolution verifies that first resolution reports stale.
func TestMergeDynamicGVK_FirstResolution(t *testing.T) {
	t.Parallel()
	state := &instanceState{}
	widgetGVK := schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "Widget"}

	stale := state.dynamicGVKs.merge("dynamic-node", widgetGVK)
	assert.True(t, stale, "first resolution should report stale (key will change)")
	assert.Equal(t, widgetGVK, state.dynamicGVKs.resolved["dynamic-node"])
}

// TestMergeDynamicGVK_SameGVK verifies steady-state does not report stale.
func TestMergeDynamicGVK_SameGVK(t *testing.T) {
	t.Parallel()
	widgetGVK := schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "Widget"}
	state := &instanceState{
		dynamicGVKs: dynamicGVKCache{resolved: map[string]schema.GroupVersionKind{"dynamic-node": widgetGVK}},
	}

	stale := state.dynamicGVKs.merge("dynamic-node", widgetGVK)
	assert.False(t, stale, "same GVK should not report stale")
}

// TestMergeDynamicGVK_GVKChange verifies that GVK change reports stale.
func TestMergeDynamicGVK_GVKChange(t *testing.T) {
	t.Parallel()
	widgetGVK := schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "Widget"}
	gadgetGVK := schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "Gadget"}
	state := &instanceState{
		dynamicGVKs: dynamicGVKCache{resolved: map[string]schema.GroupVersionKind{"dynamic-node": widgetGVK}},
	}

	stale := state.dynamicGVKs.merge("dynamic-node", gadgetGVK)
	assert.True(t, stale, "different GVK should report stale")
	assert.Equal(t, gadgetGVK, state.dynamicGVKs.resolved["dynamic-node"])
}

// TestMergeDynamicGVK_PerNode verifies per-node isolation.
func TestMergeDynamicGVK_PerNode(t *testing.T) {
	t.Parallel()
	state := &instanceState{}
	widgetGVK := schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "Widget"}
	gadgetGVK := schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "Gadget"}

	assert.True(t, state.dynamicGVKs.merge("node-a", widgetGVK))
	assert.True(t, state.dynamicGVKs.merge("node-b", gadgetGVK))
	assert.False(t, state.dynamicGVKs.merge("node-a", widgetGVK))
	assert.False(t, state.dynamicGVKs.merge("node-b", gadgetGVK))
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
		SchemaGen:      compiler.NewSchemaGeneration(),
		Caches:         newInstanceMap(),
	}

	// Phase 1: first compileRevision — no hints, bootstrap artifact (dyn).
	_, state1, err := r.compileRevision(context.Background(), "", revision)
	require.NoError(t, err)
	require.NotNil(t, state1.compilation.compiled)
	bootstrapArtifact := state1.compilation.compiled
	assert.NotContains(t, bootstrapArtifact.ResourceSchemas, "schema",
		"first compilation should NOT have schema for dynamic node (no hints)")

	// Phase 2: simulate reconciler resolving the GVK (during DAG walk).
	stale := state1.dynamicGVKs.merge("schema", widgetGVK)
	assert.True(t, stale, "first resolution should signal requeue")

	// Phase 3: second compileRevision — hints available, typed artifact.
	_, state2, err := r.compileRevision(context.Background(), "", revision)
	require.NoError(t, err)
	require.NotNil(t, state2.compilation.compiled)
	assert.NotSame(t, bootstrapArtifact, state2.compilation.compiled,
		"second compilation should produce a NEW artifact (different key)")
	assert.Contains(t, state2.compilation.compiled.ResourceSchemas, "schema",
		"second compilation should have schema for dynamic node (hint used)")

	// Verify the schema is Widget's schema.
	schemaResolved := state2.compilation.compiled.ResourceSchemas["schema"]
	require.NotNil(t, schemaResolved)
	assert.Contains(t, schemaResolved.Properties["spec"].Properties, "name",
		"resolved schema should be Widget's with spec.name field")

	// Phase 4: steady-state — recompilation with same hints succeeds.
	_, state3, err := r.compileRevision(context.Background(), "", revision)
	require.NoError(t, err)
	require.NotNil(t, state3.compilation.compiled)
	assert.Contains(t, state3.compilation.compiled.ResourceSchemas, "schema",
		"steady-state recompilation should still have schema for dynamic node")
}
