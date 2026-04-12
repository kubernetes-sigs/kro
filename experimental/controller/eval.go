// eval.go is the template evaluation engine. It walks value trees, evaluates
// ${...} CEL expressions, strips $${...} deferred expressions, checks readyWhen
// and includeWhen conditions, and extracts dependency references for DAG building.
//
// All CEL evaluation goes through the evaluator struct, which holds a reference
// to the pre-compiled compiledGraph. No CEL compilation happens in this file —
// programs are looked up from the compiled graph and evaluated against the current scope.
package graphcontroller

import (
	"fmt"
	"strings"
)

// evaluator holds the pre-compiled expression cache and the current scope
// for a single reconcile cycle. The scope is owned by the coordinator —
// workers receive read-only snapshots and return results for the coordinator
// to merge. No locking needed.
type evaluator struct {
	compiled *compiledGraph
	scope    map[string]any

	// forEach state — populated by the coordinator before dispatching a
	// forEach worker, and read by reconcileForEach. Workers write to these
	// maps (they're private to the worker's evaluator copy), and the results
	// are returned to the coordinator for merging into the shared cache.
	forEachPrevItems map[string][]any               // cache key → previous collection items
	forEachPrevScope map[string]map[string]any      // nodeID → itemID → previous scope data
	forEachPrevKeys  map[string]map[string][]string // nodeID → itemID → previous applied keys
	forEachNewItems  map[string][]any               // cache key → updated collection items (output)
	forEachNewScope  map[string]map[string]any      // nodeID → itemID → updated scope data (output)
	forEachNewKeys   map[string]map[string][]string // nodeID → itemID → updated keys (output)
}

// newEvaluator creates an evaluator for a reconcile cycle.
func newEvaluator(state *instanceState) *evaluator {
	return &evaluator{
		compiled: state.compiled,
		scope:    map[string]any{},
	}
}

// withScope returns a new evaluator that shares the compiled graph but has its own scope.
// Used for forEach inner scopes and worker snapshots.
func (e *evaluator) withScope(scope map[string]any) *evaluator {
	return &evaluator{compiled: e.compiled, scope: scope}
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

// checkReadiness evaluates readyWhen conditions against the full scope.
// Returns nil if all conditions pass, ErrWaitingForReadiness if any are false
// or data-pending. Evaluates against the full scope so readyWhen can reference
// other nodes (e.g., readyWhen: ["${workers.ready()}"]).
func (e *evaluator) checkReadiness(conditions []string, observed any, nodeID string) error {
	if len(conditions) == 0 {
		return nil
	}

	for _, cond := range conditions {
		ok, err := e.evalBoolCondition(cond)
		if err != nil {
			if isDataPending(err) {
				return fmt.Errorf("node %q: readyWhen %q: data not yet available: %w", nodeID, cond, ErrWaitingForReadiness)
			}
			return fmt.Errorf("node %q: readyWhen %q: %w", nodeID, cond, err)
		}
		if !ok {
			return fmt.Errorf("node %q: readyWhen %q evaluated to false: %w", nodeID, cond, ErrWaitingForReadiness)
		}
	}
	return nil
}

// checkPropagateWhen evaluates propagateWhen conditions against the full scope.
// Returns true if all conditions pass (data should flow to dependents).
// Returns false if any condition is false or errors.
//
// propagateWhen expressions can reference other nodes — including cross-node
// readiness checks like ${deployment.ready()} on a consumer node. Evaluating
// against the full scope (not a restricted single-node scope) is required for
// these references to resolve. The DAG walk's topological order guarantees
// that all dependency nodes are already in e.scope when this is called.
func (e *evaluator) checkPropagateWhen(conditions []string, observed any, nodeID string) bool {
	if len(conditions) == 0 {
		return true
	}

	for _, cond := range conditions {
		ok, err := e.evalBoolCondition(cond)
		if err != nil || !ok {
			return false
		}
	}
	return true
}

// toMap evaluates a template and asserts the result is a map.
// Normalizes data-pending errors to ErrDataPending.
func (e *evaluator) toMap(tmpl map[string]any) (map[string]any, error) {
	evaluated, err := e.template(tmpl)
	if err != nil {
		if isDataPending(err) {
			return nil, ErrDataPending
		}
		return nil, fmt.Errorf("evaluating: %w", err)
	}
	result, ok := evaluated.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("evaluated to %T, want map", evaluated)
	}
	return result, nil
}

