// Copyright Amazon.com Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//	http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package runtime

import (
	"fmt"
	"slices"
	"strings"

	"github.com/google/cel-go/cel"
	"golang.org/x/exp/maps"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	krocel "github.com/kro-run/kro/pkg/cel"
	"github.com/kro-run/kro/pkg/graph/variable"
	"github.com/kro-run/kro/pkg/runtime/resolver"
)

// Compile time proof to ensure that ResourceGraphDefinitionRuntime implements the
// Runtime interface.
var _ Interface = &ResourceGraphDefinitionRuntime{}

// NewResourceGraphDefinitionRuntime creates and initializes a new ResourceGraphDefinitionRuntime
// instance.
//
// It is also responsible of properly creating the ExpressionEvaluationState
// for each variable in the resources and the instance, and caching them
// for future use. This function will also call Synchronize to evaluate the
// static variables. This helps hide the complexity of the runtime from the
// caller (instance controller in this case).
//
// The output of this function is NOT thread safe.
func NewResourceGraphDefinitionRuntime(
	instance Resource,
	resources map[string]Resource,
	topologicalOrder []string,
) (*ResourceGraphDefinitionRuntime, error) {
	r := &ResourceGraphDefinitionRuntime{
		instance:                     instance,
		resources:                    resources,
		topologicalOrder:             topologicalOrder,
		resolvedResources:            make(map[string]*unstructured.Unstructured),
		runtimeVariables:             make(map[string][]*expressionEvaluationState),
		expressionsCache:             make(map[string]*expressionEvaluationState),
		ignoredByConditionsResources: make(map[string]bool),
	}
	// make sure to copy the variables and the dependencies, to avoid
	// modifying the original resource.
	for id, resource := range resources {
		// Process the resource variables.
		for _, variable := range resource.GetVariables() {
			for _, expr := range variable.Expressions {
				// If cached use the same pointer.
				if ec, seen := r.expressionsCache[expr]; seen {
					// NOTE(a-hilaly): This strikes me as an early optimization, but
					// it's a good one, i believe... We can always remove it if it's
					// too magical.
					r.runtimeVariables[id] = append(r.runtimeVariables[id], ec)
					continue
				}
				ees := &expressionEvaluationState{
					Expression:   expr,
					Dependencies: variable.Dependencies,
					Kind:         variable.Kind,
				}
				r.runtimeVariables[id] = append(r.runtimeVariables[id], ees)
				r.expressionsCache[expr] = ees
			}
		}
		// Process the readyWhenExpressions.
		for _, expr := range resource.GetReadyWhenExpressions() {
			ees := &expressionEvaluationState{
				Expression: expr,
				Kind:       variable.ResourceVariableKindReadyWhen,
			}
			r.expressionsCache[expr] = ees
		}
	}

	// Now we need to collect the instance variables.
	for _, variable := range instance.GetVariables() {
		for _, expr := range variable.Expressions {
			if ec, seen := r.expressionsCache[expr]; seen {
				// It is validated at the Graph level that the resource ids
				// can't be `instance`. This is why.
				r.runtimeVariables["instance"] = append(r.runtimeVariables["instance"], ec)
				continue
			}
			ees := &expressionEvaluationState{
				Expression:   expr,
				Dependencies: variable.Dependencies,
				Kind:         variable.Kind,
			}
			r.runtimeVariables["instance"] = append(r.runtimeVariables["instance"], ees)
			r.expressionsCache[expr] = ees
		}
	}

	// Evaluate the static variables, so that the caller only needs to call Synchronize
	// whenever a new resource is added or a variable is updated.
	err := r.evaluateStaticVariables()
	if err != nil {
		return nil, fmt.Errorf("failed to evaluate static variables: %w", err)
	}
	err = r.propagateResourceVariables()
	if err != nil {
		return nil, fmt.Errorf("failed to propagate resource variables: %w", err)
	}

	return r, nil
}

