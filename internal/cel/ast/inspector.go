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

package ast

import (
	"fmt"
	"strings"

	scel "github.com/aws-controllers-k8s/symphony/internal/cel"
	"github.com/google/cel-go/cel"
	exprpb "google.golang.org/genproto/googleapis/api/expr/v1alpha1"
)

// ResourceDependency represents a resource and its accessed path within a CEL expression.
// For example, in the expression "deployment.spec.replicas > 0",
// Name would be "deployment" and Path would be "deployment.spec.replicas"
type ResourceDependency struct {
	// Name is the root resource identifier (e.g "deployment", "service", "pod")
	Name string
	// Path is the full access path including nested fields
	// For example: "deployment.spec.replicas" or "service.metadata.name"
	Path string
}

// FunctionCall represents an invocation of a declared function within a CEL expression.
// This tracks both the function name and its arguments as they appear in the expression
//
// The arguments are string representations of the AST nodes. We mainly ignore them for
// now, but they could be used to further analyze the expression.
type FunctionCall struct {
	// Name is the function identifier
	// For example: "hash" "toLower"
	Name string

	// Arguments contains the string representation of each argument passed to the function
	// For example: ["deployment.name", "'frontend'"] for a call like concat(deployment.name, "frontend")
	Arguments []string
}

// UnknownResource represents a resource reference in the expression that wasn't
// declared in the known resources list. This helps identify potentially missing
// or misspelled resource names.
type UnknownResource struct {
	// Name is the undeclared resource identifier that was referenced
	Name string
	// Path is the full access path that was attempted with this unknown resource
	// For example: "unknown_resource.field.subfield"
	Path string
}

// UnknownFunction represents a function call in the expression that wasn't
// declared in the known functions list and isn't a CEL built in function.
type UnknownFunction struct {
	// Name is the undeclared function identifier that was called
	Name string
}

// ExpressionInspection contains all the findings from analyzing a CEL expression.
// It tracks all resources accessed, functions called, and any unknown references.
type ExpressionInspection struct {
	// ResourceDependencies lists all known resources and their access paths
	// used in the expression
	ResourceDependencies []ResourceDependency
	// FunctionCalls lists all known function calls and their arguments found
	// in the expression
	FunctionCalls []FunctionCall
	// UnknownResources lists any resource references that weren't declared
	UnknownResources []UnknownResource
	// UnknownFunctions lists any function calls that weren't declared, either
	// by symphony engine, standard libraries or CEL built-in functions.
	UnknownFunctions []UnknownFunction
}

// Inspector analyzes CEL expressions to discover resource and function dependencies.
// It maintains the CEL environment and tracks which resources and functions are known.
type Inspector struct {
	// env is the CEL evaluation environment containing type definitions and functions
	env *cel.Env

	// resources is a set of known resource names that can be referenced in expressions
	resources map[string]struct{}

	// functions is a set of known function names that can be called in expressions
	functions map[string]struct{}

	// Track active loop variables
	loopVars map[string]struct{}
}

// DefaultInspector creates a new Inspector instance with the given resources and functions.
//
// TODO(a-hilaly): unify CEL environment creation with the rest of the codebase.
func DefaultInspector(resources []string, functions []string) (*Inspector, error) {
	declarations := make([]cel.EnvOption, 0, len(resources)+len(functions))

	resourceMap := make(map[string]struct{})
	for _, resource := range resources {
		declarations = append(declarations, cel.Variable(resource, cel.AnyType))
		resourceMap[resource] = struct{}{}
	}

	functionMap := make(map[string]struct{})
	for _, function := range functions {
		fn := cel.Function(function, cel.Overload(function+"_any", []*cel.Type{cel.AnyType}, cel.AnyType))
		declarations = append(declarations, fn)
		functionMap[function] = struct{}{}
	}

	env, err := scel.DefaultEnvironment(scel.WithCustomDeclarations(declarations))
	if err != nil {
		return nil, fmt.Errorf("failed to create CEL environment: %v", err)
	}

	return &Inspector{
		env:       env,
		resources: resourceMap,
		functions: functionMap,
		loopVars:  make(map[string]struct{}),
	}, nil
}

