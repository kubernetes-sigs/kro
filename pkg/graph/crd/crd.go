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

	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/features"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
	"github.com/kubernetes-sigs/kro/pkg/simpleschema"
)

// SynthesizeCRD generates a CustomResourceDefinition for a given API version and kind
// with the provided spec and status schemas.
// scope must be either extv1.NamespaceScoped or extv1.ClusterScoped; defaults to NamespaceScoped.
func SynthesizeCRD(group, apiVersion, kind string, spec, status extv1.JSONSchemaProps, statusFieldsOverride bool, scope extv1.ResourceScope, rgSchema *v1alpha1.Schema) (*extv1.CustomResourceDefinition, error) {
	var nameSchema *extv1.JSONSchemaProps
	if rgSchema != nil && rgSchema.Metadata != nil && rgSchema.Metadata.Validation.Name != "" {
		var err error
		nameSchema, err = simpleschema.StringSchemaFromMarkers(rgSchema.Metadata.Validation.Name)
		if err != nil {
			return nil, fmt.Errorf("invalid metadata.validation.name: %w", err)
		}
	}
	return newCRD(group, apiVersion, kind, newCRDSchema(spec, status, statusFieldsOverride, nameSchema), scope, rgSchema), nil
}

func newCRD(group, apiVersion, kind string, schema *extv1.JSONSchemaProps, scope extv1.ResourceScope, rgSchema *v1alpha1.Schema) *extv1.CustomResourceDefinition {
	if scope == "" {
		scope = extv1.NamespaceScoped
	}

	var emptySchema v1alpha1.Schema
	if rgSchema == nil {
		rgSchema = &emptySchema
	}

	pluralKind := metadata.ResolvePlural(kind, rgSchema.Plural)

	objectMeta := metav1.ObjectMeta{
		Name:            fmt.Sprintf("%s.%s", pluralKind, group),
		OwnerReferences: nil, // Injecting owner references is the responsibility of the caller.
	}
	if rgSchema.Metadata != nil {
		if len(rgSchema.Metadata.Labels) > 0 {
			objectMeta.Labels = rgSchema.Metadata.Labels
		}
		if len(rgSchema.Metadata.Annotations) > 0 {
			objectMeta.Annotations = rgSchema.Metadata.Annotations
		}
	}

	return &extv1.CustomResourceDefinition{
		ObjectMeta: objectMeta,
		Spec: extv1.CustomResourceDefinitionSpec{
			Group: group,
			Names: extv1.CustomResourceDefinitionNames{
				Kind:       kind,
				ListKind:   kind + "List",
				Plural:     pluralKind,
				Singular:   strings.ToLower(kind),
				ShortNames: rgSchema.ShortNames,
				Categories: rgSchema.Categories,
			},
			Scope: scope,
			Versions: []extv1.CustomResourceDefinitionVersion{
				newCRDVersion(apiVersion, schema, rgSchema, true),
			},
		},
	}
}

// newCRDVersion builds one served version of the instance CRD. storage marks
// the hub version, which kro keeps at Versions[0].
func newCRDVersion(name string, schema *extv1.JSONSchemaProps, rgSchema *v1alpha1.Schema, storage bool) extv1.CustomResourceDefinitionVersion {
	return extv1.CustomResourceDefinitionVersion{
		Name:    name,
		Served:  true,
		Storage: storage,
		Schema: &extv1.CustomResourceValidation{
			OpenAPIV3Schema: schema,
		},
		Subresources: &extv1.CustomResourceSubresources{
			Status: &extv1.CustomResourceSubresourceStatus{},
		},
		AdditionalPrinterColumns: newCRDAdditionalPrinterColumns(rgSchema.AdditionalPrinterColumns),
	}
}

