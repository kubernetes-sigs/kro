// Copyright 2025 The Kubernetes Authors.
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

package graph

import (
	"testing"

	"github.com/stretchr/testify/require"

	krocel "github.com/kubernetes-sigs/kro/pkg/cel"
	"github.com/kubernetes-sigs/kro/pkg/cel/ast"
)

func linkerTestInspector(t *testing.T, nodeIDs []string) *ast.Inspector {
	t.Helper()
	allIDs := make([]string, 0, len(nodeIDs)+2)
	allIDs = append(allIDs, nodeIDs...)
	allIDs = append(allIDs, SchemaVarName, EachVarName)
	env, err := krocel.DefaultEnvironment(krocel.WithResourceIDs(allIDs))
	require.NoError(t, err)
	return ast.NewInspectorWithEnv(env, allIDs)
}

func linkerTestNode(id string, index int) *ResolvedNode {
	return &ResolvedNode{
		ParsedNode: ParsedNode{
			NodeMeta: NodeMeta{
				ID:    id,
				Index: index,
				Type:  NodeTypeScalar,
			},
		},
	}
}

func TestLinker_Link_Cases(t *testing.T) {
	tests := []struct {
		name    string
		rgd     *ResolvedRGD
		wantErr string
	}{
		{
			name: "duplicate vertex IDs fail",
			rgd: &ResolvedRGD{
				Instance: &ParsedInstance{},
				Nodes: []*ResolvedNode{
					linkerTestNode("dup", 0),
					linkerTestNode("dup", 1),
				},
			},
			wantErr: `vertex "dup"`,
		},
		{
			name: "dependency cycle fails topological sort",
			rgd: &ResolvedRGD{
				Instance: &ParsedInstance{},
				Nodes: []*ResolvedNode{
					{
						ParsedNode: ParsedNode{
							NodeMeta: NodeMeta{ID: "a", Index: 0},
							Fields: []*RawField{
								{Path: "spec.ref", Standalone: true, Exprs: []*RawExpr{{Raw: "b"}}},
							},
						},
					},
					{
						ParsedNode: ParsedNode{
							NodeMeta: NodeMeta{ID: "b", Index: 1},
							Fields: []*RawField{
								{Path: "spec.ref", Standalone: true, Exprs: []*RawExpr{{Raw: "a"}}},
							},
						},
					},
				},
			},
			wantErr: "cycle",
		},
		{
			name: "forEach expression inspect error",
			rgd: &ResolvedRGD{
				Instance: &ParsedInstance{},
				Nodes: []*ResolvedNode{
					{
						ParsedNode: ParsedNode{
							NodeMeta: NodeMeta{ID: "pods", Index: 0, Type: NodeTypeCollection},
							Fields: []*RawField{
								{Path: MetadataNamePath, Standalone: true, Exprs: []*RawExpr{{Raw: "item"}}},
							},
							ForEach: []*RawForEach{
								{Name: "item", Expr: &RawExpr{Raw: "1 +"}},
							},
						},
					},
				},
			},
			wantErr: `forEach "item"`,
		},
		{
			name: "includeWhen expression inspect error",
			rgd: &ResolvedRGD{
				Instance: &ParsedInstance{},
				Nodes: []*ResolvedNode{
					{
						ParsedNode: ParsedNode{
							NodeMeta: NodeMeta{ID: "cfg", Index: 0},
							IncludeWhen: []*RawExpr{
								{Raw: "1 +"},
							},
						},
					},
				},
			},
			wantErr: "includeWhen",
		},
		{
			name: "readyWhen expression inspect error",
			rgd: &ResolvedRGD{
				Instance: &ParsedInstance{},
				Nodes: []*ResolvedNode{
					{
						ParsedNode: ParsedNode{
							NodeMeta: NodeMeta{ID: "cfg", Index: 0},
							ReadyWhen: []*RawExpr{
								{Raw: "1 +"},
							},
						},
					},
				},
			},
			wantErr: "readyWhen",
		},
		{
			name: "happy path",
			rgd: &ResolvedRGD{
				Instance: &ParsedInstance{},
				Nodes: []*ResolvedNode{
					{
						ParsedNode: ParsedNode{
							NodeMeta: NodeMeta{ID: "cfg", Index: 0},
							Fields: []*RawField{
								{Path: "spec.value", Standalone: true, Exprs: []*RawExpr{{Raw: "schema"}}},
							},
						},
					},
				},
			},
		},
	}

	l := &linker{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := l.Link(tt.rgd)
			if tt.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.wantErr)
				require.True(t, IsTerminal(err))
				return
			}

			require.NoError(t, err)
			require.NotNil(t, got)
			require.Len(t, got.Nodes, 1)
			require.Len(t, got.TopologicalOrder, 1)
		})
	}
}

