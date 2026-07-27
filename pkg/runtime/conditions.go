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
	"k8s.io/apiserver/pkg/cel/openapi"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/cel/library"
	"github.com/kubernetes-sigs/kro/pkg/cel/unstructured"
	"github.com/kubernetes-sigs/kro/pkg/graph"
)

// ErrConditionEvaluationDegraded indicates that one or more author condition
// expressions failed evaluation or produced duplicate types. The surviving
// conditions are still returned; callers reflect the failure on the wire
// (state: Error) without aborting the reconcile.
var ErrConditionEvaluationDegraded = errors.New("author condition evaluation degraded")

// HasConditions reports whether this node has author-defined condition
// expressions. Only the instance node ever returns true.
func (n *Node) HasConditions() bool {
	return len(n.conditionExprs) > 0
}

// EvaluateConditions evaluates the author-defined condition expressions and
// returns the resulting conditions flattened in declaration order.
//
// kroBuiltins are kro's built-in conditions as computed for this reconcile,
// bound to schema.status.conditions so runtime.condition(schema, _) reads
// kro's internal value even when the author overrides a built-in type.
//
// Failures are per-expression: pending data is skipped silently, fatal
// errors and duplicate condition types drop that output and wrap
// ErrConditionEvaluationDegraded in err. incomplete reports whether any
// expression's output is missing for either reason, so the caller can
// preserve previously persisted conditions.
func (n *Node) EvaluateConditions(logger logr.Logger, kroBuiltins []v1alpha1.Condition) (conditions []library.Condition, incomplete bool, err error) {
	if len(n.conditionExprs) == 0 {
		return nil, false, nil
	}

	ctx := n.buildContext()
	// The instance is not its own graph dependency, so buildContext provides
	// neither schema nor runtime; bind them here.
	ctx[graph.SchemaVarName] = n.schemaForConditions(kroBuiltins)
	ctx[library.RuntimeVarName] = library.RuntimeSingleton

	var out []library.Condition
	var failures []string
	for _, expr := range n.conditionExprs {
		// Evaluate the Program directly: krocel.Expression.Eval converts to
		// Go-native values, which cannot represent the typed Condition.
		filteredCtx := filterContext(ctx, expr.Expression.References)
		raw, _, evalErr := expr.Expression.Program.Eval(filteredCtx)
		if evalErr != nil {
			if isCELDataPending(evalErr) {
				incomplete = true
				continue
			}
			logger.Error(evalErr, "skipping author condition expression with fatal evaluation error",
				"expression", expr.Expression.UserExpression())
			failures = append(failures, fmt.Sprintf("%q: %v", expr.Expression.UserExpression(), evalErr))
			continue
		}

		conds, flattenErr := flattenCelConditionValue(raw, expr.Expression.UserExpression())
		if flattenErr != nil {
			logger.Error(flattenErr, "skipping author condition expression with malformed result",
				"expression", expr.Expression.UserExpression())
			failures = append(failures, fmt.Sprintf("%q: %v", expr.Expression.UserExpression(), flattenErr))
			continue
		}
		out = append(out, conds...)
	}

	out, dups := dedupConditionTypes(out)
	if len(dups) > 0 {
		logger.Info("dropping author conditions with duplicate types", "types", dups)
		failures = append(failures, fmt.Sprintf("duplicate condition type(s) %v dropped", dups))
	}

	if len(failures) > 0 {
		return out, true, fmt.Errorf("%w: %s", ErrConditionEvaluationDegraded, strings.Join(failures, "; "))
	}
	return out, incomplete, nil
}

// schemaForConditions builds the value bound to the `schema` CEL variable
// when evaluating author conditions: the instance's spec/metadata (status
// stripped, matching every other CEL eval path) plus a synthesized
// status.conditions[] holding kro's built-in conditions for
// runtime.condition(schema, _) lookups.
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
	// The instance's resourceSchema is spec-only; re-wrap so spec/metadata
	// access stays schema-aware while status falls through dynamically.
	return unstructured.UnstructuredToVal(obj, &openapi.Schema{Schema: n.resourceSchema})
}

// kroBuiltinsAsList converts built-in conditions to plain maps carrying the
// fields runtime.condition exposes, so they take the conditionFromMap path.
func kroBuiltinsAsList(conds []v1alpha1.Condition) []any {
	out := make([]any, 0, len(conds))
	for _, c := range conds {
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
		out = append(out, entry)
	}
	return out
}

// dedupConditionTypes removes every occurrence of any condition type that
// appears more than once, returning the kept conditions and the sorted list
// of dropped types. Uniqueness is enforced at runtime because collection
// expansion can produce types that are not known until evaluation.
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

// flattenCelConditionValue extracts Condition values from a CEL result: a
// single Condition, or a list of Conditions for collection expansion.
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
