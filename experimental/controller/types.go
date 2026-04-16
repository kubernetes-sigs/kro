// types.go defines the Graph data model and spec parsing.
//
// Node and GraphSpec are the parsed representations of a Graph's spec.
// Everything else in the package depends on these types; they don't depend
// on anything else in the package.
package graphcontroller

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

// Reference is the reference type of a Graph node — it classifies how the node
// relates to its target Kubernetes resource: what it does at reconcile time and
// what it owes on cleanup. Think of it like pointer types in a PL's ownership
// model: owned, borrowed mutably, borrowed immutably, or not yet resolved.
// Watch and WatchKind are determined by template structure at compile time.
// Own and Contribute are determined by resource existence at first reconcile.
// Definition is determined by the absence of apiVersion and kind in the template.
// See 003-ownership.md § References and 001-graph.md § template.
type Reference int

const (
	// ReferenceOwn — the Graph creates the resource. Applied via SSA. Tracked
	// for cleanup. Deleted on prune.
	ReferenceOwn Reference = iota
	// ReferenceWatch — identity-only template (apiVersion, kind, metadata.name,
	// optionally metadata.namespace). Read-only GET. Not tracked.
	ReferenceWatch
	// ReferenceWatchKind — apiVersion + kind with optional selector, no
	// metadata.name. Read-only List of all resources of that kind. Not tracked.
	ReferenceWatchKind
	// ReferenceContribute — writes fields on a resource the Graph does not
	// create. Applied via SSA. Tracked for cleanup. Releases fields on prune.
	ReferenceContribute
	// ReferenceDefinition — template has no apiVersion and no kind. Puts
	// values in scope as map[string]any — literals, CEL expressions, or both.
	// No Kubernetes resource created or managed. No drift timer, no
	// applied-set entry, nothing to clean up.
	// See 001-graph.md § template.
	ReferenceDefinition
	// ReferenceUnresolved — template has fields beyond identity but Own vs
	// Contribute cannot be determined from the template alone. Resolved at
	// first reconcile by checking whether the target resource exists (absent →
	// Own, present → Contribute). Should never appear in dag.References after
	// the first reconcile of a revision.
	ReferenceUnresolved
)

// String returns the human-readable name of the Reference for logging and display.
func (r Reference) String() string {
	switch r {
	case ReferenceOwn:
		return "own"
	case ReferenceWatch:
		return "watch"
	case ReferenceWatchKind:
		return "watchKind"
	case ReferenceContribute:
		return "contribute"
	case ReferenceDefinition:
		return "definition"
	case ReferenceUnresolved:
		return "unresolved"
	default:
		return fmt.Sprintf("Reference(%d)", int(r))
	}
}

// LabelValue returns the identity label value for references that write to a
// resource. Returns ("", false) for read-only and unresolved references, which
// are never stamped with an identity label.
func (r Reference) LabelValue() (string, bool) {
	switch r {
	case ReferenceOwn:
		return "own", true
	case ReferenceContribute:
		return "contribute", true
	default:
		return "", false
	}
}

// ReferenceFromLabelValue parses an identity label value back to a Reference.
// Returns (0, false) if the value is not a recognized label value.
func ReferenceFromLabelValue(s string) (Reference, bool) {
	switch s {
	case "own":
		return ReferenceOwn, true
	case "contribute":
		return ReferenceContribute, true
	default:
		return 0, false
	}
}

// DetectReference returns the Reference type of a node's template map.
//
// Detection order (from 003-ownership.md):
//  1. Definition — non-empty template with no apiVersion and no kind
//  2. WatchKind — apiVersion + kind, no metadata.name
//  3. Watch — only identity fields (apiVersion, kind, metadata.name/namespace)
//  4. Unresolved — has fields beyond identity; Own vs Contribute determined
//     at reconcile time by resource existence
func DetectReference(tmpl map[string]any) Reference {
	if len(tmpl) == 0 {
		return ReferenceUnresolved
	}

	_, hasAPIVersion := tmpl["apiVersion"]
	_, hasKind := tmpl["kind"]

	// 1. Definition: no apiVersion and no kind — values defined in scope.
	if !hasAPIVersion && !hasKind {
		return ReferenceDefinition
	}

	md, _ := tmpl["metadata"].(map[string]any)
	_, hasName := md["name"]

	// 2. WatchKind: no metadata.name
	if !hasName {
		return ReferenceWatchKind
	}

	// 3. Watch: only identity fields
	if isIdentityOnly(tmpl) {
		return ReferenceWatch
	}

	// 4. Unresolved: has fields beyond identity. Own vs Contribute resolved
	//    at first reconcile by checking resource existence.
	return ReferenceUnresolved
}

