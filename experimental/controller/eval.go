// eval.go is the template evaluation engine. It walks value trees, evaluates
// ${...} CEL expressions, strips $${...} deferred expressions, checks readyWhen
// and includeWhen conditions, and extracts dependency references for DAG building.
//
// All CEL evaluation goes through the evaluator struct, which holds a reference
// to the pre-compiled compiledGraph. No CEL compilation happens in this file —
// programs are looked up from the compiled graph and evaluated against the current scope.
package graphcontroller

import (
	"errors"
	"fmt"
	"strings"

	"github.com/ellistarn/kro/experimental/controller/compiler"
	"github.com/ellistarn/kro/experimental/controller/graph"
)

// gateResult represents the outcome of a propagateWhen evaluation.
type gateResult int

const (
	gatePass  gateResult = iota // all conditions true — node proceeds
	gateBlock                   // at least one condition false — node waits
	gateError                   // at least one condition errored — gate cannot decide
)

// evaluator holds the pre-compiled expression cache and the current scope
// for a single reconcile cycle. The scope is mutable and shared — the
// sequential walk updates it as nodes are evaluated.
type evaluator struct {
	compiled *compiler.CompiledGraph
	scope    map[string]any

	// effectiveGeneration is the generation string to stamp on identity
	// labels during apply. Normally matches graph.GetGeneration(). On a
	// compilation-failure fallback, the active revision's generation is
	// stamped instead — the labels reflect what materialized, not the
	// generation whose spec didn't compile. Plumbed here rather than read
	// from graph.GetGeneration() at stamp time so the choice is explicit
	// and the graph object isn't mutated as a side channel.
	effectiveGeneration int64

	// nodeReady carries per-Watch readyWhen verdicts. AST-rewritten
	// `<wk_id>.ready()` expressions look up this map via the reserved
	// scope key (see readyrewrite.go). Scalar/forEach readiness continues
	// to flow through the per-item __ready stamping — those are unaffected
	// by this sidecar. Per 001-graph.md § readyWhen.
	nodeReady map[string]bool
}

// newEvaluator creates an evaluator for a reconcile cycle.
func newEvaluator(state *instanceState) *evaluator {
	// nodeReady carries per-Watch readyWhen verdicts. Created fresh per
	// evaluator — the simplified walk evaluates all nodes every cycle so
	// verdicts don't need to persist across reconciles.
	nodeReady := map[string]bool{}
	scope := map[string]any{
		compiler.ReservedNodeReadyVar: nodeReady,
	}
	return &evaluator{
		compiled:  state.compilation.compiled,
		scope:     scope,
		nodeReady: nodeReady,
	}
}

// withScope returns a new evaluator that shares the compiled graph but has its own scope.
// Used for forEach inner scopes where the iterator variable is bound per-item.
func (e *evaluator) withScope(scope map[string]any) *evaluator {
	// Preserve the node-readiness sidecar across scope substitutions so
	// inner forEach evaluations see the same verdicts as the outer walk.
	if e.nodeReady != nil {
		if _, present := scope[compiler.ReservedNodeReadyVar]; !present {
			scope[compiler.ReservedNodeReadyVar] = e.nodeReady
		}
	}
	return &evaluator{compiled: e.compiled, scope: scope, nodeReady: e.nodeReady, effectiveGeneration: e.effectiveGeneration}
}

// evalBoolCondition evaluates a CEL expression and coerces the result to bool.
// Returns (true, nil) if the value is boolean true or string "true".
// Returns (false, nil) if the value is boolean false or string != "true".
// Returns (false, error) for non-boolean/string types or evaluation errors.
func (e *evaluator) evalBoolCondition(expr string) (bool, error) {
	val, err := e.evalString(expr)
	if err != nil {
		return false, err
	}
	switch v := val.(type) {
	case bool:
		return v, nil
	case string:
		return v == "true", nil
	default:
		return false, fmt.Errorf("expression %q evaluated to %T, want bool", expr, val)
	}
}

