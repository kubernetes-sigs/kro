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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// Graph condition types.
const (
	// GraphConditionTypeReady is true when the graph has converged on its
	// desired state: every node has been reconciled and reports ready.
	GraphConditionTypeReady ConditionType = "Ready"
	// GraphConditionTypeAccepted is true when the graph spec passes
	// validation (unique node IDs, well-formed expressions, dependency
	// graph is acyclic). False with reason "InvalidGraph" otherwise.
	GraphConditionTypeAccepted ConditionType = "Accepted"
)

// GraphSpec defines the desired state of a Graph.
type GraphSpec struct {
	// Nodes is the unordered list of nodes that make up this Graph. Evaluation
	// order is derived from inter-node CEL references, not from list order.
	// Node IDs must be unique within the Graph.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	Nodes []Node `json:"nodes"`
}

// GraphStatus defines the observed state of a Graph.
type GraphStatus struct {
	// Conditions represent the latest available observations of the Graph's
	// state.
	Conditions Conditions `json:"conditions,omitempty"`

	// ManagedResources is the authoritative list of cluster resources this
	// Graph has applied. Entries are recorded in topological apply order so
	// reverse iteration gives reverse-apply order on delete + prune. Each
	// reconcile rewrites this list — before applying, it is the union of
	// the previous list and the soon-to-be-applied list (write-ahead);
	// after a fully-successful apply + prune, it shrinks to just the
	// currently-applied set.
	//
	// +kubebuilder:validation:Optional
	ManagedResources []ManagedResource `json:"managedResources,omitempty"`
}

// ManagedResource is a lightweight pointer to a cluster resource the Graph
// controller has applied. The tuple of (APIVersion, Kind, Namespace, Name)
// identifies the resource; UID is captured post-apply for safe deletion
// (preconditioned on UID so we never delete an impostor recreated by some
// other actor between apply and prune).
type ManagedResource struct {
	// NodeID is the Graph node that produced this resource. Multiple
	// resources may share a NodeID for forEach expansions.
	//
	// +kubebuilder:validation:Required
	NodeID string `json:"nodeID"`

	// APIVersion of the resource ("apps/v1", "v1", ...).
	//
	// +kubebuilder:validation:Required
	APIVersion string `json:"apiVersion"`

	// Kind of the resource ("Deployment", "ConfigMap", ...).
	//
	// +kubebuilder:validation:Required
	Kind string `json:"kind"`

	// Namespace of the resource. Empty for cluster-scoped resources.
	//
	// +kubebuilder:validation:Optional
	Namespace string `json:"namespace,omitempty"`

	// Name of the resource.
	//
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// UID returned by the API server when the resource was applied. Used
	// as a delete precondition so we don't remove a resource that was
	// deleted-and-recreated out of band between apply and prune. Absent
	// on entries that haven't been observed yet (e.g. union-state entries
	// declared but not yet applied this cycle).
	//
	// +kubebuilder:validation:Optional
	UID string `json:"uid,omitempty"`
}

