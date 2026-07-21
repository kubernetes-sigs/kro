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

package library

import (
	"fmt"
	"reflect"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common"
	"github.com/google/cel-go/common/ast"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/common/types/traits"
	"github.com/google/cel-go/parser"
)

// Runtime returns a CEL library providing the `runtime` variable used to
// author custom status conditions on RGD instances:
//
//	runtime.newCondition({type: 'X', status: 'True', reason: '', message: ''})
//	runtime.condition(schema, 'ResourcesReady').status == 'True'
//
// newCondition constructs a Condition from a map with keys type, status,
// reason, and message (type and status required; status must be True, False,
// or Unknown). condition looks up a condition by type on
// obj.status.conditions[], returning an empty Condition when not found.
func Runtime() cel.EnvOption {
	return cel.Lib(&runtimeLibrary{})
}

// ConditionTypeName is the CEL type name for Condition values.
const ConditionTypeName = "kro.run.Condition"

// RuntimeTypeName is the CEL type name for the singleton runtime variable.
const RuntimeTypeName = "kro.run.Runtime"

// RuntimeVarName is the identifier kro injects into the CEL evaluation
// context for the runtime variable.
const RuntimeVarName = "runtime"

// Condition field keys: the only keys allowed in runtime.newCondition's input
// and the only fields exposed on a Condition value.
const (
	conditionKeyType    = "type"
	conditionKeyStatus  = "status"
	conditionKeyReason  = "reason"
	conditionKeyMessage = "message"
)

// validConditionStatuses matches metav1.Condition status values.
var validConditionStatuses = map[string]struct{}{
	"True":    {},
	"False":   {},
	"Unknown": {},
}

var expectedConditionKeys = map[string]struct{}{
	conditionKeyType:    {},
	conditionKeyStatus:  {},
	conditionKeyReason:  {},
	conditionKeyMessage: {},
}

// IsValidConditionStatus reports whether s is a valid Condition status
// (True, False, or Unknown).
func IsValidConditionStatus(s string) bool {
	_, ok := validConditionStatuses[s]
	return ok
}

// IsConditionType reports whether t is the CEL Condition type returned by
// runtime.newCondition and runtime.condition.
func IsConditionType(t *cel.Type) bool {
	return t != nil && t.TypeName() == ConditionTypeName
}

// conditionType declares IndexerType so authors can access fields with dot
// notation: cond.type, cond.status, cond.reason, cond.message.
var conditionType = cel.ObjectType(ConditionTypeName, traits.IndexerType)

// runtimeType carries no traits; only method dispatch is exposed.
var runtimeType = cel.ObjectType(RuntimeTypeName)

// RuntimeSingleton is the stateless value injected at evaluation time as the
// `runtime` variable.
var RuntimeSingleton ref.Val = &runtimeValue{}

type runtimeLibrary struct{}

func (l *runtimeLibrary) LibraryName() string {
	return "kro.runtime"
}

func (l *runtimeLibrary) CompileOptions() []cel.EnvOption {
	mapType := cel.MapType(cel.StringType, cel.StringType)

	return []cel.EnvOption{
		cel.Types(runtimeType),
		cel.Variable(RuntimeVarName, runtimeType),

		// Authors write newCondition's argument as a struct-style map
		// literal with bare identifier keys. The parse-time macro below
		// rewrites the keys into string literals (and validates them), so
		// the declared signature stays map(string, string). Both functions
		// return the typed Condition so field access type-checks.
		cel.Function("newCondition",
			cel.MemberOverload("runtime_newCondition_map",
				[]*cel.Type{runtimeType, mapType},
				conditionType,
				cel.BinaryBinding(newConditionImpl),
			),
		),
		cel.Function("condition",
			cel.MemberOverload("runtime_condition_dyn_string",
				[]*cel.Type{runtimeType, cel.DynType, cel.StringType},
				conditionType,
				cel.FunctionBinding(conditionImpl),
			),
		),
		cel.Macros(parser.NewReceiverMacro("newCondition", 1, newConditionMacro)),
	}
}

