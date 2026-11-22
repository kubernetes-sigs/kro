// Copyright 2025 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
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
)

// Reserved keys the executor injects into a collection item's scope value
// when a propagateWhen gate is evaluated per item. They are never written to
// the cluster. POC for KREP-026.
const (
	// ItemReadyKey carries the item's readyWhen verdict against its live object.
	ItemReadyKey = "__kro_ready"
	// ItemUpdatedKey carries whether the live object was rendered from the
	// current desired state (spec-hash stamp matches the current render).
	ItemUpdatedKey = "__kro_updated"
)

// ItemFacts registers `<item>.ready()` and `<item>.updated()` member
// functions. They read the reserved keys above from the receiver map and
// return false when the key is absent, so an item that has not been observed
// is neither ready nor updated.
func ItemFacts() cel.EnvOption {
	return cel.Lib(&itemFactsLib{})
}

type itemFactsLib struct{}

func (l *itemFactsLib) LibraryName() string { return "kro.itemfacts" }

func (l *itemFactsLib) CompileOptions() []cel.EnvOption {
	return []cel.EnvOption{
		cel.Function("ready",
			cel.MemberOverload("dyn_ready", []*cel.Type{cel.DynType}, cel.BoolType,
				cel.UnaryBinding(factReader(ItemReadyKey)))),
		cel.Function("updated",
			cel.MemberOverload("dyn_updated", []*cel.Type{cel.DynType}, cel.BoolType,
				cel.UnaryBinding(factReader(ItemUpdatedKey)))),
	}
}

func (l *itemFactsLib) ProgramOptions() []cel.ProgramOption { return nil }

func factReader(key string) func(ref.Val) ref.Val {
	return func(recv ref.Val) ref.Val {
		m, ok := recv.Value().(map[string]any)
		if !ok {
			// A typed wrapper (celunstructured) exposes Get; fall back to it.
			if mapper, isMapper := recv.(interface{ Get(ref.Val) ref.Val }); isMapper {
				v := mapper.Get(types.String(key))
				if b, isBool := v.(types.Bool); isBool {
					return b
				}
			}
			return types.False
		}
		b, _ := m[key].(bool)
		return types.Bool(b)
	}
}
