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
	"fmt"

	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/kube-openapi/pkg/validation/spec"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/graph/dag"
)

const (
	// SchemaVarName is the reserved variable name for the instance schema root.
	SchemaVarName = "schema"
	// EachVarName is the reserved variable name used inside collection readyWhen.
	EachVarName = "each"

	// MetadataNamePath is the canonical template path for metadata.name.
	MetadataNamePath = "metadata.name"
	// MetadataNamespacePath is the canonical template path for metadata.namespace.
	MetadataNamespacePath = "metadata.namespace"
)

// FieldKind classifies how a template field is evaluated at runtime.
type FieldKind int

const (
	FieldStatic FieldKind = iota
	FieldDynamic
	FieldIteration
)

func (k FieldKind) String() string {
	switch k {
	case FieldStatic:
		return "Static"
	case FieldDynamic:
		return "Dynamic"
	case FieldIteration:
		return "Iteration"
	default:
		return fmt.Sprintf("FieldKind(%d)", int(k))
	}
}

// NodeType describes how a node participates in graph execution.
type NodeType int

const (
	NodeTypeScalar     NodeType = iota // one template -> one instance
	NodeTypeCollection                 // one template + forEach -> N instances
	NodeTypeExternal                   // observe, don't create
	NodeTypeInstance                   // the instance node itself
)

func (t NodeType) String() string {
	switch t {
	case NodeTypeScalar:
		return "Scalar"
	case NodeTypeCollection:
		return "Collection"
	case NodeTypeExternal:
		return "External"
	case NodeTypeInstance:
		return "Instance"
	default:
		return fmt.Sprintf("NodeType(%d)", int(t))
	}
}

// NodeMeta is the common metadata extracted from a resource template
// during parsing. Carried through all subsequent stages.
type NodeMeta struct {
	ID          string
	Index       int
	Type        NodeType
	Template    map[string]interface{}
	ExternalRef *v1alpha1.ExternalRef
}

// InstanceMeta is the common metadata extracted from the instance schema
// during parsing. Carried through all subsequent stages.
type InstanceMeta struct {
	Group, APIVersion, Kind string
	SpecSchema              *extv1.JSONSchemaProps
	CustomTypes             map[string]interface{}
	StatusTemplate          map[string]interface{}
	PrinterColumns          []extv1.CustomResourceColumnDefinition
	Metadata                *v1alpha1.CRDMetadata
}

// NodeIdentity is all non-expression data after resolution.
// Shared base for LinkedNode and CompiledNode, where expression
// types change but identity remains the same.
type NodeIdentity struct {
	NodeMeta
	GVK        schema.GroupVersionKind
	GVR        schema.GroupVersionResource
	Namespaced bool
	Schema     *spec.Schema
}

// IdentityFrom creates a NodeIdentity from a ResolvedNode.
func IdentityFrom(r *ResolvedNode) NodeIdentity {
	return NodeIdentity{
		NodeMeta:   r.NodeMeta,
		GVK:        r.GVK,
		GVR:        r.GVR,
		Namespaced: r.Namespaced,
		Schema:     r.Schema,
	}
}

// Graph is the final output of the build pipeline.
type Graph struct {
	Instance         *CompiledNode
	Nodes            map[string]*CompiledNode
	DAG              *dag.DirectedAcyclicGraph[string]
	TopologicalOrder []string
	CRD              *extv1.CustomResourceDefinition
}
