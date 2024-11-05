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

package graph

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/aws-controllers-k8s/symphony/internal/graph/dag"
	"github.com/aws-controllers-k8s/symphony/internal/runtime"
)

// Graph represents a processed resourcegroup. It contains the DAG representation
// and everything needed to "manage" the resources defined in the resource group.
type Graph struct {
	// DAG is the directed acyclic graph representation of the resource group.
	DAG *dag.DirectedAcyclicGraph
	// Instance is the processed resource group instance.
	Instance *Resource
	// Resources is a map of the processed resources in the resource group.
	Resources map[string]*Resource
	// TopologicalOrder is the topological order of the resources in the resource group.
	TopologicalOrder []string
}

// NewGraphRuntime creates a new runtime resource group from the resource group instance.
func (rg *Graph) NewGraphRuntime(newInstance *unstructured.Unstructured) (*runtime.ResourceGroupRuntime, error) {
	// we need to copy the resources to the runtime resources, mainly focusing
	// on the variables and dependencies.
	resources := make(map[string]runtime.Resource)
	for name, resource := range rg.Resources {
		resources[name] = resource.DeepCopy()
	}

	instance := rg.Instance.DeepCopy()
	instance.originalObject = newInstance
	rt, err := runtime.NewResourceGroupRuntime(instance, resources, rg.TopologicalOrder)
	if err != nil {
		return nil, err
	}
	return rt, nil
}
