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
	"reflect"
	"testing"

	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"sigs.k8s.io/krocodile/pkg/cel/sentinels"
)

// TestLibraryVersionOptions covers the functional version options for each
// library, which are otherwise only reachable when callers opt into versioning.
func TestLibraryVersionOptions(t *testing.T) {
	t.Run("ListsVersion", func(t *testing.T) {
		lib := &listsLibrary{}
		ListsVersion(3)(lib)
		assert.Equal(t, uint32(3), lib.version)
	})
	t.Run("MapsVersion", func(t *testing.T) {
		lib := &mapsLib{}
		MapsVersion(5)(lib)
		assert.Equal(t, uint32(5), lib.version)
	})
	t.Run("JSONVersion", func(t *testing.T) {
		lib := &jsonLibrary{}
		JSONVersion(2)(lib)
		assert.Equal(t, uint32(2), lib.version)
	})
}

// TestLibraryConstructorsWithOptions exercises the option-application loop in
// each library constructor (otherwise only the zero-option path is covered).
func TestLibraryConstructorsWithOptions(t *testing.T) {
	cases := []struct {
		name string
		opt  func()
	}{
		{name: "Hash", opt: func() { _ = Hash(HashVersion(1)) }},
		{name: "JSON", opt: func() { _ = JSON(JSONVersion(1)) }},
		{name: "Lists", opt: func() { _ = Lists(ListsVersion(1)) }},
		{name: "Maps", opt: func() { _ = Maps(MapsVersion(1)) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.NotPanics(t, tc.opt)
		})
	}
}

// TestOmitVal covers the ref.Val methods on the omit sentinel wrapper directly,
// including the non-default conversion branches that CEL never exercises.
func TestOmitVal_ConvertToNative(t *testing.T) {
	cases := []struct {
		name    string
		target  reflect.Type
		wantErr bool
	}{
		{name: "to Omit struct", target: reflect.TypeOf(sentinels.Omit{})},
		{name: "to interface", target: reflect.TypeOf((*interface{})(nil)).Elem()},
		{name: "to string errors", target: reflect.TypeOf(""), wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := omitInstance.ConvertToNative(tc.target)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, sentinels.Omit{}, got)
		})
	}
}

func TestOmitVal_ConvertToType(t *testing.T) {
	cases := []struct {
		name    string
		typ     ref.Type
		wantErr bool
	}{
		{name: "to type yields object type", typ: types.TypeType},
		{name: "to string errors", typ: types.StringType, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := omitInstance.ConvertToType(tc.typ)
			if tc.wantErr {
				assert.True(t, types.IsError(got))
				return
			}
			assert.False(t, types.IsError(got))
		})
	}
}

func TestOmitVal_EqualAndAccessors(t *testing.T) {
	// Equal always returns an error (sentinels are not comparable).
	assert.True(t, types.IsError(omitInstance.Equal(omitInstance)))
	// Type and Value accessors.
	assert.Equal(t, types.NewObjectType("kro.omit"), omitInstance.Type())
	assert.Equal(t, sentinels.Omit{}, omitInstance.Value())
}

// TestHashBindings_Errors covers the ConvertToNative/type-assertion error
// branches of the hash binding functions by invoking them directly with a
// non-string ref.Val.
func TestHashBindings_Errors(t *testing.T) {
	cases := []struct {
		name string
		fn   func(ref.Val) ref.Val
	}{
		{name: "fnv64a", fn: fnv64aHash},
		{name: "sha256", fn: sha256Hash},
		{name: "md5", fn: md5Hash},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// An Int has no native string conversion -> error branch.
			assert.True(t, types.IsError(tc.fn(types.Int(5))))
		})
	}
}

// TestUnmarshalJSON_Errors covers the non-string and bad-JSON error branches.
func TestUnmarshalJSON_Errors(t *testing.T) {
	cases := []struct {
		name string
		in   ref.Val
	}{
		{name: "non-string arg", in: types.Int(5)},
		{name: "invalid json", in: types.String("{not json")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.True(t, types.IsError(unmarshalJSON(tc.in)))
		})
	}
}

// errVal is a ref.Val whose Value() is unsupported by GoNativeType so that
// marshalJSON's conversion-error branch is exercised.
type unmarshalableVal struct{}

func (unmarshalableVal) ConvertToNative(reflect.Type) (any, error) { return nil, nil }
func (unmarshalableVal) ConvertToType(ref.Type) ref.Val            { return types.NewErr("nope") }
func (unmarshalableVal) Equal(ref.Val) ref.Val                     { return types.NewErr("nope") }
func (unmarshalableVal) Type() ref.Type                            { return types.NewObjectType("weird") }
func (unmarshalableVal) Value() any                                { return make(chan int) }

func TestMarshalJSON_Errors(t *testing.T) {
	// GoNativeType returns ErrUnsupportedType for the weird object type, so
	// marshalJSON should surface a conversion error.
	got := marshalJSON(unmarshalableVal{})
	assert.True(t, types.IsError(got))
}