func TestValidateScope_Cases(t *testing.T) {
	tests := []struct {
		name     string
		refs     []string
		allowed  []string
		wantErr  bool
		wantPart string
	}{
		{
			name:    "no references is valid",
			refs:    nil,
			allowed: []string{SchemaVarName},
		},
		{
			name:    "single allowed reference",
			refs:    []string{SchemaVarName},
			allowed: []string{SchemaVarName},
		},
		{
			name:     "single disallowed reference",
			refs:     []string{"res"},
			allowed:  []string{SchemaVarName},
			wantErr:  true,
			wantPart: "unknown identifiers",
		},
		{
			name:     "mixed allowed and disallowed references",
			refs:     []string{SchemaVarName, "res"},
			allowed:  []string{SchemaVarName},
			wantErr:  true,
			wantPart: "unknown identifiers",
		},
		{
			name:     "multiple disallowed references are all rejected",
			refs:     []string{"res", "each"},
			allowed:  []string{SchemaVarName},
			wantErr:  true,
			wantPart: "unknown identifiers",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateScope(&LinkedExpr{Raw: "x", References: tt.refs}, tt.allowed, "includeWhen", "cfg")
			if tt.wantErr {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.wantPart)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestLinker_linkNode_ScopeViolations_Cases(t *testing.T) {
	tests := []struct {
		name    string
		node    *ResolvedNode
		wantErr string
	}{
		{
			name: "includeWhen rejects non-schema references",
			node: &ResolvedNode{
				ParsedNode: ParsedNode{
					NodeMeta: NodeMeta{ID: "cfg", Index: 0},
					IncludeWhen: []*RawExpr{
						{Raw: "res"},
					},
				},
			},
			wantErr: "unknown identifiers",
		},
		{
			name: "readyWhen on scalar rejects non-self references",
			node: &ResolvedNode{
				ParsedNode: ParsedNode{
					NodeMeta: NodeMeta{ID: "cfg", Index: 0, Type: NodeTypeScalar},
					ReadyWhen: []*RawExpr{
						{Raw: "res"},
					},
				},
			},
			wantErr: "unknown identifiers",
		},
		{
			name: "readyWhen on collection rejects non-each references",
			node: &ResolvedNode{
				ParsedNode: ParsedNode{
					NodeMeta: NodeMeta{ID: "cfg", Index: 0, Type: NodeTypeCollection},
					ReadyWhen: []*RawExpr{
						{Raw: "res"},
					},
				},
			},
			wantErr: "unknown identifiers",
		},
		{
			name: "includeWhen allows schema",
			node: &ResolvedNode{
				ParsedNode: ParsedNode{
					NodeMeta: NodeMeta{ID: "cfg", Index: 0},
					IncludeWhen: []*RawExpr{
						{Raw: "schema"},
					},
				},
			},
		},
	}

	l := &linker{}
	inspector := linkerTestInspector(t, []string{"cfg", "res"})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := l.linkNode(tt.node, inspector)
			if tt.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestLinker_linkInstance_Cases(t *testing.T) {
	tests := []struct {
		name         string
		raw          string
		statusFields []*RawField
		wantErr      string
	}{
		{
			name:    "inspect failure",
			raw:     "1 +",
			wantErr: `status "message"`,
		},
		{
			name:    "schema reference rejected",
			raw:     "schema",
			wantErr: "cannot reference schema",
		},
		{
			name:    "each reference rejected as unknown resource",
			raw:     "each",
			wantErr: `unknown resource "each"`,
		},
		{
			name:    "unknown identifier rejected",
			raw:     "ghost",
			wantErr: "unknown identifiers",
		},
		{
			name:    "must reference at least one resource",
			raw:     "'static'",
			wantErr: "must reference at least one resource",
		},
		{
			name: "success",
			raw:  "res",
		},
		{
			name: "requires resource reference per field",
			statusFields: []*RawField{
				{
					Path:       "constant",
					Standalone: true,
					Exprs:      []*RawExpr{{Raw: "'static'"}},
				},
				{
					Path:       "fromResource",
					Standalone: true,
					Exprs:      []*RawExpr{{Raw: "res.status.value"}},
				},
			},
			wantErr: `status "constant": must reference at least one resource`,
		},
	}

	l := &linker{}
	inspector := linkerTestInspector(t, []string{"res"})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			statusFields := tt.statusFields
			if statusFields == nil {
				statusFields = []*RawField{
					{
						Path:       "message",
						Standalone: false,
						Exprs:      []*RawExpr{{Raw: tt.raw}},
					},
				}
			}
			inst := &ParsedInstance{StatusFields: statusFields}

			got, err := l.linkInstance(inst, inspector, []string{"res"})
			if tt.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.wantErr)
				return
			}

			require.NoError(t, err)
			require.NotNil(t, got)
			require.Len(t, got.StatusFields, len(statusFields))
			for i := range statusFields {
				require.Equal(t, statusFields[i].Path, got.StatusFields[i].Path)
			}
		})
	}
}

