// types.go defines the Graph data model and spec parsing.
//
// Node and GraphSpec are the parsed representations of a Graph's spec.
// Everything else in the package depends on these types; they don't depend
// on anything else in the package.
package graphcontroller

import (
	"fmt"
	"regexp"
	"strings"


)

// nodeIDRe matches valid node IDs: lower camelCase identifiers.
// Must start with a lowercase letter, followed by alphanumeric characters.
var nodeIDRe = regexp.MustCompile(`^[a-z][a-zA-Z0-9]*$`)

// apiVersionRe matches valid Kubernetes apiVersion strings: either a bare
// version (v1, v1alpha1) or group/version (apps/v1, kro.run/v1alpha1).
var apiVersionRe = regexp.MustCompile(`^([a-zA-Z0-9][a-zA-Z0-9.-]*/)?v[0-9]+([a-z]+[0-9]+)?$`)

// NodeType classifies a Graph node by the keyword the user declares. Each
// keyword names a distinct ownership and cleanup contract:
//
//	template:   → Template     (create, manage, delete on prune)
//	patch:      → Patch        (write fields, release on prune)
//	ref:        → Ref          (dereference a named object)
//	watch:      → Watch        (observe a collection by selector)
//	def:        → Definition   (computed values, no Kubernetes resource)
//
// The value under template/patch/def may be either a static map or a CEL
// expression string that yields the body at runtime — the shape is
// disambiguated at parse time. Ref/Watch accept only maps; they are
// identity-only classifications and have no CEL-as-whole-body form.
//
// There is no "unresolved" value. Classification is a property of the spec,
// not of cluster state, so it cannot drift between reconciles within a
// revision.
type NodeType int

const (
	// NodeTypeTemplate — the Graph creates the resource. Applied via SSA.
	// Tracked for cleanup. Deleted on prune.
	NodeTypeTemplate NodeType = iota
	// NodeTypePatch — writes fields on a resource the Graph does not
	// create. Applied via SSA. Tracked for cleanup. Releases fields on prune.
	NodeTypePatch
	// NodeTypeRef — dereference a named object into scope. Identity is
	// apiVersion + kind + metadata.name (+namespace optional). Read-only;
	// the node pulls a single object's current state from the shared
	// kind-scoped informer. Not tracked for cleanup.
	NodeTypeRef
	// NodeTypeWatch — observe a collection of objects by selector.
	// Identity is apiVersion + kind (+ optional selector). No metadata.name.
	// Read-only List+Watch via informer; membership changes drive reactive
	// dispatch. Not tracked for cleanup.
	NodeTypeWatch
	// NodeTypeDef — a computed value expressed as a map of key-value
	// pairs (literals, CEL expressions, or both). The node produces no
	// Kubernetes resource — it defines values and enters the result into
	// scope as map[string]any. No drift timer, no applied-set entry,
	// nothing to clean up.
	NodeTypeDef
)

// String returns the human-readable name of the NodeType for logging and display.
func (r NodeType) String() string {
	switch r {
	case NodeTypeTemplate:
		return "template"
	case NodeTypePatch:
		return "patch"
	case NodeTypeRef:
		return "ref"
	case NodeTypeWatch:
		return "watch"
	case NodeTypeDef:
		return "def"
	default:
		return fmt.Sprintf("NodeType(%d)", int(r))
	}
}

// LabelValue returns the identity label value for this node type. Returns
// ("", false) for classifications that do not stamp an identity label
// (Ref, Watch, Definition). Template and Patch are the only label-bearing
// classifications — they are the two that apply via SSA and must be
// discoverable by the applied-set scan on restart.
func (r NodeType) LabelValue() (string, bool) {
	switch r {
	case NodeTypeTemplate:
		return "template", true
	case NodeTypePatch:
		return "patch", true
	default:
		return "", false
	}
}

// NodeTypeFromLabelValue parses an identity label value back to a NodeType.
// Returns (0, false) if the value is not a recognized label value. Labels
// only carry Template or Patch by construction.
func NodeTypeFromLabelValue(s string) (NodeType, bool) {
	switch s {
	case "template":
		return NodeTypeTemplate, true
	case "patch":
		return NodeTypePatch, true
	default:
		return 0, false
	}
}

