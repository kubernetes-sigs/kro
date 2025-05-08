// Copyright 2025 The Kube Resource Orchestrator Authors
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

package generator

import (
	"encoding/json"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	krov1alpha1 "github.com/kro-run/kro/api/v1alpha1"
)

// ResourceGraphDefinitionOption is a functional option for ResourceGraphDefinition
type ResourceGraphDefinitionOption func(*krov1alpha1.ResourceGraphDefinition)

// NewResourceGraphDefinition creates a new ResourceGraphDefinition with the given name and options
func NewResourceGraphDefinition(name string, opts ...ResourceGraphDefinitionOption) *krov1alpha1.ResourceGraphDefinition {
	rgd := &krov1alpha1.ResourceGraphDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
	}

	for _, opt := range opts {
		opt(rgd)
	}
	return rgd
}

// WithSchema sets the definition and status of the ResourceGraphDefinition
func WithSchema(kind, version string, spec, status map[string]interface{}) ResourceGraphDefinitionOption {
	rawSpec, err := json.Marshal(spec)
	if err != nil {
		panic(err)
	}
	rawStatus, err := json.Marshal(status)
	if err != nil {
		panic(err)
	}

	return func(rgd *krov1alpha1.ResourceGraphDefinition) {
		rgd.Spec.Schema = &krov1alpha1.Schema{
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

// WithExternalRef adds an external reference to the ResourceGraphDefinition with the given name and definition
// readyWhen and includeWhen expressions are optional.
func WithExternalRef(
	id string,
	externalRef *krov1alpha1.ExternalRef,
	readyWhen []string,
	includeWhen []string,
) ResourceGraphDefinitionOption {
	return func(rgd *krov1alpha1.ResourceGraphDefinition) {
		rgd.Spec.Resources = append(rgd.Spec.Resources, &krov1alpha1.Resource{
			ID:          id,
			ReadyWhen:   readyWhen,
			IncludeWhen: includeWhen,
			ExternalRef: externalRef,
		})
	}
}

// WithResource adds a resource to the ResourceGraphDefinition with the given name and definition
// readyWhen and includeWhen expressions are optional.
func WithResource(
	id string,
	template map[string]interface{},
	readyWhen []string,
	includeWhen []string,
) ResourceGraphDefinitionOption {
	return func(rgd *krov1alpha1.ResourceGraphDefinition) {
		raw, err := json.Marshal(template)
		if err != nil {
			panic(err)
		}
		rgd.Spec.Resources = append(rgd.Spec.Resources, &krov1alpha1.Resource{
			ID:          id,
			ReadyWhen:   readyWhen,
			IncludeWhen: includeWhen,
			Template: runtime.RawExtension{
				Object: &unstructured.Unstructured{Object: template},
				Raw:    raw,
			},
		})
	}
}

func WithValidation(expression, message string) ResourceGraphDefinitionOption {
	return func(rgd *krov1alpha1.ResourceGraphDefinition) {
		rgd.Spec.Schema.Validation = append(rgd.Spec.Schema.Validation, krov1alpha1.Validation{
			Expression: expression,
			Message:    message,
		})
	}
}
