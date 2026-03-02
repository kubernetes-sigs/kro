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
	"slices"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	krocel "github.com/kubernetes-sigs/kro/pkg/cel"
	"github.com/kubernetes-sigs/kro/pkg/graph/variable"
)

// Well-known node/variable identifiers used in CEL expressions.
const (
	// SchemaVarName is the variable name for accessing instance spec in CEL.
	SchemaVarName = "schema"
	// InstanceNodeID is the ID of the instance node (same as SchemaVarName since
	// that's how it's accessed in CEL expressions).
	InstanceNodeID = SchemaVarName
	// EachVarName is the variable name for collection item iteration in CEL.
	EachVarName = "each"
)

// often used field paths in resource templates.
const (
	// MetadataNamePath is the path to the resource name field.
	MetadataNamePath = "metadata.name"
	// MetadataNamespacePath is the path to the resource namespace field.
	MetadataNamespacePath = "metadata.namespace"
	// ReservedStatusFieldState is the status field reserved by the instance
	// controller for lifecycle state tracking.
	ReservedStatusFieldState = "state"
	// ReservedStatusFieldConditions is the status field reserved by the
	// instance controller for condition reporting.
	ReservedStatusFieldConditions = "conditions"
)

// NodeType identifies the kind of node in the resource graph.
// This is set by the builder based on the resource definition.
type NodeType int

const (
	// NodeTypeResource is a regular managed resource.
	NodeTypeResource NodeType = iota
	// NodeTypeCollection is a forEach collection that expands into multiple resources.
	NodeTypeCollection
	// NodeTypeExternal is an external reference (read-only, not applied).
	NodeTypeExternal
	// NodeTypeInstance is the instance node (ID: "instance").
	NodeTypeInstance
)

// String returns a human-readable string for the node type.
func (t NodeType) String() string {
	switch t {
	case NodeTypeResource:
		return "Resource"
	case NodeTypeCollection:
		return "Collection"
	case NodeTypeExternal:
		return "External"
	case NodeTypeInstance:
		return "Instance"
	default:
		return "Unknown"
	}
}

// NodeMeta contains immutable metadata about a node.
// This is grouped separately for clarity and to distinguish
// identification data from behavioral data.
type NodeMeta struct {
	// ID is the unique identifier of the node within the graph.
	ID string
	// Index is the position of this node in the original RGD resource list.
	// Used to preserve user-defined ordering when building the dependency graph.
	Index int
	// Type identifies the kind of node (Resource, Collection, External, Instance).
	Type NodeType
	// GVR is the GroupVersionResource for this node's resources.
	GVR schema.GroupVersionResource
	// Namespaced indicates if the resource is namespace-scoped.
	Namespaced bool
	// Dependencies lists the IDs of nodes this node depends on.
	Dependencies []string
}

// ForEachDimension represents a parsed forEach dimension from an RGD resource.
type ForEachDimension struct {
	// Name is the iterator variable name (e.g., "region")
	Name string
	// Expression is the compiled CEL expression that returns a list (e.g., "schema.spec.regions")
	Expression *krocel.Expression
}

// Node is the immutable node spec produced by the builder.
// It contains the template, variables, and conditions for a resource.
// No CRD/schema references are kept here - schemas are only used
// during build for CEL type-checking.
type Node struct {
	// Meta contains identification metadata (ID, Type, GVR, etc.)
	Meta NodeMeta

	// Template is the resource template with CEL expressions.
	// This is the original object from the RGD with expressions intact.
	Template *unstructured.Unstructured

	// Variables holds the CEL expression fields found in the template.
	Variables []*variable.ResourceField

	// IncludeWhen are compiled CEL expressions that must all evaluate to true
	// for this resource to be included. Empty means always include.
	IncludeWhen []*krocel.Expression

	// ReadyWhen are compiled CEL expressions that must all evaluate to true
	// for this resource to be considered ready.
	ReadyWhen []*krocel.Expression

	// ForEach holds the forEach dimensions for collection resources.
	// nil or empty means this is not a collection.
	ForEach []ForEachDimension
}

// DeepCopy creates a deep copy of the Node.
// Use this when runtime needs a per-runtime clone to avoid shared slices/maps.
func (n *Node) DeepCopy() *Node {
	if n == nil {
		return nil
	}

	cp := &Node{
		Meta: NodeMeta{
			ID:           n.Meta.ID,
			Index:        n.Meta.Index,
			Type:         n.Meta.Type,
			GVR:          n.Meta.GVR,
			Namespaced:   n.Meta.Namespaced,
			Dependencies: slices.Clone(n.Meta.Dependencies),
		},
		IncludeWhen: slices.Clone(n.IncludeWhen),
		ReadyWhen:   slices.Clone(n.ReadyWhen),
		ForEach:     slices.Clone(n.ForEach),
	}

	if n.Template != nil {
		cp.Template = n.Template.DeepCopy()
	}

	if n.Variables != nil {
		cp.Variables = make([]*variable.ResourceField, len(n.Variables))
		for i, v := range n.Variables {
			copyVar := *v
			copyVar.Expressions = slices.Clone(v.Expressions)
			cp.Variables[i] = &copyVar
		}
	}

	return cp
}
