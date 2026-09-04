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

package graph

import (
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/kube-openapi/pkg/validation/spec"

	"github.com/kubernetes-sigs/kro/pkg/dag"
)

// Graph represents a processed ResourceGraphDefinition.
// It contains the DAG and immutable node specs produced by the builder.
type Graph struct {
	// DAG is the directed acyclic graph of node dependencies.
	DAG *dag.DirectedAcyclicGraph[string]

	// Instance is the instance node (the generated CRD instance).
	Instance *Node

	// Nodes maps node ID to immutable node spec.
	Nodes map[string]*Node

	// Resources is an alias for Nodes kept for backward compatibility in tests and tooling.
	Resources map[string]*Node

	// TopologicalOrder is the sorted order of node IDs for processing.
	// This excludes the instance node.
	TopologicalOrder []string

	// ApplyOrders maps node IDs to their one-based reverse topological layer.
	// Nodes in the same layer can be deleted in the same wave.
	ApplyOrders map[string]int

	// CRD is the generated CustomResourceDefinition for the instance.
	CRD *extv1.CustomResourceDefinition

	// ResourceSchemas maps node ID to the OpenAPI schema for that resource.
	// Includes resource schemas (keyed by resource ID) and the instance schema
	// (keyed by InstanceNodeID). Used at runtime for schema-aware CEL value conversion.
	ResourceSchemas map[string]*spec.Schema
}

func applyOrdersForDAG(
	dependencyGraph *dag.DirectedAcyclicGraph[string],
) (map[string]int, error) {
	layers, err := dependencyGraph.ReverseTopologicalLayers()
	if err != nil {
		return nil, err
	}

	orders := make(map[string]int, len(dependencyGraph.Vertices))
	for i, layer := range layers {
		order := len(layers) - i
		for _, nodeID := range layer {
			orders[nodeID] = order
		}
	}
	return orders, nil
}
