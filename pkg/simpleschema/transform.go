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

package simpleschema

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/ptr"

	"github.com/kubernetes-sigs/kro/pkg/graph/dag"
)

const (
	keyTypeString  = string(AtomicTypeString)
	keyTypeInteger = string(AtomicTypeInteger)
	keyTypeBoolean = string(AtomicTypeBool)
	keyTypeNumber  = "number"
	keyTypeObject  = "object"
	keyTypeArray   = "array"
)

// A predefinedType is a type that is predefined in the schema.
// It is used to resolve references in the schema, while capturing the fact
// whether the type has the required marker set (this information would
// otherwise be lost in the parsing process).
type predefinedType struct {
	Schema   extv1.JSONSchemaProps
	Required bool
}

// transformer is a transformer for OpenAPI schemas
type transformer struct {
	preDefinedTypes map[string]predefinedType
}

// newTransformer creates a new transformer
func newTransformer() *transformer {
	return &transformer{
		preDefinedTypes: make(map[string]predefinedType),
	}
}

// loadPreDefinedTypes loads pre-defined types into the transformer.
// The pre-defined types are used to resolve references in the schema.
// Types are loaded one by one so that each type can reference one of the
// other custom types.
// Cyclic dependencies are detected and reported as errors.
func (t *transformer) loadPreDefinedTypes(obj map[string]interface{}) error {
	// Constructs a dag of the dependencies between the types
	// If there is a cycle in the graph, then there is a cyclic dependency between the types
	// and we cannot load the types
	dagInstance := dag.NewDirectedAcyclicGraph[string]()
	t.preDefinedTypes = make(map[string]predefinedType)

	for k := range obj {
		if err := dagInstance.AddVertex(k, 0); err != nil {
			return err
		}
	}

	// Build dependencies and construct the schema
	for k, v := range obj {
		dependencies, err := extractDependenciesFromMap(v)
		if err != nil {
			return fmt.Errorf("failed to extract dependencies for type %s: %w", k, err)
		}

		// Add dependencies to the DAG and check for cycles
		if err := dagInstance.AddDependencies(k, dependencies); err != nil {
			var cycleErr *dag.CycleError[string]
			if errors.As(err, &cycleErr) {
				return fmt.Errorf("cyclic dependency detected loading type %s: %w", k, err)
			}
			return err
		}
	}

	// Perform a topological sort of the DAG to get the order of the types
	// to be processed
	orderedVertexes, err := dagInstance.TopologicalSort()
	if err != nil {
		return fmt.Errorf("failed to resolve type dependencies (possible circular reference or missing type definition): %w", err)
	}

	// Build the pre-defined types from the sorted DAG
	for _, vertex := range orderedVertexes {
		objValueAtKey := obj[vertex]
		objMap := map[string]interface{}{
			vertex: objValueAtKey,
		}
		schemaProps, err := t.buildOpenAPISchemaWithDefault(objMap, true)
		if err != nil {
			return fmt.Errorf("failed to build pre-defined types schema : %w", err)
		}
		for propKey, properties := range schemaProps.Properties {
			required := false
			if slices.Contains(schemaProps.Required, propKey) {
				required = true
			}
			t.preDefinedTypes[propKey] = predefinedType{Schema: properties, Required: required}
		}
	}

	return nil
}

func extractDependenciesFromMap(obj interface{}) (dependencies []string, err error) {
	dependenciesSet := sets.Set[string]{}

	switch t := obj.(type) {
	case map[string]interface{}:
		if err := parseMap(t, dependenciesSet); err != nil {
			return nil, err
		}
	case string:
		// Handle Type Aliases (e.g., "MyType": "string")
		if err := handleStringType(t, dependenciesSet); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("pre-defined type must be an object or string alias, got %T", obj)
	}

	return dependenciesSet.UnsortedList(), nil
}

func handleStringType(v string, dependencies sets.Set[string]) error {
	// Extract and validate type information from field declaration.
	// Markers (e.g., "required=true", "description=...") are stripped here and
	// processed separately using parseFieldSchema helper to maintain consistency
	// with main schema parsing. See parseFieldSchema for marker details.
	typeStr, _, err := parseFieldSchema(v)
	if err != nil {
		return fmt.Errorf("failed to parse markers from string type %s: %w", v, err)
	}
	v = typeStr

	return processParsedType(v, dependencies)
}

