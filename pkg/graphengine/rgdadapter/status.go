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

// Package rgdadapter – instance status projection.
//
// After executor.Apply has reconciled the Graph nodes, the runtime scope
// holds each node's observed value.  ProjectInstanceStatus evaluates the
// RGD's spec.schema.status CEL expressions against that scope to produce
// the instance's status map.  ProjectInstanceConditions evaluates the
// status.conditions block that uses runtime.newCondition(…).
package rgdadapter

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/common/types/traits"
	"k8s.io/apiserver/pkg/cel/openapi"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	krocel "github.com/kubernetes-sigs/kro/pkg/cel"
	"github.com/kubernetes-sigs/kro/pkg/cel/library"
	celunstructured "github.com/kubernetes-sigs/kro/pkg/cel/unstructured"
	"github.com/kubernetes-sigs/kro/pkg/graph"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/runtime"
	"github.com/kubernetes-sigs/kro/pkg/runtime/resolver"
)

// ProjectInstanceStatus evaluates the RGD's spec.schema.status CEL
// expressions against the reconciled runtime scope and returns a flat
// map[string]any that callers can patch onto the instance's .status.
//
// The routine:
//  1. Unmarshals RGD.Spec.Schema.Status.Raw.
//  2. Calls graph.ParseStatusExpressions to separate condition fields from
//     regular fields (and strip the ${…} wrappers).
//  3. Builds a transient CEL environment containing every Graph-node ID
//     in the runtime scope as an untyped (dyn) variable.
//  4. Compiles and evaluates each field expression against rt.Scope().
//  5. Sets the result at the field's path in the output map.
//
// The function is intentionally schemaless (dyn) at projection time: we
// have already proved the expressions are valid at Graph-compile time; we
// only need their runtime values here.
//
// status.conditions entries are excluded — call ProjectInstanceConditions
// for those.
func ProjectInstanceStatus(
	rt *runtime.Runtime,
	rgd *v1alpha1.ResourceGraphDefinition,
) (map[string]any, error) {
	if rt == nil {
		return nil, fmt.Errorf("status projection: runtime is required")
	}
	if rgd == nil {
		return nil, fmt.Errorf("status projection: rgd is required")
	}

	statusMap, err := unmarshalStatusRaw(rgd)
	if err != nil {
		return nil, err
	}
	if statusMap == nil {
		// No status block defined.
		return map[string]any{}, nil
	}

	// Parse: separates conditions from plain fields; mutates statusMap to
	// remove the conditions key.
	fields, _, noExprFields, err := graph.ParseStatusExpressions(statusMap)
	if err != nil {
		return nil, fmt.Errorf("status projection: parse: %w", err)
	}
	if len(fields) == 0 && len(noExprFields) == 0 {
		return map[string]any{}, nil
	}

	// Declare every node ID (not just published scope keys) so a field that
	// references a not-yet-applied node (includeWhen=false, or data pending)
	// surfaces as a runtime data-pending error we can skip per-field, instead
	// of a compile-time undeclared-reference error that fails the whole
	// projection and drops sibling fields that DID resolve.
	env, err := buildStatusEnvForNodes(rt, false)
	if err != nil {
		return nil, fmt.Errorf("status projection: build env: %w", err)
	}

	// Use schema-aware scope so CEL sees correctly typed values
	// (e.g. Secret.data as bytes for string(bytes) conversions).
	saScope := schemaAwareScope(rt.Scope(), rt)

	result := make(map[string]any, len(fields))
	for _, f := range fields {
		val, err := evalStatusExpr(env, saScope, f.Expression)
		if err != nil {
			if isDataPendingCEL(err) {
				// Dependency not observable this cycle: drop the field so a
				// field whose dependency is unavailable disappears rather
				// than failing the whole projection.
				continue
			}
			return nil, fmt.Errorf("status projection: field %q: %w", f.Path, err)
		}
		if err := setAtPath(result, f.Path, val); err != nil {
			return nil, fmt.Errorf("status projection: set %q: %w", f.Path, err)
		}
	}
	// Literal (expression-free) status fields copy through unchanged.
	for _, path := range noExprFields {
		val, ok := getAtPath(statusMap, path)
		if !ok {
			continue
		}
		if err := setAtPath(result, path, val); err != nil {
			return nil, fmt.Errorf("status projection: set literal %q: %w", path, err)
		}
	}
	return result, nil
}

// ErrConditionProjectionDegraded indicates that one or more author condition
// expressions failed evaluation fatally or produced duplicate types. The
// surviving conditions are still returned; callers reflect the failure on
// the wire (state: Error) without aborting the reconcile.
var ErrConditionProjectionDegraded = errors.New("author condition evaluation degraded")

