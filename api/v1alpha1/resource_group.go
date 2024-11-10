// Copyright Amazon.com Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//     http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	runtime "k8s.io/apimachinery/pkg/runtime"
)

const (
	// DefaultServiceAccountKey is the key to use for the default service account
	// in the serviceAccounts map.
	DefaultServiceAccountKey = "*"
)

// ResourceGroupSpec defines the desired state of ResourceGroup
type ResourceGroupSpec struct {
	// The schema of the resourcegroup, which includes the
	// apiVersion, kind, spec, status, types, and some validation
	// rules.
	//
	// +kubebuilder:validation:Required
	Schema *Schema `json:"schema,omitempty"`
	// The resources that are part of the resourcegroup.
	//
	// +kubebuilder:validation:Optional
	Resources []*Resource `json:"resources,omitempty"`
	// ServiceAccount configuration for controller impersonation.
	// Key is the namespace, value is the service account name to use.
	// Special key "*" defines the default service account for any
	// namespace not explicitly mapped.
	//
	// +kubebuilder:validation:Optional
	DefaultServiceAccounts map[string]string `json:"defaultServiceAccounts,omitempty"`
}

// Schema represents the attributes that define an instance of
// a resourcegroup.
type Schema struct {
	// The kind of the resourcegroup. This is used to generate
	// and create the CRD for the resourcegroup.
	//
	// +kubebuilder:validation:Required
	Kind string `json:"kind,omitempty"`
	// The APIVersion of the resourcegroup. This is used to generate
	// and create the CRD for the resourcegroup.
	//
	// +kubebuilder:validation:Required
	APIVersion string `json:"apiVersion,omitempty"`
	// The spec of the resourcegroup. Typically, this is the spec of
	// the CRD that the resourcegroup is managing. This is adhering
	// to the SimpleSchema spec
	Spec runtime.RawExtension `json:"spec,omitempty"`
	// The status of the resourcegroup. This is the status of the CRD
	// that the resourcegroup is managing. This is adhering to the
	// SimpleSchema spec.
	Status runtime.RawExtension `json:"status,omitempty"`
	// Validation is a list of validation rules that are applied to the
	// resourcegroup.
	// Not implemented yet.
	Validation []string `json:"validation,omitempty"`
}

type Validation struct {
	Expression string `json:"expression,omitempty"`
	Message    string `json:"message,omitempty"`
}

type Resource struct {
	// +kubebuilder:validation:Required
	Name string `json:"name,omitempty"`
	// +kubebuilder:validation:Required
	Template runtime.RawExtension `json:"template,omitempty"`
	// +kubebuilder:validation:Optional
	ReadyWhen []string `json:"readyWhen,omitempty"`
	// +kubebuilder:validation:Optional
	IncludeWhen []string `json:"includeWhen,omitempty"`
}

// ResourceGroupStatus defines the observed state of ResourceGroup
type ResourceGroupStatus struct {
	// State is the state of the resourcegroup
	State ResourceGroupState `json:"state,omitempty"`
	// TopologicalOrder is the topological order of the resourcegroup graph
	TopologicalOrder []string `json:"topologicalOrder,omitempty"`
	// Conditions represent the latest available observations of an object's state
	Conditions []Condition `json:"conditions,omitempty"`
	// Resources represents the resources, and their information (dependencies for now)
	Resources []ResourceInformation `json:"resources,omitempty"`
}

// ResourceInformation defines the information about a resource
// in the resourcegroup
type ResourceInformation struct {
	// Name represents the name of the resources we're providing information for
	Name string `json:"name,omitempty"`
	// Dependencies represents the resource dependencies of a resource group
	Dependencies []Dependency `json:"dependencies,omitempty"`
}

// Dependency defines the dependency a resource has observed
// from the resources it points to based on expressions
type Dependency struct {
	// Name represents the name of the dependency resource
	Name string `json:"name,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="APIVERSION",type=string,priority=0,JSONPath=`.spec.schema.apiVersion`
// +kubebuilder:printcolumn:name="KIND",type=string,priority=0,JSONPath=`.spec.schema.kind`
// +kubebuilder:printcolumn:name="STATE",type=string,priority=0,JSONPath=`.status.state`
// +kubebuilder:printcolumn:name="TOPOLOGICALORDER",type=string,priority=1,JSONPath=`.status.topologicalOrder`
// +kubebuilder:printcolumn:name="AGE",type="date",priority=0,JSONPath=".metadata.creationTimestamp"
// +kubebuilder:resource:shortName=rg

// ResourceGroup is the Schema for the resourcegroups API
type ResourceGroup struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ResourceGroupSpec   `json:"spec,omitempty"`
	Status ResourceGroupStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// ResourceGroupList contains a list of ResourceGroup
type ResourceGroupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ResourceGroup `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ResourceGroup{}, &ResourceGroupList{})
}
