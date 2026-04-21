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

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// evaluator holds the pre-compiled expression cache and the current scope
// for a single reconcile cycle. The scope is owned by the coordinator —
// workers receive read-only snapshots and return results for the coordinator
// to merge. No locking needed.
type evaluator struct {
	compiled *compiledGraph
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

	// forEach state — populated by the coordinator before dispatching a
	// forEach worker, and read by reconcileForEach. Workers write to these
	// maps (they're private to the worker's evaluator copy), and the results
	// are returned to the coordinator for merging into the shared cache.
	forEachPrevItems  map[string][]any               // cache key → previous collection items
	forEachPrevScope  map[string]map[string]any      // nodeID → itemID → previous scope data
	forEachPrevKeys   map[string]map[string][]string // nodeID → itemID → previous applied keys
	forEachPrevHashes map[string]map[string]string   // nodeID → itemID → previous content hash
	forEachNewItems   map[string][]any               // cache key → updated collection items (output)
	forEachNewScope   map[string]map[string]any      // nodeID → itemID → updated scope data (output)
	forEachNewKeys    map[string]map[string][]string // nodeID → itemID → updated keys (output)
	forEachNewHashes  map[string]map[string]string   // nodeID → itemID → updated content hash (output)

	// forEachChangedItems is set by the coordinator when a forEach dispatch
	// is triggered exclusively by collection-item changes with stable
	// identities. Contains the item identities (namespace/name) that changed.
	// nil means full rehash — the coordinator absorbs ambiguity; the worker
	// never hedges.
	forEachChangedItems map[string]bool

	// Watch incremental cache state — populated by the coordinator
	// before dispatching a Watch worker (cached list + collection
	// changes). The worker merges changes into the cached list and returns
	// the updated list via collectionUpdatedCache.
	// Per 005-reconciliation.md § Propagation: "When a single resource
	// changes, update the cached list incrementally rather than re-listing
	// — O(1) per event, not O(matching)."
	collectionNodeID       string             // node ID for cache key (set by coordinator)
	collectionCachedList   []any              // previous cached list (nil = no cache, full list needed)
	collectionChanges      []CollectionChange // buffered changes since last reconcile
	collectionUpdatedCache []any              // output: updated list for coordinator to store
	collectionDriftOrFull  bool               // true = bypass cache, do full list (drift or first reconcile)
	// collectionDidFullList is set true by reconcileWatch when the
	// worker took the full-List path (as opposed to incremental merge).
	// Used by the coordinator to tighten dirty-flag clearing: dirty is
	// cleared only when a full re-List has successfully completed,
	// because dirty means "drained incremental changes were lost, cache
	// is suspect." An incremental-merge success with an already-stale
	// cache does not address the staleness. Per 005-reconciliation.md
	// § Propagation.
	collectionDidFullList bool

	// dynamicGVKResolved maps node ID → resolved GVK for dynamic GVK nodes.
	// Per 004-compilation.md § Deferred Types: recorded after template evaluation
	// so the coordinator can compare against the previous reconcile. When a
	// dynamic GVK changes, the artifact is stale.
	dynamicGVKResolved map[string]schema.GroupVersionKind
}

// newEvaluator creates an evaluator for a reconcile cycle.
func newEvaluator(state *instanceState) *evaluator {
	// nodeReady carries per-Watch readyWhen verdicts. It lives on
	// instanceState so verdicts persist across reconciles — a Watch
	// that isn't re-evaluated on a given reconcile must still expose its
	// last verdict to downstream `<wk_id>.ready()` lookups. Scope
	// reference is stable — later writes to the map are observable by
	// all subsequent evaluations through the same scope entry. Per
	// 001-graph.md § readyWhen.
	if state.nodeReady == nil {
		state.nodeReady = map[string]bool{}
	}
	scope := map[string]any{
		reservedNodeReadyVar: state.nodeReady,
	}
	// Initialize dynamic GVK tracking if the compiled graph has dynamic nodes.
	var dynamicGVKResolved map[string]schema.GroupVersionKind
	if state.compiled != nil && len(state.compiled.dynamicGVKNodes) > 0 {
		dynamicGVKResolved = make(map[string]schema.GroupVersionKind, len(state.compiled.dynamicGVKNodes))
	}
	return &evaluator{
		compiled:           state.compiled,
		scope:              scope,
		nodeReady:          state.nodeReady,
		dynamicGVKResolved: dynamicGVKResolved,
	}
}

