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

package runtime

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/go-logr/logr"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/common/types/traits"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/cel/openapi"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/cel/library"
	"github.com/kubernetes-sigs/kro/pkg/cel/unstructured"
	"github.com/kubernetes-sigs/kro/pkg/graph"
)

// ErrConditionEvaluationDegraded indicates that one or more author
// condition expressions failed evaluation but the surviving conditions
// were returned. Callers should treat this as a non-fatal signal and
// reflect it on the wire (typically by setting state: Error) without
// aborting the reconcile.
var ErrConditionEvaluationDegraded = errors.New("author condition evaluation degraded")

// HasConditions reports whether this node has author-defined condition
// expressions to evaluate. Only the instance node ever returns true.
func (n *Node) HasConditions() bool {
	return len(n.conditionExprs) > 0
}

// EvaluateConditions evaluates each author-defined condition expression
// and returns the resulting Condition values flattened in declaration
// order.
//
// kroBuiltins are kro's internal built-in conditions (InstanceManaged,
// GraphResolved, ResourcesReady, Ready) as the controller computed them
// for this reconcile. They are exposed to author CEL via
// runtime.condition(schema, 'X'); this lookup return kro's internal
// value for the four reserved names regardless of whether the
// author has overridden any of them on the wire.
//
// Failure handling is per-condition, not per-reconcile:
//
//   - DataPending failures (e.g. "no such key") are silent, matching
//     softResolve for status fields. The condition reappears once the
//     missing data becomes available.
//   - Fatal CEL errors drop just that expression's output and surface
//     ErrConditionEvaluationDegraded; other expressions still produce
//     conditions normally.
//   - Duplicate condition types are dropped (every occurrence, since there
//     is no principled tiebreaker) and ErrConditionEvaluationDegraded is
//     surfaced.
//
// In every degraded case the caller sets state: Error on the wire and
// persists the surviving conditions; the reconcile itself is not failed.
func (n *Node) EvaluateConditions(logger logr.Logger, kroBuiltins []v1alpha1.Condition) ([]library.Condition, error) {
	if len(n.conditionExprs) == 0 {
		return nil, nil
	}

	ctx := n.buildContext()

	// Inject the `schema` variable directly. Author conditions evaluate on
	// the instance node, but the instance is not its own dep (the DAG has
	// no self-loops). Synthesize a schema map carrying spec/metadata plus
	// a status with kro's built-in conditions, so runtime.condition(schema,
	// 'X') resolves to kro's internal value for the four reserved names.
	ctx[graph.SchemaVarName] = n.schemaForConditions(kroBuiltins)

	// runtime is the CEL library variable used by author conditions.
	// It isn't a graph dep, so buildContext doesn't add it; inject here.
	ctx[library.RuntimeVarName] = library.RuntimeSingleton

	var out []library.Condition
	var failures []string
	for _, expr := range n.conditionExprs {
		// Bypass krocel.Expression.Eval here: it converts the result to a
		// Go-native value via GoNativeType, which doesn't know how to
		// handle our custom Condition CEL type. We want the typed CEL
		// value directly so flattenCelConditionValue can extract
		// Conditions from either *Condition or list(*Condition).
		filteredCtx := filterContext(ctx, expr.Expression.References)
		raw, _, err := expr.Expression.Program.Eval(filteredCtx)
		if err != nil {
			if isCELDataPending(err) {
				continue // silent omit; see failure-handling note above
			}
			logger.Error(err, "skipping author condition expression with fatal evaluation error",
				"expression", expr.Expression.UserExpression())
			failures = append(failures, fmt.Sprintf("%q: %v", expr.Expression.UserExpression(), err))
			continue
		}

		conds, err := flattenCelConditionValue(raw, expr.Expression.UserExpression())
		if err != nil {
			logger.Error(err, "skipping author condition expression with malformed result",
				"expression", expr.Expression.UserExpression())
			failures = append(failures, fmt.Sprintf("%q: %v", expr.Expression.UserExpression(), err))
			continue
		}
		out = append(out, conds...)
	}

	out, dups := dedupConditionTypes(out)
	if len(dups) > 0 {
		logger.Info("dropping author conditions with duplicate types",
			"types", dups)
		failures = append(failures, fmt.Sprintf("duplicate condition type(s) %v dropped", dups))
	}

	if len(failures) > 0 {
		return out, fmt.Errorf("%w: %s", ErrConditionEvaluationDegraded, strings.Join(failures, "; "))
	}
	return out, nil
}

// schemaForConditions builds the value to bind to the `schema` CEL
// variable when evaluating author conditions on the instance node. It
// carries the instance's spec/metadata (status stripped, matching every
// other CEL eval path on kro) plus a synthesized status.conditions[]
// containing kro's internal built-in conditions so author expressions
// can read them via runtime.condition(schema, 'X').
//
// The synthetic conditions are written as plain map[string]interface{} so
// they hit conditionFromMap in extractConditions, keeping the
// runtime.condition implementation single-source.
func (n *Node) schemaForConditions(kroBuiltins []v1alpha1.Condition) any {
	if len(n.observed) == 0 {
		return map[string]any{
			"status": map[string]any{
				"conditions": kroBuiltinsAsList(kroBuiltins),
			},
		}
	}

	obj := withStatusOmitted(n.observed[0].Object)
	obj["status"] = map[string]any{
		"conditions": kroBuiltinsAsList(kroBuiltins),
	}

	if n.resourceSchema == nil {
		return obj
	}
	// resourceSchema for the instance node is the spec-only schema set by
	// the builder via getSchemaWithoutStatus. Re-wrap so spec/metadata
	// access remains schema-aware; status access falls through dynamically
	// since the schema doesn't declare `status` for the instance variable.
	return unstructured.UnstructuredToVal(obj, &openapi.Schema{Schema: n.resourceSchema})
}

func kroBuiltinsAsList(conds []v1alpha1.Condition) []any {
	out := make([]any, 0, len(conds))
	for _, c := range conds {
		entry := map[string]any{
			"type":   string(c.Type),
			"status": string(c.Status),
		}
		if c.Reason != nil {
			entry["reason"] = *c.Reason
		} else {
			entry["reason"] = ""
		}
		if c.Message != nil {
			entry["message"] = *c.Message
		} else {
			entry["message"] = ""
		}
		if c.LastTransitionTime != nil {
			entry["lastTransitionTime"] = c.LastTransitionTime.Format(v1.RFC3339Micro)
		}
		entry["observedGeneration"] = c.ObservedGeneration
		out = append(out, entry)
	}
	return out
}

// dedupConditionTypes removes every occurrence of any condition type that
// appears more than once and returns the kept conditions plus a sorted list
// of dropped types. Uniqueness is enforced at runtime rather than build time
// because collection-expansion expressions (e.g. servers.map(s,
// runtime.newCondition({type: 'X' + s.name + 'Ready', ...}))) produce types
// that aren't known until evaluation. Both copies are dropped because there
// is no principled tiebreaker.
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
	// Sort so log output and error messages are deterministic.
	sort.Strings(dups)
	return kept, dups
}

// flattenCelConditionValue extracts Condition values from a CEL ref.Val.
// The expression is expected to return either a *library.Condition (the
// common case) or a CEL list whose elements are *library.Condition (the
// collection-expansion case).
func flattenCelConditionValue(val ref.Val, exprText string) ([]library.Condition, error) {
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
