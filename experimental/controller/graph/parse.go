// parse.go converts raw unstructured node lists ([]any from YAML/JSON) into
// the typed Node and GraphSpec data model. Includes per-keyword validation
// (template, patch, ref, watch, def) that enforces shape rules at parse time.
package graph

import (
	"fmt"
	"regexp"
	"strings"
)

// nodeIDRe matches valid Graph node identifiers: any valid CEL identifier.
// Must start with a letter or underscore, followed by alphanumeric or underscore.
// Stricter naming conventions (e.g., lower camelCase for RGD resources) are
// enforced at the consumer level — see rgd.yaml's validation node.
var nodeIDRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// apiVersionRe matches valid Kubernetes apiVersion strings: either a bare
// version (v1, v1alpha1) or group/version (apps/v1, kro.run/v1alpha1).
// The version qualifier is restricted to "alpha" or "beta" followed by
// digits, matching upstream Kubernetes conventions.
var apiVersionRe = regexp.MustCompile(`^([a-zA-Z0-9][a-zA-Z0-9.-]*/)?v[0-9]+(?:(?:alpha|beta)[0-9]+)?$`)

// ExtractGraphSpec parses the spec from a Graph object.
func ExtractGraphSpec(graphObj map[string]any) (*GraphSpec, error) {
	spec, ok := graphObj["spec"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("missing spec")
	}
	rawNodes, ok := spec["nodes"]
	if !ok {
		return nil, fmt.Errorf("missing spec.nodes")
	}
	nodes, err := ParseNodeList(rawNodes)
	if err != nil {
		return nil, err
	}

	return &GraphSpec{Nodes: nodes}, nil
}

// bodyKeywords enumerates the five mutually-exclusive classification keywords.
// Exactly one must be set per node. The value shape disambiguates map-vs-expr:
//   - map[string]any → static body
//   - string         → CEL expression evaluating to the body at runtime
//
// Ref and Watch accept only maps — they are identity-only classifications
// and have no CEL-as-whole-body form.
var bodyKeywords = []string{"template", "patch", "ref", "watch", "def"}

