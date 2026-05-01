// deferred.go validates deferred ($${...}) expressions at compile time.
// When a node's template produces a child Graph CR, the compiler extracts
// the child's scope (node IDs + forEach variables) and validates deferred
// expressions against it. Parse errors and undeclared references are caught
// at the parent's compile time instead of deferring to the child controller.
//
// Per 004-compilation.md § Recursive Compilation: "The compiler handles
// arbitrary depth by recursing. $${expr} is depth 1... The mechanism is
// general; the current patterns are not."
package compiler

import (
	"fmt"
	"strings"

	"github.com/google/cel-go/cel"

	"github.com/ellistarn/kro/experimental/controller/graph"
	krocel "github.com/kubernetes-sigs/kro/pkg/cel"
)

// ChildScope holds the identifiers visible in a child Graph's CEL environment.
type ChildScope struct {
	NodeIDs     []string // child node IDs (from spec.nodes[].id)
	ForEachVars []string // child forEach variable names
}

// deferredExpr is a single deferred expression found in a parent node's body.
type deferredExpr struct {
	inner string // expression text after stripping one $ (ready for child compilation)
}

// compileDeferredExpressions validates $${...} expressions at each deferral
// depth by building best-effort child type environments. Called during
// CompileGraphSpec after ${...} expression compilation.
//
// When a node's template produces a child Graph CR with a statically-knowable
// node list, the function also pre-compiles the child spec. Parse errors,
// type errors, and DAG cycle errors in the child are reported as parent
// compilation errors.
func compileDeferredExpressions(spec *graph.GraphSpec) error {
	for _, node := range spec.Nodes {
		// Walk the node's body for $${...} expressions.
		body := node.Body()
		if body == nil && node.TemplateExpr == "" {
			continue
		}

		// Collect all $${...} expressions from the node's body.
		var deferred []deferredExpr
		var allStrings []string
		if body != nil {
			graph.CollectStrings(body, &allStrings)
		}
		if node.TemplateExpr != "" {
			allStrings = append(allStrings, node.TemplateExpr)
		}
		for _, s := range allStrings {
			pos := 0
			for {
				dollars, expr, start, _ := graph.FindExpr(s, pos)
				if start < 0 {
					break
				}
				pos = start + len(dollars) + len(expr) + 2
				if len(dollars) == 2 { // $${...} — depth 1
					deferred = append(deferred, deferredExpr{inner: expr})
				}
			}
		}

		// Determine the child scope. If the template produces a Graph CR,
		// extract node IDs and forEach variables from spec.nodes.
		scope := ExtractChildScopeFromBody(body)

		// Validate deferred expressions against the child scope.
		if len(deferred) > 0 {
			if err := validateDeferredExprs(node.ID, scope, deferred); err != nil {
				return err
			}
		}

		// Pre-compilation: if a forEach node's template produces a Graph CR
		// with a static node list, strip one deferral level and compile the
		// child spec. This catches expression errors, type errors, and
		// DAG cycles in the child at the parent's compile time.
		// Per 004-compilation.md § Recursive Compilation: "When a forEach
		// template produces a child Graph CR..." — only forEach nodes trigger
		// pre-compilation (they stamp N identical children from one template).
		if body != nil && len(scope.NodeIDs) > 0 && node.ForEach != nil {
			if err := precompileChildGraph(node.ID, body); err != nil {
				return err
			}
		}
	}
	return nil
}

// extractChildScopeFromBody extracts the child Graph's scope (node IDs +
// forEach variables) from a template body that produces a Graph CR.
// Returns an empty scope if the body is not a Graph CR or if the child's
// node list can't be determined statically.
func ExtractChildScopeFromBody(body map[string]any) ChildScope {
	if body == nil {
		return ChildScope{}
	}

	// Check if this is a Graph CR template.
	apiVersion, _ := body["apiVersion"].(string)
	kind, _ := body["kind"].(string)
	if !graph.IsGraphCRLiteral(apiVersion, kind) {
		return ChildScope{}
	}

	// Extract spec.nodes as a literal list.
	specMap, ok := body["spec"].(map[string]any)
	if !ok {
		return ChildScope{}
	}
	nodesRaw, ok := specMap["nodes"]
	if !ok {
		return ChildScope{}
	}

	// spec.nodes might be:
	// - []any: literal YAML array → extract node IDs directly
	// - string: expression ($${...} or ${...}) → can't extract statically
	nodesList, ok := nodesRaw.([]any)
	if !ok {
		return ChildScope{} // expression-valued, not a literal list
	}

	var scope ChildScope
	for _, entry := range nodesList {
		nodeMap, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		// Extract node ID.
		if id, ok := nodeMap["id"].(string); ok {
			scope.NodeIDs = append(scope.NodeIDs, id)
		}
		// Extract forEach variable names from child nodes.
		if fe, ok := nodeMap["forEach"]; ok {
			scope.ForEachVars = append(scope.ForEachVars, extractForEachVarNames(fe)...)
		}
	}
	return scope
}

// extractForEachVarNames extracts variable names from a forEach field value.
// Handles both flat map ({k: expr}) and array-of-maps ([{k: expr}]) forms.
func extractForEachVarNames(forEach any) []string {
	var names []string
	switch fe := forEach.(type) {
	case map[string]any:
		for k := range fe {
			names = append(names, k)
		}
	case []any:
		for _, item := range fe {
			if m, ok := item.(map[string]any); ok {
				for k := range m {
					names = append(names, k)
				}
			}
		}
	}
	return names
}