// markReady injects the __ready flag into a node's scope data. This is
// read by the .ready() CEL member function to expose the graph controller's
// readiness assessment to CEL expressions like propagateWhen and readyWhen.
func (e *evaluator) markReady(nodeID string, ready bool) {
	if m, ok := e.scope[nodeID].(map[string]any); ok {
		m["__ready"] = ready
	}
}

// markUpdated injects the __updated flag into a node's scope data. This is
// read by the .updated() CEL member function. Per 001-graph.md § CEL Functions:
// ".updated() — true when the node is on the latest graph generation."
func (e *evaluator) markUpdated(nodeID string, updated bool) {
	if m, ok := e.scope[nodeID].(map[string]any); ok {
		m["__updated"] = updated
	}
}

// isForEachItemUpdated checks whether a forEach child resource was applied
// with the current graph generation by reading its generation label.
// Constructs the exact forEach child generation label key from the resource's
// identity and performs a direct lookup — no scanning.
//
// Returns false when the label is absent — missing labels mean unknown
// provenance, and the safe direction is "needs update" (re-apply is
// idempotent and self-healing). Per 005-reconciliation.md § Propagation
// Control: returning true on unknown provenance would inflate the "current"
// count in propagateWhen filters, permitting faster rollout than intended.
//
// This function is forEach-specific: it constructs a forEachChild label key.
// Scalar nodes determine updated state from control flow (just-applied = true)
// and never call this function.
func isForEachItemUpdated(item map[string]any, parentID, graphName, graphNS string, effectiveGeneration int64) bool {
	md, _ := item["metadata"].(map[string]any)
	if md == nil {
		return false
	}
	lbls, _ := md["labels"].(map[string]any)
	if lbls == nil {
		return false
	}

	name, _ := md["name"].(string)
	namespace, _ := md["namespace"].(string)
	gvk := graph.GVKFromMap(item)
	kind := gvk.Kind
	group := gvk.Group

	labelKey := graph.ForEachChildGenerationLabelKey(parentID, name, namespace, kind, group, graphName, graphNS)
	genVal, _ := lbls[labelKey].(string)
	if genVal == "" {
		return false
	}
	return genVal == fmt.Sprintf("%d", effectiveGeneration)
}

// evalReadiness evaluates readyWhen conditions and stamps __ready in scope.
// Returns compiler.ErrWaitingForReadiness if any condition is false or data-pending.
// Returns compiler.ErrReadyWhenFailed wrapping the underlying error if the expression
// itself is broken (wrong return type, CEL error). Per 001-graph.md:
// "readyWhen is a health signal — it does not gate downstream execution."
// The compiler.ErrReadyWhenFailed sentinel lets the coordinator classify this as
// NodeNotReady (not NodeError), preserving the design invariant.
func (e *evaluator) evalReadiness(nodeID string, readyWhen []string) error {
	if len(readyWhen) > 0 {
		if err := e.evalReadinessConditions(readyWhen, nodeID); err != nil {
			e.markReady(nodeID, false)
			// compiler.ErrWaitingForReadiness and compiler.ErrPending are transient — pass through.
			// All other errors are permanent expression failures that must not
			// produce NodeError (which would gate dependents). Wrap with
			// compiler.ErrReadyWhenFailed so the coordinator classifies as NodeNotReady.
			if errors.Is(err, compiler.ErrWaitingForReadiness) || errors.Is(err, compiler.ErrPending) {
				return err
			}
			return fmt.Errorf("%w: %w", compiler.ErrReadyWhenFailed, err)
		}
	}
	e.markReady(nodeID, true)
	return nil
}