// AppendSpokeVersion adds a previous (non-storage) version to an instance CRD
// and switches conversion to None, under which the API server serves the
// stored object verbatim under every version.
//
// status may be nil, which leaves an empty placeholder that SetCRDStatus
// later fills with the hub's inferred status schema.
func AppendSpokeVersion(
	crd *extv1.CustomResourceDefinition,
	previous v1alpha1.PreviousSchemaVersion,
	spec extv1.JSONSchemaProps,
	status *extv1.JSONSchemaProps,
	rgSchema *v1alpha1.Schema,
) error {
	if len(crd.Spec.Versions) == 0 {
		return fmt.Errorf("cannot append version %q: CRD has no hub version", previous.APIVersion)
	}
	for _, existing := range crd.Spec.Versions {
		if existing.Name == previous.APIVersion {
			return fmt.Errorf("cannot append version %q: already present in the CRD", previous.APIVersion)
		}
	}

	var nameSchema *extv1.JSONSchemaProps
	if rgSchema != nil && rgSchema.Metadata != nil && rgSchema.Metadata.Validation.Name != "" {
		compiled, err := simpleschema.ToOpenAPISpec(
			map[string]any{"name": rgSchema.Metadata.Validation.Name}, nil)
		if err != nil {
			return fmt.Errorf("failed to build metadata.name schema for version %q: %w", previous.APIVersion, err)
		}
		named := compiled.Properties["name"]
		nameSchema = &named
	}

	statusSchema := extv1.JSONSchemaProps{Type: "object"}
	if status != nil {
		statusSchema = *status
		applyDefaultStatusFields(&statusSchema, true)
	}

	versionSchema := newCRDSchema(spec, statusSchema, false, nameSchema)
	if status == nil {
		// Leave an empty placeholder (not even the kro-owned status fields) so
		// SetCRDStatus can tell it apart from a declared status and fill it
		// with the hub's inferred schema.
		versionSchema.Properties["status"] = extv1.JSONSchemaProps{Type: "object"}
	}

	version := newCRDVersion(previous.APIVersion, versionSchema, rgSchema, false)
	version.Served = previous.Served == nil || *previous.Served
	version.Deprecated = previous.Deprecated
	if previous.Deprecated && previous.DeprecationWarning != "" {
		warning := previous.DeprecationWarning
		version.DeprecationWarning = &warning
	}

	crd.Spec.Versions = append(crd.Spec.Versions, version)
	crd.Spec.Conversion = &extv1.CustomResourceConversion{Strategy: extv1.NoneConverter}
	return nil
}

func newCRDSchema(spec, status extv1.JSONSchemaProps, statusFieldsOverride bool, nameSchema *extv1.JSONSchemaProps) *extv1.JSONSchemaProps {
	applyDefaultStatusFields(&status, statusFieldsOverride)

	metadataSchema := extv1.JSONSchemaProps{Type: "object"}
	if nameSchema != nil {
		metadataSchema.Properties = map[string]extv1.JSONSchemaProps{"name": *nameSchema}
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
			"metadata": metadataSchema,
			"spec":     spec,
			"status":   status,
		},
	}
}

func newCRDAdditionalPrinterColumns(additionalPrinterColumns []extv1.CustomResourceColumnDefinition) []extv1.CustomResourceColumnDefinition {
	if len(additionalPrinterColumns) == 0 {
		return defaultAdditionalPrinterColumns
	}

	return additionalPrinterColumns
}

// SetCRDStatus updates the status schema in a CRD.
// This allows synthesizing a CRD early (with empty status) and updating it later
// after the status schema has been inferred from CEL expressions.
//
// The hub version is Versions[0]. Previous versions that declared their own
// status keep it; those left as an empty placeholder inherit the hub's
// inferred status, since with conversion None they serve the same object.
func SetCRDStatus(crd *extv1.CustomResourceDefinition, status extv1.JSONSchemaProps, statusFieldsOverride bool) {
	applyDefaultStatusFields(&status, statusFieldsOverride)

	// Update the status in the CRD schema
	if len(crd.Spec.Versions) > 0 && crd.Spec.Versions[0].Schema != nil {
		crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["status"] = status
	}
	for i := 1; i < len(crd.Spec.Versions); i++ {
		schema := crd.Spec.Versions[i].Schema
		if schema == nil || schema.OpenAPIV3Schema == nil {
			continue
		}
		declared, ok := schema.OpenAPIV3Schema.Properties["status"]
		if ok && len(declared.Properties) > 0 {
			continue
		}
		schema.OpenAPIV3Schema.Properties["status"] = *status.DeepCopy()
	}
}

// applyDefaultStatusFields injects the status fields kro owns into an instance
// status schema.
//
// state and conditions are only injected when statusFieldsOverride is set and
// the author did not declare them. graphRevision/targetGraphRevision are owned
// by kro unconditionally once GraphRevisionPinning is enabled; the graph
// builder rejects RGDs that declare them, so there is nothing to preserve.
func applyDefaultStatusFields(status *extv1.JSONSchemaProps, statusFieldsOverride bool) {
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
	if features.FeatureGate.Enabled(features.GraphRevisionPinning) {
		status.Properties["graphRevision"] = defaultGraphRevisionType
		status.Properties["targetGraphRevision"] = defaultGraphRevisionType
	}
}
