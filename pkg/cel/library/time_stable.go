// Copyright 2026 The Kubernetes Authors.
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

// time_stable.go implements the latched write side of KREP-025:
//
//	time.now().stableString(once())
//	time.now().stableString(whenChanged(a, b).onlyWhen(cond))
//
// A Trigger is an argument-only value: once()/whenChanged() build it,
// onlyWhen() refines it, and stableString() consumes it. stableString does
// not return a string directly — it returns a sentinels.StableStamp marker
// that the executor resolves against the live object right before apply, so
// the timestamp only advances when the trigger says so.
package library

import (
	"fmt"
	"reflect"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/common/types/traits"

	"github.com/kubernetes-sigs/kro/pkg/cel/conversion"
	"github.com/kubernetes-sigs/kro/pkg/cel/sentinels"
)

// TriggerType is the opaque CEL type of once()/whenChanged() values.
var TriggerType = types.NewOpaqueType("kro.Trigger")

// StableStampType is the opaque CEL type stableString() returns. It carries
// a sentinels.StableStamp through object rendering.
var StableStampType = types.NewOpaqueType("kro.StableStamp")

// Trigger is the runtime value behind kro.Trigger.
type Trigger struct {
	keys []ref.Val // nil => once()
	gate bool
}

var _ ref.Val = (*Trigger)(nil)

func (t *Trigger) ConvertToNative(typeDesc reflect.Type) (any, error) {
	return nil, fmt.Errorf("a kro.Trigger cannot be written to an object; pass it to time.now().stableString(...)")
}

func (t *Trigger) ConvertToType(typeVal ref.Type) ref.Val {
	if typeVal == types.TypeType {
		return TriggerType
	}
	return types.NewErr("unsupported conversion from kro.Trigger to %v", typeVal)
}

func (t *Trigger) Equal(other ref.Val) ref.Val {
	o, ok := other.(*Trigger)
	return types.Bool(ok && o == t)
}

func (t *Trigger) Type() ref.Type { return TriggerType }
func (t *Trigger) Value() any     { return t }

// StableStampVal wraps a sentinels.StableStamp as a CEL value.
type StableStampVal struct {
	stamp sentinels.StableStamp
}

var _ ref.Val = (*StableStampVal)(nil)

func (s *StableStampVal) ConvertToNative(typeDesc reflect.Type) (any, error) {
	return s.stamp, nil
}

func (s *StableStampVal) ConvertToType(typeVal ref.Type) ref.Val {
	if typeVal == types.TypeType {
		return StableStampType
	}
	return types.NewErr("unsupported conversion from kro.StableStamp to %v", typeVal)
}

func (s *StableStampVal) Equal(other ref.Val) ref.Val {
	o, ok := other.(*StableStampVal)
	return types.Bool(ok && o == s)
}

func (s *StableStampVal) Type() ref.Type { return StableStampType }

// Value returns the sentinel; conversion.GoNativeType's default branch
// passes it through so it lands in the rendered object unchanged.
func (s *StableStampVal) Value() any { return s.stamp }

func newOnce() ref.Val {
	return &Trigger{keys: nil, gate: true}
}

func newWhenChanged(args ...ref.Val) ref.Val {
	if len(args) == 0 {
		return types.NewErr("whenChanged() requires at least one key; use once() for a write-once stamp")
	}
	keys := make([]ref.Val, 0, len(args))
	for _, a := range args {
		if l, ok := a.(traits.Lister); ok && a.Type() == types.ListType {
			it := l.Iterator()
			for it.HasNext() == types.True {
				keys = append(keys, it.Next())
			}
			continue
		}
		keys = append(keys, a)
	}
	if len(keys) == 0 {
		return types.NewErr("whenChanged() requires at least one key; use once() for a write-once stamp")
	}
	return &Trigger{keys: keys, gate: true}
}

func triggerOnlyWhen(trig, cond ref.Val) ref.Val {
	t, ok := trig.(*Trigger)
	if !ok {
		return types.MaybeNoSuchOverloadErr(trig)
	}
	b, ok := cond.(types.Bool)
	if !ok {
		return types.MaybeNoSuchOverloadErr(cond)
	}
	return &Trigger{keys: t.keys, gate: t.gate && bool(b)}
}

func timestampStableString(tsVal, trig ref.Val) ref.Val {
	ts, ok := tsVal.(*KroTimestamp)
	if !ok {
		return types.MaybeNoSuchOverloadErr(tsVal)
	}
	t, ok := trig.(*Trigger)
	if !ok {
		return types.MaybeNoSuchOverloadErr(trig)
	}
	if ts.nowCount != 1 || ts.offset != 0 {
		return types.NewErr("stableString() must be called directly on time.now()")
	}
	stamp := sentinels.StableStamp{Gate: t.gate, Now: ts.clock.now}
	if t.keys != nil {
		stamp.Keys = make([]any, 0, len(t.keys))
		for _, k := range t.keys {
			native, err := conversion.GoNativeType(k)
			if err != nil {
				return types.NewErr("whenChanged key: %v", err)
			}
			stamp.Keys = append(stamp.Keys, native)
		}
	}
	return &StableStampVal{stamp: stamp}
}

// stableDeclarations registers once/whenChanged/onlyWhen/stableString.
func stableDeclarations() []cel.EnvOption {
	opts := []cel.EnvOption{
		cel.Function("once",
			cel.Overload("kro_trigger_once", nil, TriggerType,
				cel.FunctionBinding(func(_ ...ref.Val) ref.Val { return newOnce() }))),
		cel.Function("onlyWhen",
			cel.MemberOverload("kro_trigger_onlywhen_bool",
				[]*cel.Type{TriggerType, cel.BoolType}, TriggerType,
				cel.BinaryBinding(triggerOnlyWhen))),
		cel.Function("stableString",
			cel.MemberOverload("kro_timestamp_stablestring_trigger",
				[]*cel.Type{KroTimestampType, TriggerType}, StableStampType,
				cel.BinaryBinding(timestampStableString))),
	}
	// whenChanged with 1..4 dyn args. A single list argument is flattened at
	// runtime by newWhenChanged, so whenChanged([a, b]) works too.
	var wc []cel.FunctionOpt
	for n := 1; n <= 4; n++ {
		params := make([]*cel.Type, n)
		for i := range params {
			params[i] = cel.DynType
		}
		wc = append(wc, cel.Overload(fmt.Sprintf("kro_trigger_whenchanged_%d", n), params, TriggerType,
			cel.FunctionBinding(newWhenChanged)))
	}
	return append(opts, cel.Function("whenChanged", wc...))
}