func TestLinker_linkExprAndConditions_Cases(t *testing.T) {
	l := &linker{}
	inspector := linkerTestInspector(t, []string{"res"})

	exprTests := []struct {
		name    string
		expr    *RawExpr
		wantErr string
	}{
		{
			name:    "inspect error",
			expr:    &RawExpr{Raw: "1 +"},
			wantErr: `inspect "1 +"`,
		},
		{
			name:    "unknown function error",
			expr:    &RawExpr{Raw: "doesNotExist()"},
			wantErr: "unknown functions",
		},
		{
			name: "success",
			expr: &RawExpr{Raw: "res"},
		},
	}

	for _, tt := range exprTests {
		t.Run(tt.name, func(t *testing.T) {
			deps, iterRefs, linked, err := l.linkExpr(inspector, tt.expr, nil)
			if tt.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.wantErr)
				return
			}

			require.NoError(t, err)
			require.NotNil(t, linked)
			require.Equal(t, tt.expr.Raw, linked.Raw)
			require.Equal(t, []string{"res"}, deps)
			require.Empty(t, iterRefs)
		})
	}

	conditionTests := []struct {
		name    string
		raws    []*RawExpr
		wantErr string
	}{
		{
			name:    "linkConditions propagates linkExpr errors",
			raws:    []*RawExpr{{Raw: "1 +"}},
			wantErr: `inspect "1 +"`,
		},
		{
			name: "linkConditions success",
			raws: []*RawExpr{{Raw: "res"}},
		},
	}

	for _, tt := range conditionTests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := l.linkConditions(inspector, tt.raws, nil)
			if tt.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.wantErr)
				return
			}

			require.NoError(t, err)
			require.Len(t, got, 1)
			require.Equal(t, "res", got[0].Raw)
		})
	}
}

