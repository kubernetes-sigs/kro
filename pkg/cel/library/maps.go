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

package library

import (
	"math"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/common/types/traits"
)

// Maps returns a cel.EnvOption to configure extended functions for map manipulation.
//
// # Merge
//
// Merges two maps. Keys from the second map overwrite already available keys in the first map.
// Keys must be of type string, value types must be identical in the maps merged.
//
//	map(string, T).merge(map(string, T)) -> map(string, T)
//
// Examples:
//
//	{}.merge({}) == {}
//	{}.merge({'a': 1}) == {'a': 1}
//	{'a': 1}.merge({}) == {'a': 1}
//	{'a': 1}.merge({'b': 2}) == {'a': 1, 'b': 2}
//	{'a': 1}.merge({'a': 2, 'b': 2}) == {'a': 1, 'b': 2}
func Maps(options ...MapsOption) cel.EnvOption {
	l := &mapsLib{version: math.MaxUint32}
	for _, opt := range options {
		opt(l)
	}
	return cel.Lib(l)
}

type mapsLib struct {
	version uint32
}

type MapsOption func(*mapsLib) *mapsLib

// LibraryName implements the cel.SingletonLibrary interface method.
func (l *mapsLib) LibraryName() string {
	return "cel.lib.ext.kro.maps"
}

// CompileOptions implements the cel.Library interface method.
func (l *mapsLib) CompileOptions() []cel.EnvOption {
	mapType := cel.MapType(cel.TypeParamType("K"), cel.TypeParamType("V"))
	// mapDynType := cel.MapType(cel.DynType, cel.DynType)
	opts := []cel.EnvOption{
		cel.Function("merge",
			cel.MemberOverload("map_merge",
				[]*cel.Type{mapType, mapType},
				mapType,
				cel.BinaryBinding(mergeVals),
			),
		),
	}
	return opts
}

// ProgramOptions implements the cel.Library interface method.
func (l *mapsLib) ProgramOptions() []cel.ProgramOption {
	return []cel.ProgramOption{}
}

func mergeVals(lhs, rhs ref.Val) ref.Val {
	left, lok := lhs.(traits.Mapper)
	right, rok := rhs.(traits.Mapper)
	if !lok || !rok {
		return types.ValOrErr(lhs, "no such overload: %v.merge(%v)", lhs.Type(), rhs.Type())
	}
	return merge(left, right)
}

// merge returns a new map containing entries from both maps.
// Keys in 'other' overwrite keys in 'self'.
func merge(self, other traits.Mapper) traits.Mapper {
	result := mapperTraitToMutableMapper(other)
	for i := self.Iterator(); i.HasNext().(types.Bool); {
		k := i.Next()
		if !result.Contains(k).(types.Bool) {
			result.Insert(k, self.Get(k))
		}
	}
	return result.ToImmutableMap()
}

// mapperTraitToMutableMapper copies a traits.Mapper into a MutableMap.
func mapperTraitToMutableMapper(m traits.Mapper) traits.MutableMapper {
	vals := make(map[ref.Val]ref.Val, m.Size().(types.Int))
	for it := m.Iterator(); it.HasNext().(types.Bool); {
		k := it.Next()
		vals[k] = m.Get(k)
	}
	return types.NewMutableMap(types.DefaultTypeAdapter, vals)
}