// ProjectInstanceConditions evaluates the status.conditions expressions
// (runtime.newCondition(…) calls) against the reconciled runtime scope and
// returns the surviving author conditions flattened in declaration order.
//
// kroBuiltins are kro's built-in conditions as computed for this reconcile,
// bound to schema.status.conditions so runtime.condition(schema, _) reads
// kro's internal value even when the author overrides a built-in type.
//
// Failures are per-expression: pending data is skipped silently
// (incomplete=true), fatal errors and duplicate condition types drop that
// output and wrap ErrConditionProjectionDegraded in the returned error, so
// the caller preserves previously persisted conditions for the missing types.
//
// The Graph engine's compiler passes WithRuntimeLibrary(false), so the
// compiled Program has no programs for condition expressions; we re-compile
// them here against a fresh env that includes library.Runtime().
func ProjectInstanceConditions(
	rt *runtime.Runtime,
	rgd *v1alpha1.ResourceGraphDefinition,
	kroBuiltins []v1alpha1.Condition,
) (conditions []library.Condition, incomplete bool, err error) {
	if rt == nil {
		return nil, false, fmt.Errorf("condition projection: runtime is required")
	}
	if rgd == nil {
		return nil, false, fmt.Errorf("condition projection: rgd is required")
	}

	statusMap, err := unmarshalStatusRaw(rgd)
	if err != nil {
		return nil, false, err
	}
	if statusMap == nil {
		return nil, false, nil
	}

	// ParseStatusExpressions mutates statusMap to extract + remove the
	// conditions block.  We only care about conditionExprs here.
	_, conditionExprs, _, err := graph.ParseStatusExpressions(statusMap)
	if err != nil {
		return nil, false, fmt.Errorf("condition projection: parse: %w", err)
	}
	if len(conditionExprs) == 0 {
		return nil, false, nil
	}

	// Build a CEL env WITH library.Runtime() so runtime.newCondition compiles.
	// All node IDs are declared (see ProjectInstanceStatus) so a condition
	// referencing an unpublished node is a skippable data-pending error.
	env, err := buildStatusEnvForNodes(rt, true)
	if err != nil {
		return nil, false, fmt.Errorf("condition projection: build env: %w", err)
	}

	// Evaluation scope: schema-aware values for typed nodes, the `runtime`
	// singleton, and the `schema` node overlaid so its status.conditions holds
	// this reconcile's kro built-ins for runtime.condition(schema, _) lookups.
	saScope := schemaAwareScope(rt.Scope(), rt)
	scope := make(map[string]any, len(saScope)+1)
	for k, v := range saScope {
		scope[k] = v
	}
	scope[SchemaNodeID] = schemaWithBuiltinConditions(rt.Scope()[SchemaNodeID], kroBuiltins)
	scope[library.RuntimeVarName] = library.RuntimeSingleton

	var out []library.Condition
	var failures []string
	for _, rawExpr := range conditionExprs {
		inner := unwrapExpr(rawExpr)
		raw, evalErr := evalConditionRaw(env, scope, inner)
		if evalErr != nil {
			if isDataPendingCEL(evalErr) {
				incomplete = true
				continue
			}
			failures = append(failures, fmt.Sprintf("%q: %v", rawExpr, evalErr))
			continue
		}
		conds, flattenErr := flattenConditionValue(raw, rawExpr)
		if flattenErr != nil {
			failures = append(failures, flattenErr.Error())
			continue
		}
		out = append(out, conds...)
	}

	out, dups := dedupConditionTypes(out)
	if len(dups) > 0 {
		failures = append(failures, fmt.Sprintf("duplicate condition type(s) %v dropped", dups))
	}
	if len(failures) > 0 {
		return out, true, fmt.Errorf("%w: %s", ErrConditionProjectionDegraded, strings.Join(failures, "; "))
	}
	return out, incomplete, nil
}

// ── helpers ──────────────────────────────────────────────────────────────────

// schemaWithBuiltinConditions returns the value bound to the `schema` CEL
// variable for author-condition evaluation: the instance's spec/metadata
// with status replaced by a synthesized status.conditions[] holding kro's
// built-in conditions.
func schemaWithBuiltinConditions(schemaVal any, kroBuiltins []v1alpha1.Condition) any {
	builtinList := make([]any, 0, len(kroBuiltins))
	for _, c := range kroBuiltins {
		entry := map[string]any{
			"type":    string(c.Type),
			"status":  string(c.Status),
			"reason":  "",
			"message": "",
		}
		if c.Reason != nil {
			entry["reason"] = *c.Reason
		}
		if c.Message != nil {
			entry["message"] = *c.Message
		}
		builtinList = append(builtinList, entry)
	}
	status := map[string]any{"conditions": builtinList}

	obj, ok := schemaVal.(map[string]any)
	if !ok {
		return map[string]any{"status": status}
	}
	out := make(map[string]any, len(obj))
	for k, v := range obj {
		out[k] = v
	}
	out["status"] = status
	return out
}

