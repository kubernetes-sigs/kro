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
	"testing"

	"github.com/google/cel-go/cel"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiservercel "k8s.io/apiserver/pkg/cel"
)

func TestPrimitiveTypes(t *testing.T) {
	tests := []struct {
		name        string
		output      *cel.Type
		expected    *cel.Type
		compatible  bool
		errContains string
	}{
		{
			name:       "string to string",
			output:     cel.StringType,
			expected:   cel.StringType,
			compatible: true,
		},
		{
			name:       "int to int",
			output:     cel.IntType,
			expected:   cel.IntType,
			compatible: true,
		},
		{
			name:       "bool to bool",
			output:     cel.BoolType,
			expected:   cel.BoolType,
			compatible: true,
		},
		{
			name:       "double to double",
			output:     cel.DoubleType,
			expected:   cel.DoubleType,
			compatible: true,
		},
		{
			name:        "string to int",
			output:      cel.StringType,
			expected:    cel.IntType,
			compatible:  false,
			errContains: "kind mismatch",
		},
		{
			name:        "int to string",
			output:      cel.IntType,
			expected:    cel.StringType,
			compatible:  false,
			errContains: "kind mismatch",
		},
		{
			name:        "bool to string",
			output:      cel.BoolType,
			expected:    cel.StringType,
			compatible:  false,
			errContains: "kind mismatch",
		},
		{
			name:       "dyn to string",
			output:     cel.DynType,
			expected:   cel.StringType,
			compatible: true,
		},
		{
			name:       "string to dyn",
			output:     cel.StringType,
			expected:   cel.DynType,
			compatible: true,
		},
		{
			name:        "nil output type",
			output:      nil,
			expected:    cel.StringType,
			compatible:  false,
			errContains: "nil type",
		},
		{
			name:        "nil expected type",
			output:      cel.StringType,
			expected:    nil,
			compatible:  false,
			errContains: "nil type",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			compatible, err := AreTypesStructurallyCompatible(tt.output, tt.expected, nil)
			assert.Equal(t, tt.compatible, compatible)
			if tt.compatible {
				assert.NoError(t, err)
			} else {
				require.Error(t, err)
				if tt.errContains != "" {
					assert.Contains(t, err.Error(), tt.errContains)
				}
			}
		})
	}
}

func TestListTypes(t *testing.T) {
	tests := []struct {
		name        string
		output      *cel.Type
		expected    *cel.Type
		compatible  bool
		errContains string
	}{
		{
			name:       "[]string to []string",
			output:     cel.ListType(cel.StringType),
			expected:   cel.ListType(cel.StringType),
			compatible: true,
		},
		{
			name:       "[]int to []int",
			output:     cel.ListType(cel.IntType),
			expected:   cel.ListType(cel.IntType),
			compatible: true,
		},
		{
			name:        "[]string to []int",
			output:      cel.ListType(cel.StringType),
			expected:    cel.ListType(cel.IntType),
			compatible:  false,
			errContains: "list element type incompatible",
		},
		{
			name:        "[]string to string",
			output:      cel.ListType(cel.StringType),
			expected:    cel.StringType,
			compatible:  false,
			errContains: "kind mismatch",
		},
		{
			name:       "[]dyn to []string",
			output:     cel.ListType(cel.DynType),
			expected:   cel.ListType(cel.StringType),
			compatible: true,
		},
		{
			name:       "[][]string to [][]string",
			output:     cel.ListType(cel.ListType(cel.StringType)),
			expected:   cel.ListType(cel.ListType(cel.StringType)),
			compatible: true,
		},
		{
			name:        "[][]string to [][]int",
			output:      cel.ListType(cel.ListType(cel.StringType)),
			expected:    cel.ListType(cel.ListType(cel.IntType)),
			compatible:  false,
			errContains: "list element type incompatible",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			compatible, err := AreTypesStructurallyCompatible(tt.output, tt.expected, nil)
			assert.Equal(t, tt.compatible, compatible)
			if tt.compatible {
				assert.NoError(t, err)
			} else {
				require.Error(t, err)
				if tt.errContains != "" {
					assert.Contains(t, err.Error(), tt.errContains)
				}
			}
		})
	}
}

