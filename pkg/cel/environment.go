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
	"maps"
	"sync"

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

// WithListVariables adds list-typed variable declarations to the CEL environment.
// Used for collection resources so they support list operations/macros like all()
// exists(), filter(), and map() etc...
func WithListVariables(names []string) EnvOption {
	return func(opts *envOptions) {
		for _, name := range names {
			opts.customDeclarations = append(opts.customDeclarations, cel.Variable(name, cel.ListType(cel.DynType)))
		}
	}
}

var (
	baseDeclarationsOnce   sync.Once
	cachedBaseDeclarations []cel.EnvOption
)

// BaseDeclarations returns the base CEL environment options shared by all kro
// CEL environments. Includes list/string extensions, optional types, encoders,
// and Kubernetes CEL libraries (URLs, Regex, Random).
// The result is cached via sync.Once since these options are stateless.
func BaseDeclarations() []cel.EnvOption {
	baseDeclarationsOnce.Do(func() {
		cachedBaseDeclarations = []cel.EnvOption{
			ext.TwoVarComprehensions(),
			ext.Lists(),
			ext.Strings(),
			ext.Bindings(),
			cel.OptionalTypes(),
			ext.Encoders(),
			// Kubernetes CEL libraries: url(), regex, quantity, ip(), cidr(), semver(), etc.
			// See https://kubernetes.io/docs/reference/using-api/cel/ and
			// https://github.com/kubernetes-sigs/kro/issues/880.
			k8scellib.Lists(),
			k8scellib.URLs(),
			k8scellib.Regex(),
			k8scellib.Quantity(),
			k8scellib.IP(),
			k8scellib.CIDR(),
			k8scellib.SemverLib(),
			library.Random(),
			library.Maps(),
			library.JSON(),
			library.Hash(),
			library.Lists(),
			// Omit() is registered globally so CEL can parse and type-check it
			// everywhere. The graph builder rejects it in restricted contexts
			// (includeWhen, readyWhen, forEach) via inspectExpressionRestricted
			// and validateAndCompileForEach.
			library.Omit(),
		}
	})
	return cachedBaseDeclarations
}

var (
	baseEnvOnce   sync.Once
	cachedBaseEnv *cel.Env
	baseEnvErr    error
)

// baseEnv returns a cached base CEL environment containing only the base
// declarations. Use env.Extend() on the result to add custom declarations,
// which is cheaper than building a full environment from scratch.
func baseEnv() (*cel.Env, error) {
	baseEnvOnce.Do(func() {
		cachedBaseEnv, baseEnvErr = cel.NewEnv(BaseDeclarations()...)
	})
	return cachedBaseEnv, baseEnvErr
}

// DefaultEnvironment returns the default CEL environment.
func DefaultEnvironment(options ...EnvOption) (*cel.Env, error) {
	env, _, err := defaultEnvironment(options...)
	return env, err
}

// defaultEnvironment is the shared implementation that builds the CEL environment
// and returns both the environment and the DeclTypeProvider (if typed resources
// were configured).
func defaultEnvironment(options ...EnvOption) (*cel.Env, *DeclTypeProvider, error) {
	base, err := baseEnv()
	if err != nil {
		return nil, nil, fmt.Errorf("base environment: %w", err)
	}

	opts := &envOptions{}
	for _, opt := range options {
		opt(opts)
	}

	// Only non-base declarations go here; base declarations are in the cached base env.
	var declarations []cel.EnvOption
	declarations = append(declarations, opts.customDeclarations...)

	var provider *DeclTypeProvider

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
			provider = NewDeclTypeProvider(declTypes...)
			// Enable recognition of CEL reserved keywords as field names
			provider.SetRecognizeKeywordAsFieldName(true)

			registry := types.NewEmptyRegistry()
			wrappedProvider, err := provider.WithTypeProvider(registry)
			if err != nil {
				return nil, nil, err
			}

			declarations = append(declarations, cel.CustomTypeProvider(wrappedProvider))
		}
	}

	for _, name := range opts.resourceIDs {
		declarations = append(declarations, cel.Variable(name, cel.AnyType))
	}

	env, err := base.Extend(declarations...)
	return env, provider, err
}

// TypedEnvironmentWithProvider creates a typed CEL environment.
// It returns both the environment and the DeclTypeProvider.
func TypedEnvironmentWithProvider(schemas map[string]*spec.Schema) (*cel.Env, *DeclTypeProvider, error) {
	return defaultEnvironment(WithTypedResources(schemas))
}

// TypedEnvironment creates a CEL environment with type checking enabled.
//
// This should be used during RGD build time (pkg/graph.Builder) to validate
// CEL expressions against OpenAPI schemas.
func TypedEnvironment(schemas map[string]*spec.Schema) (*cel.Env, error) {
	return DefaultEnvironment(WithTypedResources(schemas))
}

// ListElementType extracts the element type from a CEL list type.
// Returns the element type if the input is a list type, or an error otherwise.
// This is useful for inferring the type of forEach iterator variables from
// the forEach expression's return type.
func ListElementType(listType *cel.Type) (*cel.Type, error) {
	params := listType.Parameters()
	if len(params) != 1 {
		return nil, fmt.Errorf("type %q is not a list type", listType.String())
	}
	// Verify it's actually a list by checking if list(elemType) matches
	elemType := params[0]
	if cel.ListType(elemType).IsAssignableType(listType) {
		return elemType, nil
	}
	return nil, fmt.Errorf("type %q is not a list type", listType.String())
}
