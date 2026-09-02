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
	// runtimeLibrary gates library.Runtime() and the ConditionTypeProvider
	// wrap. Nil means default-on.
	runtimeLibrary *bool
}

// runtimeEnabled reports whether the Runtime library (and its
// ConditionTypeProvider wrap) should be installed. Default is on.
func (o *envOptions) runtimeEnabled() bool {
	return o.runtimeLibrary == nil || *o.runtimeLibrary
}

// WithRuntimeLibrary gates the `runtime` CEL library and the
// ConditionTypeProvider wrap that resolves kro.run.Condition. Default is
// on; environments with no author-condition surface pass false to omit it.
func WithRuntimeLibrary(enabled bool) EnvOption {
	return func(opts *envOptions) {
		opts.runtimeLibrary = &enabled
	}
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
	baseDeclarationsOnce   [2]sync.Once
	cachedBaseDeclarations [2][]cel.EnvOption
)

func baseDeclarationsIndex(includeRuntime bool) int {
	if includeRuntime {
		return 1
	}
	return 0
}

// coreDeclarations is the Runtime-independent set of CEL environment options.
func coreDeclarations() []cel.EnvOption {
	return []cel.EnvOption{
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
}

// BaseDeclarations returns the base CEL environment options shared by all kro
// CEL environments. Includes list/string extensions, optional types, encoders,
// and Kubernetes CEL libraries (URLs, Regex, Random).
// The result is cached via sync.Once since these options are stateless.
//
// The default includes library.Runtime(); pass WithRuntimeLibrary(false) to
// omit it when there is no author-condition surface.
func BaseDeclarations(options ...EnvOption) []cel.EnvOption {
	opts := &envOptions{}
	for _, opt := range options {
		opt(opts)
	}
	includeRuntime := opts.runtimeEnabled()
	idx := baseDeclarationsIndex(includeRuntime)
	baseDeclarationsOnce[idx].Do(func() {
		decls := coreDeclarations()
		if includeRuntime {
			// Runtime() registers the `runtime` CEL variable used to author
			// custom status conditions. The graph builder rejects it outside
			// the schema's status.conditions block.
			decls = append(decls, library.Runtime())
		}
		cachedBaseDeclarations[idx] = decls
	})
	return cachedBaseDeclarations[idx]
}

var (
	baseEnvOnce   [2]sync.Once
	cachedBaseEnv [2]*cel.Env
	baseEnvErr    [2]error
)

// baseEnv returns a cached base CEL environment containing only the base
// declarations. Use env.Extend() on the result to add custom declarations,
// which is cheaper than building a full environment from scratch.
func baseEnv(includeRuntime bool) (*cel.Env, error) {
	idx := baseDeclarationsIndex(includeRuntime)
	baseEnvOnce[idx].Do(func() {
		var decls []cel.EnvOption
		if includeRuntime {
			decls = BaseDeclarations()
		} else {
			decls = BaseDeclarations(WithRuntimeLibrary(false))
		}
		cachedBaseEnv[idx], baseEnvErr[idx] = cel.NewEnv(decls...)
	})
	return cachedBaseEnv[idx], baseEnvErr[idx]
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
	opts := &envOptions{}
	for _, opt := range options {
		opt(opts)
	}

	includeRuntime := opts.runtimeEnabled()
	base, err := baseEnv(includeRuntime)
	if err != nil {
		return nil, nil, fmt.Errorf("base environment: %w", err)
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
	if err != nil {
		return nil, nil, err
	}

	// Wrap the resolved type provider so the type-checker can resolve the
	// kro.run.Condition type returned by runtime.newCondition / condition and
	// its fields. This must run after the typed-resource provider above is
	// installed, since cel.CustomTypeProvider replaces (not layers) the
	// provider; ConditionTypeProvider delegates everything else back to it.
	// Skipped when the runtime library is omitted.
	if includeRuntime {
		env, err = env.Extend(cel.CustomTypeProvider(library.ConditionTypeProvider(env.CELTypeProvider())))
	}
	return env, provider, err
}

// TypedEnvironmentWithProvider creates a typed CEL environment.
// It returns both the environment and the DeclTypeProvider.
func TypedEnvironmentWithProvider(schemas map[string]*spec.Schema, options ...EnvOption) (*cel.Env, *DeclTypeProvider, error) {
	opts := append([]EnvOption{WithTypedResources(schemas)}, options...)
	return defaultEnvironment(opts...)
}

// TypedEnvironmentWithIDsAndProvider builds the typed CEL environment with
// the supplied typed resources, additionally declaring the given identifiers
// as untyped (dyn) variables. Useful when some identifiers carry no schema
// but must still resolve at type-check time.
func TypedEnvironmentWithIDsAndProvider(schemas map[string]*spec.Schema, dynIDs []string, options ...EnvOption) (*cel.Env, *DeclTypeProvider, error) {
	opts := append([]EnvOption{WithTypedResources(schemas), WithResourceIDs(dynIDs)}, options...)
	return defaultEnvironment(opts...)
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