// template walks a value tree and evaluates/strips ${...} expressions.
func (e *evaluator) template(value any) (any, error) {
	switch v := value.(type) {
	case string:
		return e.evalString(v)
	case map[string]any:
		result := make(map[string]any, len(v))
		for k, val := range v {
			evaluated, err := e.template(val)
			if err != nil {
				return nil, fmt.Errorf("field %s: %w", k, err)
			}
			result[k] = evaluated
		}
		return result, nil
	case []any:
		result := make([]any, len(v))
		for i, val := range v {
			evaluated, err := e.template(val)
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
	dollars, expr, start, end := findExpr(s, 0)
	if start == 0 && end == len(s) && len(dollars) == 1 {
		result, err := e.compiled.eval(expr, e.scope)
		if err != nil {
			return nil, fmt.Errorf("evaluating %q: %w", expr, err)
		}
		return result, nil
	}

	// Multi-expression or embedded — string interpolation
	var result strings.Builder
	pos := 0
	for {
		dollars, expr, start, end = findExpr(s, pos)
		if start < 0 {
			result.WriteString(s[pos:])
			break
		}
		result.WriteString(s[pos:start])

		if len(dollars) == 1 {
			val, err := e.compiled.eval(expr, e.scope)
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
	for _, cond := range conditions {
		ok, err := e.evalBoolCondition(cond)
		if err != nil {
			if isDataPending(err) {
				return false, fmt.Errorf("includeWhen data pending: %w", ErrDataPending)
			}
			return false, err
		}
		if !ok {
			return false, nil
		}
	}
	return true, nil
}

// ---------------------------------------------------------------------------
// Utilities (unchanged — not methods because they don't need the cache)
// ---------------------------------------------------------------------------

// findExpr finds the next $+{...} expression in input starting at pos,
// handling balanced braces for nested CEL map/object literals.
func findExpr(input string, pos int) (string, string, int, int) {
	for i := pos; i < len(input); i++ {
		if input[i] != '$' {
			continue
		}
		start := i
		for i < len(input) && input[i] == '$' {
			i++
		}
		dollars := input[start:i]
		if i >= len(input) || input[i] != '{' {
			continue
		}
		depth := 0
		inString := false
		var stringChar byte
		escapeNext := false
		exprStart := i + 1
		for j := i; j < len(input); j++ {
			c := input[j]
			if escapeNext {
				escapeNext = false
				continue
			}
			if inString && c == '\\' {
				escapeNext = true
				continue
			}
			if inString {
				if c == stringChar {
					inString = false
				}
				continue
			}
			if c == '\'' || c == '"' {
				inString = true
				stringChar = c
				continue
			}
			switch c {
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					return dollars, input[exprStart:j], start, j + 1
				}
			}
		}
	}
	return "", "", -1, -1
}

// normalizeTypes converts JSON-style float64 numbers to int64 for CEL compatibility.
// The Kubernetes API server returns numbers as float64 in unstructured objects.
//
// The full recursive copy is intentional. A check-first pass to avoid copying
// objects without float64 integers would need to traverse the full tree anyway,
// and CRD objects have arbitrary shapes — branching on object content adds
// complexity without measurable benefit. This call is already gated behind the
// skip-check: triggered nodes always need a fresh normalized copy because their
// template has changed; skipped nodes never call this function.
func normalizeTypes(v any) any {
	switch val := v.(type) {
	case map[string]any:
		result := make(map[string]any, len(val))
		for k, v := range val {
			result[k] = normalizeTypes(v)
		}
		return result
	case []any:
		result := make([]any, len(val))
		for i, v := range val {
			result[i] = normalizeTypes(v)
		}
		return result
	case float64:
		if val == float64(int64(val)) {
			return int64(val)
		}
		return val
	case int:
		return int64(val)
	default:
		return v
	}
}

// copyScope creates a shallow copy of a scope map.
func copyScope(scope map[string]any) map[string]any {
	result := make(map[string]any, len(scope))
	for k, v := range scope {
		result[k] = v
	}
	return result
}

// extractReferencedIDs scans a Node for all ${...} expressions (at the
// current evaluation level — not $${...}) and returns the set of top-level
// scope variable names referenced. Self-references (the node's own ID)
// are excluded since readyWhen expressions reference the node itself
// after it's applied, not as a dependency.
func extractReferencedIDs(node Node) map[string]bool {
	refs := map[string]bool{}

	// Collect all string values that might contain expressions
	var strs []string
	collectStrings(node.Template, &strs)
	for _, s := range node.IncludeWhen {
		strs = append(strs, s)
	}
	for _, s := range node.ReadyWhen {
		strs = append(strs, s)
	}
	for _, s := range node.PropagateWhen {
		strs = append(strs, s)
	}
	if node.ForEach != nil {
		for _, v := range node.ForEach {
			strs = append(strs, v)
		}
	}

	// Extract variable names from ${...} expressions
	for _, s := range strs {
		pos := 0
		for {
			dollars, expr, start, _ := findExpr(s, pos)
			if start < 0 {
				break
			}
			pos = start + len(dollars) + len(expr) + 2 // skip past this expr
			if len(dollars) != 1 {
				continue // $${...} is deferred, not a reference at this level
			}
			id := extractFirstIdentifier(expr)
			if id != "" && id != node.ID { // exclude self-references
				refs[id] = true
			}
		}
	}

	return refs
}

// extractReferencedSections scans a Node for all ${...} expressions and returns
// three maps:
//   - depSections: for each dependency, which top-level sections are referenced
//     (e.g., "deploy" → {"spec": true, "metadata": true})
//   - selfSections: which top-level sections of the node's own observed resource
//     are referenced by readyWhen and propagateWhen
//   - readinessDeps: upstream node IDs referenced via .ready() in gate
//     expressions. These reference runtime readiness state not captured
//     by section-scoped hashing.
//
// Section scoping is the correct mechanism for input hashing — metadata.resourceVersion
// changes on every update, so full-object hashing would always differ. By hashing only
// the sections a node's expressions actually reference, the input hash is stable when
// unrelated fields change.
func extractReferencedSections(node Node) (depSections map[string]map[string]bool, selfSections map[string]bool, readinessDeps map[string]bool) {
	depSections = map[string]map[string]bool{}
	selfSections = map[string]bool{}
	readinessDeps = map[string]bool{}

	// Helper: extract (identifier, section) from a CEL expression.
	// For "deploy.spec.replicas" → ("deploy", "spec")
	// For "deploy.metadata.name" → ("deploy", "metadata")
	// For "deploy.ready()" → ("deploy", "") — function call, not a section
	// For "size(items)" → ("", "") — builtins filtered out
	extractIDAndSection := func(expr string) (string, string) {
		id := extractFirstIdentifier(expr)
		if id == "" {
			return "", ""
		}
		// Find the section: skip past the ID and the dot
		rest := strings.TrimSpace(expr)[len(id):]
		if len(rest) == 0 || rest[0] != '.' {
			return id, "" // no section (e.g., bare reference or function call)
		}
		rest = rest[1:] // skip the dot
		end := 0
		for end < len(rest) && isIdentContinue(rest[end]) {
			end++
		}
		if end == 0 {
			return id, ""
		}
		// Check if this is a function call (followed by '(').
		// dep.ready() is a CEL function, not a section access.
		if end < len(rest) && rest[end] == '(' {
			return id, "" // function call, not a section
		}
		return id, rest[:end]
	}

	// Process template + includeWhen + forEach expressions → depSections
	var templateStrs []string
	collectStrings(node.Template, &templateStrs)
	for _, s := range node.IncludeWhen {
		templateStrs = append(templateStrs, s)
	}
	if node.ForEach != nil {
		for _, v := range node.ForEach {
			templateStrs = append(templateStrs, v)
		}
	}

	for _, s := range templateStrs {
		pos := 0
		for {
			dollars, expr, start, _ := findExpr(s, pos)
			if start < 0 {
				break
			}
			pos = start + len(dollars) + len(expr) + 2
			if len(dollars) != 1 {
				continue
			}
			id, section := extractIDAndSection(expr)
			if id == "" || id == node.ID {
				continue // skip self-references and builtins
			}
			if section == "" {
				continue // bare reference without section
			}
			if depSections[id] == nil {
				depSections[id] = map[string]bool{}
			}
			depSections[id][section] = true
		}
	}

	// Process readyWhen + propagateWhen → both depSections (for upstream refs)
	// and selfSections (for self-refs)
	var gateStrs []string
	for _, s := range node.ReadyWhen {
		gateStrs = append(gateStrs, s)
	}
	for _, s := range node.PropagateWhen {
		gateStrs = append(gateStrs, s)
	}

	for _, s := range gateStrs {
		pos := 0
		for {
			dollars, expr, start, _ := findExpr(s, pos)
			if start < 0 {
				break
			}
			pos = start + len(dollars) + len(expr) + 2
			if len(dollars) != 1 {
				continue
			}
			id, section := extractIDAndSection(expr)
			if id == "" {
				continue
			}
			if section == "" {
				// Function call on upstream node (e.g., dep.ready()).
				// This references runtime state that isn't captured by
				// section-scoped hashing — track it separately.
				if id != node.ID {
					readinessDeps[id] = true
				}
				continue
			}
			if id == node.ID {
				selfSections[section] = true
			} else {
				if depSections[id] == nil {
					depSections[id] = map[string]bool{}
				}
				depSections[id][section] = true
			}
		}
	}

	return depSections, selfSections, readinessDeps
}

// collectStrings recursively collects all string values from a value tree.
func collectStrings(v any, out *[]string) {
	switch val := v.(type) {
	case string:
		*out = append(*out, val)
	case map[string]any:
		for _, child := range val {
			collectStrings(child, out)
		}
	case []any:
		for _, child := range val {
			collectStrings(child, out)
		}
	}
}

// extractFirstIdentifier extracts the first CEL identifier from an expression.
// For "deployment.metadata.name" returns "deployment".
// For "size(items)" returns "size" (a function, not a scope var — but harmless).
// For literals like "'hello'" returns "".
func extractFirstIdentifier(expr string) string {
	expr = strings.TrimSpace(expr)
	if len(expr) == 0 {
		return ""
	}
	// Skip if starts with non-identifier char (quote, digit, operator, etc.)
	if !isIdentStart(expr[0]) {
		return ""
	}
	end := 0
	for end < len(expr) && isIdentContinue(expr[end]) {
		end++
	}
	id := expr[:end]
	// Filter out CEL keywords/builtins that aren't scope variables
	switch id {
	case "true", "false", "null", "size", "has", "exists", "all",
		"filter", "map", "int", "uint", "double", "string", "bool",
		"bytes", "list", "type", "duration", "timestamp":
		return ""
	}
	return id
}

func isIdentStart(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_'
}

func isIdentContinue(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9')
}
