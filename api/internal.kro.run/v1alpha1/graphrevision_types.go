// Copyright 2025 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package v1alpha1

import (
	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GraphRevisionSpec defines the desired state of GraphRevision.
// It captures an immutable snapshot of the source ResourceGraphDefinition.
type GraphRevisionSpec struct {
	// Revision is a monotonic revision number assigned per ResourceGraphDefinition name.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=1
	Revision int64 `json:"revision"`
	// Snapshot is an immutable capture of the source ResourceGraphDefinition
	// identity and spec at the time this revision was issued.
	//
	// +kubebuilder:validation:Required
	Snapshot ResourceGraphDefinitionSnapshot `json:"snapshot"`
}

// ResourceGraphDefinitionSnapshot captures the identity and spec of a
// ResourceGraphDefinition at a point in time.
type ResourceGraphDefinitionSnapshot struct {
	// Name identifies the source ResourceGraphDefinition by name.
	// This is the authoritative identity for adoption decisions.
	//
	// +kubebuilder:validation:Required
	Name string `json:"name"`
	// Generation is the metadata.generation of the source ResourceGraphDefinition
	// when this revision was issued. Informational only.
	//
	// +kubebuilder:validation:Required
	Generation int64 `json:"generation"`
	// Spec is an immutable copy of the source ResourceGraphDefinition spec.
	// This includes user-authored schema and resource templates.
	//
	// +kubebuilder:validation:Required
	Spec krov1alpha1.ResourceGraphDefinitionSpec `json:"spec"`
}

// GraphRevisionStatus defines the observed API state of GraphRevision.
// Internal runtime states such as Pending, Active, and Failed live in
// kro's in-memory revision registry and are not persisted here.
type GraphRevisionStatus struct {
	// TopologicalOrder is the ordered list of resource IDs based on dependencies.
	TopologicalOrder []string `json:"topologicalOrder,omitempty"`
	// Conditions represent the latest available observations of the GraphRevision state.
	// GraphRevision exposes GraphVerified plus aggregate Ready.
	Conditions krov1alpha1.Conditions `json:"conditions,omitempty"`
	// Resources provides detailed information about each resource in the graph.
	Resources []krov1alpha1.ResourceInformation `json:"resources,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="RGD",type=string,priority=0,JSONPath=`.spec.snapshot.name`
// +kubebuilder:printcolumn:name="REVISION",type=integer,priority=0,JSONPath=`.spec.revision`
// +kubebuilder:printcolumn:name="HASH",type=string,priority=1,JSONPath=`.metadata.labels.kro\.run/graph-revision-hash`
// +kubebuilder:printcolumn:name="READY",type=string,priority=0,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="AGE",type="date",priority=0,JSONPath=".metadata.creationTimestamp"
// +kubebuilder:resource:shortName=gr,scope=Cluster

// GraphRevision is an immutable snapshot of a ResourceGraphDefinition revision.
type GraphRevision struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable"
	Spec   GraphRevisionSpec   `json:"spec,omitempty"`
	Status GraphRevisionStatus `json:"status,omitempty"`
}

// GetConditions returns the GraphRevision's status conditions.
func (o *GraphRevision) GetConditions() []krov1alpha1.Condition {
	return o.Status.Conditions
}

// SetConditions replaces the GraphRevision's status conditions.
func (o *GraphRevision) SetConditions(conditions []krov1alpha1.Condition) {
	o.Status.Conditions = conditions
}

// +kubebuilder:object:root=true

// GraphRevisionList contains a list of GraphRevision.
type GraphRevisionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GraphRevision `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GraphRevision{}, &GraphRevisionList{})
}
