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
	memory "k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/restmapper"
	"k8s.io/kube-openapi/pkg/validation/spec"

	expv1alpha1 "sigs.k8s.io/krocodile/api/v1alpha1"
	"sigs.k8s.io/krocodile/pkg/compiler/parser"
	testk8s "sigs.k8s.io/krocodile/pkg/testutil/k8s"
)

// newTestRootContext builds a root CompilationContext backed by the shared
// fake schema resolver + REST mapper used across the compiler tests.
func newTestRootContext(t *testing.T) *CompilationContext {
	t.Helper()
	r, disco := testk8s.NewFakeResolver()
	rm := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(disco))
	return newRootContext(r, rm)
}

func TestNewRootContext(t *testing.T) {
	t.Parallel()
	ctx := newTestRootContext(t)
	assert.Nil(t, ctx.parent, "root context has no parent frame")
	assert.NotNil(t, ctx.schemaResolver, "resolver carried on the context")
	assert.NotNil(t, ctx.restMapper, "REST mapper carried on the context")
	require.NotNil(t, ctx.fieldCache, "a fresh field cache is allocated per root")

	// Two roots get independent field caches (no cross-compile leakage).
	other := newTestRootContext(t)
	assert.NotSame(t, ctx.fieldCache, other.fieldCache)
}

// TestCompilationContext_BuildNode is the GVK-axis table: every node kind and
// every compile-time failure mode the context owns. It asserts the compiled
// Node's identity-bearing fields (kind, GVR, namespaced, dynamic) and whether
// a published schema was emitted.
func TestCompilationContext_BuildNode(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		node    *expv1alpha1.Node
		wantErr string
		check   func(t *testing.T, n *Node, sch *spec.Schema)
	}{
		{
			name: "static template resolves GVR, schema, and namespaced scope",
			node: &expv1alpha1.Node{ID: "cm", Template: rawExtensionFromObject(map[string]any{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]any{"name": "${'cm-' + 'x'}"},
				"data":     map[string]any{"k": "v"},
			})},
			check: func(t *testing.T, n *Node, sch *spec.Schema) {
				assert.Equal(t, NodeKindTemplate, n.Kind)
				assert.False(t, n.DynamicGVK)
				assert.Equal(t, "configmaps", n.GVR.Resource)
				assert.True(t, n.Namespaced)
				require.NotNil(t, sch, "static template publishes its target schema")
				assert.NotEmpty(t, n.Variables, "the CEL name fragment is parsed out")
			},
		},
		{
			name: "def node infers a schema and carries no GVR",
			node: &expv1alpha1.Node{ID: "cfg", Def: rawExtensionFromObject(map[string]any{
				"prefix": "team-", "count": 3,
			})},
			check: func(t *testing.T, n *Node, sch *spec.Schema) {
				assert.Equal(t, NodeKindDef, n.Kind)
				assert.False(t, n.DynamicGVK)
				assert.True(t, n.GVR.Empty(), "def has no target resource")
				assert.NotNil(t, sch, "def schema is inferred from literals")
			},
		},
		{
			name: "ref node resolves GVR and parses schemaless",
			node: &expv1alpha1.Node{ID: "p", Ref: &expv1alpha1.ExternalRef{
				APIVersion: "v1", Kind: "Pod",
				Metadata: expv1alpha1.ExternalRefMetadata{Name: "p"},
			}},
			check: func(t *testing.T, n *Node, sch *spec.Schema) {
				assert.Equal(t, NodeKindRef, n.Kind)
				assert.Equal(t, "pods", n.GVR.Resource)
				assert.NotNil(t, sch)
			},
		},
		{
			name: "watch node is a collection",
			node: &expv1alpha1.Node{ID: "pods", Watch: &expv1alpha1.WatchSpec{
				APIVersion: "v1", Kind: "Pod",
				Selector: map[string]string{"app": "x"},
			}},
			check: func(t *testing.T, n *Node, sch *spec.Schema) {
				assert.Equal(t, NodeKindWatch, n.Kind)
				assert.True(t, n.IsCollection(), "watch nodes expand into a list")
				assert.Equal(t, "pods", n.GVR.Resource)
			},
		},
		{
			name: "dynamic apiVersion flags the node and skips GVR/schema",
			node: &expv1alpha1.Node{ID: "dyn", Template: rawExtensionFromObject(map[string]any{
				"apiVersion": "${cfg.group}", "kind": "Widget",
				"metadata": map[string]any{"name": "w"},
			})},
			check: func(t *testing.T, n *Node, sch *spec.Schema) {
				assert.True(t, n.DynamicGVK)
				assert.True(t, n.GVR.Empty(), "no compile-time GVR for a dynamic node")
				assert.False(t, n.Namespaced)
				assert.Nil(t, sch, "dynamic node publishes no schema")
			},
		},
		{
			name: "dynamic kind also flags the node",
			node: &expv1alpha1.Node{ID: "dyn", Template: rawExtensionFromObject(map[string]any{
				"apiVersion": "example.com/v1", "kind": "${cfg.kind}",
				"metadata": map[string]any{"name": "w"},
			})},
			check: func(t *testing.T, n *Node, sch *spec.Schema) {
				assert.True(t, n.DynamicGVK)
				assert.Nil(t, sch)
			},
		},
		{
			name: "unknown GVK fails schema resolution",
			node: &expv1alpha1.Node{ID: "x", Template: rawExtensionFromObject(map[string]any{
				"apiVersion": "example.com/v1", "kind": "Widget",
				"metadata": map[string]any{"name": "w"},
			})},
			wantErr: "resolve schema",
		},
		{
			name: "cluster-scoped target with a namespace is rejected",
			node: &expv1alpha1.Node{ID: "crd", Template: rawExtensionFromObject(map[string]any{
				"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition",
				"metadata": map[string]any{"name": "widgets.example.com", "namespace": "nope"},
			})},
			wantErr: "cluster-scoped",
		},
		{
			name: "template missing metadata is rejected",
			node: &expv1alpha1.Node{ID: "x", Template: rawExtensionFromObject(map[string]any{
				"apiVersion": "v1", "kind": "ConfigMap",
			})},
			wantErr: "metadata",
		},
		{
			name: "template missing apiVersion is rejected",
			node: &expv1alpha1.Node{ID: "x", Template: rawExtensionFromObject(map[string]any{
				"kind":     "ConfigMap",
				"metadata": map[string]any{"name": "x"},
			})},
			wantErr: "missing required field",
		},
		{
			name: "non-Kubernetes version string is rejected",
			node: &expv1alpha1.Node{ID: "x", Template: rawExtensionFromObject(map[string]any{
				"apiVersion": "apps/foo", "kind": "Deployment",
				"metadata": map[string]any{"name": "x"},
			})},
			wantErr: "not a valid Kubernetes version",
		},
		{
			name: "dynamic template missing metadata is rejected",
			node: &expv1alpha1.Node{ID: "x", Template: rawExtensionFromObject(map[string]any{
				"apiVersion": "${cfg.group}", "kind": "Widget",
			})},
			wantErr: "metadata",
		},
		{
			name:    "node with no payload is rejected",
			node:    &expv1alpha1.Node{ID: "x"},
			wantErr: "no payload set",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := newTestRootContext(t)
			p := parser.New(ctx.fieldCache)
			built, sch, err := ctx.buildNode(p, tc.node, 0)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, built)
			assert.Equal(t, tc.node.ID, built.ID)
			if tc.check != nil {
				tc.check(t, built, sch)
			}
		})
	}
}

