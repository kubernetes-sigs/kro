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
		generator.WithPatch("p", "${cfg.group}", "ConfigMap", "existing", map[string]any{
			"data": map[string]any{"k": "v"},
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

// TestCompilePatch_ForEachRequiresIteratorInName verifies a forEach patch whose
// target name does NOT vary by the iterator is rejected: it would patch a single
// target N times. This is the same iterator→identity coverage rule templates use.
func TestCompilePatch_ForEachRequiresIteratorInName(t *testing.T) {
	t.Parallel()

	g := generator.NewGraph("g",
		generator.WithDef("src", map[string]any{"names": []any{"a", "b"}}),
		generator.WithPatch("p", "v1", "ConfigMap", "existing", map[string]any{"data": map[string]any{"k": "v"}}),
	)
	// Attach a forEach axis to the patch node whose iterator is not used in the name.
	g.Spec.Nodes[len(g.Spec.Nodes)-1].ForEach = []expv1alpha1.ForEachDimension{{"n": "${src.names}"}}

	_, err := newTestCompiler(t).Compile(g)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "every forEach iterator must appear in metadata.name")
}

// TestCompilePatch_ForEachFansOut verifies a patch node CAN carry forEach when
// its target name varies by the iterator: the same contribution is fanned out
// across every rendered target (e.g. a status writeback to each claimant).
func TestCompilePatch_ForEachFansOut(t *testing.T) {
	t.Parallel()

	g := generator.NewGraph("g",
		generator.WithDef("src", map[string]any{"names": []any{"a", "b"}}),
		generator.WithPatchManifest("p", map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata":   map[string]any{"name": "${n}"},
			"data":       map[string]any{"k": "v"},
		}),
	)
	g.Spec.Nodes[len(g.Spec.Nodes)-1].ForEach = []expv1alpha1.ForEachDimension{{"n": "${src.names}"}}

	prog, err := newTestCompiler(t).Compile(g)
	require.NoError(t, err)
	require.NotNil(t, prog.Nodes["p"])
	assert.True(t, prog.Nodes["p"].IsCollection(), "a forEach patch node must be a collection")
	assert.Equal(t, NodeKindPatch, prog.Nodes["p"].Kind)
}

// TestCompilePatch_RequiresName verifies a patch target must be nameable.
func TestCompilePatch_RequiresName(t *testing.T) {
	t.Parallel()

	g := generator.NewGraph("g",
		generator.WithDef("cfg", map[string]any{"x": "y"}),
		generator.WithPatchManifest("p", map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata":   map[string]any{},
			"data":       map[string]any{"k": "v"},
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

// TestCompilePatch_LabelsOnlyTargetsMainResource verifies a patch that only
// contributes metadata.labels (no status) derives to the main-resource
// endpoint (empty Subresource), not the status subresource.
func TestCompilePatch_LabelsOnlyTargetsMainResource(t *testing.T) {
	t.Parallel()

	g := generator.NewGraph("g",
		generator.WithPatchManifest("p", map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":   "existing",
				"labels": map[string]any{"team": "kro"},
			},
		}),
	)

	prog, err := newTestCompiler(t).Compile(g)
	require.NoError(t, err)

	n := prog.Nodes["p"]
	require.NotNil(t, n)
	assert.Empty(t, n.Subresource, "labels-only patch targets the main resource, not status")
}

// TestCompilePatch_StatusOnlyRoutesToStatusSubresource verifies a patch that
// only contributes a top-level status field derives to the status
// subresource.
func TestCompilePatch_StatusOnlyRoutesToStatusSubresource(t *testing.T) {
	t.Parallel()

	g := generator.NewGraph("g",
		generator.WithPatch("p", "v1", "Pod", "statuspod", map[string]any{
			"status": map[string]any{"phase": "Running"},
		}),
	)

	prog, err := newTestCompiler(t).Compile(g)
	require.NoError(t, err)

	n := prog.Nodes["p"]
	require.NotNil(t, n)
	assert.Equal(t, "status", n.Subresource)
}

// TestCompilePatch_StatusAndMainFieldRejected verifies a single patch node
// mixing a top-level status field with a main-resource field (e.g. spec) is
// a compile error: a patch must target exactly one endpoint.
func TestCompilePatch_StatusAndMainFieldRejected(t *testing.T) {
	t.Parallel()

	g := generator.NewGraph("g",
		generator.WithPatchManifest("p", map[string]any{
			"apiVersion": "v1",
			"kind":       "Pod",
			"metadata":   map[string]any{"name": "statuspod"},
			"status":     map[string]any{"phase": "Running"},
			"spec":       map[string]any{"nodeName": "node-1"},
		}),
	)

	_, err := newTestCompiler(t).Compile(g)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "a patch node targets a single endpoint")
}