// withScope returns a new evaluator that shares the compiled graph but has its own scope.
// Used for forEach inner scopes and worker snapshots.
func (e *evaluator) withScope(scope map[string]any) *evaluator {
	// Preserve the node-readiness sidecar across scope substitutions so
	// inner forEach evaluations see the same verdicts as the outer walk.
	if e.nodeReady != nil {
		if _, present := scope[reservedNodeReadyVar]; !present {
			scope[reservedNodeReadyVar] = e.nodeReady
		}
	}
	return &evaluator{compiled: e.compiled, scope: scope, nodeReady: e.nodeReady, effectiveGeneration: e.effectiveGeneration}
}

// collectionCacheUpdate returns a map of Watch cache updates if the
// evaluator's collectionUpdatedCache is set. Returns nil if no cache
// update was produced (the worker was not a Watch node, or the
// reconcileWatch call failed before producing a cache update).
func (e *evaluator) collectionCacheUpdate() map[string][]any {
	if e.collectionUpdatedCache == nil || e.collectionNodeID == "" {
		return nil
	}
	return map[string][]any{e.collectionNodeID: e.collectionUpdatedCache}
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
//
// Safety: this mutates a map inside e.scope. The mutation is safe because
// markReady runs only in two contexts: (1) inside a worker goroutine
// operating on a snapshot created by snapshotFor — the map is private to
// that worker, and (2) in the coordinator after the worker's result has
// been merged into scope — single-threaded, no concurrent readers. If the
// concurrency model changes (e.g., shared scope across workers), this
// mutation would introduce a data race.
func (e *evaluator) markReady(nodeID string, ready bool) {
	if m, ok := e.scope[nodeID].(map[string]any); ok {
		m["__ready"] = ready
	}
}

// markUpdated injects the __updated flag into a node's scope data. This is
// read by the .updated() CEL member function. Per 001-graph.md § CEL Functions:
// ".updated() — true when the node is on the latest graph generation."
//
// Same safety model as markReady: only mutated in worker-private snapshots
// or coordinator-serial code.
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
	gvk := gvkFromMap(item)
	kind := gvk.Kind
	group := gvk.Group

	labelKey := forEachChildGenerationLabelKey(parentID, name, namespace, kind, group, graphName, graphNS)
	genVal, _ := lbls[labelKey].(string)
	if genVal == "" {
		return false
	}
	return genVal == fmt.Sprintf("%d", effectiveGeneration)
}

// evalReadiness evaluates readyWhen conditions and stamps __ready in scope.
// Returns ErrWaitingForReadiness if any condition is false or data-pending.
// Returns ErrReadyWhenFailed wrapping the underlying error if the expression
// itself is broken (wrong return type, CEL error). Per 001-graph.md:
// "readyWhen is a health signal — it does not gate downstream execution."
// The ErrReadyWhenFailed sentinel lets the coordinator classify this as
// NodeNotReady (not NodeError), preserving the design invariant.
func (e *evaluator) evalReadiness(nodeID string, readyWhen []string) error {
	if len(readyWhen) > 0 {
		if err := e.evalReadinessConditions(readyWhen, nodeID); err != nil {
			e.markReady(nodeID, false)
			// ErrWaitingForReadiness and ErrPending are transient — pass through.
			// All other errors are permanent expression failures that must not
			// produce NodeError (which would gate dependents). Wrap with
			// ErrReadyWhenFailed so the coordinator classifies as NodeNotReady.
			if errors.Is(err, ErrWaitingForReadiness) || errors.Is(err, ErrPending) {
				return err
			}
			return fmt.Errorf("%w: %w", ErrReadyWhenFailed, err)
		}
	}
	e.markReady(nodeID, true)
	return nil
}

