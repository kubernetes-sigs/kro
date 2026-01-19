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
	"slices"

	"github.com/google/cel-go/cel"
)

// FieldDescriptor represents a field that contains CEL expressions in it. It
// contains the path of the field in the resource, the CEL expressions
// and the expected type of the field. The field may contain multiple
// expressions.
type FieldDescriptor struct {
	// Path is the path of the field in the resource (JSONPath-like)
	// example: spec.template.spec.containers[0].env[0].value
	// Since the object's we're dealing with are mainly made up of maps,
	// arrays and native types, we can use a string to represent the path.
	Path string
	// Expressions is a list of CEL expressions in the field.
	Expressions []string

	// ExpectedType is the expected CEL type of the field.
	// Set by: builder.setExpectedTypeOnDescriptor() - the single place where types are determined.
	// Parser leaves this nil, builder sets it based on StandaloneExpression:
	//   - For string templates (StandaloneExpression=false): always cel.StringType
	//   - For standalone expressions (StandaloneExpression=true): derived from OpenAPI schema
	ExpectedType *cel.Type

	// StandaloneExpression indicates if this is a single CEL expression vs a string template.
	// Set by: parser (both parser.go and schemaless.go)
	// Used by: builder.setExpectedTypeOnDescriptor() to determine how to set ExpectedType
	// Examples:
	//   true:  "${foo}" - single expression, type derived from schema
	//   false: "hello-${foo}" or "${foo}-${bar}" - string template, always produces string
	StandaloneExpression bool
}

// ResourceField ResourceVariable represents a variable in a resource. Variables are any
// field in a resource (under resources[*].definition) that is not a constant
// value a.k.a contains one or multiple expressions. For example
//
//	spec:
//	  replicas: ${schema.spec.mycustomReplicasField + 5}
//
// Contains a variable named "spec.mycustomReplicasField". Variables can be
// static or dynamic. Static variables are resolved at the beginning of the
// execution and their value is constant. Dynamic variables are resolved at
// runtime and their value can change during the execution.
//
// ResourceVariables are an extension of CELField, and they contain additional
// information about the variable kind.
type ResourceField struct {
	// CELField is the object that contains the expression, the path, and
	// the expected type (OpenAPI schema).
	FieldDescriptor
	// ResourceVariableKind is the kind of the variable (static or dynamic).
	Kind ResourceVariableKind
	// Dependencies is a list of resources this variable depends on. We need
	// this information to wait for the dependencies to be resolved before
	// evaluating the variable.
	Dependencies []string
	// NOTE(a-hilaly): I'm wondering if we should add another field to state
	// whether the variable is nullable or not. This can be useful... imagine
	// a dynamic variable that is not necessarily forcing a dependency.
}

// AddDependencies adds dependencies to the ResourceField.
func (rv *ResourceField) AddDependencies(dep ...string) {
	for _, d := range dep {
		if !slices.Contains(rv.Dependencies, d) {
			rv.Dependencies = append(rv.Dependencies, d)
		}
	}
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