// ForEachBinding is a single forEach binding — one iteration variable
// bound to one collection expression. The Node parser normalizes
// both YAML input shapes (flat map and array-of-maps) into this
// struct after validating exactly-one-variable cardinality.
type ForEachBinding struct {
	VarName string // CEL scope variable name
	Expr    string // CEL expression yielding the collection

	// CollectionSource is the scope variable ID that the collection
	// expression references. Set by BuildDAG when the expression
	// references exactly one scope variable AND that variable is not
	// referenced anywhere else on the node (template body, readyWhen,
	// propagateWhen, includeWhen). Empty means the forEach is not
	// eligible for incremental item diffing — the coordinator falls
	// back to O(N) rehash.
	//
	// The invariant: when CollectionSource is set, changes exclusively
	// to that node via CollectionChange are provably safe to handle
	// per-item. Template expressions reference items only through the
	// iteration variable (per-item scoped), never the collection source
	// directly (collection-wide).
	CollectionSource string
}

// Node is a parsed Graph node entry — a user's declaration of intent about
// a Kubernetes resource (or collection of resources via forEach). Definition
// nodes (declared via def:) put values into scope without creating resources.
// It is the unit of the dependency graph: each node has an identity, a body,
// and computed dependency edges populated by BuildDAG.
//
// Exactly one of the five body fields (Template/Patch/Ref/Watch/Def) is
// populated when the body is a static map. When a body-producing keyword
// (template/patch/def) supplies a CEL expression string instead of a map,
// TemplateExpr holds the expression and ExprKeyword records which
// classification it belongs to. The parser enforces mutual exclusivity at
// parse time — see parseNodeList.
//
// "Node" (not "Resource") because a node is a graph-theory concept — it
// occupies a position in the DAG, has edges, and may produce zero, one, or
// many Kubernetes resources at runtime (e.g., forEach). Kubernetes already
// uses "resource" for five distinct concepts; adding a sixth blurs the
// boundary between the user's declaration and the Kubernetes objects it
// produces.
// Lifecycle holds node-level lifecycle policies that direct the controller's
// behavior when applying (and in the future, deleting) the node's resources.
// Per 003-ownership.md § lifecycle.
type Lifecycle struct {
	Apply string // SSA strategy: "" (default, non-force) or "Force"
}

// ForceApply returns true when the lifecycle policy requests force SSA.
func (l Lifecycle) ForceApply() bool {
	return strings.EqualFold(l.Apply, "Force")
}

type Node struct {
	ID string

	// Body fields — exactly one is non-nil when the body is a static map.
	// Parser validates mutual exclusivity across all five.
	Template map[string]any // template: — Graph creates and manages this resource
	Patch    map[string]any // patch: — Graph writes fields on another actor's resource
	Ref      map[string]any // single-object reference (apiVersion + kind + metadata.name)
	Watch    map[string]any // collection observation (apiVersion + kind + optional selector)
	Def      map[string]any // Definition — computed values into scope, no K8s resource

	// TemplateExpr — CEL expression string that evaluates to the whole body
	// map at runtime. Set when a body-producing keyword (template / patch /
	// def) supplies a string value instead of a map. ExprKeyword records
	// which classification the expression belongs to (Template / Patch /
	// Definition). Never set for Ref/Watch — those classifications
	// are identity-only and have no CEL-as-whole-body form.
	TemplateExpr string
	ExprKeyword  NodeType

	// nodeType is the parse-time classification. Read via Type().
	nodeType NodeType

	// hasStatusSubresource is true when a Patch node's body declares a
	// non-nil status field. Used by the teardown path to decide whether
	// release-apply must also release the status subresource.
	// Per 003-ownership.md § Status Subresource.
	hasStatusSubresource bool

	// ForEach holds the single forEach dimension for this node. Per
	// 001-graph.md: "multi-variable cross-product expansion is not
	// supported." The struct encodes this single-dimension constraint
	// at the type level — callers access VarName and Expr directly
	// instead of iterating a map that always has exactly one entry.
	// nil means no forEach on this node.
	ForEach       *ForEachBinding
	Finalizes     string    // target node ID — resource created only during prune/teardown
	Lifecycle     Lifecycle // node-level lifecycle policies (apply strategy, etc.)
	IncludeWhen   []string
	ReadyWhen     []string // CEL conditions; all must be true for the node to be "ready"
	PropagateWhen []string // CEL conditions; input gate — all must be true for this node to evaluate

	// Dependencies are IDs of nodes this node references in its CEL expressions.
	// Populated by BuildDAG; nil before that.
	Dependencies map[string]bool

	// DepPaths maps each dependency to the field paths this node's CEL
	// expressions reference. Populated by BuildDAG; nil before that.
	DepPaths map[string][]FieldPath

	// SelfPaths is the set of field paths into this node's own observed
	// resource that readyWhen, propagateWhen, and downstream expressions
	// reference. Populated by BuildDAG; nil before that.
	SelfPaths []FieldPath

	// ReadinessDeps is the set of upstream node IDs whose readiness state
	// must be checked even when the evaluation hash matches. Populated by
	// BuildDAG; nil before that.
	ReadinessDeps map[string]bool
}

