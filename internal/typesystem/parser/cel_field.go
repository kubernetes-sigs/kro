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

package parser

import "k8s.io/kube-openapi/pkg/validation/spec"

// CELField represents a field that contains CEL expressions in it. It
// contains the path of the field in the resource, the CEL expressions
// and the expected type of the field. The field may contain multiple
// expressions.
type CELField struct {
	// Path is the path of the field in the resource (JSONPath-like)
	// example: spec.template.spec.containers[0].env[0].value
	// Since the object's we're dealing with are mainly made up of maps,
	// arrays and native types, we can use a string to represent the path.
	Path string
	// Expressions is a list of CEL expressions in the field.
	Expressions []string
	// ExpectedType is the expected type of the field.
	ExpectedType string
	// ExpectedSchema is the expected schema of the field if it is a complex type.
	// This is only set if the field is a OneShotCEL expression, and the schema
	// is expected to be a complex type (object or array).
	ExpectedSchema *spec.Schema
	// StandaloneExpression is true if the field contains a single CEL expression
	// that is not part of a larger string. example: "${foo}" is a standalone expression
	// but not "hello-${foo}" or "${foo}${bar}"
	StandaloneExpression bool
}
