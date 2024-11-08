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
	"encoding/json"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	symphonyv1alpha1 "github.com/awslabs/symphony/api/v1alpha1"
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

// WithSchema sets the definition and status of the ResourceGroup
func WithSchema(kind, version string, spec, status map[string]interface{}) ResourceGroupOption {
	rawSpec, err := json.Marshal(spec)
	if err != nil {
		panic(err)
	}
	rawStatus, err := json.Marshal(status)
	if err != nil {
		panic(err)
	}

	return func(rg *symphonyv1alpha1.ResourceGroup) {
		rg.Spec.Schema = &symphonyv1alpha1.Schema{
			Kind:       kind,
			APIVersion: version,
			Spec: runtime.RawExtension{
				Object: &unstructured.Unstructured{Object: spec},
				Raw:    rawSpec,
			},
			Status: runtime.RawExtension{
				Object: &unstructured.Unstructured{Object: status},
				Raw:    rawStatus,
			},
		}
	}
}

// WithResource adds a resource to the ResourceGroup with the given name and definition
// readyWhen and includeWhen expressions are optional.
func WithResource(
	name string,
	template map[string]interface{},
	readyWhen []string,
	includeWhen []string,
) ResourceGroupOption {
	return func(rg *symphonyv1alpha1.ResourceGroup) {
		raw, err := json.Marshal(template)
		if err != nil {
			panic(err)
		}
		rg.Spec.Resources = append(rg.Spec.Resources, &symphonyv1alpha1.Resource{
			Name:        name,
			ReadyWhen:   readyWhen,
			IncludeWhen: includeWhen,
			Template: runtime.RawExtension{
				Object: &unstructured.Unstructured{Object: template},
				Raw:    raw,
			},
		})
	}
}