// ResourceGraphDefinitionRuntime implements the Interface for managing and synchronizing
// resources. Is is the responsibility of the consumer to call Synchronize
// appropriately, and decide whether to follow the TopologicalOrder or a
// BFS/DFS traversal of the resources.
type ResourceGraphDefinitionRuntime struct {
	// instance represents the main resource instance being managed.
	// This is typically the top-level custom resource that owns or manages
	// other resources in the graph.
	instance Resource

	// resources is a map of all resources in the graph, keyed by their
	// unique identifier. These resources represent the nodes in the
	// dependency graph.
	resources map[string]Resource

	// resolvedResources stores the latest state of resolved resources.
	// When a resource is successfully created or updated in the cluster,
	// its state is stored here. This map helps track which resources have
	// been successfully reconciled with the cluster state.
	resolvedResources map[string]*unstructured.Unstructured

	// runtimeVariables maps resource ids to their associated variables.
	// These variables are used in the synchronization process to resolve
	// dependencies and compute derived values for resources.
	runtimeVariables map[string][]*expressionEvaluationState

	// expressionsCache caches evaluated expressions to avoid redundant
	// computations. This optimization helps improve performance by reusing
	// previously calculated results for expressions that haven't changed.
	//
	// NOTE(a-hilaly): It is important to note that the expressionsCache have
	// the same pointers used in the runtimeVariables. Meaning that if a variable
	// is updated here, it will be updated in the runtimeVariables as well, and
	// vice versa.
	expressionsCache map[string]*expressionEvaluationState

	// topologicalOrder holds the dependency order of resources. This order
	// ensures that resources are processed in a way that respects their
	// dependencies, preventing circular dependencies and ensuring efficient
	// synchronization.
	topologicalOrder []string

	// ignoredByConditionsResources holds the resources whos defined conditions returned false
	// or who's dependencies are ignored
	ignoredByConditionsResources map[string]bool
}

// TopologicalOrder returns the topological order of resources.
func (rt *ResourceGraphDefinitionRuntime) TopologicalOrder() []string {
	return rt.topologicalOrder
}

// ResourceDescriptor returns the descriptor for a given resource id.
//
// It is the responsibility of the caller to ensure that the resource id
// exists in the runtime. a.k.a the caller should use the TopologicalOrder
// to get the resource ids.
func (rt *ResourceGraphDefinitionRuntime) ResourceDescriptor(id string) ResourceDescriptor {
	return rt.resources[id]
}

// GetResource returns a resource so that it's either created or updated in
// the cluster, it also returns the runtime state of the resource. Indicating
// whether the resource variables are resolved or not, and whether the resource
// readiness conditions are met or not.
func (rt *ResourceGraphDefinitionRuntime) GetResource(id string) (*unstructured.Unstructured, ResourceState) {
	// Did the user set the resource?
	r, ok := rt.resolvedResources[id]
	if ok {
		return r, ResourceStateResolved
	}

	// If not, can we process the resource?
	resolved := rt.canProcessResource(id)
	if resolved {
		return rt.resources[id].Unstructured(), ResourceStateResolved
	}

	return nil, ResourceStateWaitingOnDependencies
}

// SetResource updates or sets a resource in the runtime. This is typically
// called after a resource has been created or updated in the cluster.
func (rt *ResourceGraphDefinitionRuntime) SetResource(id string, resource *unstructured.Unstructured) {
	rt.resolvedResources[id] = resource
}

// GetInstance returns the main instance object managed by this runtime.
func (rt *ResourceGraphDefinitionRuntime) GetInstance() *unstructured.Unstructured {
	return rt.instance.Unstructured()
}

// SetInstance updates the main instance object.
// This is typically called after the instance has been updated in the cluster.
func (rt *ResourceGraphDefinitionRuntime) SetInstance(obj *unstructured.Unstructured) {
	ptr := rt.instance.Unstructured()
	ptr.Object = obj.Object
}