// ParseNodeList converts a raw node list into Nodes.
// Returns an error on the first invalid node: missing ID, duplicate ID,
// invalid keyword combinations, or per-keyword shape violations.
func ParseNodeList(raw any) ([]Node, error) {
	list, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("spec.nodes is %T, want []any", raw)
	}

	seen := make(map[string]bool, len(list))
	// Track lowercased IDs to detect case-collisions. Per 001-graph.md:
	// "The node ID is lowercased when embedded in the identity label key;
	// IDs that collide after lowercasing are rejected at compile time."
	seenLower := make(map[string]string, len(list)) // lowercased → original
	// Reserved words that must not be used as node IDs or forEach iterator
	// variable names. Limited to CEL language keywords and internal scope
	// variables that would cause actual conflicts. Application-level
	// conventions (like "schema", "instance") are not included — they are
	// valid node IDs used by stdlib graphs.
	reservedWords := map[string]bool{
		// CEL language keywords
		"true": true, "false": true, "null": true, "in": true,
		"as": true, "break": true, "const": true, "continue": true,
		"else": true, "for": true, "function": true, "if": true,
		"import": true, "let": true, "loop": true, "package": true,
		"return": true, "var": true, "void": true, "while": true,
		// Graph controller internal scope variables
		"self": true,
	}
	// First pass: collect all node IDs (lowercased) so the forEach
	// variable collision check can catch collisions with nodes declared
	// anywhere in the list, not just those parsed so far. Both node IDs
	// and forEach variables enter the same CEL scope — collisions cause
	// silent shadowing.
	allNodeIDsLower := make(map[string]string, len(list)) // lowercased → original
	for _, item := range list {
		if m, ok := item.(map[string]any); ok {
			if id, ok := m["id"].(string); ok && id != "" {
				allNodeIDsLower[strings.ToLower(id)] = id
			}
		}
	}
	var nodes []Node
	for i, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("node[%d] is %T, want map", i, item)
		}

		node := Node{}
		id, ok := m["id"].(string)
		if !ok || id == "" {
			return nil, fmt.Errorf("node[%d]: missing or empty id", i)
		}
		// Per 001-graph.md: node IDs must be lower camelCase identifiers.
		// This check subsumes hyphen, underscore, uppercase-first, digit-first,
		// and special-character rejections in a single regex.
		if !nodeIDRe.MatchString(id) {
			return nil, fmt.Errorf("node[%d] %q: naming convention violation: id %s is not a valid KRO resource id: must be lower camelCase", i, id, id)
		}
		// Reserved words must not be used as node IDs — they shadow built-in
		// scope variables or CEL keywords.
		if reservedWords[strings.ToLower(id)] {
			return nil, fmt.Errorf("node[%d] %q: naming convention violation: id %s is a reserved keyword", i, id, id)
		}
		if seen[id] {
			return nil, fmt.Errorf("node[%d]: found duplicate resource IDs %q", i, id)
		}
		lower := strings.ToLower(id)
		if orig, exists := seenLower[lower]; exists && orig != id {
			return nil, fmt.Errorf("node[%d] %q: collides with %q after lowercasing (both produce %q in identity labels)", i, id, orig, lower)
		}
		seen[id] = true
		seenLower[lower] = id
		node.ID = id

		// Collect which of the five mutually-exclusive classification
		// keywords are present. Exactly one must be set.
		var present []string
		for _, kw := range bodyKeywords {
			if _, ok := m[kw]; ok {
				present = append(present, kw)
			}
		}
		if len(present) == 0 {
			return nil, fmt.Errorf("node[%d] %q: exactly one of %s must be set, got none", i, id, strings.Join(bodyKeywords, "/"))
		}
		if len(present) > 1 {
			return nil, fmt.Errorf("node[%d] %q: exactly one of %s must be set, got {%s}", i, id, strings.Join(bodyKeywords, "/"), strings.Join(present, ", "))
		}

		kw := present[0]
		rawBody := m[kw]
		if err := setNodeKeyword(&node, kw, rawBody); err != nil {
			return nil, fmt.Errorf("node[%d] %q: %w", i, id, err)
		}

		if fin, ok := m["finalizes"].(string); ok {
			node.Finalizes = fin
		}
		if fe, ok := m["forEach"].(map[string]any); ok {
			// Flat map format (Graph templates): forEach: {region: "${...}"}
			if len(fe) == 0 {
				return nil, fmt.Errorf("node[%d] %q: forEach must have at least one dimension", i, id)
			}
			if len(fe) > 1 {
				return nil, fmt.Errorf("node[%d] %q: forEach must have exactly one variable (got %d); multi-variable cross-product expansion is not supported", i, id, len(fe))
			}
			for k, v := range fe {
				vs, ok := v.(string)
				if !ok {
					return nil, fmt.Errorf("node[%d] %q: forEach variable %q value must be a string, got %T", i, id, k, v)
				}
				node.ForEach = &ForEachBinding{VarName: k, Expr: vs}
			}
		} else if feArr, ok := m["forEach"].([]any); ok {
			// Array format (upstream kro API): forEach: [{region: "${...}"}]
			// Each element is a map of variable bindings. Validate exactly one variable total.
			parsed := make(map[string]string)
			for j, dim := range feArr {
				dimMap, ok := dim.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("node[%d] %q: forEach[%d] must be a map, got %T", i, id, j, dim)
				}
				for k, v := range dimMap {
					if _, exists := parsed[k]; exists {
						return nil, fmt.Errorf("node[%d] %q: forEach has duplicate variable %q", i, id, k)
					}
					vs, ok := v.(string)
					if !ok {
						return nil, fmt.Errorf("node[%d] %q: forEach[%d] variable %q value must be a string, got %T", i, id, j, k, v)
					}
					parsed[k] = vs
				}
			}
			if len(parsed) == 0 {
				return nil, fmt.Errorf("node[%d] %q: forEach must have at least one dimension", i, id)
			}
			if len(parsed) > 1 {
				return nil, fmt.Errorf("node[%d] %q: forEach must have exactly one variable (got %d); multi-variable cross-product expansion is not supported", i, id, len(parsed))
			}
			for k, vs := range parsed {
				node.ForEach = &ForEachBinding{VarName: k, Expr: vs}
			}
		} else if _, hasForEach := m["forEach"]; hasForEach && m["forEach"] != nil {
			return nil, fmt.Errorf("node[%d] %q: forEach must be a map or array, got %T", i, id, m["forEach"])
		}
		if node.ForEach != nil {
			// Validate: forEach iterator variable names must not be reserved keywords.
			varLower := strings.ToLower(node.ForEach.VarName)
			if reservedWords[varLower] {
				return nil, fmt.Errorf("node[%d] %q: forEach iterator %q is a reserved keyword", i, id, node.ForEach.VarName)
			}
			// Validate: forEach iterator variable names must not collide
			// with any node ID in the graph (case-insensitive). Both enter the
			// same CEL scope, so a collision would shadow the node. The check
			// uses allNodeIDs (collected in the first pass above) rather than
			// `seen` to catch collisions with nodes declared later in the list.
			if collidingID, exists := allNodeIDsLower[varLower]; exists {
				return nil, fmt.Errorf("node[%d] %q: forEach iterator %q conflicts with resource ID %q", i, id, node.ForEach.VarName, collidingID)
			}
		}
		// Validate: finalizes nodes must not have CEL-evaluated names unless
		// they also have forEach (which requires dynamic per-item names).
		// Static-name finalizers are looked up by key during prune; forEach
		// finalizers use label-based discovery for cleanup.
		// NOTE: This check must run after forEach is parsed above.
		if node.Finalizes != "" && node.ForEach == nil {
			if body := node.Identity(); body != nil {
				if md, ok := body["metadata"].(map[string]any); ok {
					if name, ok := md["name"].(string); ok && strings.Contains(name, "${") {
						return nil, fmt.Errorf("node[%d] %q: finalizes nodes must not have CEL-evaluated names (found expression in metadata.name); use forEach for per-item finalizers", i, id)
					}
				}
			}
			if node.Type() == NodeTypeDef {
				return nil, fmt.Errorf("node[%d] %q: finalizes is not valid on def nodes (no Kubernetes resource to finalize)", i, id)
			}
		}
		if iw, err := parseStringList(m, "includeWhen", i, id); err != nil {
			return nil, err
		} else if len(iw) > 0 {
			node.IncludeWhen = iw
		}
		if rw, err := parseStringList(m, "readyWhen", i, id); err != nil {
			return nil, err
		} else if len(rw) > 0 {
			node.ReadyWhen = rw
		}
		if pw, err := parseStringList(m, "propagateWhen", i, id); err != nil {
			return nil, err
		} else if len(pw) > 0 {
			node.PropagateWhen = pw
		}
		if lc, ok := m["lifecycle"].(map[string]any); ok {
			if apply, ok := lc["apply"].(string); ok {
				node.Lifecycle.Apply = apply
			}
		}
		// Also accept forceApply: true for CEL-generated node maps where
		// nested map types may not survive JSON round-trip via the API server.
		if fa, ok := m["forceApply"].(bool); ok && fa {
			node.Lifecycle.Apply = "Force"
		}
		nodes = append(nodes, node)
	}
	return nodes, nil
}

