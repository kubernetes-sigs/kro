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

// Runtime returns a CEL library that provides the `runtime` variable used
// to author custom status conditions on RGD instances.
//
// runtime.newCondition({type, status, reason, message}) -> Condition
//
// Constructs a Condition value. The map argument must contain exactly the
// four keys 'type', 'status', 'reason', 'message'. All values are strings.
// 'status' must be one of "True", "False", "Unknown".
//
//	runtime.newCondition({
//	  type: 'PrimaryReady',
//	  status: 'True',
//	  reason: 'Running',
//	  message: 'Primary is healthy'
//	})
//
// runtime.condition(obj, type) -> Condition
//
// Looks up the condition with the given type on obj.status.conditions[].
// Returns a Condition with empty fields if not found.
//
//	runtime.condition(schema, 'ResourcesReady').status == 'True'
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

// validConditionStatuses lists the only string literals allowed for a
// Condition's status field. Matches metav1.Condition status values.
var validConditionStatuses = map[string]struct{}{
	"True":    {},
	"False":   {},
	"Unknown": {},
}

// expectedConditionKeys are the only keys allowed in the input to
// runtime.newCondition.
var expectedConditionKeys = map[string]struct{}{
	"type":    {},
	"status":  {},
	"reason":  {},
	"message": {},
}

// conditionType is the type descriptor for Condition values. It declares
// IndexerType so authors can access fields with dot notation:
// cond.type, cond.status, cond.reason, cond.message.
//
// cel.ObjectType returns *types.Type, which satisfies both the cel.Type
// slot used in MemberOverload signatures and the ref.Type returned from
// the ref.Val.Type() method. One value, two roles.
var conditionType = cel.ObjectType(ConditionTypeName, traits.IndexerType)

// runtimeType is the type descriptor for the singleton runtime variable.
// It carries no traits; only method dispatch via MemberOverload is exposed.
var runtimeType = cel.ObjectType(RuntimeTypeName)

// RuntimeSingleton is the value injected at evaluation time as the
// `runtime` variable. It is stateless; method dispatch ignores the
// receiver and routes to package-level binding functions.
var RuntimeSingleton ref.Val = &runtimeValue{}

type runtimeLibrary struct{}

func (l *runtimeLibrary) LibraryName() string {
	return "kro.runtime"
}

func (l *runtimeLibrary) CompileOptions() []cel.EnvOption {
	mapType := cel.MapType(cel.StringType, cel.StringType)

	return []cel.EnvOption{
		// Register the Runtime singleton type so type-checking can resolve
		// `runtime.newCondition` and `runtime.condition` calls.
		cel.Types(runtimeType),

		// Declare `runtime` as a variable of the singleton runtime type.
		cel.Variable(RuntimeVarName, runtimeType),

		// runtime.newCondition({type: ..., status: ..., reason: ..., message: ...}) -> dyn
		//
		// Authors write the input as a struct-style map literal with bare
		// identifier keys. A parse-time macro (registered below) rewrites
		// identifier keys into string literals before type-checking sees
		// them, so the function's declared signature can stay
		// map(string, string). The macro also rejects unknown keys,
		// duplicate keys, missing required keys (type, status), and invalid
		// literal status values — collapsing what used to be three separate
		// validation layers into one parse-time pass.
		//
		// The return type is dyn (rather than the typed Condition) because
		// the *Condition runtime value already implements traits.Indexer
		// for .type/.status/.reason/.message field access; declaring a
		// full struct type would not add expressiveness.
		cel.Function("newCondition",
			cel.MemberOverload("runtime_newCondition_map",
				[]*cel.Type{runtimeType, mapType},
				cel.DynType,
				cel.BinaryBinding(newConditionImpl),
			),
		),

		// runtime.condition(obj, type) -> dyn (see comment above)
		cel.Function("condition",
			cel.MemberOverload("runtime_condition_dyn_string",
				[]*cel.Type{runtimeType, cel.DynType, cel.StringType},
				cel.DynType,
				cel.FunctionBinding(conditionImpl),
			),
		),

		// Macro for runtime.newCondition: rewrites identifier-keyed map
		// literals into string-keyed ones at parse time. See
		// newConditionMacro for the rules enforced.
		cel.Macros(parser.NewReceiverMacro("newCondition", 1, newConditionMacro)),
	}
}