// Node is a single composable unit within a Graph. Each Node carries exactly
// one of the type-discriminating fields (Template, Ref, Watch, Def, Graph)
// which determines its behavior. Node IDs are the handles other nodes use to
// reference it via CEL expressions.
//
// +kubebuilder:validation:XValidation:rule="[has(self.template), has(self.ref), has(self.watch), has(self.def), has(self.graph)].exists_one(x, x)",message="exactly one of template, ref, watch, def, graph must be set"
type Node struct {
	// ID is the handle that other nodes use to reference this node from CEL
	// expressions. Must be alphanumeric (case-insensitive) and unique within
	// the Graph.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[A-Za-z][A-Za-z0-9]*$`
	ID string `json:"id"`

	// Template declares that this node creates and manages a Kubernetes
	// resource. The controller applies the resource on create and on change,
	// and deletes it on prune.
	//
	// +kubebuilder:validation:Optional
	Template *runtime.RawExtension `json:"template,omitempty"`

	// Ref imports a resource that exists outside this Graph into scope. The
	// referenced resource is read-only; its fields become available to other
	// nodes through CEL expressions.
	//
	// +kubebuilder:validation:Optional
	Ref *ExternalRef `json:"ref,omitempty"`

	// Watch makes the collection of resources matching a label selector
	// available to other nodes through CEL expressions. The set is kept
	// up-to-date as resources come and go.
	//
	// +kubebuilder:validation:Optional
	Watch *WatchSpec `json:"watch,omitempty"`

	// Def introduces raw data into scope without reading or writing any
	// Kubernetes resource. The value is a free-form object whose fields may
	// contain CEL expressions.
	//
	// +kubebuilder:validation:Optional
	Def *runtime.RawExtension `json:"def,omitempty"`

	// Graph nests another Graph as a child scope under this node's ID. The
	// child's nodes form a lexical frame: they may reference this Graph's
	// nodes (capture) and shadow their names, but a single CEL expression may
	// not mix the two scopes — it references one frame or the other. The
	// child's node outputs are addressable under this node's ID, e.g.
	// `${nodeID.childNode.field}`. Nesting has no depth limit. The payload is
	// a GraphSpec (a `nodes:` list); it is parsed at compile time, so the CRD
	// stores it as an opaque object.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:pruning:PreserveUnknownFields
	Graph *runtime.RawExtension `json:"graph,omitempty"`

	// ReadyWhen is a list of CEL expressions that must all evaluate to
	// true for this node to be considered ready. Evaluated against scope
	// after the node has been applied and its value published, so
	// expressions typically reference the node's own published state
	// (e.g. `cluster.status.phase == 'Active'`). Empty means the node is
	// ready as soon as it is applied. For collection nodes (forEach or
	// watch) the node's value in scope is a list — use CEL list functions
	// like `all()` and `exists()`:
	//     readyWhen: [ "${appPods.all(p, p.status.phase == 'Running')}" ]
	//
	// +kubebuilder:validation:Optional
	ReadyWhen []string `json:"readyWhen,omitempty"`

	// IncludeWhen is a list of CEL expressions that must all evaluate to
	// true for this node to be applied. Evaluated against scope before
	// apply, so expressions may reference upstream nodes. When any
	// returns false the node is skipped entirely: it does not apply, it
	// does not publish to scope, and dependent expressions that try to
	// read it will error.
	//
	// +kubebuilder:validation:Optional
	IncludeWhen []string `json:"includeWhen,omitempty"`

	// ForEach expands this node into a collection. Each entry binds a
	// variable name to a CEL expression that evaluates to an array; the
	// controller produces one instance per element. Multiple dimensions form
	// the cartesian product of their bindings.
	//
	// +kubebuilder:validation:Optional
	ForEach []ForEachDimension `json:"forEach,omitempty"`
}

// WatchSpec selects a set of resources of a single GroupKind to make available
// in CEL scope.
type WatchSpec struct {
	// APIVersion of the watched resource.
	//
	// +kubebuilder:validation:Required
	APIVersion string `json:"apiVersion"`
	// Kind of the watched resource.
	//
	// +kubebuilder:validation:Required
	Kind string `json:"kind"`
	// Namespace to watch. Empty means watch across all namespaces.
	//
	// +kubebuilder:validation:Optional
	Namespace string `json:"namespace,omitempty"`
	// Selector is a label-equality selector. Values may contain CEL
	// expressions evaluated in the node's scope before matching.
	//
	// +kubebuilder:validation:Optional
	Selector map[string]string `json:"selector,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="READY",type=string,priority=0,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="AGE",type="date",priority=0,JSONPath=".metadata.creationTimestamp"
// +kubebuilder:resource:scope=Namespaced

// Graph is a scope of composable nodes that manage Kubernetes resources and
// the relationships between them. Nodes reference each other through CEL
// expressions; the controller derives an execution order from the implied
// dependencies.
type Graph struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GraphSpec   `json:"spec,omitempty"`
	Status GraphStatus `json:"status,omitempty"`
}

func (g *Graph) GetConditions() []Condition {
	return g.Status.Conditions
}

func (g *Graph) SetConditions(conditions []Condition) {
	g.Status.Conditions = conditions
}

// +kubebuilder:object:root=true

// GraphList contains a list of Graph.
type GraphList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Graph `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Graph{}, &GraphList{})
}