// setNodeKeyword populates the Node's classification-bearing fields from the
// single declared keyword. Each body-producing keyword (template / patch /
// def) accepts either a static map or a CEL expression string that yields
// the body at runtime. Ref and Watch accept only maps — they are
// identity-only classifications and have no CEL-as-whole-body form.
//
// Per-keyword shape is validated here and the parse-time NodeType is
// recorded. The caller has already verified exactly one keyword is set.
func setNodeKeyword(node *Node, kw string, raw any) error {
	switch kw {
	case "template":
		node.nodeType = NodeTypeTemplate
		switch v := raw.(type) {
		case map[string]any:
			if err := validateTemplate(v); err != nil {
				return err
			}
			node.Template = v
		case string:
			if v == "" {
				return fmt.Errorf("template: empty string (expected CEL expression or non-empty map)")
			}
			node.TemplateExpr = v
			node.ExprKeyword = NodeTypeTemplate
		default:
			return fmt.Errorf("template: expected map or string (CEL expression), got %T", raw)
		}
	case "patch":
		node.nodeType = NodeTypePatch
		switch v := raw.(type) {
		case map[string]any:
			if err := validatePatch(v); err != nil {
				return err
			}
			node.Patch = v
			if status, ok := v["status"]; ok && status != nil {
				node.hasStatusSubresource = true
			}
		case string:
			if v == "" {
				return fmt.Errorf("patch: empty string (expected CEL expression or non-empty map)")
			}
			node.TemplateExpr = v
			node.ExprKeyword = NodeTypePatch
		default:
			return fmt.Errorf("patch: expected map or string (CEL expression), got %T", raw)
		}
	case "ref":
		m, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("ref: expected map, got %T (ref is identity-only and has no CEL-as-whole-body form)", raw)
		}
		if err := validateRef(m); err != nil {
			return err
		}
		node.Ref = m
		node.nodeType = NodeTypeRef
	case "watch":
		m, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("watch: expected map, got %T (watch is identity-only and has no CEL-as-whole-body form)", raw)
		}
		if err := validateWatch(m); err != nil {
			return err
		}
		node.Watch = m
		node.nodeType = NodeTypeWatch
	case "def":
		node.nodeType = NodeTypeDef
		switch v := raw.(type) {
		case map[string]any:
			if err := validateDef(v); err != nil {
				return err
			}
			node.Def = v
		case string:
			if v == "" {
				return fmt.Errorf("def: empty string (expected CEL expression or non-empty map)")
			}
			node.TemplateExpr = v
			node.ExprKeyword = NodeTypeDef
		default:
			return fmt.Errorf("def: expected map or string (CEL expression), got %T", raw)
		}
	default:
		return fmt.Errorf("unknown keyword %q", kw) // unreachable — caller already filtered
	}
	return nil
}

