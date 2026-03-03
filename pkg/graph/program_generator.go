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
	"fmt"
	"slices"

	"github.com/google/cel-go/cel"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	krocel "github.com/kubernetes-sigs/kro/pkg/cel"
	"github.com/kubernetes-sigs/kro/pkg/cel/conversion"
	"github.com/kubernetes-sigs/kro/pkg/graph/dag"
)

// CompiledExpr is a linked expression paired with its compiled CEL program.
type CompiledExpr struct {
	Raw        string
	References []string
	Program    cel.Program
}

// Eval evaluates the compiled expression with the given context.
func (e *CompiledExpr) Eval(ctx map[string]any) (any, error) {
	out, _, err := e.Program.Eval(ctx)
	if err != nil {
		return nil, fmt.Errorf("eval %q: %w", e.Raw, err)
	}
	native, err := conversion.GoNativeType(out)
	if err != nil {
		return nil, fmt.Errorf("convert %q: %w", e.Raw, err)
	}
	return native, nil
}

// CompiledVariable is one compiled template/status field with evaluation metadata.
type CompiledVariable struct {
	Path       string
	Standalone bool
	Kind       FieldKind
	Exprs      []*CompiledExpr
}

// CompiledForEach is a compiled iterator expression.
type CompiledForEach struct {
	Name string
	Expr *CompiledExpr
}

// CompiledNodeMeta contains immutable metadata about a compiled node.
// Matches the old NodeMeta layout used by runtime and controller.
type CompiledNodeMeta struct {
	ID           string
	Index        int
	Type         NodeType
	GVR          schema.GroupVersionResource
	Namespaced   bool
	Dependencies []string
}

// CompiledNode is the executable representation of a resource node.
type CompiledNode struct {
	Meta     CompiledNodeMeta
	Template map[string]interface{}

	Variables   []*CompiledVariable
	ReadyWhen   []*CompiledExpr
	IncludeWhen []*CompiledExpr
	ForEach     []*CompiledForEach
}

// DeepCopy returns a deep copy of the CompiledNode.
// Template map is deep-copied. Compiled Programs are immutable and shared.
// Struct-level copies are made for Variables/Exprs so that per-runtime
// wrappers (expressionEvaluationState) never alias the same backing array.
func (n *CompiledNode) DeepCopy() *CompiledNode {
	if n == nil {
		return nil
	}
	out := *n
	out.Meta.Dependencies = slices.Clone(n.Meta.Dependencies)
	out.Variables = deepCopyVariables(n.Variables)
	out.ReadyWhen = deepCopyExprs(n.ReadyWhen)
	out.IncludeWhen = deepCopyExprs(n.IncludeWhen)
	out.ForEach = deepCopyForEach(n.ForEach)
	if n.Template != nil {
		out.Template = k8sruntime.DeepCopyJSON(n.Template)
	}
	return &out
}

func deepCopyExprs(src []*CompiledExpr) []*CompiledExpr {
	if src == nil {
		return nil
	}
	out := make([]*CompiledExpr, len(src))
	for i, e := range src {
		cp := *e
		cp.References = slices.Clone(e.References)
		out[i] = &cp
	}
	return out
}

func deepCopyVariables(src []*CompiledVariable) []*CompiledVariable {
	if src == nil {
		return nil
	}
	out := make([]*CompiledVariable, len(src))
	for i, v := range src {
		cp := *v
		cp.Exprs = deepCopyExprs(v.Exprs)
		out[i] = &cp
	}
	return out
}

func deepCopyForEach(src []*CompiledForEach) []*CompiledForEach {
	if src == nil {
		return nil
	}
	out := make([]*CompiledForEach, len(src))
	for i, fe := range src {
		cp := *fe
		if fe.Expr != nil {
			exprCp := *fe.Expr
			exprCp.References = slices.Clone(fe.Expr.References)
			cp.Expr = &exprCp
		}
		out[i] = &cp
	}
	return out
}

// CompiledInstance is the executable representation of the instance node.
type CompiledInstance struct {
	InstanceMeta
	StatusFields []*CompiledVariable
	StatusTypes  map[string]*cel.Type
	TypeProvider *krocel.DeclTypeProvider
}

// CompiledRGD is the program-generator-stage output consumed by the assembler.
type CompiledRGD struct {
	Instance         *CompiledInstance
	Nodes            []*CompiledNode
	DAG              *dag.DirectedAcyclicGraph[string]
	TopologicalOrder []string
}

type programGenerator struct{}

func newProgramGenerator() ProgramGenerator { return &programGenerator{} }