// processParsedType checks if a type is atomic, collection, object, or a custom type reference.
// It recursively processes nested types and adds custom type references to the dependencies set.
func processParsedType(v string, dependencies sets.Set[string]) error {
	// Check if the value is an atomic type
	if isAtomicType(v) {
		return nil
	}

	// Check if the value is a slice type
	if isSliceType(v) {
		elementType, err := parseSliceType(v)
		if err != nil {
			return fmt.Errorf("failed to parse slice type %s: %w", v, err)
		}
		// Recursively handle the element type to find nested dependencies
		return processParsedType(elementType, dependencies)
	}

	// Check if the value is a map type
	if isMapType(v) {
		keyType, valueType, err := parseMapType(v)
		if err != nil {
			return fmt.Errorf("failed to parse map type %s: %w", v, err)
		}
		// Only strings are supported as map keys
		if keyType != keyTypeString {
			return fmt.Errorf("unsupported key type for maps, only strings are supported key types: %s", keyType)
		}
		// Recursively handle the value type to find nested dependencies
		return processParsedType(valueType, dependencies)
	}

	// If the type is object, we do not add any dependency
	if v == keyTypeObject {
		return nil
	}

	// At this point, we have a new custom type, we add it as dependency
	dependencies.Insert(v)
	return nil
}

func parseMap(m map[string]interface{}, dependencies sets.Set[string]) error {
	for key, value := range m {
		switch v := value.(type) {
		case map[string]interface{}:
			if err := parseMap(v, dependencies); err != nil {
				return err
			}
		case string:
			if err := handleStringType(v, dependencies); err != nil {
				return err
			}
		default:
			return fmt.Errorf("invalid type in schema: key: %s, value: %v", key, value)
		}
	}
	return nil
}

// buildOpenAPISchema builds an OpenAPI schema from the given object
func (tf *transformer) buildOpenAPISchema(obj map[string]interface{}) (*extv1.JSONSchemaProps, error) {
	return tf.buildOpenAPISchemaWithDefault(obj, true)
}

// buildOpenAPISchemaWithDefault builds an OpenAPI schema from the given object with control over object defaults
func (tf *transformer) buildOpenAPISchemaWithDefault(obj map[string]interface{}, allowObjectDefault bool) (*extv1.JSONSchemaProps, error) {
	schema := &extv1.JSONSchemaProps{
		Type:       "object",
		Properties: map[string]extv1.JSONSchemaProps{},
	}

	childHasDefault := false

	for key, value := range obj {
		fieldSchema, err := tf.transformField(key, value, schema, allowObjectDefault)
		if err != nil {
			return nil, err
		}
		schema.Properties[key] = *fieldSchema

		if fieldSchema.Default != nil {
			childHasDefault = true
		}
	}

	// Only set default if this is a predefined type (allowObjectDefault=true) AND it has child defaults
	if allowObjectDefault && childHasDefault {
		schema.Default = &extv1.JSON{Raw: []byte("{}")}
	}

	return schema, nil
}

func (tf *transformer) transformField(
	key string, value interface{},
	// parentSchema is used to add the key to the required list
	parentSchema *extv1.JSONSchemaProps,
	allowObjectDefault bool,
) (*extv1.JSONSchemaProps, error) {
	switch v := value.(type) {
	case map[interface{}]interface{}:
		nMap := transformMap(v)
		return tf.buildOpenAPISchemaWithDefault(nMap, allowObjectDefault)
	case map[string]interface{}:
		return tf.buildOpenAPISchemaWithDefault(v, allowObjectDefault)
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
		fieldJSONSchemaProps.Type = fieldType
	} else if fieldType == keyTypeObject {
		fieldJSONSchemaProps.Type = fieldType
		fieldJSONSchemaProps.XPreserveUnknownFields = ptr.To(true)
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
		fieldJSONSchemaProps = &preDefinedType.Schema
		if preDefinedType.Required {
			parentSchema.Required = append(parentSchema.Required, key)
		}
	}

	if err := tf.applyMarkers(fieldJSONSchemaProps, markers, key, parentSchema); err != nil {
		return nil, fmt.Errorf("failed to apply markers: %w", err)
	}

	return fieldJSONSchemaProps, nil
}

