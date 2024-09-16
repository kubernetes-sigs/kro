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

package celutil

import (
	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/ext"
)

type EnvironementOptions struct {
	ResourceNames []string
}

func WithResourceNames(names []string) *EnvironementOptions {
	return &EnvironementOptions{
		ResourceNames: names,
	}
}

func NewEnvironement(options *EnvironementOptions) (*cel.Env, error) {
	declarations := make([]cel.EnvOption, 0, len(options.ResourceNames))
	for _, resourceName := range options.ResourceNames {
		declarations = append(declarations, cel.Variable(resourceName, cel.AnyType))
	}
	declarations = append(declarations, []cel.EnvOption{
		ext.Lists(),
		ext.Strings(),
	}...)
	env, err := cel.NewEnv(declarations...)
	return env, err
}
