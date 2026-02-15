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

	"github.com/kubernetes-sigs/kro/pkg/graph/variable"
)

// evalExprAny evaluates an expression using its pre-compiled Program and caches the result.
func evalExprAny(expr *expressionEvaluationState, ctx map[string]any) (any, error) {
	if expr.Resolved {
		return expr.ResolvedValue, nil
	}

	val, err := expr.Expression.Eval(filterContext(ctx, expr.Expression.References))
	if err != nil {
		return nil, err
	}

	expr.Resolved = true
	expr.ResolvedValue = val
	return val, nil
}

// evalBoolExpr evaluates an expression that should return bool.
func evalBoolExpr(expr *expressionEvaluationState, ctx map[string]any) (bool, error) {
	if expr.Resolved {
		return expr.ResolvedValue.(bool), nil
	}

	val, err := expr.Expression.Eval(filterContext(ctx, expr.Expression.References))
	if err != nil {
		return false, err
	}
	if val == nil {
		return false, nil
	}
	result, ok := val.(bool)
	if !ok {
		return false, fmt.Errorf("expression %q did not return bool", expr.Expression.Original)
	}

	expr.Resolved = true
	expr.ResolvedValue = result
	return result, nil
}

// evalListExpr evaluates an expression that should return a list.
func evalListExpr(expr *expressionEvaluationState, ctx map[string]any) ([]any, error) {
	if expr.Resolved {
		return expr.ResolvedValue.([]any), nil
	}

	val, err := expr.Expression.Eval(filterContext(ctx, expr.Expression.References))
	if err != nil {
		return nil, err
	}
	if val == nil {
		return nil, fmt.Errorf("expression %q returned null, expected list", expr.Expression.Original)
	}
	result, ok := val.([]any)
	if !ok {
		return nil, fmt.Errorf("expression %q did not return a list", expr.Expression.Original)
	}

	expr.Resolved = true
	expr.ResolvedValue = result
	return result, nil
}

func filterContext(ctx map[string]any, refs []string) map[string]any {
	if len(refs) == 0 {
		return ctx
	}
	filtered := make(map[string]any, len(refs))
	for _, ref := range refs {
		if v, ok := ctx[ref]; ok {
			filtered[ref] = v
		}
	}
	return filtered
}

// toFieldDescriptors converts ResourceFields to FieldDescriptors for the resolver.
func toFieldDescriptors(vars []*variable.ResourceField) []variable.FieldDescriptor {
	result := make([]variable.FieldDescriptor, len(vars))
	for i, v := range vars {
		result[i] = variable.FieldDescriptor{
			Path:                 v.Path,
			Expressions:          v.Expressions,
			StandaloneExpression: v.StandaloneExpression,
		}
	}
	return result
}