// Generate takes LinkedRGD + TypeCheckResult, generates every expression into
// a cel.Program, and returns a CompiledRGD.
func (g *programGenerator) Generate(linked *LinkedRGD, tc *TypeCheckResult) (*CompiledRGD, error) {
	// Compile nodes
	compiled := make([]*CompiledNode, 0, len(linked.Nodes))
	for _, ln := range linked.Nodes {
		cr, err := g.generateNode(ln, tc)
		if err != nil {
			return nil, terminal("compiler", err)
		}
		compiled = append(compiled, cr)
	}

	// Compile instance status
	inst, err := g.generateInstance(linked.Instance, tc)
	if err != nil {
		return nil, terminal("compiler", err)
	}

	return &CompiledRGD{
		Instance:         inst,
		Nodes:            compiled,
		DAG:              linked.DAG,
		TopologicalOrder: linked.TopologicalOrder,
	}, nil
}

func (g *programGenerator) generateNode(lr *LinkedNode, tc *TypeCheckResult) (*CompiledNode, error) {
	env := tc.Env
	if compileEnv := tc.NodeCompileEnvs[lr.ID]; compileEnv != nil {
		env = compileEnv
	}

	fields, err := compileFields(lr.Fields, env, lr.ID, tc.CheckedExprs)
	if err != nil {
		return nil, err
	}

	readyEnv := tc.Env
	if readyWhenEnv := tc.NodeReadyWhenEnvs[lr.ID]; readyWhenEnv != nil {
		readyEnv = readyWhenEnv
	}

	readyWhen, err := compileExprs(lr.ReadyWhen, readyEnv, lr.ID, "readyWhen", tc.CheckedExprs)
	if err != nil {
		return nil, err
	}

	includeWhen, err := compileExprs(lr.IncludeWhen, tc.Env, lr.ID, "includeWhen", tc.CheckedExprs)
	if err != nil {
		return nil, err
	}

	forEach, err := g.generateForEach(lr.ForEach, tc.Env, lr.ID, tc.CheckedExprs)
	if err != nil {
		return nil, err
	}

	return &CompiledNode{
		Meta: CompiledNodeMeta{
			ID:           lr.ID,
			Index:        lr.Index,
			Type:         lr.Type,
			GVR:          lr.GVR,
			Namespaced:   lr.Namespaced,
			Dependencies: lr.Dependencies,
		},
		Template:    lr.Template,
		Variables:   fields,
		ReadyWhen:   readyWhen,
		IncludeWhen: includeWhen,
		ForEach:     forEach,
	}, nil
}

func (g *programGenerator) generateInstance(li *LinkedInstance, tc *TypeCheckResult) (*CompiledInstance, error) {
	fields, err := compileFields(li.StatusFields, tc.Env, "status", tc.CheckedExprs)
	if err != nil {
		return nil, err
	}
	return &CompiledInstance{
		InstanceMeta: li.InstanceMeta,
		StatusFields: fields,
		StatusTypes:  tc.StatusTypes,
		TypeProvider: tc.TypeProvider,
	}, nil
}

func (g *programGenerator) generateForEach(
	linked []*LinkedForEach,
	env *cel.Env,
	resourceID string,
	checkedExprs map[*LinkedExpr]*cel.Ast,
) ([]*CompiledForEach, error) {
	out := make([]*CompiledForEach, 0, len(linked))
	for _, lf := range linked {
		ce, err := compile(env, lf.Expr, checkedExprs)
		if err != nil {
			return nil, fmt.Errorf("resource %q forEach %q: %w", resourceID, lf.Name, err)
		}
		out = append(out, &CompiledForEach{Name: lf.Name, Expr: ce})
	}
	return out, nil
}

func compileFields(
	linked []*LinkedField,
	env *cel.Env,
	resourceID string,
	checkedExprs map[*LinkedExpr]*cel.Ast,
) ([]*CompiledVariable, error) {
	out := make([]*CompiledVariable, 0, len(linked))
	for _, lf := range linked {
		exprs, err := compileExprs(lf.Exprs, env, resourceID, lf.Path, checkedExprs)
		if err != nil {
			return nil, err
		}
		out = append(out, &CompiledVariable{
			Path:       lf.Path,
			Standalone: lf.Standalone,
			Kind:       lf.Kind,
			Exprs:      exprs,
		})
	}
	return out, nil
}

func compileExprs(
	linked []*LinkedExpr,
	env *cel.Env,
	resourceID, context string,
	checkedExprs map[*LinkedExpr]*cel.Ast,
) ([]*CompiledExpr, error) {
	out := make([]*CompiledExpr, 0, len(linked))
	for _, le := range linked {
		ce, err := compile(env, le, checkedExprs)
		if err != nil {
			return nil, fmt.Errorf("resource %q %s: %w", resourceID, context, err)
		}
		out = append(out, ce)
	}
	return out, nil
}

func compile(env *cel.Env, le *LinkedExpr, checkedExprs map[*LinkedExpr]*cel.Ast) (*CompiledExpr, error) {
	checked, ok := checkedExprs[le]
	if !ok {
		return nil, fmt.Errorf("missing type-checked AST for expression %q", le.Raw)
	}
	prog, err := env.Program(checked)
	if err != nil {
		return nil, fmt.Errorf("compile %q: %w", le.Raw, err)
	}
	return &CompiledExpr{
		Raw:        le.Raw,
		References: le.References,
		Program:    prog,
	}, nil
}
