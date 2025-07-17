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

package schema

import (
	"fmt"

	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/kube-openapi/pkg/validation/spec"
)

// ConvertJSONSchemaPropsToSpecSchema converts an extv1.JSONSchemaProps to a spec.Schema.
//
// NOTE(a-hilaly): there must be an upstream library that does this conversion, but life
// is too short to find it. So I'm just going to write this function here.
func ConvertJSONSchemaPropsToSpecSchema(props *extv1.JSONSchemaProps) (*spec.Schema, error) {
	if props == nil {
		return nil, nil
	}

	var externalDocs *spec.ExternalDocumentation = nil
	if props.ExternalDocs != nil {
		if props.ExternalDocs.URL != "" {
			externalDocs.URL = props.ExternalDocs.URL
		}
		if props.ExternalDocs.Description != "" {
			externalDocs.Description = props.ExternalDocs.Description
		}
	}

	schema := &spec.Schema{
		SchemaProps: spec.SchemaProps{
			ID:               props.ID,
			Schema:           spec.SchemaURL(props.Schema),
			Title:            props.Title,
			Description:      props.Description,
			Default:          props.Default,
			Format:           props.Format,
			Maximum:          props.Maximum,
			ExclusiveMaximum: props.ExclusiveMaximum,
			Minimum:          props.Minimum,
			ExclusiveMinimum: props.ExclusiveMinimum,
			MaxLength:        props.MaxLength,
			MinLength:        props.MinLength,
			Pattern:          props.Pattern,
			MaxItems:         props.MaxItems,
			MinItems:         props.MinItems,
			UniqueItems:      props.UniqueItems,
			MultipleOf:       props.MultipleOf,
			MaxProperties:    props.MaxProperties,
			MinProperties:    props.MinProperties,
			Required:         props.Required,
			Nullable:         props.Nullable,
		},
		SwaggerSchemaProps: spec.SwaggerSchemaProps{
			ExternalDocs: externalDocs,
		},
		VendorExtensible: spec.VendorExtensible{
			Extensions: nil,
		},
	}

	if len(props.Type) > 0 {
		schema.SchemaProps.Type = []string{props.Type}
	}

	if props.Items != nil {
		if props.Items.Schema != nil {
			itemsSchema, err := ConvertJSONSchemaPropsToSpecSchema(props.Items.Schema)
			if err != nil {
				return nil, fmt.Errorf("error converting items schema: %w", err)
			}
			schema.Items = &spec.SchemaOrArray{Schema: itemsSchema}
		} else if len(props.Items.JSONSchemas) > 0 {
			schemas := make([]spec.Schema, len(props.Items.JSONSchemas))
			for i, js := range props.Items.JSONSchemas {
				convertedSchema, err := ConvertJSONSchemaPropsToSpecSchema(&js)
				if err != nil {
					return nil, fmt.Errorf("error converting item schema at index %d: %w", i, err)
				}
				schemas[i] = *convertedSchema
			}
			schema.Items = &spec.SchemaOrArray{Schemas: schemas}
		}
	}

	if props.AllOf != nil {
		schema.AllOf = make([]spec.Schema, len(props.AllOf))
		for i, js := range props.AllOf {
			convertedSchema, err := ConvertJSONSchemaPropsToSpecSchema(&js)
			if err != nil {
				return nil, fmt.Errorf("error converting allOf schema at index %d: %w", i, err)
			}
			schema.AllOf[i] = *convertedSchema
		}
	}

	if props.OneOf != nil {
		schema.OneOf = make([]spec.Schema, len(props.OneOf))
		for i, js := range props.OneOf {
			convertedSchema, err := ConvertJSONSchemaPropsToSpecSchema(&js)
			if err != nil {
				return nil, fmt.Errorf("error converting oneOf schema at index %d: %w", i, err)
			}
			schema.OneOf[i] = *convertedSchema
		}
	}

	if props.AnyOf != nil {
		schema.AnyOf = make([]spec.Schema, len(props.AnyOf))
		for i, js := range props.AnyOf {
			convertedSchema, err := ConvertJSONSchemaPropsToSpecSchema(&js)
			if err != nil {
				return nil, fmt.Errorf("error converting anyOf schema at index %d: %w", i, err)
			}
			schema.AnyOf[i] = *convertedSchema
		}
	}

	if props.Not != nil {
		notSchema, err := ConvertJSONSchemaPropsToSpecSchema(props.Not)
		if err != nil {
			return nil, fmt.Errorf("error converting not schema: %w", err)
		}
		schema.Not = notSchema
	}

	if props.Properties != nil {
		schema.Properties = make(map[string]spec.Schema)
		for k, v := range props.Properties {
			convertedSchema, err := ConvertJSONSchemaPropsToSpecSchema(&v)
			if err != nil {
				return nil, fmt.Errorf("error converting property '%s': %w", k, err)
			}
			schema.Properties[k] = *convertedSchema
		}
	}

	if props.AdditionalProperties != nil {
		if props.AdditionalProperties.Schema != nil {
			additionalPropsSchema, err := ConvertJSONSchemaPropsToSpecSchema(props.AdditionalProperties.Schema)
			if err != nil {
				return nil, fmt.Errorf("error converting additionalProperties schema: %w", err)
			}
			schema.AdditionalProperties = &spec.SchemaOrBool{Schema: additionalPropsSchema}
		} else {
			schema.AdditionalProperties = &spec.SchemaOrBool{Allows: props.AdditionalProperties.Allows}
		}
	}

	return schema, nil
}
