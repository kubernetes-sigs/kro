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

package generator

import (
	symphonyv1alpha1 "github.com/aws-controllers-k8s/symphony/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

// ResourceGroupOption is a functional option for ResourceGroup
type ResourceGroupOption func(*symphonyv1alpha1.ResourceGroup)

// NewResourceGroup creates a new ResourceGroup with the given name and options
func NewResourceGroup(name string, opts ...ResourceGroupOption) *symphonyv1alpha1.ResourceGroup {
	rg := &symphonyv1alpha1.ResourceGroup{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
	}

	for _, opt := range opts {
		opt(rg)
	}
	return rg
}

// WithNamespace sets the namespace of the ResourceGroup
func WithNamespace(namespace string) ResourceGroupOption {
	return func(rg *symphonyv1alpha1.ResourceGroup) {
		rg.Namespace = namespace
	}
}

// WithKind sets the kind and version of the ResourceGroup
func WithKind(kind, version string) ResourceGroupOption {
	return func(rg *symphonyv1alpha1.ResourceGroup) {
		rg.Spec.Kind = kind
		rg.Spec.APIVersion = version
	}
}

// WithDefinition sets the definition and status of the ResourceGroup
func WithDefinition(spec, status map[string]interface{}) ResourceGroupOption {
	return func(rg *symphonyv1alpha1.ResourceGroup) {
		rg.Spec.Definition = &symphonyv1alpha1.Definition{
			Spec: runtime.RawExtension{
				Object: &unstructured.Unstructured{Object: spec},
			},
			Status: runtime.RawExtension{
				Object: &unstructured.Unstructured{Object: status},
			},
		}
	}
}

// WithResource adds a resource to the ResourceGroup with the given name and definition
// readyOn and conditions are optional.
func WithResource(
	name string,
	definition map[string]interface{},
	readyOn []string,
	conditions []string,
) ResourceGroupOption {
	return func(rg *symphonyv1alpha1.ResourceGroup) {
		rg.Spec.Resources = append(rg.Spec.Resources, &symphonyv1alpha1.Resource{
			Name:       name,
			ReadyOn:    readyOn,
			Conditions: conditions,
			Definition: runtime.RawExtension{
				Object: &unstructured.Unstructured{Object: definition},
			},
		})
	}
}
