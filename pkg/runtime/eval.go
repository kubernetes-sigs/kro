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
	"time"

	"github.com/google/cel-go/cel"

	krocel "github.com/kubernetes-sigs/kro/pkg/cel"
	"github.com/kubernetes-sigs/kro/pkg/graph/variable"
)

// buildEnv creates a CEL environment for the given variable names.
func buildEnv(resourceIDs, listIDs []string) (*cel.Env, error) {
	slices.Sort(resourceIDs)
	slices.Sort(listIDs)

	return krocel.DefaultEnvironment(
		krocel.WithResourceIDs(resourceIDs),
		krocel.WithListVariables(listIDs),
	)
}

// evalExprAny evaluates an expression and caches the result.
func evalExprAny(env *cel.Env, expr *expressionEvaluationState, ctx map[string]any) (any, error) {
	if expr.Resolved {
		return expr.ResolvedValue, nil
	}

	val, err := evalRawCEL(env, expr.Expression, ctx)
	if err != nil {
		return nil, err
	}

	expr.Resolved = true
	expr.ResolvedValue = val
	return val, nil
}

// evalBoolExpr evaluates an expression that should return bool.
func evalBoolExpr(env *cel.Env, expr *expressionEvaluationState, ctx map[string]any) (bool, error) {
	if expr.Resolved {
		return expr.ResolvedValue.(bool), nil
	}

	val, err := evalRawCEL(env, expr.Expression, ctx)
	if err != nil {
		return false, err
	}
	if val == nil {
		return false, nil
	}
	result, ok := val.(bool)
	if !ok {
		return false, fmt.Errorf("expression %q did not return bool", expr.Expression)
	}

	expr.Resolved = true
	expr.ResolvedValue = result
	return result, nil
}

// evalListExpr evaluates an expression that should return a list.
func evalListExpr(env *cel.Env, expr *expressionEvaluationState, ctx map[string]any) ([]any, error) {
	if expr.Resolved {
		return expr.ResolvedValue.([]any), nil
	}

	val, err := evalRawCEL(env, expr.Expression, ctx)
	if err != nil {
		return nil, err
	}
	if val == nil {
		return nil, fmt.Errorf("expression %q returned null, expected list", expr.Expression)
	}
	result, ok := val.([]any)
	if !ok {
		return nil, fmt.Errorf("expression %q did not return a list", expr.Expression)
	}

	expr.Resolved = true
	expr.ResolvedValue = result
	return result, nil
}

// evalRawCEL evaluates a CEL expression string and returns the native Go value.
// CEL errors are returned as-is; callers should use isCELDataPending() to check
// if the error indicates data is pending and should be retried.
func evalRawCEL(env *cel.Env, expr string, ctx map[string]any) (any, error) {
	// Measure compilation time
	compileStart := time.Now()
	ast, issues := env.Compile(expr)
	compileDuration := time.Since(compileStart).Seconds()

	var compileErr error
	if issues != nil && issues.Err() != nil {
		compileErr = fmt.Errorf("compile error: %w", issues.Err())
	}
	krocel.Metrics.ObserveCompilation(compileDuration, compileErr)

	if compileErr != nil {
		return nil, compileErr
	}

	prg, err := env.Program(ast)
	if err != nil {
		return nil, fmt.Errorf("program error: %w", err)
	}

	// Measure evaluation time
	evalStart := time.Now()
	out, _, err := prg.Eval(ctx)
	evalDuration := time.Since(evalStart).Seconds()
	krocel.Metrics.ObserveEvaluation(evalDuration, err)

	if err != nil {
		return nil, err
	}

	return krocel.GoNativeType(out)
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