// evalReadinessConditions evaluates readyWhen boolean conditions against the
// full scope. Returns nil if all conditions pass, ErrWaitingForReadiness if
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

	for _, cond := range conditions {
		ok, err := e.evalBoolCondition(cond)
		if err != nil {
			if isPending(err) {
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
// Returns true if all conditions pass (input gate is open, node should evaluate).
// Returns false if any condition is false or errors.
//
// propagateWhen is an input gate — expressions reference upstream nodes,
// not the node itself. Evaluating against the full scope (not a restricted
// single-node scope) is required for cross-node references to resolve.
// The DAG walk's topological order guarantees that all dependency nodes
// are already in e.scope when this is called.
func (e *evaluator) checkPropagateWhen(conditions []string, nodeID string) bool {
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
// Normalizes data-pending errors to ErrPending. Non-pending evaluation
// failures are wrapped with ErrEvaluation so classifyAPIError can
// distinguish them from network errors with similar message text.
func (e *evaluator) toMap(tmpl map[string]any) (map[string]any, error) {
	evaluated, err := e.template(tmpl)
	if err != nil {
		if isPending(err) {
			return nil, ErrPending
		}
		return nil, fmt.Errorf("%w: %w", ErrEvaluation, err)
	}
	result, ok := evaluated.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w: evaluated to %T, want map", ErrEvaluation, evaluated)
	}
	return result, nil
}

// toMapNode evaluates a node's body — either a static map (Template/Patch/
// Ref/Watch/Def) or a CEL expression string under template/patch/def
// that yields the body map at runtime. For Ref/Watch (identity-only)
// the identity map is evaluated — its fields may still contain CEL
// expressions.
func (e *evaluator) toMapNode(node Node) (map[string]any, error) {
	if node.TemplateExpr != "" {
		// Body came in as a CEL expression under the classification
		// keyword (template/patch/def as a string). Use evalString
		// which handles ${...} extraction and type coercion.
		raw, err := e.evalString(node.TemplateExpr)
		if err != nil {
			if isPending(err) {
				return nil, ErrPending
			}
			return nil, fmt.Errorf("%w: %s expression %q: %w", ErrEvaluation, node.ExprKeyword, node.TemplateExpr, err)
		}
		result, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%w: %s expression evaluated to %T, want map", ErrEvaluation, node.ExprKeyword, raw)
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
			if isPending(err) {
				return false, fmt.Errorf("includeWhen data pending: %w", ErrPending)
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

// extractReferencedPathsFromNode scans a Node's template and gate expressions
// for ${...} blocks, looks up pre-extracted field paths from exprPaths, and
// returns per-node dependency paths, self paths, readiness deps, and dependency IDs.
//
// When exprPaths is nil (e.g., BuildDAG called for deletion ordering without
// going through compileGraphSpec), falls back to string-based dependency
// detection. DepPaths/SelfPaths will be nil in this case — hashing won't be
// available, but dependency detection for topological sort still works.
//
// The exprPaths map is computed from CEL ASTs during compilation in
// compileGraphSpec. See 005-reconciliation.md § Hash Mechanics.
func extractReferencedPathsFromNode(node Node, exprPaths map[string]map[string][]FieldPath) (
	dependencies map[string]bool,
	depPaths map[string][]FieldPath,
	selfPaths []FieldPath,
	readinessDeps map[string]bool,
) {
	dependencies = map[string]bool{}
	depPaths = map[string][]FieldPath{}
	readinessDeps = map[string]bool{}

	// Helper: process a CEL expression's pre-extracted paths.
	// When exprPaths is nil, falls back to string-based identifier extraction
	// for dependency detection (no field paths available).
	processExpr := func(expr string, isGateExpr bool) {
		if exprPaths == nil {
			// Fallback: string-based dependency detection only.
			id := extractFirstIdentifier(expr)
			if id != "" && id != node.ID {
				dependencies[id] = true
			}
			return
		}
		paths, ok := exprPaths[expr]
		if !ok {
			return
		}
		for scopeVar, fieldPaths := range paths {
			if scopeVar == node.ID {
				// Self-reference — only meaningful in gate expressions
				if isGateExpr {
					for _, fp := range fieldPaths {
						addFieldPath(&selfPaths, fp)
					}
				}
				continue
			}
			// Upstream dependency reference
			dependencies[scopeVar] = true
			for _, fp := range fieldPaths {
				addPath(depPaths, scopeVar, fp)
			}
		}
	}

	// Helper: check if an expression contains a .ready() call on a non-self node.
	// The field path walker skips ready() targets (no paths extracted), so we
	// need a separate string-based check for ReadinessDeps. These are also
	// dependencies — the node needs the upstream in scope to check readiness.
	checkReadyRef := func(expr string) {
		if !strings.Contains(expr, ".ready()") {
			return
		}
		id := extractFirstIdentifier(expr)
		if id != "" && id != node.ID {
			readinessDeps[id] = true
			dependencies[id] = true // ready() targets are dependencies too
		}
	}

	// Process body + includeWhen + forEach expressions → depPaths.
	// Walk every possible body map — Template/Patch/Def (Payload) plus
	// Ref/Watch (Identity) — because identity fields on Ref and
	// Watch may still carry CEL expressions (e.g., a dynamic name
	// from an upstream node).
	var templateStrs []string
	if body := node.Body(); body != nil {
		collectStrings(body, &templateStrs)
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
			dollars, expr, start, _ := findExpr(s, pos)
			if start < 0 {
				break
			}
			pos = start + len(dollars) + len(expr) + 2
			if len(dollars) != 1 {
				continue
			}
			processExpr(expr, false)
		}
	}

	// Process readyWhen + propagateWhen → depPaths + selfPaths + readinessDeps
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
			processExpr(expr, true)
			checkReadyRef(expr)
		}
	}

	return dependencies, depPaths, selfPaths, readinessDeps
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
