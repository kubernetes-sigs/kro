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
	"fmt"
	"regexp"
	"strconv"

	"github.com/google/cel-go/common/types/ref"
	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/generated/openapi"
	"k8s.io/apiserver/pkg/cel/openapi/resolver"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/kube-openapi/pkg/validation/spec"

	"github.com/aws-controllers-k8s/symphony/internal/celutil"
)

// newCombinedResolver creates a new schema resolver that can resolve both core and client types.
func newCombinedResolver(clientConfig *rest.Config) (resolver.SchemaResolver, error) {
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(clientConfig)
	if err != nil {
		return nil, err
	}

	// ClientResolver is a resolver that uses the discovery client to resolve
	// client types. It is used to resolve types that are not known at compile
	// time a.k.a present in:
	// https://github.com/kubernetes/apiextensions-apiserver/blob/master/pkg/generated/openapi/zz_generated.openapi.go
	clientResolver := &resolver.ClientDiscoveryResolver{
		Discovery: discoveryClient,
	}

	// CoreResolver is a resolver that uses the OpenAPI definitions to resolve
	// core types. It is used to resolve types that are known at compile time.
	coreResolver := resolver.NewDefinitionsSchemaResolver(
		openapi.GetOpenAPIDefinitions,
		scheme.Scheme,
	)

	// Combine the two resolvers to create a single resolver that can resolve
	// both core and client types.
	combinedResolver := coreResolver.Combine(clientResolver)
	return combinedResolver, nil
}

func inferSchemaTypeFromValue(val ref.Val) (*extv1.JSONSchemaProps, error) {
	if val == nil {
		return nil, fmt.Errorf("value is nil")
	}
	goRuntimeVal, err := celutil.ConvertCELtoGo(val)
	if err != nil {
		return nil, fmt.Errorf("failed to convert CEL value to Go: %w", err)
	}
	return inferSchemaTypeFromGoValue(goRuntimeVal)
}

func inferSchemaTypeFromGoValue(goRuntimeVal interface{}) (*extv1.JSONSchemaProps, error) {
	switch goRuntimeVal := goRuntimeVal.(type) {
	case bool:
		return &extv1.JSONSchemaProps{
			Type: "boolean",
		}, nil
	case int64:
		return &extv1.JSONSchemaProps{
			Type: "integer",
		}, nil
	case uint64:
		return &extv1.JSONSchemaProps{
			Type: "integer",
		}, nil
	case float64:
		return &extv1.JSONSchemaProps{
			Type: "number",
		}, nil
	case string:
		return &extv1.JSONSchemaProps{
			Type: "string",
		}, nil
	case []interface{}:
		return inferArraySchema(goRuntimeVal)
	case map[string]interface{}:
		return inferObjectSchema(goRuntimeVal)
	default:
		return nil, fmt.Errorf("unsupported type: %T", goRuntimeVal)
	}
}

func inferArraySchema(arr []interface{}) (*extv1.JSONSchemaProps, error) {
	schema := &extv1.JSONSchemaProps{
		Type: "array",
	}

	if len(arr) > 0 {
		itemSchema, err := inferSchemaTypeFromGoValue(arr[0])
		if err != nil {
			return nil, fmt.Errorf("failed to infer schema for array item: %w", err)
		}
		schema.Items = &extv1.JSONSchemaPropsOrArray{
			Schema: itemSchema,
		}
	}

	return schema, nil
}

func inferObjectSchema(obj map[string]interface{}) (*extv1.JSONSchemaProps, error) {
	schema := &extv1.JSONSchemaProps{
		Type:       "object",
		Properties: make(map[string]extv1.JSONSchemaProps),
	}

	for key, value := range obj {
		propSchema, err := inferSchemaTypeFromGoValue(value)
		if err != nil {
			return nil, fmt.Errorf("failed to infer schema for property %s: %w", key, err)
		}
		schema.Properties[key] = *propSchema
	}

	return schema, nil
}

// BuildJSONSchemaProps generates a JSONSchemaProps from a map
func BuildJSONSchemaProps(data map[string]interface{}, mapping map[string]string) (*extv1.JSONSchemaProps, error) {
	return buildSchemaRecursive(data, mapping)
}

func buildSchemaRecursive(value interface{}, mapping map[string]string) (*extv1.JSONSchemaProps, error) {
	switch v := value.(type) {
	case string:
		schemaType, ok := mapping[v]
		if !ok {
			return nil, fmt.Errorf("no mapping found for value: %s", v)
		}
		return &extv1.JSONSchemaProps{Type: schemaType}, nil

	case map[string]interface{}:
		properties := make(map[string]extv1.JSONSchemaProps)
		for key, val := range v {
			schema, err := buildSchemaRecursive(val, mapping)
			if err != nil {
				return nil, fmt.Errorf("error processing key '%s': %w", key, err)
			}
			properties[key] = *schema
		}
		return &extv1.JSONSchemaProps{
			Type:       "object",
			Properties: properties,
		}, nil

	case []interface{}:
		if len(v) == 0 {
			return &extv1.JSONSchemaProps{Type: "array"}, nil
		}
		itemSchema, err := buildSchemaRecursive(v[0], mapping)
		if err != nil {
			return nil, fmt.Errorf("error processing array item: %w", err)
		}
		return &extv1.JSONSchemaProps{
			Type: "array",
			Items: &extv1.JSONSchemaPropsOrArray{
				Schema: itemSchema,
			},
		}, nil

	default:
		return nil, fmt.Errorf("unsupported type: %T", v)
	}
}

