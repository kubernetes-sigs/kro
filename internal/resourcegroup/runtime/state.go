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

import "github.com/aws-controllers-k8s/symphony/internal/typesystem/variable"

// ResourceState represents the current state of a resource in the runtime.
// It indicates the resource's readiness for processing or what it's waiting on.
//
// The state here isn't directly tied to the dependencies in the graph, but
// rather the readiness of the resource itself. e.g Are the CEL expressions
// evaluated and replaced properly? are the readiness conditions met?
type ResourceState string

const (
	// ResourceStateResolved indicates that the resource is ready to be processed.
	// All its dependencies are resolved, and it can be safely created or updated.
	ResourceStateResolved ResourceState = "Resolved"

	// ResourceStateWaitingOnDependencies indicates that the resource is waiting
	// for its dependencies to be resolved. This includes waiting for other
	// resources it depends on to be created or updated, or for variables from
	// those dependencies to be available.
	ResourceStateWaitingOnDependencies ResourceState = "WaitingOnDependencies"

	// ResourceStateWaitingOnReadiness indicates that the resource is waiting
	// for its readiness conditions to be met. This typically occurs after the
	// resource has been created or updated, but is not yet in a stable state
	// according to its defined readiness criteria. e.g waiting for a Pod to
	// be running and ready, a PVC to be bound ...
	ResourceStateWaitingOnReadiness ResourceState = "WaitingOnReadiness"

	// ResourceStateIgnoredByConditions indicates that the resource is ignored
	// by a condition that evaluated to false. This typically occurs before
	// a resource is created or updated, and is decided by a variable defined
	// in the instance spec. Eg. Deciding whether to create a Deployment or
	// just a simple pod based on the defined replica.
	ResourceStateIgnoredByConditions ResourceState = "IgnoredByConditions"
)

// expressionEvaluationState represents the state of an expression evaluation
// for a resource variable. It tracks the progress and result of evaluating
// a single expression associated with a resource.
//
// This is slightly different from the `ResourceVariable` in that it's more
// focused on the evaluation state and result of a single expression, rather
// than the variable as a whole; since a `ResourceVariable` can contain multiple
// expressions.
type expressionEvaluationState struct {
	// Expression is the original CEL expression to be evaluated. This
	// expression may reference other resources or their properties.
	Expression string

	// Dependencies is a list of resourceIDs that this expression depends on.
	// All these dependencies must be resolved before the expression can be
	// evaluated. This ensures correct ordering of evaluations in the graph.
	Dependencies []string

	// Kind indicates the type of the resource variable, such as static or
	// dynamic. This affects when and how the expression is evaluated.
	Kind variable.ResourceVariableKind

	// Resolved indicates whether the expression has been successfully
	// evaluated. Its set to true once the expression is evaluated without
	// errors and a value is obtained.
	Resolved bool

	// ResolvedValue holds the result of the expression evaluation. Its nil
	// if the expression hasn't been resolved yet. The type of this value
	// depends on the expression and could be any valid Go type.
	ResolvedValue interface{}
}
