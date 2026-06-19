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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	memory "k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/restmapper"

	expv1alpha1 "sigs.k8s.io/krocodile/api/v1alpha1"
	"sigs.k8s.io/krocodile/pkg/compiler/variable"
	"sigs.k8s.io/krocodile/pkg/testutil/generator"
	testk8s "sigs.k8s.io/krocodile/pkg/testutil/k8s"
)

// newSchemaDepTestCompiler builds a Compiler bound to a fake schema
// resolver. The fake serves any GVK with a permissive type:object
// schema so the compile pipeline accepts arbitrary GroupKinds without
// real apiserver discovery.
func newSchemaDepTestCompiler(t *testing.T) *Compiler {
	t.Helper()
	r, disco := testk8s.NewFakeResolver()
	rm := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(disco))
	return NewCompilerWithDependencies(r, rm)
}

// TestEmitSchemaDependencies walks the compiler's schema-dep
// extraction with realistic graph shapes. The function is package-
// private; we test through the full Compile pipeline so the IR shape
// stays the source of truth.
func TestEmitSchemaDependencies(t *testing.T) {
	tests := []struct {
		name           string
		graph          *expv1alpha1.Graph
		wantGKs        []schema.GroupKind
		wantDynamicGVK bool
	}{
		{
			name: "single-template-yields-single-GK",
			graph: generator.NewGraph("g",
				generator.WithNamespace("ns"),
				generator.WithTemplate("cm", map[string]any{
					"apiVersion": "v1", "kind": "ConfigMap",
					"metadata": map[string]any{"name": "x"},
				}),
			),
			wantGKs: []schema.GroupKind{{Group: "", Kind: "ConfigMap"}},
		},
		{
			name: "two-templates-different-GKs",
			graph: generator.NewGraph("g",
				generator.WithNamespace("ns"),
				generator.WithTemplate("cm", map[string]any{
					"apiVersion": "v1", "kind": "ConfigMap",
					"metadata": map[string]any{"name": "cm"},
				}),
				generator.WithTemplate("po", map[string]any{
					"apiVersion": "v1", "kind": "Pod",
					"metadata": map[string]any{"name": "po"},
					"spec":     map[string]any{"containers": []any{map[string]any{"name": "c", "image": "i"}}},
				}),
			),
			wantGKs: []schema.GroupKind{
				{Group: "", Kind: "ConfigMap"},
				{Group: "", Kind: "Pod"},
			},
		},
		{
			name: "duplicate-GK-deduplicated",
			graph: generator.NewGraph("g",
				generator.WithNamespace("ns"),
				generator.WithTemplate("a", map[string]any{
					"apiVersion": "v1", "kind": "ConfigMap",
					"metadata": map[string]any{"name": "a"},
				}),
				generator.WithTemplate("b", map[string]any{
					"apiVersion": "v1", "kind": "ConfigMap",
					"metadata": map[string]any{"name": "b"},
				}),
			),
			wantGKs: []schema.GroupKind{{Group: "", Kind: "ConfigMap"}},
		},
		{
			name: "def-only-graph-has-no-GKs",
			graph: generator.NewGraph("g",
				generator.WithNamespace("ns"),
				generator.WithDef("naming", map[string]any{"prefix": "team-"}),
			),
			wantGKs: nil,
		},
		{
			name: "def-plus-template-only-template-contributes",
			graph: generator.NewGraph("g",
				generator.WithNamespace("ns"),
				generator.WithDef("naming", map[string]any{"prefix": "team-"}),
				generator.WithTemplate("cm", map[string]any{
					"apiVersion": "v1", "kind": "ConfigMap",
					"metadata": map[string]any{"name": "${naming.prefix}x"},
				}),
			),
			wantGKs: []schema.GroupKind{{Group: "", Kind: "ConfigMap"}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newSchemaDepTestCompiler(t)
			prog, err := c.Compile(tc.graph)
			require.NoError(t, err)
			assert.ElementsMatch(t, tc.wantGKs, prog.RequiredGroupKinds)
			assert.Equal(t, tc.wantDynamicGVK, prog.HasDynamicGVK)
		})
	}
}

// TestEmitSchemaDependenciesUnit is a direct test of the function
// against synthetic Programs — useful for cases the compile pipeline
// won't currently accept (e.g. HasDynamicGVK=true once the dynamic-GVK
// feature lands).
func TestEmitSchemaDependenciesUnit(t *testing.T) {
	mk := func(apiVersion, kind string) *unstructured.Unstructured {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(schema.FromAPIVersionAndKind(apiVersion, kind))
		obj.SetName("x")
		return obj
	}

	tests := []struct {
		name           string
		nodes          map[string]*Node
		wantGKs        []schema.GroupKind
		wantDynamicGVK bool
	}{
		{
			name: "def-nodes-skipped",
			nodes: map[string]*Node{
				"d": {ID: "d", Kind: NodeKindDef, Object: &unstructured.Unstructured{Object: map[string]any{"k": "v"}}},
			},
			wantGKs: nil,
		},
		{
			name: "watch-kind-contributes-GK",
			nodes: map[string]*Node{
				"w": {ID: "w", Kind: NodeKindWatch, Object: mk("v1", "Pod")},
			},
			wantGKs: []schema.GroupKind{{Group: "", Kind: "Pod"}},
		},
		{
			name: "ref-kind-contributes-GK",
			nodes: map[string]*Node{
				"r": {ID: "r", Kind: NodeKindRef, Object: mk("v1", "Secret")},
			},
			wantGKs: []schema.GroupKind{{Group: "", Kind: "Secret"}},
		},
		{
			// A dynamic-GVK node flips HasDynamicGVK and contributes no
			// literal GroupKind — its target isn't known until reconcile.
			name: "dynamic-gvk-node-flips-HasDynamicGVK-and-emits-no-GK",
			nodes: map[string]*Node{
				"t": {
					ID: "t", Kind: NodeKindTemplate, DynamicGVK: true,
					Object: mk("${schema.group}/v1", "ConfigMap"),
				},
			},
			wantGKs:        nil,
			wantDynamicGVK: true,
		},
		{
			name: "cel-elsewhere-does-not-flip-HasDynamicGVK",
			nodes: map[string]*Node{
				"t": {
					ID: "t", Kind: NodeKindTemplate, Object: mk("v1", "ConfigMap"),
					Variables: variablesAt("metadata.name"),
				},
			},
			wantGKs: []schema.GroupKind{{Group: "", Kind: "ConfigMap"}},
		},
		{
			name: "empty-kind-skipped",
			nodes: map[string]*Node{
				"t": {ID: "t", Kind: NodeKindTemplate, Object: &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1"}}},
			},
			wantGKs: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := &Program{Nodes: tc.nodes}
			emitSchemaDependencies(p)
			assert.ElementsMatch(t, tc.wantGKs, p.RequiredGroupKinds)
			assert.Equal(t, tc.wantDynamicGVK, p.HasDynamicGVK)
		})
	}

	// Defensive: avoid unused-import warning if metav1 isn't otherwise
	// referenced in this test file.
	_ = metav1.Time{}
}

// variablesAt returns a single Variable at the given path, simulating
// what the parser would emit if it found a CEL fragment there. The
// expression compile state is irrelevant — emitSchemaDependencies only
// reads Path.
func variablesAt(path string) []*variable.ResourceField {
	return []*variable.ResourceField{{
		FieldDescriptor: variable.FieldDescriptor{Path: path},
	}}
}
