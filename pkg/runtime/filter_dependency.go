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

package runtime

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"

	"github.com/google/cel-go/cel"

	krocel "github.com/kubernetes-sigs/kro/pkg/cel"
	"github.com/kubernetes-sigs/kro/pkg/cel/ast"
	"github.com/kubernetes-sigs/kro/pkg/graph"
)

// preprocessNodeDependencies filters a node's dependencies and updates Expression.References
// to match. This is the preprocessing step that should be called before wiring dependencies.
func preprocessNodeDependencies(g *graph.Graph, node *graph.Node, instanceData map[string]any) error {
	originalDeps := node.Meta.Dependencies
	filteredDeps, err := filterDependenciesWithCache(g, node, instanceData, originalDeps)
	if err != nil {
		return err
	}
	node.Meta.Dependencies = filteredDeps

	removedDeps := make(map[string]bool)
	for _, dep := range originalDeps {
		removedDeps[dep] = true
	}
	for _, dep := range filteredDeps {
		delete(removedDeps, dep)
	}

	for _, expr := range node.IncludeWhen {
		expr.References = removeReferences(expr.References, removedDeps)
	}
	for _, expr := range node.ReadyWhen {
		expr.References = removeReferences(expr.References, removedDeps)
	}
	for _, dim := range node.ForEach {
		dim.Expression.References = removeReferences(dim.Expression.References, removedDeps)
	}
	for _, v := range node.Variables {
		v.Expression.References = removeReferences(v.Expression.References, removedDeps)
	}

	return nil
}

func filterDependenciesWithCache(
	g *graph.Graph,
	node *graph.Node,
	instanceData map[string]any,
	declaredDeps []string,
) ([]string, error) {
	specHash, err := hashInstanceData(instanceData)
	if err != nil {
		return nil, fmt.Errorf("hash instance data: %w", err)
	}
	cacheKey := specHash + ":" + node.Meta.ID

	if cached, ok := g.SchemaHashToProcessedDeps.Load(cacheKey); ok {
		return cached.([]string), nil
	}

	filteredDeps, err := filterDependencies(node, instanceData, declaredDeps)
	if err != nil {
		return nil, err
	}

	g.SchemaHashToProcessedDeps.Store(cacheKey, filteredDeps)
	return filteredDeps, nil
}

func hashInstanceData(data map[string]any) (string, error) {
	jsonBytes, err := json.Marshal(data)
	if err != nil {
		return "", fmt.Errorf("marshal instance data: %w", err)
	}
	h := fnv.New64a()
	h.Write(jsonBytes)
	return hex.EncodeToString(h.Sum(nil)), nil
}

// removeReferences returns a new slice with denySet items removed.
func removeReferences(refs []string, denySet map[string]bool) []string {
	var filtered []string
	for _, ref := range refs {
		if !denySet[ref] {
			filtered = append(filtered, ref)
		}
	}
	return filtered
}

// filterDependencies uses CEL partial evaluation to determine which dependencies
// are actually needed. Binds instance object (spec+metadata) and simplifies conditionals
// (e.g., "schema.spec.useDB ? db : cache" with useDB=true → "db"), then returns only
// the dependencies referenced in simplified expressions.
func filterDependencies(
	node *graph.Node,
	instanceData map[string]any,
	declaredDeps []string,
) ([]string, error) {
	if !shouldOptimizeDependencies(node) {
		return declaredDeps, nil
	}

	allIdentifiers := append([]string{graph.SchemaVarName}, declaredDeps...)
	env, err := krocel.DefaultEnvironment(krocel.WithResourceIDs(allIdentifiers))
	if err != nil {
		return nil, fmt.Errorf("create CEL environment: %w", err)
	}

	partialEval, err := newPartialEvaluator(env, instanceData, declaredDeps)
	if err != nil {
		return nil, fmt.Errorf("create partial evaluator: %w", err)
	}

	inspector := ast.NewInspectorWithEnv(env, allIdentifiers)
	referencedResources := make(map[string]struct{})
	for _, exprStr := range getExpressions(node) {
		refs, err := extractReferencesFromExpression(partialEval, inspector, exprStr)
		if err != nil {
			return nil, err
		}
		for _, ref := range refs {
			referencedResources[ref] = struct{}{}
		}
	}

	var filteredDeps []string
	for _, dep := range declaredDeps {
		if _, referenced := referencedResources[dep]; referenced {
			filteredDeps = append(filteredDeps, dep)
		}
	}

	return filteredDeps, nil
}