// evalConditions evaluates a list of boolean CEL conditions.
// Returns gatePass if all pass, gateBlock if any returns false, gateError on eval error.
// When wrapPending is true, pending errors are wrapped as ErrPending (for includeWhen).
// When wrapPending is false, pending errors are mapped to ErrWaitingForReadiness (for readyWhen).
func (e *evaluator) evalConditions(conditions []string, nodeID string, wrapPending bool) (gateResult, error) {
	for _, cond := range conditions {
		ok, err := e.evalBoolCondition(cond)
		if err != nil {
			if wrapPending && compiler.IsPending(err) {
				return gateError, fmt.Errorf("data pending: %w", compiler.ErrPending)
			}
			if !wrapPending && compiler.IsPending(err) {
				return gateError, fmt.Errorf("node %q: readyWhen %q: data not yet available: %w", nodeID, cond, compiler.ErrWaitingForReadiness)
			}
			return gateError, fmt.Errorf("node %q: condition %q: %w", nodeID, cond, err)
		}
		if !ok {
			return gateBlock, nil
		}
	}
	return gatePass, nil
}

// evalReadinessConditions evaluates readyWhen boolean conditions against the
// full scope. Returns nil if all conditions pass, compiler.ErrWaitingForReadiness if
// any are false or data-pending.
//
// This is the evaluation primitive — it does NOT stamp __ready. Callers that
// need readiness recorded in scope should use evalReadiness instead. Direct
// callers of this function (Watch, forEach) have custom per-item stamping
// logic that differs from the node-level __ready flag.
func (e *evaluator) evalReadinessConditions(conditions []string, nodeID string) error {
	if len(conditions) == 0 {
		return nil
	}
	gate, err := e.evalConditions(conditions, nodeID, false)
	if err != nil {
		return err
	}
	if gate == gateBlock {
		return fmt.Errorf("node %q: readyWhen evaluated to false: %w", nodeID, compiler.ErrWaitingForReadiness)
	}
	return nil
}

// checkPropagateWhen evaluates propagateWhen conditions against the full scope.
// Returns true if all conditions pass (input gate is open, node should evaluate).
// Returns false if any condition is false or errors.
//
// propagateWhen is an input gate — expressions reference upstream nodes,
// not the node itself. Evaluating against the full scope (not a restricted
// single-node scope) is required for cross-node references to resolve.
// The DAG walk's topological order guarantees that all dependency nodes
// are already in e.scope when this is called.
//
// Returns three possible outcomes:
//   - gatePass: all conditions evaluate to true — node proceeds
//   - gateBlock: at least one condition evaluates to false (no error) — node waits
//   - gateError: at least one condition errors during evaluation — gate cannot decide
//
// The distinction between gateBlock and gateError matters for contagious
// exclusion: when a dependency is Excluded, gateError means the gate
// can't evaluate because its inputs are missing, which should allow
// contagious exclusion to proceed. gateBlock means the gate actively
// decided "no" with valid inputs.
func (e *evaluator) checkPropagateWhen(conditions []string, nodeID string) gateResult {
	if len(conditions) == 0 {
		return gatePass
	}
	gate, _ := e.evalConditions(conditions, nodeID, false)
	return gate
}

// firstUnsatisfiedCondition returns the first condition expression that
// evaluates to false or errors. Used for diagnostic logging when
// propagateWhen blocks a node.
func (e *evaluator) firstUnsatisfiedCondition(conditions []string) string {
	for _, cond := range conditions {
		ok, err := e.evalBoolCondition(cond)
		if err != nil || !ok {
			return cond
		}
	}
	return ""
}

// toMap evaluates a template and asserts the result is a map.
// Normalizes data-pending errors to compiler.ErrPending. Non-pending evaluation
// failures are wrapped with compiler.ErrEvaluation so classifyAPIError can
// distinguish them from network errors with similar message text.
func (e *evaluator) toMap(tmpl map[string]any) (map[string]any, error) {
	evaluated, err := e.evaluateTree(tmpl)
	if err != nil {
		if compiler.IsPending(err) {
			return nil, fmt.Errorf("%w: %v", compiler.ErrPending, err)
		}
		return nil, fmt.Errorf("%w: %w", compiler.ErrEvaluation, err)
	}
	result, ok := evaluated.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w: evaluated to %T, want map", compiler.ErrEvaluation, evaluated)
	}
	return result, nil
}