// NewInspectorWithEnv creates a new Inspector instance with the given resources and functions
// using the provided CEL environment.
func NewInspectorWithEnv(env *cel.Env, resources []string, functions []string) *Inspector {
	resourceMap := make(map[string]struct{})
	for _, resource := range resources {
		resourceMap[resource] = struct{}{}
	}

	functionMap := make(map[string]struct{})
	for _, function := range functions {
		functionMap[function] = struct{}{}
	}

	return &Inspector{
		env:       env,
		resources: resourceMap,
		functions: functionMap,
		loopVars:  make(map[string]struct{}),
	}
}

// Inspect analyzes the given CEL expression and returns an ExpressionInspection.
//
// This function can be called multiple times with different expressions using the same
// Inspector instance (AND environment).
func (a *Inspector) Inspect(expression string) (ExpressionInspection, error) {
	ast, iss := a.env.Parse(expression)
	if iss.Err() != nil {
		return ExpressionInspection{}, fmt.Errorf("failed to parse expression: %v", iss.Err())
	}

	parsed, err := cel.AstToParsedExpr(ast)
	if err != nil {
		return ExpressionInspection{}, fmt.Errorf("failed to check expression: %v", err)
	}
	return a.inspectAst(parsed.GetExpr(), ""), nil
}

// inspectAst recursively traverses a CEL expressions AST and collects all resource
// dependencies and function calls. It builds paths for nested field access and handles
// different expression types.
func (a *Inspector) inspectAst(expr *exprpb.Expr, currentPath string) ExpressionInspection {
	switch e := expr.ExprKind.(type) {
	case *exprpb.Expr_SelectExpr:
		// build the path in **reverse order** /!\
		newPath := e.SelectExpr.Field
		if currentPath != "" {
			newPath = newPath + "." + currentPath
		}
		return a.inspectAst(e.SelectExpr.Operand, newPath)
	case *exprpb.Expr_CallExpr:
		return a.inspectCall(e.CallExpr, currentPath)
	case *exprpb.Expr_IdentExpr:
		return a.inspectIdent(e.IdentExpr, currentPath)
	case *exprpb.Expr_ComprehensionExpr:
		return a.inspectComprehension(e.ComprehensionExpr, currentPath)
	default:
		return ExpressionInspection{}
	}
}

// inspectCall analyzes function calls and method invocations within a CEL expression.
// It tracks three types of calls:
// 1. Custom functions (declared in Inspector initialization)
// 2. Method calls on resources ( list.filter(...))
// 3. Unknown functions (neither custom nor internal)
func (a *Inspector) inspectCall(call *exprpb.Expr_Call, currentPath string) ExpressionInspection {
	inspection := ExpressionInspection{}

	// First process arguments to get their dependencies
	for _, arg := range call.Args {
		argInspection := a.inspectAst(arg, "")
		inspection.ResourceDependencies = append(inspection.ResourceDependencies, argInspection.ResourceDependencies...)
		inspection.FunctionCalls = append(inspection.FunctionCalls, argInspection.FunctionCalls...)
		inspection.UnknownResources = append(inspection.UnknownResources, argInspection.UnknownResources...)
		inspection.UnknownFunctions = append(inspection.UnknownFunctions, argInspection.UnknownFunctions...)
	}

	// Handle the current function - only if it's not part of a chain
	if _, isFunction := a.functions[call.Function]; isFunction && call.Target == nil {
		functionCall := FunctionCall{
			Name: call.Function,
		}
		for _, arg := range call.Args {
			functionCall.Arguments = append(functionCall.Arguments, a.exprToString(arg))
		}
		inspection.FunctionCalls = append(inspection.FunctionCalls, functionCall)
	}

	// Then handle the target if it exists
	if call.Target != nil {
		targetInspection := a.inspectAst(call.Target, currentPath)
		inspection.ResourceDependencies = append(inspection.ResourceDependencies, targetInspection.ResourceDependencies...)
		inspection.FunctionCalls = append(inspection.FunctionCalls, targetInspection.FunctionCalls...)
		inspection.UnknownResources = append(inspection.UnknownResources, targetInspection.UnknownResources...)
		inspection.UnknownFunctions = append(inspection.UnknownFunctions, targetInspection.UnknownFunctions...)

		// Add the chained call representation
		inspection.FunctionCalls = append(inspection.FunctionCalls, FunctionCall{
			Name: fmt.Sprintf("%s.%s", a.exprToString(call.Target), call.Function),
		})
	} else if !isInternalFunction(call.Function) {
		// This is an unknown function, but not an internal one
		inspection.UnknownFunctions = append(inspection.UnknownFunctions, UnknownFunction{Name: call.Function})
	}

	return inspection
}