// Synchronize tries to resolve as many resources as possible. It returns true
// if the user should call Synchronize again, and false if something is still
// not resolved.
//
// Every time Synchronize is called, it walks through the resources and tries
// to resolve as many as possible. If a resource is resolved, it's added to the
// resolved resources map.
func (rt *ResourceGraphDefinitionRuntime) Synchronize() (bool, error) {
	// if everything is resolved, we're done.
	// TODO(a-hilaly): Add readiness check here.
	if rt.allExpressionsAreResolved() && len(rt.resolvedResources) == len(rt.resources) {
		return false, nil
	}

	// first synchronize the resources.
	err := rt.evaluateDynamicVariables()
	if err != nil {
		return true, fmt.Errorf("failed to evaluate dynamic variables: %w", err)
	}

	// Now propagate the resource variables.
	err = rt.propagateResourceVariables()
	if err != nil {
		return true, fmt.Errorf("failed to propagate resource variables: %w", err)
	}

	// then synchronize the instance
	err = rt.evaluateInstanceStatuses()
	if err != nil {
		return true, fmt.Errorf("failed to evaluate instance statuses: %w", err)
	}

	return true, nil
}

// propagateResourceVariables iterates over all resources and evaluates their
// variables if all dependencies are resolved.
func (rt *ResourceGraphDefinitionRuntime) propagateResourceVariables() error {
	for id := range rt.resources {
		if rt.canProcessResource(id) {
			// evaluate the resource variables
			err := rt.evaluateResourceExpressions(id)
			if err != nil {
				return fmt.Errorf("failed to evaluate resource variables for %s: %w", id, err)
			}
		}
	}
	return nil
}

// canProcessResource checks if a resource can be resolved by examining
// if all its dependencies are resolved AND if all its variables are resolved.
func (rt *ResourceGraphDefinitionRuntime) canProcessResource(resource string) bool {
	// Check if all dependencies are resolved. a.k.a all variables have been
	// evaluated.
	for _, dep := range rt.resources[resource].GetDependencies() {
		if !rt.resourceVariablesResolved(dep) {
			return false
		}
	}

	// Check if the resource variables are resolved.
	kk := rt.resourceVariablesResolved(resource)
	return kk
}

// resourceVariablesResolved determines if all variables for a given resource
// have been resolved.
func (rt *ResourceGraphDefinitionRuntime) resourceVariablesResolved(resource string) bool {
	for _, variable := range rt.runtimeVariables[resource] {
		if variable.Kind.IsDynamic() && !variable.Resolved {
			return false
		}
	}
	return true
}

// evaluateStaticVariables processes all static variables in the runtime.
// Static variables are those that can be evaluated immediately, typically
// depending only on the initial configuration. This function is usually
// called once during runtime initialization to set up the baseline state
func (rt *ResourceGraphDefinitionRuntime) evaluateStaticVariables() error {
	env, err := krocel.DefaultEnvironment(krocel.WithResourceIDs([]string{"schema"}))
	if err != nil {
		return err
	}

	evalContext := map[string]interface{}{
		"schema": rt.instance.Unstructured().Object,
	}
	for _, variable := range rt.expressionsCache {
		if variable.Kind.IsStatic() {
			value, err := evaluateExpression(env, evalContext, variable.Expression)
			if err != nil {
				return err
			}

			variable.Resolved = true
			variable.ResolvedValue = value
		}
	}
	return nil
}

type EvalError struct {
	IsIncompleteData bool
	Err              error
}

func (e *EvalError) Error() string {
	if e.IsIncompleteData {
		return fmt.Sprintf("incomplete data: %s", e.Err.Error())
	}
	return e.Err.Error()
}