// toMapNode evaluates a node's body — either a static map (Template/Patch/
// Ref/Watch/Def) or a CEL expression string under template/patch/def
// that yields the body map at runtime. For Ref/Watch (identity-only)
// the identity map is evaluated — its fields may still contain CEL
// expressions.
func (e *evaluator) toMapNode(node graph.Node) (map[string]any, error) {
	if node.TemplateExpr != "" {
		// Body came in as a CEL expression under the classification
		// keyword (template/patch/def as a string). Use evalString
		// which handles ${...} extraction and type coercion.
		raw, err := e.evalString(node.TemplateExpr)
		if err != nil {
			if compiler.IsPending(err) {
				return nil, compiler.ErrPending
			}
			return nil, fmt.Errorf("%w: %s expression %q: %w", compiler.ErrEvaluation, node.ExprKeyword, node.TemplateExpr, err)
		}
		result, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%w: %s expression evaluated to %T, want map", compiler.ErrEvaluation, node.ExprKeyword, raw)
		}
		return result, nil
	}
	// Payload() returns the body map for Template/Patch/Def. For
	// Ref/Watch (identity-only) Payload returns nil; identity
	// fields still need to be evaluated (name/namespace may be CEL).
	body := node.Payload()
	if body == nil {
		body = node.Identity()
	}
	return e.toMap(body)
}

// evaluateTree walks a value tree and evaluates/strips ${...} expressions.
func (e *evaluator) evaluateTree(value any) (any, error) {
	switch v := value.(type) {
	case string:
		return e.evalString(v)
	case map[string]any:
		result := make(map[string]any, len(v))
		for k, val := range v {
			evaluated, err := e.evaluateTree(val)
			if err != nil {
				return nil, fmt.Errorf("field %s: %w", k, err)
			}
			result[k] = evaluated
		}
		return result, nil
	case []any:
		result := make([]any, len(v))
		for i, val := range v {
			evaluated, err := e.evaluateTree(val)
			if err != nil {
				return nil, fmt.Errorf("index %d: %w", i, err)
			}
			result[i] = evaluated
		}
		return result, nil
	default:
		return value, nil
	}
}

// evalString processes a string value, handling ${...} and $${...} expressions.
func (e *evaluator) evalString(s string) (any, error) {
	// Check if the entire string is a single expression (standalone)
	dollars, expr, start, end := graph.FindExpr(s, 0)
	if start == 0 && end == len(s) && len(dollars) == 1 {
		result, err := e.compiled.Eval(expr, e.scope)
		if err != nil {
			return nil, fmt.Errorf("evaluating %q: %w", expr, err)
		}
		return result, nil
	}

	// Multi-expression or embedded — string interpolation
	var result strings.Builder
	pos := 0
	for {
		dollars, expr, start, end = graph.FindExpr(s, pos)
		if start < 0 {
			result.WriteString(s[pos:])
			break
		}
		result.WriteString(s[pos:start])

		if len(dollars) == 1 {
			val, err := e.compiled.Eval(expr, e.scope)
			if err != nil {
				return nil, fmt.Errorf("evaluating %q: %w", expr, err)
			}
			result.WriteString(fmt.Sprintf("%v", val))
		} else {
			result.WriteString(dollars[1:] + "{" + expr + "}")
		}
		pos = end
	}
	return result.String(), nil
}

// includeWhen evaluates all includeWhen conditions for a node.
func (e *evaluator) includeWhen(conditions []string) (bool, error) {
	gate, err := e.evalConditions(conditions, "", true)
	if gate == gatePass {
		return true, nil
	}
	// gateBlock means a condition returned false — not an error.
	if gate == gateBlock {
		return false, nil
	}
	// gateError — propagate the wrapped error.
	return false, err
}