func (tf *transformer) handleMapType(key, fieldType string) (*extv1.JSONSchemaProps, error) {
	keyType, valueType, err := parseMapType(fieldType)
	if err != nil {
		return nil, fmt.Errorf("failed to parse map type for %s: %w", key, err)
	}
	if keyType != keyTypeString {
		return nil, fmt.Errorf("unsupported key type for maps, only strings are supported key types: %s", keyType)
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
		fieldJSONSchemaProps.AdditionalProperties.Schema = &preDefinedType.Schema
	} else if isAtomicType(valueType) {
		fieldJSONSchemaProps.AdditionalProperties.Schema.Type = valueType
	} else if valueType == keyTypeObject {
		fieldJSONSchemaProps.AdditionalProperties.Schema.Type = valueType
		fieldJSONSchemaProps.AdditionalProperties.Schema.XPreserveUnknownFields = ptr.To(true)
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
		Type: keyTypeArray,
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
	} else if elementType == keyTypeObject {
		fieldJSONSchemaProps.Items.Schema.Type = elementType
		fieldJSONSchemaProps.Items.Schema.XPreserveUnknownFields = ptr.To(true)
	} else if preDefinedType, ok := tf.preDefinedTypes[elementType]; ok {
		fieldJSONSchemaProps.Items.Schema = &preDefinedType.Schema
	} else {
		return nil, fmt.Errorf("unknown type: %s", elementType)
	}

	return fieldJSONSchemaProps, nil
}