// evaluateDynamicVariables processes all dynamic variables in the runtime.
// Dynamic variables depend on the state of other resources and are evaluated
// iteratively as resources are resolved. This function is called during each
// synchronization cycle to update the runtime state based on newly resolved
// resources.
func (rt *ResourceGraphDefinitionRuntime) evaluateDynamicVariables() error {
	// Dynamic variables are those that depend on other resources
	// and are resolved after all the dependencies are resolved.

	resolvedResources := maps.Keys(rt.resolvedResources)
	resolvedResources = append(resolvedResources, "schema")
	env, err := krocel.DefaultEnvironment(krocel.WithResourceIDs(resolvedResources))
	if err != nil {
		return err
	}

	// let's iterate over any resolved resource and try to resolve
	// the dynamic variables that depend on it.
	// Since we have already cached the expressions, we don't need to
	// loop over all the resources.
	for _, variable := range rt.expressionsCache {
		if variable.Kind.IsDynamic() {
			// Skip the variable if it's already resolved
			if variable.Resolved {
				continue
			}

			// we need to make sure that the dependencies are
			// part of the resolved resources.
			if len(variable.Dependencies) > 0 &&
				!containsAllElements(resolvedResources, variable.Dependencies) {
				continue
			}

			evalContext := make(map[string]interface{})
			for _, dep := range variable.Dependencies {
				evalContext[dep] = rt.resolvedResources[dep].Object
			}

			evalContext["schema"] = rt.instance.Unstructured().Object

			value, err := evaluateExpression(env, evalContext, variable.Expression)
			if err != nil {
				if strings.Contains(err.Error(), "no such key") {
					// TODO(a-hilaly): I'm not sure if this is the best way to handle
					// these. Probably need to reiterate here.
					return &EvalError{
						IsIncompleteData: true,
						Err:              err,
					}
				}
				return &EvalError{
					Err: err,
				}
			}

			variable.Resolved = true
			variable.ResolvedValue = value
		}
	}

	return nil
}

// evaluateInstanceStatuses updates the status of the main instance based on
// the current state of all resources. This function aggregates information
// from all managed resources to provide an overall status of the runtime,
// which is typically reflected in the custom resource's status field.
func (rt *ResourceGraphDefinitionRuntime) evaluateInstanceStatuses() error {
	rs := resolver.NewResolver(rt.instance.Unstructured().Object, map[string]interface{}{})

	// Two pieces of information are needed here:
	//  1. Instance variables are guaranteed to be standalone expressions.
	//  2. Not all instance variables are guaranteed to be resolved. This is
	//     more like a "best effort" to resolve as many as possible.
	for _, variable := range rt.instance.GetVariables() {
		cached, ok := rt.expressionsCache[variable.Expressions[0]]
		if ok && cached.Resolved {
			err := rs.UpsertValueAtPath(variable.Path, rt.expressionsCache[variable.Expressions[0]].ResolvedValue)
			if err != nil {
				return fmt.Errorf("failed to set value at path %s: %w", variable.Path, err)
			}
		}
	}
	return nil
}

// evaluateResourceExpressions processes all expressions associated with a
// specific resource.
func (rt *ResourceGraphDefinitionRuntime) evaluateResourceExpressions(resource string) error {
	exprValues := make(map[string]interface{})
	for _, v := range rt.expressionsCache {
		if v.Resolved {
			exprValues[v.Expression] = v.ResolvedValue
		}
	}

	variables := rt.resources[resource].GetVariables()
	exprFields := make([]variable.FieldDescriptor, len(variables))
	for i, v := range variables {
		exprFields[i] = v.FieldDescriptor
	}

	rs := resolver.NewResolver(rt.resources[resource].Unstructured().Object, exprValues)

	summary := rs.Resolve(exprFields)
	if summary.Errors != nil {
		return fmt.Errorf("failed to resolve resource %s: %v", resource, summary.Errors)
	}
	return nil
}

// allExpressionsAreResolved checks if every expression in the runtimes cache
// has been successfully evaluated
func (rt *ResourceGraphDefinitionRuntime) allExpressionsAreResolved() bool {
	for _, v := range rt.expressionsCache {
		if !v.Resolved {
			return false
		}
	}
	return true
}

