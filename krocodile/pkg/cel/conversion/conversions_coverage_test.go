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

package conversion

import (
	"reflect"
	"testing"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"sigs.k8s.io/krocodile/pkg/cel/sentinels"
)

// evalExpr compiles and evaluates a CEL expression returning the ref.Val.
func evalExpr(t *testing.T, expr string) ref.Val {
	t.Helper()
	env, err := cel.NewEnv()
	require.NoError(t, err)
	ast, issues := env.Compile(expr)
	require.NoError(t, issues.Err())
	prog, err := env.Program(ast)
	require.NoError(t, err)
	val, _, err := prog.Eval(map[string]interface{}{})
	require.NoError(t, err)
	return val
}

func TestGoNativeType_Scalars(t *testing.T) {
	cases := []struct {
		name string
		val  ref.Val
		want interface{}
	}{
		{name: "nil ref.Val", val: nil, want: nil},
		{name: "bool", val: evalExpr(t, `true`), want: true},
		{name: "int", val: evalExpr(t, `int(42)`), want: int64(42)},
		{name: "uint", val: evalExpr(t, `uint(7)`), want: uint64(7)},
		{name: "double", val: evalExpr(t, `3.5`), want: float64(3.5)},
		{name: "string", val: evalExpr(t, `"hi"`), want: "hi"},
		{name: "null", val: types.NullValue, want: nil},
		{name: "optional empty", val: types.OptionalNone, want: nil},
		{name: "optional with value", val: types.OptionalOf(types.String("x")), want: "x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := GoNativeType(tc.val)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// omitTestVal is a minimal ref.Val whose Value() returns the omit sentinel
// and whose Type() is unrecognized by GoNativeType, exercising the default
// branch that special-cases the omit sentinel.
type omitTestVal struct{}

func (omitTestVal) ConvertToNative(reflect.Type) (any, error) { return sentinels.Omit{}, nil }
func (omitTestVal) ConvertToType(ref.Type) ref.Val            { return types.NewErr("nope") }
func (omitTestVal) Equal(ref.Val) ref.Val                     { return types.NewErr("nope") }
func (omitTestVal) Type() ref.Type                            { return types.NewObjectType("kro.omit") }
func (omitTestVal) Value() any                                { return sentinels.Omit{} }

func TestGoNativeType_OmitSentinel(t *testing.T) {
	got, err := GoNativeType(omitTestVal{})
	require.NoError(t, err)
	assert.Equal(t, sentinels.Omit{}, got)
}

func TestGoNativeType_UnsupportedType(t *testing.T) {
	// A type that GoNativeType doesn't recognize and isn't the omit
	// sentinel falls through to the error branch.
	got, err := GoNativeType(types.NewErr("boom"))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUnsupportedType)
	_ = got
}

func TestGoNativeType_MapKeyAndValue(t *testing.T) {
	cases := []struct {
		name    string
		expr    string
		wantErr bool
	}{
		{name: "string keyed map", expr: `{"a": 1, "b": 2}`},
		{name: "nested list value", expr: `{"items": [1, 2, 3]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			val := evalExpr(t, tc.expr)
			got, err := GoNativeType(val)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			_, ok := got.(map[string]interface{})
			assert.True(t, ok, "expected map[string]interface{}, got %T", got)
		})
	}
}

func TestConvertMap_NonStringKey(t *testing.T) {
	// A map keyed by a non-string type triggers the "map key must be string"
	// error path in convertMap.
	reg := types.NewEmptyRegistry()
	val := reg.NativeToValue(map[int64]int64{1: 2})
	require.Equal(t, types.MapType, val.Type())

	_, err := GoNativeType(val)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "map key must be string")
}

func TestConvertList_RawSlice(t *testing.T) {
	// A raw Go slice wrapped via the registry is a Lister and should round-trip
	// through convertList.
	reg := types.NewEmptyRegistry()
	val := reg.NativeToValue([]interface{}{int64(1), "two", true})
	require.Equal(t, types.ListType, val.Type())

	got, err := GoNativeType(val)
	require.NoError(t, err)
	list, ok := got.([]interface{})
	require.True(t, ok, "expected []interface{}, got %T", got)
	assert.Equal(t, []interface{}{int64(1), "two", true}, list)
}

func TestIsBoolType(t *testing.T) {
	cases := []struct {
		name string
		val  ref.Val
		want bool
	}{
		{name: "bool true", val: types.Bool(true), want: true},
		{name: "int is not bool", val: types.Int(1), want: false},
		{name: "string is not bool", val: types.String("x"), want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, IsBoolType(tc.val))
		})
	}
}

func TestWouldMatchIfUnwrapped(t *testing.T) {
	cases := []struct {
		name     string
		output   *cel.Type
		expected *cel.Type
		want     bool
	}{
		{name: "nil output", output: nil, expected: cel.BoolType, want: false},
		{name: "nil expected", output: cel.BoolType, expected: nil, want: false},
		{name: "already assignable", output: cel.BoolType, expected: cel.BoolType, want: false},
		{name: "optional of expected matches output", output: cel.OptionalType(cel.BoolType), expected: cel.BoolType, want: true},
		{name: "unrelated types", output: cel.StringType, expected: cel.BoolType, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, WouldMatchIfUnwrapped(tc.output, tc.expected))
		})
	}
}

func TestIsBoolOrOptionalBool(t *testing.T) {
	cases := []struct {
		name string
		typ  *cel.Type
		want bool
	}{
		{name: "bool", typ: cel.BoolType, want: true},
		{name: "optional bool", typ: cel.OptionalType(cel.BoolType), want: true},
		{name: "string", typ: cel.StringType, want: false},
		{name: "optional string", typ: cel.OptionalType(cel.StringType), want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, IsBoolOrOptionalBool(tc.typ))
		})
	}
}