// ---------------------------------------------------------------------------
// Per-keyword validators
// ---------------------------------------------------------------------------

// validateTemplate enforces shape rules for template: bodies.
//
// A template body must declare identity (apiVersion + kind) — the graph needs to
// know what it is creating. metadata.name is not strictly required at parse
// time because it may come from a CEL expression that is resolved at runtime,
// but apiVersion and kind must be literal or ${...}-interpolated strings
// (i.e., present as string values in the map).
func validateTemplate(tmpl map[string]any) error {
	if _, ok := tmpl["apiVersion"]; !ok {
		return fmt.Errorf("template: missing apiVersion (use def: for computed values without a Kubernetes resource)")
	}
	if _, ok := tmpl["kind"]; !ok {
		return fmt.Errorf("template: missing kind")
	}
	// Validate apiVersion format when it's a literal string (not a CEL
	// expression). Must be either "version" (e.g., "v1") or "group/version"
	// (e.g., "apps/v1"). The version qualifier is restricted to "alpha" or
	// "beta" followed by digits (e.g., v1, v1alpha1, v1beta2).
	if av, ok := tmpl["apiVersion"].(string); ok && !strings.Contains(av, "${") {
		if !apiVersionRe.MatchString(av) {
			return fmt.Errorf("template: invalid apiVersion %q: must be \"version\" or \"group/version\" (e.g., v1, apps/v1)", av)
		}
	}
	return nil
}

