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

	"github.com/google/cel-go/cel"
	"github.com/stretchr/testify/require"
	"k8s.io/kube-openapi/pkg/validation/spec"
)

func compilerTestEnv(t *testing.T) *cel.Env {
	t.Helper()
	env, err := cel.NewEnv()
	require.NoError(t, err)
	return env
}

func compilerTestCheckedAST(t *testing.T, env *cel.Env, raw string) *cel.Ast {
	t.Helper()
	parsed, iss := env.Parse(raw)
	if iss != nil {
		require.NoError(t, iss.Err())
	}
	checked, iss := env.Check(parsed)
	if iss != nil {
		require.NoError(t, iss.Err())
	}
	return checked
}

func compilerTestProgram(t *testing.T, env *cel.Env, raw string) cel.Program {
	t.Helper()
	checked := compilerTestCheckedAST(t, env, raw)
	prog, err := env.Program(checked)
	require.NoError(t, err)
	return prog
}

func compilerTestSchema() *spec.Schema {
	return &spec.Schema{
		SchemaProps: spec.SchemaProps{
			Type: []string{"object"},
			Properties: map[string]spec.Schema{
				"metadata": {
					SchemaProps: spec.SchemaProps{
						Type: []string{"object"},
						Properties: map[string]spec.Schema{
							"name": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
						},
					},
				},
			},
		},
	}
}

func compilerTestNodeExpressions() (map[string]*LinkedExpr, *LinkedNode) {
	fieldExpr := &LinkedExpr{Raw: "1"}
	readyExpr := &LinkedExpr{Raw: "true"}
	includeExpr := &LinkedExpr{Raw: "true"}
	forEachExpr := &LinkedExpr{Raw: "['x']"}

	node := &LinkedNode{
		NodeIdentity: NodeIdentity{
			NodeMeta: NodeMeta{
				ID:   "pods",
				Type: NodeTypeCollection,
			},
			Schema: compilerTestSchema(),
		},
		Fields: []*LinkedField{
			{
				Path:       "metadata.name",
				Standalone: true,
				Kind:       FieldDynamic,
				Exprs:      []*LinkedExpr{fieldExpr},
			},
		},
		ReadyWhen:   []*LinkedExpr{readyExpr},
		IncludeWhen: []*LinkedExpr{includeExpr},
		ForEach: []*LinkedForEach{
			{
				Name: "item",
				Expr: forEachExpr,
			},
		},
	}

	return map[string]*LinkedExpr{
		"field":   fieldExpr,
		"ready":   readyExpr,
		"include": includeExpr,
		"foreach": forEachExpr,
	}, node
}

func compilerTestLinkedRGD() (map[string]*LinkedExpr, *LinkedRGD) {
	nodeExpr := &LinkedExpr{Raw: "1"}
	statusExpr := &LinkedExpr{Raw: "'ok'"}

	linked := &LinkedRGD{
		Instance: &LinkedInstance{
			InstanceMeta: InstanceMeta{
				Kind: "Demo",
			},
			StatusFields: []*LinkedField{
				{
					Path:       "message",
					Standalone: false,
					Kind:       FieldDynamic,
					Exprs:      []*LinkedExpr{statusExpr},
				},
			},
		},
		Nodes: []*LinkedNode{
			{
				NodeIdentity: NodeIdentity{
					NodeMeta: NodeMeta{
						ID:   "cfg",
						Type: NodeTypeScalar,
					},
				},
				Fields: []*LinkedField{
					{
						Path:       "spec.replicas",
						Standalone: true,
						Kind:       FieldDynamic,
						Exprs:      []*LinkedExpr{nodeExpr},
					},
				},
			},
		},
		TopologicalOrder: []string{"cfg"},
	}

	return map[string]*LinkedExpr{
		"node":   nodeExpr,
		"status": statusExpr,
	}, linked
}