// inspectIdent analyzes identifier expressions in CEL and determines if they are known resources
// or unknown references. It handles the base identifiers in field access chains and distinguishes
// between declared resources and unknown/internal identifiers.
func (a *Inspector) inspectIdent(ident *exprpb.Expr_Ident, currentPath string) ExpressionInspection {
	// Check if it's a loop variable
	if _, isLoopVar := a.loopVars[ident.Name]; isLoopVar {
		return ExpressionInspection{} // Loop variables are not resources
	}

	if _, isResource := a.resources[ident.Name]; isResource {
		fullPath := ident.Name
		if currentPath != "" {
			fullPath += "." + currentPath
		}
		return ExpressionInspection{
			ResourceDependencies: []ResourceDependency{{
				Name: ident.Name,
				Path: fullPath,
			}},
		}
	}
	// If it's not a known resource, it's an unknown resource
	if !isInternalIdentifier(ident.Name) {
		path := ident.Name
		if currentPath != "" {
			path += "." + currentPath
		}
		return ExpressionInspection{
			UnknownResources: []UnknownResource{{
				Name: ident.Name,
				Path: path,
			}},
		}
	}
	return ExpressionInspection{}
}

// inspectComprehension analyzes list comprehensions in CEL expressions (filter and map operations).
// It tracks dependencies from the iteration range, condition, step, and result expressions.
func (a *Inspector) inspectComprehension(comp *exprpb.Expr_Comprehension, currentPath string) ExpressionInspection {
	inspection := ExpressionInspection{}

	// Variable scoping in CEL expressions requires careful handling of identifiers.
	// Consider this example of variable shadowing:
	//
	// given:
	//   - a declared resource: "deployment"
	//   - an expression: `i + deployment.metadata.labels.filter(i, i == "something")`
	//
	// The identifier 'i' appears in two contexts:
	//   1. As a free variable: `i +` (should be marked as unknown resource)
	//   2. As a loop variable: `filter(i, i == "something")` (should be ignored)
	//
	// Even though the same identifier 'i' is used, it has different semantics:
	//   - The first 'i' is a reference to an undeclared resource
	//   - The second 'i' is a scoped variable within the filter comprehension
	//   - The third 'i' refers to the loop variable from the filter
	//
	// This demonstrates why we need to:
	//   1. Track loop variables separately from resources
	//   2. Consider the scope of each identifier
	//   3. Properly handle variable shadowing
	a.loopVars[comp.IterVar] = struct{}{}
	defer delete(a.loopVars, comp.IterVar)

	// Inspect the range we're iterating over
	iterRangeInspection := a.inspectAst(comp.IterRange, currentPath)
	inspection.ResourceDependencies = append(inspection.ResourceDependencies, iterRangeInspection.ResourceDependencies...)
	inspection.FunctionCalls = append(inspection.FunctionCalls, iterRangeInspection.FunctionCalls...)
	inspection.UnknownResources = append(inspection.UnknownResources, iterRangeInspection.UnknownResources...)
	inspection.UnknownFunctions = append(inspection.UnknownFunctions, iterRangeInspection.UnknownFunctions...)

	// For filters, inspect the condition
	if comp.LoopCondition != nil {
		conditionInspection := a.inspectAst(comp.LoopCondition, "")
		inspection.ResourceDependencies = append(inspection.ResourceDependencies, conditionInspection.ResourceDependencies...)
		inspection.FunctionCalls = append(inspection.FunctionCalls, conditionInspection.FunctionCalls...)
		inspection.UnknownResources = append(inspection.UnknownResources, conditionInspection.UnknownResources...)
		inspection.UnknownFunctions = append(inspection.UnknownFunctions, conditionInspection.UnknownFunctions...)
	}

	// For maps, inspect the loop step (transformation)
	if comp.LoopStep != nil {
		stepInspection := a.inspectAst(comp.LoopStep, "")
		inspection.ResourceDependencies = append(inspection.ResourceDependencies, stepInspection.ResourceDependencies...)
		inspection.FunctionCalls = append(inspection.FunctionCalls, stepInspection.FunctionCalls...)
		inspection.UnknownResources = append(inspection.UnknownResources, stepInspection.UnknownResources...)
		inspection.UnknownFunctions = append(inspection.UnknownFunctions, stepInspection.UnknownFunctions...)
	}

	// Inspect the result expression
	resultInspection := a.inspectAst(comp.Result, "")
	inspection.ResourceDependencies = append(inspection.ResourceDependencies, resultInspection.ResourceDependencies...)
	inspection.FunctionCalls = append(inspection.FunctionCalls, resultInspection.FunctionCalls...)
	inspection.UnknownResources = append(inspection.UnknownResources, resultInspection.UnknownResources...)
	inspection.UnknownFunctions = append(inspection.UnknownFunctions, resultInspection.UnknownFunctions...)

	// Record the comprehension operation
	if comp.LoopStep == nil {
		inspection.FunctionCalls = append(inspection.FunctionCalls, FunctionCall{
			Name: "filter",
			Arguments: []string{
				a.exprToString(comp.IterRange),
				a.exprToString(comp.LoopCondition), // Add filter condition
				a.exprToString(comp.Result),
			},
		})
	} else {
		inspection.FunctionCalls = append(inspection.FunctionCalls, FunctionCall{
			Name: "map",
			Arguments: []string{
				a.exprToString(comp.IterRange),
				a.exprToString(comp.LoopStep), // Add map transformation
				a.exprToString(comp.Result),
			},
		})
	}

	return inspection
}