// newConditionMacro is the parse-time rewriter for runtime.newCondition's
// argument map. It enforces the four allowed-key rule, the literal-status
// rule, the required-keys rule, and prohibits quoted/computed keys — all
// at parse time, with errors reported at the source position of the
// offending node. After validation, identifier keys are rewritten as
// string literals so the downstream type-checker sees a plain
// map(string, string).
func newConditionMacro(eh parser.ExprHelper, target ast.Expr, args []ast.Expr) (ast.Expr, *common.Error) {
	// cel-go's macro matching is keyed only on (function name, arg count,
	// isReceiverStyle). Any `x.newCondition(arg)` call would land here. We
	// only own `runtime.newCondition`; for other receivers, return nil to
	// signal "not a match" so the parser leaves the AST untouched.
	if target == nil || target.Kind() != ast.IdentKind || target.AsIdent() != RuntimeVarName {
		return nil, nil
	}
	if len(args) != 1 {
		return nil, eh.NewError(0, "runtime.newCondition: expected exactly one map argument")
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

		if name == "status" && val.Kind() == ast.LiteralKind {
			if s, ok := val.AsLiteral().Value().(string); ok {
				if _, valid := validConditionStatuses[s]; !valid {
					return nil, eh.NewError(val.ID(),
						fmt.Sprintf("runtime.newCondition: status must be one of True, False, Unknown (got %q)", s))
				}
			}
		}

		newKey := eh.NewLiteral(types.String(name))
		newEntries = append(newEntries, eh.NewMapEntry(newKey, val, false))
	}

	if _, ok := seen["type"]; !ok {
		return nil, eh.NewError(mapArg.ID(), "runtime.newCondition: 'type' is required")
	}
	if _, ok := seen["status"]; !ok {
		return nil, eh.NewError(mapArg.ID(), "runtime.newCondition: 'status' is required")
	}

	newMap := eh.NewMap(newEntries...)
	return eh.NewMemberCall("newCondition", target, newMap), nil
}

func (l *runtimeLibrary) ProgramOptions() []cel.ProgramOption {
	return nil
}

// -------------------------------------------------------------------------
// runtime singleton value
// -------------------------------------------------------------------------

// runtimeValue is the stateless value backing the `runtime` CEL variable.
// Method calls on it dispatch to package-level binding functions; the
// receiver carries no per-call state.
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

// -------------------------------------------------------------------------
// Condition value
// -------------------------------------------------------------------------

// Condition is the Go representation of a CEL Condition value. It mirrors
// the shape of metav1.Condition for kro's wire output. The exported fields
// allow the runtime layer to read values out of an evaluated CEL result
// without depending on internal CEL machinery.
//
// Field names are prefixed because Type, Reason, etc. are reserved or
// would collide with the ref.Val interface (Type() method).
type Condition struct {
	ConditionType string
	Status        string
	Reason        string
	Message       string
}

// Compile-time assertion that Condition implements the interfaces CEL needs.
var (
	_ ref.Val        = (*Condition)(nil)
	_ traits.Indexer = (*Condition)(nil)
)

func (c *Condition) ConvertToNative(typeDesc reflect.Type) (any, error) {
	if typeDesc.Kind() == reflect.Map {
		return map[string]any{
			"type":    c.ConditionType,
			"status":  c.Status,
			"reason":  c.Reason,
			"message": c.Message,
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
	case "type":
		return types.String(c.ConditionType)
	case "status":
		return types.String(c.Status)
	case "reason":
		return types.String(c.Reason)
	case "message":
		return types.String(c.Message)
	}
	return types.NewErr("Condition: no such field %q", string(keyStr))
}

// -------------------------------------------------------------------------
// runtime.newCondition implementation
// -------------------------------------------------------------------------

// newConditionImpl is the runtime binding for runtime.newCondition. By
// the time it executes, the parse-time macro (newConditionMacro) has
// already enforced the structural rules — only the four allowed keys,
// type and status present, no duplicates, literal-status values valid.
//
// What still has to be checked at runtime: dynamic status values (e.g.
// status: schema.spec.healthy ? 'True' : 'False'), which the macro
// can't validate because it sees an arbitrary expression rather than a
// literal. Everything else here is straightforward map → struct
// construction.
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
		case "type":
			cond.ConditionType = string(valStr)
		case "status":
			s := string(valStr)
			if _, ok := validConditionStatuses[s]; !ok {
				return types.NewErr("runtime.newCondition: status must be one of True, False, Unknown (got %q)", s)
			}
			cond.Status = s
		case "reason":
			cond.Reason = string(valStr)
		case "message":
			cond.Message = string(valStr)
		}
	}

	return cond
}

// -------------------------------------------------------------------------
// runtime.condition implementation
// -------------------------------------------------------------------------

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

// extractConditions navigates obj.status.conditions[] and returns the slice
// of conditions found. Returns a CEL error value (already a *types.Err) if
// the navigation hits an unexpected type. A missing path returns (nil, nil).
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
// conditions list comes from the wire (raw map values) rather than from
// runtime.newCondition (typed Condition values).
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
		ConditionType: read("type"),
		Status:        read("status"),
		Reason:        read("reason"),
		Message:       read("message"),
	}
}
