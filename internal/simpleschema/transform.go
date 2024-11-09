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

// transformer is a transformer for OpenAPI schemas
type transformer struct {
	preDefinedTypes map[string]extv1.JSONSchemaProps
}

// newTransformer creates a new transformer
func newTransformer() *transformer {
	return &transformer{
		preDefinedTypes: make(map[string]extv1.JSONSchemaProps),
	}
}

// loadPreDefinedTypes loads pre-defined types into the transformer.
// The pre-defined types are used to resolve references in the schema.
//
// As of today, KRO doesn't support custom types in the schema - do
// not use this function.
func (t *transformer) loadPreDefinedTypes(obj map[string]interface{}) error {
	t.preDefinedTypes = make(map[string]extv1.JSONSchemaProps)

	jsonSchemaProps, err := t.buildOpenAPISchema(obj)
	if err != nil {
		return fmt.Errorf("failed to build pre-defined types schema: %w", err)
	}

	for k, properties := range jsonSchemaProps.Properties {
		t.preDefinedTypes[k] = properties
	}
	return nil
}

// buildOpenAPISchema builds an OpenAPI schema from the given object
// of a SimpleSchema.
func (tf *transformer) buildOpenAPISchema(obj map[string]interface{}) (*extv1.JSONSchemaProps, error) {
	schema := &extv1.JSONSchemaProps{
		Type:       "object",
		Properties: map[string]extv1.JSONSchemaProps{},
	}

	for key, value := range obj {
		fieldSchema, err := tf.transformField(key, value, schema)
		if err != nil {
			return nil, err
		}
		schema.Properties[key] = *fieldSchema
	}

	return schema, nil
}
func (tf *transformer) transformField(
	key string, value interface{},
	// parentSchema is used to add the key to the required list
	parentSchema *extv1.JSONSchemaProps,
) (*extv1.JSONSchemaProps, error) {
	switch v := value.(type) {
	case map[interface{}]interface{}:
		nMap := transformMap(v)
		return tf.buildOpenAPISchema(nMap)
	case map[string]interface{}:
		return tf.buildOpenAPISchema(v)
	case string:
		return tf.parseFieldSchema(key, v, parentSchema)
	default:
		return nil, fmt.Errorf("unknown type in schema: key: %s, value: %v", key, value)
	}
}

func (tf *transformer) parseFieldSchema(key, fieldValue string, parentSchema *extv1.JSONSchemaProps) (*extv1.JSONSchemaProps, error) {
	fieldType, markers, err := parseFieldSchema(fieldValue)
	if err != nil {
		return nil, fmt.Errorf("failed to parse field schema for %s: %v", key, err)
	}

	fieldJSONSchemaProps := &extv1.JSONSchemaProps{}

	if isAtomicType(fieldType) {
		fieldJSONSchemaProps.Type = string(fieldType)
	} else if isCollectionType(fieldType) {
		if isMapType(fieldType) {
			fieldJSONSchemaProps, err = tf.handleMapType(key, fieldType)
		} else if isSliceType(fieldType) {
			fieldJSONSchemaProps, err = tf.handleSliceType(key, fieldType)
		} else {
			return nil, fmt.Errorf("unknown collection type: %s", fieldType)
		}
		if err != nil {
			return nil, err
		}
	} else {
		preDefinedType, ok := tf.preDefinedTypes[fieldType]
		if !ok {
			return nil, fmt.Errorf("unknown type: %s", fieldType)
		}
		fieldJSONSchemaProps = &preDefinedType
	}

	tf.applyMarkers(fieldJSONSchemaProps, markers, key, parentSchema)

	return fieldJSONSchemaProps, nil
}

func (tf *transformer) handleMapType(key, fieldType string) (*extv1.JSONSchemaProps, error) {
	keyType, valueType, err := parseMapType(fieldType)
	if err != nil {
		return nil, fmt.Errorf("failed to parse map type for %s: %w", key, err)
	}
	if keyType != "string" {
		return nil, fmt.Errorf("unsupported key type for maps: %s", keyType)
	}

	fieldJSONSchemaProps := &extv1.JSONSchemaProps{
		Type: "object",
		AdditionalProperties: &extv1.JSONSchemaPropsOrBool{
			Schema: &extv1.JSONSchemaProps{},
		},
	}

	if isCollectionType(valueType) {
		valueSchema, err := tf.parseFieldSchema(key, valueType, fieldJSONSchemaProps)
		if err != nil {
			return nil, err
		}
		fieldJSONSchemaProps.AdditionalProperties.Schema = valueSchema
	} else if preDefinedType, ok := tf.preDefinedTypes[valueType]; ok {
		fieldJSONSchemaProps.AdditionalProperties.Schema = &preDefinedType
	} else if isAtomicType(valueType) {
		fieldJSONSchemaProps.AdditionalProperties.Schema.Type = valueType
	} else {
		return nil, fmt.Errorf("unknown type: %s", valueType)
	}

	return fieldJSONSchemaProps, nil
}

func (tf *transformer) handleSliceType(key, fieldType string) (*extv1.JSONSchemaProps, error) {
	elementType, err := parseSliceType(fieldType)
	if err != nil {
		return nil, fmt.Errorf("failed to parse slice type for %s: %w", key, err)
	}

	fieldJSONSchemaProps := &extv1.JSONSchemaProps{
		Type: "array",
		Items: &extv1.JSONSchemaPropsOrArray{
			Schema: &extv1.JSONSchemaProps{},
		},
	}

	if isCollectionType(elementType) {
		elementSchema, err := tf.parseFieldSchema(key, elementType, fieldJSONSchemaProps)
		if err != nil {
			return nil, err
		}
		fieldJSONSchemaProps.Items.Schema = elementSchema
	} else if isAtomicType(elementType) {
		fieldJSONSchemaProps.Items.Schema.Type = elementType
	} else if preDefinedType, ok := tf.preDefinedTypes[elementType]; ok {
		fieldJSONSchemaProps.Items.Schema = &preDefinedType
	} else {
		return nil, fmt.Errorf("unknown type: %s", elementType)
	}

	return fieldJSONSchemaProps, nil
}

func (tf *transformer) applyMarkers(schema *extv1.JSONSchemaProps, markers []*Marker, key string, parentSchema *extv1.JSONSchemaProps) {
	for _, marker := range markers {
		switch marker.MarkerType {
		case MarkerTypeRequired:
			if parentSchema != nil {
				parentSchema.Required = append(parentSchema.Required, key)
			}
		case MarkerTypeDefault:
			var defaultValue []byte
			switch schema.Type {
			case "string":
				defaultValue = []byte(fmt.Sprintf("\"%s\"", marker.Value))
			case "integer", "number", "boolean":
				defaultValue = []byte(marker.Value)
			default:
				defaultValue = []byte(marker.Value)
			}
			schema.Default = &extv1.JSON{Raw: defaultValue}
		case MarkerTypeDescription:
			schema.Description = marker.Value
		}
	}
}

// Other functions (LoadPreDefinedTypes, transformMap) remain unchanged
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
