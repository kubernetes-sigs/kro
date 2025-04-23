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
// Merges two maps. Keys from the first map take precedence over keys in the second map.
//
//	map(string, int).merge(map(string, int)) -> map(string, int)
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
	mapType := cel.MapType(cel.TypeParamType("K"), cel.DynType)
	// mapDynType := cel.MapType(cel.DynType, cel.DynType)
	opts := []cel.EnvOption{
		cel.Function("merge",
			cel.MemberOverload("map_merge",
				[]*cel.Type{mapType, mapType},
				mapType,
				cel.BinaryBinding(func(arg1, arg2 ref.Val) ref.Val {
					self, ok := arg1.(traits.Mapper)
					if !ok {
						return types.ValOrErr(arg1, "no such overload: %v.merge(%v)", arg1.Type(), arg2.Type())
					}
					other, ok := arg2.(traits.Mapper)
					if !ok {
						return types.ValOrErr(arg1, "no such overload: %v.merge(%v)", arg1.Type(), arg2.Type())
					}
					result, err := merge(self, other)
					if err != nil {
						return types.WrapErr(err)
					}
					return result
				}),
			),
		),
	}
	return opts
}

// ProgramOptions implements the cel.Library interface method.
func (l *mapsLib) ProgramOptions() []cel.ProgramOption {
	return []cel.ProgramOption{}
}

// merge merges two maps. Keys from the first map take precedence over keys in
// the second map.
func merge(self traits.Mapper, other traits.Mapper) (traits.Mapper, error) {
	var result traits.MutableMapper

	if mapVal, ok := self.Value().(map[ref.Val]ref.Val); ok {
		result = types.NewMutableMap(types.DefaultTypeAdapter, mapVal)
	} else {
		result = types.NewMutableMap(types.DefaultTypeAdapter, nil)
		for i := self.Iterator(); i.HasNext().(types.Bool); {
			k := i.Next()
			v := self.Get(k)
			result.Insert(k, v)
		}
	}

	for i := other.Iterator(); i.HasNext().(types.Bool); {
		k := i.Next()
		if result.Contains(k).(types.Bool) {
			continue
		}
		v := other.Get(k)
		result.Insert(k, v)
	}
	return result.ToImmutableMap(), nil
}
