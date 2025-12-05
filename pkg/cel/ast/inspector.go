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

package ast

import (
	"fmt"
	"strings"

	"github.com/google/cel-go/cel"
	celast "github.com/google/cel-go/common/ast"
	"github.com/google/cel-go/common/types"
)

// ResourceDependency represents a resource and its accessed path within a CEL expression.
// For example, in the expression "deployment.spec.replicas > 0",
// ID would be "deployment" and Path would be "deployment.spec.replicas"
type ResourceDependency struct {
	// ID is the root resource identifier (e.g "deployment", "service", "pod")
	ID string
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
// or misspelled resource ids.
type UnknownResource struct {
	// ID is the undeclared resource identifier that was referenced
	ID string
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
	// by kro engine, standard libraries or CEL built-in functions.
	UnknownFunctions []UnknownFunction
}

func (e *ExpressionInspection) merge(other ExpressionInspection) {
	e.ResourceDependencies = append(e.ResourceDependencies, other.ResourceDependencies...)
	e.FunctionCalls = append(e.FunctionCalls, other.FunctionCalls...)
	e.UnknownResources = append(e.UnknownResources, other.UnknownResources...)
	e.UnknownFunctions = append(e.UnknownFunctions, other.UnknownFunctions...)
}

// Inspector analyzes CEL expressions to discover resource and function dependencies.
// It maintains the CEL environment and tracks which resources and functions are known.
type Inspector struct {
	// env is the CEL evaluation environment containing type definitions and functions
	env *cel.Env

	// resources is a set of known resource ids that can be referenced in expressions
	resources map[string]struct{}

	// functions is a set of known function names that can be called in expressions
	functions map[string]struct{}

	// Track active loop variables
	loopVars map[string]struct{}
}

// knownFunctions contains the list of all CEL functions that are supported
var knownFunctions = []string{
	"random.seededString",
	"base64.decode",
	"base64.encode",
}

// NewInspectorWithEnv creates a new Inspector with the given CEL environment and resource names.
func NewInspectorWithEnv(env *cel.Env, resources []string) *Inspector {
	resourceMap := map[string]struct{}{}
	for _, r := range resources {
		resourceMap[r] = struct{}{}
	}

	functionMap := map[string]struct{}{}
	for _, fn := range knownFunctions {
		functionMap[fn] = struct{}{}
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
	parsed, iss := a.env.Parse(expression)
	if iss.Err() != nil {
		return ExpressionInspection{}, fmt.Errorf("parse error: %v", iss.Err())
	}
	native := parsed.NativeRep()
	return a.inspectExpr(native, native.Expr(), ""), nil
}

// inspectExpr dispatches analysis based on the expression's syntactic kind.
//
// It recursively walks the CEL native AST and accumulates inspection results:
//   - Identifier resolution (resource vs. unknown).
//   - Field selections forming access paths.
//   - Function and operator calls.
//   - List/map/struct traversal.
//   - Comprehension constructs such as list filters and transforms.
//
// This is the central traversal function from which all specialized inspectors
// are invoked.
func (a *Inspector) inspectExpr(ast *celast.AST, expr celast.Expr, path string) ExpressionInspection {
	switch expr.Kind() {
	case celast.IdentKind:
		return a.inspectIdent(expr, path)
	case celast.SelectKind:
		s := expr.AsSelect()
		newPath := s.FieldName()
		if path != "" {
			newPath = newPath + "." + path
		}
		return a.inspectExpr(ast, s.Operand(), newPath)
	case celast.CallKind:
		return a.inspectCall(ast, expr.AsCall(), path)
	case celast.ComprehensionKind:
		return a.inspectComprehension(ast, expr.AsComprehension(), path)
	case celast.ListKind:
		return a.inspectList(ast, expr)
	case celast.MapKind, celast.StructKind:
		return a.inspectChildren(ast, expr)
	default:
		return ExpressionInspection{}
	}
}

// inspectChildren analyzes all direct child nodes of the given expression.
//
// It uses celast.NavigateExpr to enumerate sub-expressions and merges the
// analysis results of each child. This is used for AST nodes that simply
// aggregate other expressions, such as structs, maps, and list literals.
func (a *Inspector) inspectChildren(ast *celast.AST, expr celast.Expr) ExpressionInspection {
	out := ExpressionInspection{}
	nav := celast.NavigateExpr(ast, expr)
	for _, ch := range nav.Children() {
		out.merge(a.inspectExpr(ast, ch, ""))
	}
	return out
}

// inspectIdent analyzes an identifier reference.
//
// Behavior:
//   - If the identifier is a loop variable, it is ignored.
//   - If it matches a declared resource, a ResourceDependency is recorded.
//   - If it is not internal and not declared, it is treated as an UnknownResource.
//
// The `path` argument provides any accumulated field-access suffix when the
// identifier is part of a Select chain such as deployment.spec.replicas.
func (a *Inspector) inspectIdent(expr celast.Expr, path string) ExpressionInspection {
	name := expr.AsIdent()
	if _, ok := a.loopVars[name]; ok {
		return ExpressionInspection{}
	}

	if _, ok := a.resources[name]; ok {
		full := name
		if path != "" {
			full += "." + path
		}
		return ExpressionInspection{
			ResourceDependencies: []ResourceDependency{{ID: name, Path: full}},
		}
	}

	if !isInternalIdentifier(name) {
		full := name
		if path != "" {
			full += "." + path
		}
		return ExpressionInspection{
			UnknownResources: []UnknownResource{{ID: name, Path: full}},
		}
	}

	return ExpressionInspection{}
}

// inspectCall analyzes a function or method invocation.
//
// Responsibilities:
//   - Recursively inspect all argument expressions.
//   - For member functions, inspect the target expression and record a synthetic
//     function name of the form "<target>.<method>".
//   - For direct calls, record known functions and their arguments.
//   - If the function is neither known nor registered in the CEL environment,
//     record it as an UnknownFunction.
//
// Operators represented as CEL internal functions (e.g., "_+_") are handled
// separately within exprToString and callToString and do not affect dependency
// detection.
func (a *Inspector) inspectCall(ast *celast.AST, call celast.CallExpr, path string) ExpressionInspection {
	out := ExpressionInspection{}

	for _, arg := range call.Args() {
		out.merge(a.inspectExpr(ast, arg, ""))
	}

	fn := call.FunctionName()

	// Namespaced (member) function: target.method
	if call.IsMemberFunction() {
		t := call.Target()
		if t != nil {
			targetName := a.exprToString(ast, t)
			full := fmt.Sprintf("%s.%s", targetName, fn)

			// Still inspect target for resource usage
			out.merge(a.inspectExpr(ast, t, path))

			// Treat chained method call as unknown unless known
			out.FunctionCalls = append(out.FunctionCalls, FunctionCall{
				Name: full,
			})
		}
		return out
	}

	// Direct function call
	if _, ok := a.functions[fn]; ok {
		args := make([]string, len(call.Args()))
		for i, arg := range call.Args() {
			args[i] = a.exprToString(ast, arg)
		}
		out.FunctionCalls = append(out.FunctionCalls, FunctionCall{
			Name:      fn,
			Arguments: args,
		})
	} else if !a.env.HasFunction(fn) {
		out.UnknownFunctions = append(out.UnknownFunctions, UnknownFunction{Name: fn})
	}

	return out
}

// inspectComprehension analyzes CEL comprehension expressions.
//
// A comprehension represents constructs such as filtering or mapping, expressed
// as:
//
//	{iterVar in iterRange | loopCondition : result}
//
// Steps performed:
//   - Track `iterVar` as a loop variable for the duration of the analysis.
//   - Inspect the iteration range, loop condition, step expression, and result.
//   - Synthesize a "filter" FunctionCall capturing the comprehension structure.
//     (This is informational metadata for consumers of the inspector.)
//
// Loop variables are excluded from normal identifier handling to avoid falsely
// reporting them as unknown resources.
func (a *Inspector) inspectComprehension(ast *celast.AST, comp celast.ComprehensionExpr, path string) ExpressionInspection {
	out := ExpressionInspection{}

	iterVar := comp.IterVar()
	a.loopVars[iterVar] = struct{}{}
	defer delete(a.loopVars, iterVar)

	out.merge(a.inspectExpr(ast, comp.IterRange(), path))

	if cond := comp.LoopCondition(); cond != nil {
		out.merge(a.inspectExpr(ast, cond, ""))
	}

	if step := comp.LoopStep(); step != nil {
		out.merge(a.inspectExpr(ast, step, ""))
	}

	out.merge(a.inspectExpr(ast, comp.Result(), ""))

	// Now add synthetic "filter"
	call := FunctionCall{Name: "filter", Arguments: []string{
		a.exprToString(ast, comp.IterRange()),
		a.exprToString(ast, comp.LoopStep()),
		a.exprToString(ast, comp.Result()),
	}}
	out.FunctionCalls = append(out.FunctionCalls, call)

	return out
}

// inspectList analyzes a list literal.
//
// It inspects all child elements (expressions inside the list) and records a
// synthetic FunctionCall named "createList" whose argument is the string
// representation of the list literal. This provides consistent function-like
// tracking for structural constructs that implicitly create new values.
func (a *Inspector) inspectList(ast *celast.AST, expr celast.Expr) ExpressionInspection {
	out := a.inspectChildren(ast, expr)
	out.FunctionCalls = append(out.FunctionCalls, FunctionCall{
		Name:      "createList",
		Arguments: []string{a.listExpressionToString(ast, expr)},
	})
	return out
}

// exprToString produces a deterministic string representation of an expression.
//
// It is used to serialize argument expressions when recording FunctionCall
// metadata and for debugging or tooling consumers. The representation reflects
// CEL syntax closely but is not guaranteed to round-trip.
func (a *Inspector) exprToString(ast *celast.AST, expr celast.Expr) string {
	switch expr.Kind() {
	case celast.IdentKind:
		return expr.AsIdent()
	case celast.LiteralKind:
		return types.Format(expr.AsLiteral())
	case celast.SelectKind:
		s := expr.AsSelect()
		return fmt.Sprintf("%s.%s", a.exprToString(ast, s.Operand()), s.FieldName())
	case celast.CallKind:
		return a.callToString(ast, expr.AsCall())
	case celast.ListKind:
		return a.listExpressionToString(ast, expr)
	case celast.MapKind:
		m := expr.AsMap()
		parts := make([]string, 0, len(m.Entries()))
		for _, entry := range m.Entries() {
			entry := entry.AsMapEntry()
			key := a.exprToString(ast, entry.Key())
			val := a.exprToString(ast, entry.Value())
			parts = append(parts, key+": "+val)
		}
		return fmt.Sprintf("{%s}", strings.Join(parts, ", "))
	case celast.StructKind:
		s := expr.AsStruct()
		fields := make([]string, 0, len(s.Fields()))
		for _, f := range s.Fields() {
			f := f.AsStructField()
			fields = append(fields, f.Name()+": "+a.exprToString(ast, f.Value()))
		}
		return fmt.Sprintf("%s{%s}", s.TypeName(), strings.Join(fields, ", "))
	default:
		return "<unknown>"
	}
}

// callToString formats a function or operator invocation into a human-readable
// string.
//
// Operator functions with CEL’s internal names (e.g., "_+_") are converted into
// their infix forms where applicable. Member functions are rendered as
// "<target>.<method>(args...)". All other calls are rendered as standard
// function calls "fn(arg1, arg2, ...)".
//
// This function is used only for metadata representation and does not affect
// analysis logic.
func (a *Inspector) callToString(ast *celast.AST, call celast.CallExpr) string {
	fn := call.FunctionName()
	args := call.Args()
	parts := make([]string, len(args))
	for i, arg := range args {
		parts[i] = a.exprToString(ast, arg)
	}

	// Binary/unary operators
	if strings.HasPrefix(fn, "_") {
		switch fn {
		case "_+_", "_-_", "_*_", "_/_", "_%_", "_<_", "_<=_", "_>_", "_>=_", "_==_", "_!=_":
			if len(parts) == 2 {
				op := strings.Trim(fn, "_")
				return fmt.Sprintf("(%s %s %s)", parts[0], op, parts[1])
			}
		case "_&&_":
			return fmt.Sprintf("(%s && %s)", parts[0], parts[1])
		case "_||_":
			return fmt.Sprintf("(%s || %s)", parts[0], parts[1])
		case "_?_:_":
			return fmt.Sprintf("(%s ? %s : %s)", parts[0], parts[1], parts[2])
		case "_[_]":
			return fmt.Sprintf("%s[%s]", parts[0], parts[1])
		}
	}

	if call.IsMemberFunction() && call.Target() != nil {
		return fmt.Sprintf("%s.%s(%s)",
			a.exprToString(ast, call.Target()),
			fn,
			strings.Join(parts, ", "),
		)
	}

	return fmt.Sprintf("%s(%s)", fn, strings.Join(parts, ", "))
}

// listExpressionToString formats a list literal by serializing each element via
// exprToString and joining them within brackets.
func (a *Inspector) listExpressionToString(ast *celast.AST, expr celast.Expr) string {
	nav := celast.NavigateExpr(ast, expr)
	children := nav.Children()

	out := make([]string, len(children))
	for i, ch := range children {
		out[i] = a.exprToString(ast, ch)
	}

	return fmt.Sprintf("[%s]", strings.Join(out, ", "))
}

func isInternalIdentifier(name string) bool {
	return name == "@result" || strings.HasPrefix(name, "$$")
}