//nolint:gocyclo
func (tf *transformer) applyMarkers(schema *extv1.JSONSchemaProps, markers []*Marker, key string, parentSchema *extv1.JSONSchemaProps) error {
	for _, marker := range markers {
		switch marker.MarkerType {
		case MarkerTypeRequired:
			switch isRequired, err := strconv.ParseBool(marker.Value); {
			case err != nil:
				return fmt.Errorf("failed to parse required marker value: %w", err)
			case parentSchema == nil:
				return fmt.Errorf("required marker can't be applied; parent schema is nil")
			case isRequired:
				parentSchema.Required = append(parentSchema.Required, key)
			default:
				// ignore
			}
		case MarkerTypeDefault:
			var defaultValue []byte
			switch schema.Type {
			case keyTypeString:
				defaultValue = []byte(fmt.Sprintf("\"%s\"", marker.Value))
			case keyTypeInteger, keyTypeNumber, keyTypeBoolean:
				defaultValue = []byte(marker.Value)
			default:
				defaultValue = []byte(marker.Value)
			}
			schema.Default = &extv1.JSON{Raw: defaultValue}
		case MarkerTypeDescription:
			schema.Description = marker.Value
		case MarkerTypeMinimum:
			val, err := strconv.ParseFloat(marker.Value, 64)
			if err != nil {
				return fmt.Errorf("failed to parse minimum enum value: %w", err)
			}
			schema.Minimum = &val
		case MarkerTypeMaximum:
			val, err := strconv.ParseFloat(marker.Value, 64)
			if err != nil {
				return fmt.Errorf("failed to parse maximum enum value: %w", err)
			}
			schema.Maximum = &val
		case MarkerTypeValidation:
			if strings.TrimSpace(marker.Value) == "" {
				return fmt.Errorf("validation failed")
			}
			validation := []extv1.ValidationRule{
				{
					Rule:    marker.Value,
					Message: "validation failed",
				},
			}
			schema.XValidations = validation
		case MarkerTypeImmutable:
			isImmutable, err := strconv.ParseBool(marker.Value)
			if err != nil {
				return fmt.Errorf("failed to parse immutable marker value: %w", err)
			}
			if isImmutable {
				immutableValidation := []extv1.ValidationRule{
					{
						Rule:    "self == oldSelf",
						Message: "field is immutable",
					},
				}
				schema.XValidations = append(schema.XValidations, immutableValidation...)
			}
		case MarkerTypeEnum:
			var enumJSONValues []extv1.JSON

			enumValues := strings.Split(marker.Value, ",")
			for _, val := range enumValues {
				val = strings.TrimSpace(val)
				if val == "" {
					return fmt.Errorf("empty enum values are not allowed")
				}

				var rawValue []byte
				switch schema.Type {
				case keyTypeString:
					rawValue = []byte(fmt.Sprintf("%q", val))
				case keyTypeInteger:
					if _, err := strconv.ParseInt(val, 10, 64); err != nil {
						return fmt.Errorf("failed to parse integer enum value: %w", err)
					}
					rawValue = []byte(val)
				default:
					return fmt.Errorf("enum values only supported for string and integer types, got type: %s", schema.Type)
				}
				enumJSONValues = append(enumJSONValues, extv1.JSON{Raw: rawValue})
			}
			if len(enumJSONValues) > 0 {
				schema.Enum = enumJSONValues
			}
		case MarkerTypeMinLength:
			// MinLength is only valid for string types
			if schema.Type != keyTypeString {
				return fmt.Errorf("minLength marker is only valid for string types, got type: %s", schema.Type)
			}
			val, err := strconv.ParseInt(marker.Value, 10, 64)
			if err != nil {
				return fmt.Errorf("failed to parse minLength value: %w", err)
			}
			schema.MinLength = &val

		case MarkerTypeMaxLength:
			// MaxLength is only valid for string types
			if schema.Type != keyTypeString {
				return fmt.Errorf("maxLength marker is only valid for string types, got type: %s", schema.Type)
			}
			val, err := strconv.ParseInt(marker.Value, 10, 64)
			if err != nil {
				return fmt.Errorf("failed to parse maxLength value: %w", err)
			}
			schema.MaxLength = &val
		case MarkerTypePattern:
			if marker.Value == "" {
				return fmt.Errorf("pattern marker value cannot be empty")
			}
			// Pattern is only valid for string types
			if schema.Type != keyTypeString {
				return fmt.Errorf("pattern marker is only valid for string types, got type: %s", schema.Type)
			}
			// Validate regex
			if _, err := regexp.Compile(marker.Value); err != nil {
				return fmt.Errorf("invalid pattern regex: %w", err)
			}
			schema.Pattern = marker.Value
		case MarkerTypeUniqueItems:
			// UniqueItems is only valid for array types
			switch isUnique, err := strconv.ParseBool(marker.Value); {
			case err != nil:
				return fmt.Errorf("failed to parse uniqueItems marker value: %w", err)
			case schema.Type != keyTypeArray:
				return fmt.Errorf("uniqueItems marker is only valid for array types, got type: %s", schema.Type)
			case isUnique:
				// Always set x-kubernetes-list-type to "set" when uniqueItems is true
				// https://kubernetes.io/docs/tasks/extend-kubernetes/custom-resources/custom-resource-definitions
				// https://stackoverflow.com/questions/79399232/forbidden-uniqueitems-cannot-be-set-to-true-since-the-runtime-complexity-become
				schema.XListType = ptr.To("set")
			default:
				// ignore
			}
		case MarkerTypeMinItems:
			// MinItems is only valid for array types
			if schema.Type != keyTypeArray {
				return fmt.Errorf("minItems marker is only valid for array types, got type: %s", schema.Type)
			}
			val, err := strconv.ParseInt(marker.Value, 10, 64)
			if err != nil {
				return fmt.Errorf("failed to parse minItems value: %w", err)
			}
			schema.MinItems = &val
		case MarkerTypeMaxItems:
			// MaxItems is only valid for array types
			if schema.Type != keyTypeArray {
				return fmt.Errorf("maxItems marker is only valid for array types, got type: %s", schema.Type)
			}
			val, err := strconv.ParseInt(marker.Value, 10, 64)
			if err != nil {
				return fmt.Errorf("failed to parse maxItems value: %w", err)
			}
			schema.MaxItems = &val
		}
	}
	return nil
}

// transformMap converts a map[interface{}]interface{} to map[string]interface{}
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
