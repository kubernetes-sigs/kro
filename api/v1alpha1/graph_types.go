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
	// +listType=map
	// +listMapKey=id
	Nodes []Node `json:"nodes"`

	// ServiceAccountName, when set, causes kro to apply this Graph's resources
	// while impersonating the named ServiceAccount instead of using the kro
	// controller's own identity. The ServiceAccount is always resolved in the
	// Graph's own namespace (system:serviceaccount:<graph-namespace>:<name>), so
	// a Graph can never escalate beyond the RBAC granted to a ServiceAccount in
	// its own namespace. When empty, kro impersonates the default ServiceAccount
	// of the Graph's namespace, confining resource access to that namespace by
	// default.
	//
	// The kro controller ServiceAccount must be granted the "impersonate" verb
	// on serviceaccounts for this to take effect.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	// +kubebuilder:validation:MaxLength=253
	ServiceAccountName string `json:"serviceAccountName,omitempty"`
}

// GraphStatus defines the observed state of a Graph.
type GraphStatus struct {
	// Conditions represent the latest available observations of the Graph's
	// state.
	Conditions Conditions `json:"conditions,omitempty"`

	// ManagedResources is the authoritative list of cluster resources this
	// Graph has applied. Entries are recorded in topological apply order so
	// reverse iteration gives reverse-apply order on delete + prune. Status
	// is persisted after reconciliation: on a fully-successful apply and
	// prune, it reflects the currently-applied set; on errors, it preserves
	// the union of previously-known and newly-applied resources.
	//
	// MaxItems bounds the inventory so a runaway forEach expansion cannot push
	// the Graph object past etcd's object-size limit (~1.5Mi) — which would fail
	// the status write and, because teardown reads this list, jeopardize cleanup.
	// The practical limiter is the per-node forEach cap
	// (runtime.DefaultMaxCollectionSize, default 1000); this ceiling is set well
	// above any realistic aggregate (each entry is a few short strings + a UID,
	// so 5000 entries stays comfortably under the etcd limit even during the
	// write-ahead phase, which transiently holds previous ∪ next).
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxItems=5000
	ManagedResources []ManagedResource `json:"managedResources,omitempty"`

	// AppliedServiceAccount is the impersonation username
	// (system:serviceaccount:<namespace>:<name>) the Graph last applied its
	// resources under. Teardown resolves the executor from THIS identity rather
	// than the current spec.serviceAccountName, so editing that field between
	// apply and delete cannot strand resources under an identity that can no
	// longer see them. Empty for a Graph that has never applied (or one last
	// applied by a kro version predating this field), in which case teardown
	// falls back to the current spec.
	//
	// +kubebuilder:validation:Optional
	AppliedServiceAccount string `json:"appliedServiceAccount,omitempty"`

	// Contributions is the authoritative release inventory for this Graph's
	// patch nodes: each entry records a field-manager contribution the Graph
	// applied to a resource it does not own. Because a patch never owns its
	// target, teardown/prune cannot rediscover these fields from ownership —
	// the inventory is what lets Release relinquish exactly the fields the
	// Graph contributed. The controller write-aheads the intended set BEFORE
	// apply and rewrites it with the observed set AFTER a clean apply, so a
	// crash in that window still leaves teardown a superset to release from.
	//
	// Persisted on the status subresource (not a metadata annotation) so it is
	// RBAC-separable: a principal with only spec/metadata edit rights cannot
	// forge the release inventory.
	//
	// +kubebuilder:validation:Optional
	Contributions []Contribution `json:"contributions,omitempty"`
}