// newConditionMacro rewrites runtime.newCondition's identifier-keyed map
// literal into a string-keyed one at parse time, rejecting unknown keys,
// duplicate keys, quoted/computed keys, and missing required keys with
// source-position errors. Status values are validated separately: literals
// at build time, computed values at evaluation time.
func newConditionMacro(eh parser.ExprHelper, target ast.Expr, args []ast.Expr) (ast.Expr, *common.Error) {
	// cel-go matches macros on (name, arg count, receiver-style) only, so
	// any `x.newCondition(arg)` lands here. Return nil for receivers other
	// than `runtime` so the parser leaves the AST untouched.
	if target == nil || target.Kind() != ast.IdentKind || target.AsIdent() != RuntimeVarName {
		return nil, nil
	}
	mapArg := args[0]
	if mapArg.Kind() != ast.MapKind {
		return nil, eh.NewError(mapArg.ID(), "runtime.newCondition: argument must be a map literal")
	}

	seen := make(map[string]struct{}, 4)
	newEntries := make([]ast.EntryExpr, 0, len(mapArg.AsMap().Entries()))
	for _, entry := range mapArg.AsMap().Entries() {
		me := entry.AsMapEntry()
		key := me.Key()
		val := me.Value()

		if key.Kind() != ast.IdentKind {
			return nil, eh.NewError(key.ID(),
				"runtime.newCondition: keys must be bare identifiers (type, status, reason, message); quoted or computed keys are not allowed")
		}
		name := key.AsIdent()
		if _, allowed := expectedConditionKeys[name]; !allowed {
			return nil, eh.NewError(key.ID(),
				fmt.Sprintf("runtime.newCondition: unknown key %q (allowed: type, status, reason, message)", name))
		}
		if _, dup := seen[name]; dup {
			return nil, eh.NewError(key.ID(),
				fmt.Sprintf("runtime.newCondition: duplicate key %q", name))
		}
		seen[name] = struct{}{}

		newKey := eh.NewLiteral(types.String(name))
		newEntries = append(newEntries, eh.NewMapEntry(newKey, val, false))
	}

	if _, ok := seen[conditionKeyType]; !ok {
		return nil, eh.NewError(mapArg.ID(), "runtime.newCondition: 'type' is required")
	}
	if _, ok := seen[conditionKeyStatus]; !ok {
		return nil, eh.NewError(mapArg.ID(), "runtime.newCondition: 'status' is required")
	}

	newMap := eh.NewMap(newEntries...)
	return eh.NewMemberCall("newCondition", target, newMap), nil
}

func (l *runtimeLibrary) ProgramOptions() []cel.ProgramOption {
	return nil
}

// runtimeValue is the stateless value backing the `runtime` CEL variable.
type runtimeValue struct{}

func (r *runtimeValue) ConvertToNative(typeDesc reflect.Type) (any, error) {
	return nil, fmt.Errorf("type conversion not supported for %s", RuntimeTypeName)
}

func (r *runtimeValue) ConvertToType(typeVal ref.Type) ref.Val {
	if typeVal == runtimeType {
		return r
	}
	if typeVal == types.TypeType {
		return runtimeType
	}
	return types.NewErr("type conversion error from %s to %s", RuntimeTypeName, typeVal.TypeName())
}

func (r *runtimeValue) Equal(other ref.Val) ref.Val {
	_, ok := other.(*runtimeValue)
	return types.Bool(ok)
}

func (r *runtimeValue) Type() ref.Type {
	return runtimeType
}

func (r *runtimeValue) Value() any {
	return r
}

// Condition is the Go representation of a CEL Condition value, mirroring the
// shape of kro's wire conditions. Field names are prefixed where they would
// collide with the ref.Val interface (Type() method).
type Condition struct {
	ConditionType string
	Status        string
	Reason        string
	Message       string
}