// IsResourceReady checks if a resource is ready based on the readyWhenExpressions
// defined in the resource. If no readyWhenExpressions are defined, the resource
// is considered ready.
func (rt *ResourceGraphDefinitionRuntime) IsResourceReady(resourceID string) (bool, string, error) {
	observed, ok := rt.resolvedResources[resourceID]
	if !ok {
		// Users need to make sure that the resource is resolved a.k.a (SetResource)
		// before calling this function.
		return false, fmt.Sprintf("resource %s is not resolved", resourceID), nil
	}

	expressions := rt.resources[resourceID].GetReadyWhenExpressions()
	if len(expressions) == 0 {
		return true, "", nil
	}

	// we should not expect errors here since we already compiled it
	// in the dryRun
	env, err := krocel.DefaultEnvironment(krocel.WithResourceIDs([]string{resourceID}))
	if err != nil {
		return false, "", fmt.Errorf("failed creating new Environment: %w", err)
	}
	context := map[string]interface{}{
		resourceID: observed.Object,
	}

	for _, expression := range expressions {
		out, err := evaluateExpression(env, context, expression)
		if err != nil {
			return false, "", fmt.Errorf("failed evaluating expressison %s: %w", expression, err)
		}
		// returning a reason here to point out which expression is not ready yet
		if !out.(bool) {
			return false, fmt.Sprintf("expression %s evaluated to false", expression), nil
		}
	}
	return true, "", nil
}

// IgnoreResource ignores resource that has a conditions expressison that evaluated
// to false or whose dependencies are ignored
func (rt *ResourceGraphDefinitionRuntime) IgnoreResource(resourceID string) {
	rt.ignoredByConditionsResources[resourceID] = true
}

// areDependenciesIgnored will returns true if the dependencies of the resource
// are ignored, false if they are not.
//
// Naturally, if a resource is judged to be ignored, it will be marked as ignored
// and all its dependencies will be ignored as well. Causing a chain reaction
// of ignored resources.
func (rt *ResourceGraphDefinitionRuntime) areDependenciesIgnored(resourceID string) bool {
	for _, p := range rt.resources[resourceID].GetDependencies() {
		if _, isIgnored := rt.ignoredByConditionsResources[p]; isIgnored {
			return true
		}
	}
	return false
}

// WantToCreateResource returns true if all the condition expressions return true
// if not it will add itself to the ignored resources
func (rt *ResourceGraphDefinitionRuntime) WantToCreateResource(resourceID string) (bool, error) {
	if rt.areDependenciesIgnored(resourceID) {
		return false, nil
	}

	conditions := rt.resources[resourceID].GetIncludeWhenExpressions()
	if len(conditions) == 0 {
		return true, nil
	}

	// we should not expect errors here since we already compiled it
	// in the dryRun
	env, err := krocel.DefaultEnvironment(krocel.WithResourceIDs([]string{"schema"}))
	if err != nil {
		return false, nil
	}

	context := map[string]interface{}{
		"schema": rt.instance.Unstructured().Object,
	}

	for _, condition := range conditions {
		// We should not expect an error here as well since we checked during dry-run
		value, err := evaluateExpression(env, context, condition)
		if err != nil {
			return false, err
		}
		// returning a reason here to point out which expression is not ready yet
		if !value.(bool) {
			return false, fmt.Errorf("Skipping resource creation due to condition %s", condition)
		}
	}
	return true, nil
}

// evaluateExpression evaluates an CEL expression and returns a value if successful, or error
func evaluateExpression(env *cel.Env, context map[string]interface{}, expression string) (interface{}, error) {
	ast, issues := env.Compile(expression)
	if issues != nil && issues.Err() != nil {
		return nil, fmt.Errorf("failed compiling expression %s: %w", expression, issues.Err())
	}
	// Here as well
	program, err := env.Program(ast)
	if err != nil {
		return nil, fmt.Errorf("failed programming expression %s: %w", expression, err)
	}
	// We get an error here when the value field we're looking for is not yet defined
	// For now leaving it as error, in the future when we see different scenarios
	// of this error we can make some a reason, and others an error
	val, _, err := program.Eval(context)
	if err != nil {
		return nil, fmt.Errorf("failed evaluating expression %s: %w", expression, err)
	}

	return krocel.GoNativeType(val)
}

// containsAllElements checks if all elements in the inner slice are present
// in the outer slice.
func containsAllElements[T comparable](outer, inner []T) bool {
	for _, v := range inner {
		if !slices.Contains(outer, v) {
			return false
		}
	}
	return true
}