// StatusStructurePart represents a part of the status structure
type StatusStructurePart struct {
	Path   string
	Schema *extv1.JSONSchemaProps
}

// buildJSONSchemaFromStatusStructure generates a JSONSchemaProps from a list of StatusStructureParts
func buildJSONSchemaFromStatusStructure(parts []StatusStructurePart) (*extv1.JSONSchemaProps, error) {
	rootSchema := &extv1.JSONSchemaProps{
		Type:       "object",
		Properties: make(map[string]extv1.JSONSchemaProps),
	}

	for _, part := range parts {
		if err := addPartToSchema(rootSchema, part); err != nil {
			return nil, err
		}
	}

	return rootSchema, nil
}

func addPartToSchema(schema *extv1.JSONSchemaProps, part StatusStructurePart) error {
	pathParts := parsePath(part.Path)
	currentSchema := schema

	for i, pathPart := range pathParts {
		isLast := i == len(pathParts)-1

		if pathPart.isArray {
			// Handle array part
			if currentSchema.Type != "array" {
				currentSchema.Type = "array"
				currentSchema.Items = &extv1.JSONSchemaPropsOrArray{
					Schema: &extv1.JSONSchemaProps{
						Type:       "object",
						Properties: make(map[string]extv1.JSONSchemaProps),
					},
				}
			}
			currentSchema = currentSchema.Items.Schema
		}

		if isLast {
			// This is the final part of the path, add the schema here
			if part.Schema != nil {
				if pathPart.isArray {
					*currentSchema = *part.Schema
				} else {
					currentSchema.Properties[pathPart.name] = *part.Schema
				}
			} else {
				// If no schema is provided, default to a string type
				defaultSchema := extv1.JSONSchemaProps{Type: "string"}
				if pathPart.isArray {
					*currentSchema = defaultSchema
				} else {
					currentSchema.Properties[pathPart.name] = defaultSchema
				}
			}
		} else {
			// This is an intermediate part of the path
			if !pathPart.isArray {
				if _, exists := currentSchema.Properties[pathPart.name]; !exists {
					currentSchema.Properties[pathPart.name] = extv1.JSONSchemaProps{
						Type:       "object",
						Properties: make(map[string]extv1.JSONSchemaProps),
					}
				}
				s := currentSchema.Properties[pathPart.name]
				currentSchema = &s
			}
		}
	}

	return nil
}

type pathPart struct {
	name    string
	isArray bool
	index   int
}

var pathParts = regexp.MustCompile(`(\w+)(?:\[(\d+)\])?`)

func parsePath(path string) []pathPart {
	matches := pathParts.FindAllStringSubmatch(path, -1)

	parts := make([]pathPart, len(matches))
	for i, match := range matches {
		part := pathPart{name: match[1]}
		if len(match) > 2 && match[2] != "" {
			part.isArray = true
			part.index, _ = strconv.Atoi(match[2])
		}
		parts[i] = part
	}
	return parts
}

func newInstanceSchema(spec, status extv1.JSONSchemaProps) *extv1.JSONSchemaProps {
	// inject conditions and state into status schema if they are not present
	if status.Properties == nil {
		status.Properties = make(map[string]extv1.JSONSchemaProps)
	}
	if _, ok := status.Properties["state"]; !ok {
		status.Properties["state"] = defaultStateType
	}
	if _, ok := status.Properties["conditions"]; !ok {
		status.Properties["conditions"] = defaultConditionsType
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

func JSONSchemaPropsToSpecSchema(jsonSchemaProps *apiextensions.JSONSchemaProps) (*spec.Schema, error) {
	structural, err := schema.NewStructural(jsonSchemaProps)
	if err != nil {
		return nil, err
	}

	return structural.ToKubeOpenAPI(), nil
}

func JSONV1SchemaPropsToSpecSchema(jsonSchemaProps *extv1.JSONSchemaProps) (*spec.Schema, error) {
	to := apiextensions.JSONSchemaProps{}
	extv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(jsonSchemaProps, &to, nil)

	return JSONSchemaPropsToSpecSchema(&to)
}

// ConvertJSONSchemaPropsToSpecSchema converts an extv1.JSONSchemaProps to a spec.Schema
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
			Type:             spec.StringOrArray([]string{props.Type}),
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
