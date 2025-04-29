// Copyright 2025 The Kube Resource Orchestrator Authors
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
	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/ext"

	"github.com/kro-run/kro/pkg/cel/library"
)

// EnvOption is a function that modifies the environment options.
type EnvOption func(*envOptions)

// envOptions holds all the configuration for the CEL environment.
type envOptions struct {
	// resourceIDs will be converted to CEL variable declarations
	// of type 'any'.
	//
	// TODO(a-hilaly): Add support for custom types.
	resourceIDs []string
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

// DefaultEnvironment returns the default CEL environment.
func DefaultEnvironment(options ...EnvOption) (*cel.Env, error) {
	declarations := []cel.EnvOption{
		ext.Lists(),
		ext.Strings(),
		library.Random(),
	}

	opts := &envOptions{}
	for _, opt := range options {
		opt(opts)
	}

	declarations = append(declarations, opts.customDeclarations...)

	for _, name := range opts.resourceIDs {
		declarations = append(declarations, cel.Variable(name, cel.AnyType))
	}

	return cel.NewEnv(declarations...)
}
