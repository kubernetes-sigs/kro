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
	"maps"
	"slices"
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
	ctx[library.RuntimeVarName] = library.RuntimeSingleton

	// visible is what runtime.condition(schema, _) can see, growing as each entry
	// publishes, so a later entry reads this reconcile's value. filterContext
	// reads ctx[ref] at call time, so rebinding needs no recompilation.
	visible := kroBuiltinsAsList(kroBuiltins)
	ctx[graph.SchemaVarName] = n.schemaForConditions(visible)

	results := make([][]library.Condition, len(n.conditionExprs))
	noOutputEntries := make([]bool, len(n.conditionExprs))
	duplicatedTypes := map[string]struct{}{}
	publishedBy := map[string]int{}
	var failures []string

	// Ordering is what makes the dependency check sound: by the time an entry runs,
	// every entry that could declare what it depends on has been decided.
	for _, i := range n.conditionEvalOrder {
		entry := n.Spec.Conditions[i]
		exprText := entry.Expr.UserExpression()

		if reason, missing := missingDependency(entry.DependsOn, noOutputEntries, duplicatedTypes); missing {
			logger.V(1).Info("skipping author condition: a condition it depends on is unavailable",
				"expression", exprText, "reason", reason)
			noOutputEntries[i] = true
			incomplete = true
			continue
		}

		expr := n.conditionExprs[i]
		// Evaluate the Program directly: krocel.Expression.Eval converts to
		// Go-native values, which cannot represent the typed Condition.
		filteredCtx := filterContext(ctx, expr.Expression.References)
		raw, _, evalErr := expr.Expression.Program.Eval(filteredCtx)
		if evalErr != nil {
			noOutputEntries[i] = true
			if isCELDataPending(evalErr) {
				incomplete = true
				continue
			}
			logger.Error(evalErr, "skipping author condition expression with fatal evaluation error",
				"expression", exprText)
			failures = append(failures, fmt.Sprintf("%q: %v", exprText, evalErr))
			continue
		}

		conds, flattenErr := flattenCelConditionValue(raw, exprText)
		if flattenErr != nil {
			noOutputEntries[i] = true
			logger.Error(flattenErr, "skipping author condition expression with malformed result",
				"expression", exprText)
			failures = append(failures, fmt.Sprintf("%q: %v", exprText, flattenErr))
			continue
		}
		results[i] = conds

		// Publish immediately so the next entry reads these values, and withdraw a
		// duplicated type at the moment of collision, before a later entry can read
		// an ambiguous value.
		changed := false
		for _, c := range conds {
			if _, dup := publishedBy[c.ConditionType]; dup {
				duplicatedTypes[c.ConditionType] = struct{}{}
				visible = removeConditionEntry(visible, c.ConditionType)
				changed = true
				continue
			}
			publishedBy[c.ConditionType] = i
			// An override of a built-in type must not shadow it for lookups. The
			// override still reaches the wire via results[i].
			if _, builtin := v1alpha1.KROBuiltinConditionTypes[c.ConditionType]; builtin {
				continue
			}
			visible = upsertConditionEntry(visible, c)
			changed = true
		}
		if changed {
			ctx[graph.SchemaVarName] = n.schemaForConditions(visible)
		}
	}

	// Emit in declaration order. A duplicated type is dropped even from the entry
	// that published it; dropping rather than blanking lets the caller keep the
	// last good value.
	for i := range results {
		for _, c := range results[i] {
			if _, bad := duplicatedTypes[c.ConditionType]; bad {
				continue
			}
			conditions = append(conditions, c)
		}
	}

	if len(duplicatedTypes) > 0 {
		dups := slices.Sorted(maps.Keys(duplicatedTypes))
		logger.Info("dropping author conditions with duplicate types", "types", dups)
		failures = append(failures, fmt.Sprintf("duplicate condition type(s) %v dropped", dups))
	}
	if len(failures) > 0 {
		return conditions, true, fmt.Errorf("%w: %s", ErrConditionEvaluationDegraded, strings.Join(failures, "; "))
	}
	return conditions, incomplete, nil
}

// missingDependency reports why a dependency is missing. A type is
// missing if a collision withdrew it or if an entry that could
// declare it produced no output. A built-in has no declaring entry
// and is never withdrawn, so it is never missing.
func missingDependency(
	dependsOn []graph.ConditionDependency,
	noOutputEntries []bool,
	duplicatedTypes map[string]struct{},
) (string, bool) {
	for _, d := range dependsOn {
		if _, bad := duplicatedTypes[d.Type]; bad {
			return fmt.Sprintf("condition type %q was withdrawn", d.Type), true
		}
		for _, e := range d.DeclaredBy {
			if noOutputEntries[e] {
				return fmt.Sprintf("condition type %q comes from entry %d, which did not run", d.Type, e), true
			}
		}
	}
	return "", false
}

// schemaForConditions builds the value bound to the `schema` CEL variable
// when evaluating author conditions: the instance's spec/metadata (status
// stripped, matching every other CEL eval path) plus a synthesized
// status.conditions[] holding the conditions visible so far for
// runtime.condition(schema, _) lookups.
func (n *Node) schemaForConditions(entries []any) any {
	if len(n.observed) == 0 {
		return map[string]any{
			"status": map[string]any{
				"conditions": entries,
			},
		}
	}

	obj := withStatusOmitted(n.observed[0].Object)
	obj["status"] = map[string]any{
		"conditions": entries,
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

// conditionEntry renders a condition in the plain-map shape conditionFromMap
// expects.
func conditionEntry(c library.Condition) map[string]any {
	return map[string]any{
		"type":    c.ConditionType,
		"status":  c.Status,
		"reason":  c.Reason,
		"message": c.Message,
	}
}

// upsertConditionEntry replaces the entry of c's type if present, else appends.
func upsertConditionEntry(entries []any, c library.Condition) []any {
	for i, e := range entries {
		if m, ok := e.(map[string]any); ok && m["type"] == c.ConditionType {
			entries[i] = conditionEntry(c)
			return entries
		}
	}
	return append(entries, conditionEntry(c))
}

// removeConditionEntry drops the given type, so a reader of a duplicated type sees
// the empty condition rather than an ambiguous value.
func removeConditionEntry(entries []any, typ string) []any {
	out := make([]any, 0, len(entries))
	for _, e := range entries {
		if m, ok := e.(map[string]any); ok && m["type"] == typ {
			continue
		}
		out = append(out, e)
	}
	return out
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
