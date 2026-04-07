// eval.go is the template evaluation engine. It walks value trees, evaluates
// ${...} CEL expressions, strips $${...} deferred expressions, checks readyWhen
// and includeWhen conditions, and extracts dependency references for DAG building.
//
// All CEL evaluation goes through the evaluator struct, which holds a reference
// to the pre-compiled graphCache. No CEL compilation happens in this file —
// programs are looked up from the cache and evaluated against the current scope.
package graphcontroller

import (
	"fmt"
	"strings"
)

// evaluator holds the pre-compiled expression cache and the current scope
// for a single reconcile cycle. Methods on evaluator replace the previous
// free functions (evaluateTemplate, evaluateString, etc.).
type evaluator struct {
	cache *graphCache
	scope map[string]any
}

// newEvaluator creates an evaluator for a reconcile cycle.
func newEvaluator(cache *graphCache) *evaluator {
	return &evaluator{
		cache: cache,
		scope: map[string]any{},
	}
}

// withScope returns a new evaluator that shares the cache but has its own scope.
// Used for forEach inner scopes.
func (e *evaluator) withScope(scope map[string]any) *evaluator {
	return &evaluator{cache: e.cache, scope: scope}
}

// statusTemplate evaluates user-defined status fields against the scope.
// Uses soft resolution: fields whose expressions fail (data-pending) are omitted
// rather than causing an error. This matches the existing RGD behavior where
// status is best-effort.
func (e *evaluator) statusTemplate(template map[string]any) map[string]any {
	if len(template) == 0 {
		return nil
	}

	result := make(map[string]any, len(template))
	for key, val := range template {
		evaluated, err := e.template(val)
		if err != nil {
			// Soft resolve: skip fields that can't be evaluated yet
			continue
		}
		result[key] = evaluated
	}

	if len(result) == 0 {
		return nil
	}
	return result
}

// checkReadiness evaluates readyWhen conditions against an observed resource.
// Returns nil if all conditions pass, ErrWaitingForReadiness if any are false
// or data-pending.
func (e *evaluator) checkReadiness(conditions []string, observed any, resourceID string) error {
	if len(conditions) == 0 {
		return nil
	}

	// Build a scoped evaluator with just this resource
	readyEval := e.withScope(map[string]any{resourceID: observed})

	for _, cond := range conditions {
		val, err := readyEval.evalString(cond)
		if err != nil {
			if isDataPending(err) {
				return fmt.Errorf("resource %q: readyWhen %q: data not yet available: %w", resourceID, cond, ErrWaitingForReadiness)
			}
			return fmt.Errorf("resource %q: readyWhen %q: %w", resourceID, cond, err)
		}
		switch v := val.(type) {
		case bool:
			if !v {
				return fmt.Errorf("resource %q: readyWhen %q evaluated to false: %w", resourceID, cond, ErrWaitingForReadiness)
			}
		case string:
			if v != "true" {
				return fmt.Errorf("resource %q: readyWhen %q evaluated to %q: %w", resourceID, cond, v, ErrWaitingForReadiness)
			}
		default:
			return fmt.Errorf("resource %q: readyWhen %q evaluated to %T, want bool", resourceID, cond, val)
		}
	}
	return nil
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
		result, err := e.cache.eval(expr, e.scope)
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
			val, err := e.cache.eval(expr, e.scope)
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

// includeWhen evaluates all includeWhen conditions for a resource.
func (e *evaluator) includeWhen(conditions []string) (bool, error) {
	for _, cond := range conditions {
		val, err := e.evalString(cond)
		if err != nil {
			if isDataPending(err) {
				return false, fmt.Errorf("includeWhen data pending: %w", ErrDataPending)
			}
			return false, err
		}
		switch v := val.(type) {
		case bool:
			if !v {
				return false, nil
			}
		case string:
			if v != "true" {
				return false, nil
			}
		default:
			return false, fmt.Errorf("includeWhen condition %q evaluated to %T, want bool", cond, val)
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
	case float32:
		return float64(val)
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

// extractReferencedIDs scans a Resource for all ${...} expressions (at the
// current evaluation level — not $${...}) and returns the set of top-level
// scope variable names referenced. Self-references (the resource's own ID)
// are excluded since readyWhen expressions reference the resource itself
// after it's applied, not as a dependency.
func extractReferencedIDs(res Resource) map[string]bool {
	refs := map[string]bool{}

	// Collect all string values that might contain expressions
	var strs []string
	collectStrings(res.Template, &strs)
	collectStrings(res.ExternalRef, &strs)
	for _, s := range res.IncludeWhen {
		strs = append(strs, s)
	}
	for _, s := range res.ReadyWhen {
		strs = append(strs, s)
	}
	if res.ForEach != nil {
		for _, v := range res.ForEach {
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
			if id != "" && id != res.ID { // exclude self-references
				refs[id] = true
			}
		}
	}

	return refs
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
