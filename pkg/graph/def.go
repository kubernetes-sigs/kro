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

package graph

import (
	"fmt"

	"github.com/google/cel-go/cel"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/kube-openapi/pkg/validation/spec"

	"github.com/kubernetes-sigs/kro/pkg/cel/ast"
	"github.com/kubernetes-sigs/kro/pkg/graph/defschema"
	"github.com/kubernetes-sigs/kro/pkg/graph/parser"
	"github.com/kubernetes-sigs/kro/pkg/graph/schema"
	"github.com/kubernetes-sigs/kro/pkg/graph/variable"
)

// buildDefNode builds a def node: computed data that creates and reads no
// cluster object. Its CEL expressions are extracted schemalessly. The node's
// schema is inferred later (inferDefSchemas) from the checked output type of
// each expression, because it depends on the types of the nodes it references.
func buildDefNode(rs ResourceSpec) (*Node, error) {
	fieldDescriptors, _, err := parser.ParseSchemalessResource(rs.Object)
	if err != nil {
		return nil, fmt.Errorf("failed to parse def %s: %w", rs.ID, err)
	}
	vars := make([]*variable.ResourceField, 0, len(fieldDescriptors))
	for _, fd := range fieldDescriptors {
		vars = append(vars, &variable.ResourceField{
			Kind:            variable.ResourceVariableKindStatic,
			FieldDescriptor: fd,
		})
	}
	mods, err := parseNodeModifiers(rs)
	if err != nil {
		return nil, err
	}
	return &Node{
		Meta: NodeMeta{
			ID:    rs.ID,
			Index: rs.Order,
			Type:  NodeTypeDef,
		},
		Template:    &unstructured.Unstructured{Object: rs.Object},
		Variables:   vars,
		IncludeWhen: mods.includeWhen,
		ReadyWhen:   mods.readyWhen,
		ForEach:     mods.forEach,
	}, nil
}

// inferDefSchemas computes the schema of every def node and records it in
// schemas. Def nodes are visited in topological order so that a def can
// reference resources and earlier defs with their real types.
func (b *Builder) inferDefSchemas(
	c *schema.Cache,
	nodes map[string]*Node,
	schemas map[string]*spec.Schema,
	topologicalOrder []string,
	inspector *ast.Inspector,
	instanceSchema *spec.Schema,
) error {
	for _, id := range topologicalOrder {
		node := nodes[id]
		if node.Meta.Type != NodeTypeDef {
			continue
		}
		celSchemas := collectNodeSchemas(c, nodes, schemas)
		celSchemas[SchemaVarName] = instanceSchema
		bc, err := b.typedBuildContext(c, celSchemas)
		if err != nil {
			return fmt.Errorf("failed to create CEL environment for def %q: %w", id, err)
		}
		sch, err := inferDefSchema(bc, node, inspector)
		if err != nil {
			return fmt.Errorf("failed to infer def %q: %w", id, err)
		}
		schemas[id] = sch
	}
	return nil
}

// inferDefSchema compiles the def's expressions and builds an object schema
// from the literal structure of the def, using each expression's checked
// output type for expression-valued fields.
func inferDefSchema(bc *buildContext, node *Node, inspector *ast.Inspector) (*spec.Schema, error) {
	iteratorTypes, err := validateAndCompileForEach(bc, node, inspector)
	if err != nil {
		return nil, err
	}
	env := bc.env
	if len(iteratorTypes) > 0 {
		opts := make([]cel.EnvOption, 0, len(iteratorTypes))
		for name, typ := range iteratorTypes {
			opts = append(opts, cel.Variable(name, typ))
		}
		if env, err = env.Extend(opts...); err != nil {
			return nil, fmt.Errorf("failed to extend CEL environment with iterator types: %w", err)
		}
	}
	exprTypes := make(map[string]*cel.Type, len(node.Variables))
	for _, v := range node.Variables {
		checked, err := bc.compile(env, v.Expression)
		if err != nil {
			return nil, fmt.Errorf("failed to compile expression %q at path %q: %w", v.Expression.UserExpression(), v.Path, err)
		}
		exprTypes[v.Path] = checked.OutputType()
	}
	return defschema.Infer(node.Template.Object, exprTypes, bc.typeProvider)
}
