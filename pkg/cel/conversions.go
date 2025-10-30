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
	"errors"
	"fmt"
	"reflect"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
)

var (
	// ErrUnsupportedType is returned when the type is not supported.
	ErrUnsupportedType = errors.New("unsupported type")
)

// GoNativeType transforms CEL output into corresponding Go types
func GoNativeType(v ref.Val) (interface{}, error) {
	switch v.Type() {
	case types.BoolType:
		return v.Value().(bool), nil
	case types.IntType:
		return v.Value().(int64), nil
	case types.UintType:
		return v.Value().(uint64), nil
	case types.DoubleType:
		return v.Value().(float64), nil
	case types.StringType:
		return v.Value().(string), nil
	case types.ListType:
		return v.ConvertToNative(reflect.TypeOf([]interface{}{}))
	case types.MapType:
		return v.ConvertToNative(reflect.TypeOf(map[string]interface{}{}))
	case types.OptionalType:
		opt := v.(*types.Optional)
		if !opt.HasValue() {
			return nil, nil
		}
		return GoNativeType(opt.GetValue())
	case types.NullType:
		return nil, nil
	default:
		// For types we can't convert, return as is with an error
		return v.Value(), fmt.Errorf("%w: %v", ErrUnsupportedType, v.Type())
	}
}

// IsBoolType checks if the given ref.Val is of type BoolType
func IsBoolType(v ref.Val) bool {
	return v.Type() == types.BoolType
}

// WouldMatchIfUnwrapped checks if outputType would be assignable to expectedType
// if we unwrapped the optional wrapper from outputType.
// This detects the case where outputType is optional_type(T) and expectedType is T.
func WouldMatchIfUnwrapped(outputType, expectedType *cel.Type) bool {
	if outputType == nil || expectedType == nil {
		return false
	}

	// If output is already assignable to expected, no mismatch
	if expectedType.IsAssignableType(outputType) {
		return false
	}

	// Check if wrapping expected in optional makes it match output
	// If yes, then output is an optional version of expected
	optionalExpected := cel.OptionalType(expectedType)
	return optionalExpected.IsAssignableType(outputType)
}

// IsBoolOrOptionalBool checks if a CEL type is bool or optional_type(bool).
// This is useful for validating condition expressions that must return boolean values.
func IsBoolOrOptionalBool(t *cel.Type) bool {
	// Check if it's a direct bool type
	// Note: A.IsAssignableType(B) means "A accepts B", so we check if bool accepts t
	if cel.BoolType.IsAssignableType(t) {
		return true
	}

	// Check if it's optional_type(bool)
	optionalBool := cel.OptionalType(cel.BoolType)
	return optionalBool.IsAssignableType(t)
}