// NodeType returns the parse-time classification of this node.
func (n *Node) Type() NodeType {
	return n.nodeType
}

// Identity returns the static identity view of this node's target — the
// map containing apiVersion, kind, and metadata (name/namespace) used
// to build the applied-set key, resolve GVK, and look up the target
// resource. Returns nil for Definition nodes (no Kubernetes identity)
// and for nodes whose body comes from TemplateExpr (the map isn't
// available until evaluation).
func (n *Node) Identity() map[string]any {
	switch n.nodeType {
	case NodeTypeTemplate:
		return n.Template
	case NodeTypePatch:
		return n.Patch
	case NodeTypeRef:
		return n.Ref
	case NodeTypeWatch:
		return n.Watch
	default:
		return nil
	}
}

// HasDynamicGVR returns true when the node's GVR cannot be determined
// statically — either because the entire body is a CEL expression
// (TemplateExpr, so Identity() returns nil), or because apiVersion or kind
// contains a CEL expression that is only resolved at reconcile time
// (e.g., apiVersion: "${k.spec.group}/${k.spec.version}").
//
// This predicate is scoped to GVR-level dynamism (apiVersion, kind). Dynamic
// name/namespace do not affect GVR resolution and are not checked.
//
// Used by startup hydration to skip nodes whose GVR is unknowable without
// evaluating the CEL environment. Watch nodes with dynamic GVR are safe to
// skip because they observe resources rather than creating them — no orphan
// risk on restart.
func (n *Node) HasDynamicGVR() bool {
	id := n.Identity()
	if id == nil {
		return true // TemplateExpr — whole body is CEL
	}
	for _, field := range []string{"apiVersion", "kind"} {
		if s, ok := id[field].(string); ok && isCELExpression(s) {
			return true
		}
	}
	return false
}

// isCELExpression returns true if s contains a CEL template expression
// marker. Single source of truth for detecting CEL in identity fields —
// used by HasDynamicGVR and extractLiteralGVK.
func isCELExpression(s string) bool {
	return strings.Contains(s, "${")
}

// Payload returns the evaluable body of this node — the map fed to CEL
// evaluation, expression walking, and structural inspection. Returns nil
// for Ref/Watch (identity-only, no body) and for nodes whose body
// comes from TemplateExpr (callers that need both paths should handle
// TemplateExpr separately — see eval.toMapNode).
func (n *Node) Payload() map[string]any {
	switch n.nodeType {
	case NodeTypeTemplate:
		return n.Template
	case NodeTypePatch:
		return n.Patch
	case NodeTypeDef:
		return n.Def
	default:
		return nil
	}
}

// HasBody returns true if the node has an evaluable body — either a
// static map or a CEL expression that yields a map at runtime. Returns
// false for Ref/Watch (identity-only).
func (n *Node) HasBody() bool {
	return n.Payload() != nil || n.TemplateExpr != ""
}

