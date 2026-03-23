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
	"encoding/json"
	"math"
	"reflect"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"

	"github.com/kubernetes-sigs/kro/pkg/cel/conversion"
)

// JSON returns a CEL library that provides JSON parsing functions.
//
// Library functions:
//
// json.unmarshal() parses a JSON string and returns the parsed value.
//
// The function takes one argument:
// - jsonString: a string containing JSON
//
// Returns a dynamic type that can be:
// - map for JSON objects
// - list for JSON arrays
// - string, int, double, bool for JSON primitives
// - null for JSON null
//
// Example usage:
//
//	json.unmarshal('{"name": "test"}').name
//	json.unmarshal('[1,2,3]')[0]
//	json.unmarshal(schema.spec.config).enabled
//
// json.marshal() converts a CEL value to a JSON string.
//
// The function takes one argument:
// - value: any CEL value (map, list, string, number, bool, null)
//
// Returns a JSON string representation of the value.
//
// Example usage:
//
//	json.marshal({"name": "test", "count": 42})
//	json.marshal([1, 2, 3])
//	json.marshal(schema.spec.config)
func JSON(options ...JSONOption) cel.EnvOption {
	lib := &jsonLibrary{version: math.MaxUint32}
	for _, o := range options {
		lib = o(lib)
	}
	return cel.Lib(lib)
}

type jsonLibrary struct {
	version uint32
}

// JSONOption is a functional option for configuring the json library.
type JSONOption func(*jsonLibrary) *jsonLibrary

// JSONVersion configures the version of the json library.
func JSONVersion(version uint32) JSONOption {
	return func(lib *jsonLibrary) *jsonLibrary {
		lib.version = version
		return lib
	}
}

func (l *jsonLibrary) LibraryName() string {
	return "json"
}

func (l *jsonLibrary) CompileOptions() []cel.EnvOption {
	return []cel.EnvOption{
		cel.Function("json.unmarshal",
			cel.Overload("json.unmarshal_string",
				[]*cel.Type{cel.StringType},
				cel.DynType,
				cel.UnaryBinding(unmarshalJSON),
			),
		),
		cel.Function("json.marshal",
			cel.Overload("json.marshal_dyn",
				[]*cel.Type{cel.DynType},
				cel.StringType,
				cel.UnaryBinding(marshalJSON),
			),
		),
	}
}

func (l *jsonLibrary) ProgramOptions() []cel.ProgramOption {
	return nil
}

func unmarshalJSON(jsonString ref.Val) ref.Val {
	native, err := jsonString.ConvertToNative(reflect.TypeOf(""))
	if err != nil {
		return types.NewErr("json.unmarshal argument must be a string")
	}

	str, ok := native.(string)
	if !ok {
		return types.NewErr("json.unmarshal argument must be a string")
	}

	var result interface{}
	if err := json.Unmarshal([]byte(str), &result); err != nil {
		return types.NewErr("json.unmarshal failed to parse JSON: %s", err.Error())
	}

	return types.DefaultTypeAdapter.NativeToValue(result)
}

func marshalJSON(value ref.Val) ref.Val {
	native, err := conversion.GoNativeType(value)
	if err != nil {
		return types.NewErr("json.marshal failed to convert value: %s", err.Error())
	}

	jsonBytes, err := json.Marshal(native)
	if err != nil {
		return types.NewErr("json.marshal failed to encode value: %s", err.Error())
	}

	return types.String(string(jsonBytes))
}
