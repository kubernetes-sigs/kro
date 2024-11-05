package schema

import (
	"slices"

	"k8s.io/kube-openapi/pkg/validation/spec"
)

// GetResourceTopLevelFieldNames returns the top level field names
// of the resource. It walks through the schema of the resource and
// retrieves the top level fields including spec, status, metadata,
// etc.
//
// It is up to the caller to sort filter the field names they want.
func GetResourceTopLevelFieldNames(schema *spec.Schema) []string {
	fieldNames := []string{}
	if schema == nil || schema.Properties == nil {
		return fieldNames
	}
	for fieldName := range schema.Properties {
		if fieldName != "apiVersion" && fieldName != "kind" {
			fieldNames = append(fieldNames, fieldName)
		}
	}

	slices.Sort(fieldNames)
	return fieldNames
}
