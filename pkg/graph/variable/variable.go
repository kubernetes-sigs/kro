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

package variable

import (
	krocel "github.com/kubernetes-sigs/kro/pkg/cel"
)

// FieldDescriptor represents a field in a resource template that contains CEL expressions.
// It is created by the parser and enriched by the builder:
//
//   - Parser: sets Path, StandaloneExpression, and creates Expression objects with only
//     Original populated (References and Program are nil)
//   - Builder: inspects expressions to populate References, validates types using
//     StandaloneExpression to derive expected type from schema, then compiles Program
//
// The field may contain multiple expressions for string templates like "prefix-${a}-${b}".
type FieldDescriptor struct {
	// Path is the JSONPath-like location of the field in the resource.
	// Example: spec.template.spec.containers[0].env[0].value
	Path string

	// Expressions contains the CEL expressions for this field.
	//
	// Lifecycle:
	//   - Parser creates with Expression.Original set (References, Program nil)
	//   - Builder populates Expression.References during dependency extraction
	//   - Builder populates Expression.Program during compilation
	//
	// For standalone expressions (e.g., "${foo}"), this contains one expression.
	// For string templates (e.g., "prefix-${a}-${b}"), this contains multiple
	// expressions that will be evaluated and interpolated into the template.
	Expressions []*krocel.Expression

	// StandaloneExpression indicates if this is a single CEL expression vs a string template.
	//
	// Used by builder to derive expected type:
	//   - true:  "${foo}" - expected type derived from OpenAPI schema at Path
	//   - false: "hello-${foo}" - expected type is always string (concatenation)
	StandaloneExpression bool
}

// ResourceField represents a variable in a resource template. Variables are fields
// that contain CEL expressions rather than constant values. For example:
//
//	spec:
//	  replicas: ${schema.spec.replicas + 5}
//
// The Kind field determines what the expression references:
//   - Static: only references schema or nothing (no resource dependencies)
//   - Dynamic: references other resources in the RGD (e.g., ${vpc.status.id})
//   - Iteration: references forEach iterators (e.g., ${region} in a collection)
type ResourceField struct {
	FieldDescriptor
	// Kind determines what this variable references (static, dynamic, iteration).
	// Set by builder based on expression references.
	Kind ResourceVariableKind
}

// ResourceVariableKind represents the kind of resource variable.
type ResourceVariableKind string

const (
	// ResourceVariableKindStatic represents a static variable. Static variables
	// are resolved at the beginning of the execution and their value is constant.
	// Static variables are easy to find, they always start with 'spec'. Referring
	// to the instance spec.
	//
	// For example:
	//   spec:
	//      replicas: ${schema.spec.replicas + 5}
	ResourceVariableKindStatic ResourceVariableKind = "static"
	// ResourceVariableKindDynamic represents a dynamic variable. Dynamic variables
	// are resolved at runtime and their value can change during the execution. Dynamic
	// cannot start with 'spec' and they must refer to another resource in the
	// ResourceGraphDefinition.
	//
	// For example:
	//    spec:
	//	    vpcID: ${vpc.status.vpcID}
	ResourceVariableKindDynamic ResourceVariableKind = "dynamic"
	// ResourceVariableKindReadyWhen represents readyWhen variables. ReadyWhen variables
	// are resolved at runtime. The difference between them, and the dynamic variables
	// is that dynamic variable resolutions wait for other resources to provide a value
	// while ReadyWhen variables are created and wait for certain conditions before
	// moving forward to the next resource to create
	//
	// For example:
	//   name: cluster
	//   readyWhen:
	//   - ${cluster.status.status == "Active"}
	ResourceVariableKindReadyWhen ResourceVariableKind = "readyWhen"
	// ResourceVariableKindIncludeWhen represents an includeWhen variable.
	// IncludeWhen variables are resolved at the beginning of the execution and
	// their value is constant. They decide whether we are going to create
	// a resource or not
	//
	// For example:
	//   name: deployment
	//   includeWhen:
	//   - ${schema.spec.replicas > 1}
	ResourceVariableKindIncludeWhen ResourceVariableKind = "includeWhen"
	// ResourceVariableKindIteration represents an iteration variable. Iteration
	// variables are expressions inside a collection resource (one with forEach)
	// that reference iterator variables. They are evaluated during collection
	// expansion, once per iteration, with the iterator bindings in scope.
	//
	// For example, with forEach: [{region: "${schema.spec.regions}"}]:
	//   metadata:
	//     name: ${schema.spec.name}-${region}  <- "region" is an iterator variable
	//
	// Expressions not referencing iterator variables remain static or dynamic.
	ResourceVariableKindIteration ResourceVariableKind = "iteration"
)

// String returns the string representation of a ResourceVariableKind.
func (r ResourceVariableKind) String() string {
	return string(r)
}

// IsStatic returns true if the ResourceVariableKind is static
func (r ResourceVariableKind) IsStatic() bool {
	return r == ResourceVariableKindStatic
}

// IsDynamic returns true if the ResourceVariableKind is dynamic
func (r ResourceVariableKind) IsDynamic() bool {
	return r == ResourceVariableKindDynamic
}

// IsIncludeWhen returns true if the ResourceVariableKind is includeWhen
func (r ResourceVariableKind) IsIncludeWhen() bool {
	return r == ResourceVariableKindIncludeWhen
}

// IsIteration returns true if the ResourceVariableKind is iteration
func (r ResourceVariableKind) IsIteration() bool {
	return r == ResourceVariableKindIteration
}