// validatePatch enforces shape rules for patch: bodies.
//
// A patch body must carry full identity (apiVersion + kind +
// metadata.name) — the graph writes fields to a specific existing object.
// Identity-only patches are nonsensical (use ref: for read-only
// single-object observation). Force adoption is not valid on patch: — that
// path belongs to template:.
func validatePatch(tmpl map[string]any) error {
	if _, ok := tmpl["apiVersion"]; !ok {
		return fmt.Errorf("patch: missing apiVersion")
	}
	if _, ok := tmpl["kind"]; !ok {
		return fmt.Errorf("patch: missing kind")
	}
	md, _ := tmpl["metadata"].(map[string]any)
	if _, ok := md["name"]; !ok {
		return fmt.Errorf("patch: missing metadata.name (a patch targets a named existing resource; omit metadata.name for watch:)")
	}
	return nil
}

// validateRef enforces shape rules for ref: (single-object dereference)
// bodies. A ref body is identity-only: apiVersion, kind, and metadata
// with only name and namespace. Any other field is a parse error — the
// keyword names an existing object to pull into scope, not a body to apply.
func validateRef(tmpl map[string]any) error {
	if _, ok := tmpl["apiVersion"]; !ok {
		return fmt.Errorf("ref: missing apiVersion")
	}
	if _, ok := tmpl["kind"]; !ok {
		return fmt.Errorf("ref: missing kind")
	}
	md, _ := tmpl["metadata"].(map[string]any)
	if _, ok := md["name"]; !ok {
		return fmt.Errorf("ref: missing metadata.name (for collection observation use watch:)")
	}
	for key := range tmpl {
		switch key {
		case "apiVersion", "kind", "metadata":
			continue
		default:
			return fmt.Errorf("ref: unexpected field %q (ref is identity-only — use template: or patch: to apply fields)", key)
		}
	}
	for key := range md {
		switch key {
		case "name", "namespace":
			continue
		default:
			return fmt.Errorf("ref: unexpected metadata field %q (ref is identity-only)", key)
		}
	}
	return nil
}

// validateWatch enforces shape rules for watch: (collection observation)
// bodies. Must declare apiVersion + kind, must NOT declare metadata.name
// (name implies single-object — use ref:). May declare a selector.
func validateWatch(tmpl map[string]any) error {
	if _, ok := tmpl["apiVersion"]; !ok {
		return fmt.Errorf("watch: missing apiVersion")
	}
	if _, ok := tmpl["kind"]; !ok {
		return fmt.Errorf("watch: missing kind")
	}
	md, _ := tmpl["metadata"].(map[string]any)
	if _, ok := md["name"]; ok {
		return fmt.Errorf("watch: metadata.name is not valid (name implies single-object; use ref:)")
	}
	return nil
}

// validateDef enforces shape rules for def: bodies. Must NOT carry
// apiVersion or kind — a def node produces no Kubernetes resource, only
// values into scope.
func validateDef(tmpl map[string]any) error {
	if _, ok := tmpl["apiVersion"]; ok {
		return fmt.Errorf("def: apiVersion is not valid (def produces values into scope, not a Kubernetes resource; use template:/patch:/ref:/watch: for a resource)")
	}
	if _, ok := tmpl["kind"]; ok {
		return fmt.Errorf("def: kind is not valid (def produces values into scope, not a Kubernetes resource)")
	}
	return nil
}

// parseStringList extracts a validated []string from a map key that holds []any.
// Returns an error if any element is not a string, including the node index and
// ID for diagnostics.
func parseStringList(m map[string]any, key string, nodeIdx int, nodeID string) ([]string, error) {
	raw, ok := m[key].([]any)
	if !ok {
		return nil, nil
	}
	result := make([]string, 0, len(raw))
	for j, v := range raw {
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("node[%d] %q: %s[%d] must be a string, got %T", nodeIdx, nodeID, key, j, v)
		}
		result = append(result, s)
	}
	return result, nil
}
