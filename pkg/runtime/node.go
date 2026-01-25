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

	krocel "github.com/kubernetes-sigs/kro/pkg/cel"
	"github.com/kubernetes-sigs/kro/pkg/graph"
	"github.com/kubernetes-sigs/kro/pkg/graph/variable"
	"github.com/kubernetes-sigs/kro/pkg/runtime/resolver"
)

// Node is the mutable runtime handle that wraps an immutable graph.Node.
// Each reconciliation creates fresh Node instances.
type Node struct {
	Spec *graph.Node

	// deps holds pointers to only the nodes this node depends on.
	// Includes "schema" pointing to the instance node for schema.* expressions.
	deps map[string]*Node

	desired  []*unstructured.Unstructured
	observed []*unstructured.Unstructured

	includeWhenExprs []*expressionEvaluationState
	readyWhenExprs   []*expressionEvaluationState
	forEachExprs     []*expressionEvaluationState
	templateExprs    []*expressionEvaluationState
	templateVars     []*variable.ResourceField
}

// IsIgnored reports whether this node should be skipped entirely.
// It is true when:
//   - any dependency is ignored (contagious)
//   - any includeWhen expression evaluates to false
//
// Results are memoized via expression caching - once an includeWhen
// expression evaluates to false, it stays false for this runtime instance.
func (n *Node) IsIgnored() (bool, error) {
	// Instance nodes cannot be ignored - they represent the user's CR.
	if n.Spec.Meta.Type == graph.NodeTypeInstance {
		return false, nil
	}

	// Check if any dependency is ignored (contagious).
	for _, dep := range n.deps {
		ignored, err := dep.IsIgnored()
		if err != nil {
			return false, err
		}
		if ignored {
			return true, nil
		}
	}

	if len(n.includeWhenExprs) == 0 {
		return false, nil
	}

	singles, collections, _ := n.contextDependencyIDs(nil)
	env, err := buildEnv(singles, collections)
	if err != nil {
		return false, err
	}
	ctx := n.buildContext()

	for _, expr := range n.includeWhenExprs {
		val, err := evalBoolExpr(env, expr, ctx)
		if err != nil {
			return false, fmt.Errorf("includeWhen %q: %w", expr.Expression, err)
		}
		if !val {
			return true, nil
		}
	}

	return false, nil
}

// GetDesired computes and returns the desired state(s) for this node.
// Results are cached - subsequent calls return the cached value.
// Behavior varies by node type:
//   - Resource: strict evaluation, fails fast on any error
//   - Collection: strict evaluation with forEach expansion
//   - Instance: best-effort partial evaluation
//   - External: resolves template (for name/namespace CEL), caller reads instead of applies
//
// Note: The caller should call IsIgnored() before GetDesired() for resource nodes.
func (n *Node) GetDesired() ([]*unstructured.Unstructured, error) {
	// Return cached result if available.
	if n.desired != nil {
		return n.desired, nil
	}

	// For resource types, block until all dependencies are ready.
	// This enforces readyWhen semantics: dependents wait for parents.
	if n.Spec.Meta.Type != graph.NodeTypeInstance {
		for depID, dep := range n.deps {
			if depID == graph.InstanceNodeID {
				continue
			}
			ready, err := dep.IsReady()
			if err != nil {
				return nil, err
			}
			if !ready {
				return nil, ErrDataPending
			}
		}
	}

	var result []*unstructured.Unstructured
	var err error

	switch n.Spec.Meta.Type {
	case graph.NodeTypeInstance:
		result, err = n.softResolve()
	case graph.NodeTypeCollection:
		result, err = n.hardResolveCollection()
	case graph.NodeTypeResource, graph.NodeTypeExternal:
		// External refs resolve like resources (for name/namespace CEL),
		// but the caller reads instead of applies.
		result, err = n.hardResolveSingleResource()
	default:
		panic(fmt.Sprintf("unknown node type: %v", n.Spec.Meta.Type))
	}

	if err == nil {
		n.desired = result
	}
	return result, err
}

