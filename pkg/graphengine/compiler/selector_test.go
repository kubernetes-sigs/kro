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
	"k8s.io/apimachinery/pkg/runtime"

	expv1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/graph/parser"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/testutil/generator"
)

// TestValidateRefSelector pins the compile-time shape check on a ref node's
// schemaless metadata.selector.
func TestValidateRefSelector(t *testing.T) {
	t.Parallel()

	refPayload := func(selector any) map[string]any {
		md := map[string]any{"namespace": "default"}
		if selector != nil {
			md["selector"] = selector
		}
		return map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": md}
	}

	cases := []struct {
		name    string
		payload map[string]any
		wantErr string
	}{
		// --- accepted ---
		{name: "no selector (single-object ref)", payload: refPayload(nil)},
		{
			name:    "explicit null selector is treated as absent",
			payload: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"selector": nil}},
		},
		{name: "empty selector object matches everything by contract", payload: refPayload(map[string]any{})},
		{name: "literal matchLabels", payload: refPayload(map[string]any{"matchLabels": map[string]any{"tier": "db"}})},
		{
			name: "literal matchExpressions",
			payload: refPayload(map[string]any{"matchExpressions": []any{
				map[string]any{"key": "tier", "operator": "In", "values": []any{"db", "cache"}},
			}}),
		},
		{name: "whole selector is a standalone CEL expression", payload: refPayload("${schema.spec.sel}")},
		{name: "matchLabels is a CEL expression", payload: refPayload(map[string]any{"matchLabels": "${schema.spec.labels}"})},
		{name: "matchLabels value is a CEL expression", payload: refPayload(map[string]any{"matchLabels": map[string]any{"tier": "${schema.spec.tier}"}})},
		{
			name: "matchExpressions values carry CEL",
			payload: refPayload(map[string]any{"matchExpressions": []any{
				map[string]any{"key": "tier", "operator": "In", "values": []any{"${schema.spec.tier}"}},
			}}),
		},
		{
			// Only known at apply time; left to the executor's decode.
			name: "CEL-bearing selector with a bad operator is deferred to apply time",
			payload: refPayload(map[string]any{"matchExpressions": []any{
				map[string]any{"key": "${schema.spec.key}", "operator": "Bogus"},
			}}),
		},

		// --- rejected ---
		{
			name:    "misspelled matchLabels key",
			payload: refPayload(map[string]any{"matchLabel": map[string]any{"tier": "db"}}),
			wantErr: "matchLabel",
		},
		{
			name:    "bare label map without matchLabels",
			payload: refPayload(map[string]any{"tier": "db"}),
			wantErr: "schema not found for field tier",
		},
		{
			name:    "unknown key next to a CEL matchLabels",
			payload: refPayload(map[string]any{"matchLabels": "${schema.spec.labels}", "bogus": "x"}),
			wantErr: "schema not found for field bogus",
		},
		{
			name:    "matchLabels is a plain string",
			payload: refPayload(map[string]any{"matchLabels": "tier=db"}),
			wantErr: "expected object type for path metadata.selector.matchLabels, got string",
		},
		{
			name:    "matchLabels value is not a string",
			payload: refPayload(map[string]any{"matchLabels": map[string]any{"tier": int64(3)}}),
			wantErr: "expected string type for path metadata.selector.matchLabels.tier",
		},
		{
			name:    "matchExpressions item is a scalar",
			payload: refPayload(map[string]any{"matchExpressions": []any{"tier"}}),
			wantErr: "expected object type for path metadata.selector.matchExpressions[0]",
		},
		{
			name: "unknown field in a matchExpressions item",
			payload: refPayload(map[string]any{"matchExpressions": []any{
				map[string]any{"key": "tier", "operator": "Exists", "bogus": true},
			}}),
			wantErr: "schema not found for field bogus",
		},
		{
			name: "literal bad operator",
			payload: refPayload(map[string]any{"matchExpressions": []any{
				map[string]any{"key": "tier", "operator": "Bogus"},
			}}),
			wantErr: "not a valid selector operator",
		},
		{
			name: "literal In operator without values",
			payload: refPayload(map[string]any{"matchExpressions": []any{
				map[string]any{"key": "tier", "operator": "In"},
			}}),
			wantErr: "invalid label selector",
		},
		{
			name:    "literal malformed label key",
			payload: refPayload(map[string]any{"matchLabels": map[string]any{"bad key!": "db"}}),
			wantErr: "invalid label selector",
		},
		{name: "plain string selector", payload: refPayload("tier=db"), wantErr: "must be a Kubernetes LabelSelector object"},
		{name: "templated string selector", payload: refPayload("prefix-${schema.spec.sel}"), wantErr: "must be a Kubernetes LabelSelector object"},
		{name: "unterminated CEL selector", payload: refPayload("${schema.spec.sel"), wantErr: "invalid expression"},
		{name: "list selector", payload: refPayload([]any{"tier"}), wantErr: "must be a Kubernetes LabelSelector object"},
		{name: "numeric selector", payload: refPayload(int64(42)), wantErr: "must be a Kubernetes LabelSelector object"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := newTestRootContext(t)
			err := validateRefSelector(parser.New(ctx.fieldCache), tc.payload)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// TestCompile_RefSelectorShape pins that the shape check runs on both the
// static- and dynamic-GVK ref compile paths and names the node.
func TestCompile_RefSelectorShape(t *testing.T) {
	t.Parallel()

	typo := runtime.RawExtension{Raw: []byte(`{"matchLabel":{"tier":"db"}}`)}

	t.Run("static ref with a misspelled matchLabels key is a compile error", func(t *testing.T) {
		t.Parallel()
		g := generator.NewGraph("g",
			generator.WithRef("coll", &expv1alpha1.ExternalRef{
				APIVersion: "v1", Kind: "ConfigMap",
				Metadata: expv1alpha1.ExternalRefMetadata{Selector: typo},
			}),
		)
		_, err := newTestCompiler(t).Compile(g)
		require.Error(t, err)
		assert.Contains(t, err.Error(), `build node "coll"`)
		assert.Contains(t, err.Error(), "matchLabel")
	})

	t.Run("dynamic-GVK ref with a misspelled matchLabels key is a compile error", func(t *testing.T) {
		t.Parallel()
		g := generator.NewGraph("g",
			generator.WithDef("crd", map[string]any{"kind": "Widget"}),
			generator.WithRef("coll", &expv1alpha1.ExternalRef{
				APIVersion: "example.com/v1", Kind: "${crd.kind}",
				Metadata: expv1alpha1.ExternalRefMetadata{Selector: typo},
			}),
		)
		_, err := newTestCompiler(t).Compile(g)
		require.Error(t, err)
		assert.Contains(t, err.Error(), `build node "coll"`)
		assert.Contains(t, err.Error(), "matchLabel")
	})

	t.Run("a whole-selector CEL expression compiles and marks a collection", func(t *testing.T) {
		t.Parallel()
		g := generator.NewGraph("g",
			generator.WithDef("input", map[string]any{"sel": map[string]any{"matchLabels": map[string]any{"tier": "db"}}}),
			generator.WithRef("coll", &expv1alpha1.ExternalRef{
				APIVersion: "v1", Kind: "ConfigMap",
				Metadata: expv1alpha1.ExternalRefMetadata{
					Selector: runtime.RawExtension{Raw: []byte(`"${input.sel}"`)},
				},
			}),
		)
		prog, err := newTestCompiler(t).Compile(g)
		require.NoError(t, err)
		assert.True(t, prog.Nodes["coll"].IsCollection())
		assert.Equal(t, []string{"input"}, prog.Nodes["coll"].HardDepIDs())
	})
}
