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

package simpleschema

import (
	"fmt"

	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// Transformer is a transformer for OpenAPI schemas
type Transformer struct {
	preDefinedTypes map[string]extv1.JSONSchemaProps
}

func NewTransformer() *Transformer {
	return &Transformer{
		preDefinedTypes: make(map[string]extv1.JSONSchemaProps),
	}
}

func (t *Transformer) LoadPreDefinedTypes(obj map[string]interface{}) error {
	t.preDefinedTypes = make(map[string]extv1.JSONSchemaProps)

	jsonSchemaProps, err := t.BuildOpenAPISchema(obj)
	if err != nil {
		return fmt.Errorf("failed to build pre-defined types schema: %w", err)
	}

	for k, properties := range jsonSchemaProps.Properties {
		t.preDefinedTypes[k] = properties
	}
	return nil
}

func (tf *Transformer) BuildOpenAPISchema(obj map[string]interface{}) (*extv1.JSONSchemaProps, error) {
	schema := &extv1.JSONSchemaProps{
		Type:       "object",
		Properties: map[string]extv1.JSONSchemaProps{},
	}

	for key, value := range obj {
		switch v := value.(type) {
		case map[interface{}]interface{}:

			// we have a nested object
			nMap := transformMap(v)

			fieldSchemaProps, err := tf.BuildOpenAPISchema(nMap)
			if err != nil {
				return nil, err
			}
			schema.Properties[key] = *fieldSchemaProps
		case map[string]interface{}:
			// transform the map to a map[inteface{}]interface{}
			newMap := make(map[interface{}]interface{})
			for k, v := range v {
				newMap[k] = v
			}

			// we have a nested object
			nMap := transformMap(newMap)

			fieldSchemaProps, err := tf.BuildOpenAPISchema(nMap)
			if err != nil {
				return nil, err
			}
			schema.Properties[key] = *fieldSchemaProps
		case string:
			// we have a string. Meaning it's an atomic type, a reference to another type, or a collection type.
			// It could also contain markers like `required=true` or `description="some description"`
			// We need to parse the string to determine the type and any markers.
			fieldType, markers, err := parseFieldSchema(value.(string))
			if err != nil {
				return nil, fmt.Errorf("failed to parse field schema for %s: %v", key, err)
			}

			fieldJSONSchemaProps := extv1.JSONSchemaProps{}

			if isAtomicType(fieldType) {
				// this is an atomic type
				fieldJSONSchemaProps.Type = string(fieldType)
			} else if isCollectionType(fieldType) {
				// this is a collection type, either an array or a map
				if isMapType(fieldType) {
					keyType, valueType, err := parseMapType(fieldType)
					if err != nil {
						return nil, fmt.Errorf("failed to parse map type for %s: %w", key, err)
					}
					fieldJSONSchemaProps.Type = "object"
					fieldJSONSchemaProps.AdditionalProperties = &extv1.JSONSchemaPropsOrBool{
						Schema: &extv1.JSONSchemaProps{
							Type: keyType,
						},
					}

					if preDefinedType, ok := tf.preDefinedTypes[valueType]; ok {
						fieldJSONSchemaProps.AdditionalProperties.Schema = &preDefinedType
					} else if isAtomicType(valueType) {
						fieldJSONSchemaProps.AdditionalProperties.Schema = &extv1.JSONSchemaProps{
							Type: valueType,
						}
					} else {
						return nil, fmt.Errorf("unknown type: %s", fieldType)
					}
				} else if isSliceType(fieldType) {
					elementType, err := parseSliceType(fieldType)
					if err != nil {
						return nil, fmt.Errorf("failed to parse slice type for %s: %w", key, err)
					}

					fieldJSONSchemaProps.Type = "array"
					fieldJSONSchemaProps.Items = &extv1.JSONSchemaPropsOrArray{
						Schema: &extv1.JSONSchemaProps{
							Type: elementType,
						},
					}

					if preDefinedType, ok := tf.preDefinedTypes[elementType]; ok {
						fieldJSONSchemaProps.Items.Schema = &preDefinedType
					} else if isAtomicType(elementType) {
						fieldJSONSchemaProps.Items.Schema = &extv1.JSONSchemaProps{
							Type: elementType,
						}
					} else {
						return nil, fmt.Errorf("unknown type: %s", fieldType)
					}
				} else {
					return nil, fmt.Errorf("unknown collection type: %s", fieldType)
				}
			} else {
				// this is a reference to pre defined type.. we should look it up
				preDefinedType, ok := tf.preDefinedTypes[fieldType]
				if !ok {
					return nil, fmt.Errorf("unknown type: %s", fieldType)
				}
				fieldJSONSchemaProps = preDefinedType
			}

			// apply markers
			for _, marker := range markers {
				switch marker.MarkerType {
				case MarkerTypeRequired:
					schema.Required = append(fieldJSONSchemaProps.Required, key)
				case MarkerTypeDefault:
					// depending on the type, we need to set the default value accordingly
					var defaultValue []byte
					switch fieldJSONSchemaProps.Type {
					case "string":
						defaultValue = []byte(fmt.Sprintf("\"%s\"", marker.Value))
					case "integer", "number":
						defaultValue = []byte(marker.Value)
					case "boolean":
						defaultValue = []byte(marker.Value)
					default:
						// probably an object, array, or a map type. We can just
						// set the raw value as the default
						defaultValue = []byte(marker.Value)
					}

					fieldJSONSchemaProps.Default = &extv1.JSON{
						Raw: defaultValue,
					}
				case MarkerTypeDescription:
					fieldJSONSchemaProps.Description = marker.Value
				default:
					return nil, fmt.Errorf("unknown marker: %s", marker.MarkerType)
				}
			}

			schema.Properties[key] = fieldJSONSchemaProps
		default:
			// arrays and maps are only supported using the `[]` and `map[]` prefixes
			return nil, fmt.Errorf("unknown type in schema: key: %s, value: %s", key, value)
		}

	}
	return schema, nil
}

func transformMap(original map[interface{}]interface{}) map[string]interface{} {
	result := make(map[string]interface{})
	for key, value := range original {
		strKey, ok := key.(string)
		if !ok {
			// If the key is not a string, convert it to a string
			strKey = fmt.Sprintf("%v", key)
		}
		result[strKey] = value
	}
	return result
}