var (
	_ ref.Val        = (*Condition)(nil)
	_ traits.Indexer = (*Condition)(nil)
)

func (c *Condition) ConvertToNative(typeDesc reflect.Type) (any, error) {
	if typeDesc.Kind() == reflect.Map {
		return map[string]any{
			conditionKeyType:    c.ConditionType,
			conditionKeyStatus:  c.Status,
			conditionKeyReason:  c.Reason,
			conditionKeyMessage: c.Message,
		}, nil
	}
	if typeDesc == reflect.TypeOf((*Condition)(nil)) {
		return c, nil
	}
	if typeDesc == reflect.TypeOf(Condition{}) {
		return *c, nil
	}
	return nil, fmt.Errorf("type conversion error from %s to %v", ConditionTypeName, typeDesc)
}

func (c *Condition) ConvertToType(typeVal ref.Type) ref.Val {
	if typeVal == conditionType {
		return c
	}
	if typeVal == types.TypeType {
		return conditionType
	}
	return types.NewErr("type conversion error from %s to %s", ConditionTypeName, typeVal.TypeName())
}

func (c *Condition) Equal(other ref.Val) ref.Val {
	o, ok := other.(*Condition)
	if !ok {
		return types.MaybeNoSuchOverloadErr(other)
	}
	return types.Bool(c.ConditionType == o.ConditionType && c.Status == o.Status && c.Reason == o.Reason && c.Message == o.Message)
}

func (c *Condition) Type() ref.Type {
	return conditionType
}

func (c *Condition) Value() any {
	return c
}

// Get implements traits.Indexer for field access via dot notation.
func (c *Condition) Get(key ref.Val) ref.Val {
	keyStr, ok := key.(types.String)
	if !ok {
		return types.NewErr("Condition: field key must be a string, got %v", key.Type().TypeName())
	}
	switch string(keyStr) {
	case conditionKeyType:
		return types.String(c.ConditionType)
	case conditionKeyStatus:
		return types.String(c.Status)
	case conditionKeyReason:
		return types.String(c.Reason)
	case conditionKeyMessage:
		return types.String(c.Message)
	}
	return types.NewErr("Condition: no such field %q", string(keyStr))
}

// newConditionImpl is the runtime binding for runtime.newCondition. The
// parse-time macro has already enforced the key rules; this validates the
// values, catching computed type/status expressions that build-time literal
// checks cannot see.
func newConditionImpl(receiver, arg ref.Val) ref.Val {
	mapper, ok := arg.(traits.Mapper)
	if !ok {
		return types.NewErr("runtime.newCondition: argument must be a map, got %v", arg.Type().TypeName())
	}

	cond := &Condition{}
	for it := mapper.Iterator(); it.HasNext() == types.True; {
		k := it.Next()
		keyStr, _ := k.(types.String)
		key := string(keyStr)

		v := mapper.Get(k)
		if types.IsError(v) {
			return v
		}
		valStr, ok := v.(types.String)
		if !ok {
			return types.NewErr("runtime.newCondition: %s must be a string, got %v", key, v.Type().TypeName())
		}

		switch key {
		case conditionKeyType:
			if valStr == "" {
				return types.NewErr("runtime.newCondition: type must not be empty")
			}
			cond.ConditionType = string(valStr)
		case conditionKeyStatus:
			s := string(valStr)
			if !IsValidConditionStatus(s) {
				return types.NewErr("runtime.newCondition: status must be one of True, False, Unknown (got %q)", s)
			}
			cond.Status = s
		case conditionKeyReason:
			cond.Reason = string(valStr)
		case conditionKeyMessage:
			cond.Message = string(valStr)
		}
	}

	return cond
}