func TestIsDynamicGVK(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		payload map[string]any
		want    bool
	}{
		{name: "literal GVK", payload: map[string]any{"apiVersion": "v1", "kind": "ConfigMap"}, want: false},
		{name: "CEL in apiVersion", payload: map[string]any{"apiVersion": "${cfg.group}", "kind": "Widget"}, want: true},
		{name: "CEL in kind", payload: map[string]any{"apiVersion": "v1", "kind": "${cfg.kind}"}, want: true},
		{name: "templated apiVersion segment", payload: map[string]any{"apiVersion": "${cfg.group}/v1", "kind": "Widget"}, want: true},
		{name: "CEL elsewhere does not count", payload: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "${x}"}}, want: false},
		{name: "non-string apiVersion", payload: map[string]any{"apiVersion": 3, "kind": "ConfigMap"}, want: false},
		{name: "missing fields", payload: map[string]any{}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, isDynamicGVK(tc.payload))
		})
	}
}

func TestValidateDynamicTemplateStructure(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		obj     map[string]any
		wantErr string
	}{
		{
			name: "valid",
			obj: map[string]any{"apiVersion": "${g}", "kind": "${k}",
				"metadata": map[string]any{"name": "x"}},
		},
		{name: "nil payload", obj: nil, wantErr: "empty"},
		{name: "missing apiVersion", obj: map[string]any{"kind": "${k}", "metadata": map[string]any{"name": "x"}}, wantErr: "apiVersion"},
		{name: "empty kind", obj: map[string]any{"apiVersion": "${g}", "kind": "", "metadata": map[string]any{"name": "x"}}, wantErr: "kind"},
		{name: "missing metadata", obj: map[string]any{"apiVersion": "${g}", "kind": "${k}"}, wantErr: "metadata"},
		{name: "non-object metadata", obj: map[string]any{"apiVersion": "${g}", "kind": "${k}", "metadata": "oops"}, wantErr: "must be an object"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateDynamicTemplateStructure(tc.obj)
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}
