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

package library

import (
	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/interpreter"
)

func Exist() cel.EnvOption {
	return cel.Lib(&existLibrary{})
}

type existLibrary struct{}

func (l *existLibrary) LibraryName() string {
	return "exist"
}

func (l *existLibrary) CompileOptions() []cel.EnvOption {
	return []cel.EnvOption{
		cel.Function("exist",
			cel.Overload("exist_string",
				[]*cel.Type{cel.StringType},
				cel.BoolType,
				cel.UnaryBinding(func(arg ref.Val) ref.Val {
					return types.Bool(true)
				}),
			),
		),
	}
}

func (l *existLibrary) ProgramOptions() []cel.ProgramOption {
	return []cel.ProgramOption{
		cel.CustomDecorator(existDecorator),
	}
}

func existDecorator(interp interpreter.Interpretable) (interpreter.Interpretable, error) {
	call, isCall := interp.(interpreter.InterpretableCall)
	if !isCall || call.Function() != "exist" {
		return interp, nil
	}

	return &existInterpretable{
		InterpretableCall: call,
	}, nil
}

type existInterpretable struct {
	interpreter.InterpretableCall
}

func (ei *existInterpretable) Eval(activation interpreter.Activation) ref.Val {
	args := ei.Args()
	if len(args) != 1 {
		return types.NewErr("exist() requires exactly one argument")
	}

	resourceNameVal := args[0].Eval(activation)
	if types.IsError(resourceNameVal) {
		return resourceNameVal
	}

	if resourceNameVal.Type() != types.StringType {
		return types.NewErr("exist() argument must be a string")
	}

	resourceName := string(resourceNameVal.(types.String))

	val, found := activation.ResolveName(resourceName)
	if !found {
		return types.Bool(false)
	}

	if val == nil {
		return types.Bool(false)
	}

	if refVal, ok := val.(ref.Val); ok {
		if types.IsError(refVal) {
			return types.Bool(false)
		}
	}

	return types.Bool(true)
}