func TestMapTypes(t *testing.T) {
	tests := []struct {
		name        string
		output      *cel.Type
		expected    *cel.Type
		compatible  bool
		errContains string
	}{
		{
			name:       "map[string]string to map[string]string",
			output:     cel.MapType(cel.StringType, cel.StringType),
			expected:   cel.MapType(cel.StringType, cel.StringType),
			compatible: true,
		},
		{
			name:       "map[string]int to map[string]int",
			output:     cel.MapType(cel.StringType, cel.IntType),
			expected:   cel.MapType(cel.StringType, cel.IntType),
			compatible: true,
		},
		{
			name:        "map[string]string to map[string]int",
			output:      cel.MapType(cel.StringType, cel.StringType),
			expected:    cel.MapType(cel.StringType, cel.IntType),
			compatible:  false,
			errContains: "map value type incompatible",
		},
		{
			name:        "map[string]int to map[int]int",
			output:      cel.MapType(cel.StringType, cel.IntType),
			expected:    cel.MapType(cel.IntType, cel.IntType),
			compatible:  false,
			errContains: "map key type incompatible",
		},
		{
			name:        "map[string]string to string",
			output:      cel.MapType(cel.StringType, cel.StringType),
			expected:    cel.StringType,
			compatible:  false,
			errContains: "kind mismatch",
		},
		{
			name:       "map[string]dyn to map[string]string",
			output:     cel.MapType(cel.StringType, cel.DynType),
			expected:   cel.MapType(cel.StringType, cel.StringType),
			compatible: true,
		},
		{
			name:       "map[string]map[string]int to map[string]map[string]int",
			output:     cel.MapType(cel.StringType, cel.MapType(cel.StringType, cel.IntType)),
			expected:   cel.MapType(cel.StringType, cel.MapType(cel.StringType, cel.IntType)),
			compatible: true,
		},
		{
			name:        "map[string]map[string]int to map[string]map[string]string",
			output:      cel.MapType(cel.StringType, cel.MapType(cel.StringType, cel.IntType)),
			expected:    cel.MapType(cel.StringType, cel.MapType(cel.StringType, cel.StringType)),
			compatible:  false,
			errContains: "map value type incompatible",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			compatible, err := AreTypesStructurallyCompatible(tt.output, tt.expected, nil)
			assert.Equal(t, tt.compatible, compatible)
			if tt.compatible {
				assert.NoError(t, err)
			} else {
				require.Error(t, err)
				if tt.errContains != "" {
					assert.Contains(t, err.Error(), tt.errContains)
				}
			}
		})
	}
}

func TestStructTypes(t *testing.T) {
	personFields := map[string]*apiservercel.DeclField{
		"name":  apiservercel.NewDeclField("name", apiservercel.StringType, true, nil, nil),
		"age":   apiservercel.NewDeclField("age", apiservercel.IntType, false, nil, nil),
		"email": apiservercel.NewDeclField("email", apiservercel.StringType, false, nil, nil),
	}
	personType := apiservercel.NewObjectType(TypeNamePrefix+"person", personFields)

	personSubsetFields := map[string]*apiservercel.DeclField{
		"name": apiservercel.NewDeclField("name", apiservercel.StringType, true, nil, nil),
		"age":  apiservercel.NewDeclField("age", apiservercel.IntType, false, nil, nil),
	}
	personSubsetType := apiservercel.NewObjectType(TypeNamePrefix+"personSubset", personSubsetFields)

	personWithExtraFields := map[string]*apiservercel.DeclField{
		"name":       apiservercel.NewDeclField("name", apiservercel.StringType, true, nil, nil),
		"age":        apiservercel.NewDeclField("age", apiservercel.IntType, false, nil, nil),
		"email":      apiservercel.NewDeclField("email", apiservercel.StringType, false, nil, nil),
		"extraField": apiservercel.NewDeclField("extraField", apiservercel.StringType, false, nil, nil),
	}
	personWithExtraType := apiservercel.NewObjectType(TypeNamePrefix+"personWithExtra", personWithExtraFields)

	personWrongTypeFields := map[string]*apiservercel.DeclField{
		"name": apiservercel.NewDeclField("name", apiservercel.StringType, true, nil, nil),
		"age":  apiservercel.NewDeclField("age", apiservercel.StringType, false, nil, nil),
	}
	personWrongTypeType := apiservercel.NewObjectType(TypeNamePrefix+"personWrongType", personWrongTypeFields)

	provider := NewDeclTypeProvider(personType, personSubsetType, personWithExtraType, personWrongTypeType)

	tests := []struct {
		name        string
		output      *cel.Type
		expected    *cel.Type
		compatible  bool
		errContains string
	}{
		{
			name:       "identical structs",
			output:     personType.CelType(),
			expected:   personType.CelType(),
			compatible: true,
		},
		{
			name:       "subset struct",
			output:     personSubsetType.CelType(),
			expected:   personType.CelType(),
			compatible: true,
		},
		{
			name:        "struct with extra field",
			output:      personWithExtraType.CelType(),
			expected:    personType.CelType(),
			compatible:  false,
			errContains: "exists in output but not in expected type",
		},
		{
			name:        "struct with wrong field type",
			output:      personWrongTypeType.CelType(),
			expected:    personType.CelType(),
			compatible:  false,
			errContains: "has incompatible type",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			compatible, err := AreTypesStructurallyCompatible(tt.output, tt.expected, provider)
			assert.Equal(t, tt.compatible, compatible)
			if tt.compatible {
				assert.NoError(t, err)
			} else {
				require.Error(t, err)
				if tt.errContains != "" {
					assert.Contains(t, err.Error(), tt.errContains)
				}
			}
		})
	}
}

