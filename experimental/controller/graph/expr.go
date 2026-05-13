package graph

import (
	"fmt"
	"strings"
)

// FindExpr finds the next $+{...} expression in input starting at pos,
// handling balanced braces for nested CEL map/object literals.
func FindExpr(input string, pos int) (string, string, int, int) {
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

// NormalizeTypes converts JSON-style float64 numbers to int64 for CEL compatibility.
// The Kubernetes API server returns numbers as float64 in unstructured objects.
//
// The full recursive copy is intentional. A check-first pass to avoid copying
// objects without float64 integers would need to traverse the full tree anyway,
// and CRD objects have arbitrary shapes — branching on object content adds
// complexity without measurable benefit. This call is already gated behind the
// skip-check: triggered nodes always need a fresh normalized copy because their
// template has changed; skipped nodes never call this function.
func NormalizeTypes(v any) any {
	switch val := v.(type) {
	case map[string]any:
		result := make(map[string]any, len(val))
		for k, v := range val {
			result[k] = NormalizeTypes(v)
		}
		return result
	case []any:
		result := make([]any, len(val))
		for i, v := range val {
			result[i] = NormalizeTypes(v)
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

// CopyScope creates a shallow copy of a scope map.
func CopyScope(scope map[string]any) map[string]any {
	result := make(map[string]any, len(scope))
	for k, v := range scope {
		result[k] = v
	}
	return result
}

// ExtractReferencedPathsFromNode scans a Node's template and gate expressions
// for ${...} blocks, looks up pre-extracted field paths from exprPaths, and
// returns per-node dependency classifications.
//
// When exprPaths is nil (e.g., BuildDAG called for deletion ordering without
// going through compileGraphSpec), falls back to string-based dependency
// detection, but dependency detection for topological sort still works.
//
// exprAccessModes carries per-expression, per-scope-variable optional-access
// classification computed from pre-rewrite CEL ASTs. When non-nil, it drives
// DepKind classification: a dependency accessed only through optional patterns
// (?.field, .ready(), .updated()) in ALL expressions is DepSoft. When nil,
// all dependencies default to DepHard.
//
// The exprPaths map is computed from CEL ASTs during compilation in
// compileGraphSpec.
func ExtractReferencedPathsFromNode(
	node Node,
	exprPaths map[string]map[string][]FieldPath,
	exprAccessModes map[string]map[string]bool,
) (
	dependencies map[string]DepKind,
	depPaths map[string][]FieldPath,
	selfPaths []FieldPath,
	err error,
) {
	dependencies = map[string]DepKind{}
	depPaths = map[string][]FieldPath{}

	// Process body + includeWhen + forEach + templateExpr expressions → depPaths.
	bodyStrs := collectBodyStrings(node)
	err = walkAndAccumulate(bodyStrs, false, node.ID, exprPaths, exprAccessModes, dependencies, depPaths, &selfPaths)
	if err != nil {
		return
	}

	// Process readyWhen + propagateWhen → depPaths + selfPaths.
	gateStrs := collectGateStrings(node)
	err = walkAndAccumulate(gateStrs, true, node.ID, exprPaths, exprAccessModes, dependencies, depPaths, &selfPaths)
	if err != nil {
		return
	}

	return dependencies, depPaths, selfPaths, nil
}

// collectBodyStrings gathers all strings from the node body, templateExpr,
// includeWhen, forEach, and metric that need expression scanning.
func collectBodyStrings(node Node) []string {
	var strs []string
	if body := node.Body(); body != nil {
		CollectStrings(body, &strs)
	}
	if node.TemplateExpr != "" {
		strs = append(strs, node.TemplateExpr)
	}
	for _, s := range node.IncludeWhen {
		strs = append(strs, s)
	}
	if node.ForEach != nil {
		strs = append(strs, node.ForEach.Expr)
	}
	if node.Metric != nil {
		strs = append(strs, node.Metric.Value)
		for _, labelExpr := range node.Metric.Labels {
			strs = append(strs, labelExpr)
		}
	}
	return strs
}

// collectGateStrings gathers readyWhen and propagateWhen strings.
func collectGateStrings(node Node) []string {
	var strs []string
	for _, s := range node.ReadyWhen {
		strs = append(strs, s)
	}
	for _, s := range node.PropagateWhen {
		strs = append(strs, s)
	}
	return strs
}

// walkAndAccumulate scans expression strings and accumulates dependency and
// field path information into the provided accumulators. isGateExpr controls
// whether self-references are recorded into selfPaths.
func walkAndAccumulate(
	strs []string,
	isGateExpr bool,
	nodeID string,
	exprPaths map[string]map[string][]FieldPath,
	exprAccessModes map[string]map[string]bool,
	dependencies map[string]DepKind,
	depPaths map[string][]FieldPath,
	selfPaths *[]FieldPath,
) error {
	var err error
	WalkExpressions(strs, func(expr string) {
		if err != nil {
			return
		}
		accumulateExprPaths(expr, isGateExpr, nodeID, exprPaths, dependencies, depPaths, selfPaths)
		classifyDepKinds(expr, nodeID, exprPaths, exprAccessModes, dependencies)
		err = checkDepsRef(expr, nodeID)
	})
	return err
}

// accumulateExprPaths registers field paths from the post-rewrite AST for a
// single expression. When exprPaths is nil, falls back to string-based
// dependency detection.
func accumulateExprPaths(
	expr string,
	isGateExpr bool,
	nodeID string,
	exprPaths map[string]map[string][]FieldPath,
	dependencies map[string]DepKind,
	depPaths map[string][]FieldPath,
	selfPaths *[]FieldPath,
) {
	if exprPaths == nil {
		id := ExtractFirstIdentifier(expr)
		if id != "" && id != nodeID {
			dependencies[id] = DepHard
		}
		return
	}
	paths, ok := exprPaths[expr]
	if !ok {
		return
	}
	for scopeVar, fieldPaths := range paths {
		if scopeVar == nodeID {
			if isGateExpr {
				for _, fp := range fieldPaths {
					AddFieldPath(selfPaths, fp)
				}
			}
			continue
		}
		for _, fp := range fieldPaths {
			AddPath(depPaths, scopeVar, fp)
		}
	}
}

// classifyDepKinds classifies DepKind from access modes (pre-rewrite AST).
// A scope variable accessed only through optional patterns in ALL expressions
// is DepSoft. Any direct access makes it DepHard.
func classifyDepKinds(
	expr string,
	nodeID string,
	exprPaths map[string]map[string][]FieldPath,
	exprAccessModes map[string]map[string]bool,
	dependencies map[string]DepKind,
) {
	if exprPaths == nil {
		return
	}
	if exprAccessModes != nil {
		modes, ok := exprAccessModes[expr]
		if !ok {
			return
		}
		for scopeVar, optionalOnly := range modes {
			if scopeVar == nodeID {
				continue
			}
			if !optionalOnly {
				dependencies[scopeVar] = DepHard
			} else if _, exists := dependencies[scopeVar]; !exists {
				dependencies[scopeVar] = DepSoft
			}
		}
	} else {
		paths, ok := exprPaths[expr]
		if !ok {
			return
		}
		for scopeVar := range paths {
			if scopeVar != nodeID {
				dependencies[scopeVar] = DepHard
			}
		}
	}
}

// checkDepsRef validates that .dependencies() calls reference only the node
// itself. Cross-node .dependencies() would access scope data without DAG
// edges.
func checkDepsRef(expr string, nodeID string) error {
	remaining := expr
	for {
		idx := strings.Index(remaining, ".dependencies()")
		if idx < 0 {
			return nil
		}
		end := idx
		start := end
		for start > 0 && isIdentContinue(remaining[start-1]) {
			start--
		}
		if start < end && isIdentStart(remaining[start]) {
			id := remaining[start:end]
			if id != nodeID {
				return fmt.Errorf("node %q: .dependencies() can only be called on self (%q), not %q", nodeID, nodeID, id)
			}
		}
		remaining = remaining[idx+len(".dependencies()"):]
	}
}

// WalkExpressions scans strs for ${...} expression blocks (skipping
// deferred $${...} blocks) and calls fn for each expression found.
// This is the single iteration helper for the FindExpr scan loop.
func WalkExpressions(strs []string, fn func(expr string)) {
	for _, s := range strs {
		pos := 0
		for {
			dollars, expr, start, _ := FindExpr(s, pos)
			if start < 0 {
				break
			}
			pos = start + len(dollars) + len(expr) + 2
			if len(dollars) != 1 {
				continue // $${...} is deferred, not evaluated at this level
			}
			fn(expr)
		}
	}
}

// CollectStrings recursively collects all string values from a value tree.
func CollectStrings(v any, out *[]string) {
	switch val := v.(type) {
	case string:
		*out = append(*out, val)
	case map[string]any:
		for _, child := range val {
			CollectStrings(child, out)
		}
	case []any:
		for _, child := range val {
			CollectStrings(child, out)
		}
	}
}

// ExtractFirstIdentifier extracts the first CEL identifier from an expression.
// For "deployment.metadata.name" returns "deployment".
// For "size(items)" returns "size" (a function, not a scope var — but harmless).
// For literals like "'hello'" returns "".
func ExtractFirstIdentifier(expr string) string {
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
		"bytes", "list", "type", "duration", "timestamp", "time":
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

// FieldPath is a sequence of field names representing a path into an object.
// For example, ["status", "availableReplicas"] represents .status.availableReplicas.
//
// A FieldPath is the deepest static prefix of a dot-select chain in a CEL
// expression. Static means no function calls, index operations, or
// comprehensions intervene. Examples:
//
//	deploy.status.availableReplicas      → ["status", "availableReplicas"]
//	deploy.metadata.labels["app"]        → ["metadata", "labels"]
//	deploy.status.conditions.filter(...) → ["status", "conditions"]
//	has(deploy.status.conditions)        → ["status", "conditions"]
//
// The CEL AST walking that produces FieldPaths lives in compiler/fieldpath.go.
type FieldPath []string

// equal returns true if two FieldPaths have the same segments.
func (fp FieldPath) equal(other FieldPath) bool {
	if len(fp) != len(other) {
		return false
	}
	for i := range fp {
		if fp[i] != other[i] {
			return false
		}
	}
	return true
}

// AddFieldPath appends a FieldPath to a slice, deduplicating exact matches.
func AddFieldPath(paths *[]FieldPath, path FieldPath) {
	for _, existing := range *paths {
		if existing.equal(path) {
			return
		}
	}
	*paths = append(*paths, path)
}

// AddPath adds a FieldPath to the result map, deduplicating exact matches.
func AddPath(result map[string][]FieldPath, root string, path FieldPath) {
	for _, existing := range result[root] {
		if existing.equal(path) {
			return
		}
	}
	result[root] = append(result[root], path)
}