// TestListsBindings_Errors covers the argument-count, non-list, non-int, and
// out-of-bounds error branches of the list-mutation bindings.
func TestListsBindings_Errors(t *testing.T) {
	list := types.NewRefValList(types.DefaultTypeAdapter, []ref.Val{types.Int(1), types.Int(2)})

	cases := []struct {
		name string
		got  ref.Val
	}{
		{name: "set wrong arg count", got: listsSetAtIndex(types.Int(1))},
		{name: "set non-list", got: listsSetAtIndex(types.Int(1), types.Int(0), types.Int(9))},
		{name: "set non-int index", got: listsSetAtIndex(list, types.String("x"), types.Int(9))},
		{name: "set out of bounds", got: listsSetAtIndex(list, types.Int(5), types.Int(9))},
		{name: "set negative", got: listsSetAtIndex(list, types.Int(-1), types.Int(9))},

		{name: "insert wrong arg count", got: listsInsertAtIndex(types.Int(1))},
		{name: "insert non-list", got: listsInsertAtIndex(types.Int(1), types.Int(0), types.Int(9))},
		{name: "insert non-int index", got: listsInsertAtIndex(list, types.String("x"), types.Int(9))},
		{name: "insert out of bounds", got: listsInsertAtIndex(list, types.Int(6), types.Int(9))},
		{name: "insert negative", got: listsInsertAtIndex(list, types.Int(-1), types.Int(9))},

		{name: "remove non-list", got: listsRemoveAtIndex(types.Int(1), types.Int(0))},
		{name: "remove non-int index", got: listsRemoveAtIndex(list, types.String("x"))},
		{name: "remove out of bounds", got: listsRemoveAtIndex(list, types.Int(5))},
		{name: "remove negative", got: listsRemoveAtIndex(list, types.Int(-1))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.True(t, types.IsError(tc.got), "expected error, got %v", tc.got)
		})
	}
}

// TestListsBindings_InsertAppend covers insertAtIndex with index == size (append)
// and at index 0 (prepend), and removeAtIndex of the last element.
func TestListsBindings_Edges(t *testing.T) {
	list := types.NewRefValList(types.DefaultTypeAdapter, []ref.Val{types.Int(1), types.Int(2)})

	t.Run("insert append at size", func(t *testing.T) {
		got := listsInsertAtIndex(list, types.Int(2), types.Int(99))
		lister := got.(interface{ Size() ref.Val })
		assert.Equal(t, types.Int(3), lister.Size())
	})
	t.Run("remove last", func(t *testing.T) {
		got := listsRemoveAtIndex(list, types.Int(1))
		lister := got.(interface{ Size() ref.Val })
		assert.Equal(t, types.Int(1), lister.Size())
	})
}

// TestMergeVals_NonMapper covers the no-such-overload branch of mergeVals.
func TestMergeVals_NonMapper(t *testing.T) {
	got := mergeVals(types.Int(1), types.Int(2))
	assert.True(t, types.IsError(got))
}

// TestRandomBindings_Errors covers the validation error branches of the random
// binding functions invoked directly.
func TestRandomBindings_Errors(t *testing.T) {
	cases := []struct {
		name string
		got  ref.Val
	}{
		{name: "int wrong arg count", got: generateDeterministicInt(types.Int(0))},
		{name: "int non-int min", got: generateDeterministicInt(types.String("x"), types.Int(10), types.String("s"))},
		{name: "int non-int max", got: generateDeterministicInt(types.Int(0), types.String("x"), types.String("s"))},
		{name: "int non-string seed", got: generateDeterministicInt(types.Int(0), types.Int(10), types.Int(1))},
		{name: "int min >= max", got: generateDeterministicInt(types.Int(10), types.Int(10), types.String("s"))},

		{name: "string non-int length", got: generateDeterministicString(types.String("x"), types.String("s"))},
		{name: "string zero length", got: generateDeterministicString(types.Int(0), types.String("s"))},
		{name: "string negative length", got: generateDeterministicString(types.Int(-1), types.String("s"))},
		{name: "string non-string seed", got: generateDeterministicString(types.Int(4), types.Int(1))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.True(t, types.IsError(tc.got), "expected error, got %v", tc.got)
		})
	}
}

// TestGenerateDeterministicString_LongRehash covers the hash-regeneration
// branch in generateDeterministicString that triggers when the requested
// length exceeds the available hash bytes.
func TestGenerateDeterministicString_LongRehash(t *testing.T) {
	got := generateDeterministicString(types.Int(100), types.String("seed"))
	str, ok := got.(types.String)
	require.True(t, ok, "expected string, got %T", got)
	assert.Len(t, string(str), 100)

	// Deterministic: same inputs yield the same output.
	got2 := generateDeterministicString(types.Int(100), types.String("seed"))
	assert.Equal(t, got, got2)
}
