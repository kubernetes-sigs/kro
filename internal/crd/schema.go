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

package crd

import (
	"fmt"
	"strings"

	"github.com/gobuffalo/flect"
	"gopkg.in/yaml.v2"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/aws-controllers-k8s/symphony/api/v1alpha1"
	"github.com/aws-controllers-k8s/symphony/internal/typesystem/simpleschema"
)

var (
	defaultStateType = extv1.JSONSchemaProps{
		Type: "string",
	}
	defaultConditionsType = extv1.JSONSchemaProps{
		Type: "array",
		Items: &extv1.JSONSchemaPropsOrArray{
			Schema: &extv1.JSONSchemaProps{
				Type: "object",
				Properties: map[string]extv1.JSONSchemaProps{
					"type": {
						Type: "string",
					},
					"status": {
						Type: "string",
					},
					"reason": {
						Type: "string",
					},
					"message": {
						Type: "string",
					},
					"lastTransitionTime": {
						Type: "string",
					},
				},
			},
		},
	}
)

func BuildCRDObjectFromRawNeoCRDSchema(apiVersion, kind string, def *v1alpha1.Definition) (*extv1.CustomResourceDefinition, error) {
	tr := simpleschema.NewTransformer()
	openAPIV3Schema := newBareResourceSchema()

	preDefinedTypes := make(map[string]interface{})
	if err := yaml.Unmarshal(def.Types.Raw, &preDefinedTypes); err != nil {
		return nil, err
	}

	if err := tr.LoadPreDefinedTypes(preDefinedTypes); err != nil {
		return nil, err
	}

	// spec schema
	specSchema := make(map[string]interface{})
	if err := yaml.Unmarshal(def.Spec.Raw, &specSchema); err != nil {
		return nil, err
	}

	jsonSchemaPropsSpec, err := tr.BuildOpenAPISchema(specSchema)
	if err != nil {
		return nil, fmt.Errorf("failed to build schema for %s: %v", "spec", err)
	}

	openAPIV3Schema.Properties["spec"] = *jsonSchemaPropsSpec
	if len(jsonSchemaPropsSpec.Properties) > 0 {
		openAPIV3Schema.Required = append(openAPIV3Schema.Required, "spec")
	}

	statusSchema := make(map[string]interface{})
	if err := yaml.Unmarshal(def.Status.Raw, &statusSchema); err != nil {
		return nil, err
	}

	jsonSchemaPropsStatus, err := tr.BuildOpenAPISchema(statusSchema)
	if err != nil {
		return nil, fmt.Errorf("failed to build schema for %s: %v", "status", err)
	}

	openAPIV3Schema.Properties["status"] = *jsonSchemaPropsStatus
	// inject the default state and conditions if they are not present
	if _, ok := openAPIV3Schema.Properties["status"].Properties["state"]; !ok {
		openAPIV3Schema.Properties["status"].Properties["state"] = defaultStateType
	}
	if _, ok := openAPIV3Schema.Properties["status"].Properties["conditions"]; !ok {
		openAPIV3Schema.Properties["status"].Properties["conditions"] = defaultConditionsType
	}

	return NewCRD(apiVersion, kind, openAPIV3Schema), nil
}

func newBareResourceSchema() *extv1.JSONSchemaProps {
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
			"spec": {
				Type:       "object",
				Properties: map[string]extv1.JSONSchemaProps{},
			},
			"status": {
				Type: "object",
				Properties: map[string]extv1.JSONSchemaProps{
					"state":      defaultStateType,
					"conditions": defaultConditionsType,
				},
			},
		},
	}
}

func NewCRD(apiVersion, kind string, schema *extv1.JSONSchemaProps) *extv1.CustomResourceDefinition {
	pluralKind := flect.Pluralize(strings.ToLower(kind))
	return &extv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: pluralKind + ".x.symphony.k8s.aws",
		},
		Spec: extv1.CustomResourceDefinitionSpec{
			Group: "x.symphony.k8s.aws",
			Names: extv1.CustomResourceDefinitionNames{
				Kind:     kind,
				ListKind: kind + "List",
				Plural:   pluralKind,
				Singular: strings.ToLower(kind),
			},
			Scope: extv1.NamespaceScoped,
			Versions: []extv1.CustomResourceDefinitionVersion{
				{
					Name:    apiVersion,
					Served:  true,
					Storage: true,
					Schema: &extv1.CustomResourceValidation{
						OpenAPIV3Schema: schema,
					},
				},
			},
		},
	}
}
