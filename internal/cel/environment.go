// Copyright Amazon.com Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//     http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package cel

import (
	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/ext"
)

// EnvOption is a function that modifies the environment options.
type EnvOption func(*envOptions)

// envOptions holds all the configuration for the CEL environment.
type envOptions struct {
	// resourceNames will be converted to CEL variable declarations
	// of type 'any'.
	//
	// TODO(a-hilaly): Add support for custom types.
	resourceNames []string
	// customDeclarations will be added to the CEL environment.
	customDeclarations []cel.EnvOption
}

// WithResourceNames adds resource names that will be declared as CEL variables.
func WithResourceNames(names []string) EnvOption {
	return func(opts *envOptions) {
		opts.resourceNames = append(opts.resourceNames, names...)
	}
}

// WithCustomDeclarations adds custom declarations to the CEL environment.
func WithCustomDeclarations(declarations []cel.EnvOption) EnvOption {
	return func(opts *envOptions) {
		opts.customDeclarations = append(opts.customDeclarations, declarations...)
	}
}

// DefaultEnvironment returns the default CEL environment.
func DefaultEnvironment(options ...EnvOption) (*cel.Env, error) {
	opts := &envOptions{}
	for _, opt := range options {
		opt(opts)
	}

	declarations := []cel.EnvOption{
		// default stdlibs
		ext.Lists(),
		ext.Strings(),
	}

	for _, name := range opts.resourceNames {
		declarations = append(declarations, cel.Variable(name, cel.AnyType))
	}
	return cel.NewEnv(declarations...)
}