// validateDeferredExprs builds a child CEL environment from the child scope
// and parses + type-checks each deferred expression against it.
func validateDeferredExprs(parentNodeID string, scope ChildScope, exprs []deferredExpr) error {
	// Build a minimal CEL environment with child identifiers as dyn.
	allIDs := append(scope.NodeIDs, scope.ForEachVars...)
	env, err := krocel.DefaultEnvironment(
		krocel.WithResourceIDs(allIDs),
		krocel.WithCustomDeclarations(customCELFunctions()),
		krocel.WithCustomDeclarations([]cel.EnvOption{
			cel.Variable(ReservedNodeReadyVar, cel.MapType(cel.StringType, cel.BoolType)),
			cel.Variable(ReservedDepsMapVar, cel.MapType(cel.StringType, cel.ListType(cel.DynType))),
		}),
	)
	if err != nil {
		return fmt.Errorf("node %q: creating child CEL env for deferred analysis: %w", parentNodeID, err)
	}

	// Parse and type-check each deferred expression.
	seen := make(map[string]bool)
	for _, d := range exprs {
		if seen[d.inner] {
			continue // same expression, already validated
		}
		seen[d.inner] = true

		parsed, issues := env.Parse(d.inner)
		if issues != nil && issues.Err() != nil {
			return fmt.Errorf("node %q: deferred depth 1: parsing expression %q: %w: %w",
				parentNodeID, d.inner, ErrInvalidExpression, issues.Err())
		}
		_, issues = env.Check(parsed)
		if issues != nil && issues.Err() != nil {
			return fmt.Errorf("node %q: deferred depth 1: checking expression %q: %w: %w",
				parentNodeID, d.inner, ErrInvalidExpression, issues.Err())
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Pre-compilation of child Graph specs
// ---------------------------------------------------------------------------

// precompileChildGraph extracts a child GraphSpec from a parent template body
// that produces a Graph CR, strips one deferral level ($${...} → ${...}),
// parses the child's node list, and runs CompileGraphSpec on the child spec.
// Compilation errors in the child are reported as parent compilation errors.
//
// Per 004-compilation.md § Pre-compilation: "The parent extracts the child spec
// (with $${...} → ${...} stripping), runs CompileGraphSpec on it, and caches
// the result."
func precompileChildGraph(parentNodeID string, body map[string]any) error {
	specMap, ok := body["spec"].(map[string]any)
	if !ok {
		return nil
	}
	nodesRaw, ok := specMap["nodes"]
	if !ok {
		return nil
	}
	// Only pre-compile when the node list is a literal array (statically knowable).
	nodesList, ok := nodesRaw.([]any)
	if !ok {
		return nil
	}

	// Strip one deferral level: $${...} → ${...} throughout the node list.
	stripped := StripDeferralLevel(nodesList).([]any)

	// Parse the stripped node list into Node structs.
	childNodes, err := graph.ParseNodeList(stripped)
	if err != nil {
		return fmt.Errorf("node %q: child graph: parsing node list: %w: %w",
			parentNodeID, ErrInvalidExpression, err)
	}

	// Compile the child spec. Type information is nil (all types resolve
	// as dyn) because the child's CRD schemas may not be available during
	// parent compilation. This still catches: parse errors, undeclared
	// references, forEach type errors, DAG cycles.
	childSpec := &graph.GraphSpec{Nodes: childNodes}
	_, err = CompileGraphSpec(childSpec, nil)
	if err != nil {
		return fmt.Errorf("node %q: child graph: %w", parentNodeID, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Dollar-stripping utilities for pre-compilation
// ---------------------------------------------------------------------------

// stripDeferralLevel walks a value tree and strips one $ from every $${...}
// pattern in string values. Used to produce the child Graph's spec from the
// parent's template: $${expr} → ${expr}, $$${expr} → $${expr}, ${expr} → ${expr} (unchanged).
func StripDeferralLevel(v any) any {
	switch val := v.(type) {
	case string:
		return stripOneDollar(val)
	case map[string]any:
		result := make(map[string]any, len(val))
		for k, inner := range val {
			result[k] = StripDeferralLevel(inner)
		}
		return result
	case []any:
		result := make([]any, len(val))
		for i, inner := range val {
			result[i] = StripDeferralLevel(inner)
		}
		return result
	default:
		return v
	}
}

// stripOneDollar strips one $ from each $${...} in a string.
// ${...} (single $) is replaced with a placeholder literal — these are
// parent-scope expressions that will be evaluated at reconcile time before
// the child is stamped. Pre-compilation should not attempt to compile them.
func stripOneDollar(s string) string {
	var result strings.Builder
	pos := 0
	changed := false
	for pos < len(s) {
		dollars, expr, start, end := graph.FindExpr(s, pos)
		if start < 0 {
			result.WriteString(s[pos:])
			break
		}
		result.WriteString(s[pos:start])
		if len(dollars) > 1 {
			// $${...} → ${...}, $$${...} → $${...}
			result.WriteString(dollars[1:] + "{" + expr + "}")
			changed = true
		} else {
			// ${...} is a parent expression — replace with placeholder.
			// At reconcile time this will be a concrete value.
			result.WriteString("__kro_parent_expr__")
			changed = true
		}
		pos = end
	}
	if !changed {
		return s // avoid allocation when nothing changed
	}
	return result.String()
}
