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
	"github.com/google/cel-go/common/types/traits"
)

// Lists returns a CEL library that provides index-mutation functions for lists.
// All functions are pure — they return a new list and do not modify the input.
//
// # SetIndex
//
// Returns a new list with the element at index replaced by value.
// Index must be in [0, size(list)).
//
//	lists.setIndex(list(T), int, T) -> list(T)
//
// Examples:
//
//	lists.setIndex([1, 2, 3], 1, 99)          // [1, 99, 3]
//	lists.setIndex(["a", "b", "c"], 0, "z")   // ["z", "b", "c"]
//
// # InsertAt
//
// Returns a new list with value inserted before the element at index.
// Index must be in [0, size(list)]. An index equal to size(list) appends.
//
//	lists.insertAt(list(T), int, T) -> list(T)
//
// Examples:
//
//	lists.insertAt([1, 2, 3], 1, 99)   // [1, 99, 2, 3]
//	lists.insertAt([1, 2, 3], 0, 99)   // [99, 1, 2, 3]
//	lists.insertAt([1, 2, 3], 3, 99)   // [1, 2, 3, 99]
//
// # RemoveAt
//
// Returns a new list with the element at index removed.
// Index must be in [0, size(list)).
//
//	lists.removeAt(list(T), int) -> list(T)
//
// Examples:
//
//	lists.removeAt([1, 2, 3], 1)   // [1, 3]
//	lists.removeAt([1, 2, 3], 0)   // [2, 3]
//	lists.removeAt([1, 2, 3], 2)   // [1, 2]
func Lists() cel.EnvOption {
	return cel.Lib(&listsLibrary{})
}

type listsLibrary struct{}

func (l *listsLibrary) LibraryName() string {
	return "kro.lists"
}

func (l *listsLibrary) CompileOptions() []cel.EnvOption {
	listType := cel.ListType(cel.TypeParamType("T"))
	return []cel.EnvOption{
		// lists.set is kept for backwards compatibility with existing RGDs.
		// It is typed list(int) only. New code should use lists.setIndex.
		cel.Function("lists.set",
			cel.Overload("lists.set_list_int_int",
				[]*cel.Type{cel.ListType(cel.IntType), cel.IntType, cel.IntType},
				cel.ListType(cel.IntType),
				cel.FunctionBinding(listsSetLegacy),
			),
		),

		// lists.setIndex(arr list(T), index int, value T) -> list(T)
		cel.Function("lists.setIndex",
			cel.Overload("lists.setIndex_list_int_T",
				[]*cel.Type{listType, cel.IntType, cel.TypeParamType("T")},
				listType,
				cel.FunctionBinding(listsSetIndex),
			),
		),

		// lists.insertAt(arr list(T), index int, value T) -> list(T)
		cel.Function("lists.insertAt",
			cel.Overload("lists.insertAt_list_int_T",
				[]*cel.Type{listType, cel.IntType, cel.TypeParamType("T")},
				listType,
				cel.FunctionBinding(listsInsertAt),
			),
		),

		// lists.removeAt(arr list(T), index int) -> list(T)
		cel.Function("lists.removeAt",
			cel.Overload("lists.removeAt_list_int",
				[]*cel.Type{listType, cel.IntType},
				listType,
				cel.BinaryBinding(listsRemoveAt),
			),
		),
	}
}

func (l *listsLibrary) ProgramOptions() []cel.ProgramOption {
	return nil
}

// listsSetLegacy is the original lists.set implementation, typed list(int) only.
// Kept for backwards compatibility with existing RGDs using lists.set.
func listsSetLegacy(args ...ref.Val) ref.Val {
	if len(args) != 3 {
		return types.NewErr("lists.set: expected 3 arguments (arr, index, value)")
	}
	lister, ok := args[0].(traits.Lister)
	if !ok {
		return types.NewErr("lists.set: first argument must be a list")
	}
	if args[1].Type() != types.IntType {
		return types.NewErr("lists.set: index must be an integer")
	}
	if args[2].Type() != types.IntType {
		return types.NewErr("lists.set: value must be an integer")
	}
	idx := int64(args[1].(types.Int))
	size := int64(lister.Size().(types.Int))
	if idx < 0 || idx >= size {
		return types.NewErr("lists.set: index %d out of bounds [0, %d)", idx, size)
	}
	newArr := make([]int64, size)
	newVal := int64(args[2].(types.Int))
	for i := int64(0); i < size; i++ {
		if i == idx {
			newArr[i] = newVal
		} else {
			newArr[i] = int64(lister.Get(types.Int(i)).(types.Int))
		}
	}
	return types.DefaultTypeAdapter.NativeToValue(newArr)
}

// listsSetIndex returns a new list(dyn) with the element at index replaced by value.
func listsSetIndex(args ...ref.Val) ref.Val {
	if len(args) != 3 {
		return types.NewErr("lists.setIndex: expected 3 arguments (arr, index, value)")
	}
	lister, ok := args[0].(traits.Lister)
	if !ok {
		return types.NewErr("lists.setIndex: first argument must be a list")
	}
	if args[1].Type() != types.IntType {
		return types.NewErr("lists.setIndex: index must be an integer")
	}
	idx := int64(args[1].(types.Int))
	size := int64(lister.Size().(types.Int))
	if idx < 0 || idx >= size {
		return types.NewErr("lists.setIndex: index %d out of bounds [0, %d)", idx, size)
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

// listsInsertAt returns a new list(dyn) with value inserted before the element at index.
// An index equal to size(list) appends the value.
func listsInsertAt(args ...ref.Val) ref.Val {
	if len(args) != 3 {
		return types.NewErr("lists.insertAt: expected 3 arguments (arr, index, value)")
	}
	lister, ok := args[0].(traits.Lister)
	if !ok {
		return types.NewErr("lists.insertAt: first argument must be a list")
	}
	if args[1].Type() != types.IntType {
		return types.NewErr("lists.insertAt: index must be an integer")
	}
	idx := int64(args[1].(types.Int))
	size := int64(lister.Size().(types.Int))
	if idx < 0 || idx > size {
		return types.NewErr("lists.insertAt: index %d out of bounds [0, %d]", idx, size)
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

// listsRemoveAt returns a new list(dyn) with the element at index removed.
func listsRemoveAt(arrVal, idxVal ref.Val) ref.Val {
	lister, ok := arrVal.(traits.Lister)
	if !ok {
		return types.NewErr("lists.removeAt: first argument must be a list")
	}
	if idxVal.Type() != types.IntType {
		return types.NewErr("lists.removeAt: index must be an integer")
	}
	idx := int64(idxVal.(types.Int))
	size := int64(lister.Size().(types.Int))
	if idx < 0 || idx >= size {
		return types.NewErr("lists.removeAt: index %d out of bounds [0, %d)", idx, size)
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
