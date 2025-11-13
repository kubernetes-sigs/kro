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

package cel

import (
	"strings"

	"github.com/google/cel-go/cel"
	apiservercel "k8s.io/apiserver/pkg/cel"
)

// AreTypesStructurallyCompatible checks if two CEL types are structurally compatible,
// ignoring type names and using duck-typing semantics.
//
// This performs deep structural comparison:
// - For primitives: checks kind equality
// - For lists: recursively checks element type compatibility
// - For maps: recursively checks key and value type compatibility
// - For structs: uses DeclTypeProvider to introspect fields and check all required fields exist with compatible types
//
// The provider is required for introspecting struct field information.
func AreTypesStructurallyCompatible(output, expected *cel.Type, provider *apiservercel.DeclTypeProvider) bool {
	if output == nil || expected == nil {
		return false
	}

	// Dynamic type is compatible with anything
	if expected.Kind() == cel.DynKind || output.Kind() == cel.DynKind {
		return true
	}

	// Kinds must match
	if output.Kind() != expected.Kind() {
		return false
	}

	switch expected.Kind() {
	case cel.ListKind:
		return areListTypesCompatible(output, expected, provider)
	case cel.MapKind:
		return areMapTypesCompatible(output, expected, provider)
	case cel.StructKind:
		return areStructTypesCompatible(output, expected, provider)
	default:
		// For primitives (int, string, bool, etc.), kind equality is enough
		return true
	}
}

// areListTypesCompatible checks if list element types are structurally compatible.
func areListTypesCompatible(output, expected *cel.Type, provider *apiservercel.DeclTypeProvider) bool {
	outputParams := output.Parameters()
	expectedParams := expected.Parameters()

	// Both must have element type parameters
	if len(outputParams) == 0 || len(expectedParams) == 0 {
		return len(outputParams) == len(expectedParams)
	}

	// Recursively check element type compatibility
	return AreTypesStructurallyCompatible(outputParams[0], expectedParams[0], provider)
}

// areMapTypesCompatible checks if map key and value types are structurally compatible.
func areMapTypesCompatible(output, expected *cel.Type, provider *apiservercel.DeclTypeProvider) bool {
	outputParams := output.Parameters()
	expectedParams := expected.Parameters()

	// Both must have key and value type parameters
	if len(outputParams) < 2 || len(expectedParams) < 2 {
		return len(outputParams) == len(expectedParams)
	}

	// Check key type compatibility
	if !AreTypesStructurallyCompatible(outputParams[0], expectedParams[0], provider) {
		return false
	}

	// Check value type compatibility
	return AreTypesStructurallyCompatible(outputParams[1], expectedParams[1], provider)
}

// areStructTypesCompatible checks if struct types are structurally compatible
// by introspecting their fields using the DeclTypeProvider.
func areStructTypesCompatible(output, expected *cel.Type, provider *apiservercel.DeclTypeProvider) bool {
	if provider == nil {
		// Without provider, we can't introspect fields - fall back to kind check only
		return true
	}

	// Resolve DeclTypes by walking through nested type paths
	expectedDecl := resolveDeclTypeFromPath(expected.String(), provider)
	outputDecl := resolveDeclTypeFromPath(output.String(), provider)

	// If we can't resolve both types, we can't do structural comparison
	// Fall back to accepting it (permissive - could make this stricter)
	if expectedDecl == nil || outputDecl == nil {
		return true
	}

	// Check that output has all required fields of expected
	return areStructFieldsCompatible(outputDecl, expectedDecl, provider)
}

// resolveDeclTypeFromPath resolves a DeclType by walking through a nested path.
// For example, "__type_ingressroute.spec.routes.@idx.middlewares" would:
// 1. Strip __type_ prefix and look up "ingressroute" in the provider
// 2. Find the "spec" field
// 3. Find the "routes" field
// 4. Get the list element type (@idx)
// 5. Find the "middlewares" field
func resolveDeclTypeFromPath(typePath string, provider *apiservercel.DeclTypeProvider) *apiservercel.DeclType {
	if provider == nil || typePath == "" {
		return nil
	}

	// Split the path into segments
	segments := strings.Split(typePath, ".")
	if len(segments) == 0 {
		return nil
	}

	// Get the root name, stripping __type_ prefix if present
	rootName := segments[0]
	if strings.HasPrefix(rootName, "__type_") {
		rootName = strings.TrimPrefix(rootName, "__type_")
	}

	// Look up the root type in the provider
	// Types are registered with their resource ID, not the __type_ prefixed name
	currentDecl, found := provider.FindDeclType(rootName)
	if !found {
		return nil
	}

	// Walk through remaining path segments
	for i := 1; i < len(segments); i++ {
		segment := segments[i]

		// Handle list element type (@idx) and map value type (@elem)
		if segment == "@idx" || segment == "@elem" {
			if currentDecl.ElemType != nil {
				currentDecl = currentDecl.ElemType
			} else {
				return nil
			}
			continue
		}

		// Handle array index notation like "routes[0]" - strip the index
		if idx := strings.Index(segment, "["); idx != -1 {
			segment = segment[:idx]
		}

		// Look up field in current struct
		if currentDecl.Fields == nil {
			return nil
		}

		field, exists := currentDecl.Fields[segment]
		if !exists {
			return nil
		}

		currentDecl = field.Type
		if currentDecl == nil {
			return nil
		}
	}

	return currentDecl
}

// areStructFieldsCompatible checks if output struct has all required fields of expected struct.
func areStructFieldsCompatible(output, expected *apiservercel.DeclType, provider *apiservercel.DeclTypeProvider) bool {
	if expected == nil {
		return true
	}

	// If expected has no fields, any output is compatible
	expectedFields := expected.Fields
	if len(expectedFields) == 0 {
		return true
	}

	if output == nil {
		return false
	}

	outputFields := output.Fields
	if outputFields == nil {
		return false
	}

	// Check each field in expected exists in output with compatible type
	for fieldName, expectedField := range expectedFields {
		outputField, exists := outputFields[fieldName]

		// If field is required but missing, incompatible
		if expectedField.Required && !exists {
			return false
		}

		// If field exists, check type compatibility recursively
		if exists {
			expectedFieldType := expectedField.Type
			outputFieldType := outputField.Type

			if expectedFieldType == nil || outputFieldType == nil {
				continue
			}

			// Recursively compare field types
			expectedCELType := expectedFieldType.CelType()
			outputCELType := outputFieldType.CelType()

			if !AreTypesStructurallyCompatible(outputCELType, expectedCELType, provider) {
				return false
			}
		}
	}

	return true
}
