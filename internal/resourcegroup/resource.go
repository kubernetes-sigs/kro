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

package resourcegroup

import (
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/kube-openapi/pkg/validation/spec"
)

// Resource represents a resource in a resource group, it hholds
// information about the resource, its schema, and its variables.
type Resource struct {
	ID string

	GroupVersionKind schema.GroupVersionKind
	Schema           *spec.Schema
	SchemaExt        *extv1.JSONSchemaProps
	CRD              *extv1.CustomResourceDefinition

	OriginalObject map[string]interface{}
	EmulatedObject *unstructured.Unstructured

	Variables []*ResourceVariable

	Dependencies []string
}

func (r *Resource) HasDependency(dep string) bool {
	for _, d := range r.Dependencies {
		if d == dep {
			return true
		}
	}
	return false
}

func (r *Resource) AddDependency(dep string) {
	if !r.HasDependency(dep) {
		r.Dependencies = append(r.Dependencies, dep)
	}
}

func (r *Resource) RemoveDependency(dep string) {
	for i, d := range r.Dependencies {
		if d == dep {
			r.Dependencies = append(r.Dependencies[:i], r.Dependencies[i+1:]...)
			return
		}
	}
}