// Contribution is a lightweight record of a patch node's field-manager
// contribution to a resource this Graph does not own. It mirrors the
// in-memory executor contribution: the tuple identifies the target
// (APIVersion, Kind, Namespace, Name, Subresource) and FieldManager is the
// dedicated server-side-apply manager the contributed fields were applied
// under. On prune/teardown the manager relinquishes exactly those fields;
// the target object is never deleted.
type Contribution struct {
	// APIVersion of the patched target ("apps/v1", "v1", ...).
	//
	// +kubebuilder:validation:Required
	APIVersion string `json:"apiVersion"`

	// Kind of the patched target ("Deployment", "ConfigMap", ...).
	//
	// +kubebuilder:validation:Required
	Kind string `json:"kind"`

	// Namespace of the patched target. Empty for cluster-scoped targets.
	//
	// +kubebuilder:validation:Optional
	Namespace string `json:"namespace,omitempty"`

	// Name of the patched target.
	//
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Subresource the contribution was applied through ("status" for a
	// status patch, empty for the main resource).
	//
	// +kubebuilder:validation:Optional
	Subresource string `json:"subresource,omitempty"`

	// FieldManager is the dedicated server-side-apply field manager the
	// contributed fields were applied under. Release relinquishes the fields
	// owned by this manager on the target.
	//
	// +kubebuilder:validation:Required
	FieldManager string `json:"fieldManager"`
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
	// deleted-and-recreated out of band between apply and prune.
	//
	// A UID-less entry is a not-yet-observed intent (e.g. a pre-apply
	// write-ahead entry declared but not yet applied this cycle). Such an
	// entry is intentionally SKIPPED on delete/prune: without a captured UID
	// kro cannot prove the live object is the one it applied, and deleting by
	// name alone could remove an object kro does not own. It is therefore
	// effectively required for cleanup — an entry only becomes deletable once
	// a successful apply has recorded its UID.
	//
	// +kubebuilder:validation:Optional
	UID string `json:"uid,omitempty"`
}

// Node is a single composable unit within a Graph. Each Node carries exactly
// one of the type-discriminating fields (Template, Ref, Def, Patch, Graph)
// which determines its behavior. Node IDs are the handles other nodes use to
// reference it via CEL expressions.
//
// +kubebuilder:validation:XValidation:rule="[has(self.template), has(self.ref), has(self.def), has(self.graph), has(self.patch)].exists_one(x, x)",message="exactly one of template, ref, def, graph, patch must be set"
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

	// Patch contributes fields to a resource this node does not own,
	// authored as a raw partial manifest exactly like Template (apiVersion,
	// kind, metadata.name required, metadata.namespace optional, plus the
	// contributed fields). The target identified by apiVersion, kind, and
	// metadata.name (+ namespace) must already exist; the node applies the
	// contributed fields under a dedicated field manager (server-side apply)
	// without taking ownership of the whole object. On prune the
	// contributed fields are released — the field manager relinquishes them
	// — but the target object is never deleted.
	//
	// The target subresource is derived from field presence rather than
	// declared explicitly: a top-level `status` key routes the apply
	// through the status subresource, while any other top-level key (or any
	// metadata field beyond name/namespace) routes to the main resource. A
	// single patch node may not mix status fields with main-resource
	// fields, and it must contribute at least one field beyond identity.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:pruning:PreserveUnknownFields
	Patch *runtime.RawExtension `json:"patch,omitempty"`

	// ReadyWhen is a list of CEL expressions that must all evaluate to
	// true for this node to be considered ready. Evaluated against scope
	// after the node has been applied and its value published, so
	// expressions typically reference the node's own published state
	// (e.g. `cluster.status.phase == 'Active'`). Empty means the node is
	// ready as soon as it is applied. For collection nodes (forEach) each
	// expression is evaluated once per item with `each` bound to that item,
	// and the node is ready only when every item satisfies every expression
	// (use `each`, not an aggregate over the node's own name):
	//     readyWhen: [ "${each.status.phase == 'Running'}" ]
	//
	// +kubebuilder:validation:Optional
	ReadyWhen []string `json:"readyWhen,omitempty"`

	// IncludeWhen is a list of CEL expressions that must all evaluate to
	// true for this node to be applied. Evaluated against scope before
	// apply, so expressions may reference upstream nodes. When any is
	// false the node is skipped entirely — no resolve, no apply, no scope
	// publication. The skip is contagious: nodes depending on a skipped
	// node are themselves skipped (not evaluated, no error), so a disabled
	// branch prunes cleanly instead of breaking its dependents.
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

	// +kubebuilder:validation:Required
	Spec   GraphSpec   `json:"spec"`
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
