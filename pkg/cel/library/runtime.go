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

// Condition field keys: the only keys allowed in runtime.newCondition's input
// and the only fields exposed on a Condition value.
const (
	conditionKeyType    = "type"
	conditionKeyStatus  = "status"
	conditionKeyReason  = "reason"
	conditionKeyMessage = "message"
)

// Condition status values: the only literals allowed for a Condition's status
// field. Match metav1.Condition status values.
const (
	conditionStatusTrue    = "True"
	conditionStatusFalse   = "False"
	conditionStatusUnknown = "Unknown"
)

// validConditionStatuses is the set form of the conditionStatus* constants,
// for membership checks.
var validConditionStatuses = map[string]struct{}{
	conditionStatusTrue:    {},
	conditionStatusFalse:   {},
	conditionStatusUnknown: {},
}

// expectedConditionKeys is the set form of the conditionKey* constants, for
// membership checks.
var expectedConditionKeys = map[string]struct{}{
	conditionKeyType:    {},
	conditionKeyStatus:  {},
	conditionKeyReason:  {},
	conditionKeyMessage: {},
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
		cel.Types(runtimeType),
		cel.Variable(RuntimeVarName, runtimeType),

		// runtime.newCondition({type: ..., status: ..., reason: ..., message: ...}) -> Condition
		//
		// Authors write the input as a struct-style map literal with bare
		// identifier keys. A parse-time macro (registered below) rewrites
		// identifier keys into string literals before type-checking sees
		// them, so the function's declared signature can stay
		// map(string, string). The macro also rejects unknown keys,
		// duplicate keys, and missing required keys (type, status).
		//
		// The return type is the typed Condition. Field access (cond.status,
		// cond.type, ...) resolves through ConditionTypeProvider, which
		// declares Condition's four string fields, so a misspelled field is
		// rejected at type-check time rather than at runtime.
		cel.Function("newCondition",
			cel.MemberOverload("runtime_newCondition_map",
				[]*cel.Type{runtimeType, mapType},
				conditionType,
				cel.BinaryBinding(newConditionImpl),
			),
		),

		// runtime.condition(obj, type) -> Condition (see comment above)
		cel.Function("condition",
			cel.MemberOverload("runtime_condition_dyn_string",
				[]*cel.Type{runtimeType, cel.DynType, cel.StringType},
				conditionType,
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
// argument map. It enforces the four allowed-key rule, the required-keys
// rule, and prohibits quoted/computed keys, with errors reported at the
// source position of the offending node. After validation, identifier keys
// are rewritten as string literals so the downstream type-checker sees a
// plain map(string, string). Status-value validation is handled separately:
// literal values at build time by ValidateConditionExpressions, and all
// values (literal or computed) at runtime by newConditionImpl.
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

// -------------------------------------------------------------------------
// runtime.newCondition implementation
// -------------------------------------------------------------------------

// newConditionImpl is the runtime binding for runtime.newCondition. By the
// time it executes, the parse-time macro has enforced the key rules (allowed
// keys, no duplicates, type and status present) and ValidateConditionExpressions
// has checked any literal status. The status check here is the catch-all for
// computed values (e.g. status: schema.spec.healthy ? 'True' : 'False') that
// aren't literals; everything else is straightforward map -> struct construction.
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
			cond.ConditionType = string(valStr)
		case conditionKeyStatus:
			s := string(valStr)
			if _, ok := validConditionStatuses[s]; !ok {
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
		ConditionType: read(conditionKeyType),
		Status:        read(conditionKeyStatus),
		Reason:        read(conditionKeyReason),
		Message:       read(conditionKeyMessage),
	}
}

// -------------------------------------------------------------------------
// Condition type provider
// -------------------------------------------------------------------------

// ConditionTypeProvider wraps a base types.Provider so the CEL type-checker
// can resolve the kro.run.Condition type and its four string fields
// (type, status, reason, message). Everything else delegates to base.
//
// This is what lets runtime.newCondition / runtime.condition declare a typed
// Condition return (instead of dyn) while still allowing cond.status-style
// field access to type-check. A misspelled field is rejected at check time
// rather than silently passing as it would under dyn.
//
// CEL's cel.CustomTypeProvider replaces the environment's provider outright,
// so the condition-aware provider must be the outermost wrapper and delegate
// unhandled lookups to the provider it wraps (e.g. kro's schema-derived
// DeclTypeProvider).
func ConditionTypeProvider(base types.Provider) types.Provider {
	return &conditionTypeProvider{Provider: base}
}

type conditionTypeProvider struct {
	// Embedding the interface delegates EnumValue, FindIdent, and NewValue
	// to the wrapped provider; only the struct-resolution methods below are
	// overridden.
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

// -------------------------------------------------------------------------
// Build-time condition validation
// -------------------------------------------------------------------------

// ValidateConditionExpressions performs build-time AST inspection on a
// list of author-defined condition expressions. It enforces two rules:
//
//   - The self-reference rule: runtime.condition(_, 'X') where X is a
//     literal matching a custom-defined type from runtime.newCondition is
//     rejected, since custom conditions cannot reference each other.
//   - The literal-status rule: runtime.newCondition({status: 'X'}) where X
//     is a literal must be one of True, False, Unknown. Computed status
//     values are checked at runtime by newConditionImpl instead.
//
// Key/required-field rules for runtime.newCondition are enforced earlier
// by the parse-time macro above. By the time expressions reach this
// validator the macro has already rewritten identifier-keyed map literals
// to string-keyed ones, so the helpers below find entries via
// literal-string keys.
//
// Returns the first error encountered, or nil if all expressions pass.
func ValidateConditionExpressions(env *cel.Env, expressions []string) error {
	// First pass: collect literal type values from all newCondition calls
	// so the second pass can detect self-references.
	customTypes := map[string]struct{}{}
	for _, expr := range expressions {
		parsed, iss := env.Parse(expr)
		if iss.Err() != nil {
			// Parse errors aren't this validator's responsibility; let the
			// regular CEL parse step surface them.
			continue
		}
		collectCustomTypes(parsed.NativeRep().Expr(), customTypes)
	}

	for _, expr := range expressions {
		parsed, iss := env.Parse(expr)
		if iss.Err() != nil {
			continue
		}
		if err := validateExpressionAST(parsed.NativeRep().Expr(), customTypes, expr); err != nil {
			return err
		}
	}

	return nil
}

// collectCustomTypes walks the AST looking for runtime.newCondition({type: 'X'})
// calls with literal-string type values, and adds the type names to out.
func collectCustomTypes(expr ast.Expr, out map[string]struct{}) {
	walk(expr, nil, func(e ast.Expr) {
		if !isRuntimeCall(e, "newCondition") {
			return
		}
		args := e.AsCall().Args()
		if len(args) != 1 {
			return
		}
		typeVal, ok := mapLiteralEntryStringValue(args[0], conditionKeyType)
		if !ok {
			return
		}
		out[typeVal] = struct{}{}
	})
}

// validateExpressionAST walks expr applying the self-reference rule and the
// literal-status rule. Returns the first error found.
func validateExpressionAST(expr ast.Expr, customTypes map[string]struct{}, exprText string) error {
	var firstErr error
	walk(expr, &firstErr, func(e ast.Expr) {
		switch {
		case isRuntimeCall(e, "condition"):
			if err := validateCondition(e, customTypes, exprText); err != nil {
				firstErr = err
			}
		case isRuntimeCall(e, "newCondition"):
			if err := validateNewConditionStatus(e, exprText); err != nil {
				firstErr = err
			}
		}
	})
	return firstErr
}

// validateNewConditionStatus enforces the literal-status rule: when
// runtime.newCondition's status entry is a literal string it must be one of
// True, False, Unknown. Non-literal status values are left for the runtime
// check in newConditionImpl.
func validateNewConditionStatus(call ast.Expr, exprText string) error {
	args := call.AsCall().Args()
	if len(args) != 1 {
		return nil // Wrong arity is a CEL type-checker concern.
	}
	status, ok := mapLiteralEntryStringValue(args[0], conditionKeyStatus)
	if !ok {
		return nil // Missing or non-literal status; checked elsewhere.
	}
	if _, valid := validConditionStatuses[status]; !valid {
		return fmt.Errorf(
			"runtime.newCondition: status must be one of True, False, Unknown (got %q) in expression %q",
			status, exprText,
		)
	}
	return nil
}

// validateCondition enforces the self-reference rule: runtime.condition(_, 'X')
// where X is a literal that matches a custom-defined type is rejected.
func validateCondition(call ast.Expr, customTypes map[string]struct{}, exprText string) error {
	args := call.AsCall().Args()
	if len(args) != 2 {
		return nil // Wrong arity is a CEL type-checker concern.
	}

	typeArg := args[1]
	typeName, ok := literalString(typeArg)
	if !ok {
		return nil // Dynamic type; runtime check.
	}

	if _, isCustom := customTypes[typeName]; isCustom {
		return fmt.Errorf(
			"runtime.condition(_, %q): custom conditions cannot reference each other in expression %q",
			typeName, exprText,
		)
	}

	return nil
}

// walk recursively visits expr and every sub-expression, calling visit for
// each. Order is pre-order: parent visited before children.
//
// If errPtr is non-nil, traversal aborts as soon as *errPtr becomes non-nil.
// Pass nil for exhaustive traversal (e.g., when the visitor cannot fail).
func walk(expr ast.Expr, errPtr *error, visit func(ast.Expr)) {
	if expr == nil || (errPtr != nil && *errPtr != nil) {
		return
	}
	visit(expr)

	switch expr.Kind() {
	case ast.CallKind:
		call := expr.AsCall()
		if call.Target() != nil {
			walk(call.Target(), errPtr, visit)
		}
		for _, arg := range call.Args() {
			walk(arg, errPtr, visit)
		}
	case ast.SelectKind:
		walk(expr.AsSelect().Operand(), errPtr, visit)
	case ast.ListKind:
		for _, elem := range expr.AsList().Elements() {
			walk(elem, errPtr, visit)
		}
	case ast.MapKind:
		for _, entry := range expr.AsMap().Entries() {
			me := entry.AsMapEntry()
			walk(me.Key(), errPtr, visit)
			walk(me.Value(), errPtr, visit)
		}
	case ast.StructKind:
		for _, field := range expr.AsStruct().Fields() {
			sf := field.AsStructField()
			walk(sf.Value(), errPtr, visit)
		}
	case ast.ComprehensionKind:
		comp := expr.AsComprehension()
		walk(comp.IterRange(), errPtr, visit)
		walk(comp.AccuInit(), errPtr, visit)
		walk(comp.LoopCondition(), errPtr, visit)
		walk(comp.LoopStep(), errPtr, visit)
		walk(comp.Result(), errPtr, visit)
	}
}

// isRuntimeCall reports whether expr is a call of the form
// runtime.<methodName>(...).
func isRuntimeCall(expr ast.Expr, methodName string) bool {
	if expr.Kind() != ast.CallKind {
		return false
	}
	call := expr.AsCall()
	if !call.IsMemberFunction() || call.FunctionName() != methodName {
		return false
	}
	target := call.Target()
	if target == nil || target.Kind() != ast.IdentKind {
		return false
	}
	return target.AsIdent() == RuntimeVarName
}

// literalString returns the string value of a literal-string AST node, or
// ("", false) if the node isn't a literal string.
func literalString(expr ast.Expr) (string, bool) {
	if expr.Kind() != ast.LiteralKind {
		return "", false
	}
	val := expr.AsLiteral().Value()
	s, ok := val.(string)
	return s, ok
}

// mapLiteralEntryStringValue looks up key in a map literal's entries and
// returns the entry's value if it's a literal string. Returns ("", false)
// if expr isn't a map literal, the key isn't present, or the value isn't a
// literal string.
func mapLiteralEntryStringValue(expr ast.Expr, key string) (string, bool) {
	if expr.Kind() != ast.MapKind {
		return "", false
	}
	for _, entry := range expr.AsMap().Entries() {
		me := entry.AsMapEntry()
		k, ok := literalString(me.Key())
		if !ok || k != key {
			continue
		}
		return literalString(me.Value())
	}
	return "", false
}
