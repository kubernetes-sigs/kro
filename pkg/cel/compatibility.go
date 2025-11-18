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
	"fmt"
	"strings"

	"github.com/google/cel-go/cel"
	apiservercel "k8s.io/apiserver/pkg/cel"
)

const (
	// TypeNamePrefix is the prefix used for CEL type names when converting OpenAPI schemas.
	// Used to namespace custom types and avoid conflicts with built-in CEL types.
	// Example: "__type_schema.spec.ports" for a ports field in the schema resource.
	TypeNamePrefix = "__type_"
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
// Returns true if types are compatible, false otherwise. If false, the error describes why.
func AreTypesStructurallyCompatible(output, expected *cel.Type, provider *DeclTypeProvider) (bool, error) {
	if output == nil || expected == nil {
		return false, fmt.Errorf("nil type(s): output=%v, expected=%v", output, expected)
	}

	// Dynamic type is compatible with anything
	if expected.Kind() == cel.DynKind || output.Kind() == cel.DynKind {
		return true, nil
	}

	// Kinds must match
	if output.Kind() != expected.Kind() {
		return false, fmt.Errorf("type kind mismatch: got %q, expected %q", output.String(), expected.String())
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
		return true, nil
	}
}

// areListTypesCompatible checks if list element types are structurally compatible.
func areListTypesCompatible(output, expected *cel.Type, provider *DeclTypeProvider) (bool, error) {
	outputParams := output.Parameters()
	expectedParams := expected.Parameters()

	// Both must have element type parameters
	if len(outputParams) == 0 || len(expectedParams) == 0 {
		if len(outputParams) != len(expectedParams) {
			return false, fmt.Errorf("list parameter count mismatch: got %d, expected %d", len(outputParams), len(expectedParams))
		}
		return true, nil
	}

	// Recursively check element type compatibility
	compatible, err := AreTypesStructurallyCompatible(outputParams[0], expectedParams[0], provider)
	if !compatible {
		return false, fmt.Errorf("list element type incompatible: %w", err)
	}
	return true, nil
}

// areMapTypesCompatible checks if map key and value types are structurally compatible.
func areMapTypesCompatible(output, expected *cel.Type, provider *DeclTypeProvider) (bool, error) {
	outputParams := output.Parameters()
	expectedParams := expected.Parameters()

	// Both must have key and value type parameters
	if len(outputParams) < 2 || len(expectedParams) < 2 {
		if len(outputParams) != len(expectedParams) {
			return false, fmt.Errorf("map parameter count mismatch: got %d, expected %d", len(outputParams), len(expectedParams))
		}
		return true, nil
	}

	// Check key type compatibility
	compatible, err := AreTypesStructurallyCompatible(outputParams[0], expectedParams[0], provider)
	if !compatible {
		return false, fmt.Errorf("map key type incompatible: %w", err)
	}

	// Check value type compatibility
	compatible, err = AreTypesStructurallyCompatible(outputParams[1], expectedParams[1], provider)
	if !compatible {
		return false, fmt.Errorf("map value type incompatible: %w", err)
	}
	return true, nil
}

// areStructTypesCompatible checks if struct types are structurally compatible
// by introspecting their fields using the DeclTypeProvider.
func areStructTypesCompatible(output, expected *cel.Type, provider *DeclTypeProvider) (bool, error) {
	if provider == nil {
		// Without provider, we can't introspect fields - fall back to kind check only
		return true, nil
	}

	// Resolve DeclTypes by walking through nested type paths
	expectedDecl := resolveDeclTypeFromPath(expected.String(), provider)
	outputDecl := resolveDeclTypeFromPath(output.String(), provider)

	// If we can't resolve both types, we can't do structural comparison
	// Fall back to accepting it (permissive - could make this stricter)
	if expectedDecl == nil || outputDecl == nil {
		return true, nil
	}

	// Check that output has all required fields of expected
	return areStructFieldsCompatible(outputDecl, expectedDecl, provider)
}

// resolveDeclTypeFromPath resolves a DeclType by walking through a nested path.
// For example, "__type_ingressroute.spec.routes.@idx.middlewares" would:
// 1. Strip TypeNamePrefix and look up "ingressroute" in the provider
// 2. Find the "spec" field
// 3. Find the "routes" field
// 4. Get the list element type (@idx)
// 5. Find the "middlewares" field
func resolveDeclTypeFromPath(typePath string, provider *DeclTypeProvider) *apiservercel.DeclType {
	if provider == nil || typePath == "" {
		return nil
	}

	// Split the path into segments
	segments := strings.Split(typePath, ".")
	if len(segments) == 0 {
		return nil
	}

	// Get the root name - keep it as-is (with or without prefix)
	rootName := segments[0]

	// Look up the root type in the provider
	// Try first with the name as-is, then try without prefix if it has one
	currentDecl, found := provider.FindDeclType(rootName)
	if !found && strings.HasPrefix(rootName, TypeNamePrefix) {
		// Try without prefix for backwards compatibility
		shortName := strings.TrimPrefix(rootName, TypeNamePrefix)
		currentDecl, found = provider.FindDeclType(shortName)
	}
	if !found {
		return nil
	}

	// Walk through remaining path segments
	for i := 1; i < len(segments); i++ {
		segment := segments[i]

		// Handle list element type (@idx) and map value type (@elem)
		// These are KRO conventions used in DeclTypeProvider, not CEL built-ins
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

// areStructFieldsCompatible checks if output struct is a subset of expected struct.
// The output type can have fewer fields than expected (subset semantics), but cannot have extra fields.
// For each field that exists in output:
// 1. The field must exist in expected
// 2. The field type must be compatible
func areStructFieldsCompatible(output, expected *apiservercel.DeclType, provider *DeclTypeProvider) (bool, error) {
	if expected == nil {
		return true, nil
	}

	if output == nil {
		return false, fmt.Errorf("output type is nil")
	}

	outputFields := output.Fields
	if outputFields == nil {
		// Output has no fields - this is a valid subset of any expected type
		return true, nil
	}

	expectedFields := expected.Fields
	if expectedFields == nil {
		// Expected has no fields, but output does - incompatible
		if len(outputFields) > 0 {
			return false, fmt.Errorf("output has fields but expected type has none")
		}
		return true, nil
	}

	// Check each field in output exists in expected with compatible type
	for fieldName, outputField := range outputFields {
		expectedField, exists := expectedFields[fieldName]

		// Output has a field that expected doesn't have - not a subset
		if !exists {
			return false, fmt.Errorf("field %q exists in output but not in expected type", fieldName)
		}

		// Field exists in both - check type compatibility recursively
		expectedFieldType := expectedField.Type
		outputFieldType := outputField.Type

		if expectedFieldType == nil || outputFieldType == nil {
			continue
		}

		// Recursively compare field types
		expectedCELType := expectedFieldType.CelType()
		outputCELType := outputFieldType.CelType()

		compatible, err := AreTypesStructurallyCompatible(outputCELType, expectedCELType, provider)
		if !compatible {
			return false, fmt.Errorf("field %q has incompatible type: %w", fieldName, err)
		}
	}

	return true, nil
}
