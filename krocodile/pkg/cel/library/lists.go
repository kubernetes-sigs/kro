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
	"math"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/common/types/traits"
)

// Lists returns a CEL library that provides index-mutation functions for lists.
// All functions are pure — they return a new list and do not modify the input.
//
// # SetAtIndex
//
// Returns a new list with the element at index replaced by value.
// Index must be in [0, size(list)).
//
//	lists.setAtIndex(list(T), int, T) -> list(T)
//
// Examples:
//
//	lists.setAtIndex([1, 2, 3], 1, 99)          // [1, 99, 3]
//	lists.setAtIndex(["a", "b", "c"], 0, "z")   // ["z", "b", "c"]
//
// # InsertAtIndex
//
// Returns a new list with value inserted before the element at index.
// Index must be in [0, size(list)]. An index equal to size(list) appends.
//
//	lists.insertAtIndex(list(T), int, T) -> list(T)
//
// Examples:
//
//	lists.insertAtIndex([1, 2, 3], 1, 99)   // [1, 99, 2, 3]
//	lists.insertAtIndex([1, 2, 3], 0, 99)   // [99, 1, 2, 3]
//	lists.insertAtIndex([1, 2, 3], 3, 99)   // [1, 2, 3, 99]
//
// # RemoveAtIndex
//
// Returns a new list with the element at index removed.
// Index must be in [0, size(list)).
//
//	lists.removeAtIndex(list(T), int) -> list(T)
//
// Examples:
//
//	lists.removeAtIndex([1, 2, 3], 1)   // [1, 3]
//	lists.removeAtIndex([1, 2, 3], 0)   // [2, 3]
//	lists.removeAtIndex([1, 2, 3], 2)   // [1, 2]
func Lists(options ...ListsOption) cel.EnvOption {
	l := &listsLibrary{version: math.MaxUint32}
	for _, o := range options {
		l = o(l)
	}
	return cel.Lib(l)
}

type listsLibrary struct {
	version uint32
}

// ListsOption is a functional option for configuring the lists library.
type ListsOption func(*listsLibrary) *listsLibrary

// ListsVersion configures the version of the lists library.
//
// The version limits which functions are available. Only functions introduced
// below or equal to the given version are included in the library. If this
// option is not set, all functions are available.
//
// See the library documentation to determine which version a function was
// introduced. If the documentation does not state which version a function was
// introduced, it can be assumed to be introduced at version 0, when the library
// was first created.
func ListsVersion(version uint32) ListsOption {
	return func(lib *listsLibrary) *listsLibrary {
		lib.version = version
		return lib
	}
}

func (l *listsLibrary) LibraryName() string {
	return "kro.lists"
}

func (l *listsLibrary) CompileOptions() []cel.EnvOption {
	listType := cel.ListType(cel.TypeParamType("T"))
	return []cel.EnvOption{
		// lists.setAtIndex(arr list(T), index int, value T) -> list(T)
		cel.Function("lists.setAtIndex",
			cel.Overload("lists.setAtIndex_list_int_T",
				[]*cel.Type{listType, cel.IntType, cel.TypeParamType("T")},
				listType,
				cel.FunctionBinding(listsSetAtIndex),
			),
		),

		// lists.insertAtIndex(arr list(T), index int, value T) -> list(T)
		cel.Function("lists.insertAtIndex",
			cel.Overload("lists.insertAtIndex_list_int_T",
				[]*cel.Type{listType, cel.IntType, cel.TypeParamType("T")},
				listType,
				cel.FunctionBinding(listsInsertAtIndex),
			),
		),

		// lists.removeAtIndex(arr list(T), index int) -> list(T)
		cel.Function("lists.removeAtIndex",
			cel.Overload("lists.removeAtIndex_list_int",
				[]*cel.Type{listType, cel.IntType},
				listType,
				cel.BinaryBinding(listsRemoveAtIndex),
			),
		),
	}
}

func (l *listsLibrary) ProgramOptions() []cel.ProgramOption {
	return nil
}

// listsSetAtIndex returns a new list(T) with the element at index replaced by value.
func listsSetAtIndex(args ...ref.Val) ref.Val {
	if len(args) != 3 {
		return types.NewErr("lists.setAtIndex: expected 3 arguments (arr, index, value)")
	}
	lister, ok := args[0].(traits.Lister)
	if !ok {
		return types.NewErr("lists.setAtIndex: first argument must be a list")
	}
	if args[1].Type() != types.IntType {
		return types.NewErr("lists.setAtIndex: index must be an integer")
	}
	idx := int64(args[1].(types.Int))
	size := int64(lister.Size().(types.Int))
	if idx < 0 || idx >= size {
		return types.NewErr("lists.setAtIndex: index %d out of bounds [0, %d)", idx, size)
	}
	elems := make([]ref.Val, size)
	for i := int64(0); i < size; i++ {
		if i == idx {
			elems[i] = args[2]
		} else {
			elems[i] = lister.Get(types.Int(i))
		}
	}
	return types.NewRefValList(types.DefaultTypeAdapter, elems)
}

// listsInsertAtIndex returns a new list(T) with value inserted before the element at index.
// An index equal to size(list) appends the value.
func listsInsertAtIndex(args ...ref.Val) ref.Val {
	if len(args) != 3 {
		return types.NewErr("lists.insertAtIndex: expected 3 arguments (arr, index, value)")
	}
	lister, ok := args[0].(traits.Lister)
	if !ok {
		return types.NewErr("lists.insertAtIndex: first argument must be a list")
	}
	if args[1].Type() != types.IntType {
		return types.NewErr("lists.insertAtIndex: index must be an integer")
	}
	idx := int64(args[1].(types.Int))
	size := int64(lister.Size().(types.Int))
	if idx < 0 || idx > size {
		return types.NewErr("lists.insertAtIndex: index %d out of bounds [0, %d]", idx, size)
	}
	elems := make([]ref.Val, size+1)
	for i := int64(0); i < idx; i++ {
		elems[i] = lister.Get(types.Int(i))
	}
	elems[idx] = args[2]
	for i := idx; i < size; i++ {
		elems[i+1] = lister.Get(types.Int(i))
	}
	return types.NewRefValList(types.DefaultTypeAdapter, elems)
}

// listsRemoveAtIndex returns a new list(T) with the element at index removed.
func listsRemoveAtIndex(arrVal, idxVal ref.Val) ref.Val {
	lister, ok := arrVal.(traits.Lister)
	if !ok {
		return types.NewErr("lists.removeAtIndex: first argument must be a list")
	}
	if idxVal.Type() != types.IntType {
		return types.NewErr("lists.removeAtIndex: index must be an integer")
	}
	idx := int64(idxVal.(types.Int))
	size := int64(lister.Size().(types.Int))
	if idx < 0 || idx >= size {
		return types.NewErr("lists.removeAtIndex: index %d out of bounds [0, %d)", idx, size)
	}
	elems := make([]ref.Val, size-1)
	for i := int64(0); i < idx; i++ {
		elems[i] = lister.Get(types.Int(i))
	}
	for i := idx + 1; i < size; i++ {
		elems[i-1] = lister.Get(types.Int(i))
	}
	return types.NewRefValList(types.DefaultTypeAdapter, elems)
}
