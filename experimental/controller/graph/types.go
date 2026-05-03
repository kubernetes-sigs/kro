// types.go defines the Graph data model — Node, GraphSpec, NodeType, and
// their methods. Everything else in the package depends on these types;
// they don't depend on anything else in the package.
package graph

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

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

// DepKind classifies a dependency edge. The classification is determined by
// AST analysis at compile time: a dependency accessed only through optional
// patterns (?.field, [?index], .ready(), .updated()) across all expressions
// in the consumer is Lazy. If any expression accesses the dependency directly,
// it is Hard.
type DepKind int

const (
	// DepHard — every evaluation path requires the target's data. Gates
	// dispatch ordering and contagious exclusion.
	DepHard DepKind = iota
	// DepLazy — all evaluation paths access the target through optional
	// patterns. The scope value is optional.of(data) when present,
	// optional.none() when absent. Participates in propagation triggering
	// only — no dispatch ordering, no contagious exclusion.
	DepLazy
)

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
	// scope as map[string]any. No resync timer, no applied-set entry,
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
	case NodeTypeTemplate, NodeTypePatch:
		return r.String(), true
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
// parse time — see ParseNodeList.
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

	// Dependencies maps referenced node IDs to their dependency kind.
	// A hard dependency gates dispatch and contagious exclusion — every
	// evaluation path requires the data. A lazy dependency participates
	// in propagation triggering only — at least one evaluation path can
	// produce a result without the data. Populated by BuildDAG; nil
	// before that.
	Dependencies map[string]DepKind
}

// NodeType returns the parse-time classification of this node.
func (n *Node) Type() NodeType {
	return n.nodeType
}

// SetType sets the parse-time classification. Exported for test
// construction in external test packages that build Node literals
// without going through ParseNodeList.
func (n *Node) SetType(t NodeType) {
	n.nodeType = t
}

// HasStatusSubresource reports whether the node's body declares a
// non-nil status field. Exported for test assertions in external
// test packages.
func (n *Node) HasStatusSubresource() bool {
	return n.hasStatusSubresource
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
		if s, ok := id[field].(string); ok && IsCELExpression(s) {
			return true
		}
	}
	return false
}

// IsCELExpression returns true if s contains a CEL template expression
// marker. Single source of truth for detecting CEL in identity fields —
// used by HasDynamicGVR and extractLiteralGVK.
func IsCELExpression(s string) bool {
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
// key requires a scopeResolver (to handle cluster-scoped resources
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
		WalkExpressions(strs, func(expr string) {
			if !seen[expr] {
				seen[expr] = true
				exprs = append(exprs, expr)
			}
		})
	}

	// Collect expressions from each node
	for _, node := range s.Nodes { // Template expressions
		var templateStrings []string
		// Walk body maps that may carry CEL expressions. Ref/Watch
		// bodies (identity-only) also contain ${...} in metadata.name,
		// metadata.namespace, selector values — they must be compiled too.
		if body := node.Body(); body != nil {
			CollectStrings(body, &templateStrings)
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

// StringsToAny converts []string to []any for unstructured serialization.
func StringsToAny(ss []string) []any {
	result := make([]any, len(ss))
	for i, s := range ss {
		result[i] = s
	}
	return result
}

// GVKFromMap extracts a GroupVersionKind from an unstructured map.
func GVKFromMap(m map[string]any) schema.GroupVersionKind {
	apiVersion, _ := m["apiVersion"].(string)
	kind, _ := m["kind"].(string)
	gv, _ := schema.ParseGroupVersion(apiVersion)
	return gv.WithKind(kind)
}

// IsGraphCRLiteral checks if literal apiVersion/kind values identify a Graph CR.
// Expression-valued fields (containing ${...}) return false — the GVK is
// unknown until runtime.
func IsGraphCRLiteral(apiVersion, kind string) bool {
	if strings.Contains(apiVersion, "${") || strings.Contains(kind, "${") {
		return false
	}
	return apiVersion == "experimental.kro.run/v1alpha1" && kind == "Graph"
}
