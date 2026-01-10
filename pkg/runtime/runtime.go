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
	"slices"
	"sync"

	"github.com/google/cel-go/cel"
	"golang.org/x/exp/maps"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	krocel "github.com/kubernetes-sigs/kro/pkg/cel"
	"github.com/kubernetes-sigs/kro/pkg/graph/variable"
	"github.com/kubernetes-sigs/kro/pkg/runtime/resolver"
)

const (
	// DefaultMaxCollectionSize is the maximum number of resources that can be
	// generated from a single forEach collection. This limit prevents accidental
	// resource exhaustion from misconfigured or malicious forEach expressions.
	// The limit can be adjusted if needed for specific use cases.
	DefaultMaxCollectionSize = 1000
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
		resolvedCollections:          make(map[string][]*unstructured.Unstructured),
		runtimeVariables:             make(map[string][]*expressionEvaluationState),
		expressionsCache:             make(map[string]*expressionEvaluationState),
		celProgramCache:              make(map[string]cel.Program),
		ignoredByConditionsResources: make(map[string]bool),
	}
	// make sure to copy the variables and the dependencies, to avoid
	// modifying the original resource.
	for id, resource := range resources {
		if yes, _ := r.ReadyToProcessResource(id); !yes {
			continue
		}

		// Process the resource variables (including collection resources).
		// Variable Kind determines when ALL expressions in the variable are evaluated:
		// - Static: All expressions evaluated at init time
		// - Dynamic: All expressions evaluated when dependencies ready
		// - Iteration: All expressions evaluated during ExpandCollection only
		// Iteration variables are completely skipped during static/dynamic evaluation.
		for _, variable := range resource.GetVariables() {
			for _, expr := range variable.Expressions {
				// If cached, reuse the cached expression state (they're shared pointers).
				if ec, seen := r.expressionsCache[expr]; seen {
					// NOTE(a-hilaly): This strikes me as an early optimization, but
					// it's a good one, I believe... We can always remove it if it's
					// too magical.
					//
					// IMPORTANT: An expression like "schema.spec.name" might appear in
					// multiple variables with different Kinds. The SAME expression
					// should be evaluated with the LOWEST Kind priority so it's available
					// when needed. Priority: Static < Dynamic < Iteration.
					// If the new variable has a lower Kind, update the cached expression.
					if kindPriority(variable.Kind) < kindPriority(ec.Kind) {
						ec.Kind = variable.Kind
					}
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
	if err := r.evaluateStaticVariables(); err != nil {
		return nil, fmt.Errorf("failed to evaluate static variables: %w", err)
	}
	if err := r.propagateResourceVariables(); err != nil {
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

	// resolvedResources stores the latest state of resolved single resources.
	// When a resource is successfully created or updated in the cluster,
	// its state is stored here. This map helps track which resources have
	// been successfully reconciled with the cluster state.
	// Note: Collection resources use resolvedCollections instead.
	resolvedResources map[string]*unstructured.Unstructured

	// resolvedCollections stores the latest state of resolved collection resources.
	// Each collection resource (those with forEach) expands into N individual resources
	// at runtime. The slice maintains the order from ExpandCollection.
	// Key: resource ID, Value: ordered slice of expanded resources
	resolvedCollections map[string][]*unstructured.Unstructured

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

	// celProgramCache caches compiled CEL programs by expression string to
	// avoid recompiling the same expression multiple times. This improves
	// performance during expression evaluation by reusing previously compiled
	// programs.
	celProgramCache map[string]cel.Program
	// celProgramCacheMu is a mutex that protects access to the celProgramCache
	// to ensure thread safety during concurrent reads and writes.
	celProgramCacheMu sync.RWMutex

	// topologicalOrder holds the dependency order of resources. This order
	// ensures that resources are processed in a way that respects their
	// dependencies, preventing circular dependencies and ensuring efficient
	// synchronization.
	topologicalOrder []string

	// ignoredByConditionsResources holds the resources who's defined conditions returned false
	// or who's dependencies are ignored
	ignoredByConditionsResources map[string]bool
}

// TopologicalOrder returns the topological order of resources.
func (rt *ResourceGraphDefinitionRuntime) TopologicalOrder() []string {
	return rt.topologicalOrder
}

// isResourceResolved checks if a resource has been resolved (applied to K8s).
// For single resources, it checks resolvedResources.
// For collections, it checks resolvedCollections.
func (rt *ResourceGraphDefinitionRuntime) isResourceResolved(resourceID string) bool {
	// Check if we have a resource descriptor to determine if it's a collection
	resource, hasDescriptor := rt.resources[resourceID]
	if hasDescriptor && resource.IsCollection() {
		_, exists := rt.resolvedCollections[resourceID]
		return exists
	}
	// For non-collection resources (or when descriptor is unavailable),
	// check resolvedResources
	_, exists := rt.resolvedResources[resourceID]
	return exists
}

// allResourcesResolved checks if all resources have been resolved.
// It checks both resolvedResources (for single resources) and
// resolvedCollections (for collection resources).
func (rt *ResourceGraphDefinitionRuntime) allResourcesResolved() bool {
	for id, resource := range rt.resources {
		if resource.IsCollection() {
			if _, exists := rt.resolvedCollections[id]; !exists {
				return false
			}
		} else {
			if _, exists := rt.resolvedResources[id]; !exists {
				return false
			}
		}
	}
	return true
}

// ResourceDescriptor returns the descriptor for a given resource id.
//
// It is the responsibility of the caller to ensure that the resource id
// exists in the runtime. a.k.a the caller should use the TopologicalOrder
// to get the resource ids.
func (rt *ResourceGraphDefinitionRuntime) ResourceDescriptor(id string) ResourceDescriptor {
	return rt.resources[id]
}

// GetResource returns a single (non-collection) resource for creation or update.
// For collection resources, use ExpandCollection to get expanded resources and
// GetCollectionResources to retrieve already-applied resources.
//
// Callers should check ResourceDescriptor.IsCollection() first and call the
// appropriate method.
func (rt *ResourceGraphDefinitionRuntime) GetResource(id string) (*unstructured.Unstructured, ResourceState) {
	// Did the user set the resource?
	r, ok := rt.resolvedResources[id]
	if ok {
		return r, ResourceStateResolved
	}

	// If not, can we process the resource?
	if rt.canProcessResource(id) {
		return rt.resources[id].Unstructured(), ResourceStateResolved
	}

	return nil, ResourceStateWaitingOnDependencies
}

// SetResource stores a single (non-collection) resource after it's applied to K8s.
// For collection resources, use SetCollectionResources instead.
//
// Callers should check ResourceDescriptor.IsCollection() first and call the
// appropriate method.
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
	if rt.allExpressionsAreResolved() && rt.allResourcesResolved() {
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
// For collection resources, this resolves static and dynamic variables but
// skips iteration kind variables (they're resolved during ExpandCollection).
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
// have been resolved. Iteration kind variables are skipped since they're
// resolved during ExpandCollection, not during normal variable propagation.
func (rt *ResourceGraphDefinitionRuntime) resourceVariablesResolved(resource string) bool {
	for _, variable := range rt.runtimeVariables[resource] {
		// Skip iteration kind - they're resolved during ExpandCollection
		if variable.Kind.IsIteration() {
			continue
		}
		if variable.Kind.IsDynamic() && !variable.Resolved {
			return false
		}
	}
	return true
}

// evaluateStaticVariables processes all static variables in the runtime.
// Static variables are those that can be evaluated immediately, typically
// depending only on the initial configuration. This function is usually
// called once during runtime initialization to set up the baseline state.
//
// This also evaluates forEach expressions for collection resources since
// forEach expressions are static (they only reference schema).
func (rt *ResourceGraphDefinitionRuntime) evaluateStaticVariables() error {
	env, err := krocel.DefaultEnvironment(krocel.WithResourceIDs([]string{"schema"}))
	if err != nil {
		return err
	}

	evalContext := map[string]interface{}{
		"schema": rt.instance.Unstructured().Object,
	}

	// Evaluate static variables (skip iteration kind - they need iterator values)
	for _, variable := range rt.expressionsCache {
		if variable.Kind.IsStatic() {
			value, err := rt.evaluateExpression(env, evalContext, variable.Expression)
			if err != nil {
				return err
			}

			variable.Resolved = true
			variable.ResolvedValue = value
		}
		// Skip iteration kind - they're evaluated during ExpandCollection
	}

	return nil
}

// evaluateDynamicVariables processes all dynamic variables in the runtime.
// Dynamic variables depend on the state of other resources and are evaluated
// iteratively as resources are resolved. This function is called during each
// synchronization cycle to update the runtime state based on newly resolved
// resources.
func (rt *ResourceGraphDefinitionRuntime) evaluateDynamicVariables() error {
	// Dynamic variables are those that depend on other resources
	// and are resolved after all the dependencies are resolved.

	// Include both regular resources and collection resources in the CEL environment.
	// Regular resources are declared as AnyType, but collections must be declared
	// as ListType to support CEL comprehension operations (map, filter, all, exists).
	resolvedResourceIDs := maps.Keys(rt.resolvedResources)
	resolvedCollectionIDs := maps.Keys(rt.resolvedCollections)
	regularIDs := append(resolvedResourceIDs, "schema")
	env, err := krocel.DefaultEnvironment(
		krocel.WithResourceIDs(regularIDs),
		krocel.WithListVariables(resolvedCollectionIDs),
	)
	if err != nil {
		return err
	}

	// Let's iterate over any resolved resource and try to resolve
	// the dynamic variables that depend on it.
	// Since we have already cached the expressions, we don't need to
	// loop over all the resources.
	for _, variable := range rt.expressionsCache {
		if variable.Kind.IsDynamic() {
			// Skip the variable if it's already resolved
			if variable.Resolved {
				continue
			}

			// Check if all dependencies are resolved AND ready.
			// A dependency must be:
			// - Resolved: exists in resolvedResources or resolvedCollections
			// - Ready: its readyWhen expressions evaluate to true (if any)
			// This prevents evaluating expressions that reference dependencies
			// that aren't ready yet (e.g., workerPods[0] when pods aren't Running).
			allDepsReady := true
			for _, dep := range variable.Dependencies {
				if !rt.isResourceResolved(dep) {
					allDepsReady = false
					break
				}
				// Also check if the dependency is ready (readyWhen satisfied)
				if ready, _, _ := rt.IsResourceReady(dep); !ready {
					allDepsReady = false
					break
				}
			}
			if !allDepsReady {
				continue
			}

			evalContext := make(map[string]interface{})
			for _, dep := range variable.Dependencies {
				// Check if this is a collection using the resource descriptor
				resource, hasDescriptor := rt.resources[dep]
				if hasDescriptor && resource.IsCollection() {
					evalContext[dep] = rt.getCollectionResourcesAsSlice(dep)
				} else {
					evalContext[dep] = rt.resolvedResources[dep].Object
				}
			}

			evalContext["schema"] = rt.instance.Unstructured().Object

			value, err := rt.evaluateExpression(env, evalContext, variable.Expression)
			if err != nil {
				if isCELDataPending(err) {
					// Data is not yet available (e.g., status not populated).
					// Return ErrDataPending so the controller can handle this gracefully with
					// a delayed requeue instead of exponential backoff.
					return fmt.Errorf("%w: %v", ErrDataPending, err)
				}
				return fmt.Errorf("CEL evaluation failed: %w", err)
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
// specific resource. For collection resources, iteration kind variables are
// skipped since they're resolved during ExpandCollection.
func (rt *ResourceGraphDefinitionRuntime) evaluateResourceExpressions(resource string) error {
	yes, _ := rt.ReadyToProcessResource(resource)
	if !yes {
		return nil
	}
	exprValues := make(map[string]interface{})
	for _, v := range rt.expressionsCache {
		if v.Resolved {
			exprValues[v.Expression] = v.ResolvedValue
		}
	}

	variables := rt.resources[resource].GetVariables()

	// Filter out iteration kind variables - they're resolved during ExpandCollection
	exprFields := make([]variable.FieldDescriptor, 0, len(variables))
	for _, v := range variables {
		if v.Kind.IsIteration() {
			continue
		}
		exprFields = append(exprFields, v.FieldDescriptor)
	}

	if len(exprFields) == 0 {
		return nil
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
// is considered ready. This function handles both regular resources and collections.
func (rt *ResourceGraphDefinitionRuntime) IsResourceReady(resourceID string) (bool, string, error) {
	resource, hasDescriptor := rt.resources[resourceID]

	// If there's no resource descriptor, fall back to checking if resolved.
	// This maintains backwards compatibility with tests and edge cases.
	if !hasDescriptor {
		if rt.isResourceResolved(resourceID) {
			return true, "", nil
		}
		return false, fmt.Sprintf("resource %s is not resolved", resourceID), nil
	}

	// Delegate to IsCollectionReady for collection resources
	if resource.IsCollection() {
		return rt.IsCollectionReady(resourceID)
	}

	observed, ok := rt.resolvedResources[resourceID]
	if !ok {
		// Users need to make sure that the resource is resolved a.k.a (SetResource)
		// before calling this function.
		return false, fmt.Sprintf("resource %s is not resolved", resourceID), nil
	}

	expressions := resource.GetReadyWhenExpressions()
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
		out, err := rt.evaluateExpression(env, context, expression)
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

// IgnoreResource ignores resource that has a condition expression that evaluated
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

// ReadyToProcessResource returns true if all the condition expressions return true
// if not it will add itself to the ignored resources
func (rt *ResourceGraphDefinitionRuntime) ReadyToProcessResource(resourceID string) (bool, error) {
	if rt.areDependenciesIgnored(resourceID) {
		return false, nil
	}

	includeWhenExpressions := rt.resources[resourceID].GetIncludeWhenExpressions()
	if len(includeWhenExpressions) == 0 {
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

	for _, includeWhenExpression := range includeWhenExpressions {
		// We should not expect an error here as well since we checked during dry-run
		value, err := rt.evaluateExpression(env, context, includeWhenExpression)
		if err != nil {
			return false, err
		}
		// returning a reason here to point out which expression is not ready yet
		if !value.(bool) {
			return false, fmt.Errorf("skipping resource creation due to condition %s", includeWhenExpression)
		}
	}
	return true, nil
}

func (rt *ResourceGraphDefinitionRuntime) AreDependenciesReady(resourceID string) bool {
	for _, dep := range rt.ResourceDescriptor(resourceID).GetDependencies() {
		// If the dependency is not resolved, we can't be sure that it's ready
		_, st := rt.GetResource(dep)
		if st != ResourceStateResolved {
			return false
		}
		// The dependency needs to be ready in the runtime
		ready, _, err := rt.IsResourceReady(dep)
		if err != nil || !ready {
			return false
		}
	}
	return true
}

// evaluateExpression evaluates a CEL expression and returns a value if successful, or error.
// It caches compiled programs by expression string to avoid redundant compilation.
func (rt *ResourceGraphDefinitionRuntime) evaluateExpression(
	env *cel.Env,
	context map[string]interface{},
	expression string,
) (interface{}, error) {
	var program cel.Program
	var cached bool

	if rt.celProgramCache != nil {
		rt.celProgramCacheMu.RLock()
		program, cached = rt.celProgramCache[expression]
		rt.celProgramCacheMu.RUnlock()
	}

	if !cached {
		ast, issues := env.Compile(expression)
		if issues != nil && issues.Err() != nil {
			return nil, fmt.Errorf("failed compiling expression %s: %w", expression, issues.Err())
		}

		prog, err := env.Program(ast)
		if err != nil {
			return nil, fmt.Errorf("failed programming expression %s: %w", expression, err)
		}

		program = prog

		if rt.celProgramCache != nil {
			rt.celProgramCacheMu.Lock()
			rt.celProgramCache[expression] = program
			rt.celProgramCacheMu.Unlock()
		}
	}

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

// kindPriority returns a numeric priority for ResourceVariableKind.
// Lower values mean higher priority (should be evaluated earlier).
// Static < Dynamic < Iteration
//
// NOTE(design): This priority system addresses a tension in our caching design.
// Expressions are cached globally for efficiency, but the same expression can
// appear in variables with different Kinds (e.g., "schema.spec.name" might be
// Static in one resource but Iteration in another due to variable-level classification).
//
// Current approach: When an expression is encountered again with a lower Kind,
// we demote the cached Kind so it's evaluated earlier. This works but mutates
// cached state based on non-deterministic map iteration order.
//
// Cleaner alternatives for future consideration:
//  1. Don't share expression states across resources - each resource evaluates
//     its own expressions. More memory but eliminates ordering issues.
//  2. Always evaluate pure "schema.*" expressions statically at the builder level,
//     regardless of what variable they're in. The builder could tag these.
//  3. Two-phase caching: first pass collects all expressions and their minimum
//     required Kinds, second pass caches with the correct Kind.
func kindPriority(kind variable.ResourceVariableKind) int {
	switch kind {
	case variable.ResourceVariableKindStatic:
		return 0
	case variable.ResourceVariableKindDynamic:
		return 1
	case variable.ResourceVariableKindIteration:
		return 2
	default:
		return 3 // Unknown kinds get lowest priority
	}
}

// ExpandCollection evaluates forEach expressions and generates the cartesian
// product of iterator values, then evaluates only the iteration kind
// expressions for each combination.
func (rt *ResourceGraphDefinitionRuntime) ExpandCollection(resourceID string) ([]*unstructured.Unstructured, error) {
	resource, ok := rt.resources[resourceID]
	if !ok {
		return nil, fmt.Errorf("resource %s not found", resourceID)
	}

	// Check if it's a collection resource
	if !resource.IsCollection() {
		return nil, fmt.Errorf("resource %s is not a collection", resourceID)
	}

	// Try to cast to CollectionResource to get iterator info
	collectionResource, ok := resource.(CollectionResource)
	if !ok {
		return nil, fmt.Errorf("resource %s does not implement CollectionResource", resourceID)
	}

	iterators := collectionResource.GetForEachDimensionInfo()
	if len(iterators) == 0 {
		return nil, fmt.Errorf("collection resource %s has no iterators", resourceID)
	}

	// Build CEL environment with proper typing:
	// - Regular resources declared as AnyType
	// - Collections declared as ListType to support comprehension operations (map, filter, etc.)
	regularIDs := []string{"schema"}
	for id := range rt.resolvedResources {
		regularIDs = append(regularIDs, id)
	}
	collectionIDs := maps.Keys(rt.resolvedCollections)

	env, err := krocel.DefaultEnvironment(
		krocel.WithResourceIDs(regularIDs),
		krocel.WithListVariables(collectionIDs),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create CEL environment: %w", err)
	}

	// Build eval context with schema, all resolved resources, and resolved collections
	// Collection resources are provided as lists so forEach can iterate over them
	evalContext := map[string]interface{}{
		"schema": rt.instance.Unstructured().Object,
	}
	for id, res := range rt.resolvedResources {
		evalContext[id] = res.Object
	}
	// Add resolved collections as lists
	for id := range rt.resolvedCollections {
		evalContext[id] = rt.getCollectionResourcesAsSlice(id)
	}

	// Evaluate forEach expressions and collect iterator values
	iteratorValues := make([][]interface{}, len(iterators))
	for i, iter := range iterators {
		value, err := rt.evaluateExpression(env, evalContext, iter.Expression)
		if err != nil {
			return nil, fmt.Errorf("failed to evaluate forEach expression %q for resource %s: %w", iter.Expression, resourceID, err)
		}

		// The value must be a slice/list
		list, ok := value.([]interface{})
		if !ok {
			return nil, fmt.Errorf("forEach expression %q must return a list, got %T", iter.Expression, value)
		}
		iteratorValues[i] = list
	}

	// Generate cartesian product of all iterator values
	combinations := cartesianProduct(iterators, iteratorValues)

	// Check collection size limit to prevent resource exhaustion
	if len(combinations) > DefaultMaxCollectionSize {
		return nil, fmt.Errorf("collection %s would generate %d resources, exceeding the maximum limit of %d; consider reducing the forEach input size or splitting into multiple collections",
			resourceID, len(combinations), DefaultMaxCollectionSize)
	}

	// For each combination, create a resolved resource
	// Note: We do NOT store items here. Items are stored only when
	// SetCollectionResources is called after K8s apply. This ensures
	// "item exists in resolvedResources" means "item has been applied to K8s".
	expandedResources := make([]*unstructured.Unstructured, 0, len(combinations))
	for _, combination := range combinations {
		// Deep copy the original template
		template := resource.Unstructured().DeepCopy()

		// Resolve the template with the iterator combination
		resolved, err := rt.resolveTemplateForCombination(resourceID, template, combination)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve template for collection %s: %w", resourceID, err)
		}

		expandedResources = append(expandedResources, resolved)
	}

	return expandedResources, nil
}

// cartesianProduct generates all combinations of iterator values.
// For example, with iterators [region, tier] and values [[us, eu], [web, api]],
// it returns [{region: us, tier: web}, {region: us, tier: api}, {region: eu, tier: web}, {region: eu, tier: api}]
func cartesianProduct(iterators []ForEachDimensionInfo, values [][]interface{}) []map[string]interface{} {
	if len(iterators) == 0 || len(values) == 0 {
		return nil
	}

	// Calculate total number of combinations
	total := 1
	for _, v := range values {
		if len(v) == 0 {
			return nil // Empty list means no combinations
		}
		total *= len(v)
	}

	result := make([]map[string]interface{}, 0, total)

	// Generate all combinations using indices
	indices := make([]int, len(iterators))
	for {
		// Create current combination
		combination := make(map[string]interface{})
		for i, iter := range iterators {
			combination[iter.Name] = values[i][indices[i]]
		}
		result = append(result, combination)

		// Increment indices (like a multi-digit counter)
		carry := true
		for i := len(indices) - 1; i >= 0 && carry; i-- {
			indices[i]++
			if indices[i] < len(values[i]) {
				carry = false
			} else {
				indices[i] = 0
			}
		}
		if carry {
			break // All combinations generated
		}
	}

	return result
}

// resolveTemplateForCombination resolves iteration kind variables in a template
// with iterator variable values from a single combination. Static and dynamic
// variables are already resolved in the template.
func (rt *ResourceGraphDefinitionRuntime) resolveTemplateForCombination(
	resourceID string,
	template *unstructured.Unstructured,
	combination map[string]interface{},
) (*unstructured.Unstructured, error) {
	// Build resource IDs list for the CEL environment (include collections)
	resourceIDs := []string{"schema"}
	for id := range rt.resolvedResources {
		resourceIDs = append(resourceIDs, id)
	}
	for id := range rt.resolvedCollections {
		resourceIDs = append(resourceIDs, id)
	}

	// Get iterator variable names from combination
	iteratorNames := make([]string, 0, len(combination))
	for name := range combination {
		iteratorNames = append(iteratorNames, name)
	}

	// Create CEL environment with resource IDs and iterator variables
	env, err := krocel.DefaultEnvironment(
		krocel.WithResourceIDs(resourceIDs),
		krocel.WithIteratorVariables(iteratorNames),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create CEL environment: %w", err)
	}

	// Build eval context with schema, resolved resources, resolved collections, AND iterator values
	evalContext := map[string]interface{}{
		"schema": rt.instance.Unstructured().Object,
	}
	for id, res := range rt.resolvedResources {
		evalContext[id] = res.Object
	}
	// Add resolved collections as lists
	for id := range rt.resolvedCollections {
		evalContext[id] = rt.getCollectionResourcesAsSlice(id)
	}
	for name, value := range combination {
		evalContext[name] = value
	}

	// Only process iteration kind variables - static/dynamic are already resolved
	variables := rt.resources[resourceID].GetVariables()

	// First, evaluate all iteration expressions and build the data map
	exprValues := make(map[string]interface{})
	exprFields := make([]variable.FieldDescriptor, 0, len(variables))
	for _, v := range variables {
		// Skip non-iteration variables - they're already resolved in the template
		if !v.Kind.IsIteration() {
			continue
		}
		exprFields = append(exprFields, v.FieldDescriptor)

		for _, expr := range v.Expressions {
			if _, seen := exprValues[expr]; seen {
				continue
			}
			value, err := rt.evaluateExpression(env, evalContext, expr)
			if err != nil {
				return nil, fmt.Errorf("failed to evaluate expression %q: %w", expr, err)
			}
			exprValues[expr] = value
		}
	}

	if len(exprFields) == 0 {
		return template, nil
	}

	// Use resolver to apply the evaluated expressions to the template
	rs := resolver.NewResolver(template.Object, exprValues)
	summary := rs.Resolve(exprFields)
	if summary.Errors != nil {
		return nil, fmt.Errorf("failed to resolve template for %s: %v", resourceID, summary.Errors)
	}

	return template, nil
}

// GetCollectionResources returns all expanded resources for a collection in order.
// The returned slice is guaranteed to be in the same order as originally stored via
// SetCollectionResources (index 0, 1, 2, ...).
//
// Returns:
//   - (items, ResourceStateResolved) if collection has been applied (items may be empty slice)
//   - (nil, ResourceStateResolved) if ready to expand but not yet applied
//   - (nil, ResourceStateWaitingOnDependencies) if dependencies not ready
//
// IMPORTANT: nil and empty slice have different meanings:
//   - nil means "not applied yet" (even if state is Resolved, it means ready-to-expand)
//   - empty slice [] means "applied but collection expanded to zero items"
//
// Callers checking if a collection has been applied should use `items != nil`,
// NOT `len(items) > 0`, to correctly handle empty collections.
//
// TODO(a-hilaly): Consider adding a third state (e.g., ResourceStateReady) to
// distinguish "ready to process" from "applied to K8s" more explicitly, rather
// than overloading ResourceStateResolved and relying on nil checks.
func (rt *ResourceGraphDefinitionRuntime) GetCollectionResources(resourceID string) ([]*unstructured.Unstructured, ResourceState) {
	// Already expanded and applied?
	items, ok := rt.resolvedCollections[resourceID]
	if ok {
		return items, ResourceStateResolved
	}

	// Resource must exist to check if ready
	if _, exists := rt.resources[resourceID]; !exists {
		return nil, ResourceStateWaitingOnDependencies
	}

	// Ready to expand?
	if rt.canProcessResource(resourceID) {
		return nil, ResourceStateResolved
	}

	return nil, ResourceStateWaitingOnDependencies
}

// getCollectionResourcesAsSlice returns collection resources as []interface{} for CEL evaluation.
// This allows other resources to reference a collection as a list and use CEL list functions.
func (rt *ResourceGraphDefinitionRuntime) getCollectionResourcesAsSlice(resourceID string) []interface{} {
	items := rt.resolvedCollections[resourceID]
	result := make([]interface{}, 0, len(items))
	for _, res := range items {
		if res != nil {
			result = append(result, res.Object)
		}
	}
	return result
}

// SetCollectionResources stores all expanded resources for a collection at once.
// The input slice MUST be in the same order as returned by ExpandCollection.
// The slice index becomes the storage index (0, 1, 2, ...).
// This should be called after all items have been applied to K8s.
func (rt *ResourceGraphDefinitionRuntime) SetCollectionResources(resourceID string, resources []*unstructured.Unstructured) {
	rt.resolvedCollections[resourceID] = resources
}

// IsCollectionReady checks if all expanded resources in a collection are ready.
// Returns true only if:
// 1. The collection has been expanded (resolvedCollections has an entry)
// 2. All readyWhen expressions evaluate to true for EACH item (AND semantics)
//
// Note: The reconciler guarantees that len(resolvedCollections) == len(expandedResources)
// because SetCollectionResources is only called after ALL resources are successfully applied.
// If any apply fails, the reconciler returns an error before reaching this check.
//
// The readyWhen expressions use the `each` keyword to access the current item:
//   - ${each.status.phase == 'Running'}   // Per-item check
//   - ${each.status.ready == true}        // Per-item check
//
// Note: Collections cannot reference themselves in readyWhen expressions.
// Self-reference is validated at build time in builder.go.
func (rt *ResourceGraphDefinitionRuntime) IsCollectionReady(resourceID string) (bool, string, error) {
	items := rt.resolvedCollections[resourceID]
	if items == nil {
		return false, "collection not expanded", nil
	}

	// Check for nil items before evaluation
	for i, item := range items {
		if item == nil {
			return false, fmt.Sprintf("expanded resource %d not resolved", i), nil
		}
	}

	expressions := rt.resources[resourceID].GetReadyWhenExpressions()
	if len(expressions) == 0 {
		// No readyWhen expressions means all expanded resources are ready
		return true, "", nil
	}

	// Create CEL environment with "each" as iterator variable for per-item access.
	// Collections cannot reference themselves in readyWhen (validated at build time).
	env, err := krocel.DefaultEnvironment(
		krocel.WithIteratorVariables([]string{"each"}),
	)
	if err != nil {
		return false, "", fmt.Errorf("failed creating environment for %s: %w", resourceID, err)
	}

	// Per-item evaluation with `each` variable (AND semantics)
	for i, item := range items {
		context := map[string]interface{}{
			"each": item.Object,
		}

		for _, expression := range expressions {
			out, err := rt.evaluateExpression(env, context, expression)
			if err != nil {
				return false, "", fmt.Errorf("failed evaluating expression %s for %s[%d]: %w", expression, resourceID, i, err)
			}
			if !out.(bool) {
				return false, fmt.Sprintf("item %d: expression %s evaluated to false", i, expression), nil
			}
		}
	}

	return true, "", nil
}
