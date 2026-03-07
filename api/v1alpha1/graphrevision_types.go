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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// GraphRevisionSpec defines the desired state of GraphRevision.
// It captures an immutable snapshot of the source ResourceGraphDefinition spec.
type GraphRevisionSpec struct {
	// ResourceGraphDefinitionName identifies the source ResourceGraphDefinition by name.
	// This field is the authoritative identity for matching/adoption decisions.
	//
	// +kubebuilder:validation:Required
	ResourceGraphDefinitionName string `json:"resourceGraphDefinitionName"`
	// ResourceGraphDefinitionUID is the source ResourceGraphDefinition UID at creation time.
	// It is informational only and must not be used as the primary ownership key.
	//
	// +kubebuilder:validation:Optional
	ResourceGraphDefinitionUID types.UID `json:"resourceGraphDefinitionUID,omitempty"`
	// Revision is a monotonic revision number assigned per ResourceGraphDefinition name.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=1
	Revision int64 `json:"revision"`
	// SpecHash is the canonical hash of the source ResourceGraphDefinition spec.
	//
	// +kubebuilder:validation:Required
	SpecHash string `json:"specHash"`
	// DefinitionSpec is an immutable snapshot of the source ResourceGraphDefinition spec.
	// This includes user-authored schema and resource templates.
	//
	// +kubebuilder:validation:Required
	DefinitionSpec ResourceGraphDefinitionSpec `json:"definitionSpec"`
}

// GraphRevisionStatus defines the observed state of GraphRevision.
type GraphRevisionStatus struct {
	// TopologicalOrder is the ordered list of resource IDs based on dependencies.
	TopologicalOrder []string `json:"topologicalOrder,omitempty"`
	// Conditions represent the latest available observations of the GraphRevision state.
	Conditions Conditions `json:"conditions,omitempty"`
	// Resources provides detailed information about each resource in the graph.
	Resources []ResourceInformation `json:"resources,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="RGD",type=string,priority=1,JSONPath=`.spec.resourceGraphDefinitionName`
// +kubebuilder:printcolumn:name="REVISION",type=integer,priority=0,JSONPath=`.spec.revision`
// +kubebuilder:printcolumn:name="HASH",type=string,priority=1,JSONPath=`.spec.specHash`
// +kubebuilder:printcolumn:name="READY",type=string,priority=0,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="AGE",type="date",priority=0,JSONPath=".metadata.creationTimestamp"
// +kubebuilder:resource:shortName=gv,scope=Cluster

// GraphRevision is an immutable snapshot of a ResourceGraphDefinition revision.
type GraphRevision struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable"
	Spec   GraphRevisionSpec   `json:"spec,omitempty"`
	Status GraphRevisionStatus `json:"status,omitempty"`
}

func (o *GraphRevision) GetConditions() []Condition {
	return o.Status.Conditions
}

func (o *GraphRevision) SetConditions(conditions []Condition) {
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
