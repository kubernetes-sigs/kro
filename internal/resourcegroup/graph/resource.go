// Copyright Amazon.com Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//	http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package graph

import (
	"slices"

	rgschema "github.com/aws-controllers-k8s/symphony/internal/resourcegroup/schema"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/kube-openapi/pkg/validation/spec"

	"github.com/aws-controllers-k8s/symphony/internal/typesystem/variable"
)

// Resource represents a resource in a resource group, it hholds
// information about the resource, its schema, and its variables.
//
// This object can only be created by the GraphBuilder and it should
// not be created manually. Also this object isn't designed to be
// modified after creation.
type Resource struct {
	// id is the unique identifier of the resource. It's the name of the
	// resource in the resource group.
	// An id is unique within a resource group, and adheres to the naming
	// conventions.
	id string
	// GroupVersionKind is the GVK of the resource.
	gvr schema.GroupVersionResource
	// Schema is the JSON schema of the resource. See [JSON Schema Specification Draft 4](http://json-schema.org/)
	schema *spec.Schema
	// SchemaExt is similar to Schema but can be used to create a CRD.
	crd *extv1.CustomResourceDefinition
	// Original represents the original object we found in the resource group.
	// This will contain all fields (and CEL expressions) as they were in the
	// original object.
	originalObject *unstructured.Unstructured
	// emulatedObject is the object we'll use to emulate the resource during
	// the graph building process. This object will contain the resolved values
	// of the CEL expressions.
	//
	// NOTE(a-hilaly): Do we need to keep this object? we only need it when
	// we're building the graph. We can remove it after the graph is built.
	// Or... maybe we can keep a global cache to reduce the effort of rebuilding
	// the graph every time.
	emulatedObject *unstructured.Unstructured
	// variables is a list of the variables found in the resource (CEL expressions).
	variables []*variable.ResourceField
	// dependencies is a list of the resources this resource depends on.
	dependencies []string
	// readyOnExpressions is a list of the expressions that need to be evaluated
	// before the resource is considered ready.
	readyOnExpressions []string
	// namespaced indicates if the resource is namespaced or cluster-scoped.
	// This is useful when initiating the dynamic client to interact with the
	// resource.
	namespaced bool
}

// GetDependencies returns the dependencies of the resource.
func (r *Resource) GetDependencies() []string {
	return r.dependencies
}

// HasDependency checks if the resource has a dependency on another resource.
func (r *Resource) HasDependency(dep string) bool {
	for _, d := range r.dependencies {
		if d == dep {
			return true
		}
	}
	return false
}

// AddDependency adds a dependency to the resource.
func (r *Resource) addDependency(dep string) {
	if !r.HasDependency(dep) {
		r.dependencies = append(r.dependencies, dep)
	}
}

// addDependencies adds multiple dependencies to the resource.
func (r *Resource) addDependencies(deps ...string) {
	for _, dep := range deps {
		r.addDependency(dep)
	}
}

// GetID returns the ID of the resource.
func (r *Resource) GetID() string {
	return r.id
}

// GetGroupVersionKind returns the GVK of the resource.
func (r *Resource) GetGroupVersionResource() schema.GroupVersionResource {
	return r.gvr
}

// GetCRD returns the CRD of the resource.
func (r *Resource) GetCRD() *extv1.CustomResourceDefinition {
	return r.crd.DeepCopy()
}

// Unstructured returns the original object we found in the resource group.
func (r *Resource) Unstructured() *unstructured.Unstructured {
	return r.originalObject
}

// GetVariables returns the variables found in the resource.
func (r *Resource) GetVariables() []*variable.ResourceField {
	return r.variables
}

// GetSchema returns the JSON schema of the resource.
func (r *Resource) GetSchema() *spec.Schema {
	return r.schema
}

// GetEmulatedObject returns the emulated object of the resource.
func (r *Resource) GetEmulatedObject() *unstructured.Unstructured {
	return r.emulatedObject
}

// GetReadyOnExpressions returns the readyOn expressions of the resource.
func (r *Resource) GetReadyOnExpressions() []string {
	return r.readyOnExpressions
}

// GetTopLevelFields returns the top-level fields of the resource.
func (r *Resource) GetTopLevelFields() []string {
	return rgschema.GetResourceTopLevelFieldNames(r.schema)
}

// IsNamespaced returns true if the resource is namespaced.
func (r *Resource) IsNamespaced() bool {
	return r.namespaced
}

// DeepCopy returns a deep copy of the resource.
func (r *Resource) DeepCopy() *Resource {
	return &Resource{
		id:                 r.id,
		gvr:                r.gvr,
		schema:             r.schema,
		originalObject:     r.originalObject.DeepCopy(),
		variables:          slices.Clone(r.variables),
		dependencies:       slices.Clone(r.dependencies),
		readyOnExpressions: slices.Clone(r.readyOnExpressions),
		namespaced:         r.namespaced,
	}
}