// getExpressions collects all expression strings from a node that need dependency analysis.
func getExpressions(node *graph.Node) []string {
	var exprs []string
	for _, v := range node.Variables {
		exprs = append(exprs, v.Expression.Original)
	}
	for _, expr := range node.IncludeWhen {
		exprs = append(exprs, expr.Original)
	}
	for _, expr := range node.ReadyWhen {
		exprs = append(exprs, expr.Original)
	}
	for _, iter := range node.ForEach {
		exprs = append(exprs, iter.Expression.Original)
	}
	return exprs
}

// shouldOptimizeDependencies returns true if the node has non-schema dependencies and expressions.
func shouldOptimizeDependencies(node *graph.Node) bool {
	hasNonSchemaDep := false
	for _, dep := range node.Meta.Dependencies {
		if dep != graph.SchemaVarName {
			hasNonSchemaDep = true
			break
		}
	}
	if !hasNonSchemaDep {
		return false
	}

	return len(getExpressions(node)) > 0
}

// partialEvaluator simplifies CEL expressions by binding schema values and using
// CEL's partial evaluation to eliminate dead branches in conditionals.
type partialEvaluator struct {
	env        *cel.Env
	activation any
}

func newPartialEvaluator(env *cel.Env, instanceData map[string]any, resourceIDs []string) (*partialEvaluator, error) {
	// Create attribute patterns for all resource IDs (mark them as unknown)
	patterns := make([]*cel.AttributePatternType, 0, len(resourceIDs))
	for _, resourceID := range resourceIDs {
		patterns = append(patterns, cel.AttributePattern(resourceID))
	}

	// Create partial activation: schema is bound, resources are unknown
	activation, err := cel.PartialVars(
		map[string]any{graph.SchemaVarName: instanceData},
		patterns...,
	)
	if err != nil {
		return nil, fmt.Errorf("create partial activation: %w", err)
	}

	return &partialEvaluator{
		env:        env,
		activation: activation,
	}, nil
}

// simplifyExpression attempts partial evaluation on the expression, iterating
// until a fixed point is reached (no further simplification possible).
// CEL's partial eval doesn't recursively simplify nested conditionals, so we
// must iterate.
func (pe *partialEvaluator) simplifyExpression(exprStr string) (string, error) {
	const maxIterations = 5
	current := exprStr

	for i := 0; i < maxIterations; i++ {
		ast, issues := pe.env.Compile(current)
		if issues.Err() != nil {
			// Can't compile - this might happen if expression references
			// undefined identifiers, which is fine (we'll inspect as-is)
			return current, nil
		}

		prg, err := pe.env.Program(ast, cel.EvalOptions(cel.OptTrackState, cel.OptPartialEval))
		if err != nil {
			return current, nil
		}

		_, details, err := prg.Eval(pe.activation)
		if err != nil {
			return current, nil
		}

		if details == nil || details.State() == nil {
			return current, nil
		}

		residualAst, err := pe.env.ResidualAst(ast, details)
		if err != nil {
			return current, nil
		}

		residualStr, err := cel.AstToString(residualAst)
		if err != nil {
			return current, nil
		}

		if residualStr == current {
			return current, nil
		}

		current = residualStr
	}

	return current, nil
}

func extractReferencesFromExpression(
	partialEval *partialEvaluator,
	inspector *ast.Inspector,
	exprStr string,
) ([]string, error) {
	simplified, err := partialEval.simplifyExpression(exprStr)
	if err != nil {
		return nil, fmt.Errorf("simplify expression: %w", err)
	}

	inspection, err := inspector.Inspect(simplified)
	if err != nil {
		return nil, fmt.Errorf("inspect simplified expression: %w", err)
	}

	var refs []string
	for _, dep := range inspection.ResourceDependencies {
		if dep.ID != graph.SchemaVarName { // Avoid adding schema as a node dependency.
			refs = append(refs, dep.ID)
		}
	}

	return refs, nil
}
