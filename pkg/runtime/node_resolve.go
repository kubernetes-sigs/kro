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
	"fmt"
	"maps"
	"slices"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/kubernetes-sigs/kro/pkg/graph"
	"github.com/kubernetes-sigs/kro/pkg/graph/variable"
	"github.com/kubernetes-sigs/kro/pkg/runtime/resolver"
)

func (n *Node) hardResolveSingleResource(vars []*variable.ResourceField) ([]*unstructured.Unstructured, error) {
	baseExprs, _ := n.exprSetsForVars(vars)
	values, _, err := n.evaluateExprsFiltered(baseExprs, false)
	if err != nil {
		return nil, fmt.Errorf("node %q: %w", n.Spec.Meta.ID, err)
	}

	desired := n.Spec.Template.DeepCopy()
	res := resolver.NewResolver(desired.Object, values)
	summary := res.Resolve(toFieldDescriptors(vars))
	if len(summary.Errors) > 0 {
		return nil, fmt.Errorf("node %q: resolve errors: %v", n.Spec.Meta.ID, summary.Errors)
	}

	return []*unstructured.Unstructured{desired}, nil
}

func (n *Node) hardResolveCollection(vars []*variable.ResourceField, setIndexLabel bool) ([]*unstructured.Unstructured, error) {
	baseExprs, iterExprs := n.exprSetsForVars(vars)
	baseValues, _, err := n.evaluateExprsFiltered(baseExprs, false)
	if err != nil {
		if !IsDataPending(err) {
			err = fmt.Errorf("node %q base eval: %w", n.Spec.Meta.ID, err)
		}
		return nil, err
	}

	items, err := n.evaluateForEach()
	if err != nil {
		return nil, err
	}

	collectionSize.Observe(float64(len(items)))

	if len(items) == 0 {
		// Resolved empty collection: return non-nil empty slice to distinguish
		// from unresolved (n.desired == nil).
		return []*unstructured.Unstructured{}, nil
	}

	// Build a map from expression string to expressionEvaluationState for iteration expressions.
	iterExprStates := make(map[string]*expressionEvaluationState, len(n.templateExprs))
	for _, expr := range n.templateExprs {
		if expr.Kind.IsIteration() {
			iterExprStates[expr.Expression.Original] = expr
		}
	}

	// Only build context for dependencies referenced by iteration expressions.
	iterNeeded := make(map[string]struct{})
	for exprStr := range iterExprs {
		if state, ok := iterExprStates[exprStr]; ok {
			for _, ref := range state.Expression.References {
				iterNeeded[ref] = struct{}{}
			}
		}
	}
	baseCtx := n.buildContext(slices.Collect(maps.Keys(iterNeeded))...)

	expanded := make([]*unstructured.Unstructured, 0, len(items))
	for idx, iterCtx := range items {
		values := make(map[string]any, len(baseValues)+len(iterExprs))
		maps.Copy(values, baseValues)

		// Merge iterator values into context.
		ctx := make(map[string]any, len(baseCtx)+len(iterCtx))
		maps.Copy(ctx, baseCtx)
		maps.Copy(ctx, iterCtx)

		// Evaluate iteration expressions (not cached - different context per iteration).
		for exprStr := range iterExprs {
			exprState := iterExprStates[exprStr]
			val, err := exprState.Expression.Eval(ctx)
			if err != nil {
				if isCELDataPending(err) {
					return nil, ErrDataPending
				}
				return nil, fmt.Errorf("collection iteration eval %q: %w", exprStr, err)
			}
			values[exprStr] = val
		}

		desired := n.Spec.Template.DeepCopy()
		res := resolver.NewResolver(desired.Object, values)
		summary := res.Resolve(toFieldDescriptors(vars))
		if len(summary.Errors) > 0 {
			return nil, fmt.Errorf("node %q collection resolve: resolve errors: %v", n.Spec.Meta.ID, summary.Errors)
		}
		if setIndexLabel {
			setCollectionIndexLabel(desired, idx)
		}
		expanded = append(expanded, desired)
	}

	if err := validateUniqueIdentities(expanded); err != nil {
		return nil, fmt.Errorf("node %q identity collision: %w", n.Spec.Meta.ID, err)
	}

	return expanded, nil
}

// softResolve evaluates expressions using best-effort partial resolution.
// It ignores ErrDataPending (returns partial result) but propagates fatal errors.
// Used for instance status where we populate as many fields as possible.
//
// Only fields where ALL expressions are resolved will be included in the result.
// This prevents template strings like "${expr}" from leaking into the status.
func (n *Node) softResolve() ([]*unstructured.Unstructured, error) {
	values, _, err := n.evaluateExprsFiltered(nil, true) // soft: continue on pending
	if err != nil {
		return nil, err
	}

	// Filter to only fully-resolvable fields (expression value available)
	var resolvable []variable.FieldDescriptor
	for _, v := range n.templateVars {
		if _, ok := values[v.Expression.Original]; ok {
			resolvable = append(resolvable, v.FieldDescriptor)
		}
	}

	// Resolve on template copy, then copy resolved values to empty desired
	template := n.Spec.Template.DeepCopy()
	templateRes := resolver.NewResolver(template.Object, values)
	summary := templateRes.Resolve(resolvable)

	// Resolution errors on filtered fields indicate bugs (template/path mismatch)
	if len(summary.Errors) > 0 {
		return nil, fmt.Errorf("failed to resolve status fields: %v", summary.Errors)
	}

	// Build desired with only successfully resolved fields
	desired := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"status": map[string]interface{}{},
		},
	}
	destRes := resolver.NewResolver(desired.Object, nil)
	for _, result := range summary.Results {
		if result.Resolved {
			if err := destRes.UpsertValueAtPath(result.Path, result.Replaced); err != nil {
				return nil, fmt.Errorf("failed to set status field %s: %w", result.Path, err)
			}
		}
	}

	return []*unstructured.Unstructured{desired}, nil
}

