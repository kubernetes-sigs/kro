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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/aws-controllers-k8s/symphony/internal/dag"
)

type ResourceGroup struct {
	Dag *dag.DirectedAcyclicGraph

	Instance *Resource

	Resources map[string]*Resource

	RuntimeVariables map[string][]*RuntimeVariable
}

func (rg *ResourceGroup) DeepCopyRuntimeVariables() map[string][]*RuntimeVariable {
	newMap := make(map[string][]*RuntimeVariable)
	for k, v := range rg.RuntimeVariables {
		newMap[k] = make([]*RuntimeVariable, len(v))
		for i, variable := range v {
			newMap[k][i] = &RuntimeVariable{
				Expression:   variable.Expression,
				Dependencies: variable.Dependencies,
				Kind:         variable.Kind,
				Resolved:     variable.Resolved,
			}
		}
	}
	return newMap
}

func (rg *ResourceGroup) NewRuntime(instance *unstructured.Unstructured) (*RuntimeResourceGroup, error) {
	unstructuredResources := make(map[string]*unstructured.Unstructured)
	for name, resource := range rg.Resources {
		u := &unstructured.Unstructured{
			Object: resource.OriginalObject,
		}
		unstructuredResources[name] = u.DeepCopy()
	}

	order, err := rg.Dag.TopologicalSort()
	if err != nil {
		return nil, err
	}

	runtimeVariables := rg.DeepCopyRuntimeVariables()

	expressionsCache := make(map[string]*RuntimeVariable)
	for _, resourceRuntimeVariables := range runtimeVariables {
		for _, variable := range resourceRuntimeVariables {
			expressionsCache[variable.Expression] = variable
		}
	}

	// replace runtimeVariable pointers with the the unique expressionsCache
	// This will ensure that we have a single source of truth for the runtime variables
	// and their values.
	for _, resourceRuntimeVariables := range runtimeVariables {
		for i, variable := range resourceRuntimeVariables {
			v, ok := expressionsCache[variable.Expression]
			if !ok {
				panic("expression not found in cache")
			}

			resourceRuntimeVariables[i] = v
		}
	}
	return &RuntimeResourceGroup{
		TopologicalOrder:  order,
		Instance:          instance.DeepCopy(),
		Resources:         unstructuredResources,
		ResourceGroup:     rg,
		RuntimeVariables:  runtimeVariables,
		ExpressionsCache:  expressionsCache,
		ResolvedResources: make(map[string]*unstructured.Unstructured, len(unstructuredResources)),
	}, nil
}