func TestLinker_linkNode_IteratorAndScopeMatrix(t *testing.T) {
	tests := []struct {
		name    string
		node    *ResolvedNode
		wantErr string
	}{
		{
			name: "includeWhen rejects self reference",
			node: &ResolvedNode{
				ParsedNode: ParsedNode{
					NodeMeta: NodeMeta{ID: "cfg", Index: 0, Type: NodeTypeScalar},
					IncludeWhen: []*RawExpr{
						{Raw: "cfg"},
					},
				},
			},
			wantErr: `resource "cfg" includeWhen: references unknown identifiers: [cfg]`,
		},
		{
			name: "readyWhen scalar rejects each reference",
			node: &ResolvedNode{
				ParsedNode: ParsedNode{
					NodeMeta: NodeMeta{ID: "cfg", Index: 0, Type: NodeTypeScalar},
					ReadyWhen: []*RawExpr{
						{Raw: "each"},
					},
				},
			},
			wantErr: `resource "cfg" readyWhen: references unknown identifiers: [each]`,
		},
		{
			name: "readyWhen collection allows each reference",
			node: &ResolvedNode{
				ParsedNode: ParsedNode{
					NodeMeta: NodeMeta{ID: "cfg", Index: 0, Type: NodeTypeCollection},
					ReadyWhen: []*RawExpr{
						{Raw: "each"},
					},
				},
			},
		},
		{
			name: "readyWhen collection rejects self reference",
			node: &ResolvedNode{
				ParsedNode: ParsedNode{
					NodeMeta: NodeMeta{ID: "cfg", Index: 0, Type: NodeTypeCollection},
					ReadyWhen: []*RawExpr{
						{Raw: "cfg"},
					},
				},
			},
			wantErr: `resource "cfg" readyWhen: references unknown identifiers: [cfg]`,
		},
		{
			name: "forEach requires all iterators in identity fields",
			node: &ResolvedNode{
				ParsedNode: ParsedNode{
					NodeMeta: NodeMeta{ID: "cfg", Index: 0, Type: NodeTypeCollection},
					Fields: []*RawField{
						{Path: MetadataNamePath, Standalone: true, Exprs: []*RawExpr{{Raw: "item"}}},
					},
					ForEach: []*RawForEach{
						{Name: "item", Expr: &RawExpr{Raw: "res"}},
						{Name: "zone", Expr: &RawExpr{Raw: "res"}},
					},
				},
			},
			wantErr: "missing: [zone]",
		},
		{
			name: "forEach iterators can be consumed through metadata.namespace when namespaced",
			node: &ResolvedNode{
				Namespaced: true,
				ParsedNode: ParsedNode{
					NodeMeta: NodeMeta{ID: "cfg", Index: 0, Type: NodeTypeCollection},
					Fields: []*RawField{
						{Path: MetadataNamespacePath, Standalone: true, Exprs: []*RawExpr{{Raw: "item"}}},
					},
					ForEach: []*RawForEach{
						{Name: "item", Expr: &RawExpr{Raw: "res"}},
					},
				},
			},
		},
		{
			name: "metadata.namespace does not count when resource is cluster-scoped",
			node: &ResolvedNode{
				Namespaced: false,
				ParsedNode: ParsedNode{
					NodeMeta: NodeMeta{ID: "cfg", Index: 0, Type: NodeTypeCollection},
					Fields: []*RawField{
						{Path: MetadataNamespacePath, Standalone: true, Exprs: []*RawExpr{{Raw: "item"}}},
					},
					ForEach: []*RawForEach{
						{Name: "item", Expr: &RawExpr{Raw: "res"}},
					},
				},
			},
			wantErr: "missing: [item]",
		},
		{
			name: "forEach expressions cannot reference iterator variables",
			node: &ResolvedNode{
				ParsedNode: ParsedNode{
					NodeMeta: NodeMeta{ID: "cfg", Index: 0, Type: NodeTypeCollection},
					Fields: []*RawField{
						{Path: MetadataNamePath, Standalone: true, Exprs: []*RawExpr{{Raw: "item + zone"}}},
					},
					ForEach: []*RawForEach{
						{Name: "item", Expr: &RawExpr{Raw: "res"}},
						{Name: "zone", Expr: &RawExpr{Raw: "item"}},
					},
				},
			},
			wantErr: `forEach "zone" cannot reference other iterators [item]`,
		},
		{
			name: "readyWhen rejects iterator variable and only allows each for collections",
			node: &ResolvedNode{
				ParsedNode: ParsedNode{
					NodeMeta: NodeMeta{ID: "cfg", Index: 0, Type: NodeTypeCollection},
					Fields: []*RawField{
						{Path: MetadataNamePath, Standalone: true, Exprs: []*RawExpr{{Raw: "item"}}},
					},
					ForEach: []*RawForEach{
						{Name: "item", Expr: &RawExpr{Raw: "res"}},
					},
					ReadyWhen: []*RawExpr{
						{Raw: "item"},
					},
				},
			},
			wantErr: `resource "cfg" readyWhen: references unknown identifiers: [item]`,
		},
		{
			name: "includeWhen rejects iterator variable and only allows schema",
			node: &ResolvedNode{
				ParsedNode: ParsedNode{
					NodeMeta: NodeMeta{ID: "cfg", Index: 0, Type: NodeTypeCollection},
					Fields: []*RawField{
						{Path: MetadataNamePath, Standalone: true, Exprs: []*RawExpr{{Raw: "item"}}},
					},
					ForEach: []*RawForEach{
						{Name: "item", Expr: &RawExpr{Raw: "res"}},
					},
					IncludeWhen: []*RawExpr{
						{Raw: "item"},
					},
				},
			},
			wantErr: `resource "cfg" includeWhen: references unknown identifiers: [item]`,
		},
		{
			name: "includeWhen accepts schema with forEach iterators present",
			node: &ResolvedNode{
				ParsedNode: ParsedNode{
					NodeMeta: NodeMeta{ID: "cfg", Index: 0, Type: NodeTypeCollection},
					Fields: []*RawField{
						{Path: MetadataNamePath, Standalone: true, Exprs: []*RawExpr{{Raw: "item"}}},
					},
					ForEach: []*RawForEach{
						{Name: "item", Expr: &RawExpr{Raw: "res"}},
					},
					IncludeWhen: []*RawExpr{
						{Raw: "schema"},
					},
				},
			},
		},
	}

	l := &linker{}
	inspector := linkerTestInspector(t, []string{"cfg", "res"})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := l.linkNode(tt.node, inspector)
			if tt.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.wantErr)
				return
			}

			require.NoError(t, err)
			require.NotNil(t, got)
		})
	}
}
