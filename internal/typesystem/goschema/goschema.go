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

package goschema

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// GenerateJSONSchema generates a JSON schema from a Go object, recursively.
func GenerateJSONSchema(v interface{}) apiextensionsv1.JSONSchemaProps {
	schema := apiextensionsv1.JSONSchemaProps{}

	switch val := v.(type) {
	case bool:
		schema.Type = "boolean"
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		schema.Type = "integer"
	case float32, float64:
		schema.Type = "number"
	case string:
		schema.Type = "string"
	case []interface{}:
		schema.Type = "array"
		if len(val) > 0 {
			schema.Items = &apiextensionsv1.JSONSchemaPropsOrArray{
				Schema: &apiextensionsv1.JSONSchemaProps{},
			}
			*schema.Items.Schema = GenerateJSONSchema(val[0])
		}
	case map[string]interface{}:
		schema.Type = "object"
		schema.Properties = make(map[string]apiextensionsv1.JSONSchemaProps)
		for k, v := range val {
			schema.Properties[k] = GenerateJSONSchema(v)
		}
	default:
		// For structs and other complex types, we'll need to use reflection
		// or implement custom logic for each specific type
		schema.Type = "object"
	}

	return schema
}