// isIdentityOnly returns true if the template contains only identity fields:
// apiVersion, kind, and metadata with only name and/or namespace.
func isIdentityOnly(tmpl map[string]any) bool {
	for key := range tmpl {
		switch key {
		case "apiVersion", "kind", "metadata":
			continue
		default:
			return false
		}
	}
	md, _ := tmpl["metadata"].(map[string]any)
	if md == nil {
		return true
	}
	for key := range md {
		switch key {
		case "name", "namespace":
			continue
		default:
			return false
		}
	}
	return true
}

// Node is a parsed Graph node entry — a user's declaration of intent about
// a Kubernetes resource (or collection of resources via forEach). Definition
// nodes (no apiVersion/kind) put values into scope without creating resources.
// It is the unit of the dependency graph: each node has an identity, a template,
// and computed dependency edges populated by BuildDAG.
//
// "Node" (not "Resource") because a node is a graph-theory concept — it
// occupies a position in the DAG, has edges, and may produce zero, one, or
// many Kubernetes resources at runtime (e.g., forEach). Kubernetes already
// uses "resource" for five distinct concepts; adding a sixth blurs the
// boundary between the user's declaration and the Kubernetes objects it
// produces.
type Node struct {
	ID            string
	Template      map[string]any
	ForEach       map[string]string
	Finalizes     string // target node ID — resource created only during prune/teardown
	IncludeWhen   []string
	ReadyWhen     []string // CEL conditions; all must be true for the node to be "ready"
	PropagateWhen []string // CEL conditions; all must be true for data to flow to dependents

	// Dependencies are IDs of nodes this node references in its CEL expressions.
	// Populated by BuildDAG; nil before that.
	Dependencies map[string]bool

	// DepPaths maps each dependency to the field paths this node's CEL
	// expressions reference. For example, if this node's template contains
	// ${deploy.status.availableReplicas}, DepPaths["deploy"] contains
	// ["status", "availableReplicas"]. Used for field-path-scoped evaluation
	// hashing — only hash the specific paths the node actually reads.
	// Per 004-graph-reconciliation.md § Hash Mechanics.
	// Populated by BuildDAG; nil before that.
	DepPaths map[string][]FieldPath

	// SelfPaths is the set of field paths into this node's own observed
	// resource that readyWhen, propagateWhen, and downstream expressions
	// reference. When only self paths changed (e.g., a Deployment's
	// status.availableReplicas updated but spec didn't), the template is
	// unchanged — skip template evaluation and apply, re-evaluate only
	// the gate conditions. Also used for propagation hashing — the
	// propagation hash covers only these paths.
	// Populated by BuildDAG; nil before that.
	SelfPaths []FieldPath

	// ReadinessDeps is the set of upstream node IDs whose readiness state
	// must be checked even when the evaluation hash matches. These are nodes
	// referenced via .ready() in readyWhen or propagateWhen — a runtime
	// property that doesn't map to any field path. When any ReadinessDep's
	// plan state changes between reconcile cycles, the node re-evaluates
	// its gate conditions instead of skipping entirely.
	// Populated by BuildDAG; nil before that.
	ReadinessDeps map[string]bool
}

