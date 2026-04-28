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
// returns per-node dependency paths, self paths, and dependency classifications.
//
// When exprPaths is nil (e.g., BuildDAG called for deletion ordering without
// going through compileGraphSpec), falls back to string-based dependency
// detection. DepPaths/SelfPaths will be nil in this case — hashing won't be
// available, but dependency detection for topological sort still works.
//
// exprAccessModes carries per-expression, per-scope-variable optional-access
// classification computed from pre-rewrite CEL ASTs. When non-nil, it drives
// DepKind classification: a dependency accessed only through optional patterns
// (?.field, .ready(), .updated()) in ALL expressions is DepLazy. When nil,
// all dependencies default to DepHard.
//
// The exprPaths map is computed from CEL ASTs during compilation in
// compileGraphSpec. See 005-reconciliation.md § Hash Mechanics.
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

	// Helper: process a CEL expression's pre-extracted paths and access modes.
	// Field paths (from post-rewrite ASTs) drive hash mechanics.
	// Access modes (from pre-rewrite ASTs) drive DepKind classification.
	processExpr := func(expr string, isGateExpr bool) {
		if exprPaths == nil {
			// Fallback: string-based dependency detection only.
			id := ExtractFirstIdentifier(expr)
			if id != "" && id != node.ID {
				dependencies[id] = DepHard
			}
			return
		}

		// Register field paths from the post-rewrite AST.
		if paths, ok := exprPaths[expr]; ok {
			for scopeVar, fieldPaths := range paths {
				if scopeVar == node.ID {
					if isGateExpr {
						for _, fp := range fieldPaths {
							AddFieldPath(&selfPaths, fp)
						}
					}
					continue
				}
				for _, fp := range fieldPaths {
					AddPath(depPaths, scopeVar, fp)
				}
			}
		}

		// Classify DepKind from access modes (pre-rewrite AST).
		// A scope variable accessed only through optional patterns in ALL
		// expressions → DepLazy. Any direct access → DepHard.
		if exprAccessModes != nil {
			if modes, ok := exprAccessModes[expr]; ok {
				for scopeVar, optionalOnly := range modes {
					if scopeVar == node.ID {
						continue
					}
					if !optionalOnly {
						// Direct access in this expression → hard.
						dependencies[scopeVar] = DepHard
					} else {
						// Optional-only in this expression. Set DepLazy if not
						// already hard from another expression.
						if _, exists := dependencies[scopeVar]; !exists {
							dependencies[scopeVar] = DepLazy
						}
					}
				}
			}
		} else {
			// No access modes — default all deps to hard (safe fallback).
			if paths, ok := exprPaths[expr]; ok {
				for scopeVar := range paths {
					if scopeVar != node.ID {
						dependencies[scopeVar] = DepHard
					}
				}
			}
		}
	}

	// Validate .dependencies() references are self-referential only.
	// Per 001-graph.md § CEL Functions: .dependencies() returns the scope
	// values of a node's own dependencies. Cross-node .dependencies()
	// (e.g., ${otherNode.dependencies()...}) would access scope data
	// without DAG edges — reject it.
	checkDepsRef := func(expr string) {
		remaining := expr
		for {
			idx := strings.Index(remaining, ".dependencies()")
			if idx < 0 {
				return
			}
			end := idx
			start := end
			for start > 0 && isIdentContinue(remaining[start-1]) {
				start--
			}
			if start < end && isIdentStart(remaining[start]) {
				id := remaining[start:end]
				if id != node.ID {
					err = fmt.Errorf("node %q: .dependencies() can only be called on self (%q), not %q", node.ID, node.ID, id)
					return
				}
			}
			remaining = remaining[idx+len(".dependencies()"):]
		}
	}

	// Process body + includeWhen + forEach expressions → depPaths.
	// Walk every possible body map — Template/Patch/Def (Payload) plus
	// Ref/Watch (Identity) — because identity fields on Ref and
	// Watch may still carry CEL expressions (e.g., a dynamic name
	// from an upstream node).
	var templateStrs []string
	if body := node.Body(); body != nil {
		CollectStrings(body, &templateStrs)
	}
	if node.TemplateExpr != "" {
		templateStrs = append(templateStrs, node.TemplateExpr)
	}
	for _, s := range node.IncludeWhen {
		templateStrs = append(templateStrs, s)
	}
	if node.ForEach != nil {
		templateStrs = append(templateStrs, node.ForEach.Expr)
	}

	for _, s := range templateStrs {
		pos := 0
		for {
			dollars, expr, start, _ := FindExpr(s, pos)
			if start < 0 {
				break
			}
			pos = start + len(dollars) + len(expr) + 2
			if len(dollars) != 1 {
				continue
			}
			processExpr(expr, false)
			checkDepsRef(expr)
			if err != nil {
				return
			}
		}
	}

	// Process readyWhen + propagateWhen → depPaths + selfPaths.
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
			dollars, expr, start, _ := FindExpr(s, pos)
			if start < 0 {
				break
			}
			pos = start + len(dollars) + len(expr) + 2
			if len(dollars) != 1 {
				continue
			}
			processExpr(expr, true)
			checkDepsRef(expr)
			if err != nil {
				return
			}
		}
	}

	return dependencies, depPaths, selfPaths, nil
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
