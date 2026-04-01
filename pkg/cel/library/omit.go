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
	"fmt"
	"reflect"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"

	"github.com/kubernetes-sigs/kro/pkg/cel/sentinels"
)

// omitVal is the CEL ref.Val wrapper for the omit sentinel.
type omitVal struct{}

// singleton — there's only one omit sentinel.
var omitInstance = &omitVal{}

func (v *omitVal) ConvertToNative(typeDesc reflect.Type) (any, error) {
	if typeDesc == reflect.TypeOf(sentinels.Omit{}) {
		return sentinels.Omit{}, nil
	}
	if typeDesc.Kind() == reflect.Interface {
		return sentinels.Omit{}, nil
	}
	return nil, fmt.Errorf("unsupported native conversion from omit to %v", typeDesc)
}

func (v *omitVal) ConvertToType(typeVal ref.Type) ref.Val {
	if typeVal == types.TypeType {
		return types.NewObjectType("kro.omit")
	}
	return types.NewErr("unsupported conversion from omit to %v", typeVal)
}

func (v *omitVal) Equal(other ref.Val) ref.Val {
	return types.NewErr("omit() is a field-removal sentinel and cannot be compared")
}

func (v *omitVal) Type() ref.Type {
	return types.NewObjectType("kro.omit")
}

func (v *omitVal) Value() any {
	return sentinels.Omit{}
}

// Omit returns a cel.EnvOption that registers the omit() function.
//
// omit() is a zero-argument function that returns a sentinel value.
// When the resolver encounters this sentinel as a field's resolved value,
// it removes the field (or array element) from the rendered object instead
// of writing it.
//
// omit() is only valid in standalone template field expressions.
// It must not be used in includeWhen, readyWhen, forEach, or string
// template fragments.
//
// Example usage:
//
//	policy: ${schema.spec.policy != "" ? schema.spec.policy : omit()}
//	scaling: ${schema.spec.autoscaling.enabled ? schema.spec.scaling : omit()}
func Omit() cel.EnvOption {
	return cel.Lib(&omitLib{})
}

type omitLib struct{}

func (l *omitLib) LibraryName() string {
	return "kro.omit"
}

func (l *omitLib) CompileOptions() []cel.EnvOption {
	return []cel.EnvOption{
		cel.Function("omit",
			cel.Overload("omit_void",
				[]*cel.Type{},
				cel.DynType,
				cel.FunctionBinding(func(args ...ref.Val) ref.Val {
					return omitInstance
				}),
			),
		),
	}
}

func (l *omitLib) ProgramOptions() []cel.ProgramOption {
	return nil
}
