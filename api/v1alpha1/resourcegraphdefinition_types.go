// Copyright 2025 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package v1alpha1

import (
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// ResourceGraphDefinitionSpec defines the desired state of ResourceGraphDefinition.
// It contains the schema for instances (defining the CRD structure) and the list of
// Kubernetes resources that make up the graph.
type ResourceGraphDefinitionSpec struct {
	// Schema defines the structure of instances created from this ResourceGraphDefinition.
	// It specifies the API version, kind, and fields (spec/status) for the generated CRD.
	// Use SimpleSchema syntax to define the instance schema concisely.
	//
	// +kubebuilder:validation:Required
	Schema *Schema `json:"schema,omitempty"`
	// Resources is the list of Kubernetes resources that will be created and managed
	// for each instance. Resources can reference each other using CEL expressions,
	// creating a dependency graph that determines creation order.
	//
	// +kubebuilder:validation:Optional
	Resources []*Resource `json:"resources,omitempty"`
}

// Schema defines the structure and behavior of instances created from a ResourceGraphDefinition.
// It specifies the API group, version, and kind for the generated CRD, along with the
// spec and status schemas using SimpleSchema syntax. You can also define custom types,
// validation rules, and printer columns.
type Schema struct {
	// Kind is the name of the custom resource type that will be created.
	// This becomes the kind field of the generated CRD and must be a valid Kubernetes kind name
	// (PascalCase, starting with a capital letter). This field is immutable after creation.
	// Example: "WebApplication", "Database", "MicroService"
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[A-Z][a-zA-Z0-9]{0,62}$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="kind is immutable"
	Kind string `json:"kind,omitempty"`
	// APIVersion is the version identifier for the generated CRD.
	// Must follow Kubernetes versioning conventions (v1, v1alpha1, v1beta1, etc.).
	// This field is immutable after creation.
	// Example: "v1alpha1", "v1", "v2beta1"
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^v[0-9]+(alpha[0-9]+|beta[0-9]+)?$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="apiVersion is immutable"
	APIVersion string `json:"apiVersion,omitempty"`
	// Group is the API group for the generated CRD. Together with APIVersion and Kind,
	// it forms the complete GVK (Group-Version-Kind) identifier.
	// If omitted, defaults to "kro.run". This field is immutable after creation.
	// Example: "mycompany.io", "databases.example.com"
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:default="kro.run"
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="group is immutable"
	Group string `json:"group,omitempty"`
	// Spec defines the schema for the instance's spec section using SimpleSchema syntax.
	// This becomes the OpenAPI schema for instances of the generated CRD.
	// Use SimpleSchema's concise syntax to define fields, types, defaults, and validations.
	// Example: {"replicas": "integer | default=1 | min=1 | max=10"}
	Spec runtime.RawExtension `json:"spec,omitempty"`

	// Types is a map of custom type definitions that can be referenced in the Spec.
	// This allows you to define reusable complex types using SimpleSchema syntax.
	// Reference custom types in Spec using the type name.
	// Example: {"Server": {"host": "string", "port": "integer"}}
	Types runtime.RawExtension `json:"types,omitempty"`

	// Status defines the schema for the instance's status section using SimpleSchema syntax.
	// Unlike spec, status fields use CEL expressions to project values from underlying resources.
	// This allows you to surface important information from managed resources at the instance level.
	// Example: {"connectionName": "${database.status.connectionName}", "endpoint": "${service.status.loadBalancer.ingress[0].hostname}"}
	Status runtime.RawExtension `json:"status,omitempty"`

	// AdditionalPrinterColumns defines additional printer columns
	// that will be passed down to the created CRD. If set, no
	// default printer columns will be added to the created CRD,
	// and if default printer columns need to be retained, they
	// need to be added explicitly.
	//
	// +kubebuilder:validation:Optional
	AdditionalPrinterColumns []extv1.CustomResourceColumnDefinition `json:"additionalPrinterColumns,omitempty"`
}

type ExternalRefMetadata struct {
	// Name is the name of the external resource to reference.
	// This field is required.
	//
	// +kubebuilder:validation:Required
	Name string `json:"name,omitempty"`
	// Namespace is the namespace of the external resource.
	// If empty, the instance's namespace will be used.
	//
	// +kubebuilder:validation:Optional
	Namespace string `json:"namespace,omitempty"`
}

// ExternalRef is a reference to an external resource that already exists in the cluster.
// It allows you to read and use existing resources in your ResourceGraphDefinition
// without creating them. The referenced resource's fields can be accessed using CEL
// expressions in other resources.
type ExternalRef struct {
	// APIVersion is the API version of the external resource.
	// Example: "v1" for core resources, "apps/v1" for Deployments.
	//
	// +kubebuilder:validation:Required
	APIVersion string `json:"apiVersion"`
	// Kind is the kind of the external resource.
	// Example: "Service", "ConfigMap", "Deployment".
	//
	// +kubebuilder:validation:Required
	Kind string `json:"kind"`
	// Metadata contains the name and optional namespace of the external resource.
	//
	// +kubebuilder:validation:Required
	Metadata ExternalRefMetadata `json:"metadata"`
}

// ForEachDimension defines a single expansion axis in a forEach block.
// Each dimension is a map with exactly one entry where the key is the variable name
// and the value is the CEL expression. Example: {"region": "${schema.spec.regions}"}
// Multiple dimensions create a cartesian product of expansions.
//
// +kubebuilder:validation:MinProperties=1
// +kubebuilder:validation:MaxProperties=1
type ForEachDimension map[string]string

// Resource represents a Kubernetes resource that is part of the ResourceGraphDefinition.
// Each resource can either be created using a template or reference an existing resource.
// Resources can depend on each other through CEL expressions, creating a dependency graph.
//
// +kubebuilder:validation:XValidation:rule="(has(self.template) && !has(self.externalRef)) || (!has(self.template) && has(self.externalRef))",message="exactly one of template or externalRef must be provided"
type Resource struct {
	// ID is a unique identifier for this resource within the ResourceGraphDefinition.
	// It is used to reference this resource in CEL expressions from other resources.
	// Example: "deployment", "service", "configmap".
	//
	// +kubebuilder:validation:Required
	ID string `json:"id,omitempty"`
	// Template is the Kubernetes resource manifest to create.
	// It can contain CEL expressions (using ${...} syntax) that reference other resources.
	// Exactly one of template or externalRef must be provided.
	//
	// +kubebuilder:validation:Optional
	Template runtime.RawExtension `json:"template,omitempty"`
	// ExternalRef references an existing resource in the cluster instead of creating one.
	// This is useful for reading existing resources and using their values in other resources.
	// Exactly one of template or externalRef must be provided.
	//
	// +kubebuilder:validation:Optional
	ExternalRef *ExternalRef `json:"externalRef,omitempty"`
	// ReadyWhen is a list of CEL expressions that determine when this resource is considered ready.
	// All expressions must evaluate to true for the resource to be ready.
	// If not specified, the resource is considered ready when it exists.
	// Example: ["self.status.phase == 'Running'", "self.status.readyReplicas > 0"]
	//
	// +kubebuilder:validation:Optional
	ReadyWhen []string `json:"readyWhen,omitempty"`
	// IncludeWhen is a list of CEL expressions that determine whether this resource should be created.
	// All expressions must evaluate to true for the resource to be included.
	// If not specified, the resource is always included.
	// Example: ["schema.spec.enableMonitoring == true"]
	//
	// +kubebuilder:validation:Optional
	IncludeWhen []string `json:"includeWhen,omitempty"`
	// ForEach expands this resource into a collection of resources.
	// Each entry binds a variable name to a CEL expression that evaluates to an array.
	// kro creates one resource instance for each element in the array.
	// With multiple entries, kro creates the cartesian product of all combinations.
	// Use the variable directly in template expressions (e.g., ${region}).
	// Example: [{"region": "${schema.spec.regions}"}, {"tier": "${schema.spec.tiers}"}]
	//
	// +kubebuilder:validation:Optional
	ForEach []ForEachDimension `json:"forEach,omitempty"`
}

// ResourceGraphDefinitionState defines the state of the resource graph definition.
type ResourceGraphDefinitionState string

const (
	// ResourceGraphDefinitionStateActive represents the active state of the resource definition.
	ResourceGraphDefinitionStateActive ResourceGraphDefinitionState = "Active"
	// ResourceGraphDefinitionStateInactive represents the inactive state of the resource graph definition
	ResourceGraphDefinitionStateInactive ResourceGraphDefinitionState = "Inactive"
)

// ResourceGraphDefinitionStatus defines the observed state of ResourceGraphDefinition.
// It provides information about the deployment state, resource ordering, and conditions.
type ResourceGraphDefinitionStatus struct {
	// State indicates whether the ResourceGraphDefinition is Active or Inactive.
	// Active means the CRD has been created and the controller is running.
	// Inactive means the ResourceGraphDefinition has been disabled or encountered an error.
	State ResourceGraphDefinitionState `json:"state,omitempty"`
	// TopologicalOrder is the ordered list of resource IDs based on their dependencies.
	// Resources are created in this order to ensure dependencies are satisfied.
	// Example: ["configmap", "deployment", "service"]
	TopologicalOrder []string `json:"topologicalOrder,omitempty"`
	// Conditions represent the latest available observations of the ResourceGraphDefinition's state.
	// Common condition types include "Ready", "Validated", and "ReconcilerDeployed".
	Conditions Conditions `json:"conditions,omitempty"`
	// Resources provides detailed information about each resource in the graph,
	// including their dependencies.
	Resources []ResourceInformation `json:"resources,omitempty"`
}

// ResourceInformation provides detailed information about a specific resource
// in the ResourceGraphDefinition, particularly its dependencies on other resources.
type ResourceInformation struct {
	// ID is the unique identifier of the resource as defined in the resources list.
	ID string `json:"id,omitempty"`
	// Dependencies lists all resources that this resource depends on.
	// A resource depends on another if it references it in a CEL expression.
	// These dependencies determine the order of resource creation.
	Dependencies []Dependency `json:"dependencies,omitempty"`
}

// Dependency represents a dependency relationship between resources.
// When a resource uses CEL expressions to reference another resource,
// a dependency is created to ensure proper ordering.
type Dependency struct {
	// ID is the unique identifier of the resource that this resource depends on.
	ID string `json:"id,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="APIVERSION",type=string,priority=0,JSONPath=`.spec.schema.apiVersion`
// +kubebuilder:printcolumn:name="KIND",type=string,priority=0,JSONPath=`.spec.schema.kind`
// +kubebuilder:printcolumn:name="STATE",type=string,priority=0,JSONPath=`.status.state`
// +kubebuilder:printcolumn:name="TOPOLOGICALORDER",type=string,priority=1,JSONPath=`.status.topologicalOrder`
// +kubebuilder:printcolumn:name="AGE",type="date",priority=0,JSONPath=".metadata.creationTimestamp"
// +kubebuilder:resource:shortName=rgd,scope=Cluster

// ResourceGraphDefinition is the core API for defining reusable groups of Kubernetes resources.
// It allows you to create custom resources that manage multiple underlying resources as a cohesive unit.
// When you create a ResourceGraphDefinition, kro automatically generates a CRD and deploys a controller
// to manage instances of your custom resource. Resources can reference each other using CEL expressions,
// and kro ensures they are created in the correct dependency order.
type ResourceGraphDefinition struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ResourceGraphDefinitionSpec   `json:"spec,omitempty"`
	Status ResourceGraphDefinitionStatus `json:"status,omitempty"`
}

func (o *ResourceGraphDefinition) GetConditions() []Condition {
	return o.Status.Conditions
}

func (o *ResourceGraphDefinition) SetConditions(conditions []Condition) {
	o.Status.Conditions = conditions
}

// +kubebuilder:object:root=true

// ResourceGraphDefinitionList contains a list of ResourceGraphDefinition
type ResourceGraphDefinitionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ResourceGraphDefinition `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ResourceGraphDefinition{}, &ResourceGraphDefinitionList{})
}
