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
	"fmt"

	"github.com/google/cel-go/cel"
	"k8s.io/kube-openapi/pkg/validation/spec"

	krocel "github.com/kubernetes-sigs/kro/pkg/cel"
	"github.com/kubernetes-sigs/kro/pkg/graph/defschema"
)

// inferDefSchemas types every def node that has expressions. Defs are visited
// in topological order, so each one is compiled against an environment in which
// everything it references already has its real type. The schema of a def is
// built from its literal structure plus the checked output type of each
// expression, and is added to celSchemas for the nodes that follow.
func (ctx *CompilationContext) inferDefSchemas(
	nodes map[string]*Node,
	nodeSchemas map[string]*spec.Schema,
	celSchemas map[string]*spec.Schema,
	topo []string,
	ancestors []string,
) error {
	for _, id := range topo {
		n := nodes[id]
		if n.Kind != NodeKindDef || nodeSchemas[id] != nil {
			continue
		}
		dynIDs, dynListIDs := schemalessIdentifiers(nodes, celSchemas, ancestors)
		env, provider, err := krocel.TypedEnvironmentWithIDsAndProvider(
			celSchemas,
			dynIDs,
			krocel.WithListVariables(dynListIDs),
			krocel.WithRuntimeLibrary(false),
		)
		if err != nil {
			return fmt.Errorf("compile node %q: build typed CEL environment: %w", id, err)
		}
		bc := newBuildContext(env, provider, ctx.fieldCache, ctx.costLimit)
		iteratorTypes, err := validateAndCompileForEach(bc, n)
		if err != nil {
			return fmt.Errorf("compile node %q: %w", id, err)
		}
		compileEnv, err := bc.extendWithIterators(bc.env, iteratorTypes)
		if err != nil {
			return fmt.Errorf("compile node %q: extend env with iterators: %w", id, err)
		}
		exprTypes := make(map[string]*cel.Type, len(n.Variables))
		for _, v := range n.Variables {
			checked, err := bc.compile(compileEnv, v.Expression)
			if err != nil {
				return fmt.Errorf("compile node %q: variable at %q (%q): %w", id, v.Path, v.Expression.UserExpression(), err)
			}
			exprTypes[v.Path] = checked.OutputType()
		}
		sch, err := defschema.Infer(n.Object.Object, exprTypes, provider)
		if err != nil {
			return fmt.Errorf("compile node %q: infer def schema: %w", id, err)
		}
		nodeSchemas[id] = sch
		if n.IsCollection() {
			celSchemas[id] = ctx.fieldCache.WrapAsList(sch)
		} else {
			celSchemas[id] = sch
		}
	}
	return nil
}