// Reference returns the Reference type of this node's template.
func (n *Node) Reference() Reference {
	return DetectReference(n.Template)
}

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
		for varName := range node.ForEach {
			add(varName)
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
		collectStrings(node.Template, &templateStrings)
		add(templateStrings)

		// ForEach collection expressions
		for _, v := range node.ForEach {
			add([]string{v})
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

// parseNodeList converts a raw node list into Nodes.
// Returns an error if any node is missing an ID or has a duplicate ID.
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
		// Per 001-graph.md: "Hyphens are not allowed — they are parsed as
		// subtraction by the CEL evaluator (e.g., my-app is my minus app)."
		if strings.Contains(id, "-") {
			return nil, fmt.Errorf("node[%d] %q: hyphens are not allowed in node IDs (parsed as subtraction by CEL)", i, id)
		}
		// Node IDs are embedded in identity label key prefixes as DNS
		// subdomain segments. Reject IDs that would produce invalid DNS
		// subdomains (e.g., underscores, spaces). This catches the entire
		// class of invalid characters rather than enumerating them.
		if errs := validation.IsDNS1123Label(strings.ToLower(id)); len(errs) > 0 {
			return nil, fmt.Errorf("node[%d] %q: invalid DNS subdomain segment for identity label key: %s", i, id, strings.Join(errs, "; "))
		}
		if seen[id] {
			return nil, fmt.Errorf("node[%d]: duplicate id %q", i, id)
		}
		lower := strings.ToLower(id)
		if orig, exists := seenLower[lower]; exists && orig != id {
			return nil, fmt.Errorf("node[%d] %q: collides with %q after lowercasing (both produce %q in identity labels)", i, id, orig, lower)
		}
		seen[id] = true
		seenLower[lower] = id
		node.ID = id
		if tmpl, ok := m["template"].(map[string]any); ok {
			node.Template = tmpl
		}
		if fin, ok := m["finalizes"].(string); ok {
			node.Finalizes = fin
		}
		if fe, ok := m["forEach"].(map[string]any); ok {
			// Flat map format (Graph templates): forEach: {region: "${...}"}
			parsed, err := parseForEachMap(fe)
			if err != nil {
				return nil, fmt.Errorf("node[%d] %q: %w", i, id, err)
			}
			node.ForEach = parsed
		} else if feArr, ok := m["forEach"].([]any); ok {
			// Array format (upstream kro API): forEach: [{region: "${...}"}, {tier: "${...}"}]
			// Each element is a map of variable bindings. Flatten to map[string]string.
			node.ForEach = make(map[string]string)
			for j, dim := range feArr {
				dimMap, ok := dim.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("node[%d] %q: forEach[%d] must be a map, got %T", i, id, j, dim)
				}
				for k, v := range dimMap {
					if _, exists := node.ForEach[k]; exists {
						return nil, fmt.Errorf("node[%d] %q: forEach has duplicate variable %q", i, id, k)
					}
					vs, ok := v.(string)
					if !ok {
						return nil, fmt.Errorf("node[%d] %q: forEach[%d] variable %q value must be a string, got %T", i, id, j, k, v)
					}
					node.ForEach[k] = vs
				}
			}
		} else if _, hasForEach := m["forEach"]; hasForEach && m["forEach"] != nil {
			return nil, fmt.Errorf("node[%d] %q: forEach must be a map or array, got %T", i, id, m["forEach"])
		}
		if node.ForEach != nil && len(node.ForEach) == 0 {
			return nil, fmt.Errorf("node[%d] %q: forEach must have at least one dimension", i, id)
		}
		if node.ForEach != nil {
			// Validate: forEach iterator variable names must not collide
			// with any node ID. A collision would shadow the node in scope.
			for varName := range node.ForEach {
				if seen[varName] {
					return nil, fmt.Errorf("node[%d] %q: forEach variable %q collides with a node ID", i, id, varName)
				}
			}
		}
		// Validate: finalizes nodes must not have CEL-evaluated names unless
		// they also have forEach (which requires dynamic per-item names).
		// Static-name finalizers are looked up by key during prune; forEach
		// finalizers use label-based discovery for cleanup.
		// NOTE: This check must run after forEach is parsed above.
		if node.Finalizes != "" && node.ForEach == nil && node.Template != nil {
			if md, ok := node.Template["metadata"].(map[string]any); ok {
				if name, ok := md["name"].(string); ok && strings.Contains(name, "${") {
					return nil, fmt.Errorf("node[%d] %q: finalizes nodes must not have CEL-evaluated names (found expression in metadata.name); use forEach for per-item finalizers", i, id)
				}
			}
		}
		if iw, ok := m["includeWhen"].([]any); ok {
			for _, expr := range iw {
				if s, ok := expr.(string); ok {
					node.IncludeWhen = append(node.IncludeWhen, s)
				}
			}
		}
		if rw, ok := m["readyWhen"].([]any); ok {
			for _, expr := range rw {
				if s, ok := expr.(string); ok {
					node.ReadyWhen = append(node.ReadyWhen, s)
				}
			}
		}
		if pw, ok := m["propagateWhen"].([]any); ok {
			for _, expr := range pw {
				if s, ok := expr.(string); ok {
					node.PropagateWhen = append(node.PropagateWhen, s)
				}
			}
		}
		nodes = append(nodes, node)
	}
	return nodes, nil
}

// parseForEachMap parses a flat forEach map (map[string]any → map[string]string).
// Returns an error if any value is not a string — silent drops are data loss.
func parseForEachMap(m map[string]any) (map[string]string, error) {
	result := make(map[string]string, len(m))
	for k, v := range m {
		vs, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("forEach variable %q value must be a string, got %T", k, v)
		}
		result[k] = vs
	}
	return result, nil
}
