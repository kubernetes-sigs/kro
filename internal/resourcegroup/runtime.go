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

package resourcegroup

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/aws-controllers-k8s/symphony/internal/celutil"
	"github.com/aws-controllers-k8s/symphony/internal/typesystem/parser"
	"github.com/aws-controllers-k8s/symphony/internal/typesystem/resolver"
)

type RuntimeResourceGroup struct {
	// During runtime we'll have a runtime representation of the resource group
	Instance          *unstructured.Unstructured
	Resources         map[string]*unstructured.Unstructured
	ResolvedResources map[string]*unstructured.Unstructured

	// This will be used as a read-only reference to the original resource group
	ResourceGroup *ResourceGroup

	RuntimeVariables map[string][]*RuntimeVariable

	ExpressionsCache map[string]*RuntimeVariable
}

type RuntimeVariable struct {
	Expression    string
	Dependencies  []string
	Kind          ResourceVariableKind
	Resolved      bool
	ResolvedValue interface{}
}

func (rt *RuntimeResourceGroup) ResolveStaticVariables() error {
	env, err := celutil.NewEnvironement(&celutil.EnvironementOptions{
		ResourceNames: []string{"spec"},
	})
	if err != nil {
		return err
	}

	for _, variable := range rt.ExpressionsCache {
		if variable.Kind == ResourceVariableKindStatic {
			ast, issues := env.Compile(variable.Expression)
			if issues != nil {
				return issues.Err()
			}
			program, err := env.Program(ast)
			if err != nil {
				return err
			}
			val, _, err := program.Eval(map[string]interface{}{
				"spec": rt.Instance.Object["spec"],
			})
			if err != nil {
				return err
			}
			value, err := celutil.ConvertCELtoGo(val)
			if err != nil {
				return err
			}

			variable.Resolved = true
			variable.ResolvedValue = value
		}
	}
	return nil
}

// ResourceResolved checks if a resource has all its variables resolved
// and returns true if all are resolved, false otherwise.
func (rt *RuntimeResourceGroup) ResourceResolved(resource string) bool {
	for _, variable := range rt.RuntimeVariables[resource] {
		if !variable.Resolved {
			return false
		}
	}
	return true
}

// CanResolveResource checks if a resource can be resolved by checking
// if all its dependencies are resolved.
func (rt *RuntimeResourceGroup) CanResolveResource(resource string) bool {
	if !rt.ResourceResolved(resource) {
		return false
	}
	for _, dep := range rt.ResourceGroup.Resources[resource].Dependencies {
		if !rt.ResourceResolved(dep) {
			return false
		}
	}
	return true
}

func (rt *RuntimeResourceGroup) SetLatestResource(name string, resource *unstructured.Unstructured) {
	rt.ResolvedResources[name] = resource
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

func (rt *RuntimeResourceGroup) ResolveDynamicVariables() error {
	// Dynamic variables are those that depend on other resources
	// and are resolved after all the dependencies are resolved.
	// let's iterate over any resolved resource and try to resolve
	// the dynamic variables that depend on it.

	resolvedResources := getMapKeys(rt.ResolvedResources)
	env, err := celutil.NewEnvironement(&celutil.EnvironementOptions{
		ResourceNames: resolvedResources,
	})
	if err != nil {
		return err
	}
	for _, variable := range rt.ExpressionsCache {
		if variable.Kind == ResourceVariableKindDynamic {
			// Skip the variable if it's already resolved
			if variable.Resolved {
				continue
			}

			// we need to make sure that the dependencies are
			// part of the resolved resources.
			if len(variable.Dependencies) > 0 && !elementsInSlice(variable.Dependencies, resolvedResources) {
				continue
			}
			ast, issues := env.Compile(variable.Expression)
			if issues != nil {
				return issues.Err()
			}
			program, err := env.Program(ast)
			if err != nil {
				return err
			}
			evalContext := make(map[string]interface{})
			for _, dep := range variable.Dependencies {
				evalContext[dep] = rt.ResolvedResources[dep].Object
			}
			val, _, err := program.Eval(evalContext)
			if err != nil {
				if strings.Contains(err.Error(), "no such key") {
					return &EvalError{
						IsIncompleteData: true,
						Err:              err,
					}
				}
				return &EvalError{
					Err: err,
				}
			}
			value, err := celutil.ConvertCELtoGo(val)
			if err != nil {
				return nil
			}
			variable.Resolved = true
			variable.ResolvedValue = value
		}
	}

	return nil
}

func (rt *RuntimeResourceGroup) ResolveInstanceStatus() error {
	rs := resolver.NewResolver(rt.Instance.Object, map[string]interface{}{})
	for _, v := range rt.ResourceGroup.Instance.Variables {
		/* canResolve := true
		for _, expr := range v.Expressions {
			if !runtimeResourceGroup.ExpressionsCache[expr].Resolved {
				canResolve = false
				break
			}
		} */

		e, ok := rt.ExpressionsCache[v.Expressions[0]]
		if ok && e.Resolved {
			err := rs.BlindSetValueAtPath(v.Path, rt.ExpressionsCache[v.Expressions[0]].ResolvedValue)
			if err != nil {
				return fmt.Errorf("failed to set value at path %s: %w", v.Path, err)
			}
		}
	}
	return nil
}

func (rt *RuntimeResourceGroup) ResolveResource(resource string) error {
	exprValues := make(map[string]interface{})
	for _, v := range rt.ExpressionsCache {
		if v.Resolved {
			exprValues[v.Expression] = v.ResolvedValue
		}
	}

	rs := resolver.NewResolver(rt.Resources[resource].Object, exprValues)
	exprFields := make([]parser.CELField, len(rt.ResourceGroup.Resources[resource].Variables))
	for i, v := range rt.ResourceGroup.Resources[resource].Variables {
		exprFields[i] = v.CELField
	}
	summary := rs.Resolve(exprFields)
	if summary.Errors != nil {
		return fmt.Errorf("failed to resolve resource %s: %v", resource, summary.Errors)
	}
	return nil
}

func inSlice[K comparable](element K, slice []K) bool {
	for _, e := range slice {
		if e == element {
			return true
		}
	}
	return false
}

func elementsInSlice[K comparable](elements []K, slice []K) bool {
	for _, element := range elements {
		if !inSlice(element, slice) {
			return false
		}
	}
	return true
}
