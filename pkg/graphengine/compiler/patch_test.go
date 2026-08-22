// Copyright 2026 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package compiler

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	k8sschema "k8s.io/apimachinery/pkg/runtime/schema"

	expv1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/testutil/generator"
)

// TestCompilePatch covers the patch node: a literal-GVK patch resolves its
// target GVR and types its body against the target schema, a body CEL
// reference becomes a dependency, and a patch never publishes a schema to
// scope (it contributes, it does not produce a value).
func TestCompilePatch(t *testing.T) {
	t.Parallel()

	g := generator.NewGraph("g",
		generator.WithDef("cfg", map[string]any{"target": "existing", "val": "hello"}),
		generator.WithPatch("p", "v1", "ConfigMap", "existing", map[string]any{
			"data": map[string]any{"k": "${cfg.val}"},
		}),
	)

	prog, err := newTestCompiler(t).Compile(g)
	require.NoError(t, err)

	n := prog.Nodes["p"]
	require.NotNil(t, n)
	assert.Equal(t, NodeKindPatch, n.Kind)
	assert.Equal(t, "configmaps", n.GVR.Resource)
	assert.True(t, n.Namespaced)
	assert.Empty(t, n.Subresource)
	assert.Contains(t, n.HardDepIDs(), "cfg", "body CEL reference is a dependency")

	// A patch contributes fields; it publishes no schema into CEL scope.
	_, published := prog.NodeSchemas["p"]
	assert.False(t, published, "patch node must not publish a schema")

	// The target GroupKind is a schema dependency.
	assert.Contains(t, prog.RequiredGroupKinds, k8sschema.GroupKind{Group: "", Kind: "ConfigMap"})
}

// TestCompilePatch_DynamicGVK covers a patch whose apiVersion is a CEL
// expression: it compiles schemaless, flags the node and program dynamic, and
// contributes no literal GroupKind.
func TestCompilePatch_DynamicGVK(t *testing.T) {
	t.Parallel()

	g := generator.NewGraph("g",
		generator.WithDef("cfg", map[string]any{"group": "v1"}),
		generator.WithPatchSpec("p", &expv1alpha1.PatchSpec{
			APIVersion: "${cfg.group}",
			Kind:       "ConfigMap",
			Metadata:   expv1alpha1.PatchMetadata{Name: "existing"},
			Body: generator.RawExtFromMap(map[string]any{
				"data": map[string]any{"k": "v"},
			}),
		}),
	)

	prog, err := newTestCompiler(t).Compile(g)
	require.NoError(t, err)

	n := prog.Nodes["p"]
	require.NotNil(t, n)
	assert.Equal(t, NodeKindPatch, n.Kind)
	assert.True(t, n.DynamicGVK)
	assert.True(t, prog.HasDynamicGVK)
	assert.Contains(t, n.HardDepIDs(), "cfg")
}

// TestCompilePatch_RejectsForEach verifies forEach is not allowed on a patch
// node — a patch contributes to a single existing target.
func TestCompilePatch_RejectsForEach(t *testing.T) {
	t.Parallel()

	g := generator.NewGraph("g",
		generator.WithDef("src", map[string]any{"names": []any{"a", "b"}}),
		generator.WithPatchSpec("p", &expv1alpha1.PatchSpec{
			APIVersion: "v1",
			Kind:       "ConfigMap",
			Metadata:   expv1alpha1.PatchMetadata{Name: "existing"},
			Body:       generator.RawExtFromMap(map[string]any{"data": map[string]any{"k": "v"}}),
		}),
	)
	// Attach a forEach axis to the patch node directly.
	g.Spec.Nodes[len(g.Spec.Nodes)-1].ForEach = []expv1alpha1.ForEachDimension{{"n": "${src.names}"}}

	_, err := newTestCompiler(t).Compile(g)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "forEach is not supported on patch nodes")
}

// TestCompilePatch_RequiresName verifies a patch target must be nameable.
func TestCompilePatch_RequiresName(t *testing.T) {
	t.Parallel()

	g := generator.NewGraph("g",
		generator.WithDef("cfg", map[string]any{"x": "y"}),
		generator.WithPatchSpec("p", &expv1alpha1.PatchSpec{
			APIVersion: "v1",
			Kind:       "ConfigMap",
			Body:       generator.RawExtFromMap(map[string]any{"data": map[string]any{"k": "v"}}),
		}),
	)

	_, err := newTestCompiler(t).Compile(g)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "metadata.name")
}

// TestCompilePatch_ReferencingPatchNodeRejected verifies that referencing a
// patch node ID in a CEL expression fails compilation with a clear error.
func TestCompilePatch_ReferencingPatchNodeRejected(t *testing.T) {
	t.Parallel()

	g := generator.NewGraph("g",
		generator.WithDef("cfg", map[string]any{"val": "hello"}),
		generator.WithPatch("p", "v1", "ConfigMap", "existing", map[string]any{
			"data": map[string]any{"k": "${cfg.val}"},
		}),
		generator.WithDef("consumer", map[string]any{
			"data": "${p.status}",
		}),
	)

	_, err := newTestCompiler(t).Compile(g)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `patch node "p" does not publish a value into scope and cannot be referenced in CEL expressions`)
}