// exprToString converts a CEL expression to its string representation.
// This is used primarily for recording function arguments and creating readable output.
func (a *Inspector) exprToString(expr *exprpb.Expr) string {
	if expr == nil {
		return "<nil>"
	}

	switch e := expr.ExprKind.(type) {
	case *exprpb.Expr_ConstExpr:
		return a.constantExpressionToString(e.ConstExpr)

	case *exprpb.Expr_IdentExpr:
		return e.IdentExpr.Name

	case *exprpb.Expr_SelectExpr:
		return fmt.Sprintf("%s.%s", a.exprToString(e.SelectExpr.Operand), e.SelectExpr.Field)

	case *exprpb.Expr_CallExpr:
		return a.callExpressionToString(e.CallExpr)

	case *exprpb.Expr_ListExpr:
		return a.listExpressionToString(e.ListExpr)

	case *exprpb.Expr_StructExpr:
		return a.structExpressionToString(e.StructExpr)

	default:
		return fmt.Sprintf("<unknown expression type: %T>", e)
	}
}

// constantExpressionToString converts a constant expression to its string representation.
func (a *Inspector) constantExpressionToString(c *exprpb.Constant) string {
	switch kind := c.ConstantKind.(type) {
	case *exprpb.Constant_BoolValue:
		return fmt.Sprintf("%v", kind.BoolValue)
	case *exprpb.Constant_BytesValue:
		return fmt.Sprintf("b\"%s\"", kind.BytesValue)
	case *exprpb.Constant_DoubleValue:
		return fmt.Sprintf("%v", kind.DoubleValue)
	case *exprpb.Constant_Int64Value:
		return fmt.Sprintf("%v", kind.Int64Value)
	case *exprpb.Constant_StringValue:
		return fmt.Sprintf("%q", kind.StringValue)
	case *exprpb.Constant_Uint64Value:
		return fmt.Sprintf("%vu", kind.Uint64Value)
	case *exprpb.Constant_NullValue:
		return "null"
	default:
		return "<unknown constant>"
	}
}