func TestCompiledExpr_Eval_Cases(t *testing.T) {
	env := compilerTestEnv(t)

	tests := []struct {
		name    string
		raw     string
		want    any
		wantErr string
	}{
		{
			name: "success",
			raw:  "1 + 2",
			want: int64(3),
		},
		{
			name:    "runtime eval error",
			raw:     "1 / 0",
			wantErr: `eval "1 / 0"`,
		},
		{
			name:    "native conversion error",
			raw:     "type(1)",
			wantErr: `convert "type(1)"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			expr := &CompiledExpr{
				Raw:     tt.raw,
				Program: compilerTestProgram(t, env, tt.raw),
			}
			got, err := expr.Eval(map[string]any{})
			if tt.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestCompiledNode_DeepCopy_Cases(t *testing.T) {
	tests := []struct {
		name string
		node *CompiledNode
	}{
		{
			name: "nil receiver",
			node: nil,
		},
		{
			name: "copies slices and template map",
			node: &CompiledNode{
				Meta: CompiledNodeMeta{
					Dependencies: []string{"dep-a"},
				},
				Template: map[string]interface{}{
					"metadata": map[string]interface{}{"name": "demo"},
				},
				Variables: []*CompiledVariable{
					{Path: "spec.value"},
				},
				ReadyWhen: []*CompiledExpr{
					{Raw: "true"},
				},
				IncludeWhen: []*CompiledExpr{
					{Raw: "true"},
				},
				ForEach: []*CompiledForEach{
					{Name: "item"},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.node.DeepCopy()
			if tt.node == nil {
				require.Nil(t, got)
				return
			}

			require.NotNil(t, got)
			require.Equal(t, tt.node.Meta.Dependencies, got.Meta.Dependencies)
			require.Equal(t, tt.node.Variables[0].Path, got.Variables[0].Path)
			require.Equal(t, tt.node.ReadyWhen[0].Raw, got.ReadyWhen[0].Raw)
			require.Equal(t, tt.node.IncludeWhen[0].Raw, got.IncludeWhen[0].Raw)
			require.Equal(t, tt.node.ForEach[0].Name, got.ForEach[0].Name)
			require.Equal(t, "demo", got.Template["metadata"].(map[string]interface{})["name"])

			tt.node.Meta.Dependencies[0] = "dep-b"
			tt.node.Variables[0] = &CompiledVariable{Path: "changed"}
			tt.node.ReadyWhen[0] = &CompiledExpr{Raw: "false"}
			tt.node.IncludeWhen[0] = &CompiledExpr{Raw: "false"}
			tt.node.ForEach[0] = &CompiledForEach{Name: "changed"}
			tt.node.Template["metadata"].(map[string]interface{})["name"] = "changed"
			tt.node.Template["newField"] = "x"

			require.Equal(t, "dep-a", got.Meta.Dependencies[0])
			require.Equal(t, "spec.value", got.Variables[0].Path)
			require.Equal(t, "true", got.ReadyWhen[0].Raw)
			require.Equal(t, "true", got.IncludeWhen[0].Raw)
			require.Equal(t, "item", got.ForEach[0].Name)
			require.Equal(t, "demo", got.Template["metadata"].(map[string]interface{})["name"])
			_, hasNewField := got.Template["newField"]
			require.False(t, hasNewField)
		})
	}
}

func TestCompile_Cases(t *testing.T) {
	env := compilerTestEnv(t)
	expr := &LinkedExpr{Raw: "1 + 1", References: []string{"cfg"}}
	checked := compilerTestCheckedAST(t, env, expr.Raw)

	tests := []struct {
		name       string
		checkedMap map[*LinkedExpr]*cel.Ast
		wantErr    string
	}{
		{
			name:       "missing checked AST",
			checkedMap: map[*LinkedExpr]*cel.Ast{},
			wantErr:    "missing type-checked AST",
		},
		{
			name: "program compilation error",
			checkedMap: map[*LinkedExpr]*cel.Ast{
				expr: nil,
			},
			wantErr: `compile "1 + 1"`,
		},
		{
			name: "success",
			checkedMap: map[*LinkedExpr]*cel.Ast{
				expr: checked,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := compile(env, expr, tt.checkedMap)
			if tt.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.wantErr)
				return
			}

			require.NoError(t, err)
			require.NotNil(t, got)
			require.Equal(t, expr.Raw, got.Raw)
			require.Equal(t, expr.References, got.References)
			require.NotNil(t, got.Program)
		})
	}
}

func TestCompileExprs_Cases(t *testing.T) {
	env := compilerTestEnv(t)
	expr := &LinkedExpr{Raw: "1"}
	checked := compilerTestCheckedAST(t, env, expr.Raw)

	tests := []struct {
		name       string
		checkedMap map[*LinkedExpr]*cel.Ast
		wantErr    string
	}{
		{
			name:       "missing checked AST wraps context",
			checkedMap: map[*LinkedExpr]*cel.Ast{},
			wantErr:    `resource "res" readyWhen`,
		},
		{
			name: "success",
			checkedMap: map[*LinkedExpr]*cel.Ast{
				expr: checked,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := compileExprs([]*LinkedExpr{expr}, env, "res", "readyWhen", tt.checkedMap)
			if tt.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.Len(t, got, 1)
		})
	}
}

func TestCompileFields_Cases(t *testing.T) {
	env := compilerTestEnv(t)
	expr := &LinkedExpr{Raw: "1"}
	field := &LinkedField{
		Path:       "spec.value",
		Standalone: true,
		Kind:       FieldDynamic,
		Exprs:      []*LinkedExpr{expr},
	}
	checked := compilerTestCheckedAST(t, env, expr.Raw)

	tests := []struct {
		name       string
		checkedMap map[*LinkedExpr]*cel.Ast
		wantErr    string
	}{
		{
			name:       "missing checked AST wraps field path",
			checkedMap: map[*LinkedExpr]*cel.Ast{},
			wantErr:    `resource "res" spec.value`,
		},
		{
			name: "success",
			checkedMap: map[*LinkedExpr]*cel.Ast{
				expr: checked,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := compileFields([]*LinkedField{field}, env, "res", tt.checkedMap)
			if tt.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.Len(t, got, 1)
			require.Equal(t, field.Path, got[0].Path)
		})
	}
}

func TestProgramGenerator_compileForEach_Cases(t *testing.T) {
	g := &programGenerator{}
	env := compilerTestEnv(t)
	expr := &LinkedExpr{Raw: "['a', 'b']"}
	linked := []*LinkedForEach{
		{
			Name: "item",
			Expr: expr,
		},
	}
	checked := compilerTestCheckedAST(t, env, expr.Raw)

	tests := []struct {
		name       string
		checkedMap map[*LinkedExpr]*cel.Ast
		wantErr    string
	}{
		{
			name:       "missing checked AST wraps forEach name",
			checkedMap: map[*LinkedExpr]*cel.Ast{},
			wantErr:    `resource "pods" forEach "item"`,
		},
		{
			name: "success",
			checkedMap: map[*LinkedExpr]*cel.Ast{
				expr: checked,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := g.generateForEach(linked, env, "pods", tt.checkedMap)
			if tt.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.Len(t, got, 1)
			require.Equal(t, "item", got[0].Name)
		})
	}
}

func TestProgramGenerator_compileInstance_Cases(t *testing.T) {
	g := &programGenerator{}
	env := compilerTestEnv(t)
	expr := &LinkedExpr{Raw: "'ok'"}
	checked := compilerTestCheckedAST(t, env, expr.Raw)
	inst := &LinkedInstance{
		InstanceMeta: InstanceMeta{
			Kind: "Demo",
		},
		StatusFields: []*LinkedField{
			{
				Path:       "message",
				Standalone: false,
				Kind:       FieldDynamic,
				Exprs:      []*LinkedExpr{expr},
			},
		},
	}

	tests := []struct {
		name       string
		checkedMap map[*LinkedExpr]*cel.Ast
		wantErr    string
	}{
		{
			name:       "missing checked AST",
			checkedMap: map[*LinkedExpr]*cel.Ast{},
			wantErr:    "missing type-checked AST",
		},
		{
			name: "success",
			checkedMap: map[*LinkedExpr]*cel.Ast{
				expr: checked,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tc := &TypeCheckResult{
				Env:          env,
				StatusTypes:  map[string]*cel.Type{"message": cel.StringType},
				CheckedExprs: tt.checkedMap,
			}
			got, err := g.generateInstance(inst, tc)
			if tt.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, got)
			require.Equal(t, "Demo", got.Kind)
			require.Len(t, got.StatusFields, 1)
			require.Equal(t, cel.StringType, got.StatusTypes["message"])
		})
	}
}

func TestProgramGenerator_compileNode_Cases(t *testing.T) {
	g := &programGenerator{}
	env := compilerTestEnv(t)
	exprs, node := compilerTestNodeExpressions()
	checkedAll := map[*LinkedExpr]*cel.Ast{
		exprs["field"]:   compilerTestCheckedAST(t, env, exprs["field"].Raw),
		exprs["ready"]:   compilerTestCheckedAST(t, env, exprs["ready"].Raw),
		exprs["include"]: compilerTestCheckedAST(t, env, exprs["include"].Raw),
		exprs["foreach"]: compilerTestCheckedAST(t, env, exprs["foreach"].Raw),
	}

	tests := []struct {
		name       string
		checkedMap map[*LinkedExpr]*cel.Ast
		wantErr    string
	}{
		{
			name: "missing field AST",
			checkedMap: map[*LinkedExpr]*cel.Ast{
				exprs["ready"]:   checkedAll[exprs["ready"]],
				exprs["include"]: checkedAll[exprs["include"]],
				exprs["foreach"]: checkedAll[exprs["foreach"]],
			},
			wantErr: `resource "pods" metadata.name`,
		},
		{
			name: "missing readyWhen AST",
			checkedMap: map[*LinkedExpr]*cel.Ast{
				exprs["field"]:   checkedAll[exprs["field"]],
				exprs["include"]: checkedAll[exprs["include"]],
				exprs["foreach"]: checkedAll[exprs["foreach"]],
			},
			wantErr: `resource "pods" readyWhen`,
		},
		{
			name: "missing includeWhen AST",
			checkedMap: map[*LinkedExpr]*cel.Ast{
				exprs["field"]:   checkedAll[exprs["field"]],
				exprs["ready"]:   checkedAll[exprs["ready"]],
				exprs["foreach"]: checkedAll[exprs["foreach"]],
			},
			wantErr: `resource "pods" includeWhen`,
		},
		{
			name: "missing forEach AST",
			checkedMap: map[*LinkedExpr]*cel.Ast{
				exprs["field"]:   checkedAll[exprs["field"]],
				exprs["ready"]:   checkedAll[exprs["ready"]],
				exprs["include"]: checkedAll[exprs["include"]],
			},
			wantErr: `resource "pods" forEach "item"`,
		},
		{
			name: "success",
			checkedMap: map[*LinkedExpr]*cel.Ast{
				exprs["field"]:   checkedAll[exprs["field"]],
				exprs["ready"]:   checkedAll[exprs["ready"]],
				exprs["include"]: checkedAll[exprs["include"]],
				exprs["foreach"]: checkedAll[exprs["foreach"]],
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tc := &TypeCheckResult{
				Env: env,
				IteratorTypes: map[string]map[string]*cel.Type{
					"pods": {
						"item": cel.StringType,
					},
				},
				CheckedExprs: tt.checkedMap,
			}
			got, err := g.generateNode(node, tc)
			if tt.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, got)
			require.Len(t, got.Variables, 1)
			require.Len(t, got.ReadyWhen, 1)
			require.Len(t, got.IncludeWhen, 1)
			require.Len(t, got.ForEach, 1)
		})
	}
}

func TestProgramGenerator_Compile_Cases(t *testing.T) {
	g := &programGenerator{}
	env := compilerTestEnv(t)
	exprs, linked := compilerTestLinkedRGD()
	checkedNode := compilerTestCheckedAST(t, env, exprs["node"].Raw)
	checkedStatus := compilerTestCheckedAST(t, env, exprs["status"].Raw)

	tests := []struct {
		name       string
		checkedMap map[*LinkedExpr]*cel.Ast
		wantErr    string
	}{
		{
			name: "node compile failure is terminal",
			checkedMap: map[*LinkedExpr]*cel.Ast{
				exprs["status"]: checkedStatus,
			},
			wantErr: "compiler",
		},
		{
			name: "instance compile failure is terminal",
			checkedMap: map[*LinkedExpr]*cel.Ast{
				exprs["node"]: checkedNode,
			},
			wantErr: "compiler",
		},
		{
			name: "success",
			checkedMap: map[*LinkedExpr]*cel.Ast{
				exprs["node"]:   checkedNode,
				exprs["status"]: checkedStatus,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tc := &TypeCheckResult{
				Env:          env,
				StatusTypes:  map[string]*cel.Type{"message": cel.StringType},
				CheckedExprs: tt.checkedMap,
			}
			got, err := g.Generate(linked, tc)
			if tt.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.wantErr)
				require.True(t, IsTerminal(err))
				return
			}
			require.NoError(t, err)
			require.NotNil(t, got)
			require.Len(t, got.Nodes, 1)
			require.Len(t, got.Instance.StatusFields, 1)
		})
	}
}
