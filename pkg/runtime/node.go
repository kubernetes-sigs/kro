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

var identityPaths = []string{
	"metadata.name",
	"metadata.namespace",
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
		result, err = n.hardResolveCollection(n.templateVars, true)
	case graph.NodeTypeResource, graph.NodeTypeExternal:
		// External refs resolve like resources (for name/namespace CEL),
		// but the caller reads instead of applies.
		result, err = n.hardResolveSingleResource(n.templateVars)
	default:
		panic(fmt.Sprintf("unknown node type: %v", n.Spec.Meta.Type))
	}

	if err == nil {
		if n.Spec.Meta.Namespaced && n.Spec.Meta.Type != graph.NodeTypeInstance {
			inst := n.deps[graph.InstanceNodeID]
			normalizeNamespaces(result, inst.observed[0].GetNamespace())
		}
		n.desired = result
	}
	return result, err
}

// GetDesiredIdentity resolves only identity-related fields (metadata.name & namespace)
// and skips readiness gating. It is used for deletion/observation when we only need
// stable identities and want to avoid being blocked by unrelated template fields.
//
// NOTE: This method does not cache its result in n.desired; callers in non-deletion
// paths should continue using GetDesired().
func (n *Node) GetDesiredIdentity() ([]*unstructured.Unstructured, error) {
	vars := n.templateVarsForPaths(identityPaths)
	switch n.Spec.Meta.Type {
	case graph.NodeTypeCollection:
		result, err := n.hardResolveCollection(vars, false)
		if err != nil {
			return nil, err
		}
		if n.Spec.Meta.Namespaced {
			inst := n.deps[graph.InstanceNodeID]
			normalizeNamespaces(result, inst.observed[0].GetNamespace())
		}
		return result, nil
	case graph.NodeTypeResource, graph.NodeTypeExternal:
		result, err := n.hardResolveSingleResource(vars)
		if err != nil {
			return nil, err
		}
		if n.Spec.Meta.Namespaced {
			inst := n.deps[graph.InstanceNodeID]
			normalizeNamespaces(result, inst.observed[0].GetNamespace())
		}
		return result, nil
	case graph.NodeTypeInstance:
		panic("GetDesiredIdentity called for instance node")
	default:
		panic(fmt.Sprintf("unknown node type: %v", n.Spec.Meta.Type))
	}
}

func normalizeNamespaces(objs []*unstructured.Unstructured, namespace string) {
	// TODO: When cluster-scoped instances are supported, either default to
	// metav1.NamespaceDefault here or enforce namespace presence in graphbuilder.
	for _, obj := range objs {
		if obj.GetNamespace() != "" {
			continue
		}
		obj.SetNamespace(namespace)
	}
}

// DeleteTargets returns the ordered list of objects this node should delete now.
//
// This is intentionally narrow today: it only reasons about identity resolution
// and currently observed objects, and returns the safe deletion targets. It is the
// runtime's deletion gate so callers don't re-implement matching logic.
//
// Long-term, this should evolve into an ActionPlan where the runtime tells the
// caller which resources to create, update, keep intact, or delete, and where
// propagation/rollout gates are enforced in one place.
func (n *Node) DeleteTargets() ([]*unstructured.Unstructured, error) {
	switch n.Spec.Meta.Type {
	case graph.NodeTypeCollection, graph.NodeTypeResource:
		desired, err := n.GetDesiredIdentity()
		if err != nil {
			return nil, err
		}
		if n.Spec.Meta.Type == graph.NodeTypeCollection {
			return orderedIntersection(n.observed, desired), nil
		}
		return n.observed, nil
	case graph.NodeTypeInstance, graph.NodeTypeExternal:
		panic(fmt.Sprintf("DeleteTargets called for node type %v", n.Spec.Meta.Type))
	default:
		panic(fmt.Sprintf("unknown node type: %v", n.Spec.Meta.Type))
	}
}

func (n *Node) hardResolveSingleResource(vars []*variable.ResourceField) ([]*unstructured.Unstructured, error) {
	baseExprs, _ := n.exprSetsForVars(vars)
	values, _, err := n.evaluateExprsFiltered(baseExprs, false)
	if err != nil {
		if !IsDataPending(err) {
			err = fmt.Errorf("node %q: %w", n.Spec.Meta.ID, err)
		}
		return nil, err
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
		values := make(map[string]any, len(baseValues)+len(iterExprs))
		maps.Copy(values, baseValues)

		// Merge iterator values into context.
		ctx := make(map[string]any, len(baseCtx)+len(iterCtx))
		maps.Copy(ctx, baseCtx)
		maps.Copy(ctx, iterCtx)

		for expr := range iterExprs {
			val, err := evalRawCEL(iterEnv, expr, ctx)
			if err != nil {
				if isCELDataPending(err) {
					return nil, ErrDataPending
				}
				return nil, fmt.Errorf("collection iteration eval %q: %w", expr, err)
			}
			values[expr] = val
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

// evaluateExprsFiltered evaluates non-iteration expressions and returns the values map.
// If exprs is nil, all expressions are evaluated. If exprs is empty, returns empty values.
// If continueOnPending is true, it skips expressions that return ErrDataPending.
// Returns (values, hasPending, error).
func (n *Node) evaluateExprsFiltered(exprs map[string]struct{}, continueOnPending bool) (map[string]any, bool, error) {
	if exprs != nil && len(exprs) == 0 {
		return map[string]any{}, false, nil
	}

	singles, collections, _ := n.contextDependencyIDs(nil)
	env, err := buildEnv(singles, collections)
	if err != nil {
		return nil, false, err
	}
	ctx := n.buildContext()

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
			if _, ok := exprs[expr.Expression]; !ok {
				continue
			}
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
		exprKinds[expr.Expression] = expr.Kind
	}

	for _, v := range vars {
		for _, expr := range v.Expressions {
			if kind, ok := exprKinds[expr]; ok && kind.IsIteration() {
				iterExprs[expr] = struct{}{}
			} else {
				baseExprs[expr] = struct{}{}
			}
		}
	}
	return baseExprs, iterExprs
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
		n.observed = orderedIntersection(observed, n.desired)
		return
	}

	n.observed = observed
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
