// Copyright 2026 The Kubernetes Authors.
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

package compiler

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/kube-openapi/pkg/validation/spec"

	krocel "github.com/kubernetes-sigs/kro/pkg/graphengine/cel"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/compiler/dag"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/compiler/variable"
)

// EachVarName is the CEL identifier bound to the current element during
// collection iteration.
const EachVarName = "each"

// NodeKind discriminates the four Graph node types.
type NodeKind int

const (
	NodeKindTemplate NodeKind = iota
	NodeKindRef
	NodeKindWatch
	NodeKindDef
	NodeKindGraph
)

// String returns the lowercase keyword for the node kind, matching the API.
func (k NodeKind) String() string {
	switch k {
	case NodeKindTemplate:
		return "template"
	case NodeKindRef:
		return "ref"
	case NodeKindWatch:
		return "watch"
	case NodeKindDef:
		return "def"
	case NodeKindGraph:
		return "graph"
	default:
		return "unknown"
	}
}

// ForEachDimension is the compiled form of an API forEach axis.
type ForEachDimension struct {
	Name       string
	Expression *krocel.Expression
}

// Node is the immutable compiled form of a v1alpha1.Node.
// Dependencies are populated after CEL inspection has run; Variables and
// ForEach expressions are compiled by the time the Node is exposed.
type Node struct {
	// ID is the node handle, unique within a Program.
	ID string
	// Index is the node's position in the source Graph (used for stable
	// ordering when two nodes are otherwise unordered by dependency).
	Index int
	// Kind discriminates the node type.
	Kind NodeKind

	// GVR is the target GroupVersionResource. Set for Template/Patch/Ref/Watch
	// with a literal apiVersion+kind; zero-valued for Def and for dynamic-GVK
	// templates (the GVK isn't known until reconcile-time evaluation).
	GVR schema.GroupVersionResource
	// Namespaced records the REST scope of GVR. Always false for Def and for
	// dynamic-GVK templates (scope is resolved at apply time instead).
	Namespaced bool

	// DynamicGVK is true when this node's apiVersion or kind is a CEL
	// expression, so the target GVK — and therefore the schema, GVR, and
	// REST scope — can only be resolved once scope is evaluated at
	// reconcile time. Dynamic nodes are parsed schemaless (no compile-time
	// type-checking against the target schema; cross-node references are
	// still checked) and their GVR/Namespaced are resolved per rendered
	// object by the executor.
	DynamicGVK bool

	// Object is the parsed payload as an unstructured object:
	//   Template/Patch: the user-authored manifest
	//   Ref:            the ExternalRef projected as {apiVersion, kind, metadata}
	//   Watch:          the WatchSpec projected as {apiVersion, kind, namespace?, selector}
	//   Def:            the raw def map
	Object *unstructured.Unstructured

	// Variables holds every CEL expression found inside Object, keyed by path.
	// Programs are compiled.
	Variables []*variable.ResourceField

	// Dependencies are the IDs of other nodes referenced from Variables,
	// ForEach axes, IncludeWhen, or ReadyWhen. Self-references are filtered.
	Dependencies []string

	// ForEach is the cartesian-product expansion axes. Empty when the node
	// is not a collection.
	ForEach []ForEachDimension

	// IncludeWhen are compiled CEL conditions that all must evaluate true
	// for the executor to apply this node. Empty means always included.
	IncludeWhen []*krocel.Expression

	// ReadyWhen are compiled CEL conditions that all must evaluate true
	// for the node to be considered ready after apply. Empty means the
	// node is ready immediately after the apply call returns.
	ReadyWhen []*krocel.Expression

	// SubProgram is the compiled child Graph for a NodeKindGraph node, and
	// nil for every other kind. The child is its own lexical frame: it runs
	// with a scope seeded from this Graph's scope (capture + shadowing), and
	// its node outputs are published to the parent scope as a map under this
	// node's ID. Dependencies on the parent frame (captured names) are
	// recorded on this node so the executor runs the subgraph after the
	// captured parent nodes resolve.
	SubProgram *Program
}

// IsCollection reports whether this node expands into multiple instances.
func (n *Node) IsCollection() bool {
	return len(n.ForEach) > 0 || (n.Kind == NodeKindWatch)
}

// Program is the compiled IR for a v1alpha1.Graph. It carries the dependency
// DAG, the per-node compiled spec, and the OpenAPI schemas each non-Def node
// publishes to CEL scope (Def nodes are schemaless / dyn).
type Program struct {
	// DAG captures the dependency relationships between nodes.
	DAG *dag.DirectedAcyclicGraph[string]
	// Nodes maps node ID to its compiled form.
	Nodes map[string]*Node
	// TopologicalOrder is the order in which nodes can be processed safely.
	TopologicalOrder []string
	// NodeSchemas maps node ID to the OpenAPI schema that node publishes to
	// CEL scope (collection nodes have this wrapped as a list). Def nodes are
	// absent.
	NodeSchemas map[string]*spec.Schema

	// RequiredGroupKinds is the deduplicated set of Kubernetes GroupKinds the
	// Graph statically references. Templates with literal apiVersion+kind
	// contribute; Ref and Watch nodes contribute their target GroupKind;
	// Def nodes contribute nothing. The SchemaWatcher uses this to know
	// which Graphs must be re-enqueued when a particular CRD changes.
	//
	// Templates whose apiVersion or kind is a CEL expression (dynamic
	// GVK) do NOT contribute here — they're captured via HasDynamicGVK
	// instead, which makes the Graph subscribe to all CRD changes.
	RequiredGroupKinds []schema.GroupKind

	// HasDynamicGVK is true when any node in the Graph has a CEL
	// expression in apiVersion or kind. Such Graphs cannot have their
	// schema dependencies known until reconcile-time evaluation, so
	// they subscribe to all CRD changes via the SchemaWatcher's
	// "dynamic" set.
	HasDynamicGVK bool
}