// schemaAwareScope returns a copy of rawScope where each value that has a
// corresponding OpenAPI schema in prog.NodeSchemas is wrapped with
// celunstructured.UnstructuredToVal for schema-aware CEL type conversion
// (e.g. Secret.data values are typed as bytes so string(bytes) works).
// Values without a matching schema are passed through unchanged.
func schemaAwareScope(rawScope map[string]any, rt *runtime.Runtime) map[string]any {
	if rt == nil || rt.Program() == nil {
		return rawScope
	}
	nodeSchemas := rt.Program().NodeSchemas
	if len(nodeSchemas) == 0 {
		return rawScope
	}
	out := make(map[string]any, len(rawScope))
	for k, v := range rawScope {
		sc, ok := nodeSchemas[k]
		if !ok || sc == nil {
			out[k] = v
			continue
		}
		switch val := v.(type) {
		case map[string]any:
			out[k] = celunstructured.UnstructuredToVal(val, &openapi.Schema{Schema: sc})
		case []any:
			// Collection nodes publish a []any and carry a list-wrapped schema
			// (Type=array, Items.Schema=element). Each item is an element
			// object, so it must be wrapped with the ELEMENT schema, not the
			// array wrapper — wrapping an object with the array schema makes
			// UnstructuredToVal reject it ("expected an array").
			itemSchema := sc
			if sc.Type.Contains("array") && sc.Items != nil && sc.Items.Schema != nil {
				itemSchema = sc.Items.Schema
			}
			list := make([]any, len(val))
			for i, item := range val {
				if m, ok := item.(map[string]any); ok {
					list[i] = celunstructured.UnstructuredToVal(m, &openapi.Schema{Schema: itemSchema})
				} else {
					list[i] = item
				}
			}
			out[k] = list
		default:
			out[k] = v
		}
	}
	return out
}

// unmarshalStatusRaw decodes RGD.Spec.Schema.Status.Raw into a map.
// Returns (nil, nil) when no status block is defined.
func unmarshalStatusRaw(rgd *v1alpha1.ResourceGraphDefinition) (map[string]interface{}, error) {
	if rgd.Spec.Schema == nil {
		return nil, nil
	}
	raw := rgd.Spec.Schema.Status.Raw
	if len(raw) == 0 {
		return nil, nil
	}
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("status projection: unmarshal status: %w", err)
	}
	return m, nil
}

// buildStatusEnvForNodes constructs a transient CEL environment whose
// variable declarations cover every node ID of the runtime's program,
// whether or not the node has published into scope yet. This keeps a
// reference to a not-yet-applied node a runtime data-pending error rather
// than a compile-time undeclared-reference error.
//
// Collection nodes (forEach templates, selector externalRefs) are declared
// as list(dyn) rather than the scalar `any` type, so status
// expressions that range over them — filter / map / sortBy — type-check
// (CEL rejects `any` as a comprehension range; it must be list, map, or
// dyn). Scalar nodes stay `any`, matching the schemaless projection contract.
func buildStatusEnvForNodes(rt *runtime.Runtime, includeRuntime bool) (*cel.Env, error) {
	nodes := rt.Nodes()
	scalarIDs := make([]string, 0, len(nodes))
	var listIDs []string
	for _, n := range nodes {
		if n.IsCollection() {
			listIDs = append(listIDs, n.ID())
			continue
		}
		scalarIDs = append(scalarIDs, n.ID())
	}
	opts := []krocel.EnvOption{
		krocel.WithResourceIDs(scalarIDs),
		krocel.WithRuntimeLibrary(includeRuntime),
	}
	if len(listIDs) > 0 {
		opts = append(opts, krocel.WithListVariables(listIDs))
	}
	return krocel.DefaultEnvironment(opts...)
}

// getAtPath reads the value at the dotted path in m, navigating
// map[string]any levels only. Only simple dot-separated paths (no array
// indices) are supported — same limitation as setAtPath. A non-map
// intermediate is treated as not-found.
func getAtPath(m map[string]any, path string) (any, bool) {
	r := resolver.NewResolver(m, nil)
	v, err := r.GetValueAtPath(path)
	if err != nil {
		return nil, false
	}
	return v, true
}