// Body returns the node's body map regardless of node type. All five body
// fields (Template, Patch, Ref, Watch, Def) may carry ${...} expressions
// and must be compiled. Returns nil for nodes with no body (TemplateExpr-only
// nodes have a nil Body — the expression is accessed via TemplateExpr).
func (n *Node) Body() map[string]any {
	for _, body := range []map[string]any{n.Template, n.Patch, n.Ref, n.Watch, n.Def} {
		if body != nil {
			return body
		}
	}
	return nil
}

// Note: there is no IdentityKey method because computing the applied-set
// key requires a GVKScopeResolver (to handle cluster-scoped resources
// correctly per 003-ownership.md § Priority Resolution). The scope
// resolver is a reconciler-level dependency that doesn't belong on the
// Node abstraction. Callers use staticResourceKey(node.Identity(),
// defaultNS, scope) directly.

// GraphSpec holds the parsed spec of a Graph object.
type GraphSpec struct {
	Nodes []Node
}

// AllIdentifiers returns every identifier that CEL expressions in this spec
// might reference: node IDs and forEach iterator variable names.
// Used to build the CEL environment with all variables declared upfront.
func (s *GraphSpec) AllIdentifiers() []string {
	seen := map[string]bool{}
	var ids []string
	add := func(id string) {
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	for _, node := range s.Nodes {
		add(node.ID)
		if node.ForEach != nil {
			add(node.ForEach.VarName)
		}
	}
	return ids
}

// AllExpressions returns every CEL expression string in the spec.
// This is the single source of truth for what expressions exist in a Graph.
// The compilation phase uses this to eagerly compile all programs before
// the reconcile loop touches any nodes.
func (s *GraphSpec) AllExpressions() []string {
	seen := map[string]bool{}
	var exprs []string

	add := func(strs []string) {
		for _, s := range strs {
			// Extract ${...} expressions from each string
			pos := 0
			for {
				dollars, expr, start, _ := findExpr(s, pos)
				if start < 0 {
					break
				}
				pos = start + len(dollars) + len(expr) + 2
				if len(dollars) != 1 {
					continue // $${...} is deferred, not evaluated at this level
				}
				if !seen[expr] {
					seen[expr] = true
					exprs = append(exprs, expr)
				}
			}
		}
	}

	// Collect expressions from each node
	for _, node := range s.Nodes { // Template expressions
		var templateStrings []string
		// Walk body maps that may carry CEL expressions. Ref/Watch
		// bodies (identity-only) also contain ${...} in metadata.name,
		// metadata.namespace, selector values — they must be compiled too.
		if body := node.Body(); body != nil {
			collectStrings(body, &templateStrings)
		}
		if node.TemplateExpr != "" {
			templateStrings = append(templateStrings, node.TemplateExpr)
		}
		add(templateStrings)

		// ForEach collection expressions
		if node.ForEach != nil {
			add([]string{node.ForEach.Expr})
		}

		// Condition expressions (includeWhen, readyWhen, propagateWhen)
		add(node.IncludeWhen)
		add(node.ReadyWhen)
		add(node.PropagateWhen)
	}

	return exprs
}

// extractGraphSpec parses the spec from a Graph object.
func extractGraphSpec(graphObj map[string]any) (*GraphSpec, error) {
	spec, ok := graphObj["spec"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("missing spec")
	}
	rawNodes, ok := spec["nodes"]
	if !ok {
		return nil, fmt.Errorf("missing spec.nodes")
	}
	nodes, err := parseNodeList(rawNodes)
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

// parseNodeList converts a raw node list into Nodes.
// Returns an error on the first invalid node: missing ID, duplicate ID,
// invalid keyword combinations, or per-keyword shape violations.
func parseNodeList(raw any) ([]Node, error) {
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
		} else if fas, ok := m["forceApply"].(string); ok && strings.EqualFold(fas, "true") {
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
	// (e.g., "apps/v1"). The version must match v\d+(\w+\d+)? (e.g., v1,
	// v1alpha1, v1beta2).
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

// stringsToAny converts []string to []any for unstructured serialization.
func stringsToAny(ss []string) []any {
	result := make([]any, len(ss))
	for i, s := range ss {
		result[i] = s
	}
	return result
}