// TestCompilePatch_IdentityOnlyRejected verifies a patch node with only
// identity fields (apiVersion/kind/metadata.name) and no contributed field
// is a compile error.
func TestCompilePatch_IdentityOnlyRejected(t *testing.T) {
	t.Parallel()

	g := generator.NewGraph("g",
		generator.WithPatchManifest("p", map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata":   map[string]any{"name": "existing"},
		}),
	)

	_, err := newTestCompiler(t).Compile(g)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "patch node contributes no fields")
}

// TestCompilePatch_MetadataNameIsTheTarget verifies metadata.name is the
// sole source of patch target identity — there is no separate identity vs.
// body in the raw-manifest model, so whatever name is set in metadata is
// the target the patch resolves against.
func TestCompilePatch_MetadataNameIsTheTarget(t *testing.T) {
	t.Parallel()

	g := generator.NewGraph("g",
		generator.WithPatch("p", "v1", "ConfigMap", "realtarget", map[string]any{
			"data": map[string]any{"k": "v"},
		}),
	)

	prog, err := newTestCompiler(t).Compile(g)
	require.NoError(t, err)

	n := prog.Nodes["p"]
	require.NotNil(t, n)
	assert.Equal(t, NodeKindPatch, n.Kind)
	assert.Equal(t, "configmaps", n.GVR.Resource)
}

// TestCompilePatch_RejectsOwnerReferences is a regression test for the
// documented guarantee that a patch never deletes its target: a patch that
// stamps metadata.ownerReferences onto a target it does not own could make the
// garbage collector delete that target, so it must be rejected at compile time.
func TestCompilePatch_RejectsOwnerReferences(t *testing.T) {
	t.Parallel()

	g := generator.NewGraph("g",
		generator.WithPatchManifest("p", map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name": "existing",
				"ownerReferences": []any{map[string]any{
					"apiVersion": "v1",
					"kind":       "ConfigMap",
					"name":       "owner",
					"uid":        "abc",
				}},
			},
			"data": map[string]any{"k": "v"},
		}),
	)

	_, err := newTestCompiler(t).Compile(g)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "metadata.ownerReferences")
}

// TestCompilePatch_RejectsFinalizers verifies a patch payload carrying
// metadata.finalizers is rejected — finalizers are lifecycle metadata a patch
// must not contribute.
func TestCompilePatch_RejectsFinalizers(t *testing.T) {
	t.Parallel()

	g := generator.NewGraph("g",
		generator.WithPatchManifest("p", map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":       "existing",
				"finalizers": []any{"example.com/hold"},
			},
			"data": map[string]any{"k": "v"},
		}),
	)

	_, err := newTestCompiler(t).Compile(g)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "metadata.finalizers")
}

// TestCompilePatch_RejectsDeletionTimestamp verifies a patch payload carrying
// metadata.deletionTimestamp is rejected — it would drive termination of the
// target, breaking the never-deletes guarantee.
func TestCompilePatch_RejectsDeletionTimestamp(t *testing.T) {
	t.Parallel()

	g := generator.NewGraph("g",
		generator.WithPatchManifest("p", map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":              "existing",
				"deletionTimestamp": "2026-01-01T00:00:00Z",
			},
			"data": map[string]any{"k": "v"},
		}),
	)

	_, err := newTestCompiler(t).Compile(g)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "metadata.deletionTimestamp")
}

// TestCompilePatch_DynamicGVK_RejectsOwnerReferences verifies the
// identity/lifecycle metadata guard also runs on the dynamic-GVK path.
func TestCompilePatch_DynamicGVK_RejectsOwnerReferences(t *testing.T) {
	t.Parallel()

	g := generator.NewGraph("g",
		generator.WithDef("cfg", map[string]any{"group": "v1"}),
		generator.WithPatchManifest("p", map[string]any{
			"apiVersion": "${cfg.group}",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name": "existing",
				"ownerReferences": []any{map[string]any{
					"apiVersion": "v1",
					"kind":       "ConfigMap",
					"name":       "owner",
					"uid":        "abc",
				}},
			},
			"data": map[string]any{"k": "v"},
		}),
	)

	_, err := newTestCompiler(t).Compile(g)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "metadata.ownerReferences")
}