func TestNestedTypes(t *testing.T) {
	addressFields := map[string]*apiservercel.DeclField{
		"street": apiservercel.NewDeclField("street", apiservercel.StringType, true, nil, nil),
		"city":   apiservercel.NewDeclField("city", apiservercel.StringType, true, nil, nil),
		"zip":    apiservercel.NewDeclField("zip", apiservercel.StringType, false, nil, nil),
	}
	addressType := apiservercel.NewObjectType(TypeNamePrefix+"address", addressFields)

	userFields := map[string]*apiservercel.DeclField{
		"name":    apiservercel.NewDeclField("name", apiservercel.StringType, true, nil, nil),
		"address": apiservercel.NewDeclField("address", addressType, false, nil, nil),
		"tags":    apiservercel.NewDeclField("tags", apiservercel.NewListType(apiservercel.StringType, -1), false, nil, nil),
	}
	userType := apiservercel.NewObjectType(TypeNamePrefix+"user", userFields)

	addressSubsetFields := map[string]*apiservercel.DeclField{
		"street": apiservercel.NewDeclField("street", apiservercel.StringType, true, nil, nil),
		"city":   apiservercel.NewDeclField("city", apiservercel.StringType, true, nil, nil),
	}
	addressSubsetType := apiservercel.NewObjectType(TypeNamePrefix+"addressSubset", addressSubsetFields)

	userSubsetFields := map[string]*apiservercel.DeclField{
		"name":    apiservercel.NewDeclField("name", apiservercel.StringType, true, nil, nil),
		"address": apiservercel.NewDeclField("address", addressSubsetType, false, nil, nil),
	}
	userSubsetType := apiservercel.NewObjectType(TypeNamePrefix+"userSubset", userSubsetFields)

	addressWrongTypeFields := map[string]*apiservercel.DeclField{
		"street": apiservercel.NewDeclField("street", apiservercel.IntType, true, nil, nil),
		"city":   apiservercel.NewDeclField("city", apiservercel.StringType, true, nil, nil),
	}
	addressWrongType := apiservercel.NewObjectType(TypeNamePrefix+"addressWrongType", addressWrongTypeFields)

	userWrongTypeFields := map[string]*apiservercel.DeclField{
		"name":    apiservercel.NewDeclField("name", apiservercel.StringType, true, nil, nil),
		"address": apiservercel.NewDeclField("address", addressWrongType, false, nil, nil),
	}
	userWrongType := apiservercel.NewObjectType(TypeNamePrefix+"userWrongType", userWrongTypeFields)

	provider := NewDeclTypeProvider(
		addressType, addressSubsetType, addressWrongType,
		userType, userSubsetType, userWrongType,
	)

	tests := []struct {
		name        string
		output      *cel.Type
		expected    *cel.Type
		compatible  bool
		errContains string
	}{
		{
			name:       "identical nested structs",
			output:     userType.CelType(),
			expected:   userType.CelType(),
			compatible: true,
		},
		{
			name:       "nested struct subset",
			output:     userSubsetType.CelType(),
			expected:   userType.CelType(),
			compatible: true,
		},
		{
			name:        "nested struct wrong type",
			output:      userWrongType.CelType(),
			expected:    userType.CelType(),
			compatible:  false,
			errContains: "has incompatible type",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			compatible, err := AreTypesStructurallyCompatible(tt.output, tt.expected, provider)
			assert.Equal(t, tt.compatible, compatible)
			if tt.compatible {
				assert.NoError(t, err)
			} else {
				require.Error(t, err)
				if tt.errContains != "" {
					assert.Contains(t, err.Error(), tt.errContains)
				}
			}
		})
	}
}