// compileCEL parses, type-checks, and programs a plain CEL expression (no
// ${…} wrapper) against env, wrapping each stage's error with expr context.
func compileCEL(env *cel.Env, expr string) (cel.Program, error) {
	parsed, issues := env.Parse(expr)
	if issues != nil && issues.Err() != nil {
		return nil, fmt.Errorf("parse %q: %w", expr, issues.Err())
	}
	checked, issues := env.Check(parsed)
	if issues != nil && issues.Err() != nil {
		return nil, fmt.Errorf("check %q: %w", expr, issues.Err())
	}
	prog, err := env.Program(checked, krocel.DefaultProgramOptions()...)
	if err != nil {
		return nil, fmt.Errorf("program %q: %w", expr, err)
	}
	return prog, nil
}

// evalStatusExpr compiles and evaluates a plain CEL expression (no ${…}
// wrapper) against scope.  Returns the Go-native result.
func evalStatusExpr(env *cel.Env, scope map[string]any, expr string) (any, error) {
	prog, err := compileCEL(env, expr)
	if err != nil {
		return nil, err
	}
	e := &krocel.Expression{Original: expr, Program: prog}
	return e.Eval(scope)
}

// evalConditionRaw compiles and evaluates a runtime.newCondition(…) expression
// and returns the raw CEL ref.Val for the caller to flatten. We call
// cel.Program.Eval directly (not krocel.Expression.Eval) because Go-native
// conversion does not know the custom *library.Condition ref.Val type.
func evalConditionRaw(env *cel.Env, scope map[string]any, expr string) (ref.Val, error) {
	prog, err := compileCEL(env, expr)
	if err != nil {
		return nil, err
	}
	out, _, err := prog.Eval(scope)
	if err != nil {
		return nil, fmt.Errorf("eval %q: %w", expr, err)
	}
	return out, nil
}

// flattenConditionValue extracts Condition values from a CEL result: a single
// Condition, or a list of Conditions for collection expansion.
func flattenConditionValue(val ref.Val, exprText string) ([]library.Condition, error) {
	if val == nil {
		return nil, fmt.Errorf("condition %q returned null", exprText)
	}
	if cond, ok := val.Value().(*library.Condition); ok {
		return []library.Condition{*cond}, nil
	}
	if lister, ok := val.(traits.Lister); ok {
		var out []library.Condition
		it := lister.Iterator()
		for it.HasNext() == types.True {
			elem := it.Next()
			cond, ok := elem.Value().(*library.Condition)
			if !ok {
				return nil, fmt.Errorf("condition %q list element is not a Condition (got %T)", exprText, elem.Value())
			}
			out = append(out, *cond)
		}
		return out, nil
	}
	return nil, fmt.Errorf("condition %q must return a Condition or list(Condition), got %v", exprText, val.Type().TypeName())
}

// dedupConditionTypes removes every occurrence of any condition type that
// appears more than once, returning the kept conditions and the sorted list
// of dropped types.
func dedupConditionTypes(conds []library.Condition) ([]library.Condition, []string) {
	counts := make(map[string]int, len(conds))
	for _, c := range conds {
		counts[c.ConditionType]++
	}
	kept := make([]library.Condition, 0, len(conds))
	dupSet := make(map[string]struct{})
	for _, c := range conds {
		if counts[c.ConditionType] > 1 {
			dupSet[c.ConditionType] = struct{}{}
			continue
		}
		kept = append(kept, c)
	}
	if len(dupSet) == 0 {
		return kept, nil
	}
	dups := make([]string, 0, len(dupSet))
	for t := range dupSet {
		dups = append(dups, t)
	}
	sort.Strings(dups)
	return kept, dups
}

// dataPendingPatterns are CEL error substrings meaning "required data not
// available yet" rather than an expression bug.
var dataPendingPatterns = []string{
	"no such key",
	"no such field",
	"no such attribute",
	"index out of bounds",
}

func isDataPendingCEL(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, p := range dataPendingPatterns {
		if strings.Contains(msg, p) {
			return true
		}
	}
	return false
}

// unwrapExpr strips ${...} wrappers from a CEL expression string.
// Standalone ${expr} → expr; bare expression → unchanged.
func unwrapExpr(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "${") && strings.HasSuffix(s, "}") {
		s = strings.TrimPrefix(s, "${")
		s = strings.TrimSuffix(s, "}")
	}
	return s
}

// setAtPath writes val at the path in m (supporting dot-separated paths
// and array indices like endpoints[0]), creating intermediate maps and slices
// as needed.
func setAtPath(m map[string]any, path string, val any) error {
	r := resolver.NewResolver(m, nil)
	return r.UpsertValueAtPath(path, val)
}
