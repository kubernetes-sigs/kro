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
	"slices"

	"k8s.io/kube-openapi/pkg/validation/spec"
)

type ReadyOnExpression struct {
	Resolved   bool
	Expression string
}

// This function goes through the schema of a resource
// and retrieves the field names fo rthe readOnly feature
//
// Currently we support all the top level fields besides
// apiVersion and kind. If we do see a case where they would
// be necessary for readyOns, we can include them here.
func getResourceTopLevelFieldNames(schema *spec.Schema) []string {

	fieldNames := []string{}

	if schema == nil || schema.Properties == nil {
		return fieldNames
	}
	for k, _ := range schema.Properties {
		if k != "apiVersion" && k != "kind" {
			fieldNames = append(fieldNames, k)
		}
	}

	slices.Sort(fieldNames)
	return fieldNames
}
