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

package celinspector

import (
	"fmt"
	"strings"

	"github.com/google/cel-go/cel"
	exprpb "google.golang.org/genproto/googleapis/api/expr/v1alpha1"
)

type ResourceDependency struct {
	Name string
	Path string
}

type FunctionCall struct {
	Name      string
	Arguments []string
}

type UnknownResource struct {
	Name string
	Path string
}

type UnknownFunction struct {
	Name string
}

type ExpressionInspection struct {
	ResourceDependencies []ResourceDependency
	FunctionCalls        []FunctionCall
	UnknownResources     []UnknownResource
	UnknownFunctions     []UnknownFunction
}

type Inspector struct {
	env       *cel.Env
	resources map[string]struct{}
	functions map[string]struct{}
}

func NewInspector(resources []string, functions []string) (*Inspector, error) {
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

	env, err := cel.NewEnv(
		declarations...,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create CEL environment: %v", err)
	}

	return &Inspector{
		env:       env,
		resources: resourceMap,
		functions: functionMap,
	}, nil
}

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
	}
}

func (a *Inspector) Inspect(expression string) (ExpressionInspection, error) {
	ast, iss := a.env.Parse(expression)
	if iss.Err() != nil {
		return ExpressionInspection{}, fmt.Errorf("failed to parse expression: %v", iss.Err())
	}

	// p, _ := cel.AstToParsedExpr(ast)
	// p.GetExpr()

	return a.inspectAst(ast.Expr(), ""), nil
}