func conditionImpl(args ...ref.Val) ref.Val {
	if len(args) != 3 {
		return types.NewErr("runtime.condition: expected 3 arguments (receiver, obj, type), got %d", len(args))
	}
	obj := args[1]
	typeArg := args[2]

	typeStr, ok := typeArg.(types.String)
	if !ok {
		return types.NewErr("runtime.condition: type must be a string, got %v", typeArg.Type().TypeName())
	}
	wanted := string(typeStr)

	conditions, errVal := extractConditions(obj)
	if errVal != nil {
		return errVal
	}

	for _, cond := range conditions {
		if cond.ConditionType == wanted {
			return cond
		}
	}

	return &Condition{}
}

// extractConditions navigates obj.status.conditions[] and returns the
// conditions found. A missing path returns (nil, nil); an unexpected type
// returns a CEL error value.
func extractConditions(obj ref.Val) ([]*Condition, ref.Val) {
	mapper, ok := obj.(traits.Mapper)
	if !ok {
		return nil, types.NewErr("runtime.condition: obj must be an object, got %v", obj.Type().TypeName())
	}

	statusVal, found := mapper.Find(types.String("status"))
	if !found {
		return nil, nil
	}
	statusMap, ok := statusVal.(traits.Mapper)
	if !ok {
		return nil, types.NewErr("runtime.condition: obj.status must be an object")
	}

	condsVal, found := statusMap.Find(types.String("conditions"))
	if !found {
		return nil, nil
	}
	condsList, ok := condsVal.(traits.Lister)
	if !ok {
		return nil, types.NewErr("runtime.condition: obj.status.conditions must be a list")
	}

	size, ok := condsList.Size().(types.Int)
	if !ok {
		return nil, types.NewErr("runtime.condition: unexpected list size type")
	}

	conditions := make([]*Condition, 0, int(size))
	for i := types.Int(0); i < size; i++ {
		elem := condsList.Get(i)
		switch v := elem.(type) {
		case *Condition:
			conditions = append(conditions, v)
		case traits.Mapper:
			conditions = append(conditions, conditionFromMap(v))
		default:
			return nil, types.NewErr("runtime.condition: unexpected condition element type %v", elem.Type().TypeName())
		}
	}
	return conditions, nil
}

// conditionFromMap lifts a generic CEL map into a Condition. Used when the
// conditions list comes from the wire rather than from runtime.newCondition.
func conditionFromMap(m traits.Mapper) *Condition {
	read := func(key string) string {
		v, ok := m.Find(types.String(key))
		if !ok {
			return ""
		}
		s, ok := v.(types.String)
		if !ok {
			return ""
		}
		return string(s)
	}
	return &Condition{
		ConditionType: read(conditionKeyType),
		Status:        read(conditionKeyStatus),
		Reason:        read(conditionKeyReason),
		Message:       read(conditionKeyMessage),
	}
}

// ConditionTypeProvider wraps a base types.Provider so the CEL type-checker
// can resolve the kro.run.Condition type and its four string fields, letting
// a misspelled field access fail at build time instead of evaluation time.
// cel.CustomTypeProvider replaces the environment's provider outright, so
// this wrapper must be outermost and delegate everything else to base.
func ConditionTypeProvider(base types.Provider) types.Provider {
	return &conditionTypeProvider{Provider: base}
}

type conditionTypeProvider struct {
	types.Provider
}

func (p *conditionTypeProvider) FindStructType(name string) (*types.Type, bool) {
	if name == ConditionTypeName {
		return types.NewTypeTypeWithParam(conditionType), true
	}
	return p.Provider.FindStructType(name)
}

func (p *conditionTypeProvider) FindStructFieldNames(name string) ([]string, bool) {
	if name == ConditionTypeName {
		return []string{conditionKeyType, conditionKeyStatus, conditionKeyReason, conditionKeyMessage}, true
	}
	return p.Provider.FindStructFieldNames(name)
}

func (p *conditionTypeProvider) FindStructFieldType(name, field string) (*types.FieldType, bool) {
	if name == ConditionTypeName {
		if _, ok := expectedConditionKeys[field]; ok {
			return &types.FieldType{Type: types.StringType}, true
		}
		return nil, false
	}
	return p.Provider.FindStructFieldType(name, field)
}