func (n *Node) hardResolveSingleResource() ([]*unstructured.Unstructured, error) {
	if n.Spec.Template == nil {
		return nil, nil
	}

	values, _, err := n.evaluateExprs(false) // hard: fail on pending
	if err != nil {
		if !IsDataPending(err) {
			err = fmt.Errorf("node %q: %w", n.Spec.Meta.ID, err)
		}
		return nil, err
	}

	result, err := n.resolveInTemplate(n.Spec.Template, values)
	if err != nil {
		return nil, fmt.Errorf("node %q: %w", n.Spec.Meta.ID, err)
	}

	return []*unstructured.Unstructured{result}, nil
}

func (n *Node) hardResolveCollection() ([]*unstructured.Unstructured, error) {
	// Evaluate non-iteration expressions (static + dynamic) once.
	baseValues, _, err := n.evaluateExprs(false) // hard: fail on pending
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

	if len(items) == 0 {
		return nil, nil
	}

	// Build iteration env with all iterator names added as singles (dyn, not list).
	singles, collections, _ := n.contextDependencyIDs(nil)
	iteratorNames := make([]string, 0, len(n.Spec.ForEach))
	for _, dim := range n.Spec.ForEach {
		iteratorNames = append(iteratorNames, dim.Name)
	}
	allSingles := append(singles, iteratorNames...)
	iterEnv, err := buildEnv(allSingles, collections)
	if err != nil {
		return nil, err
	}
	baseCtx := n.buildContext()

	expanded := make([]*unstructured.Unstructured, 0, len(items))
	for idx, iterCtx := range items {
		values := make(map[string]any, len(baseValues)+len(n.templateExprs))
		maps.Copy(values, baseValues)

		// Merge iterator values into context.
		ctx := make(map[string]any, len(baseCtx)+len(iterCtx))
		maps.Copy(ctx, baseCtx)
		maps.Copy(ctx, iterCtx)

		for _, expr := range n.templateExprs {
			if !expr.Kind.IsIteration() {
				continue
			}
			// Iteration expressions must NOT be cached - they depend on per-item iterator context.
			// Use evalRawCEL directly instead of evalExprAny.
			val, err := evalRawCEL(iterEnv, expr.Expression, ctx)
			if err != nil {
				if isCELDataPending(err) {
					return nil, ErrDataPending
				}
				return nil, fmt.Errorf("collection iteration eval %q: %w", expr.Expression, err)
			}
			values[expr.Expression] = val
		}

		template := n.Spec.Template.DeepCopy()
		res := resolver.NewResolver(template.Object, values)
		summary := res.Resolve(toFieldDescriptors(n.templateVars))
		if len(summary.Errors) > 0 {
			return nil, fmt.Errorf("node %q collection resolve: %v", n.Spec.Meta.ID, summary.Errors)
		}

		setCollectionIndexLabel(template, idx)
		expanded = append(expanded, template)
	}

	if err := validateUniqueIdentities(expanded, n.Spec.Meta.Namespaced); err != nil {
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
	if n.Spec.Template == nil {
		return nil, nil
	}

	values, _, err := n.evaluateExprs(true) // soft: continue on pending
	if err != nil {
		return nil, err
	}

	// Filter to only fully-resolvable fields (all expressions available)
	var resolvable []variable.FieldDescriptor
	for _, v := range n.templateVars {
		complete := true
		for _, expr := range v.Expressions {
			if _, ok := values[expr]; !ok {
				complete = false
				break
			}
		}
		if complete {
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

// evaluateExprs evaluates non-iteration expressions and returns the values map.
// If continueOnPending is true, it skips expressions that return ErrDataPending.
// Returns (values, hasPending, error).
func (n *Node) evaluateExprs(continueOnPending bool) (map[string]any, bool, error) {
	singles, collections, _ := n.contextDependencyIDs(nil)
	env, err := buildEnv(singles, collections)
	if err != nil {
		return nil, false, err
	}
	ctx := n.buildContext()

	values := make(map[string]any, len(n.templateExprs))
	var hasPending bool
	for _, expr := range n.templateExprs {
		if expr.Kind.IsIteration() {
			continue
		}
		if !expr.Resolved {
			val, err := evalExprAny(env, expr, ctx)
			if err != nil {
				if isCELDataPending(err) {
					hasPending = true
					if continueOnPending {
						continue
					}
					return nil, true, ErrDataPending
				}
				return nil, false, err
			}
			expr.Resolved = true
			expr.ResolvedValue = val
		}
		values[expr.Expression] = expr.ResolvedValue
	}
	return values, hasPending, nil
}

// resolveInTemplate applies values to template using proper template string substitution.
// Use for resources where "${schema.spec.name}-suffix" should become "myapp-suffix".
func (n *Node) resolveInTemplate(
	base *unstructured.Unstructured, values map[string]any,
) (*unstructured.Unstructured, error) {
	desired := base.DeepCopy()
	res := resolver.NewResolver(desired.Object, values)
	summary := res.Resolve(toFieldDescriptors(n.templateVars))
	if len(summary.Errors) > 0 {
		return nil, fmt.Errorf("resolve errors: %v", summary.Errors)
	}
	return desired, nil
}

// upsertToTemplate applies values by upserting at paths, creating parent fields if needed.
// Use for instance status where paths like status.foo may not exist yet.
func (n *Node) upsertToTemplate(base *unstructured.Unstructured, values map[string]any) *unstructured.Unstructured {
	desired := base.DeepCopy()
	res := resolver.NewResolver(desired.Object, values)
	for _, v := range n.templateVars {
		if len(v.Expressions) == 0 {
			continue
		}
		if val, ok := values[v.Expressions[0]]; ok {
			_ = res.UpsertValueAtPath(v.Path, val)
		}
	}
	return desired
}

// SetObserved stores the observed state(s) from the cluster.
func (n *Node) SetObserved(observed []*unstructured.Unstructured) {
	if n.Spec.Meta.Type == graph.NodeTypeCollection {
		n.observed = orderedCollectionObserved(observed, n.desired, n.Spec.Meta.Namespaced)
	} else {
		n.observed = observed
	}
}

// IsReady evaluates readyWhen expressions using observed state.
// Ignored nodes are treated as ready for dependency gating purposes.
func (n *Node) IsReady() (bool, error) {
	// Ignored nodes are satisfied for dependency gating - dependents shouldn't block.
	ignored, err := n.IsIgnored()
	if err != nil {
		return false, err
	}
	if ignored {
		return true, nil
	}

	if len(n.readyWhenExprs) == 0 {
		return true, nil
	}
	if n.Spec.Meta.Type == graph.NodeTypeCollection {
		return n.isCollectionReady()
	}
	return n.isSingleResourceReady()
}

func (n *Node) isSingleResourceReady() (bool, error) {
	if len(n.observed) == 0 {
		return false, nil
	}

	nodeID := n.Spec.Meta.ID
	env, err := krocel.DefaultEnvironment(krocel.WithResourceIDs([]string{nodeID, graph.InstanceNodeID}))
	if err != nil {
		return false, err
	}

	ctx := map[string]any{nodeID: n.observed[0].Object}
	// Add schema to context (without status).
	if schemaDep := n.deps[graph.InstanceNodeID]; schemaDep != nil {
		ctx[graph.InstanceNodeID] = withStatusOmitted(schemaDep.observed[0].Object)
	}

	for _, expr := range n.readyWhenExprs {
		result, err := evalBoolExpr(env, expr, ctx)
		if err != nil {
			if isCELDataPending(err) {
				return false, nil
			}
			return false, fmt.Errorf("readyWhen %q: %w", expr.Expression, err)
		}
		if !result {
			return false, nil
		}
	}
	return true, nil
}

func (n *Node) isCollectionReady() (bool, error) {
	if len(n.desired) == 0 {
		return true, nil
	}
	if len(n.observed) < len(n.desired) {
		return false, nil
	}

	// Collection readyWhen uses "each" (single item) plus schema.
	env, err := krocel.DefaultEnvironment(
		krocel.WithResourceIDs([]string{graph.InstanceNodeID, graph.EachVarName}),
	)
	if err != nil {
		return false, err
	}

	// Get schema value once (without status).
	var schemaObj map[string]any
	if schemaDep := n.deps[graph.InstanceNodeID]; schemaDep != nil {
		schemaObj = withStatusOmitted(schemaDep.observed[0].Object)
	}

	for i, obj := range n.observed {
		ctx := map[string]any{graph.EachVarName: obj.Object}
		if schemaObj != nil {
			ctx[graph.InstanceNodeID] = schemaObj
		}
		for _, expr := range n.readyWhenExprs {
			// readyWhen for collections must NOT be cached - each item has different "each" context.
			// Use evalRawCEL directly instead of evalBoolExpr.
			val, err := evalRawCEL(env, expr.Expression, ctx)
			if err != nil {
				if isCELDataPending(err) {
					return false, nil
				}
				return false, fmt.Errorf("readyWhen %q (item %d): %w", expr.Expression, i, err)
			}
			result, ok := val.(bool)
			if !ok {
				return false, fmt.Errorf("readyWhen %q did not return bool", expr.Expression)
			}
			if !result {
				return false, nil
			}
		}
	}

	return true, nil
}

// evaluateForEach evaluates forEach dimensions and returns iterator contexts.
func (n *Node) evaluateForEach() ([]map[string]any, error) {
	if len(n.Spec.ForEach) == 0 {
		return nil, nil
	}

	ctx := n.buildContext()
	singles, collections, _ := n.contextDependencyIDs(nil)
	env, err := buildEnv(singles, collections)
	if err != nil {
		return nil, fmt.Errorf("failed to build forEach env: %w", err)
	}

	dimensions := make([]evaluatedDimension, len(n.Spec.ForEach))
	for i, dim := range n.Spec.ForEach {
		values, err := evalListExpr(env, n.forEachExprs[i], ctx)
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

	return cartesianProduct(dimensions), nil
}

// buildContext builds the CEL activation context from node dependencies.
// If only is provided, only those dependency IDs are included in the context.
// If only is empty/nil, all dependencies are included.
func (n *Node) buildContext(only ...string) map[string]any {
	ctx := make(map[string]any)
	for depID, dep := range n.deps {
		if len(dep.observed) == 0 {
			continue
		}
		if len(only) > 0 && !slices.Contains(only, depID) {
			continue
		}
		if dep.Spec.Meta.Type == graph.NodeTypeCollection {
			items := make([]any, len(dep.observed))
			for i, obj := range dep.observed {
				items[i] = obj.Object
			}
			ctx[depID] = items
		} else {
			obj := dep.observed[0].Object
			// For schema (instance), strip status - users should only access spec/metadata.
			if depID == graph.InstanceNodeID {
				obj = withStatusOmitted(obj)
			}
			ctx[depID] = obj
		}
	}
	return ctx
}

// withStatusOmitted returns a shallow copy of obj with the "status" key removed.
// This prevents CEL expressions from accessing instance status fields.
func withStatusOmitted(obj map[string]any) map[string]any {
	result := make(map[string]any, len(obj))
	for k, v := range obj {
		if k != "status" {
			result[k] = v
		}
	}
	return result
}

// contextDependencyIDs returns CEL variable names grouped by type.
// - singles: dependencies that are single resources (declared as dyn)
// - collections: dependencies that are collections (declared as list(dyn))
// - iterators: forEach loop variable names from iterCtx (declared as list(dyn))
func (n *Node) contextDependencyIDs(iterCtx map[string]any) (singles, collections, iterators []string) {
	for depID, dep := range n.deps {
		if dep.Spec.Meta.Type == graph.NodeTypeCollection {
			collections = append(collections, depID)
		} else {
			singles = append(singles, depID)
		}
	}
	for name := range iterCtx {
		iterators = append(iterators, name)
	}
	return
}