func (a *Inspector) inspectAst(expr *exprpb.Expr, currentPath string) ExpressionInspection {
	switch e := expr.ExprKind.(type) {
	case *exprpb.Expr_SelectExpr:
		newPath := currentPath
		if currentPath != "" {
			newPath += "."
		}
		newPath += e.SelectExpr.Field
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

func (a *Inspector) inspectCall(call *exprpb.Expr_Call, currentPath string) ExpressionInspection {
	inspection := ExpressionInspection{}

	if _, isFunction := a.functions[call.Function]; isFunction {
		functionCall := FunctionCall{
			Name: call.Function,
		}
		for _, arg := range call.Args {
			functionCall.Arguments = append(functionCall.Arguments, a.exprToString(arg))
		}
		inspection.FunctionCalls = append(inspection.FunctionCalls, functionCall)
	} else {
		if call.Target == nil && !isInternalFunction(call.Function) {
			// This is an unknown function, but not an internal one
			inspection.UnknownFunctions = append(inspection.UnknownFunctions, UnknownFunction{Name: call.Function})
		} else if call.Target != nil {
			targetInspection := a.inspectAst(call.Target, currentPath)
			inspection.ResourceDependencies = append(inspection.ResourceDependencies, targetInspection.ResourceDependencies...)
			inspection.FunctionCalls = append(inspection.FunctionCalls, targetInspection.FunctionCalls...)
			inspection.UnknownResources = append(inspection.UnknownResources, targetInspection.UnknownResources...)
			inspection.UnknownFunctions = append(inspection.UnknownFunctions, targetInspection.UnknownFunctions...)
			inspection.FunctionCalls = append(inspection.FunctionCalls, FunctionCall{
				Name: fmt.Sprintf("%s.%s", a.exprToString(call.Target), call.Function),
			})
		}
	}

	for _, arg := range call.Args {
		argInspection := a.inspectAst(arg, "")
		inspection.ResourceDependencies = append(inspection.ResourceDependencies, argInspection.ResourceDependencies...)
		inspection.FunctionCalls = append(inspection.FunctionCalls, argInspection.FunctionCalls...)
		inspection.UnknownResources = append(inspection.UnknownResources, argInspection.UnknownResources...)
		inspection.UnknownFunctions = append(inspection.UnknownFunctions, argInspection.UnknownFunctions...)
	}

	return inspection
}

func (a *Inspector) inspectIdent(ident *exprpb.Expr_Ident, currentPath string) ExpressionInspection {
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

func (a *Inspector) inspectComprehension(comp *exprpb.Expr_Comprehension, currentPath string) ExpressionInspection {
	inspection := ExpressionInspection{}

	iterRangeInspection := a.inspectAst(comp.IterRange, currentPath)
	inspection.ResourceDependencies = append(inspection.ResourceDependencies, iterRangeInspection.ResourceDependencies...)
	inspection.FunctionCalls = append(inspection.FunctionCalls, iterRangeInspection.FunctionCalls...)
	inspection.UnknownResources = append(inspection.UnknownResources, iterRangeInspection.UnknownResources...)
	inspection.UnknownFunctions = append(inspection.UnknownFunctions, iterRangeInspection.UnknownFunctions...)

	resultInspection := a.inspectAst(comp.Result, "")
	inspection.ResourceDependencies = append(inspection.ResourceDependencies, resultInspection.ResourceDependencies...)
	inspection.FunctionCalls = append(inspection.FunctionCalls, resultInspection.FunctionCalls...)
	inspection.UnknownResources = append(inspection.UnknownResources, resultInspection.UnknownResources...)
	inspection.UnknownFunctions = append(inspection.UnknownFunctions, resultInspection.UnknownFunctions...)

	if comp.LoopStep == nil {
		inspection.FunctionCalls = append(inspection.FunctionCalls, FunctionCall{
			Name:      "filter",
			Arguments: []string{a.exprToString(comp.IterRange), a.exprToString(comp.Result)},
		})
	} else {
		inspection.FunctionCalls = append(inspection.FunctionCalls, FunctionCall{
			Name:      "map",
			Arguments: []string{a.exprToString(comp.IterRange), a.exprToString(comp.Result)},
		})
	}

	return inspection
}

// exprToString converts an expression to a string representation
func (a *Inspector) exprToString(expr *exprpb.Expr) string {
	switch e := expr.ExprKind.(type) {
	case *exprpb.Expr_ConstExpr:
		// Handle constant expressions (literals)
		switch kind := e.ConstExpr.ConstantKind.(type) {
		case *exprpb.Constant_BoolValue:
			return fmt.Sprintf("%v", kind.BoolValue)
		case *exprpb.Constant_BytesValue:
			return fmt.Sprintf("%v", kind.BytesValue)
		case *exprpb.Constant_DoubleValue:
			return fmt.Sprintf("%v", kind.DoubleValue)
		case *exprpb.Constant_Int64Value:
			return fmt.Sprintf("%v", kind.Int64Value)
		case *exprpb.Constant_StringValue:
			return kind.StringValue
		case *exprpb.Constant_Uint64Value:
			return fmt.Sprintf("%v", kind.Uint64Value)
		case *exprpb.Constant_NullValue:
			return "null"
		default:
			return "<unknown constant>"
		}
	case *exprpb.Expr_IdentExpr:
		// Handle identifiers
		return e.IdentExpr.Name
	case *exprpb.Expr_SelectExpr:
		// Handle field selection
		return fmt.Sprintf("%s.%s", a.exprToString(e.SelectExpr.Operand), e.SelectExpr.Field)
	case *exprpb.Expr_CallExpr:
		// Handle function and method calls
		args := make([]string, len(e.CallExpr.Args))
		for i, arg := range e.CallExpr.Args {
			args[i] = a.exprToString(arg)
		}
		if e.CallExpr.Target != nil {
			// This is a method call
			return fmt.Sprintf("%s.%s(%s)", a.exprToString(e.CallExpr.Target), e.CallExpr.Function, strings.Join(args, ", "))
		}
		// This is a function call
		return fmt.Sprintf("%s(%s)", e.CallExpr.Function, strings.Join(args, ", "))
	case *exprpb.Expr_ComprehensionExpr:
		// Handle comprehensions (filters and maps)
		if e.ComprehensionExpr.LoopStep == nil {
			// This is a filter operation
			return fmt.Sprintf("filter(%s, %s)", a.exprToString(e.ComprehensionExpr.IterRange), a.exprToString(e.ComprehensionExpr.Result))
		}
		// This is a map operation
		return fmt.Sprintf("map(%s, %s)", a.exprToString(e.ComprehensionExpr.IterRange), a.exprToString(e.ComprehensionExpr.Result))
	default:
		return "<complex expression>"
	}
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
		// Add other internal functions as needed
	}
	return internalFunctions[name]
}
