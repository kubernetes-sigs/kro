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

package crd

import (
	"fmt"
	"strings"

	"github.com/gobuffalo/flect"
	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SynthesizeCRD generates a CustomResourceDefinition for a given API version and kind
// with the provided spec and status schemas.
// scope must be either extv1.NamespaceScoped or extv1.ClusterScoped; defaults to NamespaceScoped.
func SynthesizeCRD(group, apiVersion, kind string, spec, status extv1.JSONSchemaProps, statusFieldsOverride bool, scope extv1.ResourceScope, rgSchema *v1alpha1.Schema) *extv1.CustomResourceDefinition {
	return SynthesizeCRDWithPrinterColumns(group, apiVersion, kind, spec, status, statusFieldsOverride, scope, rgSchema, nil)
}

// SynthesizeCRDWithPrinterColumns generates a CRD and appends generated printer columns
// after the configured CRD-level columns.
func SynthesizeCRDWithPrinterColumns(
	group, apiVersion, kind string,
	spec, status extv1.JSONSchemaProps,
	statusFieldsOverride bool,
	scope extv1.ResourceScope,
	rgSchema *v1alpha1.Schema,
	generatedPrinterColumns []extv1.CustomResourceColumnDefinition,
) *extv1.CustomResourceDefinition {
	return newCRD(
		group,
		apiVersion,
		kind,
		newCRDSchema(spec, status, statusFieldsOverride),
		scope,
		rgSchema.AdditionalPrinterColumns,
		generatedPrinterColumns,
		rgSchema.Metadata,
	)
}

func newCRD(
	group, apiVersion, kind string,
	schema *extv1.JSONSchemaProps,
	scope extv1.ResourceScope,
	additionalPrinterColumns []extv1.CustomResourceColumnDefinition,
	generatedPrinterColumns []extv1.CustomResourceColumnDefinition,
	metadata *v1alpha1.CRDMetadata,
) *extv1.CustomResourceDefinition {
	pluralKind := flect.Pluralize(strings.ToLower(kind))
	if scope == "" {
		scope = extv1.NamespaceScoped
	}

	objectMeta := metav1.ObjectMeta{
		Name:            fmt.Sprintf("%s.%s", pluralKind, group),
		OwnerReferences: nil, // Injecting owner references is the responsibility of the caller.
	}
	if metadata != nil {
		if len(metadata.Labels) > 0 {
			objectMeta.Labels = metadata.Labels
		}
		if len(metadata.Annotations) > 0 {
			objectMeta.Annotations = metadata.Annotations
		}
	}

	return &extv1.CustomResourceDefinition{
		ObjectMeta: objectMeta,
		Spec: extv1.CustomResourceDefinitionSpec{
			Group: group,
			Names: extv1.CustomResourceDefinitionNames{
				Kind:     kind,
				ListKind: kind + "List",
				Plural:   pluralKind,
				Singular: strings.ToLower(kind),
			},
			Scope: scope,
			Versions: []extv1.CustomResourceDefinitionVersion{
				{
					Name:    apiVersion,
					Served:  true,
					Storage: true,
					Schema: &extv1.CustomResourceValidation{
						OpenAPIV3Schema: schema,
					},
					Subresources: &extv1.CustomResourceSubresources{
						Status: &extv1.CustomResourceSubresourceStatus{},
					},
					AdditionalPrinterColumns: newCRDAdditionalPrinterColumns(additionalPrinterColumns, generatedPrinterColumns),
				},
			},
		},
	}
}

func newCRDSchema(spec, status extv1.JSONSchemaProps, statusFieldsOverride bool) *extv1.JSONSchemaProps {
	if status.Properties == nil {
		status.Properties = make(map[string]extv1.JSONSchemaProps)
	}
	// if statusFieldsOverride is true, we will override the status fields with the default ones
	// TODO(a-hilaly): Allow users to override the default status fields.
	if statusFieldsOverride {
		if _, ok := status.Properties["state"]; !ok {
			status.Properties["state"] = defaultStateType
		}
		if _, ok := status.Properties["conditions"]; !ok {
			status.Properties["conditions"] = defaultConditionsType
		}
	}

	return &extv1.JSONSchemaProps{
		Type:     "object",
		Required: []string{},
		Properties: map[string]extv1.JSONSchemaProps{
			"apiVersion": {
				Type: "string",
			},
			"kind": {
				Type: "string",
			},
			"metadata": {
				Type: "object",
			},
			"spec":   spec,
			"status": status,
		},
	}
}

func newCRDAdditionalPrinterColumns(
	additionalPrinterColumns []extv1.CustomResourceColumnDefinition,
	generatedPrinterColumns []extv1.CustomResourceColumnDefinition,
) []extv1.CustomResourceColumnDefinition {
	baseColumns := additionalPrinterColumns
	if len(baseColumns) == 0 {
		baseColumns = defaultAdditionalPrinterColumns
	}

	mergedColumns := make([]extv1.CustomResourceColumnDefinition, 0, len(baseColumns)+len(generatedPrinterColumns))
	mergedColumns = append(mergedColumns, baseColumns...)

	seenJSONPaths := make(map[string]struct{}, len(baseColumns))
	for _, column := range baseColumns {
		seenJSONPaths[column.JSONPath] = struct{}{}
	}
	for _, column := range generatedPrinterColumns {
		if _, ok := seenJSONPaths[column.JSONPath]; ok {
			continue
		}
		seenJSONPaths[column.JSONPath] = struct{}{}
		mergedColumns = append(mergedColumns, column)
	}

	return mergedColumns
}

// SetCRDStatus updates the status schema in a CRD.
// This allows synthesizing a CRD early (with empty status) and updating it later
// after the status schema has been inferred from CEL expressions.
func SetCRDStatus(crd *extv1.CustomResourceDefinition, status extv1.JSONSchemaProps, statusFieldsOverride bool) {
	if status.Properties == nil {
		status.Properties = make(map[string]extv1.JSONSchemaProps)
	}
	if statusFieldsOverride {
		if _, ok := status.Properties["state"]; !ok {
			status.Properties["state"] = defaultStateType
		}
		if _, ok := status.Properties["conditions"]; !ok {
			status.Properties["conditions"] = defaultConditionsType
		}
	}

	// Update the status in the CRD schema
	if len(crd.Spec.Versions) > 0 && crd.Spec.Versions[0].Schema != nil {
		crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["status"] = status
	}
}