// evaluateExprsFiltered evaluates non-iteration expressions and returns the values map.
// If exprs is nil, all expressions are evaluated. If exprs is empty, returns empty values.
// If continueOnPending is true, it skips expressions that return ErrDataPending.
// Returns (values, hasPending, error).
func (n *Node) evaluateExprsFiltered(exprs map[string]struct{}, continueOnPending bool) (map[string]any, bool, error) {
	if exprs != nil && len(exprs) == 0 {
		return map[string]any{}, false, nil
	}

	// Compute the union of referenced dependencies across all expressions to
	// evaluate, so buildContext only wraps needed deps with schema-aware values.
	needed := n.neededDeps(exprs)
	ctx := n.buildContext(needed...)

	capacity := len(n.templateExprs)
	if exprs != nil {
		capacity = len(exprs)
	}
	values := make(map[string]any, capacity)
	var hasPending bool
	for _, expr := range n.templateExprs {
		if expr.Kind.IsIteration() {
			continue
		}
		if exprs != nil {
			if _, ok := exprs[expr.Expression.Original]; !ok {
				continue
			}
		}
		val, err := expr.EvalCached(ctx)
		if err != nil {
			if isCELDataPending(err) {
				hasPending = true
				if continueOnPending {
					continue
				}
				return nil, true, fmt.Errorf("failed to evaluate expression: %w (%w)", err, ErrDataPending)
			}
			return nil, false, err
		}
		values[expr.Expression.Original] = val
	}
	return values, hasPending, nil
}

// evaluateForEach evaluates forEach dimensions and returns iterator contexts.
func (n *Node) evaluateForEach() ([]map[string]any, error) {
	if len(n.Spec.ForEach) == 0 {
		return nil, nil
	}

	// Only build context for dependencies referenced by forEach expressions.
	needed := make(map[string]struct{})
	for _, expr := range n.forEachExprs {
		for _, ref := range expr.Expression.References {
			needed[ref] = struct{}{}
		}
	}
	ctx := n.buildContext(slices.Collect(maps.Keys(needed))...)

	dimensions := make([]evaluatedDimension, len(n.Spec.ForEach))
	for i, dim := range n.Spec.ForEach {
		values, err := evalListExpr(n.forEachExprs[i], ctx)
		if err != nil {
			if isCELDataPending(err) {
				return nil, ErrDataPending
			}
			return nil, fmt.Errorf("forEach %q: %w", dim.Name, err)
		}
		if len(values) == 0 {
			return nil, nil
		}
		dimensions[i] = evaluatedDimension{name: dim.Name, values: values}
	}

	product, err := cartesianProduct(dimensions, n.rgdConfig.MaxCollectionSize)
	if err != nil {
		return nil, err
	}

	return product, nil
}

func (n *Node) templateVarsForPaths(paths []string) []*variable.ResourceField {
	if len(paths) == 0 {
		return n.templateVars
	}

	pathSet := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		pathSet[p] = struct{}{}
	}

	result := make([]*variable.ResourceField, 0, len(n.templateVars))
	for _, v := range n.templateVars {
		if _, ok := pathSet[v.Path]; ok {
			result = append(result, v)
		}
	}
	return result
}

func (n *Node) exprSetsForVars(
	vars []*variable.ResourceField,
) (map[string]struct{}, map[string]struct{}) {
	baseExprs := make(map[string]struct{})
	iterExprs := make(map[string]struct{})
	if len(vars) == 0 {
		return baseExprs, iterExprs
	}

	exprKinds := make(map[string]variable.ResourceVariableKind, len(n.templateExprs))
	for _, expr := range n.templateExprs {
		exprKinds[expr.Expression.Original] = expr.Kind
	}

	for _, v := range vars {
		if kind, ok := exprKinds[v.Expression.Original]; ok && kind.IsIteration() {
			iterExprs[v.Expression.Original] = struct{}{}
		} else {
			baseExprs[v.Expression.Original] = struct{}{}
		}
	}
	return baseExprs, iterExprs
}

// normalizeNamespaces inherits the instance namespace onto namespaced children
// that don't specify one. Cluster-scoped instances must resolve an explicit
// namespace for namespaced children, otherwise reconciliation cannot safely
// address them.
func (n *Node) normalizeNamespaces(objs []*unstructured.Unstructured) error {
	if !n.Spec.Meta.Namespaced {
		return nil
	}
	ns := n.deps[graph.InstanceNodeID].observed[0].GetNamespace()
	for _, obj := range objs {
		if obj.GetNamespace() != "" {
			continue
		}
		if ns == "" {
			return fmt.Errorf(
				"node %q is namespaced and must resolve metadata.namespace when the instance is cluster-scoped",
				n.Spec.Meta.ID,
			)
		}
		if obj.GetNamespace() == "" {
			obj.SetNamespace(ns)
		}
	}
	return nil
}
