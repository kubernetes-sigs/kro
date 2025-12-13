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
	"maps"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/ext"
	apiservercel "k8s.io/apiserver/pkg/cel"
	k8scellib "k8s.io/apiserver/pkg/cel/library"
	"k8s.io/apiserver/pkg/cel/openapi"
	"k8s.io/kube-openapi/pkg/validation/spec"

	"github.com/kubernetes-sigs/kro/pkg/cel/library"
)

// EnvOption is a function that modifies the environment options.
type EnvOption func(*envOptions)

// envOptions holds all the configuration for the CEL environment.
type envOptions struct {
	// resourceIDs will be converted to CEL variable declarations
	// of type 'any'.
	resourceIDs []string
	// typedResources maps resource names to their OpenAPI schemas.
	// These will be converted to typed CEL variables with field-level
	// type checking enabled.
	//
	// Note that there is not a 1:1 mapping between CEL types and OpenAPI
	// schemas. This is best effort conversion to enable type checking
	// for field access in CEL expressions.
	//
	// Native CEL types (like int, bool, list, map) will be used where
	// possible. OpenAPI's AnyOf, OneOf, and VendorExtensions features like
	// x-kubernetes-int-or-string will fall back to dyn or any type.
	typedResources map[string]*spec.Schema
	// customDeclarations will be added to the CEL environment.
	customDeclarations []cel.EnvOption
}

// WithResourceIDs adds resource ids that will be declared as CEL variables.
func WithResourceIDs(ids []string) EnvOption {
	return func(opts *envOptions) {
		opts.resourceIDs = append(opts.resourceIDs, ids...)
	}
}

// WithCustomDeclarations adds custom declarations to the CEL environment.
func WithCustomDeclarations(declarations []cel.EnvOption) EnvOption {
	return func(opts *envOptions) {
		opts.customDeclarations = append(opts.customDeclarations, declarations...)
	}
}

// WithTypedResources adds typed resource declarations to the CEL environment.
// This enables compile time type checking for field access in CEL expressions.
func WithTypedResources(schemas map[string]*spec.Schema) EnvOption {
	return func(opts *envOptions) {
		if opts.typedResources == nil {
			opts.typedResources = schemas
		} else {
			maps.Copy(opts.typedResources, schemas)
		}
	}
}

// DefaultEnvironment returns the default CEL environment.
func DefaultEnvironment(options ...EnvOption) (*cel.Env, error) {
	declarations := []cel.EnvOption{
		ext.Lists(),
		ext.Strings(),
		cel.OptionalTypes(),
		ext.Encoders(),
		// Kubernetes CEL libraries: enable url(), getHost(), regex helpers, etc.
		// See https://kubernetes.io/docs/reference/using-api/cel/ and
		// https://github.com/kubernetes-sigs/kro/issues/880.
		k8scellib.URLs(),
		k8scellib.Regex(),
		library.Random(),
	}

	opts := &envOptions{}
	for _, opt := range options {
		opt(opts)
	}

	declarations = append(declarations, opts.customDeclarations...)

	if len(opts.typedResources) > 0 {
		// We need both a TypeProvider (for field resolution) and variable declarations.
		// To avoid conflicts, we use different names for types vs variables:
		//  - Types are registered with TypeNamePrefix + "<name>" (e.g "__type_schema")
		//  - Variables use the original names (e.g "pod", "schema"...)

		declTypes := make([]*apiservercel.DeclType, 0, len(opts.typedResources))

		for name, schema := range opts.typedResources {
			declType := SchemaDeclTypeWithMetadata(&openapi.Schema{Schema: schema}, false)
			if declType != nil {
				typeName := TypeNamePrefix + name
				declType = declType.MaybeAssignTypeName(typeName)

				// add type declaration
				declTypes = append(declTypes, declType)

				celType := declType.CelType()

				// Add variable declaration
				declarations = append(declarations, cel.Variable(name, celType))
			}
		}

		if len(declTypes) > 0 {
			baseProvider := NewDeclTypeProvider(declTypes...)
			// Enable recognition of CEL reserved keywords as field names
			baseProvider.SetRecognizeKeywordAsFieldName(true)

			registry := types.NewEmptyRegistry()
			wrappedProvider, err := baseProvider.WithTypeProvider(registry)
			if err != nil {
				return nil, err
			}

			declarations = append(declarations, cel.CustomTypeProvider(wrappedProvider))
		}
	}

	for _, name := range opts.resourceIDs {
		declarations = append(declarations, cel.Variable(name, cel.AnyType))
	}

	return cel.NewEnv(declarations...)
}

// TypedEnvironment creates a CEL environment with type checking enabled.
//
// This should be used during RGD build time (pkg/graph.Builder) to validate
// CEL expressions against OpenAPI schemas.
func TypedEnvironment(schemas map[string]*spec.Schema) (*cel.Env, error) {
	return DefaultEnvironment(WithTypedResources(schemas))
}

// UntypedEnvironment creates a CEL environment without type declarations.
//
// This is theoretically cheaper to use as there are no Schema conversions
// required. NOTE(a-hilaly): maybe use this for runtime? undecided.
func UntypedEnvironment(resourceIDs []string) (*cel.Env, error) {
	return DefaultEnvironment(WithResourceIDs(resourceIDs))
}

// CreateDeclTypeProvider creates a DeclTypeProvider from OpenAPI schemas.
// This is used for deep introspection of type structures when generating schemas.
// The provider maps CEL type names to their full DeclType definitions with all fields.
func CreateDeclTypeProvider(schemas map[string]*spec.Schema) *DeclTypeProvider {
	if len(schemas) == 0 {
		return nil
	}

	declTypes := make([]*apiservercel.DeclType, 0, len(schemas))
	for name, schema := range schemas {
		declType := SchemaDeclTypeWithMetadata(&openapi.Schema{Schema: schema}, false)
		if declType != nil {
			declType = declType.MaybeAssignTypeName(name)
			declTypes = append(declTypes, declType)
		}
	}

	if len(declTypes) == 0 {
		return nil
	}

	provider := NewDeclTypeProvider(declTypes...)
	// Enable recognition of CEL reserved keywords as field names.
	// This allows users to write "schema.metadata.namespace" instead of "schema.metadata.__namespace__"
	provider.SetRecognizeKeywordAsFieldName(true)
	return provider
}
