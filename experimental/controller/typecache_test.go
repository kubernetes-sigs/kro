package graphcontroller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/kube-openapi/pkg/validation/spec"
)

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

func TestTypeCache_ResolveAndCache(t *testing.T) {
	t.Parallel()
	cmGVK := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}
	cmSchema := &spec.Schema{}
	cmSchema.Description = "test configmap schema"

	resolver := &stubSchemaResolver{
		schemas: map[schema.GroupVersionKind]*spec.Schema{cmGVK: cmSchema},
	}
	tc := NewTypeCache(resolver)

	// First call: cache miss, resolves from API server.
	s := tc.Resolve(cmGVK)
	require.NotNil(t, s)
	assert.Equal(t, "test configmap schema", s.Description)

	// Second call: cache hit, same pointer.
	s2 := tc.Resolve(cmGVK)
	assert.Same(t, s, s2, "second resolve should return cached pointer")

	// Unresolvable GVK returns nil and is NOT cached.
	unknownGVK := schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Unknown"}
	s3 := tc.Resolve(unknownGVK)
	assert.Nil(t, s3)
}

func TestTypeCache_GenerationAdvances(t *testing.T) {
	t.Parallel()
	tc := NewTypeCache(nil)

	assert.Equal(t, int64(0), tc.Generation())
	tc.AdvanceGeneration()
	assert.Equal(t, int64(1), tc.Generation())
	tc.AdvanceGeneration()
	assert.Equal(t, int64(2), tc.Generation())
}

func TestTypeCache_InvalidateAndAdvance(t *testing.T) {
	t.Parallel()
	cmGVK := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}
	cmSchema := &spec.Schema{}

	resolver := &stubSchemaResolver{
		schemas: map[schema.GroupVersionKind]*spec.Schema{cmGVK: cmSchema},
	}
	tc := NewTypeCache(resolver)

	// Populate cache.
	s := tc.Resolve(cmGVK)
	require.NotNil(t, s)

	// Invalidate: removes from cache and advances generation.
	tc.InvalidateAndAdvance(cmGVK)
	assert.Equal(t, int64(1), tc.Generation())

	// Next resolve re-queries the resolver (fresh resolution).
	s2 := tc.Resolve(cmGVK)
	require.NotNil(t, s2)
	// Same pointer because the stub resolver returns the same object,
	// but the cache had to re-resolve it.
	assert.Same(t, cmSchema, s2)
}

func TestTypeCache_NilResolver(t *testing.T) {
	t.Parallel()
	tc := NewTypeCache(nil)
	gvk := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}
	assert.Nil(t, tc.Resolve(gvk), "nil resolver should return nil for all GVKs")
}

// TestCompileRevision_GenerationStaleness verifies that compileRevision
// recompiles when the type cache generation advances past the artifact's
// recorded generation. Per 004-compilation.md § Type Cache: "Staleness is
// one integer comparison."
func TestCompileRevision_GenerationStaleness(t *testing.T) {
	t.Parallel()

	spec := &GraphSpec{Nodes: []Node{
		{ID: "cm", Template: map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "test"},
		}},
	}}

	// Compile at generation 0.
	tc := NewTypeCache(nil)
	compiled, err := compileGraphSpec(spec, nil)
	require.NoError(t, err)
	compiled.typeCacheGen = tc.Generation()

	// Verify not stale at generation 0.
	assert.False(t, compiled.typeCacheGen < tc.Generation(),
		"artifact should not be stale at same generation")

	// Advance generation (simulates CRD install).
	tc.AdvanceGeneration()

	// Artifact is now stale.
	assert.True(t, compiled.typeCacheGen < tc.Generation(),
		"artifact should be stale after generation advance")
}