// callExpressionToString converts a function call expression to its string representation.
// This includes both regular function calls and operator calls.
func (a *Inspector) callExpressionToString(call *exprpb.Expr_Call) string {
	args := make([]string, len(call.Args))
	for i, arg := range call.Args {
		args[i] = a.exprToString(arg)
	}

	// Handle special operators
	if strings.HasPrefix(call.Function, "_") && strings.HasSuffix(call.Function, "_") {
		switch call.Function {
		case "_+_", "_-_", "_*_", "_/_", "_%_", "_<_", "_<=_", "_>_", "_>=_", "_==_", "_!=_":
			if len(args) == 2 {
				op := strings.Trim(call.Function, "_")
				return fmt.Sprintf("(%s %s %s)", args[0], op, args[1])
			}
		case "_&&_":
			if len(args) == 2 {
				return fmt.Sprintf("(%s && %s)", args[0], args[1])
			}
		case "_||_":
			if len(args) == 2 {
				return fmt.Sprintf("(%s || %s)", args[0], args[1])
			}
		case "_?_:_":
			if len(args) == 3 {
				return fmt.Sprintf("(%s ? %s : %s)", args[0], args[1], args[2])
			}
		case "_[_]":
			if len(args) == 2 {
				return fmt.Sprintf("%s[%s]", args[0], args[1])
			}
		}
	}

	if call.Target != nil {
		// Method call e.g bucket.metadata.labels.keys()
		return fmt.Sprintf("%s.%s(%s)", a.exprToString(call.Target), call.Function, strings.Join(args, ", "))
	}

	// Regular function call
	return fmt.Sprintf("%s(%s)", call.Function, strings.Join(args, ", "))
}

func (a *Inspector) listExpressionToString(list *exprpb.Expr_CreateList) string {
	elements := make([]string, len(list.Elements))
	for i, elem := range list.Elements {
		elements[i] = a.exprToString(elem)
	}
	return fmt.Sprintf("[%s]", strings.Join(elements, ", "))
}

func (a *Inspector) structExpressionToString(s *exprpb.Expr_CreateStruct) string {
	if s.MessageName != "" {
		// Message construction
		entries := make([]string, len(s.Entries))
		for i, entry := range s.Entries {
			if field := entry.GetFieldKey(); field != "" {
				value := a.exprToString(entry.GetValue())
				entries[i] = fmt.Sprintf("%s: %s", field, value)
			}
		}
		return fmt.Sprintf("%s{%s}", s.MessageName, strings.Join(entries, ", "))
	}

	// Regular struct/map creation
	entries := make([]string, len(s.Entries))
	for i, entry := range s.Entries {
		value := a.exprToString(entry.GetValue())
		if field := entry.GetFieldKey(); field != "" {
			entries[i] = fmt.Sprintf("%s: %s", field, value)
		} else if key := entry.GetMapKey(); key != nil {
			entries[i] = fmt.Sprintf("%s: %s", a.exprToString(key), value)
		}
	}
	return fmt.Sprintf("{%s}", strings.Join(entries, ", "))
}

func isInternalIdentifier(name string) bool {
	return name == "__result__" || strings.HasPrefix(name, "$$")
}

func isInternalFunction(name string) bool {
	internalFunctions := map[string]bool{
		"_+_":     true,
		"_-_":     true,
		"_*_":     true,
		"_/_":     true,
		"_%_":     true,
		"_<_":     true,
		"_<=_":    true,
		"_>_":     true,
		"_>=_":    true,
		"_==_":    true,
		"_!=_":    true,
		"_&&_":    true,
		"_||_":    true,
		"_?_:_":   true,
		"_[_]":    true,
		"size":    true,
		"in":      true,
		"matches": true,
		// types
		"int":       true,
		"uint":      true,
		"double":    true,
		"bool":      true,
		"string":    true,
		"bytes":     true,
		"timestamp": true,
		"duration":  true,
		"type":      true,

		// Collection Functions
		"filter":     true,
		"map":        true,
		"all":        true,
		"exists":     true,
		"exists_one": true,
	}
	return internalFunctions[name]
}
